package table

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

// Row is one row of a table, one value per column in column order. The
// Go type of a value follows the column type: int64, float64, bool,
// string, []byte or time.Time. nil is null.
type Row []any

// ErrValue matches a row or a key whose values do not fit the table.
var ErrValue = errors.New("table: value does not fit the column")

// ErrDamaged matches stored content that does not decode. The page
// checksums held, so the content was written like this: a bug, or a
// change that this code does not know.
var ErrDamaged = errors.New("table: stored content is damaged")

type damaged struct {
	what, reason string
}

func (d *damaged) Error() string        { return fmt.Sprintf("table: %s: %s", d.what, d.reason) }
func (d *damaged) Is(target error) bool { return target == ErrDamaged }

// checkValue checks that v fits column c. In a key, a float must not be
// NaN, because NaN has no place in the order.
func checkValue(t *Table, c Column, v any, key bool) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: table %s, column %s: %s", ErrValue, t.Name, c.Name, fmt.Sprintf(format, a...))
	}
	if v == nil {
		if !c.Null {
			return bad("null, and the column does not allow null")
		}
		return nil
	}
	ok := false
	switch c.Type {
	case Int64:
		_, ok = v.(int64)
	case Float64:
		var f float64
		if f, ok = v.(float64); ok && key && math.IsNaN(f) {
			return bad("NaN in a key")
		}
	case Bool:
		_, ok = v.(bool)
	case String:
		var s string
		if s, ok = v.(string); ok && !utf8.ValidString(s) {
			return bad("the string is not UTF-8")
		}
	case Bytes:
		_, ok = v.([]byte)
	case Time:
		_, ok = v.(time.Time)
	}
	if !ok {
		return bad("%T, the column holds %v", v, c.Type)
	}
	return nil
}

const signBit = 1 << 63

// appendKey appends the key form of v. Key forms sort like the values.
// Numbers sort by value, false before true, strings and bytes by their
// bytes, times by instant. A string or bytes value ends in 0x00 0x01, and
// a 0x00 inside it becomes 0x00 0xFF. So no key form is a prefix of
// another, and keys over more columns keep the order.
func appendKey(b []byte, typ Type, v any) []byte {
	switch typ {
	case Int64:
		return binary.BigEndian.AppendUint64(b, uint64(v.(int64))^signBit)
	case Float64:
		f := v.(float64)
		if f == 0 {
			f = 0 // -0 and +0 are equal and get one key
		}
		bits := math.Float64bits(f)
		if bits&signBit != 0 {
			bits = ^bits
		} else {
			bits |= signBit
		}
		return binary.BigEndian.AppendUint64(b, bits)
	case Bool:
		if v.(bool) {
			return append(b, 1)
		}
		return append(b, 0)
	case String:
		return appendEscaped(b, []byte(v.(string)))
	case Bytes:
		return appendEscaped(b, v.([]byte))
	default: // Time
		tm := v.(time.Time)
		b = binary.BigEndian.AppendUint64(b, uint64(tm.Unix())^signBit)
		return binary.BigEndian.AppendUint32(b, uint32(tm.Nanosecond()))
	}
}

func appendEscaped(b, s []byte) []byte {
	for _, c := range s {
		if c == 0 {
			b = append(b, 0, 0xFF)
		} else {
			b = append(b, c)
		}
	}
	return append(b, 0, 1)
}

// encodeKey returns the key of a row: the key forms of its key columns.
func encodeKey(t *Table, row Row) []byte {
	var b []byte
	for _, k := range t.Key {
		b = appendKey(b, t.Columns[k].Type, row[k])
	}
	return b
}

// decodeKey reads a key into the key columns of row. It accepts only
// what appendKey writes.
func decodeKey(t *Table, b []byte, row Row) error {
	d := decoder{b: b, what: "key of table " + t.Name}
	for _, k := range t.Key {
		row[k] = d.keyValue(t.Columns[k].Type)
	}
	return d.end()
}

