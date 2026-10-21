package sqlexec

import (
	"fmt"
	"strings"

	"github.com/dyadik-eu/datumujo/internal/sqlparse"
	"github.com/dyadik-eu/datumujo/internal/table"
)

// INSERT ... ON CONFLICT (L-17). Before a row is written, the INSERT
// looks for a row that it would conflict with. That is a row with the
// same primary key, or with the same values in a unique index.
//
// Without a target, every key of the table counts; with one, only the
// key of exactly those columns. A conflict on another key fails, as in
// SQLite. A NULL in a unique index conflicts with no row, so it is never
// looked up.

// conflictKey is the primary key (index "") or a unique index.
type conflictKey struct {
	index string
	cols  []int
}

// upsert is a compiled ON CONFLICT.
type upsert struct {
	keys    []conflictKey
	nothing bool
	sets    []target      // DO UPDATE SET, over the row there is and the row excluded
	where   *Expr         // DO UPDATE ... WHERE; nil for none
	whereSt sqlparse.Expr // the same, as written
}

// upsertScope resolves the names of DO UPDATE. A column of the table,
// by its name or as t.c, is the row that is there. excluded.c is the row
// that the INSERT would have written; it follows the row that is there.
type upsertScope struct{ t *table.Table }

func (s upsertScope) Column(tbl, name string) (int, table.Type, error) {
	if tbl != "excluded" {
		return tableScope{s.t}.Column(tbl, name)
	}
	i, typ, err := tableScope{s.t}.Column("", name)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: excluded.%s", errNoColumn, name)
	}
	return i + len(s.t.Columns), typ, nil
}

// newUpsert checks ON CONFLICT against the keys of t. A target must name
// the columns of the primary key or of a unique index, in any order.
func newUpsert(t *table.Table, u *sqlparse.Upsert) (*upsert, error) {
	all := []conflictKey{{index: "", cols: t.Key}}
	for _, ix := range t.Indexes {
		if ix.Unique {
			all = append(all, conflictKey{index: ix.Name, cols: ix.Columns})
		}
	}
	up := &upsert{nothing: u.Nothing}
	if err := up.compile(t, u); err != nil {
		return nil, err
	}
	if len(u.Target) == 0 {
		up.keys = all
		return up, nil
	}
	want := map[int]bool{}
	for _, name := range u.Target {
		i, _, err := tableScope{t}.Column("", name)
		if err != nil {
			return nil, errAt(u.At, "ON CONFLICT: table %s: %v", t.Name, err)
		}
		if want[i] {
			return nil, errAt(u.At, "ON CONFLICT: column %s twice", name)
		}
		want[i] = true
	}
	for _, k := range all {
		if len(k.cols) != len(want) {
			continue
		}
		same := true
		for _, c := range k.cols {
			same = same && want[c]
		}
		if same {
			up.keys = []conflictKey{k}
			return up, nil
		}
	}
	return nil, errAt(u.At, "ON CONFLICT (%s): no PRIMARY KEY or UNIQUE of table %s has these columns", strings.Join(u.Target, ", "), t.Name)
}

// conflict returns a row of t that row would conflict with on a key of
// the upsert, or nil.
func (up *upsert) conflict(tx *table.Tx, t *table.Table, row []any) ([]any, error) {
	for _, k := range up.keys {
		vals := make([]any, len(k.cols))
		for i, c := range k.cols {
			if row[c] == nil {
				vals = nil
				break
			}
			vals[i] = row[c]
		}
		if vals == nil {
			continue
		}
		if k.index == "" {
			old, ok, err := tx.Get(t.Name, vals...)
			if err != nil || ok {
				return old, err
			}
			continue
		}
		rows, err := tx.Scan(t.Name, table.Options{Index: k.index, Prefix: vals})
		if err != nil {
			return nil, err
		}
		if rows.Next() {
			return rows.Row(), nil
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// compile checks DO UPDATE SET and WHERE. A subquery has no place there:
// it would run while the INSERT writes, and see the rows written before.
func (up *upsert) compile(t *table.Table, u *sqlparse.Upsert) error {
	scope := upsertScope{t}
	seen := map[int]bool{}
	for _, a := range u.Set {
		col, _, err := tableScope{t}.Column("", a.Column)
		if err != nil {
			return errAt(a.At, "table %s: %v", t.Name, err)
		}
		if seen[col] {
			return errAt(a.At, "column %s set twice", a.Column)
		}
		seen[col] = true
		tg, err := newTarget(t, col, a.Value, scope, nil)
		if err != nil {
			return err
		}
		up.sets = append(up.sets, tg)
	}
	if u.Where == nil {
		return nil
	}
	x, err := Compile(u.Where, scope)
	if err != nil {
		return err
	}
	if typ, ok := x.Type(); ok && typ != table.Bool {
		return errAt(u.Where.Pos(), "WHERE needs BOOLEAN, and %s is %s", u.Where, TypeName(typ))
	}
	up.where, up.whereSt = x, u.Where
	return nil
}

// update runs DO UPDATE for the row old that row conflicts with. It
// reports whether it changed old: a WHERE that is not TRUE keeps it.
func (up *upsert) update(tx *table.Tx, t *table.Table, cs []rowCheck, old, row, params []any, at sqlparse.At) (bool, error) {
	both := append(append(make([]any, 0, 2*len(old)), old...), row...)
	keep, err := keeps(up.where, up.whereSt, both, params)
	if err != nil || !keep {
		return false, err
	}
	n := append([]any(nil), old...)
	for _, tg := range up.sets {
		if n[tg.col], err = tg.value(t, both, params); err != nil {
			return false, err
		}
	}
	if err := checkNull(t, n, at); err != nil {
		return false, err
	}
	if err := checkRow(t, cs, n, at); err != nil {
		return false, err
	}
	// A new key moves the row, as in UPDATE.
	if sameKey(t, old, n) {
		err = tx.Update(t.Name, n)
	} else if _, err = tx.Delete(t.Name, keyOf(t, old)...); err == nil {
		err = tx.Insert(t.Name, n)
	}
	return true, engineErr(at, err)
}
