package btree

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// randKey makes keys from a small alphabet, so that keys share prefixes
// and the order matters.
func randKey(r *rand.Rand, max int) []byte {
	n := 1 + r.Intn(max)
	if r.Intn(4) > 0 && n > 12 {
		n = 1 + r.Intn(12)
	}
	k := make([]byte, n)
	for i := range k {
		k[i] = "abcd"[r.Intn(4)]
	}
	return k
}

// randValue makes mostly short values, and some that need overflow pages.
func randValue(r *rand.Rand, pageSize int) []byte {
	var n int
	switch r.Intn(10) {
	case 0:
		n = pageSize/2 + r.Intn(3*pageSize)
	case 1:
		n = 0
	default:
		n = r.Intn(40)
	}
	v := make([]byte, n)
	r.Read(v)
	return v
}

// compare checks the tree in snapshot r against the oracle: every key by
// Get, a full scan both ways, seeks, the structure, and that every page is
// either in the tree, free, or the header.
func compare(t *testing.T, tr *Tree, r *store.Snapshot, oracle map[string][]byte, rng *rand.Rand) Stats {
	t.Helper()
	keys := make([]string, 0, len(oracle))
	for k := range oracle {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok, err := tr.Get(r, []byte(k))
		if err != nil || !ok || !bytes.Equal(v, oracle[k]) {
			t.Fatalf("Get(%q): ok %v err %v, %d bytes, want %d", k, ok, err, len(v), len(oracle[k]))
		}
	}
	var fwd, back []string
	c := tr.Cursor(r)
	for c.First(); c.Valid(); c.Next() {
		fwd = append(fwd, string(c.Key()))
		v, err := c.Value()
		if err != nil || !bytes.Equal(v, oracle[string(c.Key())]) {
			t.Fatalf("scan value of %q: %v", c.Key(), err)
		}
	}
	if c.Err() != nil {
		t.Fatal(c.Err())
	}
	for c.Last(); c.Valid(); c.Prev() {
		back = append(back, string(c.Key()))
	}
	if fmt.Sprint(fwd) != fmt.Sprint(keys) {
		t.Fatalf("forward scan: %d keys, want %d", len(fwd), len(keys))
	}
	for i, j := 0, len(back)-1; i < j; i, j = i+1, j-1 {
		back[i], back[j] = back[j], back[i]
	}
	if fmt.Sprint(back) != fmt.Sprint(keys) {
		t.Fatalf("backward scan: %d keys, want %d", len(back), len(keys))
	}
	for i := 0; i < 20; i++ {
		probe := randKey(rng, 8)
		want := sort.SearchStrings(keys, string(probe))
		c.Seek(probe)
		switch {
		case want == len(keys) && c.Valid():
			t.Fatalf("Seek(%q) found %q past the last key", probe, c.Key())
		case want < len(keys) && (!c.Valid() || string(c.Key()) != keys[want]):
			t.Fatalf("Seek(%q): valid %v, want %q", probe, c.Valid(), keys[want])
		}
		// From the seek position, one step back is the key before.
		if want < len(keys) && want > 0 {
			c.Prev()
			if !c.Valid() || string(c.Key()) != keys[want-1] {
				t.Fatalf("Seek(%q) then Prev: want %q", probe, keys[want-1])
			}
		}
	}
	st, err := tr.Check(r)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if st.Entries != len(oracle) {
		t.Fatalf("check counts %d entries, want %d", st.Entries, len(oracle))
	}
	if used := uint64(len(st.Pages)) + r.FreeCount() + 1; used != r.Count() {
		t.Fatalf("pages: %d in the tree, %d free, 1 header; the file has %d", len(st.Pages), r.FreeCount(), r.Count())
	}
	return st
}

