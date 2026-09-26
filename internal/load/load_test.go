// Package load measures the engine with the load of requirement P-5: the
// issues, pull requests and comments of two real repositories. It runs
// only with DATUMUJO_LOAD set to a directory that holds the data as the
// GitHub REST API returns it:
//
//	for r in in-toto-golang in-toto; do
//	  gh api --paginate "repos/in-toto/$r/issues?state=all&per_page=100" --jq '.[]' > $r-issues.json
//	  gh api --paginate "repos/in-toto/$r/issues/comments?per_page=100" --jq '.[]' > $r-comments.json
//	  gh api --paginate "repos/in-toto/$r/pulls/comments?per_page=100" --jq '.[]' > $r-reviews.json
//	done
//
// There are no typed tables yet (step 8). The rows are keys and values in
// trees: fields with a length before each, as a row encoding will write
// them.
package load

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo/internal/btree"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

var repos = []string{"in-toto-golang", "in-toto"}

type user struct {
	Login string `json:"login"`
}

type issue struct {
	Number      int64     `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	User        user      `json:"user"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
	ClosedAt    string    `json:"closed_at"`
	PullRequest *struct{} `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type comment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	User      user   `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	IssueURL  string `json:"issue_url"`
	PullURL   string `json:"pull_request_url"`
	Path      string `json:"path"`
	DiffHunk  string `json:"diff_hunk"`
}

// readAll decodes a file of JSON objects, one after the other.
func readAll[T any](t *testing.T, path string) []T {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []T
	dec := json.NewDecoder(f)
	for {
		var v T
		if err := dec.Decode(&v); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, v)
	}
}

// fields encodes strings with a length before each.
func fields(fs ...string) []byte {
	var b []byte
	for _, f := range fs {
		b = binary.AppendUvarint(b, uint64(len(f)))
		b = append(b, f...)
	}
	return b
}

func key(parts ...uint64) []byte {
	var b []byte
	for _, p := range parts {
		b = binary.BigEndian.AppendUint64(b, p)
	}
	return b
}

// number reads the number at the end of an issue or pull request URL.
func number(url string) uint64 {
	n, _ := strconv.ParseUint(url[strings.LastIndexByte(url, '/')+1:], 10, 64)
	return n
}

type row struct {
	tree       int // 0 issues, 1 comments, 2 review comments, 3 state index
	key, value []byte
}

type dataset struct {
	rows   []row
	issues [][]byte // keys of all issues and pull requests
	counts map[string]int
	// orphans are comments on an issue or pull request that the data
	// does not hold, such as a deleted pull request. A prefix scan over
	// the issues cannot find them.
	orphans int
	bytes   int // keys and values
}

func load(t *testing.T, dir string) dataset {
	ds := dataset{counts: map[string]int{}}
	add := func(tree int, k, v []byte) {
		ds.rows = append(ds.rows, row{tree, k, v})
		ds.bytes += len(k) + len(v)
	}
	for ri, r := range repos {
		repo := uint64(ri + 1)
		have := map[uint64]bool{}
		for _, is := range readAll[issue](t, filepath.Join(dir, r+"-issues.json")) {
			kind := "issue"
			if is.PullRequest != nil {
				kind = "pr"
			}
			ds.counts[kind]++
			var labels []string
			for _, l := range is.Labels {
				labels = append(labels, l.Name)
			}
			k := key(repo, uint64(is.Number))
			ds.issues = append(ds.issues, k)
			have[uint64(is.Number)] = true
			add(0, k, fields(kind, is.Title, is.Body, is.State, is.User.Login, is.CreatedAt, is.UpdatedAt, is.ClosedAt, strings.Join(labels, ",")))
			add(3, append(append(key(repo), is.State...), key(uint64(is.Number))...), nil)
		}
		for _, c := range readAll[comment](t, filepath.Join(dir, r+"-comments.json")) {
			ds.counts["comment"]++
			if !have[number(c.IssueURL)] {
				ds.orphans++
			}
			add(1, key(repo, number(c.IssueURL), uint64(c.ID)), fields(c.Body, c.User.Login, c.CreatedAt, c.UpdatedAt))
		}
		for _, c := range readAll[comment](t, filepath.Join(dir, r+"-reviews.json")) {
			ds.counts["review"]++
			if !have[number(c.PullURL)] {
				ds.orphans++
			}
			add(2, key(repo, number(c.PullURL), uint64(c.ID)), fields(c.Body, c.User.Login, c.CreatedAt, c.UpdatedAt, c.Path, c.DiffHunk))
		}
	}
	return ds
}

