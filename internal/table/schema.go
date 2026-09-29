// Package table holds typed tables over the trees of a store. A table
// has columns of six types and a primary key over one or more columns.
// The schema is stored in the file, in a catalog tree at root slot 0, and
// has a version number that each change increments.
package table

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// Type is the type of a column.
type Type uint8

// The column types of requirement M-1.
const (
	Int64 Type = iota + 1
	Float64
	Bool
	String
	Bytes
	Time
)

var typeNames = map[Type]string{Int64: "int64", Float64: "float64", Bool: "bool", String: "string", Bytes: "bytes", Time: "time"}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", uint8(t))
}

// Column is one column of a table. A column with Null set can hold null.
// Default is the text of its default, "" for none. This package keeps
// the text and does not read it; the layer that writes it gives it
// meaning.
type Column struct {
	Name    string
	Type    Type
	Null    bool
	Default string
}

// Def defines a table: its name, its columns in order, the names of
// the columns of its primary key in key order, and its checks. This
// package keeps the texts of checks, as of defaults, and does not read
// them.
type Def struct {
	Name    string
	Columns []Column
	Key     []string
	Checks  []string
}

// Limits of a schema.
const (
	MaxName    = 255
	MaxColumns = 1024
	MaxIndexes = 64
)

// IndexDef defines an index: its name, the names of its columns in index
// order, and whether two rows may have the same values in them. Rows with
// a null in an index column never conflict in a unique index.
type IndexDef struct {
	Name    string
	Columns []string
	Unique  bool
}

// Index is an index of a table.
type Index struct {
	Name    string
	Columns []int // column numbers, in index order
	Unique  bool
	Root    uint64 // root page of the index's tree
}

// Table is a table of the schema.
type Table struct {
	Name    string
	Columns []Column
	Key     []int  // column numbers of the primary key, in key order
	Root    uint64 // root page of the table's tree
	Indexes []Index
	Checks  []string
}

// Index returns the index with the given name.
func (t *Table) Index(name string) (*Index, bool) {
	for i := range t.Indexes {
		if t.Indexes[i].Name == name {
			return &t.Indexes[i], true
		}
	}
	return nil, false
}

// Schema is the set of tables with the version of the schema. The tables
// are in the order of their names.
type Schema struct {
	Version uint64
	Tables  []Table
}

// Table returns the table with the given name.
func (s *Schema) Table(name string) (*Table, bool) {
	i := sort.Search(len(s.Tables), func(i int) bool { return s.Tables[i].Name >= name })
	if i < len(s.Tables) && s.Tables[i].Name == name {
		return &s.Tables[i], true
	}
	return nil, false
}

// ErrSchema matches a definition or a schema change that is not valid.
var ErrSchema = errors.New("table: schema not valid")

func schemaErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrSchema, fmt.Sprintf(format, a...))
}

func checkName(what, name string) error {
	if name == "" || len(name) > MaxName || !utf8.ValidString(name) {
		return schemaErr("%s name %q: must be 1 to %d bytes of UTF-8", what, name, MaxName)
	}
	return nil
}

