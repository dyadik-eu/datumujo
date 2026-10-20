package sqlexec

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlitetest"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// TestSubqueryAgainstSQLite runs a random script of queries and writes
// with subqueries in SQLite and here (L-16, P-4). The subqueries give a
// value, a test for a row, or a set after IN, and some hold another
// subquery. Small sets of values with NULL make IN give TRUE, FALSE and
// NULL often. A subquery as a value is an aggregate or reads one key, so
// it gives at most one row. With more rows, SQLite takes the first one
// and this package fails.
//
// Each statement must fail in both or in
// neither. Each query must give the rows of SQLite at its place in the
// script, and both tables must hold the same rows at the end.
func TestSubqueryAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{31, 1031} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { subqueryAgainstSQLite(t, seed) })
	}
}

// subqueryGen writes random subqueries over p (id, a, b) and q (x, y).
// a and x are INTEGER, b and y are TEXT.
type subqueryGen struct{ r *rand.Rand }

func (g subqueryGen) pick(xs ...string) string { return xs[g.r.Intn(len(xs))] }

func (g subqueryGen) num() string {
	if g.r.Intn(8) == 0 {
		return "NULL"
	}
	return strconv.Itoa(g.r.Intn(6))
}

func (g subqueryGen) text() string {
	if g.r.Intn(8) == 0 {
		return "NULL"
	}
	return g.pick("'a'", "'b'", "'c'", "'d'")
}

// numValue is a subquery of at most one INTEGER row.
func (g subqueryGen) numValue() string {
	n := strconv.Itoa(g.r.Intn(6))
	return g.pick(
		"(SELECT max(x) FROM q)",
		"(SELECT min(a) FROM p WHERE b = "+g.text()+")",
		"(SELECT count(*) FROM q WHERE x > "+n+")",
		"(SELECT a FROM p WHERE id = "+strconv.Itoa(1+g.r.Intn(12))+")",
		"(SELECT sum(x) FROM q WHERE y IN ('a', 'b'))",
		"(SELECT count(*) FROM p WHERE a IN "+g.numSet(false)+")",
	)
}

// textValue is a subquery of at most one TEXT row.
func (g subqueryGen) textValue() string {
	return g.pick(
		"(SELECT max(y) FROM q)",
		"(SELECT b FROM p WHERE id = "+strconv.Itoa(1+g.r.Intn(12))+")",
		"(SELECT min(b) FROM p WHERE a > "+strconv.Itoa(g.r.Intn(6))+")",
	)
}

// numSet is a subquery of INTEGER rows for IN. With deep, it may hold
// another subquery.
func (g subqueryGen) numSet(deep bool) string {
	n := strconv.Itoa(g.r.Intn(6))
	sets := []string{
		"(SELECT x FROM q)",
		"(SELECT x FROM q WHERE y IS NOT NULL)",
		"(SELECT DISTINCT a FROM p)",
		"(SELECT a FROM p WHERE b IN ('a', 'b') AND a IS NOT NULL)",
		"(SELECT x FROM q WHERE x > " + n + ")",
		"(SELECT count(*) FROM q GROUP BY y)",
		"(SELECT x FROM q WHERE x > 9)",
		"(SELECT a FROM p ORDER BY a LIMIT 2)",
	}
	if deep {
		sets = append(sets,
			"(SELECT x FROM q WHERE x IN "+g.numSet(false)+")",
			"(SELECT a FROM p WHERE a < "+g.numValue()+")",
			"(SELECT x FROM q WHERE "+g.exists()+")",
		)
	}
	return sets[g.r.Intn(len(sets))]
}

func (g subqueryGen) textSet() string {
	return g.pick(
		"(SELECT y FROM q)",
		"(SELECT b FROM p WHERE a > "+strconv.Itoa(g.r.Intn(6))+")",
		"(SELECT y FROM q WHERE y IS NOT NULL)",
	)
}

func (g subqueryGen) exists() string {
	return g.pick("", "NOT ") + g.pick(
		"EXISTS (SELECT 1 FROM q WHERE x = "+strconv.Itoa(g.r.Intn(6))+")",
		"EXISTS (SELECT * FROM p WHERE b IS NULL)",
		"EXISTS (SELECT x FROM q WHERE x > 9)",
		"EXISTS (SELECT a FROM p WHERE a IN "+g.numSet(false)+")",
	)
}

// corrValue is a subquery of at most one INTEGER row that reads the row
// of p around it. With other, it reads no table but q, for UPDATE p.
func (g subqueryGen) corrValue(other bool) string {
	vs := []string{
		"(SELECT max(x) FROM q WHERE q.x <= p.a)",
		"(SELECT count(*) FROM q WHERE q.y = p.b)",
		"(SELECT min(x) FROM q WHERE q.y = p.b AND q.x > p.id - 5)",
	}
	if !other {
		vs = append(vs,
			"(SELECT w.a FROM p AS w WHERE w.id = p.a)",
			"(SELECT count(*) FROM q WHERE EXISTS (SELECT 1 FROM p AS w WHERE w.a = q.x AND w.b = p.b))",
		)
	}
	return vs[g.r.Intn(len(vs))]
}

