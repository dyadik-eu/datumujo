package sqlexec

import (
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// The plan chooses how a statement reads the rows of one table. It reads
// all of them, or a part of the key or of an index (L-9). A plan only narrows
// the rows. The whole WHERE still runs on every row it reads, so a plan
// can make a statement slow, but never wrong.

// DefaultMaxMemory bounds the bytes of rows that a statement holds in
// memory to sort them, to drop repeated ones, or to change them (L-10).
const DefaultMaxMemory = 64 << 20

// ErrMemory matches a statement that would hold more rows in memory than
// the bound allows. It returns no rows and changes nothing.
var ErrMemory = errors.New("sqlexec: the statement holds too many rows in memory")

// Limits are the bounds of a statement. The zero value takes the
// defaults.
type Limits struct {
	MaxMemory int64 // bytes of rows in memory; 0 is DefaultMaxMemory
	// NoIndex reads every row, whatever the WHERE. The tests compare the
	// results of both ways.
	NoIndex bool
}

func (l Limits) maxMemory() int64 {
	if l.MaxMemory <= 0 {
		return DefaultMaxMemory
	}
	return l.MaxMemory
}

// rowBytes estimates the memory of a row: a fixed part for each value
// and the bytes of text and blobs.
func rowBytes(row []any) int64 {
	n := int64(24 * len(row))
	for _, v := range row {
		switch x := v.(type) {
		case string:
			n += int64(len(x))
		case []byte:
			n += int64(len(x))
		}
	}
	return n
}

// memory counts the bytes a statement holds.
type memory struct {
	used, max int64
	at        sqlparse.At
}

func (m *memory) add(row []any) error {
	m.used += rowBytes(row)
	if m.used > m.max {
		return wrap(m.at, ErrMemory, "the statement holds more than %d bytes of rows in memory; an index for WHERE or ORDER BY, or a LIMIT, would help", m.max)
	}
	return nil
}

// constraint is one term of WHERE that a scan can use: column op value.
type constraint struct {
	col  int
	op   string // =, <, <=, >, >= or IN
	vals []*Expr
	text string // for the plan
}

// constraints returns the terms of WHERE that compare a column of the
// table with a value that reads no column. Only terms joined by AND
// count: each of them must hold for a row to pass.
func constraints(where sqlparse.Expr, s Resolver) []constraint {
	return constraintsFor([]sqlparse.Expr{where}, s, nil, 0, math.MaxInt)
}

// constraintsFor returns the terms of the expressions that compare a
// column of one table of a join with a value. The table has the columns
// lo to hi of the joined row, which full resolves. The value may read
// the tables before it, which prefix resolves, and no other column. A
// scan of the table runs once for each row of those tables. A nil prefix
// allows no column. The column of a constraint counts from 0 in its
// table.
func constraintsFor(exprs []sqlparse.Expr, full, prefix Resolver, lo, hi int) []constraint {
	var out []constraint
	var walk func(e sqlparse.Expr)
	col := func(e sqlparse.Expr) (int, bool) {
		ref, ok := e.(*sqlparse.ColumnRef)
		if !ok {
			return 0, false
		}
		i, _, err := full.Column(ref.Table, ref.Name)
		if err != nil || i < lo || i >= hi {
			return 0, false
		}
		return i - lo, true
	}
	value := func(e sqlparse.Expr) (*Expr, bool) {
		x, err := Compile(e, prefix)
		return x, err == nil
	}
	flip := map[string]string{"=": "=", "<": ">", "<=": ">=", ">": "<", ">=": "<="}
	walk = func(e sqlparse.Expr) {
		switch x := e.(type) {
		case *sqlparse.Binary:
			if x.Op == "AND" {
				walk(x.L)
				walk(x.R)
				return
			}
			if _, ok := flip[x.Op]; !ok {
				return
			}
			if c, ok := col(x.L); ok {
				if v, ok := value(x.R); ok {
					out = append(out, constraint{c, x.Op, []*Expr{v}, x.String()})
				}
			} else if c, ok := col(x.R); ok {
				if v, ok := value(x.L); ok {
					out = append(out, constraint{c, flip[x.Op], []*Expr{v}, x.String()})
				}
			}
		case *sqlparse.Between:
			c, ok := col(x.X)
			lo, okLo := value(x.Lo)
			hi, okHi := value(x.Hi)
			if !x.Not && ok && okLo && okHi {
				out = append(out, constraint{c, ">=", []*Expr{lo}, x.String()}, constraint{c, "<=", []*Expr{hi}, x.String()})
			}
		case *sqlparse.In:
			c, ok := col(x.X)
			// A subquery is no list of values: its List is empty, and an
			// empty IN would read no row.
			if x.Not || !ok || x.Select != nil {
				return
			}
			var vs []*Expr
			for _, e := range x.List {
				v, ok := value(e)
				if !ok {
					return
				}
				vs = append(vs, v)
			}
			out = append(out, constraint{c, "IN", vs, x.String()})
		}
	}
	for _, e := range exprs {
		if e != nil {
			walk(e)
		}
	}
	return out
}

// access is a way to read the rows: a scan of the key or of an index,
// with values for its leading columns.
type access struct {
	index string // "" is the key
	cols  []int  // the columns of the key or index, in order
	eq    []*constraint
	lo    *constraint
	hi    *constraint
	// order is set when the scan gives the rows in the order of ORDER BY,
	// so no sort is needed.
	order   bool
	reverse bool
}

func (a *access) name(t *table.Table) string {
	if a.index == "" {
		return "PRIMARY KEY"
	}
	return "INDEX " + a.index
}

// describe writes the access for Plan.
func (a *access) describe(t *table.Table) string {
	if a == nil {
		return "SCAN " + t.Name
	}
	var terms []string
	for _, c := range a.eq {
		terms = append(terms, c.text)
	}
	for _, c := range []*constraint{a.lo, a.hi} {
		if c != nil && (len(terms) == 0 || terms[len(terms)-1] != c.text) {
			terms = append(terms, c.text)
		}
	}
	verb := "SEARCH"
	if len(terms) == 0 {
		verb = "SCAN"
	}
	s := verb + " " + t.Name + " USING " + a.name(t)
	if len(terms) > 0 {
		s += " (" + strings.Join(terms, " AND ") + ")"
	}
	if a.order {
		s += " IN ORDER"
		if a.reverse {
			s += " DESC"
		}
	}
	return s
}

// candidates returns the key and each index with their columns.
func candidates(t *table.Table) []access {
	out := []access{{index: "", cols: t.Key}}
	for _, ix := range t.Indexes {
		out = append(out, access{index: ix.Name, cols: ix.Columns})
	}
	return out
}

// choose picks the access for the constraints: the most leading columns
// with = or IN, then a range on the next column. The key wins a tie over
// an index. A scan of the key reads each row once, and an index reads
// the entry and then the row.
func choose(t *table.Table, cs []constraint) *access {
	var best *access
	bestScore := 0
	for _, a := range candidates(t) {
		a := a
		ins := 1
		for _, col := range a.cols {
			var eq *constraint
			for i := range cs {
				c := &cs[i]
				if c.col == col && (c.op == "=" || c.op == "IN" && ins*len(c.vals) <= maxCombinations) {
					if eq == nil || eq.op == "IN" && c.op == "=" {
						eq = c
					}
				}
			}
			if eq == nil {
				break
			}
			if eq.op == "IN" {
				ins *= len(eq.vals)
			}
			a.eq = append(a.eq, eq)
		}
		if k := len(a.eq); k < len(a.cols) {
			next := a.cols[k]
			for i := range cs {
				c := &cs[i]
				if c.col != next {
					continue
				}
				switch c.op {
				case ">", ">=":
					if a.lo == nil {
						a.lo = c
					}
				case "<", "<=":
					if a.hi == nil {
						a.hi = c
					}
				}
			}
		}
		score := 4 * len(a.eq)
		if a.lo != nil {
			score++
		}
		if a.hi != nil {
			score++
		}
		if score > bestScore {
			best, bestScore = &a, score
		}
	}
	return best
}

// maxCombinations bounds the scans that IN lists make together.
const maxCombinations = 1000

// orderColumn returns the column of the table that a term of ORDER BY
// names, if it names one. The term names a column directly, or through a
// column of the result that is a column of the table.
func (q *Query) orderColumn(k orderKey, o sqlparse.Order, s Resolver) (int, bool) {
	e := o.Expr
	if k.out >= 0 {
		// A column of *, or an item of the list.
		c := q.cols[k.out]
		if c.expr == nil {
			return c.row, true
		}
		e = q.itemExpr(k.out)
	}
	ref, ok := e.(*sqlparse.ColumnRef)
	if !ok {
		return 0, false
	}
	i, _, err := s.Column(ref.Table, ref.Name)
	return i, err == nil
}

// itemExpr returns the expression of the n-th column of the result.
func (q *Query) itemExpr(n int) sqlparse.Expr {
	out := 0
	for _, it := range q.st.Items {
		if it.Star {
			out += q.starWidth(it)
			continue
		}
		if out == n {
			return it.Expr
		}
		out++
	}
	return nil
}

// useOrder checks whether a scan gives the rows in the order of ORDER BY.
// The scan orders by the columns after its = values, then by the key.
// The terms must follow that order in one direction. A scan backwards
// gives equal values in the other order of the key, so DESC needs terms
// that leave no two rows equal. IN makes several scans, whose rows only
// come in order within each scan.
func useOrder(t *table.Table, a *access, terms []int, desc []bool) bool {
	for _, c := range a.eq {
		if c.op == "IN" && len(c.vals) > 1 {
			return false
		}
	}
	seq := append([]int(nil), a.cols[len(a.eq):]...)
	if a.index != "" {
		seq = append(seq, t.Key...)
	}
	if len(terms) == 0 || len(terms) > len(seq) {
		return false
	}
	for i, col := range terms {
		if seq[i] != col || desc[i] != desc[0] {
			return false
		}
	}
	if !desc[0] {
		return true
	}
	// DESC: the terms must cover the key, so that no two rows are equal.
	covered := map[int]bool{}
	for _, col := range terms {
		covered[col] = true
	}
	for _, k := range t.Key {
		if !covered[k] {
			for _, c := range a.eq {
				if c.col == k && c.op == "=" {
					covered[k] = true
				}
			}
		}
		if !covered[k] {
			return false
		}
	}
	return true
}

// plan chooses the access of a query. It prefers a scan that narrows
// the rows, and uses its order when it fits ORDER BY. Without such a
// scan, it scans the key or an index in the order of ORDER BY, so a
// query with LIMIT can stop early.
func (q *Query) plan(s Resolver, lim Limits) {
	if q.t == nil || lim.NoIndex {
		return
	}
	// Each table of a join gets its own access. Its bounds come from ON
	// and WHERE, with values that read only the tables before it.
	for i, src := range q.srcs {
		exprs := []sqlparse.Expr{q.st.Where}
		if src.onSt != nil {
			exprs = append(exprs, src.onSt)
		}
		cs := constraintsFor(exprs, s, scope{q.srcs[:i]}, src.offset, src.offset+len(src.t.Columns))
		src.access = choose(src.t, cs)
	}
	if len(q.srcs) > 1 {
		return // the order of a scan fits ORDER BY for one table only
	}
	// The access for the order below is the one the scan reads.
	defer func() { q.srcs[0].access = q.access }()
	a := q.srcs[0].access
	if q.distinct || q.group != nil || len(q.order) == 0 {
		q.access = a
		return
	}
	var terms []int
	var desc []bool
	for i, k := range q.order {
		col, ok := q.orderColumn(k, q.st.OrderBy[i], s)
		if !ok {
			q.access = a
			return
		}
		terms, desc = append(terms, col), append(desc, k.desc)
	}
	if a != nil {
		if useOrder(q.t, a, terms, desc) {
			a.order, a.reverse = true, desc[0]
		}
		q.access = a
		return
	}
	for _, c := range candidates(q.t) {
		c := c
		if useOrder(q.t, &c, terms, desc) {
			c.order, c.reverse = true, desc[0]
			q.access = &c
			return
		}
	}
}

// fit converts a value for a column of the given type, for a bound of a
// scan. It reports false when the value cannot be a bound: NULL, NaN, a
// type that does not compare, or a number that the column type does not
// hold exactly. A bound that is not used only makes the scan wider.
func fit(v any, typ table.Type) (any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case int64:
		switch typ {
		case table.Int64:
			return x, true
		case table.Float64:
			f := float64(x)
			if f < 0x1p63 && int64(f) == x {
				return f, true
			}
		}
		return nil, false
	case float64:
		if math.IsNaN(x) {
			return nil, false
		}
		switch typ {
		case table.Float64:
			return x, true
		case table.Int64:
			if x == math.Trunc(x) && x >= -0x1p63 && x < 0x1p63 {
				return int64(x), true
			}
		}
		return nil, false
	}
	if t, ok := typeOf(v); ok && t == typ {
		return v, true
	}
	return nil, false
}

