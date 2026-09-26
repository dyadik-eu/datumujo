package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"testing"
)

// result is what one call returned, in a form that can be compared.
type result struct {
	n    int
	data string
	eof  bool
	err  bool
	size int64
}

func (r result) String() string {
	return fmt.Sprintf("n=%d eof=%v err=%v size=%d data=%q", r.n, r.eof, r.err, r.size, r.data)
}

// TestSimBehavesLikeOS runs the same random calls on a file of the
// operating system and on a file of Sim, and compares every result. Sim
// stands in for the disk in the crash test; where it differs from the
// real file, the crash test proves nothing about the real one.
func TestSimBehavesLikeOS(t *testing.T) {
	for seed := int64(1); seed <= 50; seed++ {
		r := rand.New(rand.NewSource(seed))
		osf, err := OS{}.Open(filepath.Join(t.TempDir(), "f"))
		if err != nil {
			t.Fatal(err)
		}
		simf, err := NewSim().Open("f")
		if err != nil {
			t.Fatal(err)
		}
		for step := 0; step < 200; step++ {
			var a, b result
			op := r.Intn(5)
			switch op {
			case 0: // write
				off := int64(r.Intn(3000))
				data := make([]byte, r.Intn(700))
				r.Read(data)
				a = write(osf, data, off)
				b = write(simf, data, off)
			case 1: // read
				off := int64(r.Intn(3500))
				n := r.Intn(700)
				a = read(osf, n, off)
				b = read(simf, n, off)
			case 2: // truncate
				size := int64(r.Intn(3500))
				a = trunc(osf, size)
				b = trunc(simf, size)
			case 3: // size
				a = size(osf)
				b = size(simf)
			case 4: // sync
				a = result{err: osf.Sync() != nil}
				b = result{err: simf.Sync() != nil}
			}
			if a != b {
				t.Fatalf("seed %d step %d op %d:\nos:  %v\nsim: %v", seed, step, op, a, b)
			}
		}
		osf.Close()
		simf.Close()
	}
}

func write(f File, data []byte, off int64) result {
	n, err := f.WriteAt(data, off)
	return result{n: n, err: err != nil}
}

func read(f File, n int, off int64) result {
	p := make([]byte, n)
	got, err := f.ReadAt(p, off)
	return result{n: got, data: string(p[:got]), eof: errors.Is(err, io.EOF), err: err != nil && !errors.Is(err, io.EOF)}
}

func trunc(f File, size int64) result {
	return result{err: f.Truncate(size) != nil}
}

func size(f File) result {
	s, err := f.Size()
	return result{size: s, err: err != nil}
}

// TestNamesBehaveLikeOS: Open creates, Exists sees it, Remove removes it,
// on both implementations; removing a missing file is an error on both.
func TestNamesBehaveLikeOS(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		fs   FS
		name string
	}{{OS{}, filepath.Join(dir, "a")}, {NewSim(), "a"}} {
		if ok, err := c.fs.Exists(c.name); ok || err != nil {
			t.Errorf("%T: exists before open: %v %v", c.fs, ok, err)
		}
		f, err := c.fs.Open(c.name)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if ok, err := c.fs.Exists(c.name); !ok || err != nil {
			t.Errorf("%T: exists after open: %v %v", c.fs, ok, err)
		}
		// Rename to a new name and back over an existing file.
		other := c.name + "2"
		if err := c.fs.Rename(c.name, other); err != nil {
			t.Errorf("%T: rename: %v", c.fs, err)
		}
		if ok, _ := c.fs.Exists(c.name); ok {
			t.Errorf("%T: old name exists after rename", c.fs)
		}
		g, _ := c.fs.Open(c.name) // a new, empty file under the old name
		g.Close()
		if err := c.fs.Rename(other, c.name); err != nil {
			t.Errorf("%T: rename over an existing file: %v", c.fs, err)
		}
		if ok, _ := c.fs.Exists(other); ok {
			t.Errorf("%T: %s exists after renaming it away", c.fs, other)
		}
		if err := c.fs.Rename(other, c.name); err == nil {
			t.Errorf("%T: renaming a missing file succeeded", c.fs)
		}
		if err := c.fs.Remove(c.name); err != nil {
			t.Errorf("%T: remove: %v", c.fs, err)
		}
		if ok, _ := c.fs.Exists(c.name); ok {
			t.Errorf("%T: exists after remove", c.fs)
		}
		if err := c.fs.Remove(c.name); err == nil {
			t.Errorf("%T: removing a missing file succeeded", c.fs)
		}
	}
}