// check checks a table: names, types, and a key over existing columns
// that do not allow null.
func (t *Table) check() error {
	if err := checkName("table", t.Name); err != nil {
		return err
	}
	if len(t.Columns) == 0 || len(t.Columns) > MaxColumns {
		return schemaErr("table %s: %d columns, must be 1 to %d", t.Name, len(t.Columns), MaxColumns)
	}
	seen := map[string]bool{}
	for _, c := range t.Columns {
		if err := checkName("column", c.Name); err != nil {
			return err
		}
		if seen[c.Name] {
			return schemaErr("table %s: column %s twice", t.Name, c.Name)
		}
		seen[c.Name] = true
		if _, ok := typeNames[c.Type]; !ok {
			return schemaErr("table %s: column %s has %v", t.Name, c.Name, c.Type)
		}
		if !utf8.ValidString(c.Default) {
			return schemaErr("table %s: the default of column %s is not UTF-8", t.Name, c.Name)
		}
	}
	for i, c := range t.Checks {
		if c == "" || !utf8.ValidString(c) {
			return schemaErr("table %s: check %d must be text of UTF-8, not empty", t.Name, i+1)
		}
	}
	if len(t.Key) == 0 {
		return schemaErr("table %s: no primary key", t.Name)
	}
	inKey := map[int]bool{}
	for _, k := range t.Key {
		if k < 0 || k >= len(t.Columns) {
			return schemaErr("table %s: key column %d; there are %d columns", t.Name, k, len(t.Columns))
		}
		if inKey[k] {
			return schemaErr("table %s: column %s twice in the key", t.Name, t.Columns[k].Name)
		}
		inKey[k] = true
		if t.Columns[k].Null {
			return schemaErr("table %s: key column %s allows null", t.Name, t.Columns[k].Name)
		}
	}
	if len(t.Indexes) > MaxIndexes {
		return schemaErr("table %s: %d indexes, at most %d", t.Name, len(t.Indexes), MaxIndexes)
	}
	names := map[string]bool{}
	for _, ix := range t.Indexes {
		if err := checkName("index", ix.Name); err != nil {
			return err
		}
		if names[ix.Name] {
			return schemaErr("table %s: index %s twice", t.Name, ix.Name)
		}
		names[ix.Name] = true
		if len(ix.Columns) == 0 {
			return schemaErr("table %s: index %s has no columns", t.Name, ix.Name)
		}
		inIndex := map[int]bool{}
		for _, c := range ix.Columns {
			if c < 0 || c >= len(t.Columns) {
				return schemaErr("table %s: index %s: column %d; there are %d columns", t.Name, ix.Name, c, len(t.Columns))
			}
			if inIndex[c] {
				return schemaErr("table %s: index %s: column %s twice", t.Name, ix.Name, t.Columns[c].Name)
			}
			inIndex[c] = true
		}
	}
	return nil
}

// columnNumbers returns the numbers of the named columns.
func (t *Table) columnNumbers(what string, names []string) ([]int, error) {
	var out []int
	for _, name := range names {
		k := -1
		for i, c := range t.Columns {
			if c.Name == name {
				k = i
			}
		}
		if k < 0 {
			return nil, schemaErr("table %s: %s column %s does not exist", t.Name, what, name)
		}
		out = append(out, k)
	}
	return out, nil
}

// fromDef turns a definition into a table without a root.
func fromDef(d Def) (Table, error) {
	t := Table{Name: d.Name, Columns: append([]Column(nil), d.Columns...), Checks: append([]string(nil), d.Checks...)}
	var err error
	if t.Key, err = t.columnNumbers("key", d.Key); err != nil {
		return Table{}, err
	}
	return t, t.check()
}

// values returns the numbers of the columns that are not in the key, in
// column order. A row stores these; the key holds the others.
func (t *Table) values() []int {
	inKey := map[int]bool{}
	for _, k := range t.Key {
		inKey[k] = true
	}
	var v []int
	for i := range t.Columns {
		if !inKey[i] {
			v = append(v, i)
		}
	}
	return v
}

// The first byte of an encoded schema is its format. Format 1 had no
// indexes; no release wrote it, and this code does not read it. Format 2
// is the schema of v0.1.0 and v0.2.0. Format 3 adds a default to each
// column and checks to each table.
//
// A schema without a default or a check is written in format 2, so
// v0.2.0 reads it. A schema with one is written in format 3, and only a
// file of format version 2 holds it (requirement L-19).
const (
	schemaFormat2 = 2
	schemaFormat3 = 3
)

// Extended reports whether the schema has a default or a check, and so
// needs schema format 3 and format version 2 of the file.
func (s *Schema) Extended() bool {
	for _, t := range s.Tables {
		if len(t.Checks) > 0 {
			return true
		}
		for _, c := range t.Columns {
			if c.Default != "" {
				return true
			}
		}
	}
	return false
}

