package sqlexec

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// planned runs a query with the plan and without, and returns the plan,
// the rows it read, and the result. The results must be the same.
func (ss *session) planned(t *testing.T, src string, params ...any) (string, int64, string) {
	t.Helper()
	run := func(lim Limits) (string, int64, string) {
		st, err := sqlparse.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), lim)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		rows, err := q.Run(ss.tx, params)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		var b strings.Builder
		for rows.Next() {
			fmt.Fprintln(&b, rows.Row()...)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return q.Plan(), rows.Scanned(), b.String()
	}
	plan, scanned, got := run(Limits{})
	_, all, want := run(Limits{NoIndex: true})
	if !strings.Contains(src, "ORDER BY") {
		// Without ORDER BY, SQL gives no order: an index scan gives the
		// rows in the order of the index.
		got, want = sortLines(got), sortLines(want)
	}
	if got != want {
		t.Errorf("%s: with the plan %s\n%s\nwithout\n%s", src, plan, got, want)
	}
	if all < scanned {
		t.Errorf("%s: the plan read %d rows, a full scan %d", src, scanned, all)
	}
	return plan, scanned, got
}

// planTable has 1000 rows: id 1 to 1000, g = id % 10, name 'n<id>', and
// score id / 4 as REAL, NULL for every 7th row.
func planTable(t *testing.T) *session {
	ss := newSession(t, store.Options{MaxTxBytes: 64 << 20})
	ss.must("CREATE TABLE t (id INTEGER PRIMARY KEY, g INTEGER NOT NULL, name TEXT NOT NULL, score REAL)")
	ss.must("CREATE INDEX by_g ON t (g, name)")
	ss.must("CREATE UNIQUE INDEX by_name ON t (name)")
	ss.must("CREATE INDEX by_score ON t (score)")
	for i := 1; i <= 1000; i++ {
		var score any = float64(i) / 4
		if i%7 == 0 {
			score = nil
		}
		ss.must("INSERT INTO t VALUES (?, ?, ?, ?)", int64(i), int64(i%10), fmt.Sprintf("n%04d", i), score)
	}
	return ss
}

