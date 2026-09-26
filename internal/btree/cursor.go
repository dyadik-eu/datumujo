package btree

// Cursor walks the entries of a tree in key order, in both directions. It
// holds the path from the root to its leaf, so it needs no links between
// leaves. It reads from Pages that do not change while it walks: a
// snapshot, or a transaction that writes nothing meanwhile.
type Cursor struct {
	t     *Tree
	r     Pages
	stack []frame // root first; in the leaf, idx is the entry
	err   error
}

// Cursor returns a cursor on the tree, not yet positioned.
func (t *Tree) Cursor(r Pages) *Cursor { return &Cursor{t: t, r: r} }

// Valid reports whether the cursor stands on an entry.
func (c *Cursor) Valid() bool {
	if c.err != nil || len(c.stack) == 0 {
		return false
	}
	f := c.stack[len(c.stack)-1]
	return f.idx >= 0 && f.idx < len(f.n.entries)
}

// Err returns the error that stopped the cursor, if any. A cursor that is
// not Valid has either reached an end or failed; Err tells them apart.
func (c *Cursor) Err() error { return c.err }

// Key returns the key the cursor stands on.
func (c *Cursor) Key() []byte {
	f := c.stack[len(c.stack)-1]
	return append([]byte(nil), f.n.entries[f.idx].key...)
}

// Value returns the value the cursor stands on.
func (c *Cursor) Value() ([]byte, error) {
	f := c.stack[len(c.stack)-1]
	return c.t.value(c.r, f.n.entries[f.idx])
}

// descend goes down from node no to its leftmost (first) or rightmost
// leaf entry.
func (c *Cursor) descend(no uint64, first bool) {
	for {
		if len(c.stack) > maxDepth {
			c.err = &damaged{no, "the tree is deeper than any tree can be; a page points back"}
			return
		}
		n, err := c.t.read(c.r, no)
		if err != nil {
			c.err = err
			return
		}
		if n.leaf {
			i := 0
			if !first {
				i = len(n.entries) - 1
			}
			c.stack = append(c.stack, frame{no: no, n: n, idx: i})
			return
		}
		i := -1
		if !first {
			i = len(n.entries) - 1
		}
		c.stack = append(c.stack, frame{no: no, n: n, idx: i})
		no = childAt(n, i)
	}
}

// First moves to the first entry.
func (c *Cursor) First() {
	c.stack, c.err = c.stack[:0], nil
	c.descend(c.t.Root, true)
	c.settle(true)
}

// Last moves to the last entry.
func (c *Cursor) Last() {
	c.stack, c.err = c.stack[:0], nil
	c.descend(c.t.Root, false)
	c.settle(false)
}

// Seek moves to the first entry whose key is not less than key.
func (c *Cursor) Seek(key []byte) {
	c.stack, c.err = c.stack[:0], nil
	no := c.t.Root
	for {
		if len(c.stack) > maxDepth {
			c.err = &damaged{no, "the tree is deeper than any tree can be; a page points back"}
			return
		}
		n, err := c.t.read(c.r, no)
		if err != nil {
			c.err = err
			return
		}
		if n.leaf {
			i, _ := search(n, key)
			c.stack = append(c.stack, frame{no: no, n: n, idx: i})
			c.settle(true)
			return
		}
		i := childIndex(n, key)
		c.stack = append(c.stack, frame{no: no, n: n, idx: i})
		no = childAt(n, i)
	}
}

// settle moves off a position past the end or before the start of a leaf
// to the next entry in the given direction. Leaves can be empty.
func (c *Cursor) settle(forward bool) {
	for c.err == nil && len(c.stack) > 0 && !c.Valid() {
		if !c.step(forward) {
			return
		}
	}
}

// Next moves to the next entry.
func (c *Cursor) Next() {
	if !c.Valid() {
		return
	}
	c.stack[len(c.stack)-1].idx++
	c.settle(true)
}

// Prev moves to the previous entry.
func (c *Cursor) Prev() {
	if !c.Valid() {
		return
	}
	c.stack[len(c.stack)-1].idx--
	c.settle(false)
}

// step leaves the current leaf for the next one in the given direction.
// It reports false at the end of the tree, leaving the cursor not Valid.
func (c *Cursor) step(forward bool) bool {
	// Go up to the first internal node that has another child that way.
	c.stack = c.stack[:len(c.stack)-1]
	for len(c.stack) > 0 {
		f := &c.stack[len(c.stack)-1]
		if forward && f.idx+1 < len(f.n.entries) {
			f.idx++
			c.descend(childAt(f.n, f.idx), true)
			return true
		}
		if !forward && f.idx >= 0 {
			f.idx--
			c.descend(childAt(f.n, f.idx), false)
			return true
		}
		c.stack = c.stack[:len(c.stack)-1]
	}
	return false
}
