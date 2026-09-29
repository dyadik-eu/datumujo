package table

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo/internal/btree"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

const pageSize = 1024

func openStore(t *testing.T, fs vfs.FS) *store.Store {
	t.Helper()
	s, err := store.Open(fs, "db", store.Options{PageSize: pageSize})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// begin starts a store transaction and a table Tx over it.
func begin(t *testing.T, s *store.Store) (*store.Tx, *Tx) {
	t.Helper()
	stx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := Begin(stx, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	return stx, tx
}

func view(t *testing.T, s *store.Store) (*store.Snapshot, *View) {
	t.Helper()
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	v, err := Open(snap, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	return snap, v
}

// sameValue compares two values of a row: floats by their bits, times by
// instant and in UTC, bytes by content.
func sameValue(a, b any) bool {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y) && y.Location() == time.UTC
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y) && (x == nil) == (y == nil)
	default:
		return a == b
	}
}

func sameRow(a, b Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameValue(a[i], b[i]) {
			return false
		}
	}
	return true
}

var allTypes = []Type{Int64, Float64, Bool, String, Bytes, Time}

// edges are values of each type at the edges of their range.
var edges = map[Type][]any{
	Int64:   {int64(math.MinInt64), int64(-1), int64(0), int64(1), int64(math.MaxInt64)},
	Float64: {math.Inf(-1), -math.MaxFloat64, -1.5, -math.SmallestNonzeroFloat64, 0.0, math.SmallestNonzeroFloat64, 2.25, math.MaxFloat64, math.Inf(1)},
	Bool:    {false, true},
	String:  {"", "\x00", "\x00\x00", "a", "a\x00", "a\x00b", "a\x01", "ab", "ÿ", "ü", "\U0010ffff"},
	Bytes:   {[]byte{}, []byte{0}, []byte{0, 0}, []byte{0, 1}, []byte{0, 0xff}, []byte{1}, []byte{0xff}, []byte{0xff, 0}},
	Time: {time.Time{}, time.Unix(-1, 999_999_999).UTC(), time.Unix(0, 0).UTC(), time.Unix(0, 1).UTC(),
		time.Date(2026, 9, 26, 12, 0, 0, 5, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC)},
}

// less is the order of values of one type that keys must keep.
func less(typ Type, a, b any) bool {
	switch typ {
	case Int64:
		return a.(int64) < b.(int64)
	case Float64:
		return a.(float64) < b.(float64)
	case Bool:
		return !a.(bool) && b.(bool)
	case String:
		return a.(string) < b.(string)
	case Bytes:
		return bytes.Compare(a.([]byte), b.([]byte)) < 0
	default:
		return a.(time.Time).Before(b.(time.Time))
	}
}