// scans turns an access into the options of the scans, in the order of
// the key or index. The values may read outer, the row of the tables
// before this one in a join. The params are the values of the run.
func (a *access) scans(t *table.Table, outer, params []any) ([]table.Options, error) {
	// A value of a type that does not compare with its column is an
	// error of WHERE. The scan reads the whole key or index then, in its
	// order, so the error comes as it would without the plan.
	full := []table.Options{{Index: a.index, Reverse: a.reverse}}
	// Each column with = or IN gives a list of values; the scans are all
	// combinations.
	combos := [][]any{{}}
	for i, c := range a.eq {
		typ := t.Columns[a.cols[i]].Type
		var vals []any
		for _, x := range c.vals {
			v, err := x.Eval(outer, params)
			if err != nil {
				return nil, err
			}
			if !boundable(v, typ) {
				return full, nil
			}
			// NULL, NaN, or a number the column cannot hold, such as 2.5
			// for INTEGER, equals no value: it adds no scan.
			if fv, ok := fit(v, typ); ok {
				vals = append(vals, fv)
			}
		}
		vals = sortedDistinct(vals)
		var next [][]any
		for _, cmb := range combos {
			for _, v := range vals {
				next = append(next, append(append([]any(nil), cmb...), v))
			}
		}
		combos = next
	}
	// The range on the next column is the same for each combination.
	type rangeBound struct {
		op string
		v  any
	}
	var bounds []rangeBound
	if k := len(a.eq); k < len(a.cols) {
		typ := t.Columns[a.cols[k]].Type
		for _, c := range []*constraint{a.lo, a.hi} {
			if c == nil {
				continue
			}
			v, err := c.vals[0].Eval(outer, params)
			if err != nil {
				return nil, err
			}
			if !boundable(v, typ) {
				return full, nil
			}
			if v == nil || isNaN(v) {
				return nil, nil // a comparison with NULL or NaN holds for no row
			}
			op, fv, ok := rangeFit(c.op, v, typ)
			if ok {
				bounds = append(bounds, rangeBound{op, fv})
			}
		}
	}
	var opts []table.Options
	for _, cmb := range combos {
		o := table.Options{Index: a.index, Prefix: cmb, Reverse: a.reverse}
		for _, rb := range bounds {
			b := append(append([]any(nil), cmb...), rb.v)
			switch rb.op {
			case ">":
				o.From, o.FromExclusive = b, true
			case ">=":
				o.From = b
			case "<":
				o.To = b
			case "<=":
				o.To, o.ToInclusive = b, true
			}
		}
		opts = append(opts, o)
	}
	// A scan backwards has one combination: useOrder refuses IN with more
	// than one value. So the order of opts needs no turn.
	return opts, nil
}

