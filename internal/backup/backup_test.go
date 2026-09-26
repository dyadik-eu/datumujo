package backup

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/check"
	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

const pageSize = 1024

var def = table.Def{Name: "t", Columns: []table.Column{
	{Name: "k", Type: table.Int64},
	{Name: "v", Type: table.String, Null: true},
	{Name: "g", Type: table.Int64, Null: true},
}, Key: []string{"k"}}

// create makes a database with the table and an index on g. The values
// of v run over pages, too long for a key.
func create(t *testing.T, fs vfs.FS, name string) *store.Store {
	t.Helper()
	s, err := store.Open(fs, name, store.Options{PageSize: pageSize})
	if err != nil {
		t.Fatal(err)
	}
	stx, _ := s.Begin()
	tx, _ := table.Begin(stx, pageSize)
	if err := tx.CreateTable(def); err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateIndex("t", table.IndexDef{Name: "by_g", Columns: []string{"g"}}); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	return s
}

// insert adds row k and increments the counter, in one commit.
func insert(s *store.Store, k int64) error {
	stx, err := s.Begin()
	if err != nil {
		return err
	}
	tx, err := table.Begin(stx, pageSize)
	if err == nil {
		err = tx.Insert("t", table.Row{k, fmt.Sprint("value ", k, " ", string(make([]byte, k%3*pageSize))), k % 5})
	}
	if err == nil {
		_, err = tx.Next("rows")
	}
	if err != nil {
		stx.Rollback()
		return err
	}
	return stx.Commit()
}

