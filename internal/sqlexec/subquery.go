package sqlexec

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// Subqueries (L-16). A subquery runs at its first use in a run of its
// statement and keeps its answer. A subquery that no row needs, as in a
// branch of CASE that is never taken, never runs.
//
// A subquery may read columns of the queries around it: it is then
// correlated. Each such name is a slot of its frame. Before each run,
// the query around fills the frame from the row it is at. The
// subquery runs again when those values change, and keeps its answer
// while they stay the same. One that reads no column of a query around
// has no slot, so it runs once in a run of its statement.
//
// A subquery reads the state before its statement writes anything:
// INSERT computes all its rows first, and UPDATE and DELETE find all
// their rows and compute their new values before the first write.

// subqueries holds the subqueries of one query or statement. It also
// holds the schema and limits to prepare them. The field outers holds
// the resolvers of the queries around, the innermost last. The field
// refs holds the names of the queries around that this query reads, and
// frame their values in the current run.
type subqueries struct {
	sc     *table.Schema
	lim    Limits
	outers []Resolver
	local  Resolver // all tables of the query; nil for a statement
	list   []*subquery
	refs   []outerRef
	frame  []any
}

// outerRef is a name that a query reads from a query around it.
type outerRef struct {
	at          sqlparse.At
	table, name string
}

func newSubqueries(sc *table.Schema, lim Limits, outers []Resolver) *subqueries {
	return &subqueries{sc: sc, lim: lim, outers: outers}
}

// bind starts a run: each subquery forgets its answer and runs on src
// with params at its next use.
func (s *subqueries) bind(src Source, params []any) {
	for _, sub := range s.list {
		*sub = subquery{q: sub.q, at: sub.at, text: sub.text, keys: sub.keys, src: src, params: params, bound: true}
	}
}

// subquery is one subquery of a statement, with its answer for the
// current run. keys compute the values of its frame from the row of the
// query around.
type subquery struct {
	q    *Query
	at   sqlparse.At
	text string
	keys []node

	src    Source
	params []any
	bound  bool
	done   bool
	last   []any   // the frame of the answer
	rows   [][]any // at most two: enough to tell one row from more
	err    error
	set    *inSet // the answer after IN, once read
}

// fresh fills the frame from the row of e. It reports whether the
// answer of the last run holds for it: the same values in each slot.
func (s *subquery) fresh(e *env) (bool, error) {
	if !s.bound {
		return false, fmt.Errorf("sqlexec: the subquery %s ran before its statement started", s.text)
	}
	vals := make([]any, len(s.keys))
	for i, k := range s.keys {
		v, err := k.eval(e)
		if err != nil {
			return false, err
		}
		vals[i] = v
	}
	if s.done && sameValues(vals, s.last) {
		return true, nil
	}
	s.done, s.last, s.rows, s.set, s.err = true, vals, nil, nil, nil
	s.q.subs.frame = vals
	return false, nil
}

// sameValues reports whether two frames hold the same values, bit for
// bit: -0.0 and 0.0 give different answers, and NaN is never the same.
func sameValues(a, b []any) bool {
	for i := range a {
		switch x := a[i].(type) {
		case float64:
			y, ok := b[i].(float64)
			if !ok || x != y || math.Float64bits(x) != math.Float64bits(y) {
				return false
			}
		case []byte:
			y, ok := b[i].([]byte)
			if !ok || !bytes.Equal(x, y) {
				return false
			}
		case time.Time:
			y, ok := b[i].(time.Time)
			if !ok || !x.Equal(y) || x.Location() != y.Location() {
				return false
			}
		default:
			if a[i] != b[i] {
				return false
			}
		}
	}
	return true
}

// run runs the subquery for the row of e and reads at most max rows of
// it.
func (s *subquery) run(e *env, max int) error {
	if ok, err := s.fresh(e); ok || err != nil {
		if err != nil {
			return err
		}
		return s.err
	}
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
// expression around becomes one of its outers. Each name the subquery
// reads from a query around then compiles here, as a key of its frame.
// If this query does not know a name either, the name becomes a slot of
// its own frame. So a subquery can read any query around it.
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
	for _, ref := range q.subs.refs {
		k, err := c.expr(&sqlparse.ColumnRef{At: ref.at, Table: ref.table, Name: ref.name})
		if err != nil {
			return nil, err
		}
		sub.keys = append(sub.keys, k)
	}
	c.subs.list = append(c.subs.list, sub)
	return sub, nil
}

// outer compiles a name that the resolver of c does not know, err being
// its answer. The name becomes a slot of the frame of this query. That
// holds if no table of the query knows the name and a query around does.
func (c *compiler) outer(x *sqlparse.ColumnRef, err error) (node, bool) {
	if c.subs == nil || c.own || !errors.Is(err, errNoColumn) {
		return node{}, false
	}
	if c.subs.local != nil {
		if _, _, err := c.subs.local.Column(x.Table, x.Name); !errors.Is(err, errNoColumn) {
			return node{}, false
		}
	}
	n, found := node{}, false
	for i := len(c.subs.outers) - 1; i >= 0; i-- {
		_, typ, err := c.subs.outers[i].Column(x.Table, x.Name)
		if err == nil {
			n, found = known(typ), true
			break
		}
		if !errors.Is(err, errNoColumn) {
			found = true // the query around reports its error
			break
		}
	}
	if !found {
		return node{}, false
	}
	subs, j := c.subs, -1
	for i, r := range subs.refs {
		if r.table == x.Table && r.name == x.Name {
			j = i
		}
	}
	if j < 0 {
		j = len(subs.refs)
		subs.refs = append(subs.refs, outerRef{at: x.At, table: x.Table, name: x.Name})
	}
	n.eval = func(*env) (any, error) { return subs.frame[j], nil }
	return n, true
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
	n.eval = func(e *env) (any, error) {
		if err := sub.run(e, 2); err != nil {
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
	n.eval = func(e *env) (any, error) {
		if err := sub.run(e, 1); err != nil {
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

// readSet reads all rows of the subquery for the row of e. The values
// count toward the memory of the statement. They must compare with one
// another: a column whose type is only known at run time may mix them.
func (s *subquery) readSet(e *env, at sqlparse.At, lim Limits) (*inSet, error) {
	if ok, err := s.fresh(e); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return s.set, s.err
	}
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
		set, err := sub.readSet(e, x.Select.At, lim)
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
