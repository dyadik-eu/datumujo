package store

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/vfs"
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

// step is one step of a workload: a transaction or a checkpoint.
type step struct {
	checkpoint bool
	allocate   int
	pages      map[uint64]string
}

type model struct {
	count uint64
	pages map[uint64]string
}

func (m model) apply(st step) model {
	out := model{count: m.count + uint64(st.allocate), pages: map[uint64]string{}}
	for no := uint64(1); no < out.count; no++ {
		out.pages[no] = m.pages[no]
	}
	for no, c := range st.pages {
		out.pages[no] = c
	}
	return out
}

func (m model) equal(n uint64, got map[uint64]string) bool {
	return m.count == n && fmt.Sprint(m.pages) == fmt.Sprint(got)
}

func workload(seed int64) []step {
	r := rand.New(rand.NewSource(seed))
	var out []step
	count := uint64(1)
	for i := 0; i < 14; i++ {
		if i > 0 && r.Intn(4) == 0 {
			out = append(out, step{checkpoint: true})
			continue
		}
		st := step{allocate: r.Intn(3), pages: map[uint64]string{}}
		if count == 1 && st.allocate == 0 {
			st.allocate = 1
		}
		count += uint64(st.allocate)
		for k := 0; k < 1+r.Intn(3); k++ {
			no := uint64(1 + r.Intn(int(count-1)))
			st.pages[no] = fmt.Sprintf("s%d-p%d", i, no)
		}
		out = append(out, st)
	}
	return out
}

// run applies the workload to a store on fs until a call fails. It
// returns the model of what was acked and of the step in flight.
func run(fs vfs.FS, steps []step) (acked, running model) {
	acked = model{count: 1, pages: map[uint64]string{}}
	running = acked
	s, err := Open(fs, "db", Options{PageSize: ps})
	if err != nil {
		return
	}
	for _, st := range steps {
		running = acked.apply(st)
		if st.checkpoint {
			running = acked
			if _, err := s.Checkpoint(); err != nil {
				return
			}
			continue
		}
		tx, err := s.Begin()
		if err != nil {
			return
		}
		for i := 0; i < st.allocate; i++ {
			tx.Allocate()
		}
		for no, c := range st.pages {
			tx.Write(no, []byte(c))
		}
		if err := tx.Commit(); err != nil {
			return
		}
		acked = running
	}
	return
}

// TestCrashAtEveryCall is the crash test of P-1 for the store: commits
// and checkpoints, stopped after every call, then a power loss in 24
// ways. The database opens with the acked state, or with the step in
// flight applied, and takes a commit after it.
func TestCrashAtEveryCall(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		steps := workload(seed)
		full := vfs.NewSim()
		run(full, steps)
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
				n, got := readAll(t, s)
				switch {
				case acked.equal(n, got):
					toAcked++
				case running.equal(n, got):
					toRunning++
				default:
					t.Fatalf("seed %d, k %d, power %d: %d pages %v; acked %d %v, running %d %v", seed, k, power, n, got, acked.count, acked.pages, running.count, running.pages)
				}
				write(t, s, map[uint64]string{}, 1)
				if n2, _ := readAll(t, s); n2 != n+1 {
					t.Fatalf("seed %d, k %d, power %d: commit after recovery: %d pages, want %d", seed, k, power, n2, n+1)
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
