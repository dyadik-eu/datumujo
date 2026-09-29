package page

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"

	"github.com/dyadik-eu/datumujo/internal/vfs"
)

func sealed(size int, pgno uint64, seed int64) []byte {
	p := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(p[:size-TrailerSize])
	Seal(p, pgno)
	return p
}

// damagedPage reports whether err is a DamagedError that names pgno.
func damagedPage(err error, pgno uint64) bool {
	var d *DamagedError
	return errors.As(err, &d) && d.Page == pgno && errors.Is(err, ErrDamaged)
}

func TestSealThenCheck(t *testing.T) {
	for _, size := range []int{MinSize, 4096, MaxSize} {
		p := sealed(size, 7, int64(size))
		if err := Check(p, 7); err != nil {
			t.Errorf("size %d: %v", size, err)
		}
	}
}

// TestEveryBitFlipIsFound: flipping any single bit of a page, the trailer
// included, fails the check with an error that names the page (I-1).
func TestEveryBitFlipIsFound(t *testing.T) {
	for _, size := range []int{MinSize, 4096} {
		p := sealed(size, 3, 1)
		for bit := 0; bit < size*8; bit++ {
			p[bit/8] ^= 1 << (bit % 8)
			if err := Check(p, 3); !damagedPage(err, 3) {
				t.Fatalf("size %d, bit %d flipped: %v", size, bit, err)
			}
			p[bit/8] ^= 1 << (bit % 8)
		}
		if err := Check(p, 3); err != nil {
			t.Fatalf("size %d: page restored, still %v", size, err)
		}
	}
}

// TestPageAtWrongPlace: a page sealed as page 5 fails as page 6, although
// its bytes are intact.
func TestPageAtWrongPlace(t *testing.T) {
	p := sealed(4096, 5, 2)
	if err := Check(p, 6); !damagedPage(err, 6) {
		t.Errorf("page 5 checked as 6: %v", err)
	}
}

func TestReadAndWrite(t *testing.T) {
	s := vfs.NewSim()
	f, _ := s.Open("db")
	p := sealed(1024, 0, 3)
	want := append([]byte(nil), p...)
	if err := Write(f, 2, p); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 1024)
	if err := Read(f, 2, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:1024-TrailerSize], want[:1024-TrailerSize]) {
		t.Error("content differs after write and read")
	}
	// Page 3 lies past the end, page 1 is zeros: both are damaged, and
	// the error names the page.
	if err := Read(f, 3, got); !damagedPage(err, 3) {
		t.Errorf("page past the end: %v", err)
	}
	if err := Read(f, 1, got); !damagedPage(err, 1) {
		t.Errorf("page of zeros: %v", err)
	}
	// A page the file holds only in part.
	f.Truncate(2*1024 + 100)
	if err := Read(f, 2, got); !damagedPage(err, 2) {
		t.Errorf("page cut short: %v", err)
	}
}

