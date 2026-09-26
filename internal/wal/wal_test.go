package wal

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

const ps = 512

// img makes a sealed image of page no whose content is derived from tag.
func img(no uint64, tag int) []byte {
	p := make([]byte, ps)
	copy(p, fmt.Sprintf("page %d version %d", no, tag))
	page.Seal(p, no)
	return p
}

// state is what the log holds: the newest image of each page, and the
// page count.
type state struct {
	pages map[uint64]string
	count uint64
}

func (s state) String() string { return fmt.Sprintf("%d pages, count %d", len(s.pages), s.count) }

func (s state) equal(o state) bool {
	if s.count != o.count || len(s.pages) != len(o.pages) {
		return false
	}
	for k, v := range s.pages {
		if o.pages[k] != v {
			return false
		}
	}
	return true
}

// read returns the state of an open log, for the page numbers in nos.
func read(t *testing.T, l *Log, nos []uint64) state {
	t.Helper()
	st := state{pages: map[uint64]string{}}
	st.count, _ = l.Count()
	buf := make([]byte, ps)
	for _, no := range nos {
		ok, err := l.Read(no, l.Last(), buf)
		if err != nil {
			t.Fatalf("read page %d: %v", no, err)
		}
		if ok {
			st.pages[no] = string(buf)
		}
	}
	return st
}

func open(t *testing.T, s *vfs.Sim) (*Log, Recovered) {
	t.Helper()
	f, err := s.Open("log")
	if err != nil {
		t.Fatal(err)
	}
	l, rec, err := Open(f, ps)
	if err != nil {
		t.Fatal(err)
	}
	return l, rec
}

func TestCommitThenReopen(t *testing.T) {
	s := vfs.NewSim()
	l, rec := open(t, s)
	if rec.Commits != 0 {
		t.Fatalf("new log: %d commits", rec.Commits)
	}
	if err := l.Commit([]Page{{1, img(1, 1)}, {2, img(2, 1)}}, 3); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit([]Page{{2, img(2, 2)}}, 3); err != nil {
		t.Fatal(err)
	}
	want := read(t, l, []uint64{1, 2, 3})
	l2, rec := open(t, s.Crash(nil))
	if rec.Commits != 2 || rec.Ignored != 0 {
		t.Errorf("reopen: %+v", rec)
	}
	if got := read(t, l2, []uint64{1, 2, 3}); !got.equal(want) {
		t.Errorf("reopen: %v, want %v", got, want)
	}
	if got := want.pages[2]; got != string(img(2, 2)) {
		t.Error("page 2 is not its newest image")
	}
}

// commit is one commit of a workload.
type commit struct {
	pages []Page
	count uint64
}

func workload(seed int64, n int) []commit {
	r := rand.New(rand.NewSource(seed))
	var out []commit
	for i := 0; i < n; i++ {
		var c commit
		used := map[uint64]bool{}
		for k := 0; k < 1+r.Intn(4); k++ {
			no := uint64(1 + r.Intn(8))
			if used[no] {
				continue
			}
			used[no] = true
			c.pages = append(c.pages, Page{no, img(no, i)})
		}
		c.count = uint64(2 + r.Intn(20))
		out = append(out, c)
	}
	return out
}

func apply(st state, c commit) state {
	next := state{pages: map[uint64]string{}, count: c.count}
	for k, v := range st.pages {
		next.pages[k] = v
	}
	for _, p := range c.pages {
		next.pages[p.No] = string(p.Data)
	}
	return next
}

var allPages = []uint64{1, 2, 3, 4, 5, 6, 7, 8}

