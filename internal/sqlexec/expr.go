package sqlexec

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

func errAt(at sqlparse.At, format string, a ...any) error {
	return &sqlparse.Error{At: at, Msg: fmt.Sprintf(format, a...)}
}

// env is what an expression reads when it runs: the current row and the
// parameters.
type env struct {
	row    []any
	params []any
}

type evalFn func(e *env) (any, error)

// node is a compiled expression. If known is set, every value it gives is
// nil or of type typ. The fuzz test checks this.
type node struct {
	typ   table.Type
	known bool
	at    sqlparse.At
	desc  string // the expression as SQL, for messages
	eval  evalFn
}

// Resolver finds a column for a name: its place in the row and its type.
// table is "" when the name has no table.
type Resolver interface {
	Column(table, name string) (int, table.Type, error)
}

type noColumns struct{}

func (noColumns) Column(t, name string) (int, table.Type, error) {
	if t != "" {
		name = t + "." + name
	}
	return 0, 0, fmt.Errorf("no such column: %s", name)
}

// Expr is a compiled expression.
type Expr struct{ n node }

// Compile checks an expression and prepares it to run. The types of
// columns come from r; r may be nil when there are no columns. A type
// that does not fit is an error here, before any row is read. Where a
// type is known only when the expression runs, as for a parameter, the
// check runs then.
func Compile(e sqlparse.Expr, r Resolver) (*Expr, error) {
	if r == nil {
		r = noColumns{}
	}
	c := &compiler{r: r}
	n, err := c.expr(e)
	if err != nil {
		return nil, err
	}
	return &Expr{n}, nil
}

// Type returns the type of the values, if it is known before a run.
func (x *Expr) Type() (table.Type, bool) { return x.n.typ, x.n.known }

// Eval runs the expression for one row with the parameters. params[0] is
// ?1.
func (x *Expr) Eval(row, params []any) (any, error) {
	return x.n.eval(&env{row: row, params: params})
}

type compiler struct {
	r Resolver
}

func known(t table.Type) node { return node{typ: t, known: true} }

// want checks, before the run, that a node of known type has one of the
// allowed types.
func want(n node, what string, allowed ...table.Type) error {
	if !n.known {
		return nil
	}
	for _, t := range allowed {
		if n.typ == t {
			return nil
		}
	}
	return errAt(n.at, "%s needs %s, and %s is %s", what, typeList(allowed), n.desc, TypeName(n.typ))
}

// check is want at run time, for a value of a node whose type was not
// known before.
func check(n node, v any, what string, allowed ...table.Type) error {
	if v == nil || n.known {
		return nil
	}
	t, _ := typeOf(v)
	for _, a := range allowed {
		if t == a {
			return nil
		}
	}
	return errAt(n.at, "%s needs %s, and %s is %s", what, typeList(allowed), n.desc, TypeName(t))
}

func typeList(ts []table.Type) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = TypeName(t)
	}
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

var numbers = []table.Type{table.Int64, table.Float64}

// comparableNodes checks before the run that two nodes can compare.
func comparableNodes(a, b node, what string) error {
	if a.known && b.known && !comparable(a.typ, b.typ) {
		return errAt(b.at, "%s: %s is %s and %s is %s; they do not compare", what, a.desc, TypeName(a.typ), b.desc, TypeName(b.typ))
	}
	return nil
}

// comparableValues checks at run time, when a type was not known before.
func comparableValues(a, b node, x, y any, what string) error {
	if x == nil || y == nil || a.known && b.known {
		return nil
	}
	tx, _ := typeOf(x)
	ty, _ := typeOf(y)
	if !comparable(tx, ty) {
		return errAt(b.at, "%s: %s is %s and %s is %s; they do not compare", what, a.desc, TypeName(tx), b.desc, TypeName(ty))
	}
	return nil
}

// same checks that nodes whose values stand for one another, such as the
// branches of CASE, have one type. It returns that type if one is known.
func same(ns []node, what string) (node, error) {
	var first *node
	for i := range ns {
		n := &ns[i]
		if !n.known {
			continue
		}
		if first == nil {
			first = n
			continue
		}
		if n.typ != first.typ {
			return node{}, errAt(n.at, "%s: %s is %s and %s is %s; they must have one type", what, first.desc, TypeName(first.typ), n.desc, TypeName(n.typ))
		}
	}
	if first == nil {
		return node{}, nil
	}
	return known(first.typ), nil
}

