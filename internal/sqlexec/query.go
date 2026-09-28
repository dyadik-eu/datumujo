package sqlexec

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// Source is what a query reads: a view of one commit, or a write
// transaction, which sees its own changes.
type Source interface {
	Schema() *table.Schema
	Scan(table string, o table.Options) (*table.Rows, error)
}

// Column describes a column of a query result.
type Column struct {
	Name  string
	Type  table.Type
	Known bool // Type is known before the run
}

// Query is a compiled SELECT.
type Query struct {
	st       *sqlparse.Select
	t        *table.Table // nil without FROM
	cols     []outCol
	names    []Column
	where    *Expr
	order    []orderKey
	distinct bool
	limit    *Expr
	offset   *Expr
	access   *access // nil: read every row
	lim      Limits
	group    *groupPlan // set for GROUP BY, HAVING or an aggregate
	having   *Expr
}

// outCol computes one column of the result from a row of the table:
// either a column of the row, for *, or an expression.
type outCol struct {
	row  int
	expr *Expr
}

// orderKey is one term of ORDER BY: a column of the result, or an
// expression over the row of the table.
type orderKey struct {
	out  int
	expr *Expr
	desc bool
}

// scope resolves names in a query over one table. With FROM t AS x, the
// name of the table is x and not t, as in SQLite.
type scope struct {
	t    *table.Table
	name string
}

func (s scope) Column(tbl, name string) (int, table.Type, error) {
	if s.t == nil {
		return noColumns{}.Column(tbl, name)
	}
	if tbl != "" && tbl != s.name {
		return 0, 0, fmt.Errorf("no such column: %s.%s", tbl, name)
	}
	return tableScope{s.t}.Column("", name)
}

// notYet refuses a part of SELECT that a later step of the roadmap adds.
func notYet(at sqlparse.At, what string, step int) error {
	return wrap(at, ErrStatement, "%s is not supported yet (roadmap step %d)", what, step)
}

