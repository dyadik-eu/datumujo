package sqlexec

import (
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// upsertSession holds the tables of the tests of ON CONFLICT.
func upsertSession(t *testing.T) *session {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a TEXT UNIQUE, n INTEGER NOT NULL DEFAULT 0, b INTEGER)")
	ss.must("INSERT INTO t VALUES (1, 'x', 1, 1), (2, 'y', 2, 2)")
	return ss
}

// TestDoNothing checks ON CONFLICT DO NOTHING (L-17). Each case was run
// in SQLite 3.54.0 first, with the same rows and the same count.
func TestDoNothing(t *testing.T) {
	ss := upsertSession(t)
	for _, c := range []struct {
		src  string
		n    int64
		want string
	}{
		// A conflict on any key without a target: on the key, on a.
		{"INSERT INTO t VALUES (1, 'z', 9, 9) ON CONFLICT DO NOTHING", 0, "1 'x' 1 1\n2 'y' 2 2\n"},
		{"INSERT INTO t VALUES (3, 'x', 9, 9) ON CONFLICT DO NOTHING", 0, "1 'x' 1 1\n2 'y' 2 2\n"},
		{"INSERT INTO t VALUES (3, 'z', 3, 3), (4, 'z', 4, 4) ON CONFLICT (a) DO NOTHING", 1, "1 'x' 1 1\n2 'y' 2 2\n3 'z' 3 3\n"},
		{"INSERT INTO t (a) VALUES ('y'), ('w') ON CONFLICT (a) DO NOTHING", 1, "1 'x' 1 1\n2 'y' 2 2\n3 'z' 3 3\n4 'w' 0 NULL\n"},
		{"INSERT INTO t VALUES (2, 'v', 0, 0) ON CONFLICT (id) DO NOTHING", 0, "1 'x' 1 1\n2 'y' 2 2\n3 'z' 3 3\n4 'w' 0 NULL\n"},
	} {
		r, err := ss.exec(c.src)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if r.RowsAffected != c.n {
			t.Errorf("%s: %d rows, want %d", c.src, r.RowsAffected, c.n)
		}
		if got := ss.dump("t"); got != c.want {
			t.Errorf("%s: t is\n%s want\n%s", c.src, got, c.want)
		}
	}
	// NULL in a unique index conflicts with no row. A composite key in
	// another order is the key. rowid is the key of a table without one.
	ss.must("CREATE TABLE h (a INTEGER UNIQUE, b INTEGER)")
	ss.must("INSERT INTO h VALUES (1, 1), (NULL, 2)")
	if r := ss.must("INSERT INTO h VALUES (1, 5), (NULL, 6), (2, 7) ON CONFLICT (a) DO NOTHING"); r.RowsAffected != 2 {
		t.Errorf("h: %d rows, want 2", r.RowsAffected)
	}
	if got := ss.dump("h"); got != "1 1 1\n2 NULL 2\n3 NULL 6\n4 2 7\n" {
		t.Errorf("h:\n%s", got)
	}
	if r := ss.must("INSERT INTO h VALUES (3, 3) ON CONFLICT (rowid) DO NOTHING"); r.RowsAffected != 1 {
		t.Errorf("h with rowid: %d rows, want 1", r.RowsAffected)
	}
	ss.must("CREATE TABLE p (x INTEGER, y TEXT, PRIMARY KEY (x, y))")
	ss.must("INSERT INTO p VALUES (1, 'a')")
	if r := ss.must("INSERT INTO p VALUES (1, 'a'), (1, 'b') ON CONFLICT (y, x) DO NOTHING"); r.RowsAffected != 1 {
		t.Errorf("p: %d rows, want 1", r.RowsAffected)
	}
	if got := ss.dump("p"); got != "1 'a'\n1 'b'\n" {
		t.Errorf("p:\n%s", got)
	}
	// An index that is not unique is no key: b = 1 is there already.
	ss.must("CREATE INDEX t_b ON t (b)")
	if r := ss.must("INSERT INTO t VALUES (5, 'q', 0, 1) ON CONFLICT DO NOTHING"); r.RowsAffected != 1 {
		t.Errorf("t with a plain index: %d rows, want 1", r.RowsAffected)
	}
}

