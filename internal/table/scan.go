package table

import (
	"bytes"
	"fmt"

	"github.com/dyadik-eu/datumujo/internal/btree"
)

// Options select the rows of a scan. The values of Prefix, From and To
// are for the leading columns of the primary key, or of the index if
// Index is set. They may name fewer columns than there are.
type Options struct {
	Index   string // "" scans in primary key order
	Prefix  []any  // only rows with these values in the leading columns
	From    []any  // only rows at or after these values
	To      []any  // only rows before these values
	After   []byte // continue after this cursor, in scan order
	Reverse bool   // from the last row to the first
}

// Rows walks the rows of a scan.
type Rows struct {
	v            *View
	t            *Table
	ix           *Index
	c            *btree.Cursor
	lower, upper []byte // lower inclusive, upper exclusive, nil for none
	reverse      bool
	started      bool
	done         bool
	row          Row
	err          error
}

// Scan returns the rows that the options select, in key order or in
// reverse. Rows is not yet positioned: call Next first.
func (v *View) Scan(table string, o Options) (*Rows, error) {
	t, err := v.table(table)
	if err != nil {
		return nil, err
	}
	r := &Rows{v: v, t: t, reverse: o.Reverse}
	cols, root := t.Key, t.Root
	if o.Index != "" {
		ix, ok := t.Index(o.Index)
		if !ok {
			return nil, fmt.Errorf("%w: table %s, index %s", ErrNoIndex, t.Name, o.Index)
		}
		r.ix, cols, root = ix, ix.Columns, ix.Root
	}
	r.c = btree.Open(root, v.pageSize).Cursor(v.r)
	var prefix, from, to []byte
	if prefix, err = bound(t, cols, o.Prefix); err != nil {
		return nil, err
	}
	if from, err = bound(t, cols, o.From); err != nil {
		return nil, err
	}
	if to, err = bound(t, cols, o.To); err != nil {
		return nil, err
	}
	r.lower = maxLower(prefix, from)
	r.upper = minUpper(successor(prefix), to)
	if o.After != nil {
		if o.Reverse {
			r.upper = minUpper(r.upper, o.After)
		} else {
			// The first key above After is After with 0x00 appended.
			r.lower = maxLower(r.lower, append(bytes.Clone(o.After), 0))
		}
	}
	return r, nil
}

// bound encodes values for the leading columns. A column that allows
// null takes nil.
func bound(t *Table, cols []int, vals []any) ([]byte, error) {
	if len(vals) > len(cols) {
		return nil, fmt.Errorf("%w: table %s: %d values for %d columns", ErrValue, t.Name, len(vals), len(cols))
	}
	row := make(Row, len(t.Columns))
	for i, v := range vals {
		if err := checkValue(t, t.Columns[cols[i]], v, true); err != nil {
			return nil, err
		}
		row[cols[i]] = v
	}
	return appendColumns(nil, t, cols[:len(vals)], row), nil
}

// successor returns the first key above every key that starts with b,
// or nil if there is none.
func successor(b []byte) []byte {
	c := bytes.Clone(b)
	for i := len(c) - 1; i >= 0; i-- {
		if c[i] < 0xff {
			c[i]++
			return c[:i+1]
		}
	}
	return nil
}

// maxLower and minUpper combine bounds. A nil lower bound is below every
// key, a nil upper bound above every key.
func maxLower(a, b []byte) []byte {
	if bytes.Compare(a, b) >= 0 {
		return a
	}
	return b
}

func minUpper(a, b []byte) []byte {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case bytes.Compare(a, b) <= 0:
		return a
	}
	return b
}

// Next moves to the next row and reports whether there is one. After
// false, Err tells an end from a failure.
func (r *Rows) Next() bool {
	if r.err != nil || r.done {
		return false
	}
	switch {
	case !r.started:
		r.started = true
		r.position()
	case r.reverse:
		r.c.Prev()
	default:
		r.c.Next()
	}
	if !r.c.Valid() {
		r.err, r.done = r.c.Err(), true
		return false
	}
	key := r.c.Key()
	if r.reverse && r.lower != nil && bytes.Compare(key, r.lower) < 0 ||
		!r.reverse && r.upper != nil && bytes.Compare(key, r.upper) >= 0 {
		r.done = true
		return false
	}
	r.row, r.err = r.load(key)
	return r.err == nil
}

// position puts the cursor on the first row of the scan.
func (r *Rows) position() {
	switch {
	case !r.reverse && r.lower != nil:
		r.c.Seek(r.lower)
	case !r.reverse:
		r.c.First()
	case r.upper == nil:
		r.c.Last()
	default:
		r.c.Seek(r.upper)
		if r.c.Valid() {
			r.c.Prev()
		} else if r.c.Err() == nil {
			r.c.Last()
		}
	}
}

// load reads the row of a key. An index entry leads to the row by its
// primary key, and the row must have the entry's index values.
func (r *Rows) load(key []byte) (Row, error) {
	value, err := r.c.Value()
	if err != nil {
		return nil, err
	}
	if r.ix == nil {
		return r.v.decodeRow(r.t, key, value)
	}
	where := "index " + r.ix.Name + " of table " + r.t.Name
	if len(value) != 0 {
		return nil, &damaged{where, "an entry has a value"}
	}
	pk, err := decodeIndexKey(r.t, r.ix, key, make(Row, len(r.t.Columns)))
	if err != nil {
		return nil, err
	}
	row, ok, err := r.v.get(r.t, pk)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, &damaged{where, "an entry has no row"}
	case !bytes.Equal(indexKey(r.t, r.ix, row), key):
		return nil, &damaged{where, "an entry does not match its row"}
	}
	return row, nil
}

// Row returns the current row.
func (r *Rows) Row() Row { return r.row }

// Cursor returns the position of the current row. Options.After takes it
// to continue a scan with the same Index and Reverse. It stays valid when
// rows change: the scan goes on at the next key, whatever rows exist.
func (r *Rows) Cursor() []byte { return r.c.Key() }

// Err returns the error that stopped the scan, if any.
func (r *Rows) Err() error { return r.err }
