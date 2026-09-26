package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/vfs"
	"github.com/dyadik-eu/datumujo/internal/wal"
)

// ps is two sectors, so that a power loss can tear a page, the header page
// in the file included. With one sector per page it never did.
const ps = 1024

func mustOpen(t *testing.T, fs vfs.FS) *Store {
	t.Helper()
	s, err := Open(fs, "db", Options{PageSize: ps})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// content returns the text at the start of a page read into buf.
func content(buf []byte) string {
	return strings.TrimRight(string(buf[:64]), "\x00")
}

// readAll reads pages 1 to count-1 through a snapshot.
func readAll(t *testing.T, s *Store) (uint64, map[uint64]string) {
	t.Helper()
	r, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := map[uint64]string{}
	buf := make([]byte, ps)
	for no := uint64(1); no < r.Count(); no++ {
		if err := r.Read(no, buf); err != nil {
			t.Fatalf("page %d: %v", no, err)
		}
		out[no] = content(buf)
	}
	return r.Count(), out
}

func write(t *testing.T, s *Store, pages map[uint64]string, allocate int) {
	t.Helper()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < allocate; i++ {
		if _, err := tx.Allocate(); err != nil {
			t.Fatal(err)
		}
	}
	for no, c := range pages {
		if err := tx.Write(no, []byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteReadReopenCheckpoint(t *testing.T) {
	fs := vfs.NewSim()
	s := mustOpen(t, fs)
	write(t, s, map[uint64]string{1: "one", 2: "two"}, 2)
	write(t, s, map[uint64]string{2: "TWO"}, 0)
	want := map[uint64]string{1: "one", 2: "TWO"}
	check := func(when string, s *Store) {
		t.Helper()
		n, got := readAll(t, s)
		if n != 3 || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %d pages %v, want 3 %v", when, n, got, want)
		}
	}
	check("before reopen", s)
	s.Close()
	fs = fs.Crash(nil)
	s = mustOpen(t, fs)
	check("after reopen", s)
	if ok, err := s.Checkpoint(); !ok || err != nil {
		t.Fatalf("checkpoint: %v %v", ok, err)
	}
	if s.log.Commits() != 0 {
		t.Errorf("log holds %d commits after the checkpoint", s.log.Commits())
	}
	check("after checkpoint", s)
	s.Close()
	s = mustOpen(t, fs.Crash(nil))
	check("after checkpoint and reopen", s)
}

// TestSnapshotDoesNotSeeLaterCommits: a snapshot reads the database as of
// the commit it started at, while the writer goes on (T-2).
func TestSnapshotDoesNotSeeLaterCommits(t *testing.T) {
	s := mustOpen(t, vfs.NewSim())
	write(t, s, map[uint64]string{1: "v1"}, 1)
	old, _ := s.Snapshot()
	defer old.Close()
	write(t, s, map[uint64]string{1: "v2", 2: "new"}, 1)
	buf := make([]byte, ps)
	old.Read(1, buf)
	if content(buf) != "v1" || old.Count() != 2 {
		t.Errorf("old snapshot: %q, %d pages", content(buf), old.Count())
	}
	// A page past the end is the caller's mistake, not a damaged page:
	// the check command must not count it as damage.
	if err := old.Read(2, buf); err == nil || errors.Is(err, page.ErrDamaged) {
		t.Errorf("old snapshot reads page 2, which did not exist at its commit: %v", err)
	}
	n, got := readAll(t, s)
	if n != 3 || got[1] != "v2" || got[2] != "new" {
		t.Errorf("new snapshot: %d %v", n, got)
	}
}

// TestOldSnapshotHoldsCheckpoint: while a snapshot of an earlier commit
// is open, Checkpoint does nothing, and the log keeps every commit (T-5,
// D-2). When it closes, the checkpoint runs, and a snapshot of the last
// commit does not stop it.
func TestOldSnapshotHoldsCheckpoint(t *testing.T) {
	s := mustOpen(t, vfs.NewSim())
	write(t, s, map[uint64]string{1: "v1"}, 1)
	old, _ := s.Snapshot()
	for i := 2; i <= 5; i++ {
		write(t, s, map[uint64]string{1: fmt.Sprintf("v%d", i)}, 0)
	}
	if ok, err := s.Checkpoint(); ok || err != nil {
		t.Fatalf("checkpoint with an old snapshot open: %v %v", ok, err)
	}
	if s.log.Commits() != 5 {
		t.Errorf("log holds %d commits, want 5", s.log.Commits())
	}
	buf := make([]byte, ps)
	old.Read(1, buf)
	if content(buf) != "v1" {
		t.Errorf("old snapshot after the refused checkpoint: %q", content(buf))
	}
	old.Close()
	cur, _ := s.Snapshot()
	if ok, err := s.Checkpoint(); !ok || err != nil {
		t.Fatalf("checkpoint with only a current snapshot: %v %v", ok, err)
	}
	cur.Read(1, buf)
	if content(buf) != "v5" {
		t.Errorf("current snapshot after the checkpoint: %q", content(buf))
	}
	cur.Close()
}

// TestReadersAndWriterTogether runs readers and a writer that commits and
// checkpoints in goroutines. Every commit writes the same version to all
// pages, so a consistent snapshot sees one version on all of them, and a
// later snapshot never an older one. Run with -race.
func TestReadersAndWriterTogether(t *testing.T) {
	s := mustOpen(t, vfs.NewSim())
	const pages = 6
	tx, _ := s.Begin()
	for i := 0; i < pages; i++ {
		tx.Allocate()
	}
	tx.Commit()
	const versions = 150
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, ps)
			last := -1
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := s.Snapshot()
				if err != nil {
					errs <- err
					return
				}
				seen := -2
				for no := uint64(1); no <= pages; no++ {
					if err := snap.Read(no, buf); err != nil {
						errs <- fmt.Errorf("page %d: %v", no, err)
						snap.Close()
						return
					}
					v := -1
					fmt.Sscanf(content(buf), "v%d", &v)
					if seen == -2 {
						seen = v
					} else if v != seen {
						errs <- fmt.Errorf("one snapshot sees version %d and %d", seen, v)
						snap.Close()
						return
					}
				}
				snap.Close()
				if seen < last {
					errs <- fmt.Errorf("a later snapshot sees version %d after %d", seen, last)
					return
				}
				last = seen
			}
		}()
	}
	for v := 0; v < versions; v++ {
		tx, err := s.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for no := uint64(1); no <= pages; no++ {
			tx.Write(no, []byte(fmt.Sprintf("v%d", v)))
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if v%10 == 0 {
			if _, err := s.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// op is one change inside a transaction of a workload.
type op struct {
	kind string // "alloc", "write", "free", "root"
	no   uint64 // page for write, free, root
	text string // content for alloc and write
	slot int    // root slot
}

// step is one step of a workload: a transaction or a checkpoint.
type step struct {
	checkpoint bool
	ops        []op
}

// model is what the database should hold: page count, the free list with
// its head last, the roots, and the content of every page in use.
type model struct {
	count uint64
	free  []uint64
	roots [page.Roots]uint64
	pages map[uint64]string
}

func newModel() model { return model{count: 1, pages: map[uint64]string{}} }

func (m model) clone() model {
	out := m
	out.free = append([]uint64(nil), m.free...)
	out.pages = map[uint64]string{}
	for k, v := range m.pages {
		out.pages[k] = v
	}
	return out
}

// alloc returns the page Allocate hands out: the head of the free list,
// else a new one.
func (m *model) alloc() uint64 {
	if n := len(m.free); n > 0 {
		no := m.free[n-1]
		m.free = m.free[:n-1]
		return no
	}
	m.count++
	return m.count - 1
}

func (m model) apply(st step) model {
	out := m.clone()
	for _, o := range st.ops {
		switch o.kind {
		case "alloc":
			out.pages[out.alloc()] = o.text
		case "write":
			out.pages[o.no] = o.text
		case "free":
			delete(out.pages, o.no)
			out.free = append(out.free, o.no)
		case "root":
			out.roots[o.slot] = o.no
		}
	}
	return out
}

// state is what a snapshot of the database shows.
type state struct {
	count, freeCount uint64
	roots            [page.Roots]uint64
	pages            map[uint64]string
}

func readState(t *testing.T, s *Store) state {
	t.Helper()
	r, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st := state{count: r.Count(), freeCount: r.FreeCount(), pages: map[uint64]string{}}
	for i := range st.roots {
		st.roots[i] = r.Root(i)
	}
	buf := make([]byte, ps)
	for no := uint64(1); no < r.Count(); no++ {
		if err := r.Read(no, buf); err != nil {
			t.Fatalf("page %d: %v", no, err)
		}
		st.pages[no] = content(buf)
	}
	return st
}

// matches reports whether the database state is the model: header fields
// equal, pages in use hold their content, free pages carry the mark.
func (m model) matches(st state) bool {
	if st.count != m.count || st.freeCount != uint64(len(m.free)) || st.roots != m.roots {
		return false
	}
	for no := uint64(1); no < m.count; no++ {
		if c, used := m.pages[no]; used {
			if st.pages[no] != c {
				return false
			}
		} else if !strings.HasPrefix(st.pages[no], string(freeMark[:])) {
			return false
		}
	}
	return true
}

func (m model) String() string {
	return fmt.Sprintf("count %d, free %v, roots %v, pages %v", m.count, m.free, m.roots, m.pages)
}

func workload(seed int64) []step {
	r := rand.New(rand.NewSource(seed))
	m := newModel()
	var out []step
	for i := 0; i < 16; i++ {
		if i > 0 && r.Intn(4) == 0 {
			out = append(out, step{checkpoint: true})
			continue
		}
		var st step
		cur := m.clone()
		for k := 0; k < 1+r.Intn(4); k++ {
			used := make([]uint64, 0, len(cur.pages))
			for no := range cur.pages {
				used = append(used, no)
			}
			sort.Slice(used, func(a, b int) bool { return used[a] < used[b] })
			var o op
			switch n := r.Intn(10); {
			case len(used) == 0 || n < 4:
				o = op{kind: "alloc", text: fmt.Sprintf("s%d-a%d", i, k)}
			case n < 7:
				o = op{kind: "write", no: used[r.Intn(len(used))], text: fmt.Sprintf("s%d-w%d", i, k)}
			case n < 9:
				o = op{kind: "free", no: used[r.Intn(len(used))]}
			default:
				o = op{kind: "root", slot: r.Intn(page.Roots), no: used[r.Intn(len(used))]}
			}
			st.ops = append(st.ops, o)
			cur = cur.apply(step{ops: []op{o}})
		}
		m = cur
		out = append(out, st)
	}
	return out
}

// run applies the workload to a store on fs until a call fails. It
// returns the model of what was acked and of the step in flight.
func run(fs vfs.FS, steps []step) (acked, running model) {
	acked = newModel()
	running = acked
	s, err := Open(fs, "db", Options{PageSize: ps})
	if err != nil {
		return
	}
	for _, st := range steps {
		if st.checkpoint {
			running = acked
			if _, err := s.Checkpoint(); err != nil {
				return
			}
			continue
		}
		running = acked.apply(st)
		tx, err := s.Begin()
		if err != nil {
			return
		}
		for _, o := range st.ops {
			switch o.kind {
			case "alloc":
				no, err := tx.Allocate()
				if err == nil {
					err = tx.Write(no, []byte(o.text))
				}
				if err != nil {
					tx.Rollback()
					return
				}
			case "write":
				tx.Write(o.no, []byte(o.text))
			case "free":
				tx.Free(o.no)
			case "root":
				tx.SetRoot(o.slot, o.no)
			}
		}
		if err := tx.Commit(); err != nil {
			return
		}
		acked = running
	}
	return
}

// TestCrashAtEveryCall is the crash test of P-1 for the store: commits
// with allocations, frees and roots, and checkpoints, stopped after every
// call, then a power loss in 24 ways. The database opens with the acked
// state, or with the step in flight applied, and takes a commit after it.
func TestCrashAtEveryCall(t *testing.T) {
	kinds := map[string]int{}
	for seed := int64(1); seed <= 4; seed++ {
		steps := workload(seed)
		for _, st := range steps {
			if st.checkpoint {
				kinds["checkpoint"]++
			}
			for _, o := range st.ops {
				kinds[o.kind]++
			}
		}
		full := vfs.NewSim()
		if acked, _ := run(full, steps); len(acked.pages) == 0 {
			t.Fatalf("seed %d: the workload ends with no page in use", seed)
		}
		calls := full.Calls()
		toAcked, toRunning, header := 0, 0, 0
		for k := 0; k <= calls; k++ {
			fs := vfs.NewSim()
			fs.SetBudget(k)
			acked, running := run(fs, steps)
			// 24 ways: with 12, one workload saw no step in flight
			// survive, only because the lock file shifted every stop.
			for power := int64(0); power < 24; power++ {
				var r *rand.Rand
				if power > 0 {
					r = rand.New(rand.NewSource(seed*1_000_003 + int64(k)*1_009 + power))
				}
				after := fs.Crash(r)
				// Count torn header pages in the file. Open of vfs creates a
				// missing file, so look first.
				if ok, _ := after.Exists("db"); ok {
					db, _ := after.Open("db")
					if _, err := page.ReadHeader(db); errors.Is(err, page.ErrDamaged) {
						header++
					}
				}
				s, err := Open(after, "db", Options{PageSize: ps})
				if err != nil {
					t.Fatalf("seed %d, k %d, power %d: open: %v", seed, k, power, err)
				}
				got := readState(t, s)
				switch {
				case acked.matches(got):
					toAcked++
				case running.matches(got):
					toRunning++
				default:
					t.Fatalf("seed %d, k %d, power %d: %+v\nacked:   %v\nrunning: %v", seed, k, power, got, acked, running)
				}
				// The store takes a commit after recovery: an allocation
				// takes a free page if there is one, else a new one.
				write(t, s, map[uint64]string{}, 1)
				next := readState(t, s)
				if got.freeCount > 0 && (next.freeCount != got.freeCount-1 || next.count != got.count) ||
					got.freeCount == 0 && (next.freeCount != 0 || next.count != got.count+1) {
					t.Fatalf("seed %d, k %d, power %d: allocation after recovery: %d pages, %d free, before %d, %d", seed, k, power, next.count, next.freeCount, got.count, got.freeCount)
				}
				s.Close()
			}
		}
		t.Logf("seed %d: %d stops, %d to acked, %d to the step in flight, %d with a damaged header page in the file", seed, calls+1, toAcked, toRunning, header)
		// Controls: both outcomes occur, and torn header pages occur, so
		// the recovery of the header from the log was tested.
		if toAcked == 0 || toRunning == 0 || header == 0 {
			t.Errorf("seed %d: acked %d, in flight %d, torn headers %d; each must occur", seed, toAcked, toRunning, header)
		}
	}
	// Control: the workloads use every kind of step.
	t.Logf("steps: %v", kinds)
	for _, k := range []string{"alloc", "write", "free", "root", "checkpoint"} {
		if kinds[k] == 0 {
			t.Errorf("no %s in the workloads", k)
		}
	}
}

// TestHeaderRecoveredFromLog: a damaged header page in the file is
// replaced by its newest image in the log.
func TestHeaderRecoveredFromLog(t *testing.T) {
	fs := vfs.NewSim()
	s := mustOpen(t, fs)
	write(t, s, map[uint64]string{1: "one"}, 3)
	s.Close()
	db, _ := fs.Open("db")
	db.WriteAt([]byte("XX"), 20)
	db.Sync()
	s = mustOpen(t, fs.Crash(nil))
	if n, got := readAll(t, s); n != 4 || got[1] != "one" {
		t.Errorf("after a damaged header: %d %v", n, got)
	}
}

// TestDamagedHeaderWithoutLogImage: with no image of the header in the
// log, the damage is reported, not guessed around.
func TestDamagedHeaderWithoutLogImage(t *testing.T) {
	fs := vfs.NewSim()
	s := mustOpen(t, fs)
	write(t, s, map[uint64]string{1: "one"}, 1)
	s.Checkpoint()
	s.Close()
	db, _ := fs.Open("db")
	db.WriteAt([]byte("XX"), 20)
	db.Sync()
	if _, err := Open(fs.Crash(nil), "db", Options{PageSize: ps}); !errors.Is(err, page.ErrDamaged) {
		t.Errorf("open: %v", err)
	}
}

func TestTxRules(t *testing.T) {
	s := mustOpen(t, vfs.NewSim())
	tx, _ := s.Begin()
	no, _ := tx.Allocate()
	if err := tx.Write(0, []byte("x")); err == nil {
		t.Error("write to the header page accepted")
	}
	if err := tx.Write(no+1, []byte("x")); err == nil {
		t.Error("write past the end accepted")
	}
	if err := tx.Write(no, make([]byte, s.Payload()+1)); err == nil {
		t.Error("content longer than a page accepted")
	}
	tx.Write(no, []byte("mine"))
	buf := make([]byte, ps)
	if err := tx.Read(no, buf); err != nil || content(buf) != "mine" {
		t.Errorf("a Tx does not read its own write: %q %v", content(buf), err)
	}
	tx.Rollback()
	if err := tx.Write(no, []byte("x")); !errors.Is(err, ErrDone) {
		t.Errorf("write after rollback: %v", err)
	}
	if n, _ := readAll(t, s); n != 1 {
		t.Errorf("rolled back allocation is visible: %d pages", n)
	}
	// The writer lock is free again.
	tx, _ = s.Begin()
	tx.Rollback()
}

// TestMissingFileWithLogRefused: a log without its database file is not
// something this code writes; Open refuses it.
func TestMissingFileWithLogRefused(t *testing.T) {
	fs := vfs.NewSim()
	l, _ := fs.Open("db-log")
	l.WriteAt([]byte("something"), 0)
	if _, err := Open(fs, "db", Options{PageSize: ps}); err == nil {
		t.Error("a log without its database file accepted")
	}
	// An empty file that exists is not a database; Open does not take
	// it for a new one.
	fs = vfs.NewSim()
	fs.Open("db")
	if _, err := Open(fs, "db", Options{PageSize: ps}); !errors.Is(err, page.ErrNotDatabase) {
		t.Errorf("empty file: %v", err)
	}
}

// TestCreateAfterUnfinishedCreate: a creation that did not finish leaves
// db-new behind. The next Open creates a clean file of one page, not one
// with the rest of the old db-new behind its header.
func TestCreateAfterUnfinishedCreate(t *testing.T) {
	fs := vfs.NewSim()
	left, _ := fs.Open("db-new")
	left.WriteAt(make([]byte, 5*ps), 0)
	left.Sync()
	s := mustOpen(t, fs)
	defer s.Close()
	db, _ := fs.Open("db")
	if n, _ := db.Size(); n != ps {
		t.Errorf("new file has %d bytes, want one page of %d", n, ps)
	}
	if ok, _ := fs.Exists("db-new"); ok {
		t.Error("db-new still exists")
	}
}

// TestSecondOpenIsRefused: while a Store is open, a second Open of the
// same database fails with vfs.ErrLocked. After Close, or after the
// process is gone, it succeeds (S-1).
func TestSecondOpenIsRefused(t *testing.T) {
	fs := vfs.NewSim()
	s := mustOpen(t, fs)
	if _, err := Open(fs, "db", Options{PageSize: ps}); !errors.Is(err, vfs.ErrLocked) {
		t.Errorf("second open: %v, want vfs.ErrLocked", err)
	}
	s.Close()
	s = mustOpen(t, fs)
	s2 := mustOpen(t, fs.Crash(nil))
	s2.Close()
}

// TestFailedOpenReleasesTheLock: an Open that fails does not keep the
// lock.
func TestFailedOpenReleasesTheLock(t *testing.T) {
	fs := vfs.NewSim()
	fs.Open("db") // an empty file is not a database
	if _, err := Open(fs, "db", Options{PageSize: ps}); err == nil {
		t.Fatal("empty file accepted")
	}
	unlock, err := fs.Lock("db-lock")
	if err != nil {
		t.Errorf("lock after a failed open: %v", err)
	} else {
		unlock()
	}
}

func begin(t *testing.T, s *Store) *Tx {
	t.Helper()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func alloc(t *testing.T, tx *Tx) uint64 {
	t.Helper()
	no, err := tx.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	return no
}

// TestFreeThenAllocate: a freed page is handed out again, the one freed
// last first, before the file grows. The free list survives a reopen and
// a checkpoint.
func TestFreeThenAllocate(t *testing.T) {
	fs := vfs.NewSim()
	s := mustOpen(t, fs)
	tx := begin(t, s)
	for i := 0; i < 3; i++ {
		alloc(t, tx) // pages 1, 2, 3
	}
	tx.Commit()
	tx = begin(t, s)
	tx.Free(2)
	tx.Free(3)
	if tx.FreeCount() != 2 {
		t.Errorf("free count %d, want 2", tx.FreeCount())
	}
	tx.Commit()
	s.Close()
	s = mustOpen(t, fs.Crash(nil))
	s.Checkpoint()
	s.Close()
	s = mustOpen(t, fs.Crash(nil))
	tx = begin(t, s)
	if got := []uint64{alloc(t, tx), alloc(t, tx), alloc(t, tx)}; fmt.Sprint(got) != "[3 2 4]" {
		t.Errorf("allocated %v, want [3 2 4]", got)
	}
	if tx.Count() != 5 || tx.FreeCount() != 0 {
		t.Errorf("count %d, free %d", tx.Count(), tx.FreeCount())
	}
	tx.Rollback()
}

// TestSnapshotReadsAFreedPage: a snapshot from before a page was freed and
// reused still reads the page as it was.
func TestSnapshotReadsAFreedPage(t *testing.T) {
	s := mustOpen(t, vfs.NewSim())
	write(t, s, map[uint64]string{1: "kept"}, 1)
	old, _ := s.Snapshot()
	defer old.Close()
	tx := begin(t, s)
	tx.Free(1)
	no := alloc(t, tx)
	tx.Write(no, []byte("reused"))
	tx.Commit()
	if no != 1 {
		t.Fatalf("reused page %d, want 1", no)
	}
	buf := make([]byte, ps)
	old.Read(1, buf)
	if content(buf) != "kept" {
		t.Errorf("old snapshot: %q", content(buf))
	}
}

func TestFreeRefuses(t *testing.T) {
	s := mustOpen(t, vfs.NewSim())
	write(t, s, map[uint64]string{}, 2)
	tx := begin(t, s)
	defer tx.Rollback()
	for _, no := range []uint64{0, 3, 99} {
		if err := tx.Free(no); err == nil {
			t.Errorf("free %d accepted", no)
		}
	}
	tx.Free(1)
	if err := tx.Free(1); err == nil {
		t.Error("double free accepted")
	}
	if err := tx.Write(1, []byte("x")); err == nil {
		t.Error("write to a freed page accepted")
	}
}

// TestDamagedFreeListIsReported: a page on the free list is handed out
// only if it carries the free mark and points to a free page that can be
// one. Handing out a page that is not free would give away data.
func TestDamagedFreeListIsReported(t *testing.T) {
	for name, c := range map[string]struct {
		free []uint64 // freed in this order; the last is the head
		edit func(p []byte)
	}{
		// With one free page, zeros point to page 0, which is right for
		// the last free page; only the mark tells.
		"no mark": {[]uint64{2}, func(p []byte) {}},
		"points past the end": {[]uint64{3, 2}, func(p []byte) {
			copy(p, freeMark[:])
			binary.BigEndian.PutUint64(p[8:], 99)
		}},
		"points to page 0, but more are free": {[]uint64{3, 2}, func(p []byte) {
			copy(p, freeMark[:])
		}},
	} {
		fs := vfs.NewSim()
		s := mustOpen(t, fs)
		write(t, s, map[uint64]string{}, 3)
		tx := begin(t, s)
		for _, no := range c.free {
			tx.Free(no)
		}
		tx.Commit()
		s.Checkpoint()
		s.Close()
		db, _ := fs.Open("db")
		p := make([]byte, ps)
		c.edit(p)
		page.Write(db, 2, p)
		db.Sync()
		s = mustOpen(t, fs.Crash(nil))
		tx = begin(t, s)
		var d *page.DamagedError
		if _, err := tx.Allocate(); !errors.As(err, &d) || d.Page != 2 {
			t.Errorf("%s: allocate: %v", name, err)
		}
		tx.Rollback()
		s.Close()
	}
}

// TestHeaderPageIsNotRead: page 0 is the header. Neither a snapshot nor a
// Tx reads it as a page; its fields are read through Count, FreeCount and
// Root, which in a Tx show its own changes.
func TestHeaderPageIsNotRead(t *testing.T) {
	s := mustOpen(t, vfs.NewSim())
	write(t, s, map[uint64]string{}, 2)
	buf := make([]byte, ps)
	r, _ := s.Snapshot()
	if err := r.Read(0, buf); err == nil {
		t.Error("snapshot read page 0")
	}
	r.Close()
	tx := begin(t, s)
	defer tx.Rollback()
	tx.SetRoot(1, 2)
	if err := tx.Read(0, buf); err == nil {
		t.Error("Tx read page 0")
	}
	if tx.Root(1) != 2 {
		t.Errorf("Tx root %d, want its own change 2", tx.Root(1))
	}
}

// TestLogAndHeaderMustAgree: the page count in the last commit frame and
// in the header image of that commit are written together. If they
// differ, the log is damaged, and Open says so.
func TestLogAndHeaderMustAgree(t *testing.T) {
	fs := vfs.NewSim()
	s := mustOpen(t, fs)
	s.Close()
	lf, _ := fs.Open("db-log")
	l, _, err := wal.Open(lf, ps)
	if err != nil {
		t.Fatal(err)
	}
	hdr := page.EncodeHeader(page.Header{Version: page.Version, PageSize: ps, PageCount: 1})
	if err := l.Commit([]wal.Page{{No: 0, Data: hdr}}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(fs.Crash(nil), "db", Options{PageSize: ps}); !errors.Is(err, page.ErrDamaged) {
		t.Errorf("open with count 7 in the log and 1 in the header: %v", err)
	}
}

// TestRoots: a root set in a commit survives a reopen; a snapshot keeps
// the root of its commit; a rollback keeps the old one.
func TestRoots(t *testing.T) {
	fs := vfs.NewSim()
	s := mustOpen(t, fs)
	write(t, s, map[uint64]string{}, 4)
	tx := begin(t, s)
	if err := tx.SetRoot(0, 3); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	old, _ := s.Snapshot()
	tx = begin(t, s)
	tx.SetRoot(0, 4)
	tx.SetRoot(2, 1)
	tx.Commit()
	if old.Root(0) != 3 {
		t.Errorf("old snapshot root %d, want 3", old.Root(0))
	}
	old.Close()
	tx = begin(t, s)
	tx.SetRoot(0, 2)
	tx.Rollback()
	for _, bad := range []struct {
		slot int
		no   uint64
	}{{-1, 1}, {page.Roots, 1}, {0, 5}} {
		tx = begin(t, s)
		if err := tx.SetRoot(bad.slot, bad.no); err == nil {
			t.Errorf("SetRoot(%d, %d) accepted", bad.slot, bad.no)
		}
		tx.Rollback()
	}
	s.Close()
	s = mustOpen(t, fs.Crash(nil))
	st := readState(t, s)
	if st.roots != [page.Roots]uint64{4, 0, 1, 0} {
		t.Errorf("roots after reopen: %v", st.roots)
	}
}

// TestFreePages checks the walk of the free list. The pages freed last
// come first. A damaged list is an error that names the page.
func TestFreePages(t *testing.T) {
	s, err := Open(vfs.NewSim(), "db", Options{PageSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin()
	var pages []uint64
	for i := 0; i < 4; i++ {
		no, err := tx.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, no)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, _ = s.Begin()
	for _, no := range pages[:3] {
		if err := tx.Free(no); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot()
	got, err := snap.FreePages()
	snap.Close()
	if err != nil || fmt.Sprint(got) != fmt.Sprint([]uint64{pages[2], pages[1], pages[0]}) {
		t.Fatalf("free pages %v %v, want the freed ones, last first", got, err)
	}
	tx, _ = s.Begin()
	if err := tx.Write(pages[1], []byte("not free")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Snapshot()
	var de *page.DamagedError
	if got, err := snap.FreePages(); !errors.As(err, &de) || de.Page != pages[1] || len(got) != 1 {
		t.Fatalf("a free page without its mark: %v %v", got, err)
	}
	snap.Close()

	// A list that does not end after FreeCount pages, one than ends too
	// early, and a loop. The header copy of the snapshot is changed here;
	// on the disk, damage does it.
	tx, _ = s.Begin()
	loop := make([]byte, 16)
	copy(loop, freeMark[:])
	binary.BigEndian.PutUint64(loop[8:], pages[2])
	if err := tx.Write(pages[1], append(append([]byte(nil), freeMark[:]...), binary.BigEndian.AppendUint64(nil, pages[0])...)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(r *Snapshot){
		"one page short": func(r *Snapshot) { r.hdr.FreeCount-- },
		"one page more":  func(r *Snapshot) { r.hdr.FreeCount++ },
	} {
		snap, _ := s.Snapshot()
		change(snap)
		if _, err := snap.FreePages(); !errors.Is(err, page.ErrDamaged) {
			t.Errorf("%s: %v", name, err)
		}
		snap.Close()
	}
	tx, _ = s.Begin()
	if err := tx.Write(pages[0], loop); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Snapshot()
	snap.hdr.FreeCount = 10
	if _, err := snap.FreePages(); !errors.Is(err, page.ErrDamaged) || !strings.Contains(err.Error(), "reaches page") {
		t.Errorf("a loop: %v", err)
	}
	snap.Close()

	// A next page past the end of the file is damage, not a read error.
	tx, _ = s.Begin()
	past := append(append([]byte(nil), freeMark[:]...), binary.BigEndian.AppendUint64(nil, 9999)...)
	if err := tx.Write(pages[0], past); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// With one free page more in the header, the walk goes on to page
	// 9999 instead of ending there.
	snap, _ = s.Snapshot()
	defer snap.Close()
	snap.hdr.FreeCount = 4
	if _, err := snap.FreePages(); !errors.As(err, &de) || de.Page != 9999 || !strings.Contains(err.Error(), "reaches page") {
		t.Errorf("a next page past the end: %v", err)
	}
}

// TestReadOnly opens a database read-only. A missing file is not created.
// Commits in the log are read, and nothing is written: no log is started,
// no commit and no checkpoint runs.
func TestReadOnly(t *testing.T) {
	fs := vfs.NewSim()
	if _, err := Open(fs, "db", Options{ReadOnly: true}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if ok, _ := fs.Exists("db"); ok {
		t.Fatal("read-only open created the file")
	}
	s, err := Open(fs, "db", Options{PageSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := s.Begin()
	no, _ := tx.Allocate()
	if err := tx.Write(no, []byte("in the log")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	sizes := func() string {
		out := ""
		for _, n := range []string{"db", "db-log"} {
			f, _ := fs.Open(n)
			size, _ := f.Size()
			f.Close()
			out += fmt.Sprint(n, "=", size, " ")
		}
		return out
	}
	before := sizes()
	r, err := Open(fs, "db", Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := r.Snapshot()
	buf := make([]byte, 512)
	if err := snap.Read(no, buf); err != nil || !strings.HasPrefix(string(buf), "in the log") {
		t.Fatalf("page from the log: %v %q", err, buf[:10])
	}
	snap.Close()
	if _, err := r.Begin(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("begin: %v", err)
	}
	if _, err := r.Checkpoint(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("checkpoint: %v", err)
	}
	r.Close()
	if after := sizes(); after != before {
		t.Fatalf("read-only open changed the files: %s, then %s", before, after)
	}
	// Without a log file, read-only uses an empty log and creates none.
	s, _ = Open(fs, "db", Options{})
	if ok, err := s.Checkpoint(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	s.Close()
	if err := fs.Remove("db-log"); err != nil {
		t.Fatal(err)
	}
	r, err = Open(fs, "db", Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ = r.Snapshot()
	if err := snap.Read(no, buf); err != nil || !strings.HasPrefix(string(buf), "in the log") {
		t.Fatalf("page from the file: %v", err)
	}
	snap.Close()
	r.Close()
	if ok, _ := fs.Exists("db-log"); ok {
		t.Fatal("read-only open created a log")
	}
}

// TestSnapshotDuringCommit takes snapshots while commits run. Each commit
// adds a page at the end and points page 1 at it. A snapshot must see the
// header of the commit whose pages it reads, so the page that page 1
// points at is inside its page count.
func TestSnapshotDuringCommit(t *testing.T) {
	s, err := Open(vfs.NewSim(), "db", Options{PageSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin()
	first, _ := tx.Allocate()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		for i := 0; i < 2000; i++ {
			tx, err := s.Begin()
			if err != nil {
				done <- err
				return
			}
			no, err := tx.Allocate()
			if err == nil {
				err = tx.Write(no, []byte("new"))
			}
			if err == nil {
				err = tx.Write(first, binary.BigEndian.AppendUint64(nil, no))
			}
			if err == nil {
				err = tx.Commit()
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	buf := make([]byte, 512)
	seen := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if seen == 0 {
				t.Fatal("no snapshot saw a pointer")
			}
			return
		default:
		}
		snap, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := snap.Read(first, buf); err != nil {
			t.Fatal(err)
		}
		if p := binary.BigEndian.Uint64(buf); p != 0 {
			seen++
			if p >= snap.Count() {
				t.Fatalf("page %d points at page %d; the snapshot has %d pages", first, p, snap.Count())
			}
		}
		snap.Close()
	}
}
