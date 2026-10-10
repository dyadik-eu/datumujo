package sqlexec

import (
	"fmt"
	"math"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// A query with GROUP BY, HAVING or an aggregate runs in two steps. The
// first reads the rows that pass WHERE and puts each into its group. The
// second computes one row per group: the values of GROUP BY, then the
// result of each aggregate. The select list, HAVING and ORDER BY read
// that group row.
//
// Before they compile, each aggregate and each expression of GROUP BY in
// them becomes a column of the group row. A column of the table that is
// left over is an error. SQLite would take its value from some row of
// the group, a value that no rule picks.

// isAggregate reports a call of count, sum, avg, min or max over one
// argument. min and max with more arguments are the scalar functions.
func isAggregate(c *sqlparse.Call) bool {
	switch c.Name {
	case "count", "sum", "avg":
		return true
	case "min", "max":
		return len(c.Args) == 1
	}
	return false
}

// hasAggregate reports whether an expression holds an aggregate.
func hasAggregate(e sqlparse.Expr) bool {
	found := false
	rewrite(e, func(x sqlparse.Expr) (sqlparse.Expr, bool) {
		if c, ok := x.(*sqlparse.Call); ok && isAggregate(c) {
			found = true
			return x, true
		}
		return nil, false
	})
	return found
}

// rewrite returns a copy of e in which f replaces nodes. f returns the
// replacement and true, or false to go into the node.
func rewrite(e sqlparse.Expr, f func(sqlparse.Expr) (sqlparse.Expr, bool)) sqlparse.Expr {
	if e == nil {
		return nil
	}
	if r, ok := f(e); ok {
		return r
	}
	each := func(es []sqlparse.Expr) []sqlparse.Expr {
		out := make([]sqlparse.Expr, len(es))
		for i, x := range es {
			out[i] = rewrite(x, f)
		}
		return out
	}
	switch x := e.(type) {
	case *sqlparse.Unary:
		c := *x
		c.X = rewrite(x.X, f)
		return &c
	case *sqlparse.Binary:
		c := *x
		c.L, c.R = rewrite(x.L, f), rewrite(x.R, f)
		return &c
	case *sqlparse.In:
		c := *x
		c.X, c.List = rewrite(x.X, f), each(x.List)
		return &c
	case *sqlparse.Between:
		c := *x
		c.X, c.Lo, c.Hi = rewrite(x.X, f), rewrite(x.Lo, f), rewrite(x.Hi, f)
		return &c
	case *sqlparse.Call:
		c := *x
		c.Args = each(x.Args)
		return &c
	case *sqlparse.Cast:
		c := *x
		c.X = rewrite(x.X, f)
		return &c
	case *sqlparse.Case:
		c := *x
		c.Operand, c.Else = rewrite(x.Operand, f), rewrite(x.Else, f)
		c.Whens = make([]sqlparse.When, len(x.Whens))
		for i, w := range x.Whens {
			c.Whens[i] = sqlparse.When{Cond: rewrite(w.Cond, f), Result: rewrite(w.Result, f)}
		}
		return &c
	}
	return e // Literal, Param, ColumnRef
}

// aggFunc is one aggregate of a query.
type aggFunc struct {
	name     string
	arg      *Expr // nil for count(*)
	distinct bool
	at       sqlparse.At
}

// groupPlan holds what the first step computes: the values of GROUP BY
// and the aggregates. The group row has them in this order.
type groupPlan struct {
	keys  []*Expr
	texts []string // the text of each column of the group row
	aggs  []aggFunc
	slots map[string]int
	types []Column
	subs  *subqueries // of the query
}

// groupScope resolves the columns of the group row. Any other column is
// an error.
type groupScope struct{ g *groupPlan }

func (s groupScope) Column(tbl, name string) (int, table.Type, error) {
	if tbl == sqlparse.Derived {
		if i, ok := s.g.slots[name]; ok {
			return i, s.g.types[i].Type, nil
		}
	}
	if tbl != "" && tbl != sqlparse.Derived {
		name = tbl + "." + name
	}
	return 0, 0, fmt.Errorf("column %s must be in GROUP BY or in an aggregate function", name)
}

func (g *groupPlan) slot(text string, c Column) {
	if _, ok := g.slots[text]; ok {
		return
	}
	g.slots[text] = len(g.texts)
	g.texts = append(g.texts, text)
	g.types = append(g.types, c)
}

// derive rewrites an expression of the select list, HAVING or ORDER BY
// for the group row. It adds each aggregate it finds.
func (g *groupPlan) derive(e sqlparse.Expr, rows Resolver) (sqlparse.Expr, error) {
	var err error
	out := rewrite(e, func(x sqlparse.Expr) (sqlparse.Expr, bool) {
		if err != nil {
			return x, true
		}
		text := x.String()
		if _, ok := g.slots[text]; ok {
			return &sqlparse.ColumnRef{At: x.Pos(), Table: sqlparse.Derived, Name: text}, true
		}
		c, ok := x.(*sqlparse.Call)
		if !ok || !isAggregate(c) {
			return nil, false
		}
		var col Column
		if col, err = g.addAggregate(c, rows); err != nil {
			return x, true
		}
		g.slot(text, col)
		return &sqlparse.ColumnRef{At: x.Pos(), Table: sqlparse.Derived, Name: text}, true
	})
	return out, err
}

// addAggregate compiles an aggregate over the rows of the table and
// returns the type of its result.
func (g *groupPlan) addAggregate(c *sqlparse.Call, rows Resolver) (Column, error) {
	a := aggFunc{name: c.Name, distinct: c.Distinct, at: c.At}
	col := Column{Name: c.String()}
	switch {
	case c.Star:
		if c.Name != "count" {
			return col, errAt(c.At, "%s(*) does not exist; only count(*)", c.Name)
		}
	case len(c.Args) != 1:
		return col, errAt(c.At, "%s takes 1 argument, not %d", c.Name, len(c.Args))
	default:
		for _, arg := range c.Args {
			if hasAggregate(arg) {
				return col, errAt(arg.Pos(), "an aggregate inside the aggregate %s", c.Name)
			}
		}
		x, err := compileWith(c.Args[0], rows, g.subs)
		if err != nil {
			return col, err
		}
		a.arg = x
		if c.Name == "sum" || c.Name == "avg" {
			if err := want(x.n, c.Name, numbers...); err != nil {
				return col, err
			}
		}
	}
	switch c.Name {
	case "count":
		col.Type, col.Known = table.Int64, true
	case "avg":
		col.Type, col.Known = table.Float64, true
	default: // sum, min and max give values of the type of the argument
		col.Type, col.Known = a.arg.Type()
	}
	g.aggs = append(g.aggs, a)
	return col, nil
}

// sumState adds numbers as SQLite does: INTEGER while all values are
// INTEGER and the sum fits, else REAL with the compensated summation of
// Kahan, Babuska and Neumaier. An INTEGER overflow is an error, unless a
// REAL value comes after it.
type sumState struct {
	cnt          int64
	iSum         int64
	rSum, rErr   float64
	approx, ovfl bool
}

// kbnStep adds r to the compensated sum.
func (s *sumState) kbnStep(r float64) {
	t := s.rSum + r
	if math.Abs(s.rSum) > math.Abs(r) {
		s.rErr += (s.rSum - t) + r
	} else {
		s.rErr += (r - t) + s.rSum
	}
	s.rSum = t
}

// bigInt is the size from which an int64 has no exact float64 in every
// case; such a value goes into the sum in two parts.
const bigInt = 4503599627370496 // 2^52

func (s *sumState) kbnStepInt(v int64) {
	if v <= -bigInt || v >= bigInt {
		small := v % 16384
		s.kbnStep(float64(v - small))
		s.kbnStep(float64(small))
		return
	}
	s.kbnStep(float64(v))
}

func (s *sumState) kbnInit(v int64) {
	if v <= -bigInt || v >= bigInt {
		small := v % 16384
		s.rSum, s.rErr = float64(v-small), float64(small)
		return
	}
	s.rSum, s.rErr = float64(v), 0
}

func (s *sumState) add(v any) {
	s.cnt++
	i, isInt := v.(int64)
	if !s.approx {
		if !isInt {
			s.kbnInit(s.iSum)
			s.approx = true
			s.kbnStep(v.(float64))
			return
		}
		sum := s.iSum + i
		if (s.iSum > 0 && i > 0 && sum < 0) || (s.iSum < 0 && i < 0 && sum >= 0) {
			s.ovfl = true
			s.kbnInit(s.iSum)
			s.approx = true
			s.kbnStepInt(i)
			return
		}
		s.iSum = sum
		return
	}
	if isInt {
		s.kbnStepInt(i)
		return
	}
	s.ovfl = false
	s.kbnStep(v.(float64))
}

// real returns the REAL sum, with the error term where it is finite.
func (s *sumState) real() float64 {
	r := s.rSum
	if !math.IsInf(s.rErr, 0) && !math.IsNaN(s.rErr) {
		r += s.rErr
	}
	return r
}

// aggState is the state of one aggregate in one group.
type aggState struct {
	n    int64
	best any
	sum  sumState
	seen map[string]bool
}

// step adds the value of a row. It reports the bytes it kept, for the
// bound of memory.
func (a *aggFunc) step(s *aggState, row, params []any) (int64, error) {
	if a.arg == nil {
		s.n++
		return 0, nil
	}
	v, err := a.arg.Eval(row, params)
	if err != nil || v == nil {
		return 0, err
	}
	var kept int64
	if a.distinct {
		k := distinctKey([]any{v})
		if s.seen[k] {
			return 0, nil
		}
		if s.seen == nil {
			s.seen = map[string]bool{}
		}
		s.seen[k] = true
		kept = int64(len(k)) + 24
	}
	switch a.name {
	case "count":
		s.n++
	case "sum", "avg":
		if err := check(a.arg.n, v, a.name, numbers...); err != nil {
			return 0, err
		}
		s.sum.add(v)
	case "min", "max":
		if s.best == nil {
			s.best = v
			return kept, nil
		}
		ta, _ := typeOf(s.best)
		tb, _ := typeOf(v)
		if !comparable(ta, tb) {
			return 0, errAt(a.at, "%s: %s and %s do not compare", a.name, TypeName(ta), TypeName(tb))
		}
		d, ok := compare(v, s.best)
		if ok && (a.name == "min" && d < 0 || a.name == "max" && d > 0) {
			s.best = v
		}
	}
	return kept, nil
}

// result finishes an aggregate for its group.
func (a *aggFunc) result(s *aggState) (any, error) {
	switch a.name {
	case "count":
		return s.n, nil
	case "min", "max":
		return s.best, nil
	}
	if s.sum.cnt == 0 {
		return nil, nil
	}
	if a.name == "avg" {
		if s.sum.approx {
			return s.sum.real() / float64(s.sum.cnt), nil
		}
		return float64(s.sum.iSum) / float64(s.sum.cnt), nil
	}
	if !s.sum.approx {
		return s.sum.iSum, nil
	}
	if s.sum.ovfl {
		return nil, errAt(a.at, "integer overflow in sum")
	}
	r := s.sum.real()
	if math.IsInf(r, 0) {
		return nil, errAt(a.at, "REAL overflow in sum")
	}
	return r, nil
}

// group is the state of one group.
type group struct {
	keys []any
	aggs []aggState
}

// groups reads the rows and returns one group row for each group, in the
// order in which the groups first come. Without GROUP BY, there is one
// group, also when no row passes WHERE.
func (q *Query) groups(read func() ([]any, bool, error), params []any) ([][]any, error) {
	g := q.group
	mem := memory{max: q.lim.maxMemory(), at: q.st.At}
	byKey := map[string]*group{}
	var order []*group
	for {
		row, ok, err := read()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		keep, err := keeps(q.where, q.st.Where, row, params)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		keys := make([]any, len(g.keys))
		for i, k := range g.keys {
			if keys[i], err = k.Eval(row, params); err != nil {
				return nil, err
			}
		}
		id := distinctKey(keys)
		gr, ok := byKey[id]
		if !ok {
			gr = &group{keys: keys, aggs: make([]aggState, len(g.aggs))}
			byKey[id] = gr
			order = append(order, gr)
			if err := mem.add(keys); err != nil {
				return nil, err
			}
			mem.used += int64(len(id)) + 48*int64(len(g.aggs))
		}
		for i := range g.aggs {
			kept, err := g.aggs[i].step(&gr.aggs[i], row, params)
			if err != nil {
				return nil, err
			}
			mem.used += kept
		}
		if err := mem.add(nil); err != nil {
			return nil, err
		}
	}
	if len(g.keys) == 0 && len(order) == 0 {
		order = append(order, &group{aggs: make([]aggState, len(g.aggs))})
	}
	out := make([][]any, 0, len(order))
	for _, gr := range order {
		row := append([]any(nil), gr.keys...)
		for i := range g.aggs {
			v, err := g.aggs[i].result(&gr.aggs[i])
			if err != nil {
				return nil, err
			}
			row = append(row, v)
		}
		out = append(out, row)
	}
	return out, nil
}

// prepareGroups compiles a query with GROUP BY, HAVING or an aggregate.
// It sets the columns of the result, HAVING and ORDER BY of q.
func (q *Query) prepareGroups(st *sqlparse.Select, rows Resolver) error {
	g := &groupPlan{slots: map[string]int{}, subs: q.subs}
	q.group = g
	for _, e := range st.GroupBy {
		if hasAggregate(e) {
			return errAt(e.Pos(), "an aggregate in GROUP BY")
		}
		if _, ok := g.slots[e.String()]; ok {
			// A repeated expression adds nothing to the groups. As a
			// second key it would shift every later column of the group
			// row off its slot.
			continue
		}
		x, err := compileWith(e, rows, q.subs)
		if err != nil {
			return err
		}
		typ, known := x.Type()
		g.keys = append(g.keys, x)
		g.slot(e.String(), Column{Name: e.String(), Type: typ, Known: known})
	}
	// The select list, HAVING and ORDER BY add aggregates, so the slots
	// are complete only after all of them are rewritten.
	var items []sqlparse.Expr
	for _, it := range st.Items {
		if it.Star {
			return errAt(it.At, "* with GROUP BY or an aggregate: name the columns")
		}
		e, err := g.derive(it.Expr, rows)
		if err != nil {
			return err
		}
		items = append(items, e)
	}
	var having sqlparse.Expr
	if st.Having != nil {
		var err error
		if having, err = g.derive(st.Having, rows); err != nil {
			return err
		}
	}
	orders := make([]sqlparse.Expr, len(st.OrderBy))
	for i, o := range st.OrderBy {
		if q.orderTarget(o) != -1 {
			continue
		}
		e, err := g.derive(o.Expr, rows)
		if err != nil {
			return err
		}
		orders[i] = e
	}
	scope := groupScope{g}
	for i, e := range items {
		x, err := compileWith(e, scope, q.subs)
		if err != nil {
			return err
		}
		it := st.Items[i]
		name := it.Alias
		if name == "" {
			if ref, ok := it.Expr.(*sqlparse.ColumnRef); ok {
				name = ref.Name
			} else {
				name = it.Expr.String()
			}
		}
		typ, known := x.Type()
		q.cols = append(q.cols, outCol{row: -1, expr: x})
		q.names = append(q.names, Column{Name: name, Type: typ, Known: known})
	}
	if having != nil {
		x, err := compileWith(having, scope, q.subs)
		if err != nil {
			return err
		}
		if typ, ok := x.Type(); ok && typ != table.Bool {
			return errAt(st.Having.Pos(), "HAVING needs BOOLEAN, and %s is %s", st.Having, TypeName(typ))
		}
		q.having = x
	}
	for i, o := range st.OrderBy {
		k := orderKey{out: q.orderTarget(o), desc: o.Desc}
		if l, ok := o.Expr.(*sqlparse.Literal); ok {
			if n, ok := l.Value.(int64); ok && (n < 1 || n > int64(len(q.cols))) {
				return errAt(l.At, "ORDER BY %d: the result has %d columns", n, len(q.cols))
			}
		}
		if k.out == -1 {
			x, err := compileWith(orders[i], scope, q.subs)
			if err != nil {
				return err
			}
			k.expr = x
		}
		q.order = append(q.order, k)
	}
	return nil
}

// orderTarget returns the column of the result that a term of ORDER BY
// names by number or by alias, or -1. A number out of range is -1 here;
// the caller reports it.
func (q *Query) orderTarget(o sqlparse.Order) int {
	if l, ok := o.Expr.(*sqlparse.Literal); ok {
		if n, ok := l.Value.(int64); ok {
			if n < 1 || n > int64(len(q.st.Items)) {
				return -2
			}
			return int(n - 1)
		}
	}
	if ref, ok := o.Expr.(*sqlparse.ColumnRef); ok && ref.Table == "" {
		for i, it := range q.st.Items {
			if it.Alias == ref.Name {
				return i
			}
		}
	}
	return -1
}
