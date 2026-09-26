package page

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// Version is the format version this code writes. A file with another
// version is rejected, not read on a best-effort basis (I-4).
const Version = 1

// Magic starts every database file.
var Magic = [8]byte{'d', 'a', 't', 'u', 'm', 'u', 'j', 'o'}

// The header is page 0. Its fields, big-endian, after the magic:
//
//	offset  8  uint32  format version
//	offset 12  uint32  page size
//	offset 16  uint64  page count, the header included
//	offset 24  uint64  first free page, 0 if none
//	offset 32  uint64  number of free pages
//	offset 40  uint64  root 0 to root 3, one after the other: page
//	                   numbers the layers above keep here, 0 if unused
const headerFields = 72

// Roots is the number of root slots in the header.
const Roots = 4

// Header is the content of page 0.
type Header struct {
	Version   uint32
	PageSize  int
	PageCount uint64
	FreeHead  uint64
	FreeCount uint64
	Root      [Roots]uint64
}

// ErrNotDatabase is returned for a file that does not start with Magic.
var ErrNotDatabase = errors.New("not a datumujo file")

// VersionError is returned for a file of another format version.
type VersionError struct{ Version uint32 }

func (e *VersionError) Error() string {
	return fmt.Sprintf("format version %d is not supported; this code reads version %d", e.Version, Version)
}

// EncodeHeader returns page 0 for h, sealed. h.PageSize must be valid.
func EncodeHeader(h Header) []byte {
	p := make([]byte, h.PageSize)
	copy(p, Magic[:])
	binary.BigEndian.PutUint32(p[8:], h.Version)
	binary.BigEndian.PutUint32(p[12:], uint32(h.PageSize))
	binary.BigEndian.PutUint64(p[16:], h.PageCount)
	binary.BigEndian.PutUint64(p[24:], h.FreeHead)
	binary.BigEndian.PutUint64(p[32:], h.FreeCount)
	for i, r := range h.Root {
		binary.BigEndian.PutUint64(p[40+8*i:], r)
	}
	Seal(p, 0)
	return p
}

// DecodeHeader reads the header from the start of p, which must hold the
// whole page 0. It checks the magic, the version, the page size and the
// checksum, in this order: a file of another program or version gets that
// error, not a checksum error.
func DecodeHeader(p []byte) (Header, error) {
	if len(p) < headerFields || !bytes.Equal(p[:8], Magic[:]) {
		return Header{}, ErrNotDatabase
	}
	h := Header{
		Version:   binary.BigEndian.Uint32(p[8:]),
		PageSize:  int(binary.BigEndian.Uint32(p[12:])),
		PageCount: binary.BigEndian.Uint64(p[16:]),
		FreeHead:  binary.BigEndian.Uint64(p[24:]),
		FreeCount: binary.BigEndian.Uint64(p[32:]),
	}
	for i := range h.Root {
		h.Root[i] = binary.BigEndian.Uint64(p[40+8*i:])
	}
	if h.Version != Version {
		return Header{}, &VersionError{h.Version}
	}
	if !ValidSize(h.PageSize) {
		return Header{}, &DamagedError{0, fmt.Sprintf("page size %d is not a power of two from %d to %d", h.PageSize, MinSize, MaxSize)}
	}
	if len(p) < h.PageSize {
		return Header{}, &DamagedError{0, fmt.Sprintf("%d bytes, the header says the page has %d", len(p), h.PageSize)}
	}
	if err := Check(p[:h.PageSize], 0); err != nil {
		return Header{}, err
	}
	if h.PageCount == 0 {
		return Header{}, &DamagedError{0, "page count 0, but the header itself is a page"}
	}
	// Every page number in the header points into the file, and the
	// free pages are fewer than the pages. Page 0 is never free.
	if h.FreeHead >= h.PageCount || h.FreeCount >= h.PageCount || (h.FreeHead == 0) != (h.FreeCount == 0) {
		return Header{}, &DamagedError{0, fmt.Sprintf("free list head %d, count %d, in %d pages", h.FreeHead, h.FreeCount, h.PageCount)}
	}
	for i, r := range h.Root {
		if r >= h.PageCount {
			return Header{}, &DamagedError{0, fmt.Sprintf("root %d is page %d, past the end of %d pages", i, r, h.PageCount)}
		}
	}
	return h, nil
}

// ReadHeader reads and decodes page 0 of f. It reads the fields first to
// learn the page size, then the whole page.
func ReadHeader(f vfs.File) (Header, error) {
	first := make([]byte, headerFields)
	n, err := f.ReadAt(first, 0)
	if n < headerFields {
		if err == nil || errors.Is(err, io.EOF) {
			if n < 8 || !bytes.Equal(first[:8], Magic[:]) {
				return Header{}, ErrNotDatabase
			}
			return Header{}, &DamagedError{0, fmt.Sprintf("the file ends after %d bytes, inside the header", n)}
		}
		return Header{}, fmt.Errorf("page 0: %w", err)
	}
	size := int(binary.BigEndian.Uint32(first[12:]))
	if !bytes.Equal(first[:8], Magic[:]) || !ValidSize(size) {
		return DecodeHeader(first) // the error for the magic, version or size
	}
	p := make([]byte, size)
	if err := Read(f, 0, p); err != nil {
		if v := binary.BigEndian.Uint32(first[8:]); v != Version {
			return Header{}, &VersionError{v}
		}
		return Header{}, err
	}
	return DecodeHeader(p)
}
