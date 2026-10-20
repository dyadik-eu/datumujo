package sqlexec

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// correlatedSession holds the tables of the tests of correlated
// subqueries.
func correlatedSession(t *testing.T) *session {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, s TEXT)")
	ss.must("CREATE TABLE u (k INTEGER, v INTEGER)")
	ss.must("INSERT INTO t VALUES (1, 10, 'x'), (2, 20, 'y'), (3, NULL, 'z')")
	ss.must("INSERT INTO u VALUES (1, 5), (1, 6), (2, 7), (NULL, 8)")
	ss.must("CREATE TABLE m (a TEXT)")
	ss.must("INSERT INTO m VALUES ('p')")
	return ss
}

// TestCorrelated checks subqueries that read columns of the query around
// them (L-16): as a value, in EXISTS and in IN, over one level and two.
// Each expected result is the one of SQLite 3.54.0 for the same tables.
func TestCorrelated(t *testing.T) {
	ss := correlatedSession(t)
	for _, c := range []struct{ src, want string }{
		{"SELECT id, (SELECT max(v) FROM u WHERE u.k = t.id) FROM t ORDER BY id", "1 6\n2 7\n3 NULL\n"},
		{"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE k = id) ORDER BY id", "1\n2\n"},
		{"SELECT id FROM t WHERE NOT EXISTS (SELECT 1 FROM u WHERE k = id)", "3\n"},
		{"SELECT id FROM t WHERE 6 IN (SELECT v FROM u WHERE k = t.id)", "1\n"},
		{"SELECT id, 8 NOT IN (SELECT v FROM u WHERE k = t.id OR k IS NULL AND t.id = 3) FROM t ORDER BY id", "1 TRUE\n2 TRUE\n3 FALSE\n"},
		// Two levels: the innermost query reads u.k one level up and t.a
		// two levels up.
		{"SELECT id, (SELECT count(*) FROM u WHERE EXISTS (SELECT 1 FROM t AS w WHERE w.id = u.k AND w.a > t.a)) FROM t ORDER BY id", "1 1\n2 0\n3 0\n"},
		{"SELECT s, (SELECT (SELECT t.s || w.s FROM t AS w WHERE w.id = u.k) FROM u WHERE v = 7) FROM t ORDER BY id", "'x' 'xy'\n'y' 'yy'\n'z' 'zy'\n"},
		// A name of a table of the subquery is never one of the query
		// around: id is w.id here.
		{"SELECT id FROM t WHERE (SELECT count(*) FROM t AS w WHERE id < t.id) = 1", "2\n"},
		{"SELECT id FROM t ORDER BY (SELECT min(v) FROM u WHERE k = t.id) DESC, id", "2\n1\n3\n"},
		{"SELECT id, (SELECT count(*) FROM u WHERE v > t.id + 4) FROM t ORDER BY id", "1 3\n2 2\n3 1\n"},
		{"SELECT id, (SELECT v FROM u WHERE k = t.id AND v > ?1) FROM t ORDER BY id", "1 6\n2 7\n3 NULL\n"},
		// An aggregate that reads a column of the subquery too is one of
		// the subquery.
		{"SELECT id, (SELECT sum(t.a + v) FROM u) FROM t ORDER BY id", "1 66\n2 106\n3 NULL\n"},
		// The nearest query around that knows a name has it: a is m.a, a
		// TEXT, and not t.a, an INTEGER.
		{"SELECT id, (SELECT (SELECT a) FROM m) = 'p' FROM t ORDER BY id", "1 TRUE\n2 TRUE\n3 TRUE\n"},
	} {
		got, err := ss.query(c.src, int64(5))
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.src, got, err, c.want)
		}
	}
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		{"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE k = t.s)", 1, 58, "they do not compare"},
		{"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE k = t.nosuch)", 1, 58, "no such column: t.nosuch"},
		// A name that the query around has twice is an error there, as in
		// SQLite, and not a name of a query further out.
		{"SELECT (SELECT v FROM u WHERE k = a) FROM t JOIN t AS w ON TRUE", 1, 35, "ambiguous column name: a"},
		// SQLite makes sum(t.a) an aggregate of the query around.
		{"SELECT (SELECT sum(t.a)) FROM t", 1, 16, "sum(t.a) reads only columns of the query around it; such an aggregate is not supported"},
		{"SELECT (SELECT count(t.a) FROM u) FROM t", 1, 16, "is not supported"},
		// A name that no query knows, or one that is ambiguous in the
		// subquery, is the error of such an aggregate.
		{"SELECT (SELECT sum(t.a + nosuch) FROM u) FROM t", 1, 26, "no such column: nosuch"},
		{"SELECT (SELECT sum(a) FROM t AS p JOIN t AS q ON TRUE) FROM t", 1, 20, "ambiguous column name: a"},
		// LIMIT and OFFSET read no column of a query around, as in SQLite.
		{"SELECT (SELECT v FROM u LIMIT t.id) FROM t", 1, 31, "no such column: t.id"},
		{"SELECT (SELECT v FROM u LIMIT 1 OFFSET (SELECT t.id)) FROM t", 1, 48, "no such column: t.id"},
		// ON reads the tables up to its own. A name of a later table is an
		// error, and not a column of the query around of the same name.
		{"SELECT (SELECT count(*) FROM u AS p JOIN u AS q ON q.k = r.k JOIN u AS r ON TRUE) FROM u AS r", 1, 58, "no such column: r.k"},
	} {
		_, err := ss.query(c.src)
		var e *sqlparse.Error
		if !errors.As(err, &e) || e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
}

