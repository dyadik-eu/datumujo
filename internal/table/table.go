package table

import (
	"errors"
	"fmt"
	"sort"

	"github.com/dyadik-eu/datumujo/internal/btree"
)

// CatalogSlot is the root slot of the catalog tree, which holds the
// schema.
const CatalogSlot = 0

// schemaKey is the key of the schema in the catalog tree.
var schemaKey = []byte("schema")

// Reader reads pages and root slots: a snapshot or a write transaction of
// the store.
type Reader interface {
	btree.Pages
	Root(i int) uint64
}

// Writer also changes them: a write transaction of the store.
type Writer interface {
	btree.WritePages
	Root(i int) uint64
	SetRoot(i int, no uint64) error
}

// Errors of reads and writes of rows.
var (
	ErrNoTable  = errors.New("table: no such table")
	ErrExists   = errors.New("table: a row with this key exists")
	ErrNotFound = errors.New("table: no row with this key")
)

// View reads the tables as one snapshot or one write transaction sees
// them.
type View struct {
	r        Reader
	pageSize int
	schema   *Schema
}

// Open reads the schema and returns a view. A file without a catalog has
// an empty schema of version 0.
func Open(r Reader, pageSize int) (*View, error) {
	v := &View{r: r, pageSize: pageSize, schema: &Schema{}}
	root := r.Root(CatalogSlot)
	if root == 0 {
		return v, nil
	}
	b, ok, err := btree.Open(root, pageSize).Get(r, schemaKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &damaged{"catalog", "the catalog tree holds no schema"}
	}
	if v.schema, err = decodeSchema(b); err != nil {
		return nil, err
	}
	return v, nil
}

// Schema returns the schema. The caller must not change it.
func (v *View) Schema() *Schema { return v.schema }

func (v *View) table(name string) (*Table, error) {
	t, ok := v.schema.Table(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoTable, name)
	}
	return t, nil
}

// keyOf checks the values of a key and encodes them.
func keyOf(t *Table, key []any) ([]byte, error) {
	if len(key) != len(t.Key) {
		return nil, fmt.Errorf("%w: table %s: %d key values, the key has %d columns", ErrValue, t.Name, len(key), len(t.Key))
	}
	row := make(Row, len(t.Columns))
	for i, k := range t.Key {
		if err := checkValue(t, t.Columns[k], key[i], true); err != nil {
			return nil, err
		}
		row[k] = key[i]
	}
	return encodeKey(t, row), nil
}

func (v *View) decodeRow(t *Table, key, value []byte) (Row, error) {
	row := make(Row, len(t.Columns))
	if err := decodeKey(t, key, row); err != nil {
		return nil, err
	}
	if err := decodeValues(t, value, row); err != nil {
		return nil, err
	}
	return row, nil
}

// Get returns the row with the given key values. The key values are read
// back from the stored key. So a time comes back in UTC, and a float key
// of -0 comes back as +0.
func (v *View) Get(table string, key ...any) (Row, bool, error) {
	t, err := v.table(table)
	if err != nil {
		return nil, false, err
	}
	k, err := keyOf(t, key)
	if err != nil {
		return nil, false, err
	}
	value, ok, err := btree.Open(t.Root, v.pageSize).Get(v.r, k)
	if err != nil || !ok {
		return nil, false, err
	}
	row, err := v.decodeRow(t, k, value)
	return row, err == nil, err
}

// Rows walks the rows of a table in key order.
type Rows struct {
	v   *View
	t   *Table
	c   *btree.Cursor
	row Row
	err error
}

// Scan returns the rows of a table in key order, not yet positioned: call
// Next first.
func (v *View) Scan(table string) (*Rows, error) {
	t, err := v.table(table)
	if err != nil {
		return nil, err
	}
	return &Rows{v: v, t: t, c: btree.Open(t.Root, v.pageSize).Cursor(v.r)}, nil
}

// Next moves to the next row and reports whether there is one. After
// false, Err tells an end from a failure.
func (r *Rows) Next() bool {
	if r.err != nil {
		return false
	}
	if r.row == nil {
		r.c.First()
	} else {
		r.c.Next()
	}
	if !r.c.Valid() {
		r.err = r.c.Err()
		return false
	}
	value, err := r.c.Value()
	if err == nil {
		r.row, err = r.v.decodeRow(r.t, r.c.Key(), value)
	}
	r.err = err
	return err == nil
}

// Row returns the current row.
func (r *Rows) Row() Row { return r.row }

// Err returns the error that stopped the scan, if any.
func (r *Rows) Err() error { return r.err }

