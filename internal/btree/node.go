// Package btree is a B+tree of byte keys and byte values over the pages
// of a store. Keys are ordered by bytes.Compare. A value too long to sit
// in a leaf goes to a chain of overflow pages.
//
// The root page of a tree never changes: when the root splits, its
// content moves to two new pages and it becomes an internal node over
// them. So a caller keeps the root number once, in a root slot of the
// header.
package btree

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/dyadik-eu/datumujo/internal/page"
)

// Node kinds, the first byte of a node page.
const (
	kindLeaf     = 1
	kindInternal = 2
)

// overflowRef is the size of an overflow reference in a leaf: the value
// length and the first page, at most 10 + 8 bytes.
const overflowRef = binary.MaxVarintLen64 + 8

// Limits holds the sizes that follow from the page size.
type Limits struct {
	Payload int // bytes of content in a page
	MaxCell int // bytes of one entry, at most a quarter of a node
	MaxKey  int // bytes of one key
}

// LimitsFor returns the limits for pages of pageSize bytes. An entry is
// at most a quarter of a node, so a node that grows past the page by one
// entry splits into two that each fit.
func LimitsFor(pageSize int) Limits {
	payload := pageSize - page.TrailerSize
	maxCell := (payload - 3) / 4
	// A leaf entry with an overflow value: key length, key, kind byte,
	// overflow reference. An internal entry, key and child, is smaller.
	return Limits{Payload: payload, MaxCell: maxCell, MaxKey: maxCell - binary.MaxVarintLen16 - 1 - overflowRef}
}

// entry is one key with its value in a leaf, or one key with the child
// to its right in an internal node.
type entry struct {
	key []byte
	// Leaf: inline value, or an overflow chain.
	value    []byte
	overflow bool
	length   uint64 // total value length if overflow
	first    uint64 // first overflow page
	// Internal: the child with keys from this key up to the next.
	child uint64
}

// node is a decoded node page.
type node struct {
	leaf    bool
	first   uint64 // internal: the child with keys below the first key
	entries []entry
}

// ErrDamaged matches a node page whose content does not decode. The page
// checksum held, so the content was written like this: a bug, or a page
// that is not a node.
var ErrDamaged = errors.New("btree: node is damaged")

type damaged struct {
	page   uint64
	reason string
}

func (d *damaged) Error() string        { return fmt.Sprintf("btree: page %d: %s", d.page, d.reason) }
func (d *damaged) Is(target error) bool { return target == ErrDamaged }

func (n *node) cellSize(e entry) int {
	size := varintLen(uint64(len(e.key))) + len(e.key)
	switch {
	case !n.leaf:
		size += 8
	case e.overflow:
		size += 1 + varintLen(e.length) + 8
	default:
		size += 1 + varintLen(uint64(len(e.value))) + len(e.value)
	}
	return size
}

// size is the encoded size of the node.
func (n *node) size() int {
	s := 3
	if !n.leaf {
		s += 8
	}
	for _, e := range n.entries {
		s += n.cellSize(e)
	}
	return s
}

func varintLen(v uint64) int {
	var b [binary.MaxVarintLen64]byte
	return binary.PutUvarint(b[:], v)
}

// encode writes the node into a payload of the given size. The node must
// fit.
func (n *node) encode(payload int) []byte {
	p := make([]byte, 0, payload)
	if n.leaf {
		p = append(p, kindLeaf)
	} else {
		p = append(p, kindInternal)
	}
	p = binary.BigEndian.AppendUint16(p, uint16(len(n.entries)))
	if !n.leaf {
		p = binary.BigEndian.AppendUint64(p, n.first)
	}
	for _, e := range n.entries {
		p = binary.AppendUvarint(p, uint64(len(e.key)))
		p = append(p, e.key...)
		switch {
		case !n.leaf:
			p = binary.BigEndian.AppendUint64(p, e.child)
		case e.overflow:
			p = append(p, 1)
			p = binary.AppendUvarint(p, e.length)
			p = binary.BigEndian.AppendUint64(p, e.first)
		default:
			p = append(p, 0)
			p = binary.AppendUvarint(p, uint64(len(e.value)))
			p = append(p, e.value...)
		}
	}
	return p
}

// decode reads the node on page no from its payload. It checks every
// length against the page, so damaged content gives an error and no
// panic.
func decode(no uint64, p []byte) (*node, error) {
	bad := func(format string, a ...any) error { return &damaged{no, fmt.Sprintf(format, a...)} }
	if len(p) < 3 {
		return nil, bad("shorter than a node header")
	}
	n := &node{}
	switch p[0] {
	case kindLeaf:
		n.leaf = true
	case kindInternal:
	default:
		return nil, bad("kind %d is not a node", p[0])
	}
	count := int(binary.BigEndian.Uint16(p[1:]))
	off := 3
	if !n.leaf {
		if len(p) < off+8 {
			return nil, bad("internal node without its first child")
		}
		n.first = binary.BigEndian.Uint64(p[off:])
		off += 8
	}
	uvarint := func() (uint64, error) {
		v, k := binary.Uvarint(p[off:])
		if k <= 0 {
			return 0, bad("length at byte %d does not decode", off)
		}
		// encode writes the shortest form. A longer one was not written
		// by this code (found by FuzzDecode: 72 as "\xc8\x00").
		if k != varintLen(v) {
			return 0, bad("length at byte %d is not in its shortest form", off)
		}
		off += k
		return v, nil
	}
	take := func(l uint64) ([]byte, error) {
		if l > uint64(len(p)-off) {
			return nil, bad("%d bytes at byte %d run past the page", l, off)
		}
		b := p[off : off+int(l)]
		off += int(l)
		return b, nil
	}
	n.entries = make([]entry, 0, count)
	for i := 0; i < count; i++ {
		kl, err := uvarint()
		if err != nil {
			return nil, err
		}
		key, err := take(kl)
		if err != nil {
			return nil, err
		}
		e := entry{key: append([]byte(nil), key...)}
		if !n.leaf {
			b, err := take(8)
			if err != nil {
				return nil, err
			}
			e.child = binary.BigEndian.Uint64(b)
		} else {
			kind, err := take(1)
			if err != nil {
				return nil, err
			}
			switch kind[0] {
			case 0:
				vl, err := uvarint()
				if err != nil {
					return nil, err
				}
				v, err := take(vl)
				if err != nil {
					return nil, err
				}
				e.value = append([]byte(nil), v...)
			case 1:
				e.overflow = true
				if e.length, err = uvarint(); err != nil {
					return nil, err
				}
				b, err := take(8)
				if err != nil {
					return nil, err
				}
				e.first = binary.BigEndian.Uint64(b)
			default:
				return nil, bad("value kind %d", kind[0])
			}
		}
		n.entries = append(n.entries, e)
	}
	return n, nil
}
