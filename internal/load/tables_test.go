package load

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo"
	"github.com/dyadik-eu/datumujo/internal/store"
)

// The tables of the load test, as a forge keeps them.
var loadDefs = []datumujo.Def{
	{Name: "issue", Columns: []datumujo.Column{
		{Name: "repo", Type: datumujo.Int64},
		{Name: "number", Type: datumujo.Int64},
		{Name: "pr", Type: datumujo.Bool},
		{Name: "title", Type: datumujo.String},
		{Name: "body", Type: datumujo.String, Null: true},
		{Name: "state", Type: datumujo.String},
		{Name: "author", Type: datumujo.String},
		{Name: "created", Type: datumujo.Time},
		{Name: "updated", Type: datumujo.Time},
		{Name: "closed", Type: datumujo.Time, Null: true},
		{Name: "labels", Type: datumujo.String, Null: true},
	}, Key: []string{"repo", "number"}},
	{Name: "comment", Columns: []datumujo.Column{
		{Name: "repo", Type: datumujo.Int64},
		{Name: "number", Type: datumujo.Int64},
		{Name: "id", Type: datumujo.Int64},
		{Name: "author", Type: datumujo.String},
		{Name: "body", Type: datumujo.String},
		{Name: "created", Type: datumujo.Time},
		{Name: "updated", Type: datumujo.Time},
	}, Key: []string{"repo", "number", "id"}},
	{Name: "review", Columns: []datumujo.Column{
		{Name: "repo", Type: datumujo.Int64},
		{Name: "number", Type: datumujo.Int64},
		{Name: "id", Type: datumujo.Int64},
		{Name: "author", Type: datumujo.String},
		{Name: "body", Type: datumujo.String},
		{Name: "path", Type: datumujo.String},
		{Name: "hunk", Type: datumujo.String},
		{Name: "created", Type: datumujo.Time},
		{Name: "updated", Type: datumujo.Time},
	}, Key: []string{"repo", "number", "id"}},
}

var loadIndexes = map[string][]datumujo.IndexDef{
	"issue": {
		{Name: "by_state", Columns: []string{"repo", "state"}},
		{Name: "by_author", Columns: []string{"author"}},
		{Name: "by_updated", Columns: []string{"repo", "updated"}},
	},
}

func parseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// repoData is the data of one repository, grouped by issue.
type repoData struct {
	issues   []issue
	comments map[int64][]comment
	reviews  map[int64][]comment
	open     int
}

func readRepo(t *testing.T, dir, name string) repoData {
	d := repoData{comments: map[int64][]comment{}, reviews: map[int64][]comment{}}
	d.issues = readAll[issue](t, filepath.Join(dir, name+"-issues.json"))
	for _, is := range d.issues {
		if is.State == "open" {
			d.open++
		}
	}
	for _, c := range readAll[comment](t, filepath.Join(dir, name+"-comments.json")) {
		n := int64(number(c.IssueURL))
		d.comments[n] = append(d.comments[n], c)
	}
	for _, c := range readAll[comment](t, filepath.Join(dir, name+"-reviews.json")) {
		n := int64(number(c.PullURL))
		d.reviews[n] = append(d.reviews[n], c)
	}
	return d
}