// conform makes a node of unknown type give values of type t only.
func conform(n node, t node, what string) node {
	if n.known || !t.known {
		return n
	}
	inner := n.eval
	n.eval = func(e *env) (any, error) {
		v, err := inner(e)
		if err != nil || v == nil {
			return v, err
		}
		if vt, _ := typeOf(v); vt != t.typ {
			return nil, errAt(n.at, "%s: %s is %s, want %s", what, n.desc, TypeName(vt), TypeName(t.typ))
		}
		return v, nil
	}
	return n
}

func (c *compiler) expr(e sqlparse.Expr) (node, error) {
	n, err := c.node(e)
	if err != nil {
		return node{}, err
	}
	n.at, n.desc = e.Pos(), e.String()
	return n, nil
}

func (c *compiler) node(e sqlparse.Expr) (node, error) {
	switch x := e.(type) {
	case *sqlparse.Literal:
		v := x.Value
		t, ok := typeOf(v)
		n := node{typ: t, known: ok}
		n.eval = func(*env) (any, error) { return v, nil }
		return n, nil
	case *sqlparse.Param:
		i, at := x.N-1, x.At
		n := node{}
		n.eval = func(e *env) (any, error) {
			if i >= len(e.params) {
				return nil, errAt(at, "parameter ?%d has no value; %d given", i+1, len(e.params))
			}
			v := e.params[i]
			if v == nil {
				return nil, nil
			}
			if _, ok := typeOf(v); !ok {
				return nil, errAt(at, "parameter ?%d is a Go %T; want int64, float64, bool, string, []byte, time.Time or nil", i+1, v)
			}
			if s, ok := v.(string); ok && !utf8.ValidString(s) {
				return nil, errAt(at, "parameter ?%d: the string is not UTF-8", i+1)
			}
			return v, nil
		}
		return n, nil
	case *sqlparse.ColumnRef:
		i, t, err := c.r.Column(x.Table, x.Name)
		if err != nil {
			return node{}, errAt(x.At, "%v", err)
		}
		n := known(t)
		n.eval = func(e *env) (any, error) { return e.row[i], nil }
		return n, nil
	case *sqlparse.Unary:
		return c.unary(x)
	case *sqlparse.Binary:
		return c.binary(x)
	case *sqlparse.In:
		return c.in(x)
	case *sqlparse.Between:
		return c.between(x)
	case *sqlparse.Call:
		return c.call(x)
	case *sqlparse.Cast:
		return c.cast(x)
	case *sqlparse.Case:
		return c.caseExpr(x)
	case *sqlparse.Subquery:
		return node{}, notYet(x.At, "a subquery", 31)
	case *sqlparse.Exists:
		return node{}, notYet(x.At, "EXISTS", 31)
	}
	return node{}, errAt(e.Pos(), "expression %s is not supported", e)
}

func (c *compiler) unary(x *sqlparse.Unary) (node, error) {
	a, err := c.expr(x.X)
	if err != nil {
		return node{}, err
	}
	if x.Op == "NOT" {
		if err := want(a, "NOT", table.Bool); err != nil {
			return node{}, err
		}
		n := known(table.Bool)
		n.eval = func(e *env) (any, error) {
			v, err := a.eval(e)
			if err != nil || v == nil {
				return nil, err
			}
			if err := check(a, v, "NOT", table.Bool); err != nil {
				return nil, err
			}
			return !v.(bool), nil
		}
		return n, nil
	}
	what := "unary " + x.Op
	if err := want(a, what, numbers...); err != nil {
		return node{}, err
	}
	n := a
	n.eval = func(e *env) (any, error) {
		v, err := a.eval(e)
		if err != nil || v == nil {
			return nil, err
		}
		if err := check(a, v, what, numbers...); err != nil {
			return nil, err
		}
		if x.Op == "+" {
			return v, nil
		}
		switch y := v.(type) {
		case int64:
			if y == math.MinInt64 {
				return nil, errAt(x.At, "integer overflow: -(%d)", y)
			}
			return -y, nil
		case float64:
			// SQLite runs -x as `0 - x`, so -(0.0) is 0.0 and not -0.0.
			// A minus in front of a number literal is part of the literal.
			return 0 - y, nil
		}
		return nil, nil
	}
	return n, nil
}

