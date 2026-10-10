package sqlexec

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// TestInSubquery checks x IN (SELECT ...) against the values SQLite
// 3.54.0 gives. NULL in the set or as x gives NULL unless a value
// equals x. An empty set holds nothing, not even NULL.
func TestInSubquery(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, s TEXT)")
	ss.must("CREATE INDEX t_a ON t (a)")
	ss.must("CREATE TABLE e (x INTEGER)")
	ss.must("INSERT INTO t VALUES (1, 1, 'x'), (2, 2, 'y'), (3, NULL, 'z')")
	for _, c := range []struct {
		src    string
		params []any
		want   string
	}{
		{"SELECT NULL IN (SELECT x FROM e), NULL NOT IN (SELECT x FROM e)", nil, "FALSE TRUE\n"},
		{"SELECT 1 IN (SELECT a FROM t), 3 IN (SELECT a FROM t), 3 NOT IN (SELECT a FROM t)", nil, "TRUE NULL NULL\n"},
		{"SELECT 3 IN (SELECT a FROM t WHERE a IS NOT NULL), NULL IN (SELECT a FROM t)", nil, "FALSE NULL\n"},
		{"SELECT 1.0 IN (SELECT a FROM t), 1.5 IN (SELECT a FROM t WHERE a > 0)", nil, "TRUE FALSE\n"},
		{"SELECT 'y' IN (SELECT s FROM t), 'q' NOT IN (SELECT s FROM t)", nil, "TRUE TRUE\n"},
		// The key and the index of a are no help to read the rows: the
		// subquery is no list of values.
		{"SELECT id FROM t WHERE id IN (SELECT a FROM t) ORDER BY id", nil, "1\n2\n"},
		{"SELECT id FROM t WHERE a IN (SELECT id FROM t WHERE id > 1)", nil, "2\n"},
		{"SELECT id FROM t WHERE id NOT IN (SELECT a FROM t WHERE a IS NOT NULL)", nil, "3\n"},
		{"SELECT id FROM t WHERE a IN (SELECT x FROM e)", nil, ""},
		{"SELECT id FROM t WHERE id IN (SELECT max(a) FROM t) OR s = 'x' ORDER BY id", nil, "1\n2\n"},
		{"SELECT count(*) FROM t WHERE a IN (SELECT a FROM t WHERE s IN (SELECT s FROM t WHERE id < 3))", nil, "2\n"},
		{"SELECT ? IN (SELECT a FROM t)", []any{int64(2)}, "TRUE\n"},
		// NaN equals nothing, in the set as well as as x.
		{"SELECT ? IN (SELECT a FROM t WHERE a > 0)", []any{math.NaN()}, "NULL\n"},
		{"SELECT 1 IN (SELECT ? FROM t)", []any{math.NaN()}, "NULL\n"},
		{"SELECT 1 IN (SELECT ? FROM e)", []any{math.NaN()}, "FALSE\n"},
	} {
		got, err := ss.query(c.src, c.params...)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.src, got, err, c.want)
		}
	}
	for _, c := range []struct {
		src       string
		params    []any
		line, col int
		msg       string
	}{
		{"SELECT 1 IN (SELECT a, s FROM t)", nil, 1, 14, "IN takes a subquery of one column, and (SELECT a, s FROM t) gives 2"},
		{"SELECT 1 IN (SELECT s FROM t)", nil, 1, 14, "do not compare"},
		{"SELECT 1 FROM e WHERE 1 IN (SELECT s FROM t)", nil, 1, 29, "do not compare"},
		// A type known only at run time: x, or the values of the set.
		{"SELECT ? IN (SELECT s FROM t)", []any{int64(1)}, 1, 14, "do not compare"},
		{"SELECT 1 IN (SELECT CASE WHEN id = 1 THEN ?1 ELSE ?2 END FROM t)", []any{int64(1), "x"}, 1, 14, "IN: the subquery (SELECT CASE WHEN (id = 1) THEN ?1 ELSE ?2 END FROM t) gives INTEGER and TEXT; they do not compare"},
		{"SELECT id FROM t WHERE id IN (SELECT x FROM e WHERE x = a)", nil, 1, 57, "roadmap step 32"},
		{"SELECT 1 IN (SELECT 1 / 0)", nil, 1, 23, "division by zero"},
	} {
		_, err := ss.query(c.src, c.params...)
		var e *sqlparse.Error
		if !errors.As(err, &e) || e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
}

// TestInSubqueryMemory: the values of a set count toward the memory of
// the statement.
func TestInSubqueryMemory(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (s TEXT)")
	for i := 0; i < 8; i++ {
		ss.must("INSERT INTO t VALUES (?)", strings.Repeat("x", 3000))
	}
	ss.lim = Limits{MaxMemory: 20000}
	if _, err := ss.query("SELECT 'x' IN (SELECT s FROM t LIMIT 6)"); err != nil {
		t.Errorf("6 values of 3000 bytes: %v", err)
	}
	_, err := ss.query("SELECT 'x' IN (SELECT s FROM t)")
	var e *sqlparse.Error
	if !errors.Is(err, ErrMemory) || !errors.As(err, &e) || e.At.Col != 16 {
		t.Errorf("8 values of 3000 bytes: %v", err)
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
	in, err := sqlparse.ParseExpr("1 IN (SELECT 1)")
	if err != nil {
		t.Fatal(err)
	}
	y, err := compileWith(in, nil, subs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := y.Eval(nil, nil); err == nil || !strings.Contains(err.Error(), "ran before its statement started") {
		t.Errorf("an unbound IN subquery: %v", err)
	}
	subs.bind(nil, nil)
	if _, err := Compile(e, nil); err == nil || !strings.Contains(err.Error(), "a subquery cannot run here") {
		t.Errorf("a subquery where none can run: %v", err)
	}
}

// scans counts the scans of each table.
type scans struct {
	*table.Tx
	n map[string]int
}

func (s scans) Scan(name string, o table.Options) (*table.Rows, error) {
	s.n[name]++
	return s.Tx.Scan(name, o)
}

// TestSubqueryRunsOnce: a subquery reads its table once in a run of its
// statement, however many rows need it, and again in the next run.
func TestSubqueryRunsOnce(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (a INTEGER)")
	for _, name := range []string{"u", "v", "w"} {
		ss.must("CREATE TABLE " + name + " (x INTEGER)")
		ss.must("INSERT INTO " + name + " VALUES (1), (2)")
	}
	ss.must("INSERT INTO t VALUES (1), (2), (3), (4)")
	st, err := sqlparse.Parse(`SELECT a FROM t WHERE a > (SELECT min(x) FROM u)
		AND EXISTS (SELECT x FROM v) AND a NOT IN (SELECT x FROM w)`)
	if err != nil {
		t.Fatal(err)
	}
	q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), ss.lim)
	if err != nil {
		t.Fatal(err)
	}
	src := scans{ss.tx, map[string]int{}}
	for run := 1; run <= 2; run++ {
		rows, err := q.Run(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil || n != 2 {
			t.Fatalf("run %d: %d rows, %v", run, n, err)
		}
		for _, name := range []string{"t", "u", "v", "w"} {
			if src.n[name] != run {
				t.Errorf("run %d: %d scans of %s", run, src.n[name], name)
			}
		}
	}
}