// TestCrashAtEveryCall is the crash test of P-1 at the level of pages.
// A workload of commits runs on a simulated disk that stops after k
// calls, for every k, and then loses power in several random ways. The
// log that opens after it holds exactly the commits that returned, or
// those plus the one that was running: never a part of a commit, never a
// commit that is gone after it returned (T-3, T-4).
func TestCrashAtEveryCall(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		cs := workload(seed, 12)
		// Count the calls of a full run.
		full := vfs.NewSim()
		l, _ := open(t, full)
		for _, c := range cs {
			if err := l.Commit(c.pages, c.count); err != nil {
				t.Fatal(err)
			}
		}
		calls := full.Calls()
		afterFirst, afterRunning := 0, 0
		for k := 0; k <= calls; k++ {
			s := vfs.NewSim()
			s.SetBudget(k)
			f, err := s.Open("log")
			var l *Log
			if err == nil {
				l, _, err = Open(f, ps)
			}
			acked := state{pages: map[uint64]string{}}
			running := acked
			if err == nil {
				for _, c := range cs {
					running = apply(acked, c)
					if err := l.Commit(c.pages, c.count); err != nil {
						if !errors.Is(err, vfs.ErrCrashed) {
							t.Fatalf("seed %d, k %d: %v", seed, k, err)
						}
						break
					}
					acked = running
				}
			}
			for power := int64(0); power < 16; power++ {
				// A source of its own for each run, stop and power loss:
				// with the same few sources for every stop, each stop
				// made the same choice of sectors, and no commit in flight
				// ever survived.
				var r *rand.Rand
				if power > 0 {
					r = rand.New(rand.NewSource(seed*1_000_003 + int64(k)*1_009 + power))
				}
				after := s.Crash(r)
				f, err := after.Open("log")
				if err != nil {
					t.Fatal(err)
				}
				l2, rec, err := Open(f, ps)
				if err != nil {
					t.Fatalf("seed %d, k %d, power %d: open: %v", seed, k, power, err)
				}
				got := read(t, l2, allPages)
				switch {
				case got.equal(acked):
					afterFirst++
				case got.equal(running) && !running.equal(acked):
					afterRunning++
				default:
					t.Fatalf("seed %d, k %d, power %d: log holds %v; acked %v, running %v (%+v)", seed, k, power, got, acked, running, rec)
				}
				// The log goes on after recovery.
				if err := l2.Commit([]Page{{1, img(1, 999)}}, 30); err != nil {
					t.Fatalf("seed %d, k %d, power %d: commit after recovery: %v", seed, k, power, err)
				}
				l3, _ := open(t, after.Crash(nil))
				if got := read(t, l3, allPages); got.pages[1] != string(img(1, 999)) || got.count != 30 {
					t.Fatalf("seed %d, k %d, power %d: commit after recovery lost: %v", seed, k, power, got)
				}
			}
		}
		// Control: both outcomes occur, so the test looked at commits in
		// flight and not only at clean stops.
		t.Logf("seed %d: %d stops, %d recoveries to the acked state, %d to the running commit", seed, calls+1, afterFirst, afterRunning)
		if afterFirst == 0 || afterRunning == 0 {
			t.Errorf("seed %d: %d recoveries to the acked state, %d to the running commit", seed, afterFirst, afterRunning)
		}
	}
}

// TestOldGenerationDoesNotChain: frames of an earlier generation of the
// log, still in the file behind a new header, are not used. That is what
// the salt is for.
func TestOldGenerationDoesNotChain(t *testing.T) {
	s := vfs.NewSim()
	l, _ := open(t, s)
	l.Commit([]Page{{1, img(1, 1)}}, 2)
	l.Commit([]Page{{2, img(2, 1)}}, 3)
	f, _ := s.Open("log")
	size, _ := f.Size()
	old := make([]byte, size-headerSize)
	f.ReadAt(old, headerSize)
	if err := l.reset(); err != nil {
		t.Fatal(err)
	}
	// A power loss that kept the new header but lost the truncation.
	f.WriteAt(old, headerSize)
	f.Sync()
	l2, rec := open(t, s.Crash(nil))
	if rec.Commits != 0 || rec.Ignored != int64(len(old)) {
		t.Errorf("new generation with old frames behind it: %+v", rec)
	}
	if got := read(t, l2, allPages); len(got.pages) != 0 {
		t.Errorf("old frames were used: %v", got)
	}
}

// TestDamagedFrameEndsTheLog: a flipped bit in a committed frame ends the
// log before that commit. The log cannot tell this from a commit that did
// not finish; it reports the bytes it left (design.md).
func TestDamagedFrameEndsTheLog(t *testing.T) {
	s := vfs.NewSim()
	l, _ := open(t, s)
	l.Commit([]Page{{1, img(1, 1)}}, 2)
	l.Commit([]Page{{2, img(2, 1)}}, 3)
	l.Commit([]Page{{3, img(3, 1)}}, 4)
	f, _ := s.Open("log")
	frame := int64(frameHeaderSize + ps)
	off := int64(headerSize) + frame + 100 // inside the second commit
	b := make([]byte, 1)
	f.ReadAt(b, off)
	b[0] ^= 1
	f.WriteAt(b, off)
	f.Sync()
	l2, rec := open(t, s.Crash(nil))
	if rec.Commits != 1 || rec.Ignored != 2*frame {
		t.Errorf("after damage: %+v", rec)
	}
	if got := read(t, l2, allPages); len(got.pages) != 1 {
		t.Errorf("after damage: %v", got)
	}
}