type result struct {
	pageSize                     int
	fileBytes                    int64
	pages, overflow, maxDepth    int
	loadT, getT, prefixT, scanT  time.Duration
	commentsFound, entriesInTree int
}

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func measure(t *testing.T, ds dataset, pageSize int) result {
	res := result{pageSize: pageSize}
	var loads, gets, prefixes, scans []time.Duration
	for run := 0; run < 3; run++ {
		dir := t.TempDir()
		name := filepath.Join(dir, "db")
		s, err := store.Open(vfs.OS{}, name, store.Options{PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		tx, _ := s.Begin()
		var trees [4]*btree.Tree
		for i := range trees {
			if trees[i], err = btree.Create(tx, pageSize); err != nil {
				t.Fatal(err)
			}
			tx.SetRoot(i, trees[i].Root)
		}
		for i, r := range ds.rows {
			if err := trees[r.tree].Put(tx, r.key, r.value); err != nil {
				t.Fatalf("page size %d: row %d: %v", pageSize, i, err)
			}
			if (i+1)%100 == 0 {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				tx, _ = s.Begin()
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Checkpoint(); !ok || err != nil {
			t.Fatalf("checkpoint: %v %v", ok, err)
		}
		loads = append(loads, time.Since(start))

		snap, _ := s.Snapshot()
		start = time.Now()
		for _, k := range ds.issues {
			if _, ok, err := trees[0].Get(snap, k); !ok || err != nil {
				t.Fatalf("issue %x: %v %v", k, ok, err)
			}
		}
		gets = append(gets, time.Since(start))

		// All comments and review comments of each issue: a prefix scan.
		start = time.Now()
		found := 0
		for _, k := range ds.issues {
			for _, tr := range []*btree.Tree{trees[1], trees[2]} {
				c := tr.Cursor(snap)
				for c.Seek(k); c.Valid() && strings.HasPrefix(string(c.Key()), string(k)); c.Next() {
					if _, err := c.Value(); err != nil {
						t.Fatal(err)
					}
					found++
				}
				if c.Err() != nil {
					t.Fatal(c.Err())
				}
			}
		}
		prefixes = append(prefixes, time.Since(start))
		res.commentsFound = found

		start = time.Now()
		entries := 0
		for _, tr := range trees {
			c := tr.Cursor(snap)
			for c.First(); c.Valid(); c.Next() {
				if _, err := c.Value(); err != nil {
					t.Fatal(err)
				}
				entries++
			}
		}
		scans = append(scans, time.Since(start))
		res.entriesInTree = entries

		res.pages, res.overflow, res.maxDepth = 0, 0, 0
		for _, tr := range trees {
			st, err := tr.Check(snap)
			if err != nil {
				t.Fatal(err)
			}
			res.pages += len(st.Pages)
			res.overflow += st.Overflow
			if st.Depth > res.maxDepth {
				res.maxDepth = st.Depth
			}
		}
		snap.Close()
		s.Close()
		fi, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		res.fileBytes = fi.Size()
	}
	res.loadT, res.getT, res.prefixT, res.scanT = median(loads), median(gets), median(prefixes), median(scans)
	return res
}

// TestPageSize loads the data with each candidate page size. It reports
// the file size, the pages, the overflow pages and the depth. It also
// reports four times: the load, a lookup of every issue, a prefix scan
// over the comments of each issue, and a full scan. Each time is
// the median of three runs on the file system of the operating system.
func TestPageSize(t *testing.T) {
	dir := os.Getenv("DATUMUJO_LOAD")
	if dir == "" {
		t.Skip("DATUMUJO_LOAD is not set")
	}
	ds := load(t, dir)
	t.Logf("rows: %v, %d rows in 4 trees, %d bytes of keys and values, %d comments without their issue", ds.counts, len(ds.rows), ds.bytes, ds.orphans)
	want := ds.counts["comment"] + ds.counts["review"] - ds.orphans
	fmt.Printf("| page size | file | pages | overflow | depth | load | get all issues | comments by prefix | full scan |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, ps := range []int{1024, 2048, 4096, 8192, 16384} {
		r := measure(t, ds, ps)
		if r.commentsFound != want || r.entriesInTree != len(ds.rows) {
			t.Fatalf("page size %d: %d comments found, want %d; %d entries, want %d", ps, r.commentsFound, want, r.entriesInTree, len(ds.rows))
		}
		fmt.Printf("| %d | %.1f MiB | %d | %d | %d | %v | %v | %v | %v |\n", ps, float64(r.fileBytes)/(1<<20), r.pages, r.overflow, r.maxDepth,
			r.loadT.Round(time.Millisecond), r.getT.Round(time.Microsecond), r.prefixT.Round(time.Microsecond), r.scanT.Round(time.Microsecond))
	}
}