// TestPlanForms shows the plan for each form of L-9 and the rows it
// reads.
func TestPlanForms(t *testing.T) {
	ss := planTable(t)
	for _, c := range []struct {
		src     string
		plan    string
		scanned int64
		rows    int
	}{
		{"SELECT * FROM t", "SCAN t", 1000, 1000},
		{"SELECT * FROM t WHERE id = 500", "SEARCH t USING PRIMARY KEY ((id = 500))", 1, 1},
		{"SELECT * FROM t WHERE 500 = id", "SEARCH t USING PRIMARY KEY ((500 = id))", 1, 1},
		{"SELECT * FROM t WHERE 990 < id", "SEARCH t USING PRIMARY KEY ((990 < id))", 10, 10},
		{"SELECT * FROM t WHERE id < 10", "SEARCH t USING PRIMARY KEY ((id < 10))", 9, 9},
		{"SELECT * FROM t WHERE id <= 10", "SEARCH t USING PRIMARY KEY ((id <= 10))", 10, 10},
		{"SELECT * FROM t WHERE id > 990", "SEARCH t USING PRIMARY KEY ((id > 990))", 10, 10},
		{"SELECT * FROM t WHERE id >= 990", "SEARCH t USING PRIMARY KEY ((id >= 990))", 11, 11},
		{"SELECT * FROM t WHERE id > 10 AND id <= 20", "SEARCH t USING PRIMARY KEY ((id > 10) AND (id <= 20))", 10, 10},
		{"SELECT * FROM t WHERE id BETWEEN 100 AND 104", "SEARCH t USING PRIMARY KEY ((id BETWEEN 100 AND 104))", 5, 5},
		{"SELECT * FROM t WHERE id IN (7, 3, 7, 999)", "SEARCH t USING PRIMARY KEY ((id IN (7, 3, 7, 999)))", 3, 3},
		{"SELECT * FROM t WHERE g = 3", "SEARCH t USING INDEX by_g ((g = 3))", 100, 100},
		{"SELECT * FROM t WHERE g = 3 AND name >= 'n0900'", "SEARCH t USING INDEX by_g ((g = 3) AND (name >= 'n0900'))", 10, 10},
		{"SELECT * FROM t WHERE g IN (1, 2) AND name < 'n0050'", "SEARCH t USING INDEX by_g ((g IN (1, 2)) AND (name < 'n0050'))", 10, 10},
		{"SELECT * FROM t WHERE name = 'n0042'", "SEARCH t USING INDEX by_name ((name = 'n0042'))", 1, 1},
		{"SELECT * FROM t WHERE score > 249", "SEARCH t USING INDEX by_score ((score > 249))", 4, 4},
		{"SELECT * FROM t WHERE score >= 249 AND score < 249.5", "SEARCH t USING INDEX by_score ((score >= 249) AND (score < 249.5))", 2, 2},
		// A key and an index both fit; the key has as many = columns and
		// wins the tie.
		{"SELECT * FROM t WHERE id = 5 AND name = 'n0005'", "SEARCH t USING PRIMARY KEY ((id = 5))", 1, 1},
		// A term under OR, NOT or on a later column of an index narrows
		// nothing.
		{"SELECT * FROM t WHERE id = 5 OR id = 6", "SCAN t", 1000, 2},
		{"SELECT * FROM t WHERE NOT id > 5", "SCAN t", 1000, 5},
		{"SELECT * FROM t WHERE id NOT IN (1, 2)", "SCAN t", 1000, 998},
		// Two columns of the same table: the value is no bound.
		{"SELECT * FROM t WHERE g = id", "SCAN t", 1000, 9},
		// = on a column beats IN on the same column.
		{"SELECT * FROM t WHERE g IN (1, 2, 3) AND g = 2", "SEARCH t USING INDEX by_g ((g = 2))", 100, 100},
		{"SELECT * FROM t WHERE name = 'n0005' OR g = 1", "SCAN t", 1000, 101},
		// A value that no row can equal reads nothing.
		{"SELECT * FROM t WHERE id = 2.5", "SEARCH t USING PRIMARY KEY ((id = 2.5))", 0, 0},
		{"SELECT * FROM t WHERE id = NULL", "SEARCH t USING PRIMARY KEY ((id = NULL))", 0, 0},
		{"SELECT * FROM t WHERE id = 3.0", "SEARCH t USING PRIMARY KEY ((id = 3.0))", 1, 1},
		// ORDER BY in the order of a scan needs no sort, and LIMIT stops
		// the scan early.
		{"SELECT * FROM t ORDER BY id LIMIT 3", "SCAN t USING PRIMARY KEY IN ORDER", 3, 3},
		{"SELECT * FROM t ORDER BY id DESC LIMIT 3", "SCAN t USING PRIMARY KEY IN ORDER DESC", 3, 3},
		{"SELECT * FROM t WHERE g = 4 ORDER BY name LIMIT 2", "SEARCH t USING INDEX by_g ((g = 4)) IN ORDER", 2, 2},
		{"SELECT name AS x FROM t ORDER BY x LIMIT 2", "SCAN t USING INDEX by_name IN ORDER", 2, 2},
		{"SELECT * FROM t ORDER BY 2, 3 LIMIT 2", "SCAN t USING INDEX by_g IN ORDER", 2, 2},
		// An index orders by its columns, then by the key.
		{"SELECT * FROM t ORDER BY name, id LIMIT 2", "SCAN t USING INDEX by_name IN ORDER", 2, 2},
		{"SELECT * FROM t WHERE id > 995 ORDER BY id DESC", "SEARCH t USING PRIMARY KEY ((id > 995)) IN ORDER DESC", 5, 5},
		// DESC over g alone would give equal g in the other order of the
		// key; the plan sorts instead.
		{"SELECT * FROM t ORDER BY g DESC LIMIT 2", "SCAN t; SORT", 1000, 2},
		// Directions mixed, IN, DISTINCT, or an expression: a sort.
		{"SELECT * FROM t ORDER BY g, name DESC LIMIT 2", "SCAN t; SORT", 1000, 2},
		{"SELECT * FROM t WHERE g IN (1, 2) ORDER BY name LIMIT 2", "SEARCH t USING INDEX by_g ((g IN (1, 2))); SORT", 200, 2},
		{"SELECT DISTINCT g FROM t ORDER BY g", "SCAN t; DISTINCT; SORT", 1000, 10},
		{"SELECT g, count(*) FROM t GROUP BY g ORDER BY g", "SCAN t; GROUP; SORT", 1000, 10},
		{"SELECT count(*) FROM t WHERE id < 100", "SEARCH t USING PRIMARY KEY ((id < 100)); GROUP", 99, 1},
		{"SELECT * FROM t ORDER BY -id LIMIT 1", "SCAN t; SORT", 1000, 1},
	} {
		plan, scanned, got := ss.planned(t, c.src)
		rows := strings.Count(got, "\n")
		if plan != c.plan || scanned != c.scanned || rows != c.rows {
			t.Errorf("%s:\n got %s, %d read, %d rows\nwant %s, %d read, %d rows", c.src, plan, scanned, rows, c.plan, c.scanned, c.rows)
		}
	}
}

