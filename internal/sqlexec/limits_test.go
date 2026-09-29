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