// logic runs AND and OR in three-valued logic. The right side does not
// run when the left side decides.
func (c *compiler) logic(x *sqlparse.Binary, a, b node) (node, error) {
	for _, s := range []node{a, b} {
		if err := want(s, x.Op, table.Bool); err != nil {
			return node{}, err
		}
	}
	decide := x.Op == "OR" // the value of one side that decides the result
	side := func(s node, e *env) (any, error) {
		v, err := s.eval(e)
		if err != nil || v == nil {
			return nil, err
		}
		return v, check(s, v, x.Op, table.Bool)
	}
	n := known(table.Bool)
	n.eval = func(e *env) (any, error) {
		l, err := side(a, e)
		if err != nil {
			return nil, err
		}
		if l == decide {
			return decide, nil
		}
		r, err := side(b, e)
		if err != nil {
			return nil, err
		}
		switch {
		case r == decide:
			return decide, nil
		case l == nil || r == nil:
			return nil, nil
		}
		return !decide, nil
	}
	return n, nil
}

func (c *compiler) binary(x *sqlparse.Binary) (node, error) {
	a, err := c.expr(x.L)
	if err != nil {
		return node{}, err
	}
	b, err := c.expr(x.R)
	if err != nil {
		return node{}, err
	}
	switch x.Op {
	case "AND", "OR":
		return c.logic(x, a, b)
	case "=", "!=", "<", "<=", ">", ">=":
		if err := comparableNodes(a, b, x.Op); err != nil {
			return node{}, err
		}
		op := x.Op
		n := known(table.Bool)
		n.eval = func(e *env) (any, error) {
			l, r, err := both(a, b, e)
			if err != nil || l == nil || r == nil {
				return nil, err
			}
			if err := comparableValues(a, b, l, r, op); err != nil {
				return nil, err
			}
			d, ok := compare(l, r)
			if !ok {
				return nil, nil
			}
			return holds(op, d), nil
		}
		return n, nil
	case "IS", "IS NOT":
		if err := comparableNodes(a, b, x.Op); err != nil {
			return node{}, err
		}
		not := x.Op == "IS NOT"
		n := known(table.Bool)
		n.eval = func(e *env) (any, error) {
			l, r, err := both(a, b, e)
			if err != nil {
				return nil, err
			}
			eq := l == nil && r == nil
			if l != nil && r != nil {
				if err := comparableValues(a, b, l, r, x.Op); err != nil {
					return nil, err
				}
				d, ok := compare(l, r)
				eq = ok && d == 0
			}
			return eq != not, nil
		}
		return n, nil
	case "LIKE", "NOT LIKE":
		for _, s := range []node{a, b} {
			if err := want(s, x.Op, table.String); err != nil {
				return node{}, err
			}
		}
		not := x.Op == "NOT LIKE"
		n := known(table.Bool)
		n.eval = func(e *env) (any, error) {
			l, r, err := both(a, b, e)
			if err != nil || l == nil || r == nil {
				return nil, err
			}
			if err := check(a, l, x.Op, table.String); err != nil {
				return nil, err
			}
			if err := check(b, r, x.Op, table.String); err != nil {
				return nil, err
			}
			return like(l.(string), r.(string)) != not, nil
		}
		return n, nil
	case "||":
		for _, s := range []node{a, b} {
			if err := want(s, "||", table.String); err != nil {
				return node{}, err
			}
		}
		n := known(table.String)
		n.eval = func(e *env) (any, error) {
			l, r, err := both(a, b, e)
			if err != nil || l == nil || r == nil {
				return nil, err
			}
			if err := check(a, l, "||", table.String); err != nil {
				return nil, err
			}
			if err := check(b, r, "||", table.String); err != nil {
				return nil, err
			}
			return l.(string) + r.(string), nil
		}
		return n, nil
	case "+", "-", "*", "/", "%":
		allowed := numbers
		if x.Op == "%" {
			allowed = []table.Type{table.Int64}
		}
		for _, s := range []node{a, b} {
			if err := want(s, x.Op, allowed...); err != nil {
				return node{}, err
			}
		}
		var n node
		switch {
		case a.known && b.known && a.typ == table.Int64 && b.typ == table.Int64:
			n = known(table.Int64)
		case a.known && a.typ == table.Float64, b.known && b.typ == table.Float64:
			n = known(table.Float64)
		}
		op := x.Op
		n.eval = func(e *env) (any, error) {
			l, r, err := both(a, b, e)
			if err != nil || l == nil || r == nil {
				return nil, err
			}
			if err := check(a, l, op, allowed...); err != nil {
				return nil, err
			}
			if err := check(b, r, op, allowed...); err != nil {
				return nil, err
			}
			return arith(x.At, op, l, r)
		}
		return n, nil
	}
	return node{}, errAt(x.At, "operator %s is not supported", x.Op)
}

