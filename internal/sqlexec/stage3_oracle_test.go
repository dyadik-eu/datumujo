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

// TestStage3AgainstSQLite runs one random script in SQLite and here
// (P-4). It mixes the forms of stage 3 with those of stage 2. Stage 2
// gives the writes and queries of TestMixedAgainstSQLite and
// transactions. The table t4 has DEFAULT, CHECK and UNIQUE. INSERT ... ON CONFLICT writes t1, t3
// and t4, and subqueries read across all four tables.
//
// The script leaves out the differences in the oracle table. A subquery
// as a value is an aggregate or reads one key. A subquery in UPDATE reads
// no row of the table that the statement writes. UPDATE sets no column
// of a key or a UNIQUE to an expression. A subquery in DELETE can read
// the table that the statement deletes from: both see the table as it
// was before the statement.
//
// Each statement must fail in both or in neither. Each query must give
// the rows of SQLite at its place in the script, and all tables must hold
// the same rows at the end. Each form must run at least five times
// without an error, and a row that breaks a check must fail at least
// five times.
func TestStage3AgainstSQLite(t *testing.T) {
	for _, seed := range []int64{35, 1035} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { stage3AgainstSQLite(t, seed, 900) })
	}
}

// stage3 writes the statements of the new forms over t1 to t4.
type stage3 struct{ w *workload }

// id is a key of t4, or NULL for the next one.
func (g stage3) id() string {
	if g.w.r.Intn(4) == 0 {
		return "NULL"
	}
	return strconv.Itoa(1 + g.w.r.Intn(20))
}

func (g stage3) num() string { return g.w.maybeNull(strconv.Itoa(g.w.r.Intn(8) - 1)) }

// text is a value for t4.s. Half of them can also be in t1.b. The other
// half come from 40 values, so the UNIQUE of s leaves room for new rows.
func (g stage3) text() string {
	if g.w.r.Intn(2) == 0 {
		return g.w.maybeNull(g.w.one("'a'", "'b'", "'c'", "'x'"))
	}
	return "'s" + strconv.Itoa(g.w.r.Intn(40)) + "'"
}

// value is a subquery of at most one INTEGER row, around no row.
func (g stage3) value() string {
	return g.w.one(
		"(SELECT max(n) FROM t4)",
		"(SELECT count(*) FROM t1 WHERE a > "+strconv.Itoa(g.w.r.Intn(6))+")",
		"(SELECT min(x) FROM t3)",
		"(SELECT m FROM t4 WHERE id = "+strconv.Itoa(1+g.w.r.Intn(8))+")",
		"(SELECT count(*) FROM t2 WHERE n IN (SELECT n FROM t4))",
	)
}

// t1Item is a column of a query over t1.
func (g stage3) t1Item() string {
	return g.w.one(
		"(SELECT count(*) FROM t4 WHERE t4.m = t1.a)",
		"(SELECT s FROM t4 WHERE t4.id = t1.id)",
		"(SELECT sum(x) FROM t3 WHERE t3.y = t1.b)",
		"EXISTS (SELECT 1 FROM t3 WHERE t3.x = t1.a)",
		"t1.a IN (SELECT m FROM t4)",
		"t1.b NOT IN (SELECT s FROM t4 WHERE t4.n > 0)",
		g.value(),
	)
}

// t1Cond is a condition on a row of t1.
func (g stage3) t1Cond() string {
	c := g.w.one(
		"a IN (SELECT n FROM t4)",
		"NOT EXISTS (SELECT 1 FROM t4 WHERE t4.m = t1.a)",
		"a > "+g.value(),
		"id IN (SELECT m FROM t4 WHERE m IS NOT NULL)",
		"b = (SELECT s FROM t4 WHERE t4.id = t1.id)",
		g.w.cond("t1"),
	)
	if g.w.r.Intn(3) == 0 {
		c = "(" + c + ") " + g.w.one("AND", "OR") + " (" + g.w.cond("t1") + ")"
	}
	return c
}

// t4Item is a column of a query over t4.
func (g stage3) t4Item() string {
	return g.w.one(
		"(SELECT count(*) FROM t1 WHERE t1.a = t4.m)",
		"m IN (SELECT x FROM t3)",
		"EXISTS (SELECT 1 FROM t1 WHERE t1.b = t4.s)",
		"n + "+g.value(),
	)
}

func (g stage3) t4Cond() string {
	return g.w.one(
		"n > "+strconv.Itoa(g.w.r.Intn(4)),
		"m IS NULL",
		"s IN ('a', 'x')",
		"m IN (SELECT a FROM t1)",
		"NOT EXISTS (SELECT 1 FROM t3 WHERE t3.x = t4.m)",
		"id <= (SELECT count(*) FROM t1)",
	)
}

