// Package store joins the database file and its log into pages that
// readers and one writer use at the same time (design.md).
//
// A Snapshot reads the database as of one commit: each page from the log,
// newest image up to that commit, else from the file. A Tx is the one
// writer: its commit appends the changed pages to the log, and always page
// 0 with the new page count, so that the header is recovered from the log
// like any page. Checkpoint copies the newest pages into the file and
// starts a new log, but only while no open snapshot needs an older page.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/vfs"
	"github.com/dyadik-eu/datumujo/internal/wal"
)

// DefaultPageSize is the page size of a new file until the measurement of
// Q-2 sets it.
const DefaultPageSize = 4096

// Options are used when Open creates a file.
type Options struct {
	PageSize int // 0 means DefaultPageSize
}

// Store is an open database file with its log.
type Store struct {
	unlock   func() error
	db       vfs.File
	logf     vfs.File
	log      *wal.Log
	pageSize int

	// writer is held by the one Tx and by Checkpoint.
	writer sync.Mutex

	mu      sync.Mutex
	hdr     page.Header // the header as of the last commit
	readers map[int]int // open snapshots per commit number
	closed  bool
}

// ErrClosed is returned by calls on a closed Store.
var ErrClosed = errors.New("store: closed")

// Open opens the database in the file name of fs, with its log in
// name+"-log". If the file does not exist, Open creates it. It takes an
// exclusive lock on name+"-lock" first, so a second Open of the same
// database fails with vfs.ErrLocked until Close, in this process or
// another one (S-1).
func Open(fs vfs.FS, name string, opt Options) (s *Store, err error) {
	unlock, err := fs.Lock(name + "-lock")
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			unlock()
		}
	}()
	exists, err := fs.Exists(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := create(fs, name, opt); err != nil {
			return nil, err
		}
	}
	db, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	s, err = open(fs, name, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.unlock = unlock
	return s, nil
}

// create makes a new database file: the header goes to name+"-new", is
// synced, and the file is renamed to name. So a database file that exists
// had its header on the disk once. A power loss in the middle leaves no
// file under name, or a whole one. Written in place instead, a power
// loss during the creation left a torn header and no log, and the file
// could not be opened; and it could not be told from a database cut short
// by damage, so recreating it quietly was no way out.
func create(fs vfs.FS, name string, opt Options) error {
	logName := name + "-log"
	if ok, err := fs.Exists(logName); err != nil {
		return err
	} else if ok {
		lf, err := fs.Open(logName)
		if err != nil {
			return err
		}
		n, err := lf.Size()
		lf.Close()
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("store: %s does not exist, but %s does; refusing to start over", name, logName)
		}
	}
	ps := opt.PageSize
	if ps == 0 {
		ps = DefaultPageSize
	}
	if !page.ValidSize(ps) {
		return fmt.Errorf("store: page size %d is not a power of two from %d to %d", ps, page.MinSize, page.MaxSize)
	}
	tmp := name + "-new"
	if ok, err := fs.Exists(tmp); err != nil {
		return err
	} else if ok {
		if err := fs.Remove(tmp); err != nil { // left by a creation that did not finish
			return err
		}
	}
	f, err := fs.Open(tmp)
	if err != nil {
		return err
	}
	hdr := page.EncodeHeader(page.Header{Version: page.Version, PageSize: ps, PageCount: 1})
	if _, err := f.WriteAt(hdr, 0); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return fs.Rename(tmp, name)
}