// keyValue reads the key form of one value.
func (d *decoder) keyValue(typ Type) any {
	switch typ {
	case Int64:
		return int64(d.uint64() ^ signBit)
	case Float64:
		bits := d.uint64()
		if bits&signBit != 0 {
			bits &^= signBit
		} else {
			bits = ^bits
		}
		f := math.Float64frombits(bits)
		if d.err == nil && (math.IsNaN(f) || bits == signBit) {
			d.fail("a float key that is not written: %x", bits)
		}
		return f
	case Bool:
		switch d.byte() {
		case 0:
			return false
		case 1:
			return true
		}
		d.fail("bool is not 0 or 1")
		return nil
	case String:
		s := d.escaped()
		if d.err == nil && !utf8.Valid(s) {
			d.fail("a string that is not UTF-8")
		}
		return string(s)
	case Bytes:
		return d.escaped()
	default: // Time
		return d.time(d.int64FromKey(), uint64(d.uint32()))
	}
}

// appendIndexColumns appends the key forms of the index columns of a row.
func appendIndexColumns(b []byte, t *Table, ix *Index, row Row) []byte {
	return appendColumns(b, t, ix.Columns, row)
}

// appendColumns appends the key forms of the given columns of a row. A
// column that allows null has a mark before its value: 0 for null, 1 for
// a value. So null sorts before every value. Key columns never allow
// null, so for them this is encodeKey.
func appendColumns(b []byte, t *Table, cols []int, row Row) []byte {
	for _, c := range cols {
		col := t.Columns[c]
		if col.Null {
			if row[c] == nil {
				b = append(b, 0)
				continue
			}
			b = append(b, 1)
		}
		b = appendKey(b, col.Type, row[c])
	}
	return b
}

// indexKey returns the key of a row in an index: its index columns, then
// its primary key. The primary key makes each entry distinct.
func indexKey(t *Table, ix *Index, row Row) []byte {
	return append(appendIndexColumns(nil, t, ix, row), encodeKey(t, row)...)
}

// decodeIndexKey reads the index columns of an index key into row and
// returns the primary key that follows them.
func decodeIndexKey(t *Table, ix *Index, b []byte, row Row) ([]byte, error) {
	d := decoder{b: b, what: "key of index " + ix.Name + " of table " + t.Name}
	for _, c := range ix.Columns {
		col := t.Columns[c]
		if col.Null {
			switch d.byte() {
			case 0:
				row[c] = nil
				continue
			case 1:
			default:
				d.fail("null mark of column %s", col.Name)
			}
		}
		row[c] = d.keyValue(col.Type)
	}
	if d.err != nil {
		return nil, d.err
	}
	return b[d.off:], nil
}

// encodeValues returns the stored form of the columns that are not in
// the key. It starts with their count. A bitmap with a bit set for each
// null follows, then the values that are not null.
func encodeValues(t *Table, row Row) []byte {
	cols := t.values()
	b := binary.AppendUvarint(nil, uint64(len(cols)))
	nulls := make([]byte, (len(cols)+7)/8)
	for i, c := range cols {
		if row[c] == nil {
			nulls[i/8] |= 1 << (i % 8)
		}
	}
	b = append(b, nulls...)
	for _, c := range cols {
		switch v := row[c].(type) {
		case nil:
		case int64:
			b = binary.AppendVarint(b, v)
		case float64:
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(v))
		case bool:
			if v {
				b = append(b, 1)
			} else {
				b = append(b, 0)
			}
		case string:
			b = binary.AppendUvarint(b, uint64(len(v)))
			b = append(b, v...)
		case []byte:
			b = binary.AppendUvarint(b, uint64(len(v)))
			b = append(b, v...)
		case time.Time:
			b = binary.AppendVarint(b, v.Unix())
			b = binary.AppendUvarint(b, uint64(v.Nanosecond()))
		}
	}
	return b
}