// Prepare compiles a SELECT against a schema and chooses its plan.
func Prepare(sc *table.Schema, st *sqlparse.Select, lim Limits) (*Query, error) {
	if len(st.Joins) > 0 {
		return nil, notYet(st.Joins[0].At, "JOIN", 22)
	}
	q := &Query{st: st, distinct: st.Distinct, lim: lim}
	var s scope
	if st.From != nil {
		t, ok := sc.Table(st.From.Name)
		if !ok {
			return nil, wrap(st.From.At, table.ErrNoTable, "no such table: %s", st.From.Name)
		}
		q.t, s = t, scope{t: t, name: t.Name}
		if st.From.Alias != "" {
			s.name = st.From.Alias
		}
	}
	grouped := len(st.GroupBy) > 0 || st.Having != nil
	for _, it := range st.Items {
		grouped = grouped || !it.Star && hasAggregate(it.Expr)
	}
	for _, o := range st.OrderBy {
		grouped = grouped || hasAggregate(o.Expr)
	}
	if grouped {
		if err := q.prepareGroups(st, s); err != nil {
			return nil, err
		}
	}
	for _, it := range st.Items {
		if grouped {
			break
		}
		if it.Star {
			if q.t == nil {
				return nil, errAt(it.At, "* needs a table in FROM")
			}
			if it.Table != "" && it.Table != s.name {
				return nil, errAt(it.At, "no such table: %s", it.Table)
			}
			for _, i := range visible(q.t) {
				c := q.t.Columns[i]
				q.cols = append(q.cols, outCol{row: i})
				q.names = append(q.names, Column{Name: c.Name, Type: c.Type, Known: true})
			}
			continue
		}
		x, err := Compile(it.Expr, s)
		if err != nil {
			return nil, err
		}
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
	var err error
	if st.Where != nil {
		if q.where, err = Compile(st.Where, s); err != nil {
			return nil, err
		}
		if typ, ok := q.where.Type(); ok && typ != table.Bool {
			return nil, errAt(st.Where.Pos(), "WHERE needs BOOLEAN, and %s is %s", st.Where, TypeName(typ))
		}
	}
	for _, o := range st.OrderBy {
		if grouped {
			break
		}
		k, err := q.orderKey(o, s)
		if err != nil {
			return nil, err
		}
		q.order = append(q.order, k)
	}
	if st.Limit != nil {
		if q.limit, err = bound(st.Limit, "LIMIT"); err != nil {
			return nil, err
		}
	}
	if st.Offset != nil {
		if q.offset, err = bound(st.Offset, "OFFSET"); err != nil {
			return nil, err
		}
	}
	q.plan(s, lim)
	return q, nil
}

// orderKey resolves one term of ORDER BY as SQLite does. A number is a
// column of the result. A name that is an alias in the result names that
// column. Anything else is an expression over the row of the table.
func (q *Query) orderKey(o sqlparse.Order, s scope) (orderKey, error) {
	k := orderKey{out: -1, desc: o.Desc}
	if l, ok := o.Expr.(*sqlparse.Literal); ok {
		if n, ok := l.Value.(int64); ok {
			if n < 1 || n > int64(len(q.cols)) {
				return k, errAt(l.At, "ORDER BY %d: the result has %d columns", n, len(q.cols))
			}
			k.out = int(n - 1)
			return k, nil
		}
	}
	if ref, ok := o.Expr.(*sqlparse.ColumnRef); ok && ref.Table == "" {
		out := 0
		for _, it := range q.st.Items {
			if it.Star {
				out += len(visible(q.t))
				continue
			}
			if it.Alias == ref.Name {
				k.out = out
				return k, nil
			}
			out++
		}
	}
	x, err := Compile(o.Expr, s)
	if err != nil {
		return k, err
	}
	k.expr = x
	return k, nil
}

// bound compiles LIMIT or OFFSET. It reads no column.
func bound(e sqlparse.Expr, what string) (*Expr, error) {
	x, err := Compile(e, nil)
	if err != nil {
		return nil, err
	}
	if typ, ok := x.Type(); ok && typ != table.Int64 {
		return nil, errAt(e.Pos(), "%s needs INTEGER, and %s is %s", what, e, TypeName(typ))
	}
	return x, nil
}

// Columns returns the columns of the result.
func (q *Query) Columns() []Column { return q.names }

// Rows walks the result of a query.
type Rows struct {
	q       *Query
	next    func() ([]any, error) // nil at the end
	row     []any
	err     error
	done    bool
	scanned *int64
}

// Scanned returns the number of rows of the table that the query has
// read so far. The tests use it to show what a plan saves.
func (r *Rows) Scanned() int64 { return *r.scanned }

// Run runs the query on src. A query without ORDER BY and DISTINCT
// streams: it reads the next row of the table when Next asks for it. The
// others read all rows first, then sort them or drop the repeated ones.
func (q *Query) Run(src Source, params []any) (*Rows, error) {
	limit, offset := int64(-1), int64(0)
	var err error
	if q.limit != nil {
		if limit, err = boundValue(q.limit, q.st.Limit, params); err != nil {
			return nil, err
		}
	}
	if q.offset != nil {
		if offset, err = boundValue(q.offset, q.st.Offset, params); err != nil {
			return nil, err
		}
	}
	scanned := new(int64)
	in, err := q.source(src, params, scanned)
	if err != nil {
		return nil, err
	}
	var next func() ([]any, error)
	if (len(q.order) == 0 || q.sorted()) && !q.distinct {
		next = func() ([]any, error) {
			r, err := in()
			if err != nil || r == nil {
				return nil, err
			}
			return r.out, nil
		}
	} else {
		all, err := q.collect(in)
		if err != nil {
			return nil, err
		}
		i := 0
		next = func() ([]any, error) {
			if i == len(all) {
				return nil, nil
			}
			i++
			return all[i-1].out, nil
		}
	}
	// OFFSET and LIMIT count rows of the result.
	skipped, given := int64(0), int64(0)
	inner := next
	next = func() ([]any, error) {
		for skipped < offset {
			r, err := inner()
			if err != nil || r == nil {
				return nil, err
			}
			skipped++
		}
		if limit >= 0 && given >= limit {
			return nil, nil
		}
		r, err := inner()
		if r != nil {
			given++
		}
		return r, err
	}
	return &Rows{q: q, next: next, scanned: scanned}, nil
}

// boundValue runs LIMIT or OFFSET. As in SQLite, a negative LIMIT is no
// limit, and a negative OFFSET skips no row: Run checks limit >= 0 and
// skips while fewer than offset rows are skipped.
func boundValue(x *Expr, e sqlparse.Expr, params []any) (int64, error) {
	v, err := x.Eval(nil, params)
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, errAt(e.Pos(), "%s is %s; LIMIT and OFFSET need an INTEGER", e, Text(v))
	}
	return n, nil
}

