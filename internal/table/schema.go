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
type Column struct {
	Name string
	Type Type
	Null bool
}

// Def defines a table: its name, its columns in order, and the names of
// the columns of its primary key, in key order.
type Def struct {
	Name    string
	Columns []Column
	Key     []string
}

// Limits of a schema. They bound what a decoder allocates for a schema
// from the file.
const (
	MaxName    = 255
	MaxColumns = 1024
)

// Table is a table of the schema.
type Table struct {
	Name    string
	Columns []Column
	Key     []int  // column numbers of the primary key, in key order
	Root    uint64 // root page of the table's tree
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
	return nil
}

// fromDef turns a definition into a table without a root.
func fromDef(d Def) (Table, error) {
	t := Table{Name: d.Name, Columns: append([]Column(nil), d.Columns...)}
	for _, name := range d.Key {
		k := -1
		for i, c := range d.Columns {
			if c.Name == name {
				k = i
			}
		}
		if k < 0 {
			return Table{}, schemaErr("table %s: key column %s does not exist", d.Name, name)
		}
		t.Key = append(t.Key, k)
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

// schemaFormat is the first byte of an encoded schema.
const schemaFormat = 1

// encodeSchema writes the schema. decodeSchema of the result gives the
// same schema, and encodeSchema of a decoded schema gives the same bytes.
func encodeSchema(s *Schema) []byte {
	b := []byte{schemaFormat}
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
		}
		b = binary.AppendUvarint(b, uint64(len(t.Key)))
		for _, k := range t.Key {
			b = binary.AppendUvarint(b, uint64(k))
		}
	}
	return b
}

// decodeSchema reads a schema. It accepts only what encodeSchema writes
// for a valid schema, and returns an error for anything else.
func decodeSchema(b []byte) (*Schema, error) {
	d := decoder{b: b, what: "schema"}
	if f := d.byte(); d.err == nil && f != schemaFormat {
		return nil, d.fail("format %d; this code reads format %d", f, schemaFormat)
	}
	s := &Schema{Version: d.uvarint()}
	// Each table takes at least 6 bytes, each column 3, each key column
	// 1. Counts above what the rest of the input can hold are rejected
	// before anything is allocated for them.
	n := d.count(6)
	for i := 0; i < n && d.err == nil; i++ {
		t := Table{Name: d.str()}
		t.Root = d.uvarint()
		cols := d.count(3)
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
			t.Columns = append(t.Columns, c)
		}
		keys := d.count(1)
		for j := 0; j < keys && d.err == nil; j++ {
			// check rejects a column number out of range. One above
			// the range of int becomes negative and is rejected too.
			t.Key = append(t.Key, int(d.uvarint()))
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
	return s, nil
}
