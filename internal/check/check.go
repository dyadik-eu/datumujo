// Package check reads a whole database and reports each damaged page and
// each fault in its structure (requirement I-2), with statistics about the
// file (O-3).
package check

import (
	"errors"
	"fmt"

	"github.com/dyadik-eu/datumujo/internal/btree"
	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/table"
	"github.com/dyadik-eu/datumujo/internal/vfs"
	"github.com/dyadik-eu/datumujo/internal/wal"
)

// Finding is one fault. Page is set when the fault is on one page.
type Finding struct {
	Page    uint64
	HasPage bool
	What    string
}

func (f Finding) String() string {
	if f.HasPage {
		return fmt.Sprintf("page %d: %s", f.Page, f.What)
	}
	return f.What
}

// IndexStats counts one index.
type IndexStats struct {
	Name    string
	Entries int
	Pages   int
}

// TableStats counts one table.
type TableStats struct {
	Name    string
	Rows    int
	Pages   int // pages of the table's tree, overflow pages included
	Indexes []IndexStats
}

// Stats describes the file. Only the sizes are measured when the database
// could not be opened; Opened tells.
type Stats struct {
	FileBytes, LogBytes int64
	Opened              bool
	PageSize            int
	Pages               uint64 // pages of the database, the header included
	FreePages           uint64
	CatalogPages        int
	SchemaVersion       uint64
	Tables              []TableStats
}

// Report is the result of a check. It is intact when it has no findings.
type Report struct {
	Findings []Finding
	Stats    Stats
}

// ErrCannotCheck matches an error that stopped the check before it could
// say anything about the file.
var ErrCannotCheck = errors.New("check: could not check")

// Run checks the database in the file name of fs. It opens it read-only
// and writes nothing but the lock file. An error from Run matches ErrCannotCheck; a damaged file
// gives a Report with findings instead.
func Run(fs vfs.FS, name string) (*Report, error) {
	// Both errors stay reachable with errors.Is: that the check could not
	// check, and why, for example vfs.ErrLocked.
	cannot := func(err error) error { return fmt.Errorf("%w: %s: %w", ErrCannotCheck, name, err) }
	exists, err := fs.Exists(name)
	if err != nil {
		return nil, cannot(err)
	}
	if !exists {
		return nil, cannot(errors.New("no such file"))
	}
	r := &Report{}
	if r.Stats.FileBytes, err = size(fs, name); err != nil {
		return nil, cannot(err)
	}
	if ok, err := fs.Exists(name + "-log"); err != nil {
		return nil, cannot(err)
	} else if ok {
		if r.Stats.LogBytes, err = size(fs, name+"-log"); err != nil {
			return nil, cannot(err)
		}
	}
	s, err := store.Open(fs, name, store.Options{ReadOnly: true})
	if err != nil {
		var pd *page.DamagedError
		var wd *wal.DamagedError
		switch {
		case errors.As(err, &pd):
			r.Findings = append(r.Findings, Finding{Page: pd.Page, HasPage: true, What: err.Error()})
			return r, nil
		case errors.As(err, &wd):
			r.Findings = append(r.Findings, Finding{What: err.Error()})
			return r, nil
		}
		return nil, cannot(err)
	}
	defer s.Close()
	r.Stats.Opened = true
	// Bytes after the last complete commit are a commit that did not
	// finish, or damage in the last commit. Opening refuses damage with a
	// later commit behind it; in the last commit, the log cannot tell the
	// two apart. The next commit cuts the bytes. So this is a finding:
	// after a crash a false alarm, after damage the one warning there is.
	if n := s.Recovered().Ignored; n > 0 {
		r.Findings = append(r.Findings, Finding{What: fmt.Sprintf("the log has %d bytes after its last complete commit: a commit that did not finish, or damage in the last commit; the next commit cuts them", n)})
	}
	snap, err := s.Snapshot()
	if err != nil {
		return nil, cannot(err)
	}
	defer snap.Close()
	c := &checker{r: r, snap: snap, pageSize: s.PageSize(), owner: map[uint64]string{}, damaged: map[uint64]bool{}, complete: true}
	r.Stats.PageSize, r.Stats.Pages, r.Stats.FreePages = s.PageSize(), snap.Count(), snap.FreeCount()
	if err := c.pages(); err != nil {
		return nil, cannot(err)
	}
	if err := c.structure(); err != nil {
		return nil, cannot(err)
	}
	return r, nil
}

func size(fs vfs.FS, name string) (int64, error) {
	f, err := fs.Open(name)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Size()
}

type checker struct {
	r        *Report
	snap     *store.Snapshot
	pageSize int
	owner    map[uint64]string // who uses a page
	damaged  map[uint64]bool   // pages whose checksum failed
	complete bool              // every structure was walked to its end
}