// TestCorrelatedRuns: a correlated subquery runs again when the values it
// reads from the query around change, and not while they stay the same.
// The rows of r come in key order, so the subquery sees 1, 1, 2, 2, 1.
func TestCorrelatedRuns(t *testing.T) {
	ss := correlatedSession(t)
	ss.must("CREATE TABLE r (id INTEGER PRIMARY KEY, k INTEGER)")
	ss.must("INSERT INTO r VALUES (1, 1), (2, 1), (3, 2), (4, 2), (5, 1)")
	for _, c := range []struct {
		src   string
		want  string
		scans int
	}{
		{"SELECT id, (SELECT max(v) FROM u WHERE u.k = r.k) FROM r", "1 6\n2 6\n3 7\n4 7\n5 6\n", 3},
		{"SELECT id FROM r WHERE EXISTS (SELECT 1 FROM u WHERE u.k = r.k AND v > 6)", "3\n4\n", 3},
		{"SELECT id FROM r WHERE 5 IN (SELECT v FROM u WHERE u.k = r.k)", "1\n2\n5\n", 3},
		// A subquery that reads no column of r runs once.
		{"SELECT id FROM r WHERE k = (SELECT min(k) FROM u)", "1\n2\n5\n", 1},
	} {
		st, err := sqlparse.Parse(c.src)
		if err != nil {
			t.Fatal(err)
		}
		q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), ss.lim)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		src := scans{ss.tx, map[string]int{}}
		rows, err := q.Run(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for rows.Next() {
			for i, v := range rows.Row() {
				if i > 0 {
					b.WriteString(" ")
				}
				b.WriteString(sqlLiteral(v))
			}
			b.WriteString("\n")
		}
		if err := rows.Err(); err != nil || b.String() != c.want {
			t.Errorf("%s: %q %v, want %q", c.src, b.String(), err, c.want)
		}
		if src.n["u"] != c.scans || src.n["r"] != 1 {
			t.Errorf("%s: %d scans of u and %d of r, want %d and 1", c.src, src.n["u"], src.n["r"], c.scans)
		}
	}
}

// TestSameValues: a correlated subquery keeps its answer only for the
// same values, bit for bit.
func TestSameValues(t *testing.T) {
	berlin := time.FixedZone("CET", 3600)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	nan := math.NaN()
	for _, c := range []struct {
		a, b []any
		same bool
	}{
		{[]any{}, []any{}, true},
		{[]any{int64(1), "x", nil, true}, []any{int64(1), "x", nil, true}, true},
		{[]any{int64(1)}, []any{int64(2)}, false},
		{[]any{int64(1)}, []any{1.0}, false},
		{[]any{nil}, []any{int64(0)}, false},
		{[]any{0.0}, []any{math.Copysign(0, -1)}, false},
		{[]any{2.5}, []any{2.5}, true},
		{[]any{nan}, []any{nan}, false},
		{[]any{[]byte{1, 2}}, []any{[]byte{1, 2}}, true},
		{[]any{[]byte{1, 2}}, []any{[]byte{1, 3}}, false},
		{[]any{[]byte{}}, []any{""}, false},
		{[]any{at}, []any{at}, true},
		{[]any{at}, []any{at.In(berlin)}, false},
		{[]any{at}, []any{at.Add(time.Nanosecond)}, false},
	} {
		if got := sameValues(c.a, c.b); got != c.same {
			t.Errorf("sameValues(%v, %v) = %v", c.a, c.b, got)
		}
	}
}

