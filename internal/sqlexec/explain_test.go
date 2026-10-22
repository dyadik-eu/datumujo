package sqlexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// explain runs EXPLAIN over the schema of the session and returns its
// rows, one line each.
func (ss *session) explain(src string) (string, error) {
	st, err := sqlparse.Parse(src)
	if err != nil {
		return "", err
	}
	ex, ok := st.(*sqlparse.Explain)
	if !ok {
		ss.t.Fatalf("%s is no EXPLAIN", src)
	}
	rows, err := Explain(ss.tx.Schema(), ex, ss.lim)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for rows.Next() {
		b.WriteString(rows.Row()[0].(string) + "\n")
	}
	return b.String(), rows.Err()
}

// TestExplain checks EXPLAIN (L-18): a row for the statement, then a row
// for each subquery, numbered in the order the engine compiles them.
func TestExplain(t *testing.T) {
	ss := correlatedSession(t)
	ss.must("CREATE INDEX u_k ON u (k)")
	for _, c := range []struct{ src, want string }{
		{"EXPLAIN SELECT 1", "ONE ROW\n"},
		{"EXPLAIN SELECT id, (SELECT max(v) FROM u WHERE u.k = t.id) FROM t WHERE id = 2",
			"SEARCH t USING PRIMARY KEY ((id = 2))\n" +
				"SUBQUERY 1 (SELECT max(v) FROM u WHERE (u.k = t.id)), CORRELATED: SEARCH u USING INDEX u_k ((u.k = t.id)); GROUP\n"},
		{"EXPLAIN QUERY PLAN SELECT a FROM t WHERE EXISTS (SELECT 1 FROM u WHERE k IN (SELECT id FROM t)) ORDER BY a",
			"SCAN t; SORT\n" +
				"SUBQUERY 1 (SELECT 1 FROM u WHERE (k IN (SELECT id FROM t))): SCAN u\n" +
				"SUBQUERY 1.1 (SELECT id FROM t): SCAN t\n"},
		{"EXPLAIN SELECT (SELECT 1), (SELECT 2)", "ONE ROW\nSUBQUERY 1 (SELECT 1): ONE ROW\nSUBQUERY 2 (SELECT 2): ONE ROW\n"},
		{"EXPLAIN INSERT INTO t VALUES (9, 1, 'q'), (10, 2, 'r') ON CONFLICT DO UPDATE SET a = excluded.a",
			"INSERT INTO t: 2 ROWS\nON CONFLICT: LOOK UP PRIMARY KEY; DO UPDATE\n"},
		{"EXPLAIN INSERT INTO u VALUES ((SELECT count(*) FROM t), 1)",
			"INSERT INTO u: 1 ROW\nSUBQUERY 1 (SELECT count(*) FROM t): SCAN t; GROUP\n"},
		{"EXPLAIN UPDATE u SET v = 1 WHERE k = 1", "UPDATE u: SEARCH u USING INDEX u_k ((k = 1))\n"},
		{"EXPLAIN DELETE FROM t WHERE a > (SELECT min(v) FROM u)",
			"DELETE FROM t: SCAN t\nSUBQUERY 1 (SELECT min(v) FROM u): SCAN u; GROUP\n"},
	} {
		got, err := ss.explain(c.src)
		if err != nil || got != c.want {
			t.Errorf("%s:\n%s%v\nwant\n%s", c.src, got, err, c.want)
		}
	}
	// The keys of ON CONFLICT: every key without a target, the one key
	// with it.
	ss.must("CREATE TABLE w (id INTEGER PRIMARY KEY, a TEXT UNIQUE)")
	for _, c := range []struct{ src, want string }{
		{"EXPLAIN INSERT INTO w VALUES (1, 'a') ON CONFLICT DO NOTHING", "ON CONFLICT: LOOK UP PRIMARY KEY, INDEX w_unique_1; DO NOTHING"},
		{"EXPLAIN INSERT INTO w VALUES (1, 'a') ON CONFLICT (a) DO NOTHING", "ON CONFLICT: LOOK UP INDEX w_unique_1; DO NOTHING"},
	} {
		got, err := ss.explain(c.src)
		if err != nil || !strings.Contains(got, c.want+"\n") {
			t.Errorf("%s:\n%s%v\nwant a row %s", c.src, got, err, c.want)
		}
	}
	// With NoIndex, the plan reads every row, as the run does.
	ss.lim = Limits{NoIndex: true}
	if got, err := ss.explain("EXPLAIN UPDATE u SET v = 1 WHERE k = 1"); err != nil || got != "UPDATE u: SCAN u\n" {
		t.Errorf("NoIndex: %q %v", got, err)
	}
}

