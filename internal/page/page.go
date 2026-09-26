// Package page defines the pages of a database file: the checksum trailer
// every page carries, and the header on page 0.
//
// A page of size n holds n-4 bytes of content and ends with a CRC-32C
// (Castagnoli) over the content and the page number. The page number is
// not stored; it only enters the checksum. So a page that stands at the
// wrong place in the file fails the check like a damaged one (I-1).
package page

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// TrailerSize is the number of bytes at the end of a page that hold its
// checksum.
const TrailerSize = 4

// Page sizes a file can have: a power of two in this range.
const (
	MinSize = 512
	MaxSize = 65536
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// DamagedError reports a page that fails its check. The read returns no
// data with it.
type DamagedError struct {
	Page   uint64
	Reason string
}

func (e *DamagedError) Error() string {
	return fmt.Sprintf("page %d is damaged: %s", e.Page, e.Reason)
}

// ErrDamaged matches every DamagedError with errors.Is.
var ErrDamaged = errors.New("page is damaged")

func (e *DamagedError) Is(target error) bool { return target == ErrDamaged }

// ValidSize reports whether n is a page size a file can have.
func ValidSize(n int) bool {
	return n >= MinSize && n <= MaxSize && n&(n-1) == 0
}

func sum(p []byte, pgno uint64) uint32 {
	var no [8]byte
	binary.BigEndian.PutUint64(no[:], pgno)
	c := crc32.Update(0, castagnoli, p[:len(p)-TrailerSize])
	return crc32.Update(c, castagnoli, no[:])
}

// Seal writes the checksum of page pgno into the trailer of p.
func Seal(p []byte, pgno uint64) {
	binary.BigEndian.PutUint32(p[len(p)-TrailerSize:], sum(p, pgno))
}

// Check reports whether p is page pgno as it was sealed.
func Check(p []byte, pgno uint64) error {
	if len(p) < MinSize {
		return &DamagedError{pgno, fmt.Sprintf("%d bytes, shorter than any page", len(p))}
	}
	if binary.BigEndian.Uint32(p[len(p)-TrailerSize:]) != sum(p, pgno) {
		return &DamagedError{pgno, "checksum does not match"}
	}
	return nil
}

// Read reads page pgno of size len(p) from f into p and checks it. A page
// that the file does not hold in full is damaged too: the file ends
// inside it.
func Read(f vfs.File, pgno uint64, p []byte) error {
	n, err := f.ReadAt(p, int64(pgno)*int64(len(p)))
	if n < len(p) {
		if err == nil || errors.Is(err, io.EOF) {
			return &DamagedError{pgno, fmt.Sprintf("the file ends after %d of its %d bytes", n, len(p))}
		}
		return fmt.Errorf("page %d: %w", pgno, err)
	}
	return Check(p, pgno)
}

// Write seals p as page pgno and writes it to f.
func Write(f vfs.File, pgno uint64, p []byte) error {
	Seal(p, pgno)
	if _, err := f.WriteAt(p, int64(pgno)*int64(len(p))); err != nil {
		return fmt.Errorf("page %d: %w", pgno, err)
	}
	return nil
}