// state reads a copy: the keys in order, and the counter. It fails the
// test if the check does not accept the copy.
func state(t *testing.T, fs vfs.FS, name string) ([]int64, uint64) {
	t.Helper()
	r, err := check.Run(fs, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != 0 {
		t.Fatalf("%s: findings %v", name, r.Findings)
	}
	s, err := store.Open(fs, name, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, _ := s.Snapshot()
	defer snap.Close()
	v, err := table.Open(snap, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := v.Scan("t", table.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var keys []int64
	for rows.Next() {
		keys = append(keys, rows.Row()[0].(int64))
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	n, err := v.Counter("rows")
	if err != nil {
		t.Fatal(err)
	}
	return keys, n
}

// TestBackupWhileWriting takes copies while a writer adds one row per
// commit. Each copy must hold the rows 0 to k-1 and the counter k, for
// one k. So it is one commit for the table, its index and the catalog.
func TestBackupWhileWriting(t *testing.T) {
	fs := vfs.NewSim()
	s := create(t, fs, "db")
	defer s.Close()
	const rows = 300
	var wg sync.WaitGroup
	var werr error
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for k := int64(0); k < rows; k++ {
			if err := insert(s, k); err != nil {
				werr = err
				return
			}
			if k%40 == 39 {
				// A checkpoint does not run while a copy holds a
				// snapshot, and that is fine.
				if _, err := s.Checkpoint(); err != nil {
					werr = err
					return
				}
			}
		}
	}()
	copies, midway := 0, 0
	for running := true; running; copies++ {
		select {
		case <-done:
			running = false
		default:
		}
		name := fmt.Sprint("copy", copies)
		if err := Backup(s, fs, name); err != nil {
			t.Fatal(err)
		}
		keys, n := state(t, fs, name)
		for i, k := range keys {
			if k != int64(i) {
				t.Fatalf("%s: row %d has key %d", name, i, k)
			}
		}
		if n != uint64(len(keys)) {
			t.Fatalf("%s: %d rows and counter %d: not one commit", name, len(keys), n)
		}
		if len(keys) > 0 && len(keys) < rows {
			midway++
		}
	}
	wg.Wait()
	if werr != nil {
		t.Fatal(werr)
	}
	// Without copies during the writes, the test did not show O-1.
	if midway < 3 {
		t.Fatalf("%d copies, %d of them while the writer ran", copies, midway)
	}
	t.Logf("%d copies, %d taken while the writer ran", copies, midway)
}

// TestBackupRefuses checks that a copy never replaces a file: not the
// target, and not a log next to it.
func TestBackupRefuses(t *testing.T) {
	fs := vfs.NewSim()
	s := create(t, fs, "db")
	defer s.Close()
	for _, existing := range []string{"copy", "copy-log"} {
		f, _ := fs.Open(existing)
		f.WriteAt([]byte("keep"), 0)
		f.Sync()
		f.Close()
		if err := Backup(s, fs, "copy"); !errors.Is(err, ErrExists) {
			t.Fatalf("%s exists: %v", existing, err)
		}
		f, _ = fs.Open(existing)
		b := make([]byte, 4)
		f.ReadAt(b, 0)
		f.Close()
		if string(b) != "keep" {
			t.Fatalf("%s was changed", existing)
		}
		fs.Remove(existing)
	}
	if err := Restore(fs, "db", "db"); err == nil || !strings.Contains(err.Error(), "both source and target") {
		t.Fatalf("restore onto the source: %v", err)
	}
}

// TestLeftoverOfACopy puts the files of a copy that did not finish where
// the next copy writes: a longer file and a log. The next copy removes
// both first, so it has no bytes of the old one and is checked alone.
func TestLeftoverOfACopy(t *testing.T) {
	fs := vfs.NewSim()
	s := create(t, fs, "db")
	defer s.Close()
	for _, name := range []string{"copy-new", "copy-new-log"} {
		f, _ := fs.Open(name)
		f.WriteAt(make([]byte, 100*pageSize), 0)
		f.Sync()
		f.Close()
	}
	if err := Backup(s, fs, "copy"); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot()
	want := int64(snap.Count()) * pageSize
	snap.Close()
	f, _ := fs.Open("copy")
	size, _ := f.Size()
	f.Close()
	if size != want {
		t.Fatalf("the copy has %d bytes, its pages %d", size, want)
	}
}

// TestDamagedSource flips a bit in a page of the database. The copy stops
// with an error that names the page and leaves no file.
func TestDamagedSource(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "db")
	s := create(t, vfs.OS{}, name)
	for k := int64(0); k < 20; k++ {
		if err := insert(s, k); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.Checkpoint(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	s.Close()
	b, _ := os.ReadFile(name)
	const no = 3
	b[no*pageSize+50] ^= 1
	os.WriteFile(name, b, 0o644)
	dest := filepath.Join(dir, "copy")
	err := Restore(vfs.OS{}, name, dest)
	if !errors.Is(err, page.ErrDamaged) || !strings.HasPrefix(fmt.Sprint(err), fmt.Sprintf("backup: page %d:", no)) {
		t.Fatalf("copy of a damaged page: %v", err)
	}
	for _, left := range []string{dest, dest + "-new"} {
		if _, err := os.Stat(left); !os.IsNotExist(err) {
			t.Fatalf("%s was left: %v", left, err)
		}
	}
}

// TestRestore restores a database whose last commits are only in its log.
// The copy has them, and the check accepts it (O-2).
func TestRestore(t *testing.T) {
	fs := vfs.NewSim()
	s := create(t, fs, "db")
	for k := int64(0); k < 30; k++ {
		if err := insert(s, k); err != nil {
			t.Fatal(err)
		}
		if k == 9 {
			if ok, err := s.Checkpoint(); !ok || err != nil {
				t.Fatal(ok, err)
			}
		}
	}
	s.Close()
	if err := Restore(fs, "db", "restored"); err != nil {
		t.Fatal(err)
	}
	keys, n := state(t, fs, "restored")
	if len(keys) != 30 || n != 30 {
		t.Fatalf("restored %d rows, counter %d", len(keys), n)
	}
	for _, left := range []string{"restored-log", "restored-new", "restored-new-lock"} {
		if ok, _ := fs.Exists(left); ok {
			t.Fatalf("%s was left", left)
		}
	}
	// The restored database takes writes.
	r, err := store.Open(fs, "restored", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := insert(r, 30); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if keys, n := state(t, fs, "restored"); len(keys) != 31 || n != 31 {
		t.Fatalf("after a write: %d rows, counter %d", len(keys), n)
	}
	if err := Restore(fs, "db", "restored"); !errors.Is(err, ErrExists) {
		t.Fatalf("restore over a database: %v", err)
	}
	// A restore from a copy reads it and writes nothing next to it.
	if err := Restore(fs, "restored", "again"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := fs.Exists("restored-log"); !ok {
		t.Fatal("control: the restored database, opened for a write, has a log")
	}
	if err := Restore(fs, "again", "third"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := fs.Exists("again-log"); ok {
		t.Fatal("a restore wrote a log next to its source")
	}
}

// TestCheckRefusesTheCopy copies a database whose pages pass their
// checksums and whose structure does not: a page that no tree uses. The
// check refuses the copy, and no file is left under the target name.
func TestCheckRefusesTheCopy(t *testing.T) {
	fs := vfs.NewSim()
	s := create(t, fs, "db")
	stx, _ := s.Begin()
	no, _ := stx.Allocate()
	if err := stx.Write(no, []byte("lost")); err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}
	err := Backup(s, fs, "copy")
	s.Close()
	if !errors.Is(err, ErrCheckFailed) || !strings.Contains(err.Error(), "belongs to no tree") {
		t.Fatalf("copy of a database with a lost page: %v", err)
	}
	for _, left := range []string{"copy", "copy-new", "copy-new-lock"} {
		if ok, _ := fs.Exists(left); ok {
			t.Fatalf("%s was left", left)
		}
	}
}

// TestCrashDuringBackup stops a copy after each of its calls and cuts the
// power in 32 ways. The target is then absent or a copy that the check
// accepts, never part of one. The next copy to the same name works.
func TestCrashDuringBackup(t *testing.T) {
	prepare := func(fs *vfs.Sim) {
		s := create(t, fs, "db")
		for k := int64(0); k < 12; k++ {
			if err := insert(s, k); err != nil {
				t.Fatal(err)
			}
		}
		s.Close()
	}
	base := vfs.NewSim()
	prepare(base)
	start := base.Calls()
	if err := Restore(base, "db", "copy"); err != nil {
		t.Fatal(err)
	}
	calls := base.Calls()
	whole, absent := 0, 0
	// SetBudget counts from the moment it is set, so k is the number of
	// calls the copy may make.
	for k := 0; k <= calls-start; k++ {
		fs := vfs.NewSim()
		prepare(fs)
		fs.SetBudget(k)
		Restore(fs, "db", "copy")
		for power := int64(1); power <= 32; power++ {
			after := fs.Crash(rand.New(rand.NewSource(int64(k)*977 + power)))
			if ok, _ := after.Exists("copy"); !ok {
				absent++
				if err := Restore(after, "db", "copy"); err != nil {
					t.Fatalf("k %d, power %d: the next copy: %v", k, power, err)
				}
			} else {
				whole++
			}
			keys, n := state(t, after, "copy")
			if len(keys) != 12 || n != 12 {
				t.Fatalf("k %d, power %d: %d rows, counter %d", k, power, len(keys), n)
			}
		}
	}
	if whole == 0 || absent == 0 {
		t.Fatalf("%d outcomes with a copy, %d without", whole, absent)
	}
	t.Logf("%d stops, 32 power losses each: %d with the copy, %d without it", calls-start+1, whole, absent)
}
