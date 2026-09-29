package sqlexec

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/sqlitetest"
	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// TestMixedAgainstSQLite runs one random script in SQLite and here:
// writes, queries, and transactions that end with COMMIT or ROLLBACK, in
// any order (P-4). Each statement must fail in both or in neither, and
// each query must give the rows of SQLite at its place in the script.
// Outside a transaction, each statement commits on its own, as in
// SQLite.
func TestMixedAgainstSQLite(t *testing.T) {
	for _, seed := range []int64{25, 1025} {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) { mixedAgainstSQLite(t, seed, 800) })
	}
}

func mixedAgainstSQLite(t *testing.T, seed int64, n int) {
	w := &workload{r: rand.New(rand.NewSource(seed)), altered: true}
	g := queries{w}
	type step struct {
		src     string
		query   bool
		names   []string
		ordered bool
	}
	steps := []step{}
	for _, s := range schema {
		steps = append(steps, step{src: s})
	}
	inTx := false
	for len(steps) < len(schema)+n {
		switch r := w.r.Intn(20); {
		case r == 0 && !inTx:
			steps, inTx = append(steps, step{src: "BEGIN"}), true
		case r == 1 && inTx:
			steps, inTx = append(steps, step{src: w.one("COMMIT", "ROLLBACK")}), false
		case r < 10:
			steps = append(steps, step{src: w.statement()})
		default:
			tb := queryTables[w.r.Intn(len(queryTables))]
			var s step
			switch w.r.Intn(3) {
			case 0:
				s.src, s.names, s.ordered = g.aggQuery(tb)
			case 1:
				s.src, s.names, s.ordered = g.joinQuery()
			default:
				s.src, s.names, s.ordered = g.query(tb)
			}
			s.query = true
			steps = append(steps, s)
		}
	}
	if inTx {
		steps = append(steps, step{src: "COMMIT"})
	}

	// SQLite: one statement per line, so an error names its step.
	var script strings.Builder
	for i, s := range steps {
		if !s.query {
			script.WriteString(s.src + ";\n")
			continue
		}
		var cols []string
		for _, c := range s.names {
			cols = append(cols, fmt.Sprintf("typeof(%s), CASE typeof(%s) WHEN 'real' THEN hex(ieee754_to_blob(%s)) WHEN 'text' THEN hex(%s) WHEN 'blob' THEN hex(%s) ELSE %s END", c, c, c, c, c, c))
		}
		fmt.Fprintf(&script, "SELECT 'Q', %d, %s FROM (%s);\n", i, strings.Join(cols, ", "), s.src)
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

	// Here: the session of the tests holds a write transaction. Outside
	// BEGIN, each statement commits.
	ss := newSession(t, store.Options{})
	inTx = false
	queries, writes, failed, rows := 0, 0, 0, 0
	for i, s := range steps {
		_, theyFailed := sqliteErrs[i+1]
		var err error
		switch s.src {
		case "BEGIN":
			inTx = true
		case "COMMIT":
			inTx = false
			ss.commit()
		case "ROLLBACK":
			inTx = false
			ss.stx.Rollback()
			ss.begin()
		default:
			if s.query {
				var ours []string
				ours, err = runForOracle(ss, s.src)
				if err == nil {
					queries++
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
				writes++
			}
			if !inTx {
				ss.commit()
			}
		}
		if theyFailed != (err != nil) {
			t.Errorf("step %d: %s\n SQLite: %q\n here: %v", i, s.src, sqliteErrs[i+1], err)
		}
		if err != nil {
			failed++
		}
	}
	t.Logf("%d steps: %d queries with %d rows, %d writes, %d failed in both", len(steps), queries, rows, writes, failed)
	if queries < n/3 || writes < n/3 || rows < 500 {
		t.Errorf("a weak script: %d queries, %d rows, %d writes", queries, rows, writes)
	}
}

// runForOracle runs a query and prints its rows as oracleForm does.
func runForOracle(ss *session, src string) ([]string, error) {
	st, err := sqlparse.Parse(src)
	if err != nil {
		return nil, err
	}
	q, err := Prepare(ss.tx.Schema(), st.(*sqlparse.Select), ss.lim)
	if err != nil {
		return nil, err
	}
	rows, err := q.Run(ss.tx, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var fields []string
		for _, v := range rows.Row() {
			class, value := oracleForm(v)
			fields = append(fields, class, value)
		}
		out = append(out, strings.Join(fields, " "))
	}
	return out, rows.Err()
}
