package table

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/dyadik-eu/datumujo/internal/btree"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// CatalogSlot is the root slot of the catalog tree, which holds the
// schema and the counters.
const CatalogSlot = 0

// schemaKey is the key of the schema in the catalog tree.
var schemaKey = []byte("schema")

// Reader reads pages, root slots and the format version of the file: a
// snapshot or a write transaction of the store.
type Reader interface {
	btree.Pages
	Root(i int) uint64
	Version() uint32
}

// Writer also changes them: a write transaction of the store.
type Writer interface {
	btree.WritePages
	Root(i int) uint64
	Version() uint32
	SetRoot(i int, no uint64) error
	SetVersion(v uint32) error
	Savepoint() store.Savepoint
	RollbackTo(sp store.Savepoint) error
}

// Errors of reads and writes of rows.
var (
	ErrNoTable  = errors.New("table: no such table")
	ErrNoIndex  = errors.New("table: no such index")
	ErrExists   = errors.New("table: a row with this key exists")
	ErrNotFound = errors.New("table: no row with this key")
	ErrUnique   = errors.New("table: a row with these values exists in a unique index")
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
	if v.schema.Extended() && r.Version() < 2 {
		return nil, &damaged{"schema", fmt.Sprintf("a default or a check in a file of format version %d", r.Version())}
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
	return v.get(t, k)
}

func (v *View) get(t *Table, k []byte) (Row, bool, error) {
	value, ok, err := btree.Open(t.Root, v.pageSize).Get(v.r, k)
	if err != nil || !ok {
		return nil, false, err
	}
	row, err := v.decodeRow(t, k, value)
	return row, err == nil, err
}

// Tx changes tables and the schema in one write transaction of the store.
// The store transaction commits or rolls back all of it. After an error
// that does not match ErrValue, ErrSchema, ErrNoTable, ErrNoIndex,
// ErrExists, ErrNotFound or ErrUnique, the caller rolls the store
// transaction back. A write during a scan of the same Tx leaves the scan
// undefined.
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

// catalog returns the catalog tree. Without one, it creates it with the
// schema as it is.
func (tx *Tx) catalog() (*btree.Tree, error) {
	if root := tx.w.Root(CatalogSlot); root != 0 {
		return btree.Open(root, tx.pageSize), nil
	}
	cat, err := btree.Create(tx.w, tx.pageSize)
	if err != nil {
		return nil, err
	}
	if err := tx.w.SetRoot(CatalogSlot, cat.Root); err != nil {
		return nil, err
	}
	return cat, cat.Put(tx.w, schemaKey, encodeSchema(tx.schema))
}

// saveSchema writes the schema with the next version. The first default
// or check raises the file to format version 2. A file never goes back
// to version 1, even when the last one is dropped.
func (tx *Tx) saveSchema(s *Schema) error {
	cat, err := tx.catalog()
	if err != nil {
		return err
	}
	if s.Extended() && tx.w.Version() < 2 {
		if err := tx.w.SetVersion(2); err != nil {
			return err
		}
	}
	s.Version = tx.schema.Version + 1
	if err := cat.Put(tx.w, schemaKey, encodeSchema(s)); err != nil {
		return err
	}
	tx.schema = s
	return nil
}

// copySchema returns a copy that a change can modify. The copy shares
// the column, key and index slices of each table. A change only appends
// to them and never writes within their length, so the schema it copies
// from stays as it was.
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

// AddColumn adds a column at the end of a table. The rows written before
// read its Fill, or null without one. So it must allow null or have a
// Fill. No row is rewritten. checks are added to the checks of the
// table; this package keeps them and does not test the rows against
// them.
func (tx *Tx) AddColumn(table string, c Column, checks ...string) error {
	if !c.Null && c.Fill == nil {
		return schemaErr("table %s: added column %s must allow null or have a fill", table, c.Name)
	}
	if _, err := tx.table(table); err != nil {
		return err
	}
	s := tx.copySchema()
	t, _ := s.Table(table)
	t.Columns = append(t.Columns, c)
	t.Checks = append(t.Checks, checks...)
	if err := t.check(); err != nil {
		return err
	}
	return tx.saveSchema(s)
}

// CreateIndex adds an index to a table and fills it from the rows. If a
// row does not fit the index, it fails, frees the pages of the new index
// and changes no row and no schema.
func (tx *Tx) CreateIndex(table string, d IndexDef) error {
	if _, err := tx.table(table); err != nil {
		return err
	}
	s := tx.copySchema()
	t, _ := s.Table(table)
	cols, err := t.columnNumbers("index", d.Columns)
	if err != nil {
		return err
	}
	t.Indexes = append(append([]Index(nil), t.Indexes...), Index{Name: d.Name, Columns: cols, Unique: d.Unique})
	if err := t.check(); err != nil {
		return err
	}
	tree, err := btree.Create(tx.w, tx.pageSize)
	if err != nil {
		return err
	}
	ix := &t.Indexes[len(t.Indexes)-1]
	ix.Root = tree.Root
	if err := tx.fill(t, ix); err != nil {
		if drop := tree.Drop(tx.w); drop != nil {
			return errors.Join(err, drop)
		}
		return err
	}
	return tx.saveSchema(s)
}

// fill puts an entry for each row of the table into a new index.
func (tx *Tx) fill(t *Table, ix *Index) error {
	c := btree.Open(t.Root, tx.pageSize).Cursor(tx.w)
	for c.First(); c.Valid(); c.Next() {
		value, err := c.Value()
		if err != nil {
			return err
		}
		row, err := tx.decodeRow(t, c.Key(), value)
		if err != nil {
			return err
		}
		for _, col := range ix.Columns {
			if err := checkValue(t, t.Columns[col], row[col], true); err != nil {
				return err
			}
		}
		if err := tx.addEntry(t, ix, row); err != nil {
			return err
		}
	}
	return c.Err()
}

// indexed reports the columns that are in the key or in an index. Their
// values have a key form, so a float there must not be NaN.
func indexed(t *Table) map[int]bool {
	m := map[int]bool{}
	for _, k := range t.Key {
		m[k] = true
	}
	for _, ix := range t.Indexes {
		for _, c := range ix.Columns {
			m[c] = true
		}
	}
	return m
}

func (tx *Tx) checkRow(t *Table, row Row) error {
	if len(row) != len(t.Columns) {
		return fmt.Errorf("%w: table %s: %d values, the table has %d columns", ErrValue, t.Name, len(row), len(t.Columns))
	}
	inKey := indexed(t)
	for i, c := range t.Columns {
		if err := checkValue(t, c, row[i], inKey[i]); err != nil {
			return err
		}
	}
	return nil
}

// checkLength checks a key against the limit of the tree before anything
// is written.
func (tx *Tx) checkLength(what string, k []byte) error {
	if max := btree.LimitsFor(tx.pageSize).MaxKey; len(k) > max {
		return fmt.Errorf("%w: %s: a key of %d bytes, at most %d for pages of %d", ErrValue, what, len(k), max, tx.pageSize)
	}
	return nil
}

// checkUnique fails if a row has the index values of row in a unique
// index. A row with a null in the index conflicts with no row. The row
// itself has no entry with these values. In put, the check runs only
// for an index whose values change. In fill, it runs before the row's
// entry is added.
func (tx *Tx) checkUnique(t *Table, ix *Index, row Row) error {
	if !ix.Unique {
		return nil
	}
	for _, c := range ix.Columns {
		if row[c] == nil {
			return nil
		}
	}
	// The key forms of index values are prefix-free, so an entry starts
	// with prefix exactly when it has these values.
	prefix := appendIndexColumns(nil, t, ix, row)
	c := btree.Open(ix.Root, tx.pageSize).Cursor(tx.w)
	if c.Seek(prefix); c.Valid() && bytes.HasPrefix(c.Key(), prefix) {
		return fmt.Errorf("%w: table %s, index %s", ErrUnique, t.Name, ix.Name)
	}
	return c.Err()
}

// addEntry checks an index entry for a row and puts it.
func (tx *Tx) addEntry(t *Table, ix *Index, row Row) error {
	k := indexKey(t, ix, row)
	if err := tx.checkLength("index "+ix.Name, k); err != nil {
		return err
	}
	if err := tx.checkUnique(t, ix, row); err != nil {
		return err
	}
	return btree.Open(ix.Root, tx.pageSize).Put(tx.w, k, nil)
}

// removeEntry deletes the index entry of a row. A missing entry means the
// index does not match the table.
func (tx *Tx) removeEntry(t *Table, ix *Index, row Row) error {
	found, err := btree.Open(ix.Root, tx.pageSize).Delete(tx.w, indexKey(t, ix, row))
	if err == nil && !found {
		err = &damaged{"index " + ix.Name + " of table " + t.Name, "no entry for a row of the table"}
	}
	return err
}

// put writes a row. With exists false, no row may have the key yet. With
// exists true, a row must have it. All checks run before the first write,
// so a failed check changes nothing.
func (tx *Tx) put(table string, row Row, exists bool) error {
	t, err := tx.table(table)
	if err != nil {
		return err
	}
	if err := tx.checkRow(t, row); err != nil {
		return err
	}
	k := encodeKey(t, row)
	if err := tx.checkLength("table "+t.Name, k); err != nil {
		return err
	}
	old, found, err := tx.get(t, k)
	switch {
	case err != nil:
		return err
	case found && !exists:
		return fmt.Errorf("%w: table %s", ErrExists, t.Name)
	case !found && exists:
		return fmt.Errorf("%w: table %s", ErrNotFound, t.Name)
	}
	// An index entry changes only if the index values change.
	changed := make([]bool, len(t.Indexes))
	for i := range t.Indexes {
		ix := &t.Indexes[i]
		changed[i] = !found || !bytes.Equal(indexKey(t, ix, old), indexKey(t, ix, row))
		if !changed[i] {
			continue
		}
		if err := tx.checkLength("index "+ix.Name, indexKey(t, ix, row)); err != nil {
			return err
		}
		if err := tx.checkUnique(t, ix, row); err != nil {
			return err
		}
	}
	for i := range t.Indexes {
		ix := &t.Indexes[i]
		if !changed[i] {
			continue
		}
		if found {
			if err := tx.removeEntry(t, ix, old); err != nil {
				return err
			}
		}
		if err := btree.Open(ix.Root, tx.pageSize).Put(tx.w, indexKey(t, ix, row), nil); err != nil {
			return err
		}
	}
	return btree.Open(t.Root, tx.pageSize).Put(tx.w, k, encodeValues(t, row))
}

// Insert adds a row. It fails with ErrExists if a row has the same key,
// and with ErrUnique if a unique index has its values.
func (tx *Tx) Insert(table string, row Row) error { return tx.put(table, row, false) }

// Update replaces the row with the same key. It fails with ErrNotFound if
// there is none, and with ErrUnique if another row has its values in a
// unique index.
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
	old, found, err := tx.get(t, k)
	if err != nil || !found {
		return false, err
	}
	for i := range t.Indexes {
		if err := tx.removeEntry(t, &t.Indexes[i], old); err != nil {
			return false, err
		}
	}
	return btree.Open(t.Root, tx.pageSize).Delete(tx.w, k)
}