// TestLeftoverTailIsCut: bytes of a commit that did not finish are cut
// off by the next commit, so a later reopen sees the new commit and
// nothing behind it.
func TestLeftoverTailIsCut(t *testing.T) {
	s := vfs.NewSim()
	l, _ := open(t, s)
	l.Commit([]Page{{1, img(1, 1)}}, 2)
	f, _ := s.Open("log")
	size, _ := f.Size()
	// Three frames of garbage behind the commit, as a torn commit leaves.
	garbage := bytes.Repeat([]byte{0xAB}, 3*(frameHeaderSize+ps))
	f.WriteAt(garbage, size)
	f.Sync()
	s = s.Crash(nil)
	l2, rec := open(t, s)
	if rec.Ignored != int64(len(garbage)) {
		t.Fatalf("ignored %d, want %d", rec.Ignored, len(garbage))
	}
	if err := l2.Commit([]Page{{2, img(2, 1)}}, 3); err != nil {
		t.Fatal(err)
	}
	after := s.Crash(nil)
	g, _ := after.Open("log")
	n, _ := g.Size()
	if want := size + int64(frameHeaderSize+ps); n != want {
		t.Errorf("log size %d after the next commit, want %d", n, want)
	}
}

func TestOpenRejects(t *testing.T) {
	good := func() *vfs.Sim {
		s := vfs.NewSim()
		l, _ := open(t, s)
		l.Commit([]Page{{1, img(1, 1)}}, 2)
		return s.Crash(nil)
	}
	edit := func(off int64, b byte) error {
		s := good()
		f, _ := s.Open("log")
		f.WriteAt([]byte{b}, off)
		_, _, err := Open(f, ps)
		return err
	}
	var d *DamagedError
	if err := edit(0, 'X'); !errors.Is(err, ErrNotLog) {
		t.Errorf("other magic: %v", err)
	}
	if err := edit(20, 0x55); !errors.As(err, &d) {
		t.Errorf("damaged header with frames behind it: %v", err)
	}
	s := good()
	f, _ := s.Open("log")
	if _, _, err := Open(f, 1024); !errors.As(err, &d) {
		t.Errorf("other page size: %v", err)
	}
	// A partial header and nothing else is a log that never held a commit.
	s = vfs.NewSim()
	f, _ = s.Open("log")
	f.WriteAt(Magic[:5], 0)
	if _, rec, err := Open(f, ps); err != nil || rec.Commits != 0 {
		t.Errorf("partial header: %+v %v", rec, err)
	}
}

