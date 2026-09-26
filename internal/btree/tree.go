package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Pages reads pages: a snapshot or a write transaction of the store.
type Pages interface {
	Read(no uint64, buf []byte) error
}

// WritePages also changes pages: a write transaction of the store.
type WritePages interface {
	Pages
	Write(no uint64, payload []byte) error
	Allocate() (uint64, error)
	Free(no uint64) error
}

// Tree is a B+tree with its root page, for pages of one size.
type Tree struct {
	Root     uint64
	pageSize int
	lim      Limits
}

// Open returns the tree with the given root page.
func Open(root uint64, pageSize int) *Tree {
	return &Tree{Root: root, pageSize: pageSize, lim: LimitsFor(pageSize)}
}

// Create makes an empty tree and returns it. Its root is a new page.
func Create(w WritePages, pageSize int) (*Tree, error) {
	root, err := w.Allocate()
	if err != nil {
		return nil, err
	}
	t := Open(root, pageSize)
	if err := t.write(w, root, &node{leaf: true}); err != nil {
		return nil, err
	}
	return t, nil
}

// ErrKeyTooLong and ErrEmptyKey are returned for keys the tree cannot
// hold.
var (
	ErrKeyTooLong = errors.New("btree: key too long")
	ErrEmptyKey   = errors.New("btree: empty key")
)

func (t *Tree) checkKey(key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > t.lim.MaxKey {
		return fmt.Errorf("%w: %d bytes, at most %d for pages of %d", ErrKeyTooLong, len(key), t.lim.MaxKey, t.pageSize)
	}
	return nil
}

func (t *Tree) read(r Pages, no uint64) (*node, error) {
	buf := make([]byte, t.pageSize)
	if err := r.Read(no, buf); err != nil {
		return nil, err
	}
	return decode(no, buf[:t.lim.Payload])
}

func (t *Tree) write(w WritePages, no uint64, n *node) error {
	if n.size() > t.lim.Payload {
		return fmt.Errorf("btree: page %d: node of %d bytes, a page holds %d", no, n.size(), t.lim.Payload)
	}
	return w.Write(no, n.encode(t.lim.Payload))
}

// search returns the first index whose key is not less than key, and
// whether that key is equal.
func search(n *node, key []byte) (int, bool) {
	i := sort.Search(len(n.entries), func(i int) bool { return bytes.Compare(n.entries[i].key, key) >= 0 })
	return i, i < len(n.entries) && bytes.Equal(n.entries[i].key, key)
}

// childIndex returns the position of the child of internal node n that
// covers key: -1 for n.first, else the index of the entry whose child it
// is.
func childIndex(n *node, key []byte) int {
	i := sort.Search(len(n.entries), func(i int) bool { return bytes.Compare(n.entries[i].key, key) > 0 })
	return i - 1
}

func childAt(n *node, i int) uint64 {
	if i < 0 {
		return n.first
	}
	return n.entries[i].child
}

// maxDepth bounds a descent. A tree of pages of 512 bytes with millions of
// keys is less than 20 deep; a path longer than this is a loop of pages.
const maxDepth = 64

// Get returns the value of key, and false if the tree does not hold it.
func (t *Tree) Get(r Pages, key []byte) ([]byte, bool, error) {
	if err := t.checkKey(key); err != nil {
		return nil, false, err
	}
	no := t.Root
	for depth := 0; ; depth++ {
		if depth > maxDepth {
			return nil, false, &damaged{no, "the tree is deeper than any tree can be; a page points back"}
		}
		n, err := t.read(r, no)
		if err != nil {
			return nil, false, err
		}
		if n.leaf {
			i, ok := search(n, key)
			if !ok {
				return nil, false, nil
			}
			v, err := t.value(r, n.entries[i])
			return v, err == nil, err
		}
		no = childAt(n, childIndex(n, key))
	}
}

// value returns the value of a leaf entry, reading its overflow chain.
func (t *Tree) value(r Pages, e entry) ([]byte, error) {
	if !e.overflow {
		return append([]byte(nil), e.value...), nil
	}
	out := make([]byte, 0, e.length)
	buf := make([]byte, t.pageSize)
	no := e.first
	for uint64(len(out)) < e.length {
		if no == 0 {
			return nil, fmt.Errorf("btree: overflow chain ends after %d of %d bytes: %w", len(out), e.length, ErrDamaged)
		}
		if err := r.Read(no, buf); err != nil {
			return nil, err
		}
		chunk := buf[8:t.lim.Payload]
		if rest := e.length - uint64(len(out)); rest < uint64(len(chunk)) {
			chunk = chunk[:rest]
		}
		out = append(out, chunk...)
		no = binary.BigEndian.Uint64(buf)
	}
	if no != 0 {
		return nil, fmt.Errorf("btree: overflow chain goes on past its %d bytes: %w", e.length, ErrDamaged)
	}
	return out, nil
}