// rangeFit converts the value of a range for a column of type typ. A
// REAL that is not a whole number bounds an INTEGER column by the next
// whole number inside the range. So id <= 4.5 is id <= 4, and id > 3.5
// is id >= 4. It reports false when no exact bound exists; the scan is then
// wider, and WHERE decides.
func rangeFit(op string, v any, typ table.Type) (string, any, bool) {
	if fv, ok := fit(v, typ); ok {
		return op, fv, true
	}
	f, isFloat := v.(float64)
	if !isFloat || typ != table.Int64 || math.IsInf(f, 0) {
		return "", nil, false
	}
	switch op {
	case ">", ">=":
		if c := math.Ceil(f); c >= -0x1p63 && c < 0x1p63 {
			return ">=", int64(c), true
		}
	case "<", "<=":
		if c := math.Floor(f); c >= -0x1p63 && c < 0x1p63 {
			return "<=", int64(c), true
		}
	}
	return "", nil, false
}

// boundable reports that a value compares with a column of type typ, or
// is NULL. Else WHERE fails on it.
func boundable(v any, typ table.Type) bool {
	t, ok := typeOf(v)
	return v == nil || ok && comparable(t, typ)
}

func isNaN(v any) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
}

// sortedDistinct sorts values of one type and drops repeated ones.
func sortedDistinct(vals []any) []any {
	sort.SliceStable(vals, func(i, j int) bool {
		d, _ := compare(vals[i], vals[j])
		return d < 0
	})
	var out []any
	for i, v := range vals {
		if i > 0 {
			if d, _ := compare(vals[i-1], v); d == 0 {
				continue
			}
		}
		out = append(out, v)
	}
	return out
}

// Plan describes how the query reads its rows, for tests and for a
// person: SCAN, or SEARCH with the terms of WHERE that bound the scan,
// then SORT if the rows need one.
func (q *Query) Plan() string {
	var parts []string
	switch {
	case q.t == nil:
		parts = append(parts, "ONE ROW")
	case len(q.srcs) == 1:
		parts = append(parts, q.access.describe(q.t))
	default:
		for _, src := range q.srcs {
			d := src.access.describe(src.t)
			if src.name != src.t.Name {
				d += " AS " + src.name
			}
			if src.left {
				d = "LEFT " + d
			}
			parts = append(parts, d)
		}
	}
	if q.group != nil {
		parts = append(parts, "GROUP")
	}
	if q.distinct {
		parts = append(parts, "DISTINCT")
	}
	if len(q.order) > 0 && !q.sorted() {
		parts = append(parts, "SORT")
	}
	return strings.Join(parts, "; ")
}

// sorted reports that the scan gives the rows in the order of ORDER BY.
func (q *Query) sorted() bool { return q.access != nil && q.access.order }