func open(fs vfs.FS, name string, db vfs.File) (*Store, error) {
	logName := name + "-log"
	logExists, err := fs.Exists(logName)
	if err != nil {
		return nil, err
	}
	h, herr := page.ReadHeader(db)
	if herr != nil && !(errors.Is(herr, page.ErrDamaged) && logExists) {
		return nil, herr
	}
	logf, err := fs.Open(logName)
	if err != nil {
		return nil, err
	}
	ps := h.PageSize
	if herr != nil {
		// The header page is damaged. A checkpoint that did not finish
		// can leave it so; then the log holds its newest image.
		if ps, err = wal.PageSize(logf); err != nil {
			logf.Close()
			return nil, fmt.Errorf("%w; the log cannot replace it: %v", herr, err)
		}
	}
	l, _, err := wal.Open(logf, ps)
	if err != nil {
		logf.Close()
		return nil, err
	}
	// The newest header is in the log if a commit is there: every commit
	// writes page 0. It replaces a damaged header page in the file too.
	img := make([]byte, ps)
	inLog, err := l.Read(0, l.Last(), img)
	if err != nil {
		logf.Close()
		return nil, err
	}
	switch {
	case inLog:
		if h, err = page.DecodeHeader(img); err != nil {
			logf.Close()
			return nil, err
		}
		if c, _ := l.Count(); c != h.PageCount {
			logf.Close()
			return nil, &page.DamagedError{Page: 0, Reason: fmt.Sprintf("the log says %d pages, its header image %d", c, h.PageCount)}
		}
	case herr != nil:
		logf.Close()
		return nil, fmt.Errorf("%w; the log holds no image of it", herr)
	}
	return &Store{db: db, logf: logf, log: l, pageSize: ps, hdr: h, readers: map[int]int{}}, nil
}

// Close closes the files. Open snapshots and transactions must be closed
// before.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	err := s.logf.Close()
	if e := s.db.Close(); err == nil {
		err = e
	}
	if e := s.unlock(); err == nil {
		err = e
	}
	return err
}

// PageSize returns the page size of the file.
func (s *Store) PageSize() int { return s.pageSize }

// Payload returns how many bytes of a page are free for content.
func (s *Store) Payload() int { return s.pageSize - page.TrailerSize }

// read reads page no as of commit upTo: from the log, else from the file.
// Page 0 is the header; its fields are read through Count, FreeCount and
// Root, and inside a Tx they may have changed since the last commit.
func (s *Store) read(no uint64, upTo int, count uint64, buf []byte) error {
	if no == 0 {
		return errors.New("store: page 0 is the header; read its fields instead")
	}
	if len(buf) != s.pageSize {
		return fmt.Errorf("store: buffer of %d bytes for a page of %d", len(buf), s.pageSize)
	}
	if no >= count {
		return fmt.Errorf("store: page %d is past the end; the database has %d pages", no, count)
	}
	ok, err := s.log.Read(no, upTo, buf)
	if ok || err != nil {
		return err
	}
	return page.Read(s.db, no, buf)
}

// Snapshot is a reader of the database as of one commit.
type Snapshot struct {
	s      *Store
	commit int
	hdr    page.Header
	once   sync.Once
}

// Snapshot starts a reader of the last commit. It never waits for the
// writer. It must be closed; while it is open, a checkpoint that would
// remove a page it needs does not run.
func (s *Store) Snapshot() (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	c := s.log.Last()
	s.readers[c]++
	return &Snapshot{s: s, commit: c, hdr: s.hdr}, nil
}

// Count returns the page count as of the snapshot.
func (r *Snapshot) Count() uint64 { return r.hdr.PageCount }

// FreeCount returns the number of free pages as of the snapshot.
func (r *Snapshot) FreeCount() uint64 { return r.hdr.FreeCount }

// Root returns root slot i as of the snapshot, 0 if unused.
func (r *Snapshot) Root(i int) uint64 { return r.hdr.Root[i] }

// Read reads page no into buf, which must be one page long. The content is
// buf[:Payload()]; the rest is the checksum.
func (r *Snapshot) Read(no uint64, buf []byte) error {
	return r.s.read(no, r.commit, r.hdr.PageCount, buf)
}

// Close ends the snapshot. A second Close does nothing.
func (r *Snapshot) Close() {
	r.once.Do(func() {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		if r.s.readers[r.commit]--; r.s.readers[r.commit] == 0 {
			delete(r.s.readers, r.commit)
		}
	})
}