// writeOverflow stores value in a new chain and returns its first page.
func (t *Tree) writeOverflow(w WritePages, value []byte) (uint64, error) {
	chunk := t.lim.Payload - 8
	var pages []uint64
	for off := 0; off < len(value); off += chunk {
		no, err := w.Allocate()
		if err != nil {
			return 0, err
		}
		pages = append(pages, no)
	}
	for i, no := range pages {
		p := make([]byte, 8, t.lim.Payload)
		if i+1 < len(pages) {
			binary.BigEndian.PutUint64(p, pages[i+1])
		}
		end := (i + 1) * chunk
		if end > len(value) {
			end = len(value)
		}
		p = append(p, value[i*chunk:end]...)
		if err := w.Write(no, p); err != nil {
			return 0, err
		}
	}
	return pages[0], nil
}

// freeOverflow frees the chain of a leaf entry.
func (t *Tree) freeOverflow(w WritePages, e entry) error {
	if !e.overflow {
		return nil
	}
	buf := make([]byte, t.pageSize)
	no := e.first
	for n := uint64(0); no != 0; n++ {
		if n*uint64(t.lim.Payload-8) > e.length {
			return fmt.Errorf("btree: overflow chain longer than its %d bytes: %w", e.length, ErrDamaged)
		}
		if err := w.Read(no, buf); err != nil {
			return err
		}
		next := binary.BigEndian.Uint64(buf)
		if err := w.Free(no); err != nil {
			return err
		}
		no = next
	}
	return nil
}

// leafEntry makes the leaf entry for key and value, with an overflow
// chain if the value does not fit in a quarter of a node.
func (t *Tree) leafEntry(w WritePages, key, value []byte) (entry, error) {
	e := entry{key: append([]byte(nil), key...), value: append([]byte(nil), value...)}
	if (&node{leaf: true}).cellSize(e) <= t.lim.MaxCell {
		return e, nil
	}
	first, err := t.writeOverflow(w, value)
	if err != nil {
		return entry{}, err
	}
	return entry{key: e.key, overflow: true, length: uint64(len(value)), first: first}, nil
}

// frame is one node on the path from the root to a leaf.
type frame struct {
	no  uint64
	n   *node
	idx int // internal: the child index taken
}

func (t *Tree) path(r Pages, key []byte) ([]frame, error) {
	var path []frame
	no := t.Root
	for depth := 0; ; depth++ {
		if depth > maxDepth {
			return nil, &damaged{no, "the tree is deeper than any tree can be; a page points back"}
		}
		n, err := t.read(r, no)
		if err != nil {
			return nil, err
		}
		if n.leaf {
			return append(path, frame{no: no, n: n}), nil
		}
		i := childIndex(n, key)
		path = append(path, frame{no: no, n: n, idx: i})
		no = childAt(n, i)
	}
}

// Put sets the value of key.
func (t *Tree) Put(w WritePages, key, value []byte) error {
	if err := t.checkKey(key); err != nil {
		return err
	}
	path, err := t.path(w, key)
	if err != nil {
		return err
	}
	leaf := path[len(path)-1]
	e, err := t.leafEntry(w, key, value)
	if err != nil {
		return err
	}
	i, found := search(leaf.n, key)
	if found {
		if err := t.freeOverflow(w, leaf.n.entries[i]); err != nil {
			return err
		}
		leaf.n.entries[i] = e
	} else {
		leaf.n.entries = append(leaf.n.entries, entry{})
		copy(leaf.n.entries[i+1:], leaf.n.entries[i:])
		leaf.n.entries[i] = e
	}
	return t.store(w, path, len(path)-1)
}

// store writes node path[level] and splits it, up the path, if it is too
// large.
func (t *Tree) store(w WritePages, path []frame, level int) error {
	f := path[level]
	if f.n.size() <= t.lim.Payload {
		return t.write(w, f.no, f.n)
	}
	left, sep, right := split(f.n)
	if level == 0 {
		// The root splits: both halves go to new pages, and the root page
		// becomes an internal node over them, so its number stays.
		l, err := w.Allocate()
		if err != nil {
			return err
		}
		r, err := w.Allocate()
		if err != nil {
			return err
		}
		if err := t.write(w, l, left); err != nil {
			return err
		}
		if err := t.write(w, r, right); err != nil {
			return err
		}
		return t.write(w, f.no, &node{first: l, entries: []entry{{key: sep, child: r}}})
	}
	r, err := w.Allocate()
	if err != nil {
		return err
	}
	if err := t.write(w, f.no, left); err != nil {
		return err
	}
	if err := t.write(w, r, right); err != nil {
		return err
	}
	parent := path[level-1]
	at := parent.idx + 1
	parent.n.entries = append(parent.n.entries, entry{})
	copy(parent.n.entries[at+1:], parent.n.entries[at:])
	parent.n.entries[at] = entry{key: sep, child: r}
	return t.store(w, path, level-1)
}

