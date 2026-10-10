package sqlexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// TestSubqueryNotYet: the parser reads IN (SELECT ...) before the
// engine runs it. Until it does, it fails as not supported. It must not
// run as IN with an empty list, which is FALSE for every row.
func TestSubqueryNotYet(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a INTEGER)")
	ss.must("INSERT INTO t VALUES (1)")
	for _, src := range []string{
		"SELECT a FROM t WHERE a IN (SELECT a FROM t)",
		"SELECT a FROM t WHERE a NOT IN (SELECT a FROM t)",
	} {
		_, err := ss.query(src)
		if !errors.Is(err, ErrStatement) || !strings.Contains(err.Error(), "roadmap step 31") {
			t.Errorf("%s: %v", src, err)
		}
	}
}

// TestScalarSubquery checks (SELECT ...) as a value (L-16): the column
// of its row, NULL without a row, and an error with more than one row.
func TestScalarSubquery(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, s TEXT)")
	ss.must("CREATE TABLE e (x INTEGER)")
	ss.must("INSERT INTO t VALUES (1, 10, 'x'), (2, 20, 'y'), (3, NULL, 'z')")
	for _, c := range []struct{ src, want string }{
		{"SELECT (SELECT max(a) FROM t)", "20\n"},
		{"SELECT (SELECT a FROM t WHERE id = 3)", "NULL\n"},
		{"SELECT (SELECT x FROM e)", "NULL\n"},
		{"SELECT id FROM t WHERE a = (SELECT min(a) FROM t)", "1\n"},
		{"SELECT id, (SELECT s FROM t WHERE id = 2) FROM t ORDER BY id", "1 'y'\n2 'y'\n3 'y'\n"},
		{"SELECT id FROM t ORDER BY (SELECT 1), id DESC LIMIT (SELECT count(*) - 1 FROM t)", "3\n2\n"},
		{"SELECT sum(a + (SELECT 1)), (SELECT 5) FROM t", "32 5\n"},
		{"SELECT s, count(*) FROM t GROUP BY s HAVING count(*) = (SELECT 1) ORDER BY s LIMIT 1", "'x' 1\n"},
		{"SELECT (SELECT (SELECT a FROM t WHERE id = 2) + 1)", "21\n"},
		{"SELECT (SELECT a FROM t WHERE id = ?)", "10\n"},
		{"SELECT ?2 + (SELECT a FROM t WHERE id = ?1)", "17\n"},
		// The subquery a row never needs never runs: two rows are not an
		// error here.
		{"SELECT CASE WHEN FALSE THEN (SELECT a FROM t) ELSE 5 END", "5\n"},
		{"SELECT x FROM e WHERE (SELECT a FROM t) = 1", ""},
	} {
		got, err := ss.query(c.src, int64(1), int64(7))
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.src, got, err, c.want)
		}
	}
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"SELECT (SELECT a FROM t)", 1, 8, "gives more than one row, and a subquery as a value needs at most one"},
		{"SELECT (SELECT a, s FROM t)", 1, 8, "a subquery as a value gives one column, and (SELECT a, s FROM t) gives 2"},
		{"SELECT (SELECT * FROM t)", 1, 8, "gives 3"},
		{"SELECT 1 FROM t WHERE (SELECT s FROM t WHERE id = 1) = 1", 1, 56, "do not compare"},
		// The type of a subquery is known before the run: an empty table
		// does not hide the error.
		{"SELECT x FROM e WHERE (SELECT s FROM t WHERE id = 1) = 1", 1, 56, "do not compare"},
		{"SELECT (SELECT 1 / 0)", 1, 18, "division by zero"},
		{"SELECT (SELECT nosuch FROM t)", 1, 16, "no such column: nosuch"},
		{"SELECT id FROM t WHERE (SELECT a FROM e) = 1", 1, 32, "a subquery that reads a column of the query around it is not supported yet (roadmap step 32)"},
		{"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM e WHERE x = id)", 1, 58, "roadmap step 32"},
		{"SELECT (SELECT (SELECT s) FROM e) FROM t", 1, 24, "roadmap step 32"},
	} {
		_, err := ss.query(c.src)
		var e *sqlparse.Error
		if !errors.As(err, &e) || e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
}

