package sqlexec

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// Subqueries (L-16). A subquery here reads no column of the query around
// it, so each run of its statement gives it one answer. It runs once, at
// its first use, and keeps the answer for the rest of the run, as in
// SQLite. A subquery that no row needs, as in a branch of CASE that is
// never taken, never runs.
//
// A subquery reads the state before its statement writes anything:
// INSERT computes all its rows first, and UPDATE and DELETE find all
// their rows before the first write.

// subqueries holds the subqueries of one statement. It also holds what
// they need: the schema and limits to prepare them, and the resolvers of
// the queries around (innermost last).
type subqueries struct {
	sc     *table.Schema
	lim    Limits
	outers []Resolver
	list   []*subquery
}

func newSubqueries(sc *table.Schema, lim Limits, outers []Resolver) *subqueries {
	return &subqueries{sc: sc, lim: lim, outers: outers}
}

// bind starts a run: each subquery forgets its answer and runs on src
// with params at its next use.
func (s *subqueries) bind(src Source, params []any) {
	for _, sub := range s.list {
		*sub = subquery{q: sub.q, at: sub.at, text: sub.text, src: src, params: params, bound: true}
	}
}

// subquery is one subquery of a statement, with its answer for the
// current run.
type subquery struct {
	q    *Query
	at   sqlparse.At
	text string

	src    Source
	params []any
	bound  bool
	done   bool
	rows   [][]any // at most two: enough to tell one row from more
	err    error
	set    *inSet // the answer after IN, once read
}

// run runs the subquery once per run and reads at most max rows of it.
func (s *subquery) run(max int) error {
	if !s.bound {
		return fmt.Errorf("sqlexec: the subquery %s ran before its statement started", s.text)
	}
	if s.done {
		return s.err
	}
	s.done = true
	rows, err := s.q.Run(s.src, s.params)
	if err != nil {
		s.err = err
		return err
	}
	for len(s.rows) < max && rows.Next() {
		s.rows = append(s.rows, rows.Row())
	}
	s.err = rows.Err()
	return s.err
}

// prepare compiles the SELECT of a subquery. The resolver of the
// expression around becomes one of its outers. A name that only the
// query around knows is a correlated subquery, roadmap step 32.
func (c *compiler) prepare(at sqlparse.At, sel *sqlparse.Select) (*subquery, error) {
	if c.subs == nil {
		return nil, errAt(at, "a subquery cannot run here")
	}
	outers := append(slices.Clone(c.subs.outers), c.r)
	q, err := prepare(c.subs.sc, sel, c.subs.lim, outers)
	if err != nil {
		return nil, err
	}
	sub := &subquery{q: q, at: at, text: "(" + sel.String() + ")"}
	c.subs.list = append(c.subs.list, sub)
	return sub, nil
}

// outerColumn reports whether a name is a column of a query around. If
// the resolver of c does not know it, it is a correlated subquery.
func (c *compiler) outerColumn(tbl, name string) bool {
	if c.subs == nil {
		return false
	}
	for _, r := range c.subs.outers {
		if r == nil {
			continue
		}
		if _, _, err := r.Column(tbl, name); err == nil {
			return true
		}
	}
	return false
}

// scalar compiles (SELECT ...) as a value: the one column of its row,
// NULL without a row. More than one row is an error (L-16).
func (c *compiler) scalar(x *sqlparse.Subquery) (node, error) {
	sub, err := c.prepare(x.At, x.Select)
	if err != nil {
		return node{}, err
	}
	cols := sub.q.names
	if len(cols) != 1 {
		return node{}, errAt(x.At, "a subquery as a value gives one column, and %s gives %d", sub.text, len(cols))
	}
	n := node{typ: cols[0].Type, known: cols[0].Known}
	n.eval = func(*env) (any, error) {
		if err := sub.run(2); err != nil {
			return nil, err
		}
		switch len(sub.rows) {
		case 0:
			return nil, nil
		case 1:
			return sub.rows[0][0], nil
		}
		return nil, errAt(x.At, "the subquery %s gives more than one row, and a subquery as a value needs at most one", sub.text)
	}
	return n, nil
}