func both(a, b node, e *env) (any, any, error) {
	l, err := a.eval(e)
	if err != nil {
		return nil, nil, err
	}
	r, err := b.eval(e)
	return l, r, err
}

func holds(op string, d int) bool {
	switch op {
	case "=":
		return d == 0
	case "!=":
		return d != 0
	case "<":
		return d < 0
	case "<=":
		return d <= 0
	case ">":
		return d > 0
	}
	return d >= 0
}

// arith computes an arithmetic operator on two numbers. Two INTEGER
// values give an INTEGER, and an overflow is an error. Otherwise the
// result is a REAL, and a result that is not finite is an error. A
// division by zero is an error.
func arith(at sqlparse.At, op string, l, r any) (any, error) {
	x, xi := l.(int64)
	y, yi := r.(int64)
	if xi && yi {
		overflow := func() (any, error) {
			return nil, errAt(at, "integer overflow: %d %s %d", x, op, y)
		}
		switch op {
		case "+":
			s := x + y
			if (x > 0 && y > 0 && s < 0) || (x < 0 && y < 0 && s >= 0) {
				return overflow()
			}
			return s, nil
		case "-":
			d := x - y
			if (x >= 0 && y < 0 && d < 0) || (x < 0 && y > 0 && d >= 0) {
				return overflow()
			}
			return d, nil
		case "*":
			if x == 0 || y == 0 {
				return int64(0), nil
			}
			p := x * y
			if p/y != x || (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64) {
				return overflow()
			}
			return p, nil
		case "/", "%":
			if y == 0 {
				return nil, errAt(at, "division by zero: %d %s 0", x, op)
			}
			if op == "%" {
				return x % y, nil
			}
			if x == math.MinInt64 && y == -1 {
				return overflow()
			}
			return x / y, nil
		}
	}
	f, g := float(l), float(r)
	var v float64
	switch op {
	case "+":
		v = f + g
	case "-":
		v = f - g
	case "*":
		v = f * g
	case "/":
		if g == 0 {
			return nil, errAt(at, "division by zero: %s / %s", realText(f), realText(g))
		}
		v = f / g
	}
	if (math.IsInf(v, 0) || math.IsNaN(v)) && !math.IsInf(f, 0) && !math.IsInf(g, 0) && !math.IsNaN(f) && !math.IsNaN(g) {
		return nil, errAt(at, "REAL overflow: %s %s %s", realText(f), op, realText(g))
	}
	return v, nil
}

func float(v any) float64 {
	if i, ok := v.(int64); ok {
		return float64(i)
	}
	return v.(float64)
}

func (c *compiler) exprs(es []sqlparse.Expr) ([]node, error) {
	ns := make([]node, len(es))
	for i, e := range es {
		n, err := c.expr(e)
		if err != nil {
			return nil, err
		}
		ns[i] = n
	}
	return ns, nil
}

// equal runs = in three-valued logic: nil, true or false.
func equal(a, b node, l, r any, what string) (any, error) {
	if l == nil || r == nil {
		return nil, nil
	}
	if err := comparableValues(a, b, l, r, what); err != nil {
		return nil, err
	}
	d, ok := compare(l, r)
	if !ok {
		return nil, nil
	}
	return d == 0, nil
}

func (c *compiler) in(x *sqlparse.In) (node, error) {
	if x.Select != nil {
		return node{}, notYet(x.At, "IN (SELECT ...)", 31)
	}
	a, err := c.expr(x.X)
	if err != nil {
		return node{}, err
	}
	list, err := c.exprs(x.List)
	if err != nil {
		return node{}, err
	}
	for _, b := range list {
		if err := comparableNodes(a, b, "IN"); err != nil {
			return node{}, err
		}
	}
	n := known(table.Bool)
	n.eval = func(e *env) (any, error) {
		v, err := a.eval(e)
		if err != nil {
			return nil, err
		}
		var result any = false
		for _, b := range list {
			w, err := b.eval(e)
			if err != nil {
				return nil, err
			}
			eq, err := equal(a, b, v, w, "IN")
			if err != nil {
				return nil, err
			}
			if eq == true {
				result = true
				break
			}
			if eq == nil {
				result = nil
			}
		}
		if result == nil || !x.Not {
			return result, nil
		}
		return !result.(bool), nil
	}
	return n, nil
}