func openStore(t *testing.T, fs vfs.FS, pageSize int) *store.Store {
	t.Helper()
	s, err := store.Open(fs, "db", store.Options{PageSize: pageSize})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAgainstAMap runs random puts, overwrites and deletes against a map
// as oracle, commits after each batch and compares everything.
func TestAgainstAMap(t *testing.T) {
	for _, pageSize := range []int{512, 1024} {
		for seed := int64(1); seed <= 3; seed++ {
			rng := rand.New(rand.NewSource(seed))
			s := openStore(t, vfs.NewSim(), pageSize)
			tx, _ := s.Begin()
			tr, err := Create(tx, pageSize)
			if err != nil {
				t.Fatal(err)
			}
			tx.SetRoot(0, tr.Root)
			tx.Commit()
			oracle := map[string][]byte{}
			maxDepth := 0
			for batch := 0; batch < 60; batch++ {
				tx, _ := s.Begin()
				for op := 0; op < 25; op++ {
					k := randKey(rng, tr.lim.MaxKey)
					switch {
					case batch > 35 && rng.Intn(3) > 0, rng.Intn(4) == 0:
						found, err := tr.Delete(tx, k)
						if err != nil {
							t.Fatal(err)
						}
						if _, want := oracle[string(k)]; found != want {
							t.Fatalf("Delete(%q) found %v, want %v", k, found, want)
						}
						delete(oracle, string(k))
					default:
						v := randValue(rng, pageSize)
						if err := tr.Put(tx, k, v); err != nil {
							t.Fatal(err)
						}
						oracle[string(k)] = v
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if batch%7 == 0 {
					s.Checkpoint()
				}
				r, _ := s.Snapshot()
				st := compare(t, tr, r, oracle, rng)
				r.Close()
				if st.Depth > maxDepth {
					maxDepth = st.Depth
				}
			}
			// Control: the trees grew deep enough to split internal nodes,
			// and the deletes of the last batches shrank them again.
			if maxDepth < 3 {
				t.Errorf("page size %d, seed %d: depth reached only %d", pageSize, seed, maxDepth)
			}
			s.Close()
		}
	}
}

// TestEmptyAgainAndDrop: deleting every key leaves one empty leaf, the
// root, and Drop frees every page, so no page leaks.
func TestEmptyAgainAndDrop(t *testing.T) {
	s := openStore(t, vfs.NewSim(), 512)
	tx, _ := s.Begin()
	tr, _ := Create(tx, 512)
	rng := rand.New(rand.NewSource(7))
	var keys [][]byte
	for i := 0; i < 400; i++ {
		k := []byte(fmt.Sprintf("key-%05d", i))
		keys = append(keys, k)
		tr.Put(tx, k, randValue(rng, 512))
	}
	tx.Commit()
	tx, _ = s.Begin()
	for _, k := range keys {
		if found, err := tr.Delete(tx, k); !found || err != nil {
			t.Fatalf("Delete(%q): %v %v", k, found, err)
		}
	}
	st, err := tr.Check(tx)
	if err != nil || st.Entries != 0 || len(st.Pages) != 1 {
		t.Errorf("after deleting all: %+v %v", st, err)
	}
	if err := tr.Drop(tx); err != nil {
		t.Fatal(err)
	}
	if tx.FreeCount()+1 != tx.Count() {
		t.Errorf("after Drop: %d free of %d pages", tx.FreeCount(), tx.Count())
	}
	tx.Rollback()
}

func TestKeyLimits(t *testing.T) {
	s := openStore(t, vfs.NewSim(), 512)
	tx, _ := s.Begin()
	defer tx.Rollback()
	tr, _ := Create(tx, 512)
	if err := tr.Put(tx, nil, []byte("x")); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("empty key: %v", err)
	}
	long := bytes.Repeat([]byte("k"), tr.lim.MaxKey+1)
	if err := tr.Put(tx, long, nil); !errors.Is(err, ErrKeyTooLong) {
		t.Errorf("key of %d bytes: %v", len(long), err)
	}
	if err := tr.Put(tx, long[:tr.lim.MaxKey], bytes.Repeat([]byte("v"), 5000)); err != nil {
		t.Errorf("longest key with an overflow value: %v", err)
	}
	if _, err := tr.Check(tx); err != nil {
		t.Error(err)
	}
}

// TestDamagedNodeIsAnError: a node whose content does not decode gives an
// error, not a panic and not a wrong answer.
func TestDamagedNodeIsAnError(t *testing.T) {
	s := openStore(t, vfs.NewSim(), 512)
	tx, _ := s.Begin()
	tr, _ := Create(tx, 512)
	tr.Put(tx, []byte("a"), []byte("1"))
	// 65535 entries claimed; the page runs out long before.
	tx.Write(tr.Root, []byte{kindLeaf, 0xFF, 0xFF, 1, 'a'})
	if _, _, err := tr.Get(tx, []byte("a")); !errors.Is(err, ErrDamaged) {
		t.Errorf("Get on a damaged node: %v", err)
	}
	if _, err := tr.Check(tx); !errors.Is(err, ErrDamaged) {
		t.Errorf("Check on a damaged node: %v", err)
	}
	tx.Rollback()
}

// FuzzDecode: any bytes decode to a node or an error, never a panic. A
// node that decodes encodes back to the same bytes.
func FuzzDecode(f *testing.F) {
	f.Add((&node{leaf: true, entries: []entry{{key: []byte("a"), value: []byte("b")}}}).encode(508))
	f.Add((&node{first: 3, entries: []entry{{key: []byte("m"), child: 4}}}).encode(508))
	f.Add([]byte{kindLeaf, 0, 1, 1, 'k', 1, 200, 1, 0, 0, 0, 0, 0, 0, 0, 9})
	f.Fuzz(func(t *testing.T, p []byte) {
		n, err := decode(1, p)
		if err != nil {
			return
		}
		out := n.encode(len(p))
		if !bytes.Equal(out, p[:len(out)]) {
			t.Fatal("decoded node encodes to other bytes")
		}
	})
}

// crashStep is one transaction of the crash workload.
type crashStep struct {
	puts map[string]string
	dels []string
}

// TestCrashWithTree is the crash test of P-1 with the tree: batches of
// puts and deletes, stopped after every call, then a power loss in 16
// ways. The tree that opens holds the acked batches or those plus the one
// in flight, and Check finds it sound.
func TestCrashWithTree(t *testing.T) {
	const pageSize = 1024
	rng := rand.New(rand.NewSource(3))
	var steps []crashStep
	for i := 0; i < 12; i++ {
		st := crashStep{puts: map[string]string{}}
		// Every second batch is one short put: a commit of few pages, so
		// that a commit in flight can survive a power loss whole. With
		// only large batches, none ever did.
		if i%2 == 1 {
			st.puts[string(randKey(rng, 8))] = fmt.Sprintf("s%d", i)
			steps = append(steps, st)
			continue
		}
		for k := 0; k < 12; k++ {
			key := string(randKey(rng, 30))
			if rng.Intn(4) == 0 {
				st.dels = append(st.dels, key)
			} else {
				st.puts[key] = fmt.Sprintf("s%d-%s-%s", i, key, bytes.Repeat([]byte("v"), rng.Intn(3)*pageSize))
			}
		}
		steps = append(steps, st)
	}
	apply := func(m map[string]string, st crashStep) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = v
		}
		for _, k := range st.dels {
			delete(out, k)
		}
		for k, v := range st.puts {
			out[k] = v
		}
		return out
	}
	run := func(fs vfs.FS) (acked, running map[string]string) {
		acked, running = map[string]string{}, map[string]string{}
		s, err := store.Open(fs, "db", store.Options{PageSize: pageSize})
		if err != nil {
			return
		}
		tx, err := s.Begin()
		if err != nil {
			return
		}
		tr, err := Create(tx, pageSize)
		if err != nil {
			tx.Rollback()
			return
		}
		tx.SetRoot(0, tr.Root)
		if tx.Commit() != nil {
			return
		}
		for i, st := range steps {
			running = apply(acked, st)
			tx, err := s.Begin()
			if err != nil {
				return
			}
			for _, k := range st.dels {
				if _, err := tr.Delete(tx, []byte(k)); err != nil {
					tx.Rollback()
					return
				}
			}
			for k, v := range st.puts {
				if err := tr.Put(tx, []byte(k), []byte(v)); err != nil {
					tx.Rollback()
					return
				}
			}
			if tx.Commit() != nil {
				return
			}
			acked = running
			if i%4 == 3 {
				if _, err := s.Checkpoint(); err != nil {
					return
				}
				running = acked
			}
		}
		return
	}
	full := vfs.NewSim()
	run(full)
	calls := full.Calls()
	toAcked, toRunning := 0, 0
	for k := 0; k <= calls; k++ {
		fs := vfs.NewSim()
		fs.SetBudget(k)
		acked, running := run(fs)
		for power := int64(0); power < 16; power++ {
			var r *rand.Rand
			if power > 0 {
				r = rand.New(rand.NewSource(int64(k)*1_009 + power))
			}
			after := fs.Crash(r)
			s, err := store.Open(after, "db", store.Options{PageSize: pageSize})
			if err != nil {
				t.Fatalf("k %d, power %d: open: %v", k, power, err)
			}
			snap, _ := s.Snapshot()
			got := map[string]string{}
			if root := snap.Root(0); root != 0 {
				tr := Open(root, pageSize)
				if _, err := tr.Check(snap); err != nil {
					t.Fatalf("k %d, power %d: check: %v", k, power, err)
				}
				c := tr.Cursor(snap)
				for c.First(); c.Valid(); c.Next() {
					v, err := c.Value()
					if err != nil {
						t.Fatal(err)
					}
					got[string(c.Key())] = string(v)
				}
				if c.Err() != nil {
					t.Fatal(c.Err())
				}
			}
			snap.Close()
			s.Close()
			switch {
			case fmt.Sprint(got) == fmt.Sprint(acked):
				toAcked++
			case fmt.Sprint(got) == fmt.Sprint(running):
				toRunning++
			default:
				t.Fatalf("k %d, power %d: tree holds %d keys; acked %d, running %d", k, power, len(got), len(acked), len(running))
			}
		}
	}
	t.Logf("%d stops, %d to acked, %d to the batch in flight", calls+1, toAcked, toRunning)
	if toAcked == 0 || toRunning == 0 {
		t.Errorf("both outcomes must occur: acked %d, in flight %d", toAcked, toRunning)
	}
}