// corrExists is EXISTS over a subquery that reads the row of p around it.
func (g subqueryGen) corrExists() string {
	return g.pick("", "NOT ") + g.pick(
		"EXISTS (SELECT 1 FROM q WHERE q.x = p.a)",
		"EXISTS (SELECT 1 FROM q WHERE q.y = p.b AND q.x > p.a)",
		"EXISTS (SELECT 1 FROM p AS w WHERE w.a = p.a AND w.id <> p.id)",
	)
}

// corrSet is a subquery of INTEGER rows for IN that reads the row of p
// around it.
func (g subqueryGen) corrSet() string {
	return g.pick(
		"(SELECT x FROM q WHERE q.y = p.b)",
		"(SELECT w.a FROM p AS w WHERE w.b = p.b AND w.id <> p.id)",
		"(SELECT x FROM q WHERE q.x > p.id)",
	)
}

// cond is a condition on a row of p.
func (g subqueryGen) cond() string {
	one := func() string {
		switch g.r.Intn(12) {
		case 9:
			return "a " + g.pick("=", "<", ">=") + " " + g.corrValue(false)
		case 10:
			return g.corrExists()
		case 11:
			return "a " + g.pick("IN", "NOT IN") + " " + g.corrSet()
		case 0:
			return "a " + g.pick("IN", "NOT IN") + " " + g.numSet(true)
		case 1:
			return "id " + g.pick("IN", "NOT IN") + " " + g.numSet(true)
		case 2:
			return "b " + g.pick("IN", "NOT IN") + " " + g.textSet()
		case 3:
			return "a " + g.pick("=", "<", ">=") + " " + g.numValue()
		case 4:
			return "b = " + g.textValue()
		case 5:
			return g.exists()
		case 6:
			return g.numValue() + " IS NULL"
		case 7:
			return "(a IN " + g.numSet(false) + ") IS NULL"
		default:
			return "a + " + g.numValue() + " > 3"
		}
	}
	c := one()
	if g.r.Intn(3) == 0 {
		c = "(" + c + ") " + g.pick("AND", "OR") + " (" + one() + ")"
	}
	return c
}

// item is a column of a query over p.
func (g subqueryGen) item() string {
	switch g.r.Intn(9) {
	case 6:
		return g.corrValue(false)
	case 7:
		return g.corrExists()
	case 8:
		return "a IN " + g.corrSet()
	case 0:
		return "a IN " + g.numSet(true)
	case 1:
		return "b NOT IN " + g.textSet()
	case 2:
		return g.numValue()
	case 3:
		return g.textValue()
	case 4:
		return g.exists()
	default:
		return "a + " + g.numValue()
	}
}

