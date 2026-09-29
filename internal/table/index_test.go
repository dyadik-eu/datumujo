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

	"github.com/dyadik-eu/datumujo/internal/btree"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// The table of the index tests: issues of repositories.
var issueDef = Def{Name: "issue", Columns: []Column{
	{Name: "repo", Type: Int64, Null: false},
	{Name: "num", Type: Int64, Null: false},
	{Name: "state", Type: String, Null: false},
	{Name: "title", Type: String, Null: true},
	{Name: "score", Type: Float64, Null: true},
}, Key: []string{"repo", "num"}}

var issueIndexes = []IndexDef{
	{Name: "by_state", Columns: []string{"repo", "state"}},
	{Name: "by_title", Columns: []string{"title"}, Unique: true},
	{Name: "by_score", Columns: []string{"score"}},
}

// compareValues orders two values of one column, null first. It does not
// use the key forms: it is the oracle they are checked against.
func compareValues(typ Type, a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case less(typ, a, b):
		return -1
	case less(typ, b, a):
		return 1
	}
	return 0
}

// compareTuple orders rows by the given columns.
func compareTuple(t *Table, cols []int, a, b Row) int {
	for _, c := range cols {
		if r := compareValues(t.Columns[c].Type, a[c], b[c]); r != 0 {
			return r
		}
	}
	return 0
}

// compareBound compares the leading columns of a row with bound values.
func compareBound(t *Table, cols []int, row Row, vals []any) int {
	for i, v := range vals {
		if r := compareValues(t.Columns[cols[i]].Type, row[cols[i]], v); r != 0 {
			return r
		}
	}
	return 0
}

