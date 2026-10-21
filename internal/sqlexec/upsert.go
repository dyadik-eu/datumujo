package sqlexec

import (
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
	if !u.Nothing {
		return nil, notYet(u.At, "ON CONFLICT DO UPDATE", 33)
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
