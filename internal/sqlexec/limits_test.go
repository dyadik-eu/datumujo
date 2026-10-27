package sqlexec

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// TestLimits hits each limit that docs/sql.md names through SQL: the
// last value that fits, and the first that does not. The page size is
// 4096, the size of a new file.
func TestLimits(t *testing.T) {
	ss := newSession(t, store.Options{MaxTxBytes: 64 << 20})
	fails := func(what, src string, cause error) {
		t.Helper()
		if _, err := ss.exec(src); !errors.Is(err, cause) {
			t.Errorf("%s: %v, want %v", what, err, cause)
		}
	}
	columns := func(n int, key bool) string {
		var cs []string
		for i := 0; i < n; i++ {
			c := fmt.Sprintf("c%d INTEGER", i)
			if key && i == 0 {
				c += " PRIMARY KEY"
			}
			cs = append(cs, c)
		}
		return strings.Join(cs, ", ")
	}

	// A name: 255 bytes.
	ss.must(fmt.Sprintf(`CREATE TABLE "%s" (x INTEGER)`, strings.Repeat("n", 255)))
	fails("a name of 256 bytes", fmt.Sprintf(`CREATE TABLE "%s" (x INTEGER)`, strings.Repeat("n", 256)), table.ErrSchema)

	// Columns: 1024, and the hidden key of a table without PRIMARY KEY
	// is one of them.
	ss.must("CREATE TABLE wide (" + columns(1024, true) + ")")
	fails("1025 columns", "CREATE TABLE wider ("+columns(1025, true)+")", table.ErrSchema)
	ss.must("CREATE TABLE widenokey (" + columns(1023, false) + ")")
	fails("1024 columns without a key", "CREATE TABLE widernokey ("+columns(1024, false)+")", table.ErrSchema)

	// Indexes: 64 on a table.
	for i := 0; i < 64; i++ {
		ss.must(fmt.Sprintf("CREATE INDEX i%d ON wide (c%d)", i, i+1))
	}
	fails("the 65th index", "CREATE INDEX i64 ON wide (c65)", table.ErrSchema)

	// The key form: 1000 bytes. A TEXT without the byte 0 takes its
	// length plus 2 bytes of end mark, so a TEXT key holds 998 bytes.
	ss.must("CREATE TABLE k (s TEXT PRIMARY KEY)")
	ss.must("INSERT INTO k VALUES (?)", strings.Repeat("a", 998))
	fails("a TEXT key of 999 bytes", "INSERT INTO k VALUES ('"+strings.Repeat("b", 999)+"')", table.ErrValue)

	// An index entry is the index columns, then the key. A TEXT column
	// that allows NULL takes 1 byte of mark, its length and 2 bytes of end
	// mark; an INTEGER key 8 bytes. So the text holds 989 bytes.
	ss.must("CREATE TABLE ix (id INTEGER PRIMARY KEY, s TEXT)")
	ss.must("CREATE INDEX by_s ON ix (s)")
	ss.must("INSERT INTO ix VALUES (1, ?)", strings.Repeat("a", 989))
	fails("an indexed TEXT of 990 bytes", "INSERT INTO ix VALUES (2, '"+strings.Repeat("b", 990)+"')", table.ErrValue)

	// A value outside the key has no limit of its own; a large one takes
	// overflow pages. The bound of the transaction limits it.
	big := strings.Repeat("v", 1<<20)
	ss.must("CREATE TABLE blob (id INTEGER PRIMARY KEY, body TEXT)")
	ss.must("INSERT INTO blob VALUES (1, ?)", big)
	got, err := ss.query("SELECT length(body) FROM blob")
	if err != nil || got != "1048576\n" {
		t.Errorf("a value of 1 MiB: %q %v", got, err)
	}
	small := newSession(t, store.Options{MaxTxBytes: 1 << 20})
	small.must("CREATE TABLE blob (id INTEGER PRIMARY KEY, body TEXT)")
	if _, err := small.exec("INSERT INTO blob VALUES (1, ?)", big); !errors.Is(err, store.ErrTxTooLarge) {
		t.Errorf("a value of 1 MiB in a transaction of 1 MiB: %v", err)
	}
}