// srcRow is a row that passed WHERE: the columns of the result and the
// keys of ORDER BY.
type srcRow struct {
	out  []any
	keys []any
}

// source returns a function that gives the next row that passes WHERE,
// or nil at the end.
func (q *Query) source(src Source, params []any, scanned *int64) (func() (*srcRow, error), error) {
	var read func() ([]any, bool, error)
	if q.t == nil {
		once := false
		read = func() ([]any, bool, error) {
			if once {
				return nil, false, nil
			}
			once = true
			return nil, true, nil
		}
	} else {
		var err error
		if read, err = readRows(src, q.t, q.access, params, q.st.At, scanned); err != nil {
			return nil, err
		}
	}
	if q.group != nil {
		return q.groupSource(read, params)
	}
	return func() (*srcRow, error) {
		for {
			row, ok, err := read()
			if err != nil || !ok {
				return nil, err
			}
			keep, err := keeps(q.where, q.st.Where, row, params)
			if err != nil {
				return nil, err
			}
			if !keep {
				continue
			}
			r := &srcRow{out: make([]any, len(q.cols))}
			for i, c := range q.cols {
				if c.expr == nil {
					r.out[i] = row[c.row]
				} else if r.out[i], err = c.expr.Eval(row, params); err != nil {
					return nil, err
				}
			}
			for _, k := range q.order {
				var v any
				if k.expr == nil {
					v = r.out[k.out]
				} else if v, err = k.expr.Eval(row, params); err != nil {
					return nil, err
				}
				r.keys = append(r.keys, v)
			}
			return r, nil
		}
	}, nil
}