// TestCorrelatedWrites checks correlated subqueries in UPDATE and DELETE.
// Each reads the table as it was before the statement: UPDATE computes
// every new row before it writes the first, and DELETE finds every row
// first. In SQLite 3.54.0, the UPDATE of s below gives 1, 3, 7: each row
// sees the rows written before it. Here it gives 1, 3, 6, the sums over
// the old rows, whatever the order of the rows. The other results are
// those of SQLite.
func TestCorrelatedWrites(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER)")
	ss.must("INSERT INTO t VALUES (1, 1), (2, 2), (3, 3)")
	ss.must("CREATE TABLE u (k INTEGER, v INTEGER)")
	ss.must("INSERT INTO u VALUES (1, 5), (1, 6), (2, 7)")
	for _, c := range []struct {
		src, table, want string
		n                int64
	}{
		{"UPDATE t SET a = (SELECT max(v) FROM u WHERE u.k = t.id) WHERE EXISTS (SELECT 1 FROM u WHERE u.k = t.id)", "t", "1 6\n2 7\n3 3\n", 2},
		{"DELETE FROM u WHERE v < (SELECT max(w.v) FROM u AS w WHERE w.k = u.k)", "u", "2 1 6\n3 2 7\n", 1},
		{"DELETE FROM t WHERE NOT EXISTS (SELECT 1 FROM u WHERE u.k = t.id)", "t", "1 6\n2 7\n", 1},
		{"CREATE TABLE s (id INTEGER PRIMARY KEY, a INTEGER)", "", "", 0},
		{"INSERT INTO s VALUES (1, 1), (2, 2), (3, 3)", "", "", 3},
		{"UPDATE s SET a = (SELECT sum(w.a) FROM s AS w WHERE w.id <= s.id)", "s", "1 1\n2 3\n3 6\n", 3},
	} {
		r, err := ss.exec(c.src)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		if r.RowsAffected != c.n {
			t.Errorf("%s: %d rows, want %d", c.src, r.RowsAffected, c.n)
		}
		if c.table != "" {
			if got := ss.dump(c.table); got != c.want {
				t.Errorf("%s: %s is\n%s want\n%s", c.src, c.table, got, c.want)
			}
		}
	}
	// INSERT ... VALUES has no row of its table to read, as in SQLite.
	failsAt(t, ss, "INSERT INTO t VALUES (5, (SELECT t.id))", 1, 34, "no such column: t.id", nil)
	failsAt(t, ss, "INSERT INTO t VALUES (5, (SELECT a))", 1, 34, "no such column: a", nil)
}

// TestCorrelatedGroups: a subquery in a query with GROUP BY reads a key
// of GROUP BY, also as t.a for GROUP BY a. Any other column of the table
// is an error, as it is outside a subquery. The expected results are
// those of SQLite 3.54.0.
func TestCorrelatedGroups(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, a INTEGER, s TEXT)")
	ss.must("INSERT INTO t VALUES (1, 1, 'x'), (2, 1, 'y'), (3, 2, 'z'), (4, NULL, 'w')")
	ss.must("CREATE TABLE u (k INTEGER, v INTEGER)")
	ss.must("INSERT INTO u VALUES (1, 5), (1, 6), (2, 7)")
	for _, c := range []struct{ src, want string }{
		{"SELECT a, (SELECT count(*) FROM u WHERE u.k = t.a) FROM t GROUP BY a ORDER BY a", "NULL 0\n1 2\n2 1\n"},
		{"SELECT a, (SELECT count(*) FROM u WHERE u.k = a) FROM t GROUP BY t.a ORDER BY a", "NULL 0\n1 2\n2 1\n"},
		// The key a is the second slot of the group row.
		{"SELECT s, (SELECT count(*) FROM u WHERE u.k = t.a) FROM t GROUP BY s, a ORDER BY s", "'w' 0\n'x' 2\n'y' 2\n'z' 1\n"},
		{"SELECT a, count(*) FROM t GROUP BY a HAVING EXISTS (SELECT 1 FROM u WHERE u.k = a) ORDER BY a", "1 2\n2 1\n"},
		{"SELECT a FROM t GROUP BY a ORDER BY (SELECT max(v) FROM u WHERE u.k = t.a) DESC", "2\n1\nNULL\n"},
		// Outside a subquery too: t.a is the key a.
		{"SELECT t.a FROM t GROUP BY a ORDER BY 1", "NULL\n1\n2\n"},
		{"SELECT a, t.a FROM t GROUP BY a, t.a ORDER BY 1", "NULL NULL\n1 1\n2 2\n"},
	} {
		got, err := ss.query(c.src)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.src, got, err, c.want)
		}
	}
	for _, c := range []struct {
		src       string
		line, col int
		msg       string
	}{
		// SQLite takes id from some row of the group, a value no rule
		// picks.
		{"SELECT a, (SELECT count(*) FROM u WHERE u.k = t.id) FROM t GROUP BY a", 1, 47, "column t.id must be in GROUP BY or in an aggregate function"},
		// A key that is an expression matches by its text only. SQLite
		// matches t.a + 1 in the subquery with GROUP BY a + 1.
		{"SELECT a + 1, (SELECT max(v) FROM u WHERE u.k + 1 = t.a + 1) FROM t GROUP BY a + 1", 1, 53, "column t.a must be in GROUP BY"},
		{"SELECT a, (SELECT sum(t.id) FROM u) FROM t GROUP BY a", 1, 19, "reads only columns of the query around it"},
	} {
		_, err := ss.query(c.src)
		var e *sqlparse.Error
		if !errors.As(err, &e) || e.At.Line != c.line || e.At.Col != c.col || !strings.Contains(e.Error(), c.msg) {
			t.Errorf("%s: %v\n want line %d, column %d: ...%s...", c.src, err, c.line, c.col, c.msg)
		}
	}
}