// expected returns the rows of the oracle that the options select, in
// scan order.
func expected(t *Table, o Options, rows []Row) []Row {
	cols := t.Key
	if o.Index != "" {
		ix, _ := t.Index(o.Index)
		cols = append(append([]int(nil), ix.Columns...), t.Key...)
	}
	var out []Row
	for _, r := range rows {
		if compareBound(t, cols, r, o.Prefix) != 0 ||
			o.From != nil && compareBound(t, cols, r, o.From) < 0 ||
			o.To != nil && compareBound(t, cols, r, o.To) >= 0 {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return compareTuple(t, cols, out[i], out[j]) < 0 })
	if o.Reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func scanAll(t *testing.T, v *View, table string, o Options) []Row {
	t.Helper()
	rows, err := v.Scan(table, o)
	if err != nil {
		t.Fatal(err)
	}
	var out []Row
	for rows.Next() {
		out = append(out, rows.Row())
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	return out
}

func sameRows(a, b []Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameRow(a[i], b[i]) {
			return false
		}
	}
	return true
}

// checkPages checks every tree and that each page of the file is in a
// tree, free, or the header. A page that an aborted change allocated and
// did not free would break the sum.
func checkPages(t *testing.T, s *store.Store) {
	t.Helper()
	snap, v := view(t, s)
	defer snap.Close()
	pages := 0
	check := func(root uint64) {
		st, err := btree.Open(root, pageSize).Check(snap)
		if err != nil {
			t.Fatal(err)
		}
		pages += len(st.Pages)
	}
	if root := snap.Root(CatalogSlot); root != 0 {
		check(root)
	}
	for _, tb := range v.Schema().Tables {
		check(tb.Root)
		for _, ix := range tb.Indexes {
			check(ix.Root)
		}
	}
	if uint64(pages)+snap.FreeCount()+1 != snap.Count() {
		t.Fatalf("pages: %d in trees, %d free, 1 header; the file has %d", pages, snap.FreeCount(), snap.Count())
	}
}

func createIssues(t *testing.T, s *store.Store) {
	t.Helper()
	stx, tx := begin(t, s)
	if err := tx.CreateTable(issueDef); err != nil {
		t.Fatal(err)
	}
	for _, d := range issueIndexes {
		if err := tx.CreateIndex("issue", d); err != nil {
			t.Fatal(err)
		}
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestIndexesAgainstAMap runs random inserts, updates and deletes on a
// table with three indexes, one of them unique, against a map. After each
// commit it compares full scans, both directions, and random range and
// prefix scans on the key and each index. An index is created on the way
// from existing rows, and one that must fail is tried.
func TestIndexesAgainstAMap(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		rng := rand.New(rand.NewSource(seed))
		fs := vfs.NewSim()
		s := openStore(t, fs)
		createIssues(t, s)
		oracle := map[string]Row{}
		pkOf := func(r Row) string { return fmt.Sprint(r[0], "/", r[1]) }
		states := []string{"open", "closed", "draft"}
		// Half the titles are null. The others come from 30 values, so
		// some writes hit a taken title and the table still grows.
		titles := []any{"a", "b", "c", "a\x00", "ü", "", "bug"}
		for i := len(titles); i < 30; i++ {
			titles = append(titles, fmt.Sprint("t", i))
		}
		scores := []any{nil, -1.5, 0.0, math.Copysign(0, -1), 2.0, math.Inf(1)}
		randRow := func() Row {
			var title any
			if rng.Intn(2) == 0 {
				title = titles[rng.Intn(len(titles))]
			}
			return Row{int64(rng.Intn(3)), int64(rng.Intn(12)), states[rng.Intn(3)], title, scores[rng.Intn(len(scores))]}
		}
		conflicts, uniqueErrs := 0, 0
		for batch := 0; batch < 30; batch++ {
			before := map[string]Row{}
			for k, v := range oracle {
				before[k] = v
			}
			stx, tx := begin(t, s)
			if batch == 10 {
				// state repeats, so a unique index on it must fail.
				if err := tx.CreateIndex("issue", IndexDef{Name: "u_state", Columns: []string{"state"}, Unique: true}); len(oracle) > 3 && !errors.Is(err, ErrUnique) {
					t.Fatalf("seed %d: unique index over repeated values: %v", seed, err)
				}
				if err := tx.CreateIndex("issue", IndexDef{Name: "by_num", Columns: []string{"num", "title"}}); err != nil {
					t.Fatal(err)
				}
			}
			for op := 0; op < 20; op++ {
				row := randRow()
				pk := pkOf(row)
				_, exists := oracle[pk]
				// Would the row break the unique index on title?
				clash := false
				for k, r := range oracle {
					if k != pk && row[3] != nil && r[3] == row[3] {
						clash = true
					}
				}
				var err error
				switch rng.Intn(5) {
				case 0, 1, 2:
					err = tx.Insert("issue", row)
					if exists {
						if !errors.Is(err, ErrExists) {
							t.Fatalf("seed %d: insert of existing key: %v", seed, err)
						}
						continue
					}
				case 3:
					err = tx.Update("issue", row)
					if !exists {
						if !errors.Is(err, ErrNotFound) {
							t.Fatalf("seed %d: update of missing key: %v", seed, err)
						}
						continue
					}
				default:
					found, err := tx.Delete("issue", row[0], row[1])
					if err != nil || found != exists {
						t.Fatalf("seed %d: delete: %v %v", seed, found, err)
					}
					delete(oracle, pk)
					continue
				}
				if clash {
					conflicts++
					if !errors.Is(err, ErrUnique) {
						t.Fatalf("seed %d: title %q is taken: %v", seed, row[3], err)
					}
					uniqueErrs++
					continue
				}
				if err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
				oracle[pk] = row
			}
			if rng.Intn(5) == 0 && batch != 10 {
				stx.Rollback()
				oracle = before
				continue
			}
			if err := stx.Commit(); err != nil {
				t.Fatal(err)
			}
			compareScans(t, s, oracle, rng)
		}
		if conflicts == 0 {
			t.Fatalf("seed %d: no write hit the unique index; the test did not check it", seed)
		}
		checkPages(t, s)
		s.Close()
		s = openStore(t, fs)
		compareScans(t, s, oracle, rng)
		s.Close()
		t.Logf("seed %d: %d rows, %d writes refused by the unique index", seed, len(oracle), uniqueErrs)
	}
}

// compareScans compares scans of the key and of every index with the
// oracle: full scans in both directions and random bounds.
func compareScans(t *testing.T, s *store.Store, oracle map[string]Row, rng *rand.Rand) {
	t.Helper()
	snap, v := view(t, s)
	defer snap.Close()
	tb, _ := v.Schema().Table("issue")
	var rows []Row
	for _, r := range oracle {
		rows = append(rows, r)
	}
	indexes := []string{""}
	for _, ix := range tb.Indexes {
		indexes = append(indexes, ix.Name)
	}
	randValue := func(c int) any {
		if len(rows) > 0 && rng.Intn(2) == 0 {
			return rows[rng.Intn(len(rows))][c]
		}
		switch tb.Columns[c].Type {
		case Int64:
			return int64(rng.Intn(14) - 1)
		case Float64:
			return float64(rng.Intn(5) - 2)
		default:
			return []string{"", "a", "b", "c", "open", "z"}[rng.Intn(6)]
		}
	}
	randBound := func(cols []int) []any {
		var vals []any
		for i := 0; i < rng.Intn(len(cols)+1); i++ {
			v := randValue(cols[i])
			if v == nil && !tb.Columns[cols[i]].Null {
				v = randValue(cols[i])
				if v == nil {
					break
				}
			}
			vals = append(vals, v)
		}
		return vals
	}
	for _, name := range indexes {
		cols := tb.Key
		if name != "" {
			ix, _ := tb.Index(name)
			cols = ix.Columns
		}
		for _, rev := range []bool{false, true} {
			o := Options{Index: name, Reverse: rev}
			if got, want := scanAll(t, v, "issue", o), expected(tb, o, rows); !sameRows(got, want) {
				t.Fatalf("index %q, reverse %v: %d rows, want %d", name, rev, len(got), len(want))
			}
		}
		for i := 0; i < 20; i++ {
			o := Options{Index: name, Reverse: rng.Intn(2) == 0}
			switch rng.Intn(4) {
			case 0:
				o.Prefix = randBound(cols)
			case 1:
				o.From, o.To = randBound(cols), randBound(cols)
			case 2:
				o.Prefix, o.From = randBound(cols[:1]), randBound(cols)
			default:
				o.Prefix, o.To = randBound(cols[:1]), randBound(cols)
			}
			if o.From != nil && len(o.From) == 0 {
				o.From = nil
			}
			if o.To != nil && len(o.To) == 0 {
				o.To = nil
			}
			got, want := scanAll(t, v, "issue", o), expected(tb, o, rows)
			if !sameRows(got, want) {
				t.Fatalf("index %q, %+v: %v, want %v", name, o, got, want)
			}
		}
	}
}

// TestUniqueChangesNothing checks that a write refused by a unique index
// leaves the table and every index as they were. It also checks that
// nulls never conflict, and that a row may keep its own value.
func TestUniqueChangesNothing(t *testing.T) {
	s := openStore(t, vfs.NewSim())
	defer s.Close()
	createIssues(t, s)
	stx, tx := begin(t, s)
	defer stx.Rollback()
	for _, r := range []Row{
		{int64(1), int64(1), "open", "a", 1.0},
		{int64(1), int64(2), "open", nil, 2.0},
		{int64(1), int64(3), "open", nil, 3.0},
	} {
		if err := tx.Insert("issue", r); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() string {
		var b strings.Builder
		for _, ix := range []string{"", "by_state", "by_title", "by_score"} {
			fmt.Fprint(&b, scanAll(t, &tx.View, "issue", Options{Index: ix}))
		}
		return b.String()
	}
	before := snapshot()
	if err := tx.Insert("issue", Row{int64(2), int64(1), "closed", "a", 9.0}); !errors.Is(err, ErrUnique) {
		t.Fatalf("insert of a taken title: %v", err)
	}
	if err := tx.Update("issue", Row{int64(1), int64(2), "closed", "a", 9.0}); !errors.Is(err, ErrUnique) {
		t.Fatalf("update to a taken title: %v", err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("a refused write changed the tables:\n%s\n%s", before, after)
	}
	// The row keeps its own title while other columns change.
	if err := tx.Update("issue", Row{int64(1), int64(1), "closed", "a", 5.0}); err != nil {
		t.Fatal(err)
	}
	// After the title is free, another row takes it.
	if err := tx.Update("issue", Row{int64(1), int64(1), "closed", "b", 5.0}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Update("issue", Row{int64(1), int64(2), "open", "a", 2.0}); err != nil {
		t.Fatalf("title freed by an update: %v", err)
	}
	got := scanAll(t, &tx.View, "issue", Options{Index: "by_title", Prefix: []any{"a"}})
	if len(got) != 1 || got[0][1] != int64(2) {
		t.Fatalf("lookup by title: %v", got)
	}
	if _, err := tx.Delete("issue", int64(1), int64(2)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("issue", Row{int64(3), int64(3), "open", "a", nil}); err != nil {
		t.Fatalf("title freed by a delete: %v", err)
	}
}

// TestIndexRules checks what an index accepts: definitions, NaN, key
// length, and a failed CreateIndex that must free its pages.
func TestIndexRules(t *testing.T) {
	s := openStore(t, vfs.NewSim())
	defer s.Close()
	createIssues(t, s)
	stx, tx := begin(t, s)
	for name, d := range map[string]IndexDef{
		"no columns":     {Name: "x"},
		"missing column": {Name: "x", Columns: []string{"nope"}},
		"column twice":   {Name: "x", Columns: []string{"num", "num"}},
		"name exists":    {Name: "by_title", Columns: []string{"num"}},
		"empty name":     {Name: "", Columns: []string{"num"}},
	} {
		if err := tx.CreateIndex("issue", d); !errors.Is(err, ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := tx.CreateIndex("nope", IndexDef{Name: "x", Columns: []string{"a"}}); !errors.Is(err, ErrNoTable) {
		t.Errorf("no table: %v", err)
	}
	if _, err := tx.Scan("issue", Options{Index: "nope"}); !errors.Is(err, ErrNoIndex) {
		t.Errorf("scan of no index: %v", err)
	}
	for i := len(issueIndexes); i < MaxIndexes; i++ {
		if err := tx.CreateIndex("issue", IndexDef{Name: fmt.Sprint("i", i), Columns: []string{"num"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.CreateIndex("issue", IndexDef{Name: "one too many", Columns: []string{"num"}}); !errors.Is(err, ErrSchema) {
		t.Errorf("index %d: %v", MaxIndexes+1, err)
	}
	stx.Rollback()

	stx, tx = begin(t, s)
	if err := tx.CreateTable(Def{Name: "m", Columns: []Column{{Name: "k", Type: Int64, Null: false}, {Name: "f", Type: Float64, Null: true}, {Name: "s", Type: String, Null: true}}, Key: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	// NaN is a value outside an index.
	if err := tx.Insert("m", Row{int64(1), math.NaN(), nil}); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	stx, tx = begin(t, s)
	if err := tx.CreateIndex("m", IndexDef{Name: "by_f", Columns: []string{"f"}}); !errors.Is(err, ErrValue) {
		t.Fatalf("index over a NaN: %v", err)
	}
	if m, _ := tx.Schema().Table("m"); len(m.Indexes) != 0 {
		t.Fatalf("the failed index is in the schema: %+v", tx.Schema())
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	checkPages(t, s)

	stx, tx = begin(t, s)
	defer stx.Rollback()
	if _, err := tx.Delete("m", int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateIndex("m", IndexDef{Name: "by_f", Columns: []string{"f"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateIndex("m", IndexDef{Name: "by_s", Columns: []string{"s"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("m", Row{int64(2), math.NaN(), nil}); !errors.Is(err, ErrValue) {
		t.Fatalf("NaN into an index: %v", err)
	}
	if _, err := tx.Scan("m", Options{Index: "by_f", From: []any{math.NaN()}}); !errors.Is(err, ErrValue) {
		t.Fatalf("scan from NaN: %v", err)
	}
	if _, err := tx.Scan("m", Options{From: []any{int64(1), int64(2)}}); !errors.Is(err, ErrValue) {
		t.Fatalf("bound with more values than columns: %v", err)
	}
	// A key that fits the table and not the index: the index adds the
	// mark and the primary key.
	max := btree.LimitsFor(pageSize).MaxKey
	long := strings.Repeat("x", max-3)
	if err := tx.Insert("m", Row{int64(3), 1.0, long}); !errors.Is(err, ErrValue) {
		t.Fatalf("index key too long: %v", err)
	}
	if got := scanAll(t, &tx.View, "m", Options{}); len(got) != 0 {
		t.Fatalf("a refused write left rows: %v", got)
	}
	// A primary key too long, in a table without an index: the check
	// runs before the tree, which would refuse it with its own error.
	if err := tx.CreateTable(Def{Name: "p", Columns: []Column{{Name: "k", Type: String, Null: false}}, Key: []string{"k"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Insert("p", Row{strings.Repeat("x", max)}); !errors.Is(err, ErrValue) {
		t.Fatalf("primary key too long: %v", err)
	}
}

// TestBounds checks the helpers that combine scan bounds.
func TestBounds(t *testing.T) {
	for _, c := range []struct{ in, want []byte }{
		{nil, nil},
		{[]byte{}, nil},
		{[]byte{1}, []byte{2}},
		{[]byte{1, 0xfe}, []byte{1, 0xff}},
		{[]byte{1, 0xff}, []byte{2}},
		{[]byte{0xff, 0xff}, nil},
	} {
		if got := successor(c.in); !bytes.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("successor(%x) = %x, want %x", c.in, got, c.want)
		}
	}
	a, b := []byte{1}, []byte{2}
	for _, c := range []struct {
		got, want []byte
		what      string
	}{
		{maxLower(a, b), b, "maxLower(a, b)"},
		{maxLower(b, a), b, "maxLower(b, a)"},
		{maxLower(nil, a), a, "maxLower(nil, a)"},
		{minUpper(a, b), a, "minUpper(a, b)"},
		{minUpper(b, a), a, "minUpper(b, a)"},
		{minUpper(nil, b), b, "minUpper(nil, b)"},
		{minUpper(b, nil), b, "minUpper(b, nil)"},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s = %x, want %x", c.what, c.got, c.want)
		}
	}
	if minUpper(nil, nil) != nil {
		t.Error("minUpper(nil, nil) is not nil")
	}
}

// TestPagination pages through the key and an index while rows are added
// and deleted between pages. Each row that exists through the whole walk
// must come exactly once, and the walk never goes back.
func TestPagination(t *testing.T) {
	for _, index := range []string{"", "by_state"} {
		for _, reverse := range []bool{false, true} {
			rng := rand.New(rand.NewSource(7))
			s := openStore(t, vfs.NewSim())
			createIssues(t, s)
			stx, tx := begin(t, s)
			for n := int64(0); n < 60; n++ {
				if err := tx.Insert("issue", Row{int64(1), n, []string{"open", "closed"}[n%2], nil, nil}); err != nil {
					t.Fatal(err)
				}
			}
			if err := stx.Commit(); err != nil {
				t.Fatal(err)
			}
			stay := map[int64]bool{}
			for n := int64(0); n < 60; n++ {
				stay[n] = true
			}
			seen := map[int64]int{}
			var after, last []byte
			for page := 0; ; page++ {
				snap, v := view(t, s)
				rows, err := v.Scan("issue", Options{Index: index, Reverse: reverse, After: after})
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for n < 7 && rows.Next() {
					cur := rows.Cursor()
					if last != nil && (bytes.Compare(cur, last) <= 0) != reverse {
						t.Fatalf("index %q, reverse %v: the walk went back", index, reverse)
					}
					last = cur
					seen[rows.Row()[1].(int64)]++
					after = cur
					n++
				}
				if rows.Err() != nil {
					t.Fatal(rows.Err())
				}
				snap.Close()
				if n == 0 {
					break
				}
				// Between pages: add rows and delete others.
				stx, tx := begin(t, s)
				for i := 0; i < 3; i++ {
					k := int64(rng.Intn(80))
					if rng.Intn(2) == 0 {
						delete(stay, k)
						if _, err := tx.Delete("issue", int64(1), k); err != nil {
							t.Fatal(err)
						}
					} else if err := tx.Insert("issue", Row{int64(1), k + 100, "open", nil, nil}); err != nil && !errors.Is(err, ErrExists) {
						t.Fatal(err)
					}
				}
				if err := stx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			for k, n := range seen {
				if n != 1 {
					t.Fatalf("index %q, reverse %v: row %d came %d times", index, reverse, k, n)
				}
			}
			for k := range stay {
				if seen[k] != 1 {
					t.Fatalf("index %q, reverse %v: row %d stayed and did not come", index, reverse, k)
				}
			}
			s.Close()
		}
	}
}

// TestCounters checks that counters start at 1, are separate, give a
// value again only after a rollback, and keep their value after reopen.
func TestCounters(t *testing.T) {
	fs := vfs.NewSim()
	s := openStore(t, fs)
	next := func(tx *Tx, name string, want uint64) {
		t.Helper()
		if got, err := tx.Next(name); err != nil || got != want {
			t.Fatalf("%s: %d %v, want %d", name, got, err, want)
		}
	}
	// A counter before any table creates the catalog, and the file still
	// opens with an empty schema.
	stx, tx := begin(t, s)
	next(tx, "repo/1", 1)
	next(tx, "repo/1", 2)
	next(tx, "repo/2", 1)
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	stx, tx = begin(t, s)
	next(tx, "repo/1", 3)
	stx.Rollback()
	stx, tx = begin(t, s)
	next(tx, "repo/1", 3)
	if _, err := tx.Next(""); !errors.Is(err, ErrSchema) {
		t.Fatalf("empty name: %v", err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openStore(t, fs)
	defer s.Close()
	snap, v := view(t, s)
	defer snap.Close()
	if v.Schema().Version != 0 {
		t.Fatalf("counters changed the schema version to %d", v.Schema().Version)
	}
	for name, want := range map[string]uint64{"repo/1": 3, "repo/2": 1, "repo/3": 0} {
		if got, err := v.Counter(name); err != nil || got != want {
			t.Fatalf("%s: %d %v, want %d", name, got, err, want)
		}
	}
	// Stored values that Next does not write, and the end of the range.
	stx, tx = begin(t, s)
	cat := btree.Open(stx.Root(CatalogSlot), pageSize)
	for value, want := range map[string]error{"\x00": ErrDamaged, "\x80\x00": ErrDamaged, "\x01\x01": ErrDamaged} {
		if err := cat.Put(stx, []byte(counterPrefix+"bad"), []byte(value)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Next("bad"); !errors.Is(err, want) {
			t.Errorf("stored %x: %v", value, err)
		}
	}
	if err := cat.Put(stx, []byte(counterPrefix+"end"), binary.AppendUvarint(nil, math.MaxUint64)); err != nil {
		t.Fatal(err)
	}
	if n, err := tx.Next("end"); err == nil || n != 0 {
		t.Errorf("counter at its largest value: %d %v", n, err)
	}
	if n, err := tx.Counter("end"); err != nil || n != math.MaxUint64 {
		t.Errorf("counter at its largest value, read: %d %v", n, err)
	}
	stx.Rollback()
	empty, err := Open(emptyReader{}, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := empty.Counter("x"); got != 0 || err != nil {
		t.Fatalf("counter without a catalog: %d %v", got, err)
	}
}

type emptyReader struct{}

func (emptyReader) Read(uint64, []byte) error { return errors.New("no pages") }
func (emptyReader) Root(int) uint64           { return 0 }
func (emptyReader) Version() uint32           { return 1 }

// TestIndexDamaged writes index entries that do not match the table.
// Scans and deletes return ErrDamaged instead of a wrong row.
func TestIndexDamaged(t *testing.T) {
	s := openStore(t, vfs.NewSim())
	defer s.Close()
	createIssues(t, s)
	stx, tx := begin(t, s)
	defer stx.Rollback()
	row := Row{int64(1), int64(1), "open", "a", 1.0}
	if err := tx.Insert("issue", row); err != nil {
		t.Fatal(err)
	}
	tb, _ := tx.Schema().Table("issue")
	ix, _ := tb.Index("by_state")
	tree := btree.Open(ix.Root, pageSize)
	good := indexKey(tb, ix, row)
	other := indexKey(tb, ix, Row{int64(1), int64(2), "open", "a", 1.0})
	wrong := indexKey(tb, ix, Row{int64(1), int64(1), "closed", "a", 1.0})
	cases := []struct {
		name  string
		key   []byte
		value []byte
	}{
		{"an entry with a value", good, []byte{1}},
		{"an entry without a row", other, nil},
		{"an entry that does not match its row", wrong, nil},
	}
	for _, c := range cases {
		if _, err := tree.Delete(stx, good); err != nil {
			t.Fatal(err)
		}
		if err := tree.Put(stx, c.key, c.value); err != nil {
			t.Fatal(err)
		}
		rows, _ := tx.Scan("issue", Options{Index: "by_state"})
		for rows.Next() {
		}
		if !errors.Is(rows.Err(), ErrDamaged) {
			t.Errorf("%s: %v", c.name, rows.Err())
		}
		if _, err := tree.Delete(stx, c.key); err != nil {
			t.Fatal(err)
		}
		if err := tree.Put(stx, good, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A row whose entry is gone: deleting it finds the index damaged.
	if _, err := tree.Delete(stx, good); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Delete("issue", int64(1), int64(1)); !errors.Is(err, ErrDamaged) {
		t.Errorf("delete without its entry: %v", err)
	}
}

// FuzzDecodeIndexKey checks that decodeIndexKey returns an error and no
// panic for any input, and that the columns it accepts encode to the
// same bytes.
func FuzzDecodeIndexKey(f *testing.F) {
	ix := &fuzzTable.Indexes[0]
	f.Add(indexKey(fuzzTable, ix, fuzzRow()))
	f.Fuzz(func(t *testing.T, b []byte) {
		row := make(Row, len(fuzzTable.Columns))
		pk, err := decodeIndexKey(fuzzTable, ix, b, row)
		if err != nil {
			return
		}
		if again := append(appendIndexColumns(nil, fuzzTable, ix, row), pk...); !bytes.Equal(again, b) {
			t.Fatalf("decoded %x, encodes to %x", b, again)
		}
	})
}

// TestExclusiveAndInclusiveBounds checks FromExclusive and ToInclusive
// against a filter over all rows, through the key and through an index,
// in both directions. The key includes the largest int64, whose key form
// is all 0xFF bytes: nothing is after it.
func TestExclusiveAndInclusiveBounds(t *testing.T) {
	s := openStore(t, vfs.NewSim())
	stx, tx := begin(t, s)
	defer stx.Rollback()
	if err := tx.CreateTable(Def{Name: "t", Columns: []Column{
		{Name: "id", Type: Int64}, {Name: "g", Type: String, Null: true},
	}, Key: []string{"id"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateIndex("t", IndexDef{Name: "by_g", Columns: []string{"g"}}); err != nil {
		t.Fatal(err)
	}
	ids := []int64{math.MinInt64, -5, 0, 1, 2, 3, 7, 8, math.MaxInt64}
	groups := []any{"a", "b", nil, "b", "c", "a", "b", "c", "b"}
	for i, id := range ids {
		if err := tx.Insert("t", Row{id, groups[i]}); err != nil {
			t.Fatal(err)
		}
	}
	scan := func(o Options) []int64 {
		t.Helper()
		rows, err := tx.Scan("t", o)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for rows.Next() {
			out = append(out, rows.Row()[0].(int64))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// want filters all rows in the order of a scan without bounds.
	want := func(o Options, keep func(id int64, g any) bool) []int64 {
		all := scan(Options{Index: o.Index, Reverse: o.Reverse})
		var out []int64
		for _, id := range all {
			i := sort.Search(len(ids), func(i int) bool { return ids[i] >= id })
			if keep(id, groups[i]) {
				out = append(out, id)
			}
		}
		return out
	}
	cmpG := func(g any, b string) int {
		if g == nil {
			return -1 // null sorts first in an index
		}
		return strings.Compare(g.(string), b)
	}
	for _, from := range []int64{math.MinInt64, -6, 0, 2, 7, math.MaxInt64} {
		for _, to := range []int64{math.MinInt64, 0, 3, 8, math.MaxInt64} {
			for _, fx := range []bool{false, true} {
				for _, ti := range []bool{false, true} {
					for _, rev := range []bool{false, true} {
						o := Options{From: []any{from}, To: []any{to}, FromExclusive: fx, ToInclusive: ti, Reverse: rev}
						w := want(o, func(id int64, _ any) bool {
							lo := id > from || !fx && id == from
							hi := id < to || ti && id == to
							return lo && hi
						})
						if got := scan(o); fmt.Sprint(got) != fmt.Sprint(w) {
							t.Errorf("key %+v: %v, want %v", o, got, w)
						}
					}
				}
			}
		}
	}
	for _, from := range []string{"", "a", "b", "bb", "c", "d"} {
		for _, to := range []string{"a", "b", "c", "z"} {
			for _, fx := range []bool{false, true} {
				for _, ti := range []bool{false, true} {
					o := Options{Index: "by_g", From: []any{from}, To: []any{to}, FromExclusive: fx, ToInclusive: ti}
					w := want(o, func(_ int64, g any) bool {
						lo := cmpG(g, from) > 0 || !fx && cmpG(g, from) == 0
						hi := cmpG(g, to) < 0 || ti && cmpG(g, to) == 0
						return g != nil && lo && hi
					})
					if got := scan(o); fmt.Sprint(got) != fmt.Sprint(w) {
						t.Errorf("index %+v: %v, want %v", o, got, w)
					}
				}
			}
		}
	}
	if got := scan(Options{From: []any{int64(math.MaxInt64)}, FromExclusive: true}); len(got) != 0 {
		t.Errorf("after the largest int64: %v", got)
	}
}