// TestDoNothingErrors: a conflict on a key that is not the target fails.
// So do the checks of the row, before the conflict is looked up. An
// error writes no row of the INSERT.
func TestDoNothingErrors(t *testing.T) {
	ss := upsertSession(t)
	ss.must("CREATE TABLE c (id INTEGER PRIMARY KEY, a INTEGER CHECK (a > 0))")
	ss.must("INSERT INTO c VALUES (1, 1)")
	ss.must("CREATE TABLE q (x INTEGER, y INTEGER, z INTEGER, PRIMARY KEY (x, y))")
	ss.must("CREATE INDEX t_n ON t (n)")
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
		is        error
	}{
		{"INSERT INTO t VALUES (3, 'x', 9, 9) ON CONFLICT (id) DO NOTHING", 1, 23, "index t_unique_1", table.ErrUnique},
		{"INSERT INTO t VALUES (5, 'q', 5, 5), (3, 'x', 9, 9) ON CONFLICT (id) DO NOTHING", 1, 39, "", table.ErrUnique},
		{"INSERT INTO t VALUES (1, 'x', NULL, 0) ON CONFLICT DO NOTHING", 1, 23, "NOT NULL", nil},
		{"INSERT INTO c VALUES (1, -1) ON CONFLICT DO NOTHING", 1, 23, "", ErrCheck},
		{"INSERT INTO t VALUES (1, 'w', 0, 0) ON CONFLICT (b) DO NOTHING", 1, 37, "ON CONFLICT (b): no PRIMARY KEY or UNIQUE of table t has these columns", nil},
		{"INSERT INTO t VALUES (1, 'w', 0, 0) ON CONFLICT (a, id) DO NOTHING", 1, 37, "no PRIMARY KEY or UNIQUE", nil},
		{"INSERT INTO t VALUES (1, 'w', 0, 0) ON CONFLICT (a, a) DO NOTHING", 1, 37, "column a twice", nil},
		{"INSERT INTO t VALUES (1, 'w', 0, 0) ON CONFLICT (z) DO NOTHING", 1, 37, "no such column: z", nil},
		{"INSERT INTO t VALUES (1, 'w', 0, 0) ON CONFLICT (n) DO NOTHING", 1, 37, "no PRIMARY KEY or UNIQUE", nil},
		// The key of q is (x, y): (z, y) has its size and its last column.
		{"INSERT INTO q VALUES (1, 1, 1) ON CONFLICT (z, y) DO NOTHING", 1, 32, "no PRIMARY KEY or UNIQUE", nil},
	} {
		failsAt(t, ss, c.src, c.line, c.col, c.msg, c.is)
	}
	if got := ss.dump("t"); got != "1 'x' 1 1\n2 'y' 2 2\n" {
		t.Errorf("t after only errors:\n%s", got)
	}
}