// TestPlanParams: the values of a plan come from parameters at the run.
// A parameter of another type is an error of WHERE with the plan too.
func TestPlanParams(t *testing.T) {
	ss := planTable(t)
	for _, c := range []struct {
		params  []any
		scanned int64
		rows    int
	}{
		{[]any{int64(10), int64(12)}, 3, 3},
		{[]any{3.0, 4.5}, 2, 2},
		{[]any{2.5, 4.0}, 2, 2},
		{[]any{-0.5, 1.5}, 1, 1},
		{[]any{nil, int64(3)}, 0, 0},
		{[]any{int64(999), int64(5)}, 0, 0},
	} {
		_, scanned, got := ss.planned(t, "SELECT id FROM t WHERE id >= ? AND id <= ?", c.params...)
		if rows := strings.Count(got, "\n"); scanned != c.scanned || rows != c.rows {
			t.Errorf("%v: %d read, %d rows; want %d, %d", c.params, scanned, rows, c.scanned, c.rows)
		}
	}
	for _, src := range []string{"SELECT id FROM t WHERE id = ?", "SELECT id FROM t WHERE id > ?", "SELECT id FROM t WHERE id IN (1, ?)"} {
		for _, lim := range []Limits{{}, {NoIndex: true}} {
			st, _ := sqlparse.Parse(src)
			q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), lim)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := q.Run(ss.tx, []any{"x"})
			for err == nil && rows.Next() {
			}
			if err == nil {
				err = rows.Err()
			}
			if err == nil || !strings.Contains(err.Error(), "do not compare") {
				t.Errorf("%s with a TEXT parameter, %+v: %v", src, lim, err)
			}
		}
	}
}

// TestMemoryBound: a statement that would hold more rows than the bound
// fails and returns or changes nothing (L-10). A query in the order of
// a scan holds no rows and runs.
func TestMemoryBound(t *testing.T) {
	ss := planTable(t)
	ss.lim = Limits{MaxMemory: 20000}
	for _, src := range []string{
		"SELECT * FROM t ORDER BY -g",
		"SELECT DISTINCT name FROM t",
	} {
		if _, err := ss.query(src); !errors.Is(err, ErrMemory) {
			t.Errorf("%s: %v, want ErrMemory", src, err)
		}
	}
	for _, src := range []string{
		"SELECT * FROM t",
		"SELECT * FROM t ORDER BY id DESC",
		"SELECT * FROM t WHERE g = 1 ORDER BY name",
		"SELECT * FROM t ORDER BY score",      // the order of by_score
		"SELECT * FROM t ORDER BY -g LIMIT 3", // sorts all rows first
	} {
		_, err := ss.query(src)
		if strings.HasSuffix(src, "LIMIT 3") {
			if !errors.Is(err, ErrMemory) {
				t.Errorf("%s: %v, want ErrMemory: LIMIT does not bound a sort", src, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
	sum := func() string {
		got, err := ss.query("SELECT g FROM t WHERE id IN (1, 500, 1000)")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	was := sum()
	if _, err := ss.exec("UPDATE t SET g = g + 1"); !errors.Is(err, ErrMemory) {
		t.Errorf("UPDATE of 1000 rows: %v, want ErrMemory", err)
	}
	if _, err := ss.exec("DELETE FROM t WHERE id > 10"); !errors.Is(err, ErrMemory) {
		t.Errorf("DELETE of 990 rows: %v, want ErrMemory", err)
	}
	if got := sum(); got != was {
		t.Errorf("a failed statement changed rows: %s, before %s", got, was)
	}
	// With the plan, an UPDATE of few rows holds few rows.
	if r, err := ss.exec("UPDATE t SET g = g + 1 WHERE id BETWEEN 1 AND 5"); err != nil || r.RowsAffected != 5 {
		t.Errorf("UPDATE of 5 rows: %+v %v", r, err)
	}
}

func sortLines(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// TestPlanExactBounds: an INTEGER that REAL does not hold exactly is no
// bound of a REAL column. Rounded, 9007199254740993 would be
// 9007199254740992.0, and score < it would miss the row that has it.
func TestPlanExactBounds(t *testing.T) {
	ss := newSession(t, store.Options{})
	ss.must("CREATE TABLE r (id INTEGER PRIMARY KEY, score REAL)")
	ss.must("CREATE INDEX by_score ON r (score)")
	ss.must("INSERT INTO r VALUES (1, 9007199254740992.0), (2, 9007199254740994.0)")
	for _, c := range []struct{ src, want string }{
		{"SELECT id FROM r WHERE score < 9007199254740993", "1\n"},
		{"SELECT id FROM r WHERE score > 9007199254740993", "2\n"},
		{"SELECT id FROM r WHERE score = 9007199254740993", ""},
	} {
		if _, _, got := ss.planned(t, c.src); got != c.want {
			t.Errorf("%s: %q, want %q", c.src, got, c.want)
		}
	}
}

// TestPlanWrites: UPDATE and DELETE read only the rows the plan finds.
func TestPlanWrites(t *testing.T) {
	for _, lim := range []Limits{{}, {NoIndex: true}} {
		ss := planTable(t)
		ss.lim = lim
		for _, c := range []struct {
			src          string
			plan, noPlan int64
		}{
			{"UPDATE t SET name = name || 'x' WHERE id = 5", 1, 1000},
			{"DELETE FROM t WHERE g = 3", 100, 1000},
			// The DELETE took id 23.
			{"UPDATE t SET g = 0 WHERE id BETWEEN 20 AND 29", 9, 900},
		} {
			r := ss.must(c.src)
			want := c.plan
			if lim.NoIndex {
				want = c.noPlan
			}
			if r.scanned != want {
				t.Errorf("%+v %s: %d read, want %d", lim, c.src, r.scanned, want)
			}
		}
	}
}