// finding adds a fault from an error. A damaged error ends up here; any
// other error is returned, because it stops the check. A page that the
// first pass reported is not reported again.
func (c *checker) finding(where string, err error) error {
	var pd *page.DamagedError
	switch {
	case errors.As(err, &pd) && c.damaged[pd.Page]:
	case errors.As(err, &pd):
		c.r.Findings = append(c.r.Findings, Finding{Page: pd.Page, HasPage: true, What: fmt.Sprintf("%s: %s", where, pd.Reason)})
	case errors.Is(err, btree.ErrDamaged), errors.Is(err, table.ErrDamaged):
		c.r.Findings = append(c.r.Findings, Finding{What: fmt.Sprintf("%s: %v", where, err)})
	default:
		return err
	}
	c.complete = false
	return nil
}

// pages reads every page of the file and reports each whose checksum
// fails. It does not depend on any structure, so it finds each damaged
// page, also one that no structure reaches.
func (c *checker) pages() error {
	buf := make([]byte, c.pageSize)
	for no := uint64(1); no < c.snap.Count(); no++ {
		err := c.snap.Read(no, buf)
		var pd *page.DamagedError
		switch {
		case err == nil:
		case errors.As(err, &pd):
			c.r.Findings = append(c.r.Findings, Finding{Page: no, HasPage: true, What: pd.Reason})
			c.damaged[no] = true
		default:
			return err
		}
	}
	return nil
}

// use records the pages of a structure. A page used twice is a fault.
func (c *checker) use(who string, pages []uint64) {
	for _, no := range pages {
		if other, ok := c.owner[no]; ok {
			c.r.Findings = append(c.r.Findings, Finding{Page: no, HasPage: true, What: fmt.Sprintf("used by %s and by %s", other, who)})
			continue
		}
		c.owner[no] = who
	}
}

// tree checks a tree and records its pages.
func (c *checker) tree(who string, root uint64) (btree.Stats, bool, error) {
	st, err := btree.Open(root, c.pageSize).Check(c.snap)
	if err != nil {
		return st, false, c.finding(who, err)
	}
	c.use(who, st.Pages)
	return st, true, nil
}

// structure walks the catalog, every table and index, and the free list.
// If all of them were walked to the end, every page but the header must
// belong to exactly one of them.
func (c *checker) structure() error {
	free, err := c.snap.FreePages()
	if err != nil {
		if err := c.finding("free list", err); err != nil {
			return err
		}
	}
	c.use("the free list", free)
	for i := 0; i < page.Roots; i++ {
		if root := c.snap.Root(i); i != table.CatalogSlot && root != 0 {
			c.r.Findings = append(c.r.Findings, Finding{What: fmt.Sprintf("root slot %d is set to page %d, and no structure uses the slot", i, root)})
			c.complete = false
		}
	}
	if root := c.snap.Root(table.CatalogSlot); root != 0 {
		st, ok, err := c.tree("the catalog", root)
		if err != nil {
			return err
		}
		if ok {
			c.r.Stats.CatalogPages = len(st.Pages)
		}
	}
	v, err := table.Open(c.snap, c.pageSize)
	if err != nil {
		return c.finding("schema", err)
	}
	c.r.Stats.SchemaVersion = v.Schema().Version
	for _, t := range v.Schema().Tables {
		if err := c.table(v, &t); err != nil {
			return err
		}
	}
	if c.complete {
		for no := uint64(1); no < c.snap.Count(); no++ {
			if _, ok := c.owner[no]; !ok {
				c.r.Findings = append(c.r.Findings, Finding{Page: no, HasPage: true, What: "belongs to no tree and is not free"})
			}
		}
	}
	return nil
}

// rows counts the rows of a scan. Each row is decoded, and a scan through
// an index checks each entry against its row.
func rows(v *table.View, name string, o table.Options) (int, error) {
	r, err := v.Scan(name, o)
	if err != nil {
		return 0, err
	}
	n := 0
	for r.Next() {
		n++
	}
	return n, r.Err()
}

func (c *checker) table(v *table.View, t *table.Table) error {
	ts := TableStats{Name: t.Name}
	who := "table " + t.Name
	st, ok, err := c.tree(who, t.Root)
	if err != nil {
		return err
	}
	if ok {
		ts.Pages = len(st.Pages)
		if ts.Rows, err = rows(v, t.Name, table.Options{}); err != nil {
			if err := c.finding(who, err); err != nil {
				return err
			}
		}
	}
	for _, ix := range t.Indexes {
		is := IndexStats{Name: ix.Name}
		who := fmt.Sprintf("index %s of table %s", ix.Name, t.Name)
		st, ok, err := c.tree(who, ix.Root)
		if err != nil {
			return err
		}
		if ok {
			is.Pages = len(st.Pages)
			if is.Entries, err = rows(v, t.Name, table.Options{Index: ix.Name}); err != nil {
				if err := c.finding(who, err); err != nil {
					return err
				}
			} else if is.Entries != ts.Rows {
				// A scan through an index reads each row through the
				// table. So if the table could not be read in full, this
				// scan failed too, and the count is never compared with
				// a partial one.
				c.r.Findings = append(c.r.Findings, Finding{What: fmt.Sprintf("%s: %d entries, the table has %d rows", who, is.Entries, ts.Rows)})
			}
		}
		ts.Indexes = append(ts.Indexes, is)
	}
	c.r.Stats.Tables = append(c.r.Stats.Tables, ts)
	return nil
}