func (c *compiler) between(x *sqlparse.Between) (node, error) {
	a, err := c.expr(x.X)
	if err != nil {
		return node{}, err
	}
	lo, err := c.expr(x.Lo)
	if err != nil {
		return node{}, err
	}
	hi, err := c.expr(x.Hi)
	if err != nil {
		return node{}, err
	}
	for _, b := range []node{lo, hi} {
		if err := comparableNodes(a, b, "BETWEEN"); err != nil {
			return node{}, err
		}
	}
	// side compares v with a bound: true, false or nil.
	side := func(b node, v, w any, op string) (any, error) {
		if v == nil || w == nil {
			return nil, nil
		}
		if err := comparableValues(a, b, v, w, "BETWEEN"); err != nil {
			return nil, err
		}
		d, ok := compare(v, w)
		if !ok {
			return nil, nil
		}
		return holds(op, d), nil
	}
	n := known(table.Bool)
	n.eval = func(e *env) (any, error) {
		v, err := a.eval(e)
		if err != nil {
			return nil, err
		}
		l, err := lo.eval(e)
		if err != nil {
			return nil, err
		}
		h, err := hi.eval(e)
		if err != nil {
			return nil, err
		}
		ge, err := side(lo, v, l, ">=")
		if err != nil {
			return nil, err
		}
		le, err := side(hi, v, h, "<=")
		if err != nil {
			return nil, err
		}
		var r any
		switch {
		case ge == false || le == false:
			r = false
		case ge == nil || le == nil:
			return nil, nil
		default:
			r = true
		}
		return r != x.Not, nil
	}
	return n, nil
}

func (c *compiler) caseExpr(x *sqlparse.Case) (node, error) {
	var operand node
	var err error
	if x.Operand != nil {
		if operand, err = c.expr(x.Operand); err != nil {
			return node{}, err
		}
	}
	conds := make([]node, len(x.Whens))
	results := make([]node, 0, len(x.Whens)+1)
	for i, w := range x.Whens {
		if conds[i], err = c.expr(w.Cond); err != nil {
			return node{}, err
		}
		if x.Operand != nil {
			if err := comparableNodes(operand, conds[i], "CASE"); err != nil {
				return node{}, err
			}
		} else if err := want(conds[i], "WHEN", table.Bool); err != nil {
			return node{}, err
		}
		r, err := c.expr(w.Result)
		if err != nil {
			return node{}, err
		}
		results = append(results, r)
	}
	var elseNode *node
	if x.Else != nil {
		r, err := c.expr(x.Else)
		if err != nil {
			return node{}, err
		}
		results = append(results, r)
		elseNode = &results[len(results)-1]
	}
	n, err := same(results, "CASE")
	if err != nil {
		return node{}, err
	}
	for i := range results {
		results[i] = conform(results[i], n, "CASE")
	}
	n.eval = func(e *env) (any, error) {
		var v any
		if x.Operand != nil {
			if v, err = operand.eval(e); err != nil {
				return nil, err
			}
		}
		for i, cond := range conds {
			w, err := cond.eval(e)
			if err != nil {
				return nil, err
			}
			var hit any
			if x.Operand != nil {
				if hit, err = equal(operand, cond, v, w, "CASE"); err != nil {
					return nil, err
				}
			} else {
				if err := check(cond, w, "WHEN", table.Bool); err != nil {
					return nil, err
				}
				hit = w
			}
			if hit == true {
				return results[i].eval(e)
			}
		}
		if elseNode != nil {
			return results[len(results)-1].eval(e)
		}
		return nil, nil
	}
	return n, nil
}

// like matches s against a LIKE pattern: % is any run of characters, _
// is one character. Letters A to Z match their lower case, as in SQLite;
// other letters match only themselves.
func like(s, pattern string) bool {
	fold := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}
	sr, pr := []rune(s), []rune(pattern)
	si, pi, starP, starS := 0, 0, -1, 0
	for si < len(sr) {
		switch {
		case pi < len(pr) && pr[pi] == '%':
			starP, starS = pi, si
			pi++
		case pi < len(pr) && (pr[pi] == '_' || fold(pr[pi]) == fold(sr[si])):
			si++
			pi++
		case starP >= 0:
			starS++
			si, pi = starS, starP+1
		default:
			return false
		}
	}
	for pi < len(pr) && pr[pi] == '%' {
		pi++
	}
	return pi == len(pr)
}