func TestCommitRefuses(t *testing.T) {
	l, _ := open(t, vfs.NewSim())
	unsealed := make([]byte, ps)
	for name, c := range map[string]commit{
		"no pages":     {nil, 2},
		"count 0":      {[]Page{{1, img(1, 1)}}, 0},
		"unsealed":     {[]Page{{1, unsealed}}, 2},
		"wrong number": {[]Page{{2, img(1, 1)}}, 2},
		"short page":   {[]Page{{1, img(1, 1)[:100]}}, 2},
		"page twice":   {[]Page{{1, img(1, 1)}, {1, img(1, 2)}}, 2},
	} {
		if err := l.Commit(c.pages, c.count); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if l.Commits() != 0 {
		t.Errorf("refused commits counted: %d", l.Commits())
	}
}

// TestDamagedCountEndsTheLog: the page count of a frame is covered by the
// chain. A frame whose count was changed, so that a frame in the middle
// of a commit looks like its end, is not used.
func TestDamagedCountEndsTheLog(t *testing.T) {
	s := vfs.NewSim()
	l, _ := open(t, s)
	l.Commit([]Page{{1, img(1, 1)}}, 2)
	l.Commit([]Page{{2, img(2, 1)}, {3, img(3, 1)}}, 4)
	f, _ := s.Open("log")
	frame := int64(frameHeaderSize + ps)
	// The first frame of the second commit carries count 0; make it 9.
	f.WriteAt([]byte{9}, int64(headerSize)+frame+15)
	f.Sync()
	l2, rec := open(t, s.Crash(nil))
	if rec.Commits != 1 {
		t.Errorf("after changing a count: %+v", rec)
	}
	if n, _ := l2.Count(); n != 2 {
		t.Errorf("page count %d, want 2", n)
	}
}

// failOnce fails its first Sync, as a disk that reports an I/O error
// once, and works after it.
type failOnce struct {
	vfs.File
	failed bool
}

func (f *failOnce) Sync() error {
	if !f.failed {
		f.failed = true
		return errors.New("i/o error")
	}
	return f.File.Sync()
}

// TestFailedCommitLeavesNoTail: a commit whose Sync fails has written its
// frames. The next commit cuts them off, so that a reopen sees the log up
// to the next commit and nothing of the failed one.
func TestFailedCommitLeavesNoTail(t *testing.T) {
	s := vfs.NewSim()
	l, _ := open(t, s)
	l.Commit([]Page{{1, img(1, 1)}}, 2)
	f, _ := s.Open("log")
	l.f = &failOnce{File: f}
	if err := l.Commit([]Page{{2, img(2, 1)}, {3, img(3, 1)}, {4, img(4, 1)}}, 5); err == nil {
		t.Fatal("the failing Sync did not fail the commit")
	}
	if err := l.Commit([]Page{{5, img(5, 1)}}, 6); err != nil {
		t.Fatal(err)
	}
	l2, rec := open(t, s.Crash(nil))
	if rec.Commits != 2 || rec.Ignored != 0 {
		t.Errorf("after a failed commit: %+v", rec)
	}
	got := read(t, l2, allPages)
	if _, ok := got.pages[2]; ok || got.pages[5] != string(img(5, 1)) {
		t.Errorf("after a failed commit: %v", got)
	}
}

// TestReadAtAnOlderCommit: a reader of commit n sees each page as the
// commits up to n left it, not as later ones did.
func TestReadAtAnOlderCommit(t *testing.T) {
	l, _ := open(t, vfs.NewSim())
	l.Commit([]Page{{1, img(1, 1)}, {2, img(2, 1)}}, 3) // commit 1
	l.Commit([]Page{{1, img(1, 2)}}, 3)                 // commit 2
	l.Commit([]Page{{2, img(2, 3)}}, 3)                 // commit 3
	buf := make([]byte, ps)
	for _, c := range []struct {
		no   uint64
		upTo int
		tag  int // 0: no image
	}{{1, 0, 0}, {1, 1, 1}, {1, 2, 2}, {1, 3, 2}, {2, 1, 1}, {2, 2, 1}, {2, 3, 3}} {
		ok, err := l.Read(c.no, c.upTo, buf)
		if err != nil {
			t.Fatal(err)
		}
		if c.tag == 0 {
			if ok {
				t.Errorf("page %d up to commit %d: an image, want none", c.no, c.upTo)
			}
			continue
		}
		if !ok || string(buf) != string(img(c.no, c.tag)) {
			t.Errorf("page %d up to commit %d: want version %d", c.no, c.upTo, c.tag)
		}
	}
}

// TestNumbersGoOnAfterReset: after Reset the next commit gets the next
// number. A reader of an earlier commit does not see it, and finds no
// image of the old generation either.
func TestNumbersGoOnAfterReset(t *testing.T) {
	l, _ := open(t, vfs.NewSim())
	l.Commit([]Page{{1, img(1, 1)}}, 2)
	l.Commit([]Page{{1, img(1, 2)}}, 2)
	if err := l.Reset(); err != nil {
		t.Fatal(err)
	}
	if l.Last() != 2 || l.Commits() != 0 {
		t.Fatalf("after reset: last %d, commits %d", l.Last(), l.Commits())
	}
	l.Commit([]Page{{1, img(1, 3)}}, 2) // commit 3
	buf := make([]byte, ps)
	if ok, _ := l.Read(1, 2, buf); ok {
		t.Error("a reader of commit 2 sees an image after the reset")
	}
	if ok, _ := l.Read(1, 3, buf); !ok || string(buf) != string(img(1, 3)) {
		t.Error("commit 3 not readable")
	}
}

func TestNewest(t *testing.T) {
	l, _ := open(t, vfs.NewSim())
	l.Commit([]Page{{3, img(3, 1)}, {1, img(1, 1)}}, 4)
	l.Commit([]Page{{1, img(1, 2)}}, 4)
	var got []string
	l.Newest(func(no uint64, p []byte) error {
		got = append(got, fmt.Sprintf("%d:%s", no, bytes.TrimRight(p[:30], "\x00")))
		return nil
	})
	want := []string{"1:page 1 version 2", "3:page 3 version 1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("newest: %v, want %v", got, want)
	}
}

// TestDamageAfterOpenIsFound: an image damaged in the log file after Open
// is not handed out, neither to a reader nor to a checkpoint through
// Newest, which would copy it into the database file.
func TestDamageAfterOpenIsFound(t *testing.T) {
	s := vfs.NewSim()
	l, _ := open(t, s)
	l.Commit([]Page{{1, img(1, 1)}}, 2)
	f, _ := s.Open("log")
	off := int64(headerSize + frameHeaderSize + 7)
	b := make([]byte, 1)
	f.ReadAt(b, off)
	b[0] ^= 0x10
	f.WriteAt(b, off)
	if _, err := l.Read(1, l.Last(), make([]byte, ps)); !errors.Is(err, page.ErrDamaged) {
		t.Errorf("Read: %v", err)
	}
	copied := 0
	err := l.Newest(func(uint64, []byte) error { copied++; return nil })
	if !errors.Is(err, page.ErrDamaged) || copied != 0 {
		t.Errorf("Newest: %v, %d pages handed out", err, copied)
	}
}
