package vfs

import (
	"fmt"
	"io"
	"math/rand"
	"sort"
	"sync"
)

// SectorSize is the unit a simulated power loss tears a write into. A
// disk writes one sector completely or not at all, but it gives no such
// promise for a longer write, nor about the order of writes between two
// syncs.
const SectorSize = 512

// Sim is a simulated disk in memory. What a file holds as of its last Sync
// is durable; writes and truncations after it are pending. The process
// sees both. Crash returns the disk as a power loss leaves it: the durable
// state, and a random part of what was pending.
//
// A Sim can also stop the process: after SetBudget(n), the n calls that
// change something (WriteAt, Sync, Truncate, an Open that creates, Remove)
// succeed, and every call after them returns ErrCrashed.
type Sim struct {
	mu      sync.Mutex
	files   map[string]*simFile
	budget  int // calls left; negative means no limit
	calls   int
	crashed bool
}

type simFile struct {
	durable []byte
	view    []byte // what the process sees
	pending []op
}

// op is a write, or a truncation to size. The kind is its own field: an
// empty write has nil data too, and read as a truncation to size 0 it
// emptied a file in a simulated power loss (TestCrashAfterEmptyWrite).
type op struct {
	truncate bool
	off      int64
	data     []byte
	size     int64
}

// NewSim returns an empty disk without a call budget.
func NewSim() *Sim {
	return &Sim{files: map[string]*simFile{}, budget: -1}
}

// SetBudget lets n more changing calls succeed; later calls return
// ErrCrashed.
func (s *Sim) SetBudget(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budget = n
}

// Calls returns how many changing calls have succeeded.
func (s *Sim) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// spend counts one changing call, or reports that the process stopped.
// The caller holds s.mu.
func (s *Sim) spend() error {
	if s.crashed {
		return ErrCrashed
	}
	if s.budget == 0 {
		s.crashed = true
		return ErrCrashed
	}
	if s.budget > 0 {
		s.budget--
	}
	s.calls++
	return nil
}

// Crash returns a new disk with the state a power loss leaves. Each file
// keeps its durable content. Of its pending operations, each one is kept
// or dropped at random, and each sector of a kept write is kept or dropped
// on its own; what is kept is applied in the original order. With r nil,
// all pending operations are dropped. The old disk returns ErrCrashed from
// then on.
func (s *Sim) Crash(r *rand.Rand) *Sim {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crashed = true
	out := NewSim()
	for name, f := range s.files {
		data := append([]byte(nil), f.durable...)
		if r != nil {
			for _, o := range f.pending {
				if r.Intn(2) == 0 {
					continue
				}
				if o.truncate {
					data = truncate(data, o.size)
					continue
				}
				for _, part := range sectors(o.off, o.data) {
					if r.Intn(2) == 0 {
						data = writeAt(data, part.off, part.data)
					}
				}
			}
		}
		out.files[name] = &simFile{durable: data, view: append([]byte(nil), data...)}
	}
	return out
}

// Names returns the names of the files on the disk, sorted.
func (s *Sim) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for n := range s.files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *Sim) Open(name string) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.crashed {
		return nil, ErrCrashed
	}
	if _, ok := s.files[name]; !ok {
		if err := s.spend(); err != nil {
			return nil, err
		}
		s.files[name] = &simFile{}
	}
	return &simHandle{s: s, name: name}, nil
}

func (s *Sim) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.spend(); err != nil {
		return err
	}
	if _, ok := s.files[name]; !ok {
		return fmt.Errorf("vfs: remove %s: no such file", name)
	}
	delete(s.files, name)
	return nil
}

// Rename moves the file, with what is durable and what is pending in it,
// to the new name. The rename itself is durable at once, as OS.Rename
// makes it with fsync on the directory.
func (s *Sim) Rename(oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.spend(); err != nil {
		return err
	}
	f, ok := s.files[oldName]
	if !ok {
		return fmt.Errorf("vfs: rename %s: no such file", oldName)
	}
	delete(s.files, oldName)
	s.files[newName] = f
	return nil
}

func (s *Sim) Exists(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.crashed {
		return false, ErrCrashed
	}
	_, ok := s.files[name]
	return ok, nil
}

type simHandle struct {
	s      *Sim
	name   string
	closed bool
}

// file returns the file behind the handle. The caller holds s.mu.
func (h *simHandle) file() (*simFile, error) {
	if h.s.crashed {
		return nil, ErrCrashed
	}
	if h.closed {
		return nil, fmt.Errorf("vfs: %s is closed", h.name)
	}
	f, ok := h.s.files[h.name]
	if !ok {
		return nil, fmt.Errorf("vfs: %s was removed", h.name)
	}
	return f, nil
}

func (h *simHandle) ReadAt(p []byte, off int64) (int, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	f, err := h.file()
	if err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fmt.Errorf("vfs: negative offset %d", off)
	}
	// os.File reads nothing and reports no error for an empty buffer,
	// also past the end (found by TestSimBehavesLikeOS).
	if len(p) == 0 {
		return 0, nil
	}
	if off >= int64(len(f.view)) {
		return 0, io.EOF
	}
	n := copy(p, f.view[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *simHandle) WriteAt(p []byte, off int64) (int, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	f, err := h.file()
	if err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fmt.Errorf("vfs: negative offset %d", off)
	}
	if err := h.s.spend(); err != nil {
		return 0, err
	}
	data := append([]byte(nil), p...)
	f.pending = append(f.pending, op{off: off, data: data})
	f.view = writeAt(f.view, off, data)
	return len(p), nil
}

func (h *simHandle) Truncate(size int64) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	f, err := h.file()
	if err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("vfs: negative size %d", size)
	}
	if err := h.s.spend(); err != nil {
		return err
	}
	f.pending = append(f.pending, op{truncate: true, size: size})
	f.view = truncate(f.view, size)
	return nil
}

func (h *simHandle) Sync() error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	f, err := h.file()
	if err != nil {
		return err
	}
	if err := h.s.spend(); err != nil {
		return err
	}
	f.durable = append([]byte(nil), f.view...)
	f.pending = nil
	return nil
}

func (h *simHandle) Size() (int64, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	f, err := h.file()
	if err != nil {
		return 0, err
	}
	return int64(len(f.view)), nil
}

func (h *simHandle) Close() error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.closed = true
	return nil
}

// writeAt writes data at off into b, growing it with zeros as a file does.
// An empty write does not grow it, as with os.File (found by
// TestSimBehavesLikeOS).
func writeAt(b []byte, off int64, data []byte) []byte {
	if len(data) == 0 {
		return b
	}
	if end := off + int64(len(data)); end > int64(len(b)) {
		b = append(b, make([]byte, end-int64(len(b)))...)
	}
	copy(b[off:], data)
	return b
}

func truncate(b []byte, size int64) []byte {
	if size <= int64(len(b)) {
		return b[:size]
	}
	return append(b, make([]byte, size-int64(len(b)))...)
}

type part struct {
	off  int64
	data []byte
}

// sectors cuts a write at the sector boundaries of the file.
func sectors(off int64, data []byte) []part {
	var out []part
	for len(data) > 0 {
		n := SectorSize - int(off%SectorSize)
		if n > len(data) {
			n = len(data)
		}
		out = append(out, part{off, data[:n]})
		off += int64(n)
		data = data[n:]
	}
	return out
}
