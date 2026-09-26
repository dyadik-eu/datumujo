// Package wal is the log of page images that every commit appends to
// (design.md, "Writes go to a log of page images").
//
// The log file starts with a header, then frames. A frame is a frame
// header and one page image. The last frame of a commit carries the page
// count of the database after the commit; the frames before it carry 0.
// Each frame header holds a checksum over the frame, chained to the
// checksum of the frame before; the chain starts from the salt in the log
// header. Open reads the frames in order and uses them up to the last
// commit frame whose chain holds. The frames after it are from a commit
// that did not finish, or damaged; Open reports how many bytes it left.
//
// A new generation of the log gets a new salt, so frames of an earlier
// generation that are still in the file do not chain.
package wal

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// Version is the format version of the log.
const Version = 1

// Magic starts every log file.
var Magic = [8]byte{'d', 'a', 't', 'u', 'l', 'o', 'g', 0}

// The log header, big-endian:
//
//	offset  0  [8]byte  magic
//	offset  8  uint32   format version
//	offset 12  uint32   page size
//	offset 16  uint64   salt
//	offset 24  uint32   CRC-32C of bytes 0 to 23
const headerSize = 28

// A frame header, big-endian, then the page image:
//
//	offset  0  uint64  page number
//	offset  8  uint64  page count after the commit, 0 if not the last frame
//	offset 16  uint32  chained checksum
const frameHeaderSize = 20

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Page is one page image to append.
type Page struct {
	No   uint64
	Data []byte // exactly the page size, sealed with page.Seal
}

// Recovered tells what Open found in the log.
type Recovered struct {
	Commits int
	// Ignored is the number of bytes after the last complete commit: a
	// commit that did not finish, or damage. It cannot tell the two apart.
	Ignored int64
}

// Log is an open log.
type Log struct {
	f        vfs.File
	pageSize int
	salt     uint64
	chain    uint32
	end      int64 // offset after the last committed frame
	commits  int
	count    uint64 // page count after the last commit
	// latest holds the offset of the newest committed image of a page.
	latest map[uint64]int64
	// tail is set while bytes after end may still be in the file, left by
	// a commit that did not finish. The next commit cuts them off first:
	// a new commit shorter than them would leave frames behind it that
	// could, by a chance of 2^-32 each, continue the chain.
	tail bool
}

// ErrNotLog is returned for a file that does not start with Magic.
var ErrNotLog = errors.New("wal: not a log file")

// DamagedError reports a log header that fails its checks while frames
// follow it. Starting a new log then would drop commits.
type DamagedError struct{ Reason string }

func (e *DamagedError) Error() string { return "wal: the log is damaged: " + e.Reason }

// Open opens the log in f, a log of pages of pageSize bytes, and
// recovers the committed frames. An empty file, or a file that holds no
// more than a partial header, becomes a new log: a new log is synced with
// its header before the first commit, so such a file never held one.
func Open(f vfs.File, pageSize int) (*Log, Recovered, error) {
	l := &Log{f: f, pageSize: pageSize, latest: map[uint64]int64{}}
	size, err := f.Size()
	if err != nil {
		return nil, Recovered{}, err
	}
	hdr := make([]byte, headerSize)
	n, err := f.ReadAt(hdr, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, Recovered{}, err
	}
	if n < headerSize {
		return l, Recovered{}, l.reset()
	}
	if err := l.parseHeader(hdr); err != nil {
		if size > headerSize {
			return nil, Recovered{}, err
		}
		return l, Recovered{}, l.reset()
	}
	return l, l.recover(size), nil
}

func (l *Log) parseHeader(hdr []byte) error {
	if string(hdr[:8]) != string(Magic[:]) {
		return ErrNotLog
	}
	if crc32.Checksum(hdr[:24], castagnoli) != binary.BigEndian.Uint32(hdr[24:]) {
		return &DamagedError{"header checksum does not match"}
	}
	if v := binary.BigEndian.Uint32(hdr[8:]); v != Version {
		return fmt.Errorf("wal: format version %d is not supported; this code reads version %d", v, Version)
	}
	if ps := int(binary.BigEndian.Uint32(hdr[12:])); ps != l.pageSize {
		return &DamagedError{fmt.Sprintf("page size %d, the database has %d", ps, l.pageSize)}
	}
	l.salt = binary.BigEndian.Uint64(hdr[16:])
	l.chain = uint32(l.salt) ^ uint32(l.salt>>32)
	return nil
}