// exists compiles EXISTS (SELECT ...): TRUE if the query has a row.
func (c *compiler) exists(x *sqlparse.Exists) (node, error) {
	sub, err := c.prepare(x.At, x.Select)
	if err != nil {
		return node{}, err
	}
	n := known(table.Bool)
	n.eval = func(*env) (any, error) {
		if err := sub.run(1); err != nil {
			return nil, err
		}
		return len(sub.rows) > 0, nil
	}
	return n, nil
}

// inSet is the answer of a subquery after IN: its values, sorted, and
// whether one of them is NULL or NaN, which equals nothing.
type inSet struct {
	vals    []any
	unknown bool
}

// readSet reads all rows of the subquery once per run. The values count
// toward the memory of the statement. They must compare with one
// another: a column whose type is only known at run time may mix them.
func (s *subquery) readSet(at sqlparse.At, lim Limits) (*inSet, error) {
	if !s.bound {
		return nil, fmt.Errorf("sqlexec: the subquery %s ran before its statement started", s.text)
	}
	if s.done {
		return s.set, s.err
	}
	s.done = true
	s.set, s.err = s.collect(at, lim)
	return s.set, s.err
}

func (s *subquery) collect(at sqlparse.At, lim Limits) (*inSet, error) {
	rows, err := s.q.Run(s.src, s.params)
	if err != nil {
		return nil, err
	}
	mem := &memory{max: lim.maxMemory(), at: at}
	set := &inSet{}
	var first table.Type
	for rows.Next() {
		v := rows.Row()[0]
		if err := mem.add([]any{v}); err != nil {
			return nil, err
		}
		if f, ok := v.(float64); v == nil || ok && math.IsNaN(f) {
			set.unknown = true
			continue
		}
		typ, _ := typeOf(v)
		if len(set.vals) == 0 {
			first = typ
		} else if !comparable(first, typ) {
			return nil, errAt(at, "IN: the subquery %s gives %s and %s; they do not compare", s.text, TypeName(first), TypeName(typ))
		}
		set.vals = append(set.vals, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(set.vals, func(i, j int) bool {
		d, _ := compare(set.vals[i], set.vals[j])
		return d < 0
	})
	return set, nil
}

// has reports whether v is in the set: true or false. It gives nil when
// v or a value of the set is NULL and nothing equals v. An empty set
// holds nothing, so even NULL is not in it.
func (set *inSet) has(v any) any {
	if len(set.vals) == 0 && !set.unknown {
		return false
	}
	if v == nil {
		return nil
	}
	if f, ok := v.(float64); ok && math.IsNaN(f) {
		return nil
	}
	i := sort.Search(len(set.vals), func(i int) bool {
		d, _ := compare(set.vals[i], v)
		return d >= 0
	})
	if i < len(set.vals) {
		if d, _ := compare(set.vals[i], v); d == 0 {
			return true
		}
	}
	if set.unknown {
		return nil
	}
	return false
}

// inSelect compiles x IN (SELECT ...) with x compiled as a. The query
// gives one column, which must compare with a.
func (c *compiler) inSelect(x *sqlparse.In, a node) (node, error) {
	sub, err := c.prepare(x.Select.At, x.Select)
	if err != nil {
		return node{}, err
	}
	cols := sub.q.names
	if len(cols) != 1 {
		return node{}, errAt(x.Select.At, "IN takes a subquery of one column, and %s gives %d", sub.text, len(cols))
	}
	b := node{typ: cols[0].Type, known: cols[0].Known, at: x.Select.At, desc: sub.text}
	if err := comparableNodes(a, b, "IN"); err != nil {
		return node{}, err
	}
	lim := c.subs.lim
	n := known(table.Bool)
	n.eval = func(e *env) (any, error) {
		v, err := a.eval(e)
		if err != nil {
			return nil, err
		}
		set, err := sub.readSet(x.Select.At, lim)
		if err != nil {
			return nil, err
		}
		if len(set.vals) > 0 {
			if err := comparableValues(a, b, v, set.vals[0], "IN"); err != nil {
				return nil, err
			}
		}
		r := set.has(v)
		if r == nil || !x.Not {
			return r, nil
		}
		return !r.(bool), nil
	}
	return n, nil
}
