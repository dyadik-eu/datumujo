package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Stats is what Check found in a tree.
type Stats struct {
	Entries  int
	Depth    int      // levels, 1 for a lone leaf
	Pages    []uint64 // every page of the tree, overflow pages included
	Overflow int      // overflow pages
}

// Check walks the whole tree and checks it: every node decodes and fits,
// keys rise within each node and stay within the bounds its parent gives,
// every leaf is at the same depth, no page is used twice, and every
// overflow chain holds exactly its length. It returns the first fault it
// finds, or the stats of a sound tree.
func (t *Tree) Check(r Pages) (Stats, error) {
	var st Stats
	seen := map[uint64]bool{}
	leafDepth := -1
	use := func(no uint64) error {
		if no == 0 {
			return &damaged{no, "the tree points to page 0, the header"}
		}
		if seen[no] {
			return &damaged{no, "used twice in the tree"}
		}
		seen[no] = true
		st.Pages = append(st.Pages, no)
		return nil
	}
	var walk func(no uint64, lo, hi []byte, depth int) error
	walk = func(no uint64, lo, hi []byte, depth int) error {
		if depth > maxDepth {
			return &damaged{no, "the tree is deeper than any tree can be"}
		}
		if err := use(no); err != nil {
			return err
		}
		n, err := t.read(r, no)
		if err != nil {
			return err
		}
		var prev []byte
		for i, e := range n.entries {
			if len(e.key) == 0 || len(e.key) > t.lim.MaxKey {
				return &damaged{no, fmt.Sprintf("entry %d has a key of %d bytes", i, len(e.key))}
			}
			if prev != nil && bytes.Compare(prev, e.key) >= 0 {
				return &damaged{no, fmt.Sprintf("entry %d is not above the one before", i)}
			}
			if lo != nil && bytes.Compare(e.key, lo) < 0 || hi != nil && bytes.Compare(e.key, hi) >= 0 {
				return &damaged{no, fmt.Sprintf("entry %d is outside the range its parent gives", i)}
			}
			prev = e.key
		}
		if n.leaf {
			if leafDepth < 0 {
				leafDepth = depth
			} else if depth != leafDepth {
				return &damaged{no, fmt.Sprintf("leaf at depth %d, others at %d", depth, leafDepth)}
			}
			for _, e := range n.entries {
				st.Entries++
				if !e.overflow {
					continue
				}
				if err := t.checkOverflow(r, e, use, &st); err != nil {
					return err
				}
			}
			return nil
		}
		if len(n.entries) > 0 {
			if err := walk(n.first, lo, n.entries[0].key, depth+1); err != nil {
				return err
			}
		} else if err := walk(n.first, lo, hi, depth+1); err != nil {
			return err
		}
		for i, e := range n.entries {
			upper := hi
			if i+1 < len(n.entries) {
				upper = n.entries[i+1].key
			}
			if err := walk(e.child, e.key, upper, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(t.Root, nil, nil, 1); err != nil {
		return st, err
	}
	st.Depth = leafDepth
	return st, nil
}

func (t *Tree) checkOverflow(r Pages, e entry, use func(uint64) error, st *Stats) error {
	chunk := uint64(t.lim.Payload - 8)
	want := (e.length + chunk - 1) / chunk
	// The same rule as leafEntry: a value goes to overflow pages when the
	// whole entry, key included, would not fit in a quarter of a node.
	inline := varintLen(uint64(len(e.key))) + len(e.key) + 1 + varintLen(e.length) + int(e.length)
	if e.length < uint64(t.lim.Payload) && inline <= t.lim.MaxCell {
		return &damaged{e.first, fmt.Sprintf("an overflow value of %d bytes would fit in the leaf", e.length)}
	}
	buf := make([]byte, t.pageSize)
	no := e.first
	for i := uint64(0); i < want; i++ {
		if err := use(no); err != nil {
			return err
		}
		if err := r.Read(no, buf); err != nil {
			return err
		}
		st.Overflow++
		no = binary.BigEndian.Uint64(buf)
	}
	if no != 0 {
		return &damaged{no, fmt.Sprintf("an overflow chain goes on past its %d pages", want)}
	}
	return nil
}