// TestDoUpdate checks ON CONFLICT DO UPDATE with excluded (L-17). Each
// case starts from the rows (1, 'x', 1, 1) and (2, 'y', 2, 2). SQLite
// 3.54.0 gives the same rows and counts for each.
func TestDoUpdate(t *testing.T) {
	for _, c := range []struct {
		src  string
		n    int64
		want string
	}{
		{"INSERT INTO t VALUES (3, 'x', 9, 9) ON CONFLICT (a) DO UPDATE SET n = n + excluded.n", 1, "1 'x' 10 1\n2 'y' 2 2\n"},
		// The second row conflicts with the row the first one changed.
		{"INSERT INTO t (a, b) VALUES ('y', 5), ('y', 6) ON CONFLICT (a) DO UPDATE SET b = t.b + excluded.b", 2, "1 'x' 1 1\n2 'y' 2 13\n"},
		{"INSERT INTO t VALUES (9, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET b = 100 WHERE excluded.n > 5", 0, "1 'x' 1 1\n2 'y' 2 2\n"},
		{"INSERT INTO t VALUES (3, 'x', 9, 9) ON CONFLICT (a) DO UPDATE SET b = excluded.b WHERE b IS NULL", 0, "1 'x' 1 1\n2 'y' 2 2\n"},
		// excluded.n of a column that the INSERT does not name is its
		// default; one row is new and one changed.
		{"INSERT INTO t (a) VALUES ('q'), ('x') ON CONFLICT (a) DO UPDATE SET n = excluded.n", 2, "1 'x' 0 1\n2 'y' 2 2\n3 'q' 0 NULL\n"},
		// A new key moves the row.
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET id = 7", 1, "2 'y' 2 2\n7 'x' 1 1\n"},
		// Without a target, the primary key comes first: the row has
		// id 1, and a 'y' of the row with id 2. With the target a, the
		// row with a 'y' changes.
		{"INSERT INTO t VALUES (1, 'y', 9, 9) ON CONFLICT DO UPDATE SET b = 50", 1, "1 'x' 1 50\n2 'y' 2 2\n"},
		{"INSERT INTO t VALUES (1, 'y', 9, 9) ON CONFLICT (a) DO UPDATE SET b = 50", 1, "1 'x' 1 1\n2 'y' 2 50\n"},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT DO UPDATE SET b = 7", 1, "1 'x' 1 7\n2 'y' 2 2\n"},
	} {
		ss := upsertSession(t)
		r, err := ss.exec(c.src)
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if r.RowsAffected != c.n {
			t.Errorf("%s: %d rows, want %d", c.src, r.RowsAffected, c.n)
		}
		if got := ss.dump("t"); got != c.want {
			t.Errorf("%s: t is\n%s want\n%s", c.src, got, c.want)
		}
	}
}

// TestDoUpdateErrors: DO UPDATE that breaks a key, NOT NULL or a type
// fails, as do names it does not know. A subquery has no place in it.
func TestDoUpdateErrors(t *testing.T) {
	ss := upsertSession(t)
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
		is        error
	}{
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET a = 'y'", 1, 23, "", table.ErrUnique},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET id = 2", 1, 23, "", table.ErrExists},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET n = NULL", 1, 23, "NOT NULL", nil},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET b = 'text'", 1, 71, "column b is INTEGER", nil},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET b = excluded.nosuch", 1, 71, "no such column: excluded.nosuch", nil},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET nosuch = 1", 1, 67, "no such column: nosuch", nil},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET b = 1, b = 2", 1, 74, "column b set twice", nil},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET b = 1 WHERE b", 1, 79, "WHERE needs BOOLEAN", nil},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET b = (SELECT max(id) FROM t)", 1, 71, "a subquery cannot run here", nil},
		{"INSERT INTO t VALUES (3, 'x', 0, 0) ON CONFLICT (a) DO UPDATE SET b = 1 WHERE EXISTS (SELECT 1)", 1, 79, "a subquery cannot run here", nil},
	} {
		failsAt(t, ss, c.src, c.line, c.col, c.msg, c.is)
	}
	// The type of WHERE fails before the run, so also with no conflict,
	// where DO UPDATE never runs.
	failsAt(t, ss, "INSERT INTO t VALUES (9, 'new', 0, 0) ON CONFLICT (a) DO UPDATE SET b = 1 WHERE b", 1, 81, "WHERE needs BOOLEAN", nil)
	// The row of DO UPDATE passes the checks of the table, as in SQLite.
	ss.must("CREATE TABLE c (id INTEGER PRIMARY KEY, a INTEGER CHECK (a > 0))")
	ss.must("INSERT INTO c VALUES (1, 5)")
	failsAt(t, ss, "INSERT INTO c VALUES (1, 5) ON CONFLICT DO UPDATE SET a = -1", 1, 23, "", ErrCheck)
	// excluded is a name of DO UPDATE only, as in SQLite.
	if _, err := ss.query("SELECT excluded.a FROM t"); err == nil || !strings.Contains(err.Error(), "no such column: excluded.a") {
		t.Errorf("excluded outside DO UPDATE: %v", err)
	}
	if got := ss.dump("t"); got != "1 'x' 1 1\n2 'y' 2 2\n" {
		t.Errorf("t after only errors:\n%s", got)
	}
}
