package sqlexec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// EXPLAIN (L-18) shows the plan of a statement instead of running it. It
// compiles and plans the statement with the code of its run, so a
// statement that would fail to compile fails here too. It reads no row
// and writes none, and no subquery runs.
//
// The plan is rows of one TEXT column, plan. The first row is the
// statement, and each subquery has a row after it. A subquery inside a
// subquery has the number of its parent before its own: 1, 1.1, 2.

// Explain returns the plan of the statement of ex over the tables of sc.
func Explain(sc *table.Schema, ex *sqlparse.Explain, lim Limits) (*Rows, error) {
	var lines []string
	var subs *subqueries
	switch s := ex.Stmt.(type) {
	case *sqlparse.Select:
		q, err := prepare(sc, s, lim, nil)
		if err != nil {
			return nil, err
		}
		lines, subs = []string{q.Plan()}, q.subs
	case *sqlparse.Insert:
		p, err := prepareInsert(sc, s, lim)
		if err != nil {
			return nil, err
		}
		lines, subs = []string{fmt.Sprintf("INSERT INTO %s: %d %s", p.t.Name, len(p.rows), plural(len(p.rows), "ROW"))}, p.subs
		if p.up != nil {
			lines = append(lines, p.up.describe(p.t))
		}
	case *sqlparse.Update:
		p, err := prepareUpdate(sc, s, lim)
		if err != nil {
			return nil, err
		}
		lines, subs = []string{"UPDATE " + p.t.Name + ": " + writeAccess(p.t, s.Where, lim).describe(p.t)}, p.subs
	case *sqlparse.Delete:
		t, _, ss, err := prepareDelete(sc, s, lim)
		if err != nil {
			return nil, err
		}
		lines, subs = []string{"DELETE FROM " + t.Name + ": " + writeAccess(t, s.Where, lim).describe(t)}, ss
	default:
		return nil, errAt(ex.Stmt.Pos(), "EXPLAIN takes SELECT, INSERT, UPDATE or DELETE")
	}
	lines = subs.explain("", lines)
	i := 0
	next := func() ([]any, error) {
		if i == len(lines) {
			return nil, nil
		}
		i++
		return []any{lines[i-1]}, nil
	}
	q := &Query{names: []Column{{Name: "plan", Type: table.String, Known: true}}}
	return &Rows{q: q, next: next, scanned: new(int64)}, nil
}

// explain adds a row for each subquery to lines, and the rows of the
// subqueries inside it after it.
func (s *subqueries) explain(prefix string, lines []string) []string {
	for i, sub := range s.list {
		n := prefix + strconv.Itoa(i+1)
		line := "SUBQUERY " + n + " " + sub.text
		if len(sub.keys) > 0 {
			line += ", CORRELATED"
		}
		lines = append(lines, line+": "+sub.q.Plan())
		lines = sub.q.subs.explain(n+".", lines)
	}
	return lines
}

// describe writes ON CONFLICT for EXPLAIN: the keys it looks up, then
// what it does on a conflict.
func (up *upsert) describe(t *table.Table) string {
	keys := make([]string, len(up.keys))
	for i, k := range up.keys {
		keys[i] = (&access{index: k.index}).name(t)
	}
	do := "DO UPDATE"
	if up.nothing {
		do = "DO NOTHING"
	}
	return "ON CONFLICT: LOOK UP " + strings.Join(keys, ", ") + "; " + do
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "S"
}