// upsert writes INSERT ... ON CONFLICT over t1, t3 or t4.
func (g stage3) upsert() string {
	w := g.w
	switch w.r.Intn(3) {
	case 0:
		row := func() string {
			return strings.Join([]string{strconv.Itoa(1 + w.r.Intn(15)), w.maybeNull(w.intLit()), w.maybeNull(w.textLit()), w.realLit(), w.maybeNull(w.boolLit())}, ", ")
		}
		return "INSERT INTO t1 VALUES " + w.rows(1+w.r.Intn(2), row) + " ON CONFLICT" + w.one(
			" DO NOTHING", " (id) DO NOTHING", " (b) DO NOTHING", " (a) DO NOTHING",
			" (id) DO UPDATE SET a = excluded.a, c = t1.c + 1",
			" (id) DO UPDATE SET b = excluded.b WHERE excluded.a > t1.a",
			" (b) DO UPDATE SET a = coalesce(t1.a, 0) + 1",
			" DO UPDATE SET d = NOT d",
		)
	case 1:
		return "INSERT INTO t3 (x, y) VALUES (" + w.maybeNull(w.intLit()) + ", " + w.maybeNull(w.textLit()) + ") ON CONFLICT" + w.one(
			" (x) DO UPDATE SET y = excluded.y", " (x) DO NOTHING", " DO NOTHING")
	}
	row := func() string {
		return strings.Join([]string{g.id(), g.text(), g.num(), g.num()}, ", ")
	}
	return "INSERT INTO t4 (id, s, n, m) VALUES " + w.rows(1+w.r.Intn(3), row) + " ON CONFLICT" + w.one(
		" DO NOTHING", " (id) DO NOTHING", " (s) DO NOTHING",
		" (id) DO UPDATE SET n = t4.n + excluded.n",
		" (s) DO UPDATE SET m = excluded.m WHERE excluded.n > t4.n",
		" (id) DO UPDATE SET s = excluded.s, m = NULL",
		" DO UPDATE SET n = coalesce(n, 0) + 1",
	)
}

// write is a statement of stage 3 that changes a table.
func (g stage3) write() (src, kind string) {
	w := g.w
	switch w.r.Intn(11) {
	case 0, 8:
		// The defaults fill the columns left out: n with 0, s with 'x'.
		// A second row with the default s breaks the UNIQUE in both.
		if w.r.Intn(4) == 0 {
			return "INSERT INTO t4 (id, m) VALUES (" + g.id() + ", " + g.num() + ")", "default"
		}
		return "INSERT INTO t4 (id, s, m) VALUES (" + g.id() + ", " + g.text() + ", " + g.num() + ")", "default"
	case 1, 9:
		return "INSERT INTO t4 (s, n, m) VALUES (" + g.text() + ", " + w.one(g.num(), g.value()) + ", " + w.one(g.num(), g.value()) + ")", "subquery"
	case 2, 3:
		return g.upsert(), "upsert"
	case 4:
		return "UPDATE t4 SET " + w.one("n = n + 1", "n = n - 1", "m = m + 3", "n = (SELECT count(*) FROM t3 WHERE t3.x = t4.m)", "m = (SELECT max(a) FROM t1)") + " WHERE " + g.t4Cond(), "update"
	case 5:
		return "UPDATE t1 SET a = " + w.one("(SELECT max(n) FROM t4 WHERE t4.m = t1.a)", g.value(), "a + 1") + " WHERE " + g.t1Cond(), "update"
	case 6:
		return "DELETE FROM t3 WHERE " + w.one(
			"x IN (SELECT n FROM t4)",
			"NOT EXISTS (SELECT 1 FROM t1 WHERE t1.a = t3.x)",
			"x > (SELECT count(*) FROM t4 WHERE t4.s = t3.y)"), "delete"
	case 7:
		if w.r.Intn(2) == 0 {
			return "DELETE FROM t4 WHERE " + g.t4Cond(), "delete"
		}
		return "DELETE FROM t4 WHERE " + w.one(
			"id = "+strconv.Itoa(1+w.r.Intn(20)),
			"n < (SELECT max(n) FROM t4) - 2",
			"EXISTS (SELECT 1 FROM t4 AS o WHERE o.m = t4.m AND o.id < t4.id)",
			"id NOT IN (SELECT max(id) FROM t4 GROUP BY s IS NULL)"), "delete"
	}
	return "INSERT INTO t4 (id, s, n, m) VALUES (" + g.id() + ", " + g.text() + ", " + g.num() + ", " + w.one(g.num(), "12") + ")", "check"
}

// query is a query of stage 3, with the number of its columns.
func (g stage3) query() (src string, cols int, ordered bool) {
	w := g.w
	switch w.r.Intn(4) {
	case 0:
		items := []string{"id", "a", "b"}
		for k := 1 + w.r.Intn(2); k > 0; k-- {
			items = append(items, g.t1Item())
		}
		return "SELECT " + strings.Join(items, ", ") + " FROM t1 WHERE " + g.t1Cond() + " ORDER BY id", len(items), true
	case 1:
		return "SELECT id, s, n, m, " + g.t4Item() + " FROM t4 WHERE " + g.t4Cond() + " ORDER BY id", 5, true
	case 2:
		return "SELECT t1.id, t4.id, t4.n FROM t1 JOIN t4 ON t4.m = t1.a WHERE t4.n IN (SELECT x FROM t3) OR t1.b = t4.s", 3, false
	}
	// m is the key of the group, and the subquery reads it as t4.m.
	return "SELECT m, count(*), sum(n), (SELECT count(*) FROM t1 WHERE t1.a = t4.m) FROM t4 GROUP BY m", 4, false
}

