package sqlexec

import (
	"fmt"
	"time"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// The DEFAULT of a column (L-13). The schema keeps it as the text that
// sqlparse.Default.String writes, and an INSERT reads it back with
// sqlparse.ParseDefault.

// defaultOf checks the DEFAULT of column col, which is not in the table
// yet, and returns the text for the schema. The text is "" for no
// default: DEFAULT NULL on a column that allows NULL is the same as none.
// auto is true for the column that takes the next key.
func defaultOf(t *table.Table, c sqlparse.ColumnDef, col table.Column, auto bool) (string, error) {
	d := c.Default
	switch {
	case d == nil:
		return "", nil
	case auto:
		return "", errAt(d.At, "column %s takes the next key when an INSERT gives it no value; a DEFAULT would never apply", c.Name)
	case !d.Now && d.Value == nil:
		if !col.Null {
			return "", errAt(d.At, "column %s is NOT NULL, and its default is NULL", c.Name)
		}
		return "", nil
	}
	// The time does not matter here: only the type is checked.
	if _, err := fitDefault(t, col, d, time.Time{}); err != nil {
		return "", err
	}
	return d.String(), nil
}

// fitDefault returns the value of default d for column col, fitted to
// the column as an INSERT fits a value. now is the time of the
// statement, for CURRENT_TIMESTAMP.
func fitDefault(t *table.Table, col table.Column, d *sqlparse.Default, now time.Time) (any, error) {
	if d.Now {
		if col.Type != table.Time {
			return nil, errAt(d.At, "column %s is %s, and CURRENT_TIMESTAMP is TIMESTAMP", col.Name, TypeName(col.Type))
		}
		return now, nil
	}
	// A table with the column alone, so that newTarget checks the type
	// and value fits the value, as for an INSERT.
	one := &table.Table{Name: t.Name, Columns: []table.Column{col}}
	tg, err := newTarget(one, 0, &sqlparse.Literal{At: d.At, Value: d.Value}, nil, nil)
	if err != nil {
		return nil, err
	}
	return tg.value(one, nil, nil)
}

// defaults returns the value of the default of each column of t that the
// INSERT does not name, nil where a column has none. given marks the
// columns the INSERT names. The value of CURRENT_TIMESTAMP is read once,
// so each row of the statement gets the same.
func defaults(t *table.Table, given map[int]bool, at sqlparse.At) ([]any, error) {
	out := make([]any, len(t.Columns))
	var now time.Time
	for i, c := range t.Columns {
		if given[i] || c.Default == "" {
			continue
		}
		d, err := sqlparse.ParseDefault(c.Default)
		if err != nil {
			return nil, fmt.Errorf("%w: table %s, the default of column %s: %v", table.ErrDamaged, t.Name, c.Name, err)
		}
		if d.Now && now.IsZero() {
			now = time.Now().UTC()
		}
		d.At = at
		if out[i], err = fitDefault(t, c, d, now); err != nil {
			return nil, err
		}
	}
	return out, nil
}