// recover reads frames and keeps those up to the last commit frame whose
// chain holds.
func (l *Log) recover(size int64) Recovered {
	l.end = headerSize
	frame := make([]byte, frameHeaderSize+l.pageSize)
	pending := map[uint64]int64{}
	chain := l.chain
	for off := int64(headerSize); off+int64(len(frame)) <= size; off += int64(len(frame)) {
		if n, _ := l.f.ReadAt(frame, off); n < len(frame) {
			break
		}
		next := frameSum(chain, frame)
		if binary.BigEndian.Uint32(frame[16:]) != next {
			break
		}
		chain = next
		pending[binary.BigEndian.Uint64(frame[0:])] = off
		if count := binary.BigEndian.Uint64(frame[8:]); count != 0 {
			for no, o := range pending {
				l.latest[no] = o
			}
			pending = map[uint64]int64{}
			l.chain, l.end, l.count = chain, off+int64(len(frame)), count
			l.commits++
		}
	}
	l.tail = size > l.end
	return Recovered{Commits: l.commits, Ignored: size - l.end}
}

// frameSum is the checksum of a frame, chained to prev. It covers the
// page number, the page count and the page image.
func frameSum(prev uint32, frame []byte) uint32 {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], prev)
	c := crc32.Update(0, castagnoli, p[:])
	c = crc32.Update(c, castagnoli, frame[:16])
	return crc32.Update(c, castagnoli, frame[frameHeaderSize:])
}

// reset starts a new generation: an empty log with a new salt, synced.
func (l *Log) reset() error {
	var salt [8]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return err
	}
	hdr := make([]byte, headerSize)
	copy(hdr, Magic[:])
	binary.BigEndian.PutUint32(hdr[8:], Version)
	binary.BigEndian.PutUint32(hdr[12:], uint32(l.pageSize))
	copy(hdr[16:], salt[:])
	binary.BigEndian.PutUint32(hdr[24:], crc32.Checksum(hdr[:24], castagnoli))
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.WriteAt(hdr, 0); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.salt = binary.BigEndian.Uint64(salt[:])
	l.chain = uint32(l.salt) ^ uint32(l.salt>>32)
	l.end, l.commits, l.count = headerSize, 0, 0
	l.latest = map[uint64]int64{}
	return nil
}

// Commit appends the pages as one commit, with count as the page count of
// the database after it, and syncs the log. When Commit returns nil, the
// commit is durable (T-3). Each page must be sealed; a page number must
// not repeat within one commit.
func (l *Log) Commit(pages []Page, count uint64) error {
	if len(pages) == 0 {
		return errors.New("wal: a commit needs at least one page")
	}
	if count == 0 {
		return errors.New("wal: page count 0; the header page alone makes 1")
	}
	seen := map[uint64]bool{}
	for _, p := range pages {
		if len(p.Data) != l.pageSize {
			return fmt.Errorf("wal: page %d has %d bytes, not %d", p.No, len(p.Data), l.pageSize)
		}
		if err := page.Check(p.Data, p.No); err != nil {
			return fmt.Errorf("wal: refusing to log it: %w", err)
		}
		if seen[p.No] {
			return fmt.Errorf("wal: page %d twice in one commit", p.No)
		}
		seen[p.No] = true
	}
	buf := make([]byte, 0, len(pages)*(frameHeaderSize+l.pageSize))
	chain := l.chain
	offsets := make(map[uint64]int64, len(pages))
	for i, p := range pages {
		frame := make([]byte, frameHeaderSize+l.pageSize)
		binary.BigEndian.PutUint64(frame[0:], p.No)
		if i == len(pages)-1 {
			binary.BigEndian.PutUint64(frame[8:], count)
		}
		copy(frame[frameHeaderSize:], p.Data)
		chain = frameSum(chain, frame)
		binary.BigEndian.PutUint32(frame[16:], chain)
		offsets[p.No] = l.end + int64(len(buf))
		buf = append(buf, frame...)
	}
	if l.tail {
		if err := l.f.Truncate(l.end); err != nil {
			return err
		}
	}
	// From here on, a failure leaves bytes after end.
	l.tail = true
	if _, err := l.f.WriteAt(buf, l.end); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.tail = false
	for no, off := range offsets {
		l.latest[no] = off
	}
	l.chain, l.end, l.count = chain, l.end+int64(len(buf)), count
	l.commits++
	return nil
}

// Read reads the newest committed image of page no into p. It reports
// false if the log holds no image of it. The image is checked as a page.
func (l *Log) Read(no uint64, p []byte) (bool, error) {
	off, ok := l.latest[no]
	if !ok {
		return false, nil
	}
	if _, err := l.f.ReadAt(p, off+frameHeaderSize); err != nil {
		return true, fmt.Errorf("wal: page %d: %w", no, err)
	}
	return true, page.Check(p, no)
}

// Count returns the page count of the database after the last commit, and
// false if the log holds no commit.
func (l *Log) Count() (uint64, bool) { return l.count, l.commits > 0 }

// Commits returns the number of commits in the log.
func (l *Log) Commits() int { return l.commits }
