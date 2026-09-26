package table

import (
	"encoding/binary"
	"fmt"

	"github.com/dyadik-eu/datumujo/internal/btree"
)

// counterPrefix starts the key of a counter in the catalog tree. The
// schema key does not start with it.
const counterPrefix = "counter:"

// Counter returns the last value that Next gave for the counter, or 0.
func (v *View) Counter(name string) (uint64, error) {
	if err := checkName("counter", name); err != nil {
		return 0, err
	}
	root := v.r.Root(CatalogSlot)
	if root == 0 {
		return 0, nil
	}
	return readCounter(btree.Open(root, v.pageSize), v.r, name)
}

func readCounter(cat *btree.Tree, r btree.Pages, name string) (uint64, error) {
	b, ok, err := cat.Get(r, []byte(counterPrefix+name))
	if err != nil || !ok {
		return 0, err
	}
	d := decoder{b: b, what: "counter " + name}
	n := d.uvarint()
	if d.err == nil && n == 0 {
		d.fail("value 0 is never stored")
	}
	return n, d.end()
}

// Next increments the counter and returns its new value. The first value
// is 1. A committed value is never given again, also not after a crash. A
// value given in a transaction that rolls back is given again.
func (tx *Tx) Next(name string) (uint64, error) {
	if err := checkName("counter", name); err != nil {
		return 0, err
	}
	cat, err := tx.catalog()
	if err != nil {
		return 0, err
	}
	n, err := readCounter(cat, tx.w, name)
	if err != nil {
		return 0, err
	}
	if n == ^uint64(0) {
		return 0, fmt.Errorf("table: counter %s is at its largest value", name)
	}
	n++
	return n, cat.Put(tx.w, []byte(counterPrefix+name), binary.AppendUvarint(nil, n))
}