// DropTable removes a table and its indexes and frees their pages. If it
// fails, pages may be freed already: the caller rolls back.
func (tx *Tx) DropTable(name string) error {
	t, err := tx.table(name)
	if err != nil {
		return err
	}
	for _, ix := range t.Indexes {
		if err := btree.Open(ix.Root, tx.pageSize).Drop(tx.w); err != nil {
			return err
		}
	}
	if err := btree.Open(t.Root, tx.pageSize).Drop(tx.w); err != nil {
		return err
	}
	s := tx.copySchema()
	for i := range s.Tables {
		if s.Tables[i].Name == name {
			s.Tables = append(s.Tables[:i:i], s.Tables[i+1:]...)
			break
		}
	}
	return tx.saveSchema(s)
}

// DropIndex removes an index of a table and frees its pages. If it fails,
// pages may be freed already: the caller rolls back.
func (tx *Tx) DropIndex(table, index string) error {
	t, err := tx.table(table)
	if err != nil {
		return err
	}
	ix, ok := t.Index(index)
	if !ok {
		return fmt.Errorf("%w: table %s, index %s", ErrNoIndex, table, index)
	}
	if err := btree.Open(ix.Root, tx.pageSize).Drop(tx.w); err != nil {
		return err
	}
	s := tx.copySchema()
	nt, _ := s.Table(table)
	var kept []Index
	for _, other := range nt.Indexes {
		if other.Name != index {
			kept = append(kept, other)
		}
	}
	nt.Indexes = kept
	return tx.saveSchema(s)
}

// Savepoint is the state of a write transaction at one point: its pages
// and its schema.
type Savepoint struct {
	store  store.Savepoint
	schema *Schema
}

// Savepoint returns the state of the transaction now.
func (tx *Tx) Savepoint() Savepoint {
	return Savepoint{store: tx.w.Savepoint(), schema: tx.schema}
}

// RollbackTo drops every change after the savepoint sp: rows, indexes,
// counters and the schema. The transaction goes on, and sp stays valid.
func (tx *Tx) RollbackTo(sp Savepoint) error {
	if err := tx.w.RollbackTo(sp.store); err != nil {
		return err
	}
	tx.schema = sp.schema
	return nil
}
