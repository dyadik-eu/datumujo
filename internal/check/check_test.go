package check

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/btree"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

const pageSize = 1024

var def = table.Def{Name: "issue", Columns: []table.Column{
	{Name: "num", Type: table.Int64},
	{Name: "state", Type: table.String},
	{Name: "title", Type: table.String, Null: true},
	{Name: "body", Type: table.String, Null: true},
}, Key: []string{"num"}}

// build writes a database with one table and two indexes. Long values
// take overflow pages, and deleted rows leave free pages. A checkpoint
// puts every page in the file. build returns the rows it keeps.
func build(t *testing.T, fs vfs.FS, name string) map[int64]table.Row {
	t.Helper()
	s, err := store.Open(fs, name, store.Options{PageSize: pageSize})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stx, _ := s.Begin()
	tx, err := table.Begin(stx, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateTable(def); err != nil {
		t.Fatal(err)
	}
	for _, d := range []table.IndexDef{{Name: "by_state", Columns: []string{"state"}}, {Name: "by_title", Columns: []string{"title"}, Unique: true}} {
		if err := tx.CreateIndex("issue", d); err != nil {
			t.Fatal(err)
		}
	}
	want := map[int64]table.Row{}
	rng := rand.New(rand.NewSource(1))
	for n := int64(0); n < 200; n++ {
		row := table.Row{n, []string{"open", "closed"}[n%2], fmt.Sprint("title ", n), strings.Repeat("b", rng.Intn(3*pageSize))}
		if err := tx.Insert("issue", row); err != nil {
			t.Fatal(err)
		}
		want[n] = row
	}
	if _, err := tx.Next("issues"); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	stx, _ = s.Begin()
	tx, _ = table.Begin(stx, pageSize)
	for n := int64(0); n < 200; n += 7 {
		if _, err := tx.Delete("issue", n); err != nil {
			t.Fatal(err)
		}
		delete(want, n)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Checkpoint(); !ok || err != nil {
		t.Fatalf("checkpoint: %v %v", ok, err)
	}
	return want
}

func run(t *testing.T, fs vfs.FS, name string) *Report {
	t.Helper()
	r, err := Run(fs, name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestIntact checks an intact file: no finding, and statistics that match
// what build wrote, counted apart from the check.
func TestIntact(t *testing.T) {
	fs := vfs.NewSim()
	want := build(t, fs, "db")
	r := run(t, fs, "db")
	if len(r.Findings) != 0 {
		t.Fatalf("findings %v", r.Findings)
	}
	st := r.Stats
	if len(st.Tables) != 1 || st.Tables[0].Rows != len(want) {
		t.Fatalf("tables: %+v, want %d rows", st.Tables, len(want))
	}
	pages := uint64(st.CatalogPages) + st.FreePages + 1
	for _, tb := range st.Tables {
		pages += uint64(tb.Pages)
		for _, ix := range tb.Indexes {
			if ix.Entries != len(want) {
				t.Fatalf("index %s: %d entries, want %d", ix.Name, ix.Entries, len(want))
			}
			pages += uint64(ix.Pages)
		}
	}
	if pages != st.Pages || st.Pages*uint64(st.PageSize) != uint64(st.FileBytes) {
		t.Fatalf("pages: %d counted, %d in the header, %d bytes in the file", pages, st.Pages, st.FileBytes)
	}
	if st.FreePages == 0 || st.SchemaVersion != 3 {
		t.Fatalf("free pages %d, schema version %d: build left no free page or the schema is not what it wrote", st.FreePages, st.SchemaVersion)
	}
}

// copyDB copies the files of a database on the disk.
func copyDB(t *testing.T, from, to string) {
	t.Helper()
	for _, suffix := range []string{"", "-log"} {
		b, err := os.ReadFile(from + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to+suffix, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func flip(t *testing.T, name string, bit int64) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	b[bit/8] ^= 1 << (bit % 8)
	if err := os.WriteFile(name, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readAll reads the database the way a program does: each scan, and a Get
// of each row. It fails the test on a wrong row, and on a scan that ends
// without an error and without all rows. It returns the number of reads
// that returned an error.
func readAll(t *testing.T, name string, want map[int64]table.Row) (errs int) {
	t.Helper()
	s, err := store.Open(vfs.OS{}, name, store.Options{})
	if err != nil {
		return 1
	}
	defer s.Close()
	snap, _ := s.Snapshot()
	defer snap.Close()
	v, err := table.Open(snap, pageSize)
	if err != nil {
		return 1
	}
	same := func(got table.Row) {
		w, ok := want[got[0].(int64)]
		if !ok || fmt.Sprint(w) != fmt.Sprint(got) {
			t.Fatalf("a wrong row: %.60v", got)
		}
	}
	for _, index := range []string{"", "by_state", "by_title"} {
		rows, err := v.Scan("issue", table.Options{Index: index})
		if err != nil {
			errs++
			continue
		}
		n := 0
		for rows.Next() {
			same(rows.Row())
			n++
		}
		switch {
		case rows.Err() != nil:
			errs++
		case n != len(want):
			t.Fatalf("scan %q ended without an error after %d of %d rows", index, n, len(want))
		}
	}
	for k := range want {
		row, ok, err := v.Get("issue", k)
		switch {
		case err != nil:
			errs++
		case !ok:
			t.Fatalf("row %d is gone without an error", k)
		default:
			same(row)
		}
	}
	return errs
}

// TestEveryFlippedPageIsReported is the damage test of P-3. It flips one
// random bit in each page of the file in turn. The check must report that
// page. A program that reads the file must get each row right or an
// error, never a wrong or a missing row.
func TestEveryFlippedPageIsReported(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "orig")
	want := build(t, vfs.OS{}, orig)
	info, err := os.Stat(orig)
	if err != nil {
		t.Fatal(err)
	}
	pages := info.Size() / pageSize
	rng := rand.New(rand.NewSource(2))
	withErrors := 0
	for no := int64(0); no < pages; no++ {
		name := filepath.Join(dir, fmt.Sprint("db", no))
		copyDB(t, orig, name)
		flip(t, name, no*pageSize*8+rng.Int63n(pageSize*8))
		r := run(t, vfs.OS{}, name)
		// The page comes once. Damage moves no page out of its tree and
		// changes no count, so no finding says so.
		found := 0
		for _, f := range r.Findings {
			if f.HasPage && f.Page == uint64(no) {
				found++
			}
			if s := f.String(); strings.Contains(s, "belongs to no tree") || strings.Contains(s, "entries, the table has") {
				t.Fatalf("page %d flipped: a finding that damage does not cause: %s", no, s)
			}
		}
		if found != 1 {
			t.Fatalf("page %d flipped, reported %d times; findings: %v", no, found, r.Findings)
		}
		if readAll(t, name, want) > 0 {
			withErrors++
		}
	}
	// The reads touch every page but the free ones. So exactly the flips
	// on a free page leave every read right.
	free := run(t, vfs.OS{}, orig).Stats.FreePages
	if withErrors != int(pages)-int(free) {
		t.Fatalf("%d of %d flips made a read fail; %d pages are free", withErrors, pages, free)
	}
	t.Logf("%d pages, each flipped once: all reported, %d made a read return an error, none a wrong row", pages, withErrors)
}

// TestSeveralFlipsAllReported flips five pages at once. The check reports
// each of them.
func TestSeveralFlipsAllReported(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "db")
	build(t, vfs.OS{}, name)
	info, _ := os.Stat(name)
	rng := rand.New(rand.NewSource(3))
	flipped := map[uint64]bool{}
	for len(flipped) < 5 {
		no := 1 + rng.Int63n(info.Size()/pageSize-1)
		if !flipped[uint64(no)] {
			flipped[uint64(no)] = true
			flip(t, name, no*pageSize*8+rng.Int63n(pageSize*8))
		}
	}
	r := run(t, vfs.OS{}, name)
	for _, f := range r.Findings {
		if f.HasPage {
			delete(flipped, f.Page)
		}
	}
	if len(flipped) != 0 {
		t.Fatalf("pages %v not reported; findings: %v", flipped, r.Findings)
	}
}

// change runs fn in a write transaction of the database and commits.
func change(t *testing.T, fs vfs.FS, fn func(stx *store.Tx, tx *table.Tx)) {
	t.Helper()
	s, err := store.Open(fs, "db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stx, _ := s.Begin()
	tx, err := table.Begin(stx, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	fn(stx, tx)
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestStructureFaults makes faults that every checksum passes: pages whose
// content is sound and whose place is wrong.
func TestStructureFaults(t *testing.T) {
	var free uint64 // the head of the free list, set before each case
	cases := map[string]struct {
		fn   func(t *testing.T, stx *store.Tx, tx *table.Tx)
		want string
		not  string // a finding that must not come
	}{
		"a page no tree uses": {func(t *testing.T, stx *store.Tx, tx *table.Tx) {
			no, err := stx.Allocate()
			if err == nil {
				err = stx.Write(no, []byte("lost"))
			}
			if err != nil {
				t.Fatal(err)
			}
		}, "belongs to no tree and is not free", ""},
		"an index entry missing": {func(t *testing.T, stx *store.Tx, tx *table.Tx) {
			tb, _ := tx.Schema().Table("issue")
			ix, _ := tb.Index("by_state")
			c := btree.Open(ix.Root, pageSize).Cursor(stx)
			c.First()
			if _, err := btree.Open(ix.Root, pageSize).Delete(stx, c.Key()); err != nil {
				t.Fatal(err)
			}
		}, "entries, the table has", ""},
		"an index entry without a row": {func(t *testing.T, stx *store.Tx, tx *table.Tx) {
			if _, err := tx.Delete("issue", int64(1)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Insert("issue", table.Row{int64(1), "open", nil, nil}); err != nil {
				t.Fatal(err)
			}
			tb, _ := tx.Schema().Table("issue")
			if _, err := btree.Open(tb.Root, pageSize).Delete(stx, keyOf(int64(1))); err != nil {
				t.Fatal(err)
			}
		}, "an entry has no row", ""},
		"a free page overwritten": {func(t *testing.T, stx *store.Tx, tx *table.Tx) {
			if err := stx.Write(free, []byte("not free any more")); err != nil {
				t.Fatal(err)
			}
		}, "not marked free", ""},
		"a root slot no structure uses": {func(t *testing.T, stx *store.Tx, tx *table.Tx) {
			tr, err := btree.Create(stx, pageSize)
			if err == nil {
				err = stx.SetRoot(2, tr.Root)
			}
			if err != nil {
				t.Fatal(err)
			}
		}, "root slot 2", "belongs to no tree"},
		"a schema that does not decode": {func(t *testing.T, stx *store.Tx, tx *table.Tx) {
			if err := btree.Open(stx.Root(table.CatalogSlot), pageSize).Put(stx, []byte("schema"), []byte{9}); err != nil {
				t.Fatal(err)
			}
		}, "schema", ""},
	}
	for name, c := range cases {
		fs := vfs.NewSim()
		build(t, fs, "db")
		free = firstFree(t, fs)
		change(t, fs, func(stx *store.Tx, tx *table.Tx) { c.fn(t, stx, tx) })
		r := run(t, fs, "db")
		found := false
		for _, f := range r.Findings {
			found = found || strings.Contains(f.String(), c.want)
			if c.not != "" && strings.Contains(f.String(), c.not) {
				t.Errorf("%s: a finding with %q: %s", name, c.not, f)
			}
		}
		if !found {
			t.Errorf("%s: findings %v, want one with %q", name, r.Findings, c.want)
		}
	}
}

// keyOf is the stored key of an int64 primary key.
func keyOf(n int64) []byte {
	b := make([]byte, 8)
	u := uint64(n) ^ 1<<63
	for i := 7; i >= 0; i-- {
		b[i] = byte(u)
		u >>= 8
	}
	return b
}

// firstFree returns the head of the free list of the database.
func firstFree(t *testing.T, fs vfs.FS) uint64 {
	t.Helper()
	s, err := store.Open(fs, "db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, _ := s.Snapshot()
	defer snap.Close()
	free, err := snap.FreePages()
	if err != nil || len(free) == 0 {
		t.Fatalf("free pages: %v %v", free, err)
	}
	return free[0]
}

// TestUsedTwice checks the one fault that no file here can reach without a
// damaged tree first: a page claimed by two structures.
func TestUsedTwice(t *testing.T) {
	c := &checker{r: &Report{}, owner: map[uint64]string{}}
	c.use("a", []uint64{4, 5})
	c.use("b", []uint64{6, 4})
	if len(c.r.Findings) != 1 || c.r.Findings[0].Page != 4 || !strings.Contains(c.r.Findings[0].What, "used by a and by b") {
		t.Fatalf("findings: %v", c.r.Findings)
	}
}

// TestCannotCheck checks the cases that exit 2: no file, and a file
// another program holds. The check creates no file.
func TestCannotCheck(t *testing.T) {
	fs := vfs.NewSim()
	if _, err := Run(fs, "db"); !errors.Is(err, ErrCannotCheck) {
		t.Fatalf("no file: %v", err)
	}
	if ok, _ := fs.Exists("db"); ok {
		t.Fatal("the check created the file")
	}
	build(t, fs, "db")
	s, err := store.Open(fs, "db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Run(fs, "db"); !errors.Is(err, ErrCannotCheck) || !errors.Is(err, vfs.ErrLocked) {
		t.Fatalf("locked: %v", err)
	}
}

// TestDamagedLogHeader flips a bit in the header of a log that holds a
// commit. The check reports the log. A damaged header with no frame after
// it starts a new log instead, because no commit is lost.
func TestDamagedLogHeader(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "db")
	build(t, vfs.OS{}, name)
	s, err := store.Open(vfs.OS{}, name, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	stx, _ := s.Begin()
	tx, _ := table.Begin(stx, pageSize)
	if err := tx.Insert("issue", table.Row{int64(1000), "open", nil, nil}); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	flip(t, name+"-log", 20*8)
	r := run(t, vfs.OS{}, name)
	if len(r.Findings) != 1 || !strings.Contains(r.Findings[0].What, "log is damaged") {
		t.Fatalf("findings: %v", r.Findings)
	}
}

// TestLogTailIsAFinding appends bytes to the log that are no commit. The
// check reports them: after a crash they are normal, but the log cannot
// tell them from damage.
func TestLogTailIsAFinding(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "db")
	build(t, vfs.OS{}, name)
	f, err := os.OpenFile(name+"-log", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r := run(t, vfs.OS{}, name)
	if len(r.Findings) != 1 || !strings.Contains(r.Findings[0].What, "100 bytes") {
		t.Fatalf("findings %v", r.Findings)
	}
}

// TestEveryFlipInTheLogIsReported flips one bit in each frame of a log
// that holds two commits. A flip in the first commit is damage that the
// second commit shows, and the log is refused. A flip in the last commit
// looks like a commit that did not finish and is reported as bytes after
// the last complete commit. The check changes no file.
func TestEveryFlipInTheLogIsReported(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "orig")
	build(t, vfs.OS{}, orig)
	s, err := store.Open(vfs.OS{}, orig, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var firstEnd int64 // the size of the log after the first commit
	for i := int64(0); i < 2; i++ {
		if i == 1 {
			info, err := os.Stat(orig + "-log")
			if err != nil {
				t.Fatal(err)
			}
			firstEnd = info.Size()
		}
		stx, _ := s.Begin()
		tx, _ := table.Begin(stx, pageSize)
		if err := tx.Insert("issue", table.Row{1000 + i, "open", nil, strings.Repeat("x", pageSize)}); err != nil {
			t.Fatal(err)
		}
		if err := stx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	info, err := os.Stat(orig + "-log")
	if err != nil {
		t.Fatal(err)
	}
	// Header of 28 bytes, frames of 28 bytes and a page.
	const frame = 28 + pageSize
	frames := (info.Size() - 28) / frame
	firstFrames := (firstEnd - 28) / frame
	if (info.Size()-28)%frame != 0 || firstFrames < 2 || frames-firstFrames < 2 {
		t.Fatalf("the log holds %d bytes, %d frames, %d in the first commit", info.Size(), frames, firstFrames)
	}
	// Each byte of the log header. A flip in the magic or the version makes
	// the log unreadable as a log: the check cannot check and says so. Any
	// other flip is a finding. Neither says intact.
	for at := int64(0); at < 28; at++ {
		name := filepath.Join(dir, fmt.Sprint("h", at))
		copyDB(t, orig, name)
		flip(t, name+"-log", at*8)
		r, err := Run(vfs.OS{}, name)
		switch {
		case at < 12 && errors.Is(err, ErrCannotCheck):
		case err != nil:
			t.Fatalf("header byte %d: %v", at, err)
		case len(r.Findings) == 0:
			t.Fatalf("header byte %d flipped: intact", at)
		}
	}
	rng := rand.New(rand.NewSource(4))
	for f := int64(0); f < frames; f++ {
		name := filepath.Join(dir, fmt.Sprint("db", f))
		copyDB(t, orig, name)
		flip(t, name+"-log", (28+f*frame+rng.Int63n(frame))*8)
		before, _ := os.ReadFile(name + "-log")
		r := run(t, vfs.OS{}, name)
		want := "log is damaged"
		if f >= firstFrames {
			want = "bytes after its last complete commit"
		}
		if len(r.Findings) != 1 || !strings.Contains(r.Findings[0].What, want) {
			t.Fatalf("frame %d of %d, %d in the first commit: findings %v, want %q", f, frames, firstFrames, r.Findings, want)
		}
		if after, _ := os.ReadFile(name + "-log"); string(after) != string(before) {
			t.Fatalf("frame %d: the check changed the log", f)
		}
	}
	t.Logf("28 header bytes and %d frames, %d in the first commit, each flipped once: none intact", frames, firstFrames)
}

// TestCheckWritesNothing checks a database whose log is gone, as after a
// checkpoint and a copy of the file alone. The check must not create a
// log, and must leave the file as it was.
func TestCheckWritesNothing(t *testing.T) {
	fs := vfs.NewSim()
	build(t, fs, "db")
	if err := fs.Remove("db-log"); err != nil {
		t.Fatal(err)
	}
	f, _ := fs.Open("db")
	size, _ := f.Size()
	before := make([]byte, size)
	f.ReadAt(before, 0)
	f.Close()
	r := run(t, fs, "db")
	if len(r.Findings) != 0 {
		t.Fatalf("findings %v", r.Findings)
	}
	if ok, _ := fs.Exists("db-log"); ok {
		t.Fatal("the check created a log")
	}
	f, _ = fs.Open("db")
	after := make([]byte, size+1)
	n, _ := f.ReadAt(after, 0)
	f.Close()
	if n != int(size) || string(after[:n]) != string(before) {
		t.Fatal("the check changed the file")
	}
}