// encodeSchema writes the schema. decodeSchema of the result gives the
// same schema, and encodeSchema of a decoded schema gives the same bytes.
func encodeSchema(s *Schema) []byte {
	format := byte(schemaFormat2)
	if s.Extended() {
		format = schemaFormat3
	}
	b := []byte{format}
	b = binary.AppendUvarint(b, s.Version)
	b = binary.AppendUvarint(b, uint64(len(s.Tables)))
	str := func(v string) {
		b = binary.AppendUvarint(b, uint64(len(v)))
		b = append(b, v...)
	}
	for _, t := range s.Tables {
		str(t.Name)
		b = binary.AppendUvarint(b, t.Root)
		b = binary.AppendUvarint(b, uint64(len(t.Columns)))
		for _, c := range t.Columns {
			str(c.Name)
			flags := byte(0)
			if c.Null {
				flags = 1
			}
			b = append(b, byte(c.Type), flags)
			if format == schemaFormat3 {
				str(c.Default)
			}
		}
		b = binary.AppendUvarint(b, uint64(len(t.Key)))
		for _, k := range t.Key {
			b = binary.AppendUvarint(b, uint64(k))
		}
		b = binary.AppendUvarint(b, uint64(len(t.Indexes)))
		for _, ix := range t.Indexes {
			str(ix.Name)
			unique := byte(0)
			if ix.Unique {
				unique = 1
			}
			b = append(b, unique)
			b = binary.AppendUvarint(b, ix.Root)
			b = binary.AppendUvarint(b, uint64(len(ix.Columns)))
			for _, c := range ix.Columns {
				b = binary.AppendUvarint(b, uint64(c))
			}
		}
		if format == schemaFormat3 {
			b = binary.AppendUvarint(b, uint64(len(t.Checks)))
			for _, c := range t.Checks {
				str(c)
			}
		}
	}
	return b
}

// decodeSchema reads a schema. It accepts only what encodeSchema writes
// for a valid schema, and returns an error for anything else.
func decodeSchema(b []byte) (*Schema, error) {
	d := decoder{b: b, what: "schema"}
	format := d.byte()
	if d.err == nil && format != schemaFormat2 && format != schemaFormat3 {
		return nil, d.fail("format %d; this code reads formats %d and %d", format, schemaFormat2, schemaFormat3)
	}
	// In format 3, a column takes 1 byte more, the length of its default.
	colSize := 3
	if format == schemaFormat3 {
		colSize = 4
	}
	s := &Schema{Version: d.uvarint()}
	// Each table takes at least 6 bytes, each column 3, each key column
	// 1. Counts above what the rest of the input can hold are rejected
	// before anything is allocated for them.
	n := d.count(6)
	for i := 0; i < n && d.err == nil; i++ {
		t := Table{Name: d.str()}
		t.Root = d.uvarint()
		cols := d.count(colSize)
		for j := 0; j < cols && d.err == nil; j++ {
			c := Column{Name: d.str()}
			c.Type = Type(d.byte())
			switch d.byte() {
			case 0:
			case 1:
				c.Null = true
			default:
				d.fail("column flags")
			}
			if format == schemaFormat3 {
				c.Default = d.str()
			}
			t.Columns = append(t.Columns, c)
		}
		keys := d.count(1)
		for j := 0; j < keys && d.err == nil; j++ {
			// check rejects a column number out of range. One above
			// the range of int becomes negative and is rejected too.
			t.Key = append(t.Key, int(d.uvarint()))
		}
		// Each index takes at least 5 bytes, each of its columns 1.
		nix := d.count(5)
		for j := 0; j < nix && d.err == nil; j++ {
			ix := Index{Name: d.str()}
			switch d.byte() {
			case 0:
			case 1:
				ix.Unique = true
			default:
				d.fail("index flags")
			}
			ix.Root = d.uvarint()
			if d.err == nil && ix.Root == 0 {
				d.fail("index %s has root page 0", ix.Name)
			}
			cols := d.count(1)
			for k := 0; k < cols && d.err == nil; k++ {
				ix.Columns = append(ix.Columns, int(d.uvarint()))
			}
			t.Indexes = append(t.Indexes, ix)
		}
		if format == schemaFormat3 {
			// Each check takes at least 2 bytes: a length and a byte.
			nc := d.count(2)
			for j := 0; j < nc && d.err == nil; j++ {
				t.Checks = append(t.Checks, d.str())
			}
		}
		if d.err != nil {
			break
		}
		if err := t.check(); err != nil {
			return nil, d.fail("%v", err)
		}
		if t.Root == 0 {
			return nil, d.fail("table %s has root page 0", t.Name)
		}
		if i > 0 && s.Tables[i-1].Name >= t.Name {
			return nil, d.fail("table %s is not after %s", t.Name, s.Tables[i-1].Name)
		}
		s.Tables = append(s.Tables, t)
	}
	if err := d.end(); err != nil {
		return nil, err
	}
	// encodeSchema never writes format 3 without a default or a check.
	// Such a schema is in a form that v0.2.0 cannot read, for no reason.
	if format == schemaFormat3 && !s.Extended() {
		return nil, d.fail("format %d without a default or a check", format)
	}
	return s, nil
}