// TestExists checks EXISTS (SELECT ...): TRUE if the query has a row,
// whatever its columns hold.
func TestExists(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a INTEGER)")
	ss.must("CREATE TABLE e (x INTEGER)")
	ss.must("INSERT INTO t VALUES (NULL), (2)")
	for _, c := range []struct{ src, want string }{
		{"SELECT EXISTS (SELECT 1 FROM t), EXISTS (SELECT * FROM e), NOT EXISTS (SELECT x FROM e)", "TRUE FALSE TRUE\n"},
		{"SELECT EXISTS (SELECT a FROM t WHERE a IS NULL)", "TRUE\n"},
		{"SELECT EXISTS (SELECT a FROM t WHERE a > ?)", "FALSE\n"},
		{"SELECT count(*) FROM t WHERE EXISTS (SELECT 1 FROM e)", "0\n"},
		{"SELECT count(*) FROM t WHERE NOT EXISTS (SELECT 1 FROM e)", "2\n"},
		{"SELECT EXISTS (SELECT 1 FROM t LIMIT 0)", "FALSE\n"},
	} {
		got, err := ss.query(c.src, int64(5))
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.src, got, err, c.want)
		}
	}
}

// TestSubqueryInWrites: INSERT, UPDATE and DELETE run subqueries too.
// Each reads the table as it was before the statement, as in SQLite. An
// INSERT of three rows sees the table empty in each of them.
func TestSubqueryInWrites(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER)")
	ss.must("INSERT INTO t (a) VALUES ((SELECT count(*) FROM t)), ((SELECT count(*) FROM t)), ((SELECT count(*) + 10 FROM t))")
	if got := ss.dump("t"); got != "1 0\n2 0\n3 10\n" {
		t.Errorf("after INSERT: %q", got)
	}
	ss.must("UPDATE t SET a = (SELECT max(a) FROM t) + 1 WHERE a < (SELECT max(a) FROM t)")
	if got := ss.dump("t"); got != "1 11\n2 11\n3 10\n" {
		t.Errorf("after UPDATE: %q", got)
	}
	r := ss.must("DELETE FROM t WHERE a = (SELECT max(a) FROM t) AND EXISTS (SELECT 1 FROM t WHERE id = 3)")
	if got := ss.dump("t"); got != "3 10\n" || r.RowsAffected != 2 {
		t.Errorf("after DELETE of %d: %q", r.RowsAffected, got)
	}
	ss.must("INSERT INTO t VALUES (4, 10)")
	failsAt(t, ss, "INSERT INTO t (a) VALUES ((SELECT a FROM t))", 1, 27, "gives more than one row", nil)
	// A CHECK takes no subquery, as in SQLite.
	failsAt(t, ss, "CREATE TABLE c (n INTEGER CHECK (n > (SELECT 1)))", 1, 38, "a CHECK takes no subquery", nil)
	failsAt(t, ss, "CREATE TABLE c (n INTEGER CHECK (EXISTS (SELECT 1)))", 1, 34, "a CHECK takes no subquery", nil)
	failsAt(t, ss, "CREATE TABLE c (n INTEGER CHECK (n IN (SELECT 1)))", 1, 36, "a CHECK takes no subquery", nil)
}

// TestSubqueryUnbound: a subquery that runs before its statement started
// a run is an error, not a run on no source.
func TestSubqueryUnbound(t *testing.T) {
	e, err := sqlparse.ParseExpr("(SELECT 1)")
	if err != nil {
		t.Fatal(err)
	}
	subs := newSubqueries(&table.Schema{}, Limits{}, nil)
	x, err := compileWith(e, nil, subs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Eval(nil, nil); err == nil || !strings.Contains(err.Error(), "ran before its statement started") {
		t.Errorf("an unbound subquery: %v", err)
	}
	subs.bind(nil, nil)
	if _, err := Compile(e, nil); err == nil || !strings.Contains(err.Error(), "a subquery cannot run here") {
		t.Errorf("a subquery where none can run: %v", err)
	}
}