// TestLimitsOfStage3 hits the limits of the forms of stage 3 that
// docs/sql.md names: the last value that fits, and the first that does
// not.
func TestLimitsOfStage3(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a INTEGER)")
	ss.must("INSERT INTO t VALUES (1)")

	// Subqueries inside each other: 199. Each is a level of the 200
	// levels of nesting, and the SELECT around them is one more. A
	// correlated subquery reads the row of t through all 199.
	nest := func(n int, inner, outer string) string {
		return "SELECT " + strings.Repeat("(SELECT ", n) + inner + strings.Repeat(")", n) + outer
	}
	for _, src := range []string{nest(199, "a FROM t", ""), nest(199, "t.a", " FROM t")} {
		if got, err := ss.query(src); err != nil || got != "1\n" {
			t.Errorf("199 subqueries inside each other: %q %v", got, err)
		}
	}
	if _, err := ss.query(nest(200, "a FROM t", "")); err == nil || !strings.Contains(err.Error(), "nested more than 200 levels") {
		t.Errorf("200 subqueries inside each other: %v", err)
	}

	// Each UNIQUE is an index, so a table takes 64 of them.
	unique := func(n int) string {
		var cs []string
		for i := 0; i < n; i++ {
			cs = append(cs, fmt.Sprintf("c%d INTEGER UNIQUE", i))
		}
		return strings.Join(cs, ", ")
	}
	ss.must("CREATE TABLE u64 (" + unique(64) + ")")
	if _, err := ss.exec("CREATE TABLE u65 (" + unique(65) + ")"); !errors.Is(err, table.ErrSchema) {
		t.Errorf("65 UNIQUE: %v", err)
	}
	if _, err := ss.exec("CREATE INDEX one_more ON u64 (c0)"); !errors.Is(err, table.ErrSchema) {
		t.Errorf("an index after 64 UNIQUE: %v", err)
	}

	// The name of the index of a UNIQUE is the table name and _unique_1,
	// 9 bytes. So the table name holds 246 of the 255 bytes.
	ss.must(fmt.Sprintf(`CREATE TABLE "%s" (x INTEGER UNIQUE)`, strings.Repeat("u", 246)))
	if _, err := ss.exec(fmt.Sprintf(`CREATE TABLE "%s" (x INTEGER UNIQUE)`, strings.Repeat("v", 247))); err == nil || !strings.Contains(err.Error(), "longer than 255 bytes") {
		t.Errorf("a table name of 247 bytes with a UNIQUE: %v", err)
	}

	// A DEFAULT and a CHECK have no limit of their own. The schema is one
	// value that holds their text, and each change of the schema writes
	// it again. In a transaction of 1 MiB, a table with a DEFAULT of
	// 1037305 bytes can be made. But from 518613 bytes on, the next
	// change of the schema fails, as does a CHECK of that size.
	schema := func(def, check int, next bool) error {
		t.Helper()
		small := newSession(t, store.Options{MaxTxBytes: 1 << 20})
		src := "CREATE TABLE d (a TEXT DEFAULT '" + strings.Repeat("d", def) + "')"
		if check > 0 {
			src = "CREATE TABLE d (a TEXT CHECK (a <> '" + strings.Repeat("c", check) + "'))"
		}
		if _, err := small.exec(src); err != nil || !next {
			return err
		}
		small.commit()
		_, err := small.exec("CREATE TABLE x (a INTEGER)")
		return err
	}
	for _, c := range []struct {
		what       string
		def, check int
		next, ok   bool
	}{
		{"a DEFAULT of 1037305 bytes", 1037305, 0, false, true},
		{"a DEFAULT of 1037306 bytes", 1037306, 0, false, false},
		{"a change after a DEFAULT of 518612 bytes", 518612, 0, true, true},
		{"a change after a DEFAULT of 518613 bytes", 518613, 0, true, false},
		{"a change after a CHECK of 518613 bytes", 0, 518613, true, false},
	} {
		if err := schema(c.def, c.check, c.next); (err == nil) != c.ok || err != nil && !errors.Is(err, store.ErrTxTooLarge) {
			t.Errorf("%s in a transaction of 1 MiB: %v", c.what, err)
		}
	}

	// The values of IN (SELECT ...) count toward the memory of the
	// statement, also in a correlated subquery. 6 values of 3000 bytes
	// fit in 20000 bytes, 8 do not.
	ss.must("CREATE TABLE s (k INTEGER, v TEXT)")
	for i := 0; i < 8; i++ {
		ss.must("INSERT INTO s VALUES (1, ?)", strings.Repeat("x", 3000))
	}
	ss.lim = Limits{MaxMemory: 20000}
	if _, err := ss.query("SELECT 'x' IN (SELECT v FROM s WHERE s.k = t.a LIMIT 6) FROM t"); err != nil {
		t.Errorf("6 values of 3000 bytes: %v", err)
	}
	if _, err := ss.query("SELECT 'x' IN (SELECT v FROM s WHERE s.k = t.a) FROM t"); !errors.Is(err, ErrMemory) {
		t.Errorf("8 values of 3000 bytes: %v", err)
	}
}