// Tx is the one write transaction. It sees the last commit and its own
// writes.
type Tx struct {
	s      *Store
	commit int
	hdr    page.Header
	dirty  map[uint64][]byte
	freed  map[uint64]bool // freed in this Tx
	done   bool
}

// Begin starts the write transaction. It waits while another one or a
// checkpoint runs.
func (s *Store) Begin() (*Tx, error) {
	s.writer.Lock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.writer.Unlock()
		return nil, ErrClosed
	}
	return &Tx{s: s, commit: s.log.Last(), hdr: s.hdr, dirty: map[uint64][]byte{}, freed: map[uint64]bool{}}, nil
}

// ErrDone is returned by calls on a committed or rolled back Tx.
var ErrDone = errors.New("store: transaction is done")

// Count returns the page count, the pages allocated in this Tx included.
func (t *Tx) Count() uint64 { return t.hdr.PageCount }

// FreeCount returns the number of free pages.
func (t *Tx) FreeCount() uint64 { return t.hdr.FreeCount }

// Root returns root slot i, 0 if unused.
func (t *Tx) Root(i int) uint64 { return t.hdr.Root[i] }

// SetRoot sets root slot i to page no, or to 0 to clear it.
func (t *Tx) SetRoot(i int, no uint64) error {
	if t.done {
		return ErrDone
	}
	if i < 0 || i >= page.Roots {
		return fmt.Errorf("store: root slot %d; there are %d", i, page.Roots)
	}
	if no >= t.hdr.PageCount {
		return fmt.Errorf("store: root %d set to page %d, past the end", i, no)
	}
	t.hdr.Root[i] = no
	// The header changes, and a Tx that changes nothing else must still
	// commit it. dirty[0] only marks that; Commit encodes the header.
	t.dirty[0] = nil
	return nil
}

// Read reads page no into buf, which must be one page long: this Tx's
// write if there is one, else the page as of the last commit.
func (t *Tx) Read(no uint64, buf []byte) error {
	if t.done {
		return ErrDone
	}
	if no == 0 {
		return errors.New("store: page 0 is the header; read its fields instead")
	}
	if p, ok := t.dirty[no]; ok {
		if len(buf) != len(p) {
			return fmt.Errorf("store: buffer of %d bytes for a page of %d", len(buf), len(p))
		}
		copy(buf, p)
		return nil
	}
	return t.s.read(no, t.commit, t.hdr.PageCount, buf)
}

// Write sets the content of page no to payload, at most Payload() bytes;
// the rest of the page is zero. Page 0 is the header and belongs to the
// store.
func (t *Tx) Write(no uint64, payload []byte) error {
	if t.done {
		return ErrDone
	}
	if no == 0 {
		return errors.New("store: page 0 is the header")
	}
	if no >= t.hdr.PageCount {
		return fmt.Errorf("store: page %d is past the end; allocate it first", no)
	}
	if t.freed[no] {
		return fmt.Errorf("store: page %d was freed in this transaction", no)
	}
	if len(payload) > t.s.Payload() {
		return fmt.Errorf("store: %d bytes of content, a page holds %d", len(payload), t.s.Payload())
	}
	p := make([]byte, t.s.pageSize)
	copy(p, payload)
	t.dirty[no] = p
	return nil
}

// freeMark starts the content of a free page, before the number of the
// next free page. Allocate checks it: a page on the free list without it
// means the list is damaged, and handing that page out would overwrite
// data.
var freeMark = [8]byte{'d', 'm', 'j', 'o', 'f', 'r', 'e', 'e'}