// content reads a whole file of a Sim.
func content(t *testing.T, s *Sim, name string) []byte {
	t.Helper()
	f, err := s.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := f.Size()
	p := make([]byte, n)
	if _, err := f.ReadAt(p, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return p
}

// TestCrashKeepsWhatWasSynced: a power loss keeps everything up to the
// last Sync. Without a random source it drops everything after it.
func TestCrashKeepsWhatWasSynced(t *testing.T) {
	s := NewSim()
	f, _ := s.Open("f")
	f.WriteAt([]byte("durable"), 0)
	f.Sync()
	f.WriteAt([]byte("PENDING"), 0)
	f.WriteAt([]byte("more"), 100)
	after := s.Crash(nil)
	if got := content(t, after, "f"); string(got) != "durable" {
		t.Errorf("after crash: %q, want %q", got, "durable")
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrCrashed) {
		t.Errorf("old disk after crash: %v, want ErrCrashed", err)
	}
}

// TestCrashTearsBySector: after a power loss, each sector a pending write
// touched holds either its old or its new content, never a mix within the
// sector, and never anything else. Over many seeds each sector is seen in
// both states, and a write is seen torn: the control that the simulation
// loses parts at all.
func TestCrashTearsBySector(t *testing.T) {
	const sectorsN = 4
	old := bytes.Repeat([]byte{'o'}, sectorsN*SectorSize)
	neu := bytes.Repeat([]byte{'n'}, sectorsN*SectorSize)
	seenOld := make([]bool, sectorsN)
	seenNew := make([]bool, sectorsN)
	torn := false
	for seed := int64(0); seed < 200; seed++ {
		s := NewSim()
		f, _ := s.Open("f")
		f.WriteAt(old, 0)
		f.Sync()
		f.WriteAt(neu, 0) // one write over all sectors
		got := content(t, s.Crash(rand.New(rand.NewSource(seed))), "f")
		if len(got) != len(old) {
			t.Fatalf("seed %d: size %d", seed, len(got))
		}
		kinds := map[byte]bool{}
		for i := 0; i < sectorsN; i++ {
			sec := got[i*SectorSize : (i+1)*SectorSize]
			switch {
			case bytes.Equal(sec, old[:SectorSize]):
				seenOld[i] = true
				kinds['o'] = true
			case bytes.Equal(sec, neu[:SectorSize]):
				seenNew[i] = true
				kinds['n'] = true
			default:
				t.Fatalf("seed %d: sector %d is neither old nor new", seed, i)
			}
		}
		if len(kinds) == 2 {
			torn = true
		}
	}
	for i := 0; i < sectorsN; i++ {
		if !seenOld[i] || !seenNew[i] {
			t.Errorf("sector %d: old seen %v, new seen %v", i, seenOld[i], seenNew[i])
		}
	}
	if !torn {
		t.Error("no seed tore the write")
	}
}

// TestCrashCanLoseATruncation: a truncation after the last Sync may or may
// not survive, and both outcomes occur.
func TestCrashCanLoseATruncation(t *testing.T) {
	seen := map[int]bool{}
	for seed := int64(0); seed < 50; seed++ {
		s := NewSim()
		f, _ := s.Open("f")
		f.WriteAt(make([]byte, 1000), 0)
		f.Sync()
		f.Truncate(10)
		seen[len(content(t, s.Crash(rand.New(rand.NewSource(seed))), "f"))] = true
	}
	if !seen[10] || !seen[1000] || len(seen) != 2 {
		t.Errorf("sizes after crash: %v, want 10 and 1000", seen)
	}
}

// TestBudgetStopsTheProcess: after SetBudget(n), n changing calls succeed
// and every later call, reading ones too, returns ErrCrashed. Reads do not
// use up the budget.
func TestBudgetStopsTheProcess(t *testing.T) {
	s := NewSim()
	f, _ := s.Open("f") // creating counts: 1
	s.SetBudget(2)
	if _, err := f.ReadAt(make([]byte, 1), 0); errors.Is(err, ErrCrashed) {
		t.Fatal("a read used the budget")
	}
	if _, err := f.WriteAt([]byte("a"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("b"), 1); !errors.Is(err, ErrCrashed) {
		t.Errorf("third call: %v, want ErrCrashed", err)
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrCrashed) {
		t.Errorf("read after the crash: %v, want ErrCrashed", err)
	}
	if _, err := s.Open("g"); !errors.Is(err, ErrCrashed) {
		t.Errorf("open after the crash: %v, want ErrCrashed", err)
	}
	if s.Calls() != 3 {
		t.Errorf("calls: %d, want 3", s.Calls())
	}
	if got := content(t, s.Crash(nil), "f"); string(got) != "a" {
		t.Errorf("after crash: %q, want %q", got, "a")
	}
}

// TestCrashAfterEmptyWrite: an empty write changes nothing, also not in a
// power loss. It must not read as a truncation to size 0.
func TestCrashAfterEmptyWrite(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		s := NewSim()
		f, _ := s.Open("f")
		f.WriteAt([]byte("durable"), 0)
		f.Sync()
		f.WriteAt(nil, 3)
		if got := content(t, s.Crash(rand.New(rand.NewSource(seed))), "f"); string(got) != "durable" {
			t.Fatalf("seed %d: %q after an empty write and a crash", seed, got)
		}
	}
}

// TestCrashAfterTwoSyncs: what an earlier Sync made durable does not come
// back over what a later Sync made durable.
func TestCrashAfterTwoSyncs(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		s := NewSim()
		f, _ := s.Open("f")
		f.WriteAt([]byte("first"), 0)
		f.Sync()
		f.WriteAt([]byte("LATER"), 0)
		f.Sync()
		if got := content(t, s.Crash(rand.New(rand.NewSource(seed))), "f"); string(got) != "LATER" {
			t.Fatalf("seed %d: %q after two syncs and a crash", seed, got)
		}
	}
}

// TestLockBehavesLikeOS: on both implementations, a lock excludes a
// second one, also in the same process, and Unlock frees it.
func TestLockBehavesLikeOS(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		fs   FS
		name string
	}{{OS{}, filepath.Join(dir, "lock")}, {NewSim(), "lock"}} {
		unlock, err := c.fs.Lock(c.name)
		if err != nil {
			t.Fatalf("%T: first lock: %v", c.fs, err)
		}
		if _, err := c.fs.Lock(c.name); !errors.Is(err, ErrLocked) {
			t.Errorf("%T: second lock: %v, want ErrLocked", c.fs, err)
		}
		if err := unlock(); err != nil {
			t.Errorf("%T: unlock: %v", c.fs, err)
		}
		unlock, err = c.fs.Lock(c.name)
		if err != nil {
			t.Errorf("%T: lock after unlock: %v", c.fs, err)
		} else {
			unlock()
		}
	}
}

// TestCrashReleasesLocks: after a power loss the process that held a lock
// is gone, and a new one can take it.
func TestCrashReleasesLocks(t *testing.T) {
	s := NewSim()
	if _, err := s.Lock("lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Crash(nil).Lock("lock"); err != nil {
		t.Errorf("lock after crash: %v", err)
	}
}
