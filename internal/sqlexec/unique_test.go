package sqlexec

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// indexes lists the indexes of a table as name(columns), unique ones
// with a !.
func indexes(t *testing.T, ss *session, name string) string {
	t.Helper()
	tb, ok := ss.tx.Schema().Table(name)
	if !ok {
		t.Fatalf("no table %s", name)
	}
	var out []string
	for _, ix := range tb.Indexes {
		var cols []string
		for _, c := range ix.Columns {
			cols = append(cols, tb.Columns[c].Name)
		}
		mark := ""
		if ix.Unique {
			mark = "!"
		}
		out = append(out, ix.Name+mark+"("+strings.Join(cols, ",")+")")
	}
	return strings.Join(out, " ")
}

// TestUnique checks L-15: each UNIQUE is a unique index with a name of
// its own. A second row with the same values fails, NULL never
// conflicts, and a set that is there already adds no index.
func TestUnique(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must(`CREATE TABLE t (id INTEGER PRIMARY KEY UNIQUE, a TEXT UNIQUE, b INTEGER, c INTEGER,
		UNIQUE (b, c), UNIQUE (a), UNIQUE (c, b), UNIQUE (id))`)
	if got := indexes(t, ss, "t"); got != "t_unique_1!(a) t_unique_2!(b,c)" {
		t.Fatalf("indexes of t: %s", got)
	}
	ss.commit()
	ss.must("INSERT INTO t VALUES (1, 'x', 1, 1), (2, NULL, NULL, 1), (3, NULL, NULL, 1)")
	failsAt(t, ss, "INSERT INTO t VALUES (4, 'x', 2, 2)", 1, 23, "", table.ErrUnique)
	failsAt(t, ss, "INSERT INTO t VALUES (4, 'y', 1, 1)", 1, 23, "", table.ErrUnique)
	failsAt(t, ss, "UPDATE t SET a = 'x' WHERE id = 2", 1, 1, "", table.ErrUnique)
	// The first row fits, the second does not: the INSERT writes none.
	failsAt(t, ss, "INSERT INTO t VALUES (4, 'y', 5, 5), (5, 'y', 6, 6)", 1, 39, "", table.ErrUnique)
	ss.must("UPDATE t SET a = 'z' WHERE id = 2")
	want := "1 'x' 1 1\n2 'z' NULL 1\n3 NULL NULL 1\n"
	if got := ss.dump("t"); got != want {
		t.Errorf("t:\n%s\nwant\n%s", got, want)
	}

	// The name takes the first number that no index and no table has.
	ss.must("CREATE TABLE u_unique_1 (x INTEGER)")
	ss.must("CREATE TABLE o (y INTEGER)")
	ss.must("CREATE INDEX u_unique_2 ON o (y)")
	ss.must("CREATE TABLE u (a INTEGER UNIQUE, b INTEGER UNIQUE)")
	if got := indexes(t, ss, "u"); got != "u_unique_3!(a) u_unique_4!(b)" {
		t.Errorf("indexes of u: %s", got)
	}
	// A table without PRIMARY KEY: its hidden key is unique already.
	ss.must("CREATE TABLE h (a INTEGER, UNIQUE (a))")
	if got := indexes(t, ss, "h"); got != "h_unique_1!(a)" {
		t.Errorf("indexes of h: %s", got)
	}
	// The index of a UNIQUE is an index: DROP INDEX drops it, unlike in
	// SQLite.
	ss.must("DROP INDEX h_unique_1")
	ss.must("INSERT INTO h VALUES (1), (1)")
	// UNIQUE alone keeps the file at format version 1, which v0.2.0
	// reads: v0.2.0 knows unique indexes.
	if ss.stx.Version() != 1 {
		t.Errorf("format version %d with UNIQUE only", ss.stx.Version())
	}
}

// TestUniqueErrors: a UNIQUE that names no column of the table, or one
// column twice, is an error when the table is made. ALTER TABLE does not
// add a UNIQUE column, as SQLite does not.
func TestUniqueErrors(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY)")
	long := strings.Repeat("n", table.MaxName-len("_unique_1")+1)
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"CREATE TABLE u (a INTEGER, UNIQUE (z))", 1, 28, "UNIQUE: no such column: z"},
		{"CREATE TABLE u (a INTEGER, UNIQUE (a, a))", 1, 28, "UNIQUE: column a twice"},
		{"CREATE TABLE u (a INTEGER, UNIQUE (rowid))", 1, 28, "UNIQUE: no such column: rowid"},
		{"ALTER TABLE t ADD COLUMN n INTEGER UNIQUE", 1, 36, "an added column cannot be UNIQUE"},
		{fmt.Sprintf("CREATE TABLE %s (a INTEGER UNIQUE)", long), 1, 273, "is longer than 255 bytes"},
	} {
		failsAt(t, ss, c.src, c.line, c.col, c.msg, nil)
	}
	if n := len(ss.tx.Schema().Tables); n != 1 {
		t.Errorf("%d tables after only rejected ones", n)
	}
	// A table name that leaves room for the suffix works.
	ss.must(fmt.Sprintf("CREATE TABLE %s (a INTEGER UNIQUE)", long[1:]))
	// More UNIQUE than indexes a table can have: the table is not made.
	var cols, uniq []string
	for i := 0; i <= table.MaxIndexes; i++ {
		cols = append(cols, fmt.Sprintf("c%d INTEGER", i))
		uniq = append(uniq, fmt.Sprintf("UNIQUE (c%d)", i))
	}
	src := "CREATE TABLE many (" + strings.Join(append(cols, uniq...), ", ") + ")"
	if _, err := ss.exec(src); !errors.Is(err, table.ErrSchema) {
		t.Errorf("%d UNIQUE: %v", table.MaxIndexes+1, err)
	}
	if _, ok := ss.tx.Schema().Table("many"); ok {
		t.Error("a table with too many UNIQUE was made")
	}
}