// TestExplainRunsNothing: EXPLAIN writes no row and runs no subquery.
// The subquery below gives three rows, an error at a run.
func TestExplainRunsNothing(t *testing.T) {
	ss := correlatedSession(t)
	before := ss.dump("t")
	for _, src := range []string{
		"EXPLAIN DELETE FROM t",
		"EXPLAIN UPDATE t SET a = 0",
		"EXPLAIN INSERT INTO t VALUES (9, 9, 'q')",
		"EXPLAIN SELECT (SELECT a FROM t)",
	} {
		if _, err := ss.explain(src); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	if got := ss.dump("t"); got != before {
		t.Errorf("t after EXPLAIN:\n%s want\n%s", got, before)
	}
	if _, err := ss.query("SELECT (SELECT a FROM t)"); err == nil {
		t.Error("the subquery runs without an error, so the case proves nothing")
	}
}

// TestExplainErrors: a statement that does not compile fails under
// EXPLAIN too, at the same place. Exec does not run EXPLAIN.
func TestExplainErrors(t *testing.T) {
	ss := correlatedSession(t)
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"EXPLAIN SELECT nosuch FROM t", 1, 16, "no such column: nosuch"},
		{"EXPLAIN SELECT a FROM nosuch", 1, 23, "no such table: nosuch"},
		{"EXPLAIN UPDATE t SET a = 'x'", 1, 26, "column a is INTEGER"},
		{"EXPLAIN DELETE FROM t WHERE a", 1, 29, "WHERE needs BOOLEAN"},
		{"EXPLAIN INSERT INTO t VALUES (1)", 1, 31, "1 values for 3 columns"},
		{"EXPLAIN INSERT INTO t VALUES (1, 1, 'a') ON CONFLICT (s) DO NOTHING", 1, 42, "no PRIMARY KEY or UNIQUE"},
	} {
		_, err := ss.explain(c.src)
		var e *sqlparse.Error
		if !errors.As(err, &e) || e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
	if _, err := ss.exec("EXPLAIN SELECT 1"); !errors.Is(err, ErrStatement) || !strings.Contains(err.Error(), "EXPLAIN returns rows; run it with Query") {
		t.Errorf("Exec of EXPLAIN: %v", err)
	}
	// The parser takes no other statement after EXPLAIN; Explain says so
	// too, for a caller that builds the tree.
	if _, err := Explain(ss.tx.Schema(), &sqlparse.Explain{Stmt: &sqlparse.Begin{}}, ss.lim); err == nil || !strings.Contains(err.Error(), "EXPLAIN takes SELECT, INSERT, UPDATE or DELETE") {
		t.Errorf("EXPLAIN BEGIN: %v", err)
	}
	// The rows have one TEXT column, plan.
	st, _ := sqlparse.Parse("EXPLAIN SELECT 1")
	rows, err := Explain(ss.tx.Schema(), st.(*sqlparse.Explain), ss.lim)
	if err != nil {
		t.Fatal(err)
	}
	if cols := rows.Columns(); len(cols) != 1 || cols[0].Name != "plan" || cols[0].Type != table.String || !cols[0].Known {
		t.Errorf("columns: %+v", cols)
	}
}