// Tx changes tables and the schema in one write transaction of the store.
// The store transaction commits or rolls back all of it. After an error
// other than ErrValue, ErrSchema, ErrNoTable, ErrExists or ErrNotFound, the
// caller rolls the store transaction back.
type Tx struct {
	View
	w Writer
}

// Begin returns a Tx over a write transaction of the store.
func Begin(w Writer, pageSize int) (*Tx, error) {
	v, err := Open(w, pageSize)
	if err != nil {
		return nil, err
	}
	return &Tx{View: *v, w: w}, nil
}

// saveSchema writes the schema with the next version.
func (tx *Tx) saveSchema(s *Schema) error {
	root := tx.w.Root(CatalogSlot)
	var cat *btree.Tree
	if root == 0 {
		var err error
		if cat, err = btree.Create(tx.w, tx.pageSize); err != nil {
			return err
		}
		if err := tx.w.SetRoot(CatalogSlot, cat.Root); err != nil {
			return err
		}
	} else {
		cat = btree.Open(root, tx.pageSize)
	}
	s.Version = tx.schema.Version + 1
	if err := cat.Put(tx.w, schemaKey, encodeSchema(s)); err != nil {
		return err
	}
	tx.schema = s
	return nil
}

// copySchema returns a copy that a change can modify. The copy shares
// the column and key slices of each table. A change only appends to them
// and never writes within their length, so the schema it copies from
// stays as it was.
func (tx *Tx) copySchema() *Schema {
	return &Schema{Version: tx.schema.Version, Tables: append([]Table(nil), tx.schema.Tables...)}
}

// CreateTable adds a table.
func (tx *Tx) CreateTable(d Def) error {
	t, err := fromDef(d)
	if err != nil {
		return err
	}
	if _, ok := tx.schema.Table(t.Name); ok {
		return schemaErr("table %s exists", t.Name)
	}
	tree, err := btree.Create(tx.w, tx.pageSize)
	if err != nil {
		return err
	}
	t.Root = tree.Root
	s := tx.copySchema()
	s.Tables = append(s.Tables, t)
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	return tx.saveSchema(s)
}

// AddColumn adds a column at the end of a table. It must allow null: the
// rows written before read it as null.
func (tx *Tx) AddColumn(table string, c Column) error {
	if !c.Null {
		return schemaErr("table %s: added column %s must allow null", table, c.Name)
	}
	if _, err := tx.table(table); err != nil {
		return err
	}
	s := tx.copySchema()
	t, _ := s.Table(table)
	t.Columns = append(t.Columns, c)
	if err := t.check(); err != nil {
		return err
	}
	return tx.saveSchema(s)
}

func (tx *Tx) checkRow(t *Table, row Row) error {
	if len(row) != len(t.Columns) {
		return fmt.Errorf("%w: table %s: %d values, the table has %d columns", ErrValue, t.Name, len(row), len(t.Columns))
	}
	inKey := map[int]bool{}
	for _, k := range t.Key {
		inKey[k] = true
	}
	for i, c := range t.Columns {
		if err := checkValue(t, c, row[i], inKey[i]); err != nil {
			return err
		}
	}
	return nil
}

// put writes a row. With exists false, no row may have the key yet. With
// exists true, a row must have it.
func (tx *Tx) put(table string, row Row, exists bool) error {
	t, err := tx.table(table)
	if err != nil {
		return err
	}
	if err := tx.checkRow(t, row); err != nil {
		return err
	}
	tree := btree.Open(t.Root, tx.pageSize)
	k := encodeKey(t, row)
	_, found, err := tree.Get(tx.w, k)
	switch {
	case err != nil:
		return err
	case found && !exists:
		return fmt.Errorf("%w: table %s", ErrExists, t.Name)
	case !found && exists:
		return fmt.Errorf("%w: table %s", ErrNotFound, t.Name)
	}
	return tree.Put(tx.w, k, encodeValues(t, row))
}

// Insert adds a row. It fails with ErrExists if a row has the same key.
func (tx *Tx) Insert(table string, row Row) error { return tx.put(table, row, false) }

// Update replaces the row with the same key. It fails with ErrNotFound if
// there is none.
func (tx *Tx) Update(table string, row Row) error { return tx.put(table, row, true) }

// Delete removes the row with the given key values and reports whether
// there was one.
func (tx *Tx) Delete(table string, key ...any) (bool, error) {
	t, err := tx.table(table)
	if err != nil {
		return false, err
	}
	k, err := keyOf(t, key)
	if err != nil {
		return false, err
	}
	return btree.Open(t.Root, tx.pageSize).Delete(tx.w, k)
}