// scanLog notes the options of each scan of one table.
type scanLog struct {
	*table.Tx
	table string
	log   []string
}

func (s *scanLog) Scan(name string, o table.Options) (*table.Rows, error) {
	if name == s.table {
		s.log = append(s.log, fmt.Sprintf("%s %v %v", o.Index, o.Prefix, o.From))
	}
	return s.Tx.Scan(name, o)
}

// TestCorrelatedPlan: a correlated subquery reads its table by an index
// with the values of the row it runs for, not all rows for each row. It
// gives the rows that a scan of all rows gives.
func TestCorrelatedPlan(t *testing.T) {
	ss := correlatedSession(t)
	ss.must("CREATE INDEX u_k ON u (k)")
	for _, c := range []struct {
		src  string
		want string
		log  []string
	}{
		{"SELECT id, (SELECT max(v) FROM u WHERE u.k = t.id) FROM t", "1 6\n2 7\n3 NULL\n",
			[]string{"u_k [1] []", "u_k [2] []", "u_k [3] []"}},
		{"SELECT id FROM t WHERE EXISTS (SELECT 1 FROM u WHERE k > t.id)", "1\n",
			[]string{"u_k [] [1]", "u_k [] [2]", "u_k [] [3]"}},
		{"SELECT id FROM t WHERE 6 IN (SELECT v FROM u WHERE t.id = k)", "1\n",
			[]string{"u_k [1] []", "u_k [2] []", "u_k [3] []"}},
		// Two levels: the inner subquery reads u by t.a of the outer query.
		// For t.a NULL, u.k = NULL holds for no row, and u is not read.
		{"SELECT id, (SELECT (SELECT count(*) FROM u WHERE u.k = t.a / 10) FROM m) FROM t", "1 2\n2 1\n3 0\n",
			[]string{"u_k [1] []", "u_k [2] []"}},
	} {
		for _, noIndex := range []bool{false, true} {
			st, err := sqlparse.Parse(c.src)
			if err != nil {
				t.Fatal(err)
			}
			q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), Limits{NoIndex: noIndex})
			if err != nil {
				t.Fatalf("%s: %v", c.src, err)
			}
			src := &scanLog{Tx: ss.tx, table: "u"}
			rows, err := q.Run(src, nil)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for rows.Next() {
				for i, v := range rows.Row() {
					if i > 0 {
						b.WriteString(" ")
					}
					b.WriteString(sqlLiteral(v))
				}
				b.WriteString("\n")
			}
			if err := rows.Err(); err != nil || b.String() != c.want {
				t.Errorf("%s, NoIndex %v: %q %v, want %q", c.src, noIndex, b.String(), err, c.want)
			}
			if !noIndex && strings.Join(src.log, ", ") != strings.Join(c.log, ", ") {
				t.Errorf("%s: scans of u\n %s\nwant\n %s", c.src, strings.Join(src.log, ", "), strings.Join(c.log, ", "))
			}
		}
	}
}

// TestPlanValueNoSubquery: a value of the planner holds no subquery. The
// subquery below gives three rows, an error for a value. Over an empty
// table no row needs it, so it never runs, also with an index on x.
func TestPlanValueNoSubquery(t *testing.T) {
	ss := correlatedSession(t)
	ss.must("CREATE TABLE e (x INTEGER)")
	ss.must("CREATE INDEX e_x ON e (x)")
	if got, err := ss.query("SELECT x FROM e WHERE x = (SELECT a FROM t)"); err != nil || got != "" {
		t.Errorf("%q %v", got, err)
	}
}