// split cuts a node in two halves of about equal size. For a leaf, the
// separator is the first key of the right half and stays in it; for an
// internal node, the middle key moves up and its child becomes the first
// child of the right half.
func split(n *node) (left *node, sep []byte, right *node) {
	total := n.size()
	acc := 3
	cut := 1
	for i, e := range n.entries {
		acc += n.cellSize(e)
		if acc > total/2 && i > 0 {
			cut = i
			break
		}
	}
	if cut >= len(n.entries) {
		cut = len(n.entries) - 1
	}
	if n.leaf {
		left = &node{leaf: true, entries: append([]entry(nil), n.entries[:cut]...)}
		right = &node{leaf: true, entries: append([]entry(nil), n.entries[cut:]...)}
		return left, append([]byte(nil), n.entries[cut].key...), right
	}
	mid := n.entries[cut]
	left = &node{first: n.first, entries: append([]entry(nil), n.entries[:cut]...)}
	right = &node{first: mid.child, entries: append([]entry(nil), n.entries[cut+1:]...)}
	return left, append([]byte(nil), mid.key...), right
}

// Delete removes key and reports whether the tree held it.
func (t *Tree) Delete(w WritePages, key []byte) (bool, error) {
	if err := t.checkKey(key); err != nil {
		return false, err
	}
	path, err := t.path(w, key)
	if err != nil {
		return false, err
	}
	leaf := path[len(path)-1]
	i, found := search(leaf.n, key)
	if !found {
		return false, nil
	}
	if err := t.freeOverflow(w, leaf.n.entries[i]); err != nil {
		return false, err
	}
	leaf.n.entries = append(leaf.n.entries[:i], leaf.n.entries[i+1:]...)
	return true, t.shrink(w, path, len(path)-1)
}

// shrink writes node path[level] after an entry left it. A node below a
// quarter of a page is merged with a neighbour if both fit in one page.
// An internal root with a single child takes over the child's content.
func (t *Tree) shrink(w WritePages, path []frame, level int) error {
	f := path[level]
	if level == 0 {
		if !f.n.leaf && len(f.n.entries) == 0 {
			child, err := t.read(w, f.n.first)
			if err != nil {
				return err
			}
			if err := t.write(w, f.no, child); err != nil {
				return err
			}
			return w.Free(f.n.first)
		}
		return t.write(w, f.no, f.n)
	}
	if f.n.size() >= t.lim.Payload/4 {
		return t.write(w, f.no, f.n)
	}
	parent := path[level-1]
	if len(parent.n.entries) == 0 {
		// The parent has one child, f, and no neighbour to merge with. It
		// stays so when it could not merge itself, as a node too small
		// next to a full one.
		return t.write(w, f.no, f.n)
	}
	// Merge with the right neighbour, or with the left one if f is the
	// last child. li is the entry of the parent that separates the two.
	li := parent.idx + 1
	leftNo, rightNo := f.no, uint64(0)
	var left, right *node
	if li < len(parent.n.entries) {
		rightNo = parent.n.entries[li].child
		r, err := t.read(w, rightNo)
		if err != nil {
			return err
		}
		left, right = f.n, r
	} else {
		li = parent.idx
		leftNo, rightNo = childAt(parent.n, li-1), f.no
		l, err := t.read(w, leftNo)
		if err != nil {
			return err
		}
		left, right = l, f.n
	}
	merged := &node{leaf: left.leaf, first: left.first, entries: append([]entry(nil), left.entries...)}
	if !left.leaf {
		merged.entries = append(merged.entries, entry{key: parent.n.entries[li].key, child: right.first})
	}
	merged.entries = append(merged.entries, right.entries...)
	if merged.size() > t.lim.Payload {
		return t.write(w, f.no, f.n) // too large to merge; stays below a quarter
	}
	if err := t.write(w, leftNo, merged); err != nil {
		return err
	}
	if err := w.Free(rightNo); err != nil {
		return err
	}
	parent.n.entries = append(parent.n.entries[:li], parent.n.entries[li+1:]...)
	return t.shrink(w, path, level-1)
}

// Drop frees every page of the tree, the root included.
func (t *Tree) Drop(w WritePages) error {
	var walk func(no uint64, depth int) error
	walk = func(no uint64, depth int) error {
		if depth > maxDepth {
			return &damaged{no, "the tree is deeper than any tree can be; a page points back"}
		}
		n, err := t.read(w, no)
		if err != nil {
			return err
		}
		if n.leaf {
			for _, e := range n.entries {
				if err := t.freeOverflow(w, e); err != nil {
					return err
				}
			}
		} else {
			if err := walk(n.first, depth+1); err != nil {
				return err
			}
			for _, e := range n.entries {
				if err := walk(e.child, depth+1); err != nil {
					return err
				}
			}
		}
		return w.Free(no)
	}
	return walk(t.Root, 0)
}
