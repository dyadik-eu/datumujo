package sqlexec

import (
	"errors"
	"fmt"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// The CHECK conditions of a table (L-14). The schema keeps each as the
// text that sqlparse.Expr.String writes. A statement that writes rows
// reads them back with sqlparse.ParseExpr.

// ErrCheck matches a row for which a CHECK of its table is FALSE.
var ErrCheck = errors.New("sqlexec: a CHECK of the table is false")

// compileCheck checks the condition e of a CHECK against the columns of
// t. It must be BOOLEAN, and it must give the same answer for the same
// row each time: no parameter and no aggregate.
func compileCheck(t *table.Table, e sqlparse.Expr) (*Expr, error) {
	var param, agg sqlparse.Expr
	rewrite(e, func(x sqlparse.Expr) (sqlparse.Expr, bool) {
		if _, ok := x.(*sqlparse.Param); ok && param == nil {
			param = x
		}
		if c, ok := x.(*sqlparse.Call); ok && isAggregate(c) && agg == nil {
			agg = x
		}
		return nil, false
	})
	if param != nil {
		return nil, errAt(param.Pos(), "a CHECK takes no parameter")
	}
	if agg != nil {
		return nil, errAt(agg.Pos(), "a CHECK takes no aggregate")
	}
	x, err := Compile(e, tableScope{t})
	if err != nil {
		return nil, err
	}
	if typ, ok := x.Type(); !ok || typ != table.Bool {
		what := "has no type"
		if ok {
			what = "is " + TypeName(typ)
		}
		return nil, errAt(e.Pos(), "CHECK needs BOOLEAN, and %s %s", e, what)
	}
	return x, nil
}

// rowCheck is a compiled CHECK with its text.
type rowCheck struct {
	x    *Expr
	text string
}

// checksOf compiles the checks in texts, from the schema of t.
func checksOf(t *table.Table, texts []string) ([]rowCheck, error) {
	var out []rowCheck
	for _, text := range texts {
		e, err := sqlparse.ParseExpr(text)
		if err == nil {
			var x *Expr
			if x, err = compileCheck(t, e); err == nil {
				out = append(out, rowCheck{x, text})
				continue
			}
		}
		return nil, fmt.Errorf("%w: table %s, CHECK (%s): %v", table.ErrDamaged, t.Name, text, err)
	}
	return out, nil
}

// checkRow fails if a check is FALSE for row. NULL passes, as in SQL. An
// error of a check, such as a division by zero, is an error of the
// statement at at.
func checkRow(t *table.Table, cs []rowCheck, row []any, at sqlparse.At) error {
	for _, c := range cs {
		v, err := c.x.Eval(row, nil)
		if err != nil {
			var e *sqlparse.Error
			if errors.As(err, &e) {
				return wrap(at, e.Err, "CHECK (%s) of table %s: %s", c.text, t.Name, e.Msg)
			}
			return err
		}
		if v == false {
			return wrap(at, ErrCheck, "CHECK (%s) of table %s is false", c.text, t.Name)
		}
	}
	return nil
}