// timed runs fn three times and returns the median time.
func timed(fn func()) time.Duration {
	var ds []time.Duration
	for i := 0; i < 3; i++ {
		start := time.Now()
		fn()
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[1]
}

// TestLoad is the load test of P-5 with typed tables. It loads the issues,
// pull requests, comments and review comments of the two in-toto
// repositories. Each issue is one commit, as a forge writes them. It uses
// the public API, with the automatic checkpoint. Then it measures the reads a forge makes,
// the check and a backup. It runs only with DATUMUJO_LOAD set, see the
// package comment. DATUMUJO_LOAD_PAGE_SIZE sets the page size.
func TestLoad(t *testing.T) {
	dir := os.Getenv("DATUMUJO_LOAD")
	if dir == "" {
		t.Skip("DATUMUJO_LOAD is not set")
	}
	var data []repoData
	var issues, comments, reviews, orphans int
	for _, r := range repos {
		d := readRepo(t, dir, r)
		data = append(data, d)
		issues += len(d.issues)
		have := map[int64]bool{}
		for _, is := range d.issues {
			have[is.Number] = true
		}
		for n, cs := range d.comments {
			comments += len(cs)
			if !have[n] {
				orphans += len(cs)
			}
		}
		for n, cs := range d.reviews {
			reviews += len(cs)
			if !have[n] {
				orphans += len(cs)
			}
		}
	}

	work := t.TempDir()
	name := filepath.Join(work, "db")
	// DATUMUJO_LOAD_PAGE_SIZE repeats the measurement of the page size
	// (design.md, "Page size") with tables. Without it, new files get the
	// default.
	var pageSize int
	if ps := os.Getenv("DATUMUJO_LOAD_PAGE_SIZE"); ps != "" {
		if _, err := fmt.Sscan(ps, &pageSize); err != nil {
			t.Fatal(err)
		}
	}
	// Through the public API, as a program uses it. Checkpoints run by
	// themselves, at the default log size.
	db, err := datumujo.Open(name, datumujo.Options{PageSize: pageSize})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	begin := func() *datumujo.Tx {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tx := begin()
	for _, d := range loadDefs {
		must(tx.CreateTable(d))
		for _, ix := range loadIndexes[d.Name] {
			must(tx.CreateIndex(d.Name, ix))
		}
	}
	must(tx.Commit())

	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	commits, maxChanged, largest := 0, 0, ""
	var maxLog int64
	commit := func(tx *datumujo.Tx, what string) {
		if c := tx.Changed(); c > maxChanged {
			maxChanged, largest = c, what
		}
		must(tx.Commit())
		commits++
		if l := db.LogBytes(); l > maxLog {
			maxLog = l
		}
	}
	addComments := func(tx *datumujo.Tx, repo int64, cs, rs []comment) {
		for _, c := range cs {
			must(tx.Insert("comment", datumujo.Row{repo, int64(number(c.IssueURL)), c.ID, c.User.Login, c.Body, parseTime(t, c.CreatedAt), parseTime(t, c.UpdatedAt)}))
		}
		for _, c := range rs {
			must(tx.Insert("review", datumujo.Row{repo, int64(number(c.PullURL)), c.ID, c.User.Login, c.Body, c.Path, c.DiffHunk, parseTime(t, c.CreatedAt), parseTime(t, c.UpdatedAt)}))
		}
	}
	start := time.Now()
	for ri, d := range data {
		repo := int64(ri + 1)
		have := map[int64]bool{}
		for _, is := range d.issues {
			have[is.Number] = true
			var labels []string
			for _, l := range is.Labels {
				labels = append(labels, l.Name)
			}
			var closed any
			if is.ClosedAt != "" {
				closed = parseTime(t, is.ClosedAt)
			}
			tx := begin()
			must(tx.Insert("issue", datumujo.Row{repo, is.Number, is.PullRequest != nil, is.Title, nullString(is.Body), is.State, is.User.Login,
				parseTime(t, is.CreatedAt), parseTime(t, is.UpdatedAt), closed, nullString(strings.Join(labels, ","))}))
			addComments(tx, repo, d.comments[is.Number], d.reviews[is.Number])
			commit(tx, fmt.Sprintf("%s #%d", repos[ri], is.Number))
		}
		// Comments whose issue the API no longer returns: one commit.
		tx := begin()
		for n := range d.comments {
			if !have[n] {
				addComments(tx, repo, d.comments[n], nil)
			}
		}
		for n := range d.reviews {
			if !have[n] {
				addComments(tx, repo, nil, d.reviews[n])
			}
		}
		commit(tx, "orphans of "+repos[ri])
	}
	loadTime := time.Since(start)
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	if _, err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	v, err := db.View()
	must(err)
	count := func(tb string, o datumujo.ScanOptions) int {
		rows, err := v.Scan(tb, o)
		must(err)
		n := 0
		for rows.Next() {
			n++
		}
		must(rows.Err())
		return n
	}

	getTime := timed(func() {
		for ri, d := range data {
			for _, is := range d.issues {
				if _, ok, err := v.Get("issue", int64(ri+1), is.Number); !ok || err != nil {
					t.Fatalf("issue %d/%d: %v %v", ri+1, is.Number, ok, err)
				}
			}
		}
	})

	// Open issues, 30 per page, as the API lists them, with the cursor.
	pages, openSeen := 0, 0
	pageTime := timed(func() {
		pages, openSeen = 0, 0
		for ri := range data {
			var after []byte
			for {
				rows, err := v.Scan("issue", datumujo.ScanOptions{Index: "by_state", Prefix: []any{int64(ri + 1), "open"}, After: after})
				must(err)
				n := 0
				for n < 30 && rows.Next() {
					after = rows.Cursor()
					n++
				}
				must(rows.Err())
				if n == 0 {
					break
				}
				pages++
				openSeen += n
			}
		}
	})

	// The comments and review comments of each issue, by prefix.
	threadRows := 0
	threadTime := timed(func() {
		threadRows = 0
		for ri, d := range data {
			for _, is := range d.issues {
				threadRows += count("comment", datumujo.ScanOptions{Prefix: []any{int64(ri + 1), is.Number}})
				threadRows += count("review", datumujo.ScanOptions{Prefix: []any{int64(ri + 1), is.Number}})
			}
		}
	})

	// The 30 issues changed last, per repository.
	recentTime := timed(func() {
		for ri := range data {
			rows, err := v.Scan("issue", datumujo.ScanOptions{Index: "by_updated", Prefix: []any{int64(ri + 1)}, Reverse: true})
			must(err)
			var last time.Time
			for n := 0; n < 30 && rows.Next(); n++ {
				u := rows.Row()[8].(time.Time)
				if n > 0 && u.After(last) {
					t.Fatalf("issues not in order of change: %v after %v", u, last)
				}
				last = u
			}
			must(rows.Err())
		}
	})
	counts := map[string]int{}
	for _, d := range loadDefs {
		counts[d.Name] = count(d.Name, datumujo.ScanOptions{})
	}
	v.Close()

	copyName := filepath.Join(work, "copy")
	start = time.Now()
	must(db.Backup(copyName))
	backupTime := time.Since(start)
	must(db.Close())

	start = time.Now()
	r, err := datumujo.Check(name)
	must(err)
	checkTime := time.Since(start)
	if len(r.Findings) != 0 {
		t.Fatalf("findings: %v", r.Findings)
	}

	// The counts, each against the JSON.
	wantOpen := 0
	for _, d := range data {
		wantOpen += d.open
	}
	for what, got := range map[string][2]int{
		"issues":             {counts["issue"], issues},
		"comments":           {counts["comment"], comments},
		"review comments":    {counts["review"], reviews},
		"open issues paged":  {openSeen, wantOpen},
		"comments by prefix": {threadRows, comments + reviews - orphans},
	} {
		if got[0] != got[1] {
			t.Fatalf("%s: %d, want %d", what, got[0], got[1])
		}
	}
	for _, ts := range r.Stats.Tables {
		if ts.Rows != counts[ts.Name] {
			t.Fatalf("check counts %d rows in %s, the scan %d", ts.Rows, ts.Name, counts[ts.Name])
		}
	}

	maxBytes := int64(maxChanged) * int64(r.Stats.PageSize)
	fmt.Printf("| measure | value |\n|---|---|\n")
	fmt.Printf("| rows | %d issues and pull requests, %d comments, %d review comments, %d of them without their issue |\n", issues, comments, reviews, orphans)
	fmt.Printf("| load | %d commits in %v, %v per commit |\n", commits, loadTime.Round(time.Millisecond), (loadTime / time.Duration(commits)).Round(time.Microsecond))
	fmt.Printf("| largest transaction | %d pages, %d bytes, of %d allowed (%s) |\n", maxChanged, maxBytes, store.DefaultMaxTxBytes, largest)
	fmt.Printf("| log | at most %d bytes after a commit, checkpoints at %d |\n", maxLog, datumujo.DefaultCheckpointBytes)
	// m0 was taken with the JSON data already read, so the difference is
	// what the engine holds after the load and a checkpoint.
	fmt.Printf("| memory | %d bytes allocated per commit; %d bytes more on the heap after the load than before |\n", (m1.TotalAlloc-m0.TotalAlloc)/uint64(commits), int64(m2.HeapAlloc)-int64(m0.HeapAlloc))
	fmt.Printf("| file | %d bytes, %d pages of %d bytes, %d free |\n", r.Stats.FileBytes, r.Stats.Pages, r.Stats.PageSize, r.Stats.FreePages)
	fmt.Printf("| get every issue by key | %v for %d |\n", getTime.Round(time.Microsecond), issues)
	fmt.Printf("| open issues, 30 per page with cursor | %v for %d pages, %v per page |\n", pageTime.Round(time.Microsecond), pages, (pageTime / time.Duration(pages)).Round(time.Microsecond))
	fmt.Printf("| comments of each issue by prefix | %v for %d issues |\n", threadTime.Round(time.Microsecond), issues)
	fmt.Printf("| 30 issues changed last, per repository | %v for %d repositories |\n", recentTime.Round(time.Microsecond), len(data))
	fmt.Printf("| check | %v, intact |\n", checkTime.Round(time.Millisecond))
	fmt.Printf("| backup while open | %v, accepted by the check |\n", backupTime.Round(time.Millisecond))
	if maxBytes > store.DefaultMaxTxBytes {
		t.Fatalf("the largest transaction passed the bound")
	}
}