func subqueryAgainstSQLite(t *testing.T, seed int64) {
	g := subqueryGen{rand.New(rand.NewSource(seed))}
	type step struct {
		src     string
		query   bool
		cols    int
		ordered bool
	}
	steps := []step{
		{src: "CREATE TABLE p (id INTEGER PRIMARY KEY, a INTEGER, b TEXT)"},
		{src: "CREATE TABLE q (x INTEGER, y TEXT)"},
		{src: "CREATE INDEX p_a ON p (a)"},
	}
	for i := 0; i < 12; i++ {
		steps = append(steps,
			step{src: fmt.Sprintf("INSERT INTO p (a, b) VALUES (%s, %s)", g.num(), g.text())},
			step{src: fmt.Sprintf("INSERT INTO q VALUES (%s, %s)", g.num(), g.text())})
	}
	for len(steps) < 400 {
		switch n := g.r.Intn(40); {
		case n < 6:
			steps = append(steps, step{src: "INSERT INTO p (a, b) VALUES (" + g.pick(g.num(), g.numValue()) + ", " + g.pick(g.text(), g.textValue()) + ")"})
		case n < 10:
			steps = append(steps, step{src: "INSERT INTO q VALUES (" + g.pick(g.num(), g.numValue()) + ", " + g.text() + "), (" + g.numValue() + ", " + g.textValue() + ")"})
		case n < 14:
			// A value of SET reads no row of p but its own: SQLite lets a
			// row see the rows that the UPDATE wrote before it.
			steps = append(steps, step{src: "UPDATE p SET a = " + g.pick(g.num(), g.numValue(), g.corrValue(true)) + " WHERE " + g.cond()})
		case n < 15:
			steps = append(steps, step{src: "DELETE FROM p WHERE " + g.cond()})
		case n < 16:
			steps = append(steps, step{src: "DELETE FROM q WHERE " + g.pick(
				"x "+g.pick("IN", "NOT IN")+" "+g.numSet(true),
				"NOT EXISTS (SELECT 1 FROM p WHERE p.a = q.x)",
				"q.x > (SELECT count(*) FROM p WHERE p.b = q.y)")})
		case n < 17:
			// b is the second key: its slot is not the first one.
			steps = append(steps, step{src: "SELECT a, b, count(*), (SELECT count(*) FROM q WHERE q.y = p.b), (SELECT max(x) FROM q WHERE q.y = b AND q.x <> p.a) FROM p GROUP BY a, b", query: true, cols: 5})
		case n < 34:
			items := []string{"id", "a", "b"}
			for k := g.r.Intn(3); k > 0; k-- {
				items = append(items, g.item())
			}
			steps = append(steps, step{src: "SELECT " + strings.Join(items, ", ") + " FROM p WHERE " + g.cond() + " ORDER BY id", query: true, cols: len(items), ordered: true})
		default:
			cond := "x " + g.pick("IN", "NOT IN") + " " + g.numSet(true)
			switch g.r.Intn(4) {
			case 0:
				cond = "y = " + g.textValue()
			case 1:
				cond = g.pick("", "NOT ") + "EXISTS (SELECT 1 FROM p WHERE p.a = q.x AND p.b = q.y)"
			case 2:
				cond = "q.x <= (SELECT count(*) FROM p WHERE p.b = q.y OR p.a = q.x)"
			}
			steps = append(steps, step{src: "SELECT x, y, x IN " + g.numSet(true) + " FROM q WHERE " + cond, query: true, cols: 3})
		}
	}
	steps = append(steps,
		step{src: "SELECT id, a, b FROM p ORDER BY id", query: true, cols: 3, ordered: true},
		step{src: "SELECT x, y FROM q", query: true, cols: 2})

	// SQLite: one statement per line, so an error names its step. Each
	// column of a query is read through WITH, as c1, c2, ...
	var script strings.Builder
	for i, s := range steps {
		if !s.query {
			script.WriteString(s.src + ";\n")
			continue
		}
		var cols []string
		for k := 1; k <= s.cols; k++ {
			c := "c" + strconv.Itoa(k)
			cols = append(cols, fmt.Sprintf("typeof(%[1]s), CASE typeof(%[1]s) WHEN 'text' THEN hex(%[1]s) ELSE %[1]s END", c))
		}
		var names []string
		for k := 1; k <= s.cols; k++ {
			names = append(names, "c"+strconv.Itoa(k))
		}
		fmt.Fprintf(&script, "WITH r(%s) AS (%s) SELECT 'Q', %d, %s FROM r;\n", strings.Join(names, ", "), s.src, i, strings.Join(cols, ", "))
	}
	lines, sqliteErrs := sqlitetest.RunErrors(t, script.String())
	theirs := map[int][]string{}
	for _, f := range lines {
		i, err := strconv.Atoi(f[1])
		if f[0] != "Q" || err != nil {
			t.Fatalf("sqlite3 line %q", f)
		}
		theirs[i] = append(theirs[i], strings.Join(f[2:], " "))
	}

	ss := newSession(t, store.Options{})
	queries, writes, failed, rows, nulls, correlated := 0, 0, 0, 0, 0, 0
	for i, s := range steps {
		msg, theyFailed := sqliteErrs[i+1]
		var err error
		if s.query {
			var ours []string
			ours, err = runForOracle(ss, s.src)
			if err == nil {
				queries++
				rows += len(ours)
				for _, row := range ours {
					nulls += strings.Count(row, "null")
				}
				their := append([]string(nil), theirs[i]...)
				if !s.ordered {
					sort.Strings(ours)
					sort.Strings(their)
				}
				if strings.Join(ours, "\n") != strings.Join(their, "\n") {
					t.Errorf("step %d: %s\n SQLite:\n  %s\n here:\n  %s", i, s.src, strings.Join(their, "\n  "), strings.Join(ours, "\n  "))
				}
			}
		} else {
			_, err = ss.exec(s.src)
			writes++
		}
		ss.commit()
		if theyFailed != (err != nil) {
			t.Errorf("step %d: %s\n SQLite: %q\n here: %v", i, s.src, msg, err)
		}
		if err != nil {
			failed++
		} else if strings.Contains(s.src, "p.") || strings.Contains(s.src, "q.") {
			// Only a correlated subquery names p or q with its table.
			correlated++
		}
	}
	t.Logf("%d steps: %d queries with %d rows and %d NULL values, %d writes, %d failed in both, %d correlated", len(steps), queries, rows, nulls, writes, failed, correlated)
	if queries < 150 || writes < 80 || rows < 500 || nulls < 100 || correlated < 100 {
		t.Errorf("a weak script: %d queries, %d rows, %d NULL values, %d writes, %d correlated", queries, rows, nulls, writes, correlated)
	}
}