// TestReadPassesOnFileErrors: an error of the file itself is no damaged
// page. The caller must see the difference: one is a finding, the other
// means the page could not be checked.
func TestReadPassesOnFileErrors(t *testing.T) {
	s := vfs.NewSim()
	f, _ := s.Open("db")
	Write(f, 0, make([]byte, 512))
	s.Crash(nil)
	err := Read(f, 0, make([]byte, 512))
	if !errors.Is(err, vfs.ErrCrashed) || errors.Is(err, ErrDamaged) {
		t.Errorf("read on a stopped disk: %v", err)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	for i, size := range []int{MinSize, 4096, MaxSize} {
		// Both versions this code reads.
		h := Header{Version: []uint32{MinVersion, Version, MinVersion}[i], PageSize: size, PageCount: 12345, FreeHead: 77, FreeCount: 3, Root: [Roots]uint64{5, 0, 900, 12344}}
		s := vfs.NewSim()
		f, _ := s.Open("db")
		f.WriteAt(EncodeHeader(h), 0)
		got, err := ReadHeader(f)
		if err != nil || got != h {
			t.Errorf("size %d: %+v, %v", size, got, err)
		}
	}
}

// headerFile writes p as the start of a file and reads the header back.
func headerFile(p []byte) error {
	s := vfs.NewSim()
	f, _ := s.Open("db")
	f.WriteAt(p, 0)
	_, err := ReadHeader(f)
	return err
}

// TestHeaderRejects: each wrong header gets its own error. Another
// program's file is not a damaged database, and a file of another format
// version is rejected before its checksum is looked at (I-4).
func TestHeaderRejects(t *testing.T) {
	good := EncodeHeader(Header{Version: Version, PageSize: 4096, PageCount: 1})
	edit := func(f func(p []byte)) []byte {
		p := append([]byte(nil), good...)
		f(p)
		return p
	}
	var verr *VersionError
	for _, c := range []struct {
		name string
		p    []byte
		ok   func(error) bool
	}{
		{"empty file", nil, func(err error) bool { return errors.Is(err, ErrNotDatabase) }},
		{"other magic", edit(func(p []byte) { p[0] = 'X' }), func(err error) bool { return errors.Is(err, ErrNotDatabase) }},
		{"magic only", good[:8], func(err error) bool { return damagedPage(err, 0) }},
		{"version 3", edit(func(p []byte) { binary.BigEndian.PutUint32(p[8:], 3) }), func(err error) bool {
			return errors.As(err, &verr) && verr.Version == 3
		}},
		{"version 3, resealed", edit(func(p []byte) { binary.BigEndian.PutUint32(p[8:], 3); Seal(p, 0) }), func(err error) bool {
			return errors.As(err, &verr) && verr.Version == 3
		}},
		{"version 0, resealed", edit(func(p []byte) { binary.BigEndian.PutUint32(p[8:], 0); Seal(p, 0) }), func(err error) bool {
			return errors.As(err, &verr) && verr.Version == 0
		}},
		{"page size 1000", edit(func(p []byte) { binary.BigEndian.PutUint32(p[12:], 1000) }), func(err error) bool { return damagedPage(err, 0) }},
		{"page size larger than the file", edit(func(p []byte) { binary.BigEndian.PutUint32(p[12:], 8192); Seal(p, 0) }), func(err error) bool { return damagedPage(err, 0) }},
		{"damaged count", edit(func(p []byte) { p[20] ^= 1 }), func(err error) bool { return damagedPage(err, 0) }},
		{"page count 0", edit(func(p []byte) { binary.BigEndian.PutUint64(p[16:], 0); Seal(p, 0) }), func(err error) bool { return damagedPage(err, 0) }},
		{"cut after the fields", good[:100], func(err error) bool { return damagedPage(err, 0) }},
		{"version 3, cut after the fields", edit(func(p []byte) { binary.BigEndian.PutUint32(p[8:], 3) })[:100], func(err error) bool {
			return errors.As(err, &verr) && verr.Version == 3
		}},
		{"version 1, cut after the fields", edit(func(p []byte) { binary.BigEndian.PutUint32(p[8:], 1) })[:100], func(err error) bool { return damagedPage(err, 0) }},
		{"free head past the end", edit(func(p []byte) {
			binary.BigEndian.PutUint64(p[24:], 1)
			binary.BigEndian.PutUint64(p[32:], 1)
			Seal(p, 0)
		}), func(err error) bool { return damagedPage(err, 0) }},
		{"free head without count", edit(func(p []byte) {
			binary.BigEndian.PutUint64(p[16:], 5)
			binary.BigEndian.PutUint64(p[24:], 2)
			Seal(p, 0)
		}), func(err error) bool { return damagedPage(err, 0) }},
		{"free count without head", edit(func(p []byte) {
			binary.BigEndian.PutUint64(p[16:], 5)
			binary.BigEndian.PutUint64(p[32:], 2)
			Seal(p, 0)
		}), func(err error) bool { return damagedPage(err, 0) }},
		{"free count as large as the file", edit(func(p []byte) {
			binary.BigEndian.PutUint64(p[16:], 5)
			binary.BigEndian.PutUint64(p[24:], 2)
			binary.BigEndian.PutUint64(p[32:], 5)
			Seal(p, 0)
		}), func(err error) bool { return damagedPage(err, 0) }},
		{"root past the end", edit(func(p []byte) { binary.BigEndian.PutUint64(p[40+8*3:], 1); Seal(p, 0) }), func(err error) bool { return damagedPage(err, 0) }},
	} {
		if err := headerFile(c.p); !c.ok(err) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// FuzzDecodeHeader: any bytes give a header or an error, never a panic.
// A header that decodes encodes back to the same bytes.
func FuzzDecodeHeader(f *testing.F) {
	f.Add(EncodeHeader(Header{Version: Version, PageSize: 512, PageCount: 3}))
	f.Add([]byte("datumujo"))
	f.Add(make([]byte, 600))
	f.Fuzz(func(t *testing.T, p []byte) {
		h, err := DecodeHeader(p)
		if err != nil {
			return
		}
		if !bytes.Equal(EncodeHeader(h), p[:h.PageSize]) {
			t.Fatalf("decoded %+v, encodes to other bytes", h)
		}
	})
}

// FuzzCheck: any bytes pass or fail the check without a panic. A page
// that passes is exactly what Seal writes for its content.
func FuzzCheck(f *testing.F) {
	f.Add(sealed(512, 1, 1), uint64(1))
	f.Add(make([]byte, 512), uint64(0))
	f.Add([]byte{1, 2, 3}, uint64(9))
	f.Fuzz(func(t *testing.T, p []byte, pgno uint64) {
		if Check(p, pgno) != nil {
			return
		}
		q := append([]byte(nil), p...)
		Seal(q, pgno)
		if !bytes.Equal(p, q) {
			t.Fatal("a page passes the check but differs from its sealed form")
		}
	})
}

func TestValidSize(t *testing.T) {
	for n, want := range map[int]bool{256: false, 512: true, 1000: false, 4096: true, 6144: false, 65536: true, 131072: false} {
		if ValidSize(n) != want {
			t.Errorf("ValidSize(%d) = %v", n, !want)
		}
	}
}

// TestHeaderRejectsOddSizeEvenIfSealed: a header for 1000-byte pages,
// sealed correctly over 1000 bytes, is still rejected. The size must be a
// power of two, not only match the checksum.
func TestHeaderRejectsOddSizeEvenIfSealed(t *testing.T) {
	p := make([]byte, 1000)
	copy(p, Magic[:])
	binary.BigEndian.PutUint32(p[8:], Version)
	binary.BigEndian.PutUint32(p[12:], 1000)
	binary.BigEndian.PutUint64(p[16:], 1)
	Seal(p, 0)
	if _, err := DecodeHeader(p); !damagedPage(err, 0) {
		t.Errorf("DecodeHeader: %v", err)
	}
	if err := headerFile(p); !damagedPage(err, 0) {
		t.Errorf("ReadHeader: %v", err)
	}
}

// TestDecodeHeaderShortBuffer: a buffer shorter than the page size the
// header names is an error, not a panic.
func TestDecodeHeaderShortBuffer(t *testing.T) {
	good := EncodeHeader(Header{Version: Version, PageSize: 4096, PageCount: 1})
	if _, err := DecodeHeader(good[:100]); !damagedPage(err, 0) {
		t.Errorf("DecodeHeader of 100 of 4096 bytes: %v", err)
	}
}

// TestShortForeignFile: a file shorter than the header fields that does
// not start with the magic is another program's file, not a damaged
// database.
func TestShortForeignFile(t *testing.T) {
	if err := headerFile([]byte("hello, world")); !errors.Is(err, ErrNotDatabase) {
		t.Errorf("12 foreign bytes: %v", err)
	}
}
