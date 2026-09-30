package sqlexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// failsAt checks that a statement fails at a position with a message,
// and that the error matches cause if cause is not nil.
func failsAt(t *testing.T, ss *session, src string, line, col int, msg string, cause error) {
	t.Helper()
	_, err := ss.exec(src)
	var e *sqlparse.Error
	if !errors.As(err, &e) {
		t.Errorf("%s: %v, want an error at a position", src, err)
		return
	}
	if e.At.Line != line || e.At.Col != col || !strings.Contains(e.Error(), msg) {
		t.Errorf("%s: %v\n want line %d, column %d: ...%s...", src, err, line, col, msg)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Errorf("%s: %v does not match %v", src, err, cause)
	}
}

// TestChecks checks L-14: an INSERT or UPDATE that makes a CHECK FALSE
// fails and changes no row. NULL passes. A check of a column can read
// other columns, and the checks hold after a commit.
func TestChecks(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must(`CREATE TABLE t (id INTEGER PRIMARY KEY CHECK (id < 100), a INTEGER CHECK (a > 0) CHECK (a <> 7),
		b TEXT, lo INTEGER, hi INTEGER CHECK (lo <= hi), CHECK (length(b) < 3))`)
	ss.commit()
	ss.must("INSERT INTO t VALUES (1, 5, 'ab', 1, 2)")
	ss.must("INSERT INTO t (id, a) VALUES (2, NULL)")
	ss.must("INSERT INTO t (id, lo) VALUES (3, 9)")
	failsAt(t, ss, "INSERT INTO t VALUES (4, 0, 'x', 1, 1)", 1, 23, "CHECK ((a > 0)) of table t is false", ErrCheck)
	failsAt(t, ss, "INSERT INTO t (id, a) VALUES (4, 7)", 1, 31, "CHECK ((a != 7))", ErrCheck)
	failsAt(t, ss, "INSERT INTO t (id, b) VALUES (4, 'long')", 1, 31, "CHECK ((length(b) < 3))", ErrCheck)
	failsAt(t, ss, "INSERT INTO t (id, lo, hi) VALUES (4, 5, 4)", 1, 36, "CHECK ((lo <= hi))", ErrCheck)
	failsAt(t, ss, "INSERT INTO t (id) VALUES (100)", 1, 28, "CHECK ((id < 100))", ErrCheck)
	// The first row is fine, the second is not: the INSERT writes none.
	failsAt(t, ss, "INSERT INTO t (id, a) VALUES (5, 1), (6, -1)", 1, 39, "CHECK ((a > 0))", ErrCheck)
	failsAt(t, ss, "UPDATE t SET a = a - 5", 1, 1, "CHECK ((a > 0))", ErrCheck)
	failsAt(t, ss, "UPDATE t SET hi = 0 WHERE id = 1", 1, 1, "CHECK ((lo <= hi))", ErrCheck)
	ss.must("UPDATE t SET a = a + 1")
	want := "1 6 'ab' 1 2\n2 NULL NULL NULL NULL\n3 NULL NULL 9 NULL\n"
	if got := ss.dump("t"); got != want {
		t.Errorf("t:\n%s\nwant\n%s", got, want)
	}
	// An error of a check is an error of the statement, at its position.
	ss.must("CREATE TABLE d (n INTEGER, CHECK (10 / n > 1))")
	failsAt(t, ss, "INSERT INTO d VALUES (0)", 1, 23, "CHECK (((10 / n) > 1)) of table d: division by zero", nil)
	ss.must("INSERT INTO d VALUES (2)")
}

// TestCheckErrors: a CHECK that is not a BOOLEAN of the columns of its
// table is an error when the table is made.
func TestCheckErrors(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY)")
	ss.must("INSERT INTO t VALUES (1), (2)")
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"CREATE TABLE u (a INTEGER CHECK (a > ?))", 1, 38, "a CHECK takes no parameter"},
		{"CREATE TABLE u (a INTEGER CHECK (count(*) > 0))", 1, 34, "a CHECK takes no aggregate"},
		{"CREATE TABLE u (a INTEGER CHECK (a))", 1, 34, "CHECK needs BOOLEAN, and a is INTEGER"},
		{"CREATE TABLE u (a INTEGER CHECK (NULL))", 1, 34, "CHECK needs BOOLEAN, and NULL has no type"},
		{"CREATE TABLE u (a INTEGER, CHECK (b > 0))", 1, 35, "no such column: b"},
		{"CREATE TABLE u (a INTEGER CHECK (a > 'x'))", 1, 38, "do not compare"},
		{"ALTER TABLE t ADD COLUMN n INTEGER CHECK (m > 0)", 1, 43, "no such column: m"},
		{"ALTER TABLE t ADD COLUMN n INTEGER DEFAULT 5 CHECK (n > 10)", 1, 1, "CHECK ((n > 10)) of table t is false"},
		{"ALTER TABLE t ADD COLUMN n INTEGER CHECK (n IS NOT NULL)", 1, 1, "CHECK ((n IS NOT NULL)) of table t is false"},
	} {
		failsAt(t, ss, c.src, c.line, c.col, c.msg, nil)
	}
	if n := len(ss.tx.Schema().Tables); n != 1 {
		t.Errorf("%d tables after only rejected ones", n)
	}
	tb, _ := ss.tx.Schema().Table("t")
	if len(tb.Columns) != 1 || len(tb.Checks) != 0 {
		t.Errorf("a rejected ALTER TABLE changed t: %+v", tb)
	}
	if ss.stx.Version() != 1 {
		t.Errorf("format version %d after rejected checks only", ss.stx.Version())
	}
}

// TestAddColumnCheck: ALTER TABLE tests the rows that exist against the
// checks of the new column, with its fill, as SQLite does. A row added
// later is tested as always.
func TestAddColumnCheck(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY)")
	ss.must("INSERT INTO t VALUES (1), (2)")
	ss.must("ALTER TABLE t ADD COLUMN n INTEGER CHECK (n > 10)")
	ss.must("ALTER TABLE t ADD COLUMN m INTEGER NOT NULL DEFAULT 3 CHECK (m < id + 3)")
	failsAt(t, ss, "INSERT INTO t (id, n) VALUES (3, 1)", 1, 31, "CHECK ((n > 10))", ErrCheck)
	failsAt(t, ss, "INSERT INTO t (id) VALUES (0)", 1, 28, "CHECK ((m < (id + 3)))", ErrCheck)
	ss.must("INSERT INTO t (id, n) VALUES (3, 11)")
	tb, _ := ss.tx.Schema().Table("t")
	if strings.Join(tb.Checks, "; ") != "(n > 10); (m < (id + 3))" {
		t.Errorf("checks in the schema: %q", tb.Checks)
	}
	if ss.stx.Version() != 2 {
		t.Errorf("format version %d with checks", ss.stx.Version())
	}
}

// TestCheckDamaged: a check in the schema that does not compile is
// damage, found by the first statement that writes the table.
func TestCheckDamaged(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER)")
	if err := ss.tx.AddColumn("t", table.Column{Name: "b", Type: table.Int64, Null: true}, "(nosuch > 0)"); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"INSERT INTO t (id) VALUES (1)", "UPDATE t SET a = 1"} {
		if _, err := ss.exec(src); !errors.Is(err, table.ErrDamaged) {
			t.Errorf("%s with a check that does not compile: %v", src, err)
		}
	}
}