func stage3AgainstSQLite(t *testing.T, seed int64, n int) {
	w := &workload{r: rand.New(rand.NewSource(seed)), altered: true}
	g, old := stage3{w}, queries{w}
	type step struct {
		src, kind string
		query     bool
		cols      int
		ordered   bool
	}
	var steps []step
	for _, s := range append(append([]string(nil), schema...),
		"CREATE TABLE t4 (id INTEGER PRIMARY KEY, s TEXT NOT NULL DEFAULT 'x' UNIQUE, n INTEGER DEFAULT 0 CHECK (n >= 0), m INTEGER, CHECK (m IS NULL OR m < 10))",
		"CREATE INDEX t4_m ON t4 (m)") {
		steps = append(steps, step{src: s})
	}
	start := len(steps)
	inTx := false
	for len(steps) < start+n {
		switch r := w.r.Intn(24); {
		case r == 0 && !inTx:
			steps, inTx = append(steps, step{src: "BEGIN"}), true
		case r == 1 && inTx:
			steps, inTx = append(steps, step{src: w.one("COMMIT", "ROLLBACK")}), false
		case r < 6:
			steps = append(steps, step{src: w.statement(), kind: "stage 2"})
		case r < 14:
			src, kind := g.write()
			steps = append(steps, step{src: src, kind: kind})
		case r < 17:
			tb := queryTables[w.r.Intn(len(queryTables))]
			var src string
			var names []string
			var ordered bool
			if w.r.Intn(2) == 0 {
				src, names, ordered = old.query(tb)
			} else {
				src, names, ordered = old.joinQuery()
			}
			steps = append(steps, step{src: src, kind: "stage 2 query", query: true, cols: len(names), ordered: ordered})
		default:
			src, cols, ordered := g.query()
			steps = append(steps, step{src: src, kind: "query", query: true, cols: cols, ordered: ordered})
		}
	}
	if inTx {
		steps = append(steps, step{src: "COMMIT"})
	}
	for _, d := range []struct {
		src  string
		cols int
	}{
		{"SELECT id, a, b, c, d FROM t1 ORDER BY id", 5},
		{"SELECT k, n, v, w FROM t2 ORDER BY k, n", 4},
		{"SELECT rowid, x, y FROM t3 ORDER BY rowid", 3},
		{"SELECT id, s, n, m FROM t4 ORDER BY id", 4},
	} {
		steps = append(steps, step{src: d.src, kind: "dump", query: true, cols: d.cols, ordered: true})
	}

	// SQLite: one statement per line, so an error names its step. Each
	// column of a query is read through WITH, as c1, c2, ...
	var script strings.Builder
	for i, s := range steps {
		if !s.query {
			script.WriteString(s.src + ";\n")
			continue
		}
		var names, cols []string
		for k := 1; k <= s.cols; k++ {
			c := "c" + strconv.Itoa(k)
			names = append(names, c)
			cols = append(cols, fmt.Sprintf("typeof(%[1]s), CASE typeof(%[1]s) WHEN 'real' THEN hex(ieee754_to_blob(%[1]s)) WHEN 'text' THEN hex(%[1]s) WHEN 'blob' THEN hex(%[1]s) ELSE %[1]s END", c))
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
	inTx = false
	ran, refused := map[string]int{}, map[string]int{}
	failed, rows := 0, 0
	for i, s := range steps {
		msg, theyFailed := sqliteErrs[i+1]
		var err error
		switch s.src {
		case "BEGIN":
			inTx = true
			continue
		case "COMMIT":
			inTx = false
			ss.commit()
			continue
		case "ROLLBACK":
			inTx = false
			ss.stx.Rollback()
			ss.begin()
			continue
		}
		if s.query {
			var ours []string
			if ours, err = runForOracle(ss, s.src); err == nil {
				rows += len(ours)
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
		}
		if !inTx {
			ss.commit()
		}
		if theyFailed != (err != nil) {
			t.Errorf("step %d: %s\n SQLite: %q\n here: %v", i, s.src, msg, err)
		}
		if err != nil {
			failed++
			refused[s.kind]++
		} else {
			ran[s.kind]++
		}
	}
	t.Logf("%d steps, %d failed in both, %d rows; ran: %v; failed: %v", len(steps), failed, rows, ran, refused)
	for _, kind := range []string{"stage 2", "stage 2 query", "query", "default", "subquery", "upsert", "update", "delete", "check"} {
		if ran[kind] < 5 {
			t.Errorf("a weak script: %d of %s ran without an error", ran[kind], kind)
		}
	}
	// A check that breaks fails the statement in both.
	if refused["check"] < 5 {
		t.Errorf("a weak script: %d of check failed", refused["check"])
	}
	if failed < 20 || rows < 500 {
		t.Errorf("a weak script: %d failed, %d rows", failed, rows)
	}
}