// Allocate returns a page for new content: the page freed last, or else a
// new one at the end. Its content is zero until written.
func (t *Tx) Allocate() (uint64, error) {
	if t.done {
		return 0, ErrDone
	}
	if t.hdr.FreeCount == 0 {
		no := t.hdr.PageCount
		t.hdr.PageCount++
		t.dirty[no] = make([]byte, t.s.pageSize)
		return no, nil
	}
	no := t.hdr.FreeHead
	buf := make([]byte, t.s.pageSize)
	if err := t.Read(no, buf); err != nil {
		return 0, err
	}
	if string(buf[:8]) != string(freeMark[:]) {
		return 0, &page.DamagedError{Page: no, Reason: "on the free list, but not marked free"}
	}
	next := binary.BigEndian.Uint64(buf[8:])
	if next >= t.hdr.PageCount || (next == 0) != (t.hdr.FreeCount == 1) {
		return 0, &page.DamagedError{Page: no, Reason: fmt.Sprintf("free page points to page %d, with %d free pages left", next, t.hdr.FreeCount-1)}
	}
	t.hdr.FreeHead, t.hdr.FreeCount = next, t.hdr.FreeCount-1
	delete(t.freed, no)
	t.dirty[no] = make([]byte, t.s.pageSize)
	return no, nil
}

// Free puts page no on the free list. Its content is lost; a later
// Allocate hands it out again. A snapshot that still reads the page sees
// its old content, from the log or the file, as of its commit.
func (t *Tx) Free(no uint64) error {
	if t.done {
		return ErrDone
	}
	if no == 0 || no >= t.hdr.PageCount {
		return fmt.Errorf("store: page %d cannot be freed; pages 1 to %d can", no, t.hdr.PageCount-1)
	}
	if t.freed[no] {
		return fmt.Errorf("store: page %d freed twice", no)
	}
	p := make([]byte, t.s.pageSize)
	copy(p, freeMark[:])
	binary.BigEndian.PutUint64(p[8:], t.hdr.FreeHead)
	t.dirty[no] = p
	t.freed[no] = true
	t.hdr.FreeHead, t.hdr.FreeCount = no, t.hdr.FreeCount+1
	return nil
}

// Commit appends the changed pages and the header to the log and syncs
// it. When it returns nil, the commit is durable. The Tx is done either
// way.
func (t *Tx) Commit() error {
	if t.done {
		return ErrDone
	}
	t.done = true
	defer t.s.writer.Unlock()
	if len(t.dirty) == 0 {
		return nil
	}
	pages := make([]wal.Page, 0, len(t.dirty)+1)
	pages = append(pages, wal.Page{No: 0, Data: page.EncodeHeader(t.hdr)})
	for no, p := range t.dirty {
		if no == 0 {
			continue // the header, written above
		}
		page.Seal(p, no)
		pages = append(pages, wal.Page{No: no, Data: p})
	}
	if err := t.s.log.Commit(pages, t.hdr.PageCount); err != nil {
		return err
	}
	t.s.mu.Lock()
	t.s.hdr = t.hdr
	t.s.mu.Unlock()
	return nil
}

// Rollback ends the Tx without a change. A Rollback after Commit does
// nothing.
func (t *Tx) Rollback() {
	if t.done {
		return
	}
	t.done = true
	t.s.writer.Unlock()
}

// Checkpoint copies the newest page images from the log into the file,
// syncs the file and starts a new log. It does nothing and reports false
// while a snapshot of an earlier commit is open: that snapshot may still
// need an older page, and the file must not change under it (D-2). It
// waits for the writer.
func (s *Store) Checkpoint() (bool, error) {
	s.writer.Lock()
	defer s.writer.Unlock()
	s.mu.Lock()
	last := s.log.Last()
	for c := range s.readers {
		if c < last {
			s.mu.Unlock()
			return false, nil
		}
	}
	s.mu.Unlock()
	if s.log.Commits() == 0 {
		return true, nil
	}
	err := s.log.Newest(func(no uint64, img []byte) error {
		_, err := s.db.WriteAt(img, int64(no)*int64(s.pageSize))
		return err
	})
	if err != nil {
		return false, err
	}
	if err := s.db.Sync(); err != nil {
		return false, err
	}
	if err := s.log.Reset(); err != nil {
		return false, err
	}
	return true, nil
}