// decodeValues reads the stored columns into row. A row written before a
// column was added has fewer columns; the rest are null. It accepts only
// what encodeValues writes.
func decodeValues(t *Table, b []byte, row Row) error {
	d := decoder{b: b, what: "row of table " + t.Name}
	cols := t.values()
	n := d.uvarint()
	if d.err == nil && n > uint64(len(cols)) {
		return d.fail("%d columns; the table has %d outside the key", n, len(cols))
	}
	nulls := d.take(uint64((n + 7) / 8))
	if d.err != nil {
		return d.err
	}
	if n%8 != 0 && nulls[len(nulls)-1]>>(n%8) != 0 {
		return d.fail("bits set past the last column")
	}
	for i, c := range cols {
		col := t.Columns[c]
		if uint64(i) >= n || nulls[i/8]&(1<<(i%8)) != 0 {
			if !col.Null {
				return d.fail("column %s is null and does not allow null", col.Name)
			}
			row[c] = nil
			continue
		}
		switch col.Type {
		case Int64:
			row[c] = d.varint()
		case Float64:
			row[c] = math.Float64frombits(d.uint64())
		case Bool:
			switch d.byte() {
			case 0:
				row[c] = false
			case 1:
				row[c] = true
			default:
				d.fail("column %s: bool is not 0 or 1", col.Name)
			}
		case String:
			s := d.take(d.uvarint())
			if d.err == nil && !utf8.Valid(s) {
				d.fail("column %s: a string that is not UTF-8", col.Name)
			}
			row[c] = string(s)
		case Bytes:
			row[c] = bytes.Clone(d.take(d.uvarint()))
		case Time:
			row[c] = d.time(d.varint(), d.uvarint())
		}
		if d.err != nil {
			return d.err
		}
	}
	return d.end()
}

// decoder reads stored content and keeps the first fault. After a fault,
// every read returns a zero value.
type decoder struct {
	b    []byte
	off  int
	what string
	err  error
}

func (d *decoder) fail(format string, a ...any) error {
	if d.err == nil {
		d.err = &damaged{d.what, fmt.Sprintf(format, a...)}
	}
	return d.err
}

func (d *decoder) take(n uint64) []byte {
	if d.err != nil {
		return nil
	}
	if n > uint64(len(d.b)-d.off) {
		d.fail("%d bytes at byte %d run past the end", n, d.off)
		return nil
	}
	s := d.b[d.off : d.off+int(n)]
	d.off += int(n)
	return s
}

func (d *decoder) byte() byte {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (d *decoder) uint64() uint64 {
	if b := d.take(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}
	return 0
}

func (d *decoder) uint32() uint32 {
	if b := d.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (d *decoder) int64FromKey() int64 { return int64(d.uint64() ^ signBit) }

// uvarint and varint accept only the shortest form, the one that
// encoding/binary writes.
func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, k := binary.Uvarint(d.b[d.off:])
	if k <= 0 || k != len(binary.AppendUvarint(nil, v)) {
		d.fail("number at byte %d does not decode in its shortest form", d.off)
		return 0
	}
	d.off += k
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, k := binary.Varint(d.b[d.off:])
	if k <= 0 || k != len(binary.AppendVarint(nil, v)) {
		d.fail("number at byte %d does not decode in its shortest form", d.off)
		return 0
	}
	d.off += k
	return v
}

func (d *decoder) str() string { return string(d.take(d.uvarint())) }

// count reads a count of items that each take at least size bytes.
func (d *decoder) count(size int) int {
	n := d.uvarint()
	if d.err == nil && n > uint64(len(d.b)-d.off)/uint64(size) {
		d.fail("%d items do not fit in the %d bytes left", n, len(d.b)-d.off)
		return 0
	}
	return int(n)
}

func (d *decoder) escaped() []byte {
	var out []byte
	for d.err == nil {
		c := d.byte()
		if d.err != nil {
			break
		}
		if c != 0 {
			out = append(out, c)
			continue
		}
		switch d.byte() {
		case 0xFF:
			out = append(out, 0)
		case 1:
			if out == nil {
				out = []byte{}
			}
			return out
		default:
			d.fail("0x00 at byte %d is not followed by 0xFF or 0x01", d.off-2)
		}
	}
	return nil
}

func (d *decoder) time(sec int64, nsec uint64) time.Time {
	if d.err == nil && nsec >= 1e9 {
		d.fail("%d nanoseconds", nsec)
	}
	if d.err != nil {
		return time.Time{}
	}
	return time.Unix(sec, int64(nsec)).UTC()
}

func (d *decoder) end() error {
	if d.err == nil && d.off != len(d.b) {
		d.fail("%d bytes after the end", len(d.b)-d.off)
	}
	return d.err
}