// TestRoundTrip stores the edge values of each type in the key and in a
// column outside the key. It reads them back by Get and Scan.
func TestRoundTrip(t *testing.T) {
	s := openStore(t, vfs.NewSim())
	defer s.Close()
	stx, tx := begin(t, s)
	for _, typ := range allTypes {
		name := typ.String()
		def := Def{Name: name, Columns: []Column{{Name: "k", Type: typ, Null: false}, {Name: "v", Type: typ, Null: true}}, Key: []string{"k"}}
		if err := tx.CreateTable(def); err != nil {
			t.Fatal(err)
		}
		for i, v := range edges[typ] {
			// The value of row i is the edge value before it, and null
			// for the first row.
			var val any
			if i > 0 {
				val = edges[typ][i-1]
			}
			if err := tx.Insert(name, Row{v, val}); err != nil {
				t.Fatalf("%s %v: %v", name, v, err)
			}
		}
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, v := view(t, s)
	defer snap.Close()
	for _, typ := range allTypes {
		name := typ.String()
		for i, key := range edges[typ] {
			var val any
			if i > 0 {
				val = edges[typ][i-1]
			}
			row, ok, err := v.Get(name, key)
			if err != nil || !ok || !sameRow(row, Row{key, val}) {
				t.Fatalf("%s: get %v: %v %v %v", name, key, row, ok, err)
			}
		}
		// Scan returns the rows in the order of the values.
		rows, err := v.Scan(name, Options{})
		if err != nil {
			t.Fatal(err)
		}
		var got []any
		for rows.Next() {
			got = append(got, rows.Row()[0])
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		want := append([]any(nil), edges[typ]...)
		sort.SliceStable(want, func(i, j int) bool { return less(typ, want[i], want[j]) })
		if len(got) != len(want) {
			t.Fatalf("%s: scan gave %d rows, want %d", name, len(got), len(want))
		}
		for i := range want {
			if !sameValue(want[i], got[i]) {
				t.Fatalf("%s: row %d is %v, want %v", name, i, got[i], want[i])
			}
		}
	}
}

// TestKeyOrder checks the key forms against the order of the values. It
// uses random pairs of each type, and keys over two columns. There, a
// string that starts another must not change the order of the second
// column.
func TestKeyOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	random := func(typ Type) any {
		if rng.Intn(3) == 0 {
			e := edges[typ]
			return e[rng.Intn(len(e))]
		}
		switch typ {
		case Int64:
			return int64(rng.Uint64())
		case Float64:
			f := math.Float64frombits(rng.Uint64())
			if math.IsNaN(f) {
				return 0.5
			}
			return f
		case Bool:
			return rng.Intn(2) == 1
		case String:
			return string(randBytes(rng, "a\x00\x01\xff"))
		case Bytes:
			return randBytes(rng, "a\x00\x01\xff")
		default:
			return time.Unix(rng.Int63n(1<<40)-1<<39, rng.Int63n(1e9)).UTC()
		}
	}
	cmp := func(typ Type, a, b any) int {
		switch {
		case less(typ, a, b):
			return -1
		case less(typ, b, a):
			return 1
		}
		return 0
	}
	for _, typ := range allTypes {
		for i := 0; i < 5000; i++ {
			a, b := random(typ), random(typ)
			if typ == String {
				a, b = strings.ToValidUTF8(a.(string), "?"), strings.ToValidUTF8(b.(string), "?")
			}
			want := cmp(typ, a, b)
			if got := bytes.Compare(appendKey(nil, typ, a), appendKey(nil, typ, b)); got != want {
				t.Fatalf("%v: %q against %q: keys compare %d, values %d", typ, a, b, got, want)
			}
			// Two columns: the second decides only when the first is
			// equal.
			x, y := random(Int64), random(Int64)
			ka := appendKey(appendKey(nil, typ, a), Int64, x)
			kb := appendKey(appendKey(nil, typ, b), Int64, y)
			if want == 0 {
				want = cmp(Int64, x, y)
			}
			if got := bytes.Compare(ka, kb); got != want {
				t.Fatalf("%v: (%q, %d) against (%q, %d): keys compare %d, want %d", typ, a, x, b, y, got, want)
			}
		}
	}
	// -0 and +0 are one key.
	if !bytes.Equal(appendKey(nil, Float64, math.Copysign(0, -1)), appendKey(nil, Float64, 0.0)) {
		t.Fatal("-0 and +0 have different keys")
	}
}

func randBytes(rng *rand.Rand, alphabet string) []byte {
	b := make([]byte, rng.Intn(6))
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return b
}

// TestDefsRejected checks each rule of a table definition.
func TestDefsRejected(t *testing.T) {
	ok := func() Def {
		return Def{Name: "t", Columns: []Column{{Name: "a", Type: Int64, Null: false}, {Name: "b", Type: String, Null: true}}, Key: []string{"a"}}
	}
	cases := map[string]func(d *Def){
		"empty table name":     func(d *Def) { d.Name = "" },
		"long table name":      func(d *Def) { d.Name = strings.Repeat("x", MaxName+1) },
		"table name not UTF-8": func(d *Def) { d.Name = "\xff" },
		"no columns":           func(d *Def) { d.Columns = nil; d.Key = nil },
		"too many columns": func(d *Def) {
			for i := 0; i < MaxColumns; i++ {
				d.Columns = append(d.Columns, Column{Name: fmt.Sprint("c", i), Type: Int64, Null: true})
			}
		},
		"empty column name":    func(d *Def) { d.Columns[1].Name = "" },
		"column twice":         func(d *Def) { d.Columns[1].Name = "a" },
		"unknown type":         func(d *Def) { d.Columns[1].Type = 7 },
		"type zero":            func(d *Def) { d.Columns[1].Type = 0 },
		"no key":               func(d *Def) { d.Key = nil },
		"key column missing":   func(d *Def) { d.Key = []string{"z"} },
		"key column twice":     func(d *Def) { d.Key = []string{"a", "a"} },
		"key column with null": func(d *Def) { d.Key = []string{"b"} },
		"default not UTF-8":    func(d *Def) { d.Columns[1].Default = "\xff" },
		"empty check":          func(d *Def) { d.Checks = []string{"a > 0", ""} },
		"check not UTF-8":      func(d *Def) { d.Checks = []string{"\xff"} },
		"fill on a new column": func(d *Def) { d.Columns[1].Fill = "x" },
	}
	s := openStore(t, vfs.NewSim())
	defer s.Close()
	stx, tx := begin(t, s)
	defer stx.Rollback()
	if err := tx.CreateTable(ok()); err != nil {
		t.Fatalf("the valid definition: %v", err)
	}
	for name, change := range cases {
		d := ok()
		d.Name = "u"
		change(&d)
		if err := tx.CreateTable(d); !errors.Is(err, ErrSchema) {
			t.Errorf("%s: %v, want ErrSchema", name, err)
		}
	}
	// A missing key column is named in the error.
	d := ok()
	d.Key = []string{"zz"}
	if err := tx.CreateTable(d); err == nil || !strings.Contains(err.Error(), "zz does not exist") {
		t.Errorf("missing key column: %v", err)
	}
	if err := tx.CreateTable(ok()); !errors.Is(err, ErrSchema) {
		t.Errorf("a table that exists: %v", err)
	}
	if err := tx.AddColumn("t", Column{Name: "c", Type: Int64, Null: false}); !errors.Is(err, ErrSchema) {
		t.Errorf("added column without null: %v", err)
	}
	if err := tx.AddColumn("t", Column{Name: "b", Type: Int64, Null: true}); !errors.Is(err, ErrSchema) {
		t.Errorf("added column with a name that exists: %v", err)
	}
	if err := tx.AddColumn("nope", Column{Name: "c", Type: Int64, Null: true}); !errors.Is(err, ErrNoTable) {
		t.Errorf("added column to no table: %v", err)
	}
	if err := tx.AddColumn("t", Column{Name: "c", Type: Int64, Fill: "zero"}); !errors.Is(err, ErrSchema) {
		t.Errorf("added column with a fill of another type: %v", err)
	}
	if got := tx.Schema().Version; got != 1 {
		t.Errorf("schema version %d after one valid change, want 1", got)
	}
	if len(tx.Schema().Tables) != 1 || len(tx.Schema().Tables[0].Columns) != 2 {
		t.Errorf("rejected changes changed the schema: %+v", tx.Schema())
	}
}

// TestRowsRejected checks each rule of a row and of a key, and that a
// rejected write leaves the table as it was.
func TestRowsRejected(t *testing.T) {
	s := openStore(t, vfs.NewSim())
	defer s.Close()
	stx, tx := begin(t, s)
	defer stx.Rollback()
	def := Def{Name: "t", Columns: []Column{{Name: "f", Type: Float64, Null: false}, {Name: "s", Type: String, Null: false}, {Name: "n", Type: Int64, Null: true}}, Key: []string{"f"}}
	if err := tx.CreateTable(def); err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("t", Row{1.0, "x", nil}); err != nil {
		t.Fatal(err)
	}
	bad := map[string]Row{
		"too few values":    {2.0, "x"},
		"too many values":   {2.0, "x", nil, nil},
		"int for int64":     {2.0, "x", 5},
		"null without null": {2.0, nil, nil},
		"string not UTF-8":  {2.0, "\xff", nil},
		"NaN in the key":    {math.NaN(), "x", nil},
		"string for float":  {"2", "x", nil},
		"bytes for string":  {2.0, []byte("x"), nil},
	}
	for name, row := range bad {
		if err := tx.Insert("t", row); !errors.Is(err, ErrValue) {
			t.Errorf("insert, %s: %v, want ErrValue", name, err)
		}
	}
	if err := tx.Insert("t", Row{1.0, "y", nil}); !errors.Is(err, ErrExists) {
		t.Errorf("insert of a key that exists: %v", err)
	}
	if err := tx.Insert("t", Row{math.Copysign(0, -1), "zero", nil}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("t", Row{0.0, "zero", nil}); !errors.Is(err, ErrExists) {
		t.Errorf("insert of +0 after -0: %v", err)
	}
	if err := tx.Update("t", Row{3.0, "y", nil}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update of a key that does not exist: %v", err)
	}
	if err := tx.Insert("nope", Row{1.0}); !errors.Is(err, ErrNoTable) {
		t.Errorf("insert into no table: %v", err)
	}
	if _, _, err := tx.Get("t"); !errors.Is(err, ErrValue) {
		t.Errorf("get without key values: %v", err)
	}
	if _, _, err := tx.Get("t", math.NaN()); !errors.Is(err, ErrValue) {
		t.Errorf("get of NaN: %v", err)
	}
	if _, err := tx.Delete("t", "1"); !errors.Is(err, ErrValue) {
		t.Errorf("delete with a string key: %v", err)
	}
	rows, _ := tx.Scan("t", Options{})
	var got []string
	for rows.Next() {
		got = append(got, fmt.Sprint(rows.Row()))
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if want := "[[0 zero <nil>] [1 x <nil>]]"; fmt.Sprint(got) != want {
		t.Errorf("the table holds %v, want %v", got, want)
	}
}

// TestSchemaInTransaction checks that a schema change is part of the
// transaction: a rollback removes it, a commit keeps it after reopen.
func TestSchemaInTransaction(t *testing.T) {
	fs := vfs.NewSim()
	s := openStore(t, fs)
	stx, tx := begin(t, s)
	def := Def{Name: "t", Columns: []Column{{Name: "a", Type: Int64, Null: false}}, Key: []string{"a"}}
	if err := tx.CreateTable(def); err != nil {
		t.Fatal(err)
	}
	stx.Rollback()
	snap, v := view(t, s)
	if v.Schema().Version != 0 || len(v.Schema().Tables) != 0 {
		t.Fatalf("after rollback: %+v", v.Schema())
	}
	snap.Close()

	stx, tx = begin(t, s)
	if err := tx.CreateTable(def); err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("t", Row{int64(7)}); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	stx, tx = begin(t, s)
	if err := tx.AddColumn("t", Column{Name: "b", Type: String, Null: true}); err != nil {
		t.Fatal(err)
	}
	// A snapshot from before the commit keeps the old schema.
	snap, old := view(t, s)
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := len(old.Schema().Tables[0].Columns); n != 1 {
		t.Fatalf("the old snapshot sees %d columns", n)
	}
	snap.Close()
	s.Close()

	s = openStore(t, fs)
	defer s.Close()
	snap, v = view(t, s)
	defer snap.Close()
	if v.Schema().Version != 2 {
		t.Fatalf("version %d after reopen, want 2", v.Schema().Version)
	}
	// The row written before the column was added reads it as null.
	row, ok, err := v.Get("t", int64(7))
	if err != nil || !ok || !sameRow(row, Row{int64(7), nil}) {
		t.Fatalf("old row: %v %v %v", row, ok, err)
	}
}

// TestFormatVersion checks requirement L-19. A file stays at format
// version 1, which v0.2.0 reads, until the first default or check. Its
// commit raises the file to version 2; a rollback does not. The version
// never goes back, and the defaults and checks survive a reopen.
func TestFormatVersion(t *testing.T) {
	fs := vfs.NewSim()
	s := openStore(t, fs)
	plain := Def{Name: "p", Columns: []Column{{Name: "a", Type: Int64}}, Key: []string{"a"}}
	checked := Def{
		Name:    "c",
		Columns: []Column{{Name: "a", Type: Int64}, {Name: "b", Type: String, Null: true, Default: "'none'"}},
		Key:     []string{"a"},
		Checks:  []string{"a > 0", "length(b) < 10"},
	}
	version := func(want uint32, when string) {
		t.Helper()
		snap, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		defer snap.Close()
		if snap.Version() != want {
			t.Errorf("%s: format version %d, want %d", when, snap.Version(), want)
		}
	}

	stx, tx := begin(t, s)
	if err := tx.CreateTable(plain); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddColumn("p", Column{Name: "n", Type: String, Null: true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateIndex("p", IndexDef{Name: "by_n", Columns: []string{"n"}}); err != nil {
		t.Fatal(err)
	}
	stx.Commit()
	version(1, "a table, a column and an index")

	// A default alone raises the version, and so does a check alone.
	for _, d := range []Def{
		{Name: "d", Columns: []Column{{Name: "a", Type: Int64, Default: "1"}}, Key: []string{"a"}},
		{Name: "k", Columns: []Column{{Name: "a", Type: Int64}}, Key: []string{"a"}, Checks: []string{"a > 0"}},
	} {
		stx, tx = begin(t, s)
		if err := tx.CreateTable(d); err != nil {
			t.Fatal(err)
		}
		if stx.Version() != 2 {
			t.Errorf("table %s: version %d in the transaction", d.Name, stx.Version())
		}
		stx.Rollback()
	}
	version(1, "two rolled back tables")

	stx, tx = begin(t, s)
	sp := tx.Savepoint()
	if err := tx.CreateTable(checked); err != nil {
		t.Fatal(err)
	}
	tx.RollbackTo(sp)
	if stx.Version() != 1 {
		t.Errorf("version %d after a rollback to a savepoint", stx.Version())
	}
	if err := tx.CreateTable(checked); err != nil {
		t.Fatal(err)
	}
	stx.Commit()
	version(2, "a table with a default and checks")

	stx, tx = begin(t, s)
	if err := tx.DropTable("c"); err != nil {
		t.Fatal(err)
	}
	stx.Commit()
	version(2, "the last default and check dropped")
	stx, tx = begin(t, s)
	if err := tx.CreateTable(checked); err != nil {
		t.Fatal(err)
	}
	stx.Commit()
	s.Close()

	s = openStore(t, fs.Crash(nil))
	defer s.Close()
	version(2, "after a reopen")
	snap, v := view(t, s)
	defer snap.Close()
	got, _ := v.Schema().Table("c")
	if got.Columns[1].Default != "'none'" || got.Columns[0].Default != "" || strings.Join(got.Checks, ";") != "a > 0;length(b) < 10" {
		t.Errorf("after a reopen: columns %+v, checks %q", got.Columns, got.Checks)
	}
	if p, _ := v.Schema().Table("p"); len(p.Checks) != 0 || p.Columns[1].Default != "" {
		t.Errorf("table p: %+v", p)
	}
}

// TestFill adds a NOT NULL column of each type with a fill. The rows
// written before read the fill, the rows written after keep their own
// value, and both hold after a reopen. A fill raises the file to
// format version 2, since v0.2.0 would read it as null.
func TestFill(t *testing.T) {
	fs := vfs.NewSim()
	s := openStore(t, fs)
	when := time.Date(2026, 9, 29, 12, 0, 0, 5, time.UTC)
	fills := []Column{
		{Name: "i", Type: Int64, Fill: int64(-7)},
		{Name: "f", Type: Float64, Fill: math.Copysign(0, -1)},
		{Name: "b", Type: Bool, Fill: true},
		{Name: "s", Type: String, Fill: "ä"},
		{Name: "y", Type: Bytes, Fill: []byte{0, 1}},
		{Name: "m", Type: Time, Fill: when},
		{Name: "n", Type: Int64, Null: true},
	}
	stx, tx := begin(t, s)
	if err := tx.CreateTable(Def{Name: "t", Columns: []Column{{Name: "k", Type: Int64}}, Key: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	for k := int64(1); k <= 2; k++ {
		if err := tx.Insert("t", Row{k}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range fills {
		if err := tx.AddColumn("t", c); err != nil {
			t.Fatalf("column %s: %v", c.Name, err)
		}
	}
	if stx.Version() != 2 {
		t.Errorf("format version %d after a fill", stx.Version())
	}
	own := Row{int64(3), int64(1), 2.5, false, "o", []byte{9}, when.Add(time.Hour), int64(4)}
	if err := tx.Insert("t", own); err != nil {
		t.Fatal(err)
	}
	stx.Commit()
	s.Close()

	s = openStore(t, fs.Crash(nil))
	defer s.Close()
	snap, v := view(t, s)
	defer snap.Close()
	want := Row{int64(1), int64(-7), math.Copysign(0, -1), true, "ä", []byte{0, 1}, when, nil}
	row, ok, err := v.Get("t", int64(1))
	if err != nil || !ok || !sameRow(row, want) || !math.Signbit(row[2].(float64)) {
		t.Fatalf("an old row: %v %v %v", row, ok, err)
	}
	// A caller that changes the row does not change the fill.
	row[5].([]byte)[0] = 99
	if again, _, _ := v.Get("t", int64(2)); again[5].([]byte)[0] != 0 {
		t.Errorf("the fill changed through a row: %v", again[5])
	}
	if row, _, _ := v.Get("t", int64(3)); !sameRow(row, own) {
		t.Errorf("a new row: %v", row)
	}

	// A unique index over the filled column sees the fill: the two old
	// rows have the same value.
	stx, tx = begin(t, s)
	defer stx.Rollback()
	if err := tx.CreateIndex("t", IndexDef{Name: "u", Columns: []string{"s"}, Unique: true}); !errors.Is(err, ErrUnique) {
		t.Errorf("a unique index over a fill that two rows read: %v", err)
	}
	if err := tx.CreateIndex("t", IndexDef{Name: "by_i", Columns: []string{"i"}}); err != nil {
		t.Fatal(err)
	}
	n := 0
	rows, err := tx.Scan("t", Options{Index: "by_i", From: []any{int64(-7)}, To: []any{int64(-7)}, ToInclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil || n != 2 {
		t.Errorf("the index finds %d rows with the fill, want 2: %v", n, err)
	}
}

// TestExtendedSchemaInVersion1 opens a file of format version 1 whose
// schema holds a check. The code never writes one, so it is damage.
func TestExtendedSchemaInVersion1(t *testing.T) {
	fs := vfs.NewSim()
	s := openStore(t, fs)
	defer s.Close()
	stx, tx := begin(t, s)
	if err := tx.CreateTable(Def{Name: "p", Columns: []Column{{Name: "a", Type: Int64}}, Key: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	// Write a schema with a check past saveSchema, which would raise the
	// version.
	cat, err := tx.catalog()
	if err != nil {
		t.Fatal(err)
	}
	ext := tx.copySchema()
	ext.Tables[0].Checks = []string{"a > 0"}
	if err := cat.Put(stx, schemaKey, encodeSchema(ext)); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if _, err := Open(snap, pageSize); !errors.Is(err, ErrDamaged) {
		t.Errorf("a check in a file of version 1: %v", err)
	}
}

// TestAgainstAMap runs random inserts, updates, deletes and gets on two
// tables against a map as oracle. Batches commit or roll back, a column is
// added on the way, and the store is reopened at the end.
func TestAgainstAMap(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		rng := rand.New(rand.NewSource(seed))
		fs := vfs.NewSim()
		s := openStore(t, fs)
		defs := []Def{
			{Name: "a", Columns: []Column{{Name: "s", Type: String, Null: false}, {Name: "n", Type: Int64, Null: false}, {Name: "f", Type: Float64, Null: true}, {Name: "b", Type: Bytes, Null: true}, {Name: "t", Type: Time, Null: true}, {Name: "ok", Type: Bool, Null: false}}, Key: []string{"s", "n"}},
			{Name: "b", Columns: []Column{{Name: "id", Type: Int64, Null: false}, {Name: "text", Type: String, Null: true}}, Key: []string{"id"}},
		}
		oracle := map[string]map[string]Row{"a": {}, "b": {}}
		copyOracle := func() map[string]map[string]Row {
			out := map[string]map[string]Row{}
			for n, m := range oracle {
				out[n] = map[string]Row{}
				for k, r := range m {
					out[n][k] = r
				}
			}
			return out
		}
		stx, tx := begin(t, s)
		for _, d := range defs {
			if err := tx.CreateTable(d); err != nil {
				t.Fatal(err)
			}
		}
		if err := stx.Commit(); err != nil {
			t.Fatal(err)
		}
		added := false
		randRow := func(name string) Row {
			if name == "b" {
				var text any
				if rng.Intn(3) > 0 {
					text = strings.Repeat("x", rng.Intn(3*pageSize))
				}
				return Row{int64(rng.Intn(40)), text}
			}
			row := Row{string(rune('a' + rng.Intn(4))), int64(rng.Intn(10) - 5), nil, nil, nil, rng.Intn(2) == 0}
			if rng.Intn(2) == 0 {
				row[2] = math.Float64frombits(rng.Uint64())
			}
			if rng.Intn(2) == 0 {
				row[3] = randBytes(rng, "\x00\x01xy")
			}
			if rng.Intn(2) == 0 {
				row[4] = time.Unix(rng.Int63n(1e10), rng.Int63n(1e9)).UTC()
			}
			if added {
				row = append(row, nil)
				if rng.Intn(2) == 0 {
					row[6] = int64(rng.Intn(100))
				}
			}
			return row
		}
		for batch := 0; batch < 40; batch++ {
			before := copyOracle()
			stx, tx := begin(t, s)
			if batch == 20 {
				if err := tx.AddColumn("a", Column{Name: "extra", Type: Int64, Null: true}); err != nil {
					t.Fatal(err)
				}
				for k, r := range oracle["a"] {
					oracle["a"][k] = append(append(Row(nil), r...), nil)
				}
				added = true
			}
			for op := 0; op < 25; op++ {
				name := []string{"a", "b"}[rng.Intn(2)]
				tb, _ := tx.Schema().Table(name)
				row := randRow(name)
				key := string(encodeKey(tb, row))
				_, exists := oracle[name][key]
				keyVals := []any{}
				for _, k := range tb.Key {
					keyVals = append(keyVals, row[k])
				}
				switch rng.Intn(4) {
				case 0:
					err := tx.Insert(name, row)
					if exists != errors.Is(err, ErrExists) || (!exists && err != nil) {
						t.Fatalf("seed %d: insert, exists %v: %v", seed, exists, err)
					}
					if !exists {
						oracle[name][key] = row
					}
				case 1:
					err := tx.Update(name, row)
					if exists == errors.Is(err, ErrNotFound) || (exists && err != nil) {
						t.Fatalf("seed %d: update, exists %v: %v", seed, exists, err)
					}
					if exists {
						oracle[name][key] = row
					}
				case 2:
					found, err := tx.Delete(name, keyVals...)
					if err != nil || found != exists {
						t.Fatalf("seed %d: delete, exists %v: %v %v", seed, exists, found, err)
					}
					delete(oracle[name], key)
				default:
					got, ok, err := tx.Get(name, keyVals...)
					if err != nil || ok != exists || exists && !sameRow(got, oracle[name][key]) {
						t.Fatalf("seed %d: get: %v %v %v, want %v", seed, got, ok, err, oracle[name][key])
					}
				}
			}
			if rng.Intn(4) == 0 {
				stx.Rollback()
				oracle = before
				if batch == 20 {
					added = false
				}
				continue
			}
			if err := stx.Commit(); err != nil {
				t.Fatal(err)
			}
			compareAll(t, s, oracle)
		}
		if !added {
			t.Fatalf("seed %d: the batch that adds the column rolled back; the test did not cover old rows", seed)
		}
		s.Close()
		s = openStore(t, fs)
		compareAll(t, s, oracle)
		s.Close()
	}
}

// compareAll scans every table of the oracle and checks each tree.
func compareAll(t *testing.T, s *store.Store, oracle map[string]map[string]Row) {
	t.Helper()
	snap, v := view(t, s)
	defer snap.Close()
	for name, m := range oracle {
		tb, _ := v.Schema().Table(name)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rows, err := v.Scan(name, Options{})
		if err != nil {
			t.Fatal(err)
		}
		i := 0
		for rows.Next() {
			if i >= len(keys) || !sameRow(rows.Row(), m[keys[i]]) {
				t.Fatalf("table %s, row %d: %v", name, i, rows.Row())
			}
			i++
		}
		if rows.Err() != nil || i != len(keys) {
			t.Fatalf("table %s: %d rows, want %d: %v", name, i, len(keys), rows.Err())
		}
		st, err := btree.Open(tb.Root, pageSize).Check(snap)
		if err != nil || st.Entries != len(keys) {
			t.Fatalf("table %s: check: %v, %d entries", name, err, st.Entries)
		}
	}
}

// TestDamagedRowIsAnError writes rows that this code does not write
// straight into the tree of a table. Get and Scan return ErrDamaged.
func TestDamagedRowIsAnError(t *testing.T) {
	s := openStore(t, vfs.NewSim())
	defer s.Close()
	stx, tx := begin(t, s)
	defer stx.Rollback()
	def := Def{Name: "t", Columns: []Column{{Name: "k", Type: Int64, Null: false}, {Name: "v", Type: Int64, Null: false}}, Key: []string{"k"}}
	if err := tx.CreateTable(def); err != nil {
		t.Fatal(err)
	}
	tb, _ := tx.Schema().Table("t")
	tree := btree.Open(tb.Root, pageSize)
	key := appendKey(nil, Int64, int64(1))
	for name, value := range map[string][]byte{
		"null in a column without null": {1, 1},
		"more columns than the table":   {2, 0, 2, 2},
		"a byte after the end":          {1, 0, 2, 9},
		"no value":                      {1, 0},
		"long form of a number":         {1, 0, 0x82, 0x00},
	} {
		if err := tree.Put(stx, key, value); err != nil {
			t.Fatal(err)
		}
		if _, _, err := tx.Get("t", int64(1)); !errors.Is(err, ErrDamaged) {
			t.Errorf("%s: get: %v", name, err)
		}
		rows, _ := tx.Scan("t", Options{})
		if rows.Next() || !errors.Is(rows.Err(), ErrDamaged) {
			t.Errorf("%s: scan: %v", name, rows.Err())
		}
	}
	// A key this code does not write: an int64 key of 7 bytes.
	if err := tree.Put(stx, key[:7], []byte{1, 0, 2}); err != nil {
		t.Fatal(err)
	}
	rows, _ := tx.Scan("t", Options{})
	if rows.Next() || !errors.Is(rows.Err(), ErrDamaged) {
		t.Errorf("short key: scan: %v", rows.Err())
	}
	// The catalog without a schema.
	cat := btree.Open(stx.Root(CatalogSlot), pageSize)
	if _, err := cat.Delete(stx, schemaKey); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(stx, pageSize); !errors.Is(err, ErrDamaged) || !strings.Contains(err.Error(), "holds no schema") {
		t.Errorf("catalog without schema: %v", err)
	}
}

// fuzzTable has every type in the key and outside it.
var fuzzTable = func() *Table {
	var cols []Column
	var key []int
	for i, typ := range allTypes {
		cols = append(cols, Column{Name: fmt.Sprint("k", i), Type: typ, Null: false})
		key = append(key, i)
	}
	for i, typ := range allTypes {
		cols = append(cols, Column{Name: fmt.Sprint("v", i), Type: typ, Null: true}, Column{Name: fmt.Sprint("w", i), Type: typ, Null: false})
	}
	t := &Table{Name: "f", Columns: cols, Key: key, Root: 1, Indexes: []Index{
		{Name: "i", Columns: []int{6, 7, 8, 9, 10, 11, 12, 13}, Root: 2},
		{Name: "u", Columns: []int{0, 7}, Unique: true, Root: 3},
	}}
	if err := t.check(); err != nil {
		panic(err)
	}
	return t
}()

func fuzzRow() Row {
	row := Row{int64(-3), -2.5, true, "a\x00b", []byte{0, 0xff}, time.Unix(5, 6).UTC()}
	for _, typ := range allTypes {
		e := edges[typ]
		row = append(row, nil, e[len(e)-1])
	}
	return row
}

// FuzzDecodeKey checks that decodeKey returns an error and no panic for
// any input, and that a key it accepts encodes to the same bytes.
func FuzzDecodeKey(f *testing.F) {
	f.Add(encodeKey(fuzzTable, fuzzRow()))
	f.Fuzz(func(t *testing.T, b []byte) {
		row := make(Row, len(fuzzTable.Columns))
		if decodeKey(fuzzTable, b, row) != nil {
			return
		}
		if again := encodeKey(fuzzTable, row); !bytes.Equal(again, b) {
			t.Fatalf("decoded %x, encodes to %x", b, again)
		}
	})
}

// FuzzDecodeValues does the same for the stored columns. A row with fewer
// columns than the table encodes with all of them, so the check of the
// bytes applies to full rows.
func FuzzDecodeValues(f *testing.F) {
	f.Add(encodeValues(fuzzTable, fuzzRow()))
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, b []byte) {
		row := make(Row, len(fuzzTable.Columns))
		if decodeValues(fuzzTable, b, row) != nil {
			return
		}
		if len(b) > 0 && int(b[0]) == len(fuzzTable.values()) {
			if again := encodeValues(fuzzTable, row); !bytes.Equal(again, b) {
				t.Fatalf("decoded %x, encodes to %x", b, again)
			}
		}
	})
}

// FuzzDecodeSchema does the same for the schema.
func FuzzDecodeSchema(f *testing.F) {
	f.Add(encodeSchema(&Schema{Version: 3, Tables: []Table{*fuzzTable, {Name: "g", Columns: []Column{{Name: "a", Type: String, Null: false}}, Key: []int{0}, Root: 9}}}))
	f.Add(encodeSchema(extendedSchema))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := decodeSchema(b)
		if err != nil {
			return
		}
		if again := encodeSchema(s); !bytes.Equal(again, b) {
			t.Fatalf("decoded %x, encodes to %x", b, again)
		}
	})
}

// TestCrashWithTables is the crash test of P-1 with tables. It runs a
// schema change and batches of inserts, updates and deletes. It stops
// the run after every call, then simulates 64 power losses each. The file opens with the schema and the
// rows of the acked batches, or of those plus the one in flight.
func TestCrashWithTables(t *testing.T) {
	// The index is on g, not on v: v holds values of a page and more, too
	// long for a key.
	def := Def{Name: "t", Columns: []Column{{Name: "k", Type: Int64, Null: false}, {Name: "v", Type: String, Null: true}, {Name: "g", Type: Int64, Null: true}}, Key: []string{"k"}}
	type step struct {
		puts map[int64]string
		dels []int64
		add  bool
	}
	rng := rand.New(rand.NewSource(5))
	var steps []step
	for i := 0; i < 10; i++ {
		st := step{puts: map[int64]string{}, add: i == 5}
		n := 1
		if i%2 == 0 {
			n = 8
		}
		for j := 0; j < n; j++ {
			k := int64(rng.Intn(30))
			if rng.Intn(4) == 0 {
				st.dels = append(st.dels, k)
			} else {
				st.puts[k] = fmt.Sprintf("s%d-%s", i, strings.Repeat("v", rng.Intn(2)*pageSize))
			}
		}
		steps = append(steps, st)
	}
	type state struct {
		version uint64
		counter uint64
		rows    map[int64]string
	}
	apply := func(s state, st step) state {
		out := state{version: s.version, counter: s.counter + 1, rows: map[int64]string{}}
		for k, v := range s.rows {
			out.rows[k] = v
		}
		for _, k := range st.dels {
			delete(out.rows, k)
		}
		for k, v := range st.puts {
			out.rows[k] = v
		}
		if st.add {
			out.version++
		}
		return out
	}
	final := state{version: 2, rows: map[int64]string{}}
	for _, st := range steps {
		final = apply(final, st)
	}
	run := func(fs vfs.FS) (acked, running state) {
		s, err := store.Open(fs, "db", store.Options{PageSize: pageSize})
		if err != nil {
			return
		}
		stx, err := s.Begin()
		if err != nil {
			return
		}
		running = state{version: 2, rows: map[int64]string{}}
		tx, err := Begin(stx, pageSize)
		if err != nil || tx.CreateTable(def) != nil || tx.CreateIndex("t", IndexDef{Name: "by_g", Columns: []string{"g"}}) != nil || stx.Commit() != nil {
			return
		}
		acked = running
		for i, st := range steps {
			running = apply(acked, st)
			stx, err := s.Begin()
			if err != nil {
				return
			}
			tx, err := Begin(stx, pageSize)
			if err != nil {
				return
			}
			if st.add && tx.AddColumn("t", Column{Name: "n", Type: Int64, Null: true}) != nil {
				return
			}
			if _, err := tx.Next("steps"); err != nil {
				return
			}
			width := len(tx.Schema().Tables[0].Columns)
			for _, k := range st.dels {
				if _, err := tx.Delete("t", k); err != nil {
					return
				}
			}
			// In key order: map order changes from run to run, and the
			// stopped runs must make the same calls as the full one.
			keys := make([]int64, 0, len(st.puts))
			for k := range st.puts {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			for _, k := range keys {
				row := make(Row, width)
				row[0], row[1], row[2] = k, st.puts[k], k%3
				err := tx.Insert("t", row)
				if errors.Is(err, ErrExists) {
					err = tx.Update("t", row)
				}
				if err != nil {
					return
				}
			}
			if stx.Commit() != nil {
				return
			}
			acked = running
			if i%3 == 2 {
				if _, err := s.Checkpoint(); err != nil {
					return
				}
			}
		}
		return
	}
	// A power loss keeps each unsynced write with chance 1/2 and each of
	// its sectors with chance 1/2. A short commit writes two pages of three
	// sectors each, so it survives whole in about 1 of 256 losses. With 8
	// losses per stop, none did.
	const powers = 64
	full := vfs.NewSim()
	if last, _ := run(full); fmt.Sprint(last) != fmt.Sprint(final) {
		t.Fatalf("the run without a stop ends at version %d with %d rows; the steps give %d with %d", last.version, len(last.rows), final.version, len(final.rows))
	}
	calls := full.Calls()
	toAcked, toRunning := 0, 0
	for k := 0; k <= calls; k++ {
		fs := vfs.NewSim()
		fs.SetBudget(k)
		acked, running := run(fs)
		for power := int64(0); power < powers; power++ {
			var r *rand.Rand
			if power > 0 {
				r = rand.New(rand.NewSource(int64(k)*1_013 + power))
			}
			after := fs.Crash(r)
			s, err := store.Open(after, "db", store.Options{PageSize: pageSize})
			if err != nil {
				t.Fatalf("k %d, power %d: open: %v", k, power, err)
			}
			snap, _ := s.Snapshot()
			v, err := Open(snap, pageSize)
			if err != nil {
				t.Fatalf("k %d, power %d: schema: %v", k, power, err)
			}
			got := state{version: v.Schema().Version, rows: map[int64]string{}}
			if got.counter, err = v.Counter("steps"); err != nil {
				t.Fatalf("k %d, power %d: counter: %v", k, power, err)
			}
			if got.version > 0 {
				// The index holds exactly the rows of the table.
				byV := map[int64]string{}
				rows, err := v.Scan("t", Options{Index: "by_g"})
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					byV[rows.Row()[0].(int64)] = rows.Row()[1].(string)
				}
				if rows.Err() != nil {
					t.Fatalf("k %d, power %d: index scan: %v", k, power, rows.Err())
				}
				if rows, err = v.Scan("t", Options{}); err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					got.rows[rows.Row()[0].(int64)] = rows.Row()[1].(string)
				}
				if rows.Err() != nil {
					t.Fatalf("k %d, power %d: scan: %v", k, power, rows.Err())
				}
				if fmt.Sprint(byV) != fmt.Sprint(got.rows) {
					t.Fatalf("k %d, power %d: the index holds %d rows, the table %d", k, power, len(byV), len(got.rows))
				}
			}
			snap.Close()
			s.Close()
			switch fmt.Sprint(got) {
			case fmt.Sprint(acked):
				toAcked++
			case fmt.Sprint(running):
				toRunning++
			default:
				t.Fatalf("k %d, power %d: version %d, counter %d, %d rows; acked %+v, running %+v",
					k, power, got.version, got.counter, len(got.rows), acked, running)
			}
		}
	}
	// Without both kinds, the test did not show what it claims.
	if toAcked == 0 || toRunning == 0 {
		t.Fatalf("%d stops: %d opened at the acked state, %d at the one in flight", calls, toAcked, toRunning)
	}
	t.Logf("%d stops, %d power losses each: %d acked, %d in flight", calls+1, powers, toAcked, toRunning)
}

// extendedSchema has a default, a check, and a table with neither, so
// its encoding is format 3.
var extendedSchema = &Schema{Version: 5, Tables: []Table{
	{Name: "e", Columns: []Column{{Name: "a", Type: Int64}, {Name: "b", Type: Time, Null: true, Default: "CURRENT_TIMESTAMP"}}, Key: []int{0}, Root: 3, Checks: []string{"a > 0", "a < 100"}},
	{Name: "f", Columns: []Column{{Name: "a", Type: String}, {Name: "b", Type: Time, Fill: time.Unix(5, 6).UTC()}}, Key: []int{0}, Root: 4},
}}

// TestFuzzSeedsDecode checks that the seeds of the fuzz targets decode.
// Otherwise the check that a decoded input encodes to the same bytes
// would start from nothing.
func TestFuzzSeedsDecode(t *testing.T) {
	row := make(Row, len(fuzzTable.Columns))
	if err := decodeKey(fuzzTable, encodeKey(fuzzTable, fuzzRow()), row); err != nil {
		t.Errorf("key: %v", err)
	}
	if err := decodeValues(fuzzTable, encodeValues(fuzzTable, fuzzRow()), row); err != nil {
		t.Errorf("values: %v", err)
	}
	if !sameRow(row, fuzzRow()) {
		t.Errorf("the seed row comes back as %v", row)
	}
	s, err := decodeSchema(encodeSchema(&Schema{Version: 3, Tables: []Table{*fuzzTable}}))
	if err != nil || len(s.Tables[0].Indexes) != 2 {
		t.Errorf("schema: %v", err)
	}
	enc := encodeSchema(extendedSchema)
	if s, err := decodeSchema(enc); err != nil || enc[0] != schemaFormat3 || !bytes.Equal(encodeSchema(s), enc) {
		t.Errorf("the extended schema: format %d, %v", enc[0], err)
	}
	ix := &fuzzTable.Indexes[0]
	if _, err := decodeIndexKey(fuzzTable, ix, indexKey(fuzzTable, ix, fuzzRow()), make(Row, len(fuzzTable.Columns))); err != nil {
		t.Errorf("index key: %v", err)
	}
}

// TestDecodersReject gives each decoder one input for each rule it
// checks. Each must return ErrDamaged.
func TestDecodersReject(t *testing.T) {
	one := func(typ Type) *Table {
		return &Table{Name: "t", Columns: []Column{{Name: "k", Type: typ, Null: false}}, Key: []int{0}, Root: 1}
	}
	u64 := func(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
	keys := map[string]struct {
		t *Table
		b []byte
	}{
		"int64 short":            {one(Int64), []byte{1, 2, 3}},
		"float NaN":              {one(Float64), u64(math.Float64bits(math.NaN()) | signBit)},
		"float -0":               {one(Float64), u64(^uint64(signBit))},
		"bool 2":                 {one(Bool), []byte{2}},
		"string without end":     {one(String), []byte("ab")},
		"string 0x00 then 0x02":  {one(String), []byte{'a', 0, 2}},
		"string not UTF-8":       {one(String), []byte{0xff, 0, 1}},
		"bytes cut after 0x00":   {one(Bytes), []byte{'a', 0}},
		"time with 1e9 ns":       {one(Time), append(u64(signBit), 0x3b, 0x9a, 0xca, 0x00)},
		"time short":             {one(Time), u64(signBit)},
		"byte after the key":     {one(Bool), []byte{1, 0}},
		"second key column gone": {&Table{Name: "t", Columns: []Column{{Name: "a", Type: Bool, Null: false}, {Name: "b", Type: Bool, Null: false}}, Key: []int{0, 1}}, []byte{1}},
	}
	for name, c := range keys {
		if err := decodeKey(c.t, c.b, make(Row, len(c.t.Columns))); !errors.Is(err, ErrDamaged) {
			t.Errorf("key, %s: %v", name, err)
		}
	}
	vt := &Table{Name: "t", Columns: []Column{{Name: "k", Type: Int64, Null: false}, {Name: "a", Type: Bool, Null: true}, {Name: "s", Type: String, Null: true}, {Name: "m", Type: Time, Null: true}}, Key: []int{0}}
	values := map[string][]byte{
		"empty":                   {},
		"count in long form":      {0x83, 0x00, 0x07},
		"bit past the last":       {3, 0x0f},
		"bool 2":                  {3, 0x06, 2},
		"string past the end":     {3, 0x05, 4, 'a'},
		"string not UTF-8":        {3, 0x05, 1, 0xff},
		"time with 1e9 ns":        {3, 0x03, 0, 0x80, 0x94, 0xeb, 0xdc, 0x03},
		"no bitmap":               {3},
		"four columns":            {4, 0x0f},
		"long varint for seconds": {3, 0x03, 0x80, 0x00, 0},
	}
	for name, b := range values {
		if err := decodeValues(vt, b, make(Row, len(vt.Columns))); !errors.Is(err, ErrDamaged) {
			t.Errorf("values, %s: %v", name, err)
		}
	}
	good := &Schema{Version: 1, Tables: []Table{{Name: "a", Columns: []Column{{Name: "x", Type: Int64, Null: false}}, Key: []int{0}, Root: 3}}}
	enc := encodeSchema(good)
	change := func(i int, v byte) []byte {
		b := append([]byte(nil), enc...)
		b[i] = v
		return b
	}
	// enc is: format, version, 1 table, len 1 "a", root 3, 1 column,
	// len 1 "x", type, flags, 1 key column, column 0, 0 indexes.
	twoTables := encodeSchema(&Schema{Tables: []Table{good.Tables[0], good.Tables[0]}})
	// A second column outside the key: its flags byte is the last but
	// four.
	wide := encodeSchema(&Schema{Tables: []Table{{Name: "a", Columns: []Column{{Name: "x", Type: Int64, Null: false}, {Name: "y", Type: Int64, Null: false}}, Key: []int{0}, Root: 3}}})
	wide[len(wide)-4] = 2
	noKey := append(change(11, 0)[:12:12], enc[13:]...)
	// 2^63 tables: as an int the count is negative, and a loop over it
	// would read no table and report an empty schema.
	huge := binary.AppendUvarint([]byte{schemaFormat2, 1}, 1<<63)
	// enc in format 3: an empty default and no fill after the flags, and
	// 0 checks at the end. It holds no default, fill or check, so
	// encodeSchema writes format 2 for it.
	bare3 := append(append([]byte{schemaFormat3}, enc[1:11]...), 0, 0)
	bare3 = append(append(bare3, enc[11:]...), 0)
	// withCheck is bare3 with one check of the given bytes.
	withCheck := func(check ...byte) []byte {
		b := append([]byte(nil), bare3[:len(bare3)-1]...)
		return append(append(b, 1, byte(len(check))), check...)
	}
	// withFill is bare3 with a column y of type bool after x. The given
	// bytes are the fill mark and value of y. keyFill gives the key
	// column x a fill of 5 (the varint 10).
	withFill := func(fill ...byte) []byte {
		b := append([]byte(nil), bare3[:6]...)
		b = append(b, 2)
		b = append(b, bare3[7:13]...)
		b = append(append(b, 1, 'y', byte(Bool), 1, 0), fill...)
		return append(b, bare3[13:]...)
	}
	keyFill := append(append(append([]byte(nil), bare3[:12]...), 1, 10), bare3[13:]...)
	defaultNotUTF8 := append([]byte(nil), withCheck('x')...)
	defaultNotUTF8 = append(append(defaultNotUTF8[:11:11], 1, 0xff), defaultNotUTF8[12:]...)
	// ix is enc with one index: name "i", unique flag, root 4, one column,
	// column 0. It ends at the old end plus 6 bytes.
	withIndex := func(flag, root, ncols, col byte) []byte {
		b := append([]byte(nil), enc[:len(enc)-1]...)
		b = append(b, 1, 1, 'i', flag, root, ncols)
		if ncols > 0 {
			b = append(b, col)
		}
		return b
	}
	twoIndexes := append(withIndex(0, 4, 1, 0)[:len(enc)-1:len(enc)-1], 2, 1, 'i', 0, 4, 1, 0, 1, 'i', 0, 5, 1, 0)
	schemas := map[string][]byte{
		"index flags 2":          withIndex(2, 4, 1, 0),
		"index root 0":           withIndex(0, 0, 1, 0),
		"index without column":   withIndex(0, 4, 0, 0),
		"index column 1 of 1":    withIndex(0, 4, 1, 1),
		"index name twice":       twoIndexes,
		"empty":                  {},
		"format 1":               change(0, 1),
		"root 0":                 change(5, 0),
		"type 9":                 change(9, 9),
		"flags 2":                change(10, 2),
		"key column 1 of 1":      change(12, 1),
		"key column allows null": change(10, 1),
		"table count too big":    change(2, 100),
		"byte after the end":     append(append([]byte(nil), enc...), 0),
		"cut":                    enc[:len(enc)-1],
		"same table twice":       twoTables,
		"no key":                 noKey,
		"flags 2 outside key":    wide,
		"table count 2^63":       huge,
		"format 4":               change(0, 4),
		"format 3 without extra": bare3,
		"format 2 with a check":  append(append([]byte{schemaFormat2}, withCheck('x')[1:11]...), withCheck('x')[13:]...),
		"empty check":            append(bare3[:len(bare3)-1:len(bare3)-1], 2, 0, 2, 'x', 'y'),
		"check not UTF-8":        withCheck(0xff),
		"check count too big":    append(bare3[:len(bare3)-1:len(bare3)-1], 5, 1, 'x'),
		"default not UTF-8":      defaultNotUTF8,
		"fill mark 2":            withFill(2),
		"bool fill 2":            withFill(1, 2),
		"fill cut":               withFill(1)[:len(withFill(1))-len(bare3[13:])],
		"fill on a key column":   keyFill,
	}
	for name, b := range schemas {
		if _, err := decodeSchema(b); !errors.Is(err, ErrDamaged) {
			t.Errorf("schema, %s: %v", name, err)
		}
	}
	// Each count lies between two bounds: the smallest size of its items
	// in format 2 and in format 3. Only the bound of format 3 rejects it
	// at once. manyColumns holds 12 bytes after its column count, and 3
	// columns take at least 15. Two checks take at least 4 bytes, and 2
	// follow.
	manyColumns := append(append([]byte(nil), bare3...), 0, 0)
	manyColumns[6] = 3
	for name, b := range map[string][]byte{
		"3 columns in format 3": manyColumns,
		"2 checks in 2 bytes":   append(bare3[:len(bare3)-1:len(bare3)-1], 2, 1, 'x'),
	} {
		if _, err := decodeSchema(b); !errors.Is(err, ErrDamaged) || !strings.Contains(err.Error(), "do not fit") {
			t.Errorf("schema, %s: %v", name, err)
		}
	}
	for name, b := range map[string][]byte{"the unchanged schema": enc, "one index": withIndex(0, 4, 1, 0), "one unique index": withIndex(1, 4, 1, 0), "format 3 with a check": withCheck('x'), "a bool fill": withFill(1, 1)} {
		if _, err := decodeSchema(b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// An index key with a null mark of 2.
	ix := &Index{Name: "i", Columns: []int{1}}
	nt := &Table{Name: "t", Columns: []Column{{Name: "k", Type: Int64, Null: false}, {Name: "a", Type: Int64, Null: true}}, Key: []int{0}}
	if _, err := decodeIndexKey(nt, ix, append([]byte{2}, appendKey(nil, Int64, int64(5))...), make(Row, 2)); !errors.Is(err, ErrDamaged) {
		t.Errorf("index key, null mark 2: %v", err)
	}
}