// collect reads all rows, drops the repeated ones for DISTINCT and sorts
// them for ORDER BY. The sort is stable, so equal keys keep the order of
// the table.
func (q *Query) collect(in func() (*srcRow, error)) ([]*srcRow, error) {
	var all []*srcRow
	seen := map[string]bool{}
	mem := memory{max: q.lim.maxMemory(), at: q.st.At}
	for {
		r, err := in()
		if err != nil {
			return nil, err
		}
		if r == nil {
			break
		}
		if err := mem.add(r.out); err != nil {
			return nil, err
		}
		if err := mem.add(r.keys); err != nil {
			return nil, err
		}
		if q.distinct {
			k := distinctKey(r.out)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		all = append(all, r)
	}
	var sortErr error
	sort.SliceStable(all, func(i, j int) bool {
		for n, k := range q.order {
			d, err := orderCompare(all[i].keys[n], all[j].keys[n])
			if err != nil && sortErr == nil {
				sortErr = err
			}
			if d != 0 {
				if k.desc {
					return d > 0
				}
				return d < 0
			}
		}
		return false
	})
	if sortErr != nil {
		return nil, wrap(q.st.At, ErrStatement, "ORDER BY: %v", sortErr)
	}
	return all, nil
}

// orderCompare orders two values of ORDER BY. NULL comes first, then
// NaN, then the other values. SQLite stores NaN as NULL; here a REAL
// column can hold it.
func orderCompare(a, b any) (int, error) {
	rank := func(v any) int {
		if v == nil {
			return 0
		}
		if f, ok := v.(float64); ok && math.IsNaN(f) {
			return 1
		}
		return 2
	}
	if ra, rb := rank(a), rank(b); ra != 2 || rb != 2 {
		return cmp3(ra < rb, ra > rb), nil
	}
	ta, _ := typeOf(a)
	tb, _ := typeOf(b)
	if !comparable(ta, tb) {
		return 0, fmt.Errorf("%s and %s do not compare", TypeName(ta), TypeName(tb))
	}
	d, _ := compare(a, b)
	return d, nil
}

// distinctKey is the same for two rows exactly when DISTINCT counts them
// as one. NULL equals NULL. A REAL with an integer value equals that
// INTEGER, as in SQLite, and -0.0 equals 0.0.
func distinctKey(row []any) string {
	var b strings.Builder
	for _, v := range row {
		switch x := v.(type) {
		case nil:
			b.WriteString("n;")
		case float64:
			if x == math.Trunc(x) && x >= -0x1p63 && x < 0x1p63 {
				fmt.Fprintf(&b, "i%d;", int64(x))
			} else {
				fmt.Fprintf(&b, "f%x;", math.Float64bits(x))
			}
		case int64:
			fmt.Fprintf(&b, "i%d;", x)
		case bool:
			fmt.Fprintf(&b, "t%t;", x)
		case time.Time:
			fmt.Fprintf(&b, "d%s;", x.UTC().Format(time.RFC3339Nano))
		case string:
			fmt.Fprintf(&b, "s%d:%s;", len(x), x)
		case []byte:
			fmt.Fprintf(&b, "b%d:%s;", len(x), x)
		default:
			fmt.Fprintf(&b, "%T:%v;", v, v)
		}
	}
	return b.String()
}

// Next moves to the next row. It returns false at the end or on an
// error; Err tells which.
func (r *Rows) Next() bool {
	if r.done {
		return false
	}
	r.row, r.err = r.next()
	if r.err != nil || r.row == nil {
		r.done, r.row = true, nil
		return false
	}
	return true
}

// Row returns the current row. The slice belongs to the caller.
func (r *Rows) Row() []any { return append([]any(nil), r.row...) }

// Err returns the error that ended the rows, if one did.
func (r *Rows) Err() error { return r.err }

// Columns returns the columns of the result.
func (r *Rows) Columns() []Column { return r.q.names }

// readRows returns a function that reads the rows of an access, one row
// at a time over all its scans. A nil access reads every row by the key.
func readRows(src Source, t *table.Table, a *access, params []any, at sqlparse.At, scanned *int64) (func() ([]any, bool, error), error) {
	opts := []table.Options{{}}
	if a != nil {
		var err error
		if opts, err = a.scans(t, params); err != nil {
			return nil, err
		}
	}
	var rows *table.Rows
	return func() ([]any, bool, error) {
		for {
			if rows == nil {
				if len(opts) == 0 {
					return nil, false, nil
				}
				var err error
				if rows, err = src.Scan(t.Name, opts[0]); err != nil {
					return nil, false, engineErr(at, err)
				}
				opts = opts[1:]
			}
			if rows.Next() {
				*scanned++
				return rows.Row(), true, nil
			}
			if err := rows.Err(); err != nil {
				return nil, false, engineErr(at, err)
			}
			rows = nil
		}
	}, nil
}

// groupSource computes the groups, then gives one row for each group
// that HAVING keeps.
func (q *Query) groupSource(read func() ([]any, bool, error), params []any) (func() (*srcRow, error), error) {
	rows, err := q.groups(read, params)
	if err != nil {
		return nil, err
	}
	return func() (*srcRow, error) {
		for len(rows) > 0 {
			grow := rows[0]
			rows = rows[1:]
			keep, err := keeps(q.having, q.st.Having, grow, params)
			if err != nil {
				return nil, err
			}
			if !keep {
				continue
			}
			r := &srcRow{out: make([]any, len(q.cols))}
			for i, c := range q.cols {
				if r.out[i], err = c.expr.Eval(grow, params); err != nil {
					return nil, err
				}
			}
			for _, k := range q.order {
				var v any
				if k.expr == nil {
					v = r.out[k.out]
				} else if v, err = k.expr.Eval(grow, params); err != nil {
					return nil, err
				}
				r.keys = append(r.keys, v)
			}
			return r, nil
		}
		return nil, nil
	}, nil
}
