// Package backup copies a database while a program writes to it
// (requirement O-1) and restores a database from a copy (O-2).
//
// A copy is one snapshot: every page as of one commit, written to a new
// file without a log. The check command must accept the copy before it
// gets its name, so a copy under the target name is always whole and
// sound.
package backup

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dyadik-eu/datumujo/internal/check"
	"github.com/dyadik-eu/datumujo/internal/page"
	"github.com/dyadik-eu/datumujo/internal/store"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

// ErrExists is returned when the target or its log exists. A copy never
// replaces a database, and a log next to it would be applied to it.
var ErrExists = errors.New("backup: the target exists")

// ErrCheckFailed is returned when the check does not accept the copy.
var ErrCheckFailed = errors.New("backup: the check does not accept the copy")

// Backup writes the database of s, as one snapshot sees it, to dest. The
// program can go on writing meanwhile. The snapshot stops checkpoints
// until the copy is done.
func Backup(s *store.Store, fs vfs.FS, dest string) error {
	snap, err := s.Snapshot()
	if err != nil {
		return err
	}
	defer snap.Close()
	return copyTo(fs, snap, s.PageSize(), dest)
}

// Restore makes the database to from from: a copy that Backup wrote, or
// any database that is not open. It opens from read-only.
func Restore(fs vfs.FS, from, to string) error {
	if from == to {
		return fmt.Errorf("backup: %s is both source and target", from)
	}
	s, err := store.Open(fs, from, store.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer s.Close()
	return Backup(s, fs, to)
}

// copyTo writes the snapshot to dest-new, checks it, and renames it to
// dest.
func copyTo(fs vfs.FS, snap *store.Snapshot, pageSize int, dest string) (err error) {
	unlock, err := fs.Lock(dest + "-lock")
	if err != nil {
		return fmt.Errorf("backup: %s: %w", dest, err)
	}
	defer unlock()
	for _, name := range []string{dest, dest + "-log"} {
		if ok, err := fs.Exists(name); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("%w: %s", ErrExists, name)
		}
	}
	tmp := dest + "-new"
	for _, name := range []string{tmp, tmp + "-log"} {
		if ok, err := fs.Exists(name); err != nil {
			return err
		} else if ok {
			// Left by a copy that did not finish.
			if err := fs.Remove(name); err != nil {
				return err
			}
		}
	}
	defer func() {
		if err != nil {
			if ok, _ := fs.Exists(tmp); ok {
				err = errors.Join(err, fs.Remove(tmp))
			}
		}
	}()
	if err := write(fs, snap, pageSize, tmp); err != nil {
		return err
	}
	r, err := check.Run(fs, tmp)
	if rmErr := removeIfExists(fs, tmp+"-lock"); err == nil {
		err = rmErr
	}
	if err != nil {
		return err
	}
	if len(r.Findings) > 0 {
		var what []string
		for _, f := range r.Findings {
			what = append(what, f.String())
		}
		return fmt.Errorf("%w: %s", ErrCheckFailed, strings.Join(what, "; "))
	}
	return fs.Rename(tmp, dest)
}

func removeIfExists(fs vfs.FS, name string) error {
	if ok, err := fs.Exists(name); err != nil || !ok {
		return err
	}
	return fs.Remove(name)
}

// write writes the header and every page of the snapshot to name and
// syncs it. A page that fails its checksum stops the copy.
func write(fs vfs.FS, snap *store.Snapshot, pageSize int, name string) error {
	f, err := fs.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := snap.Header()
	if _, err := f.WriteAt(page.EncodeHeader(hdr), 0); err != nil {
		return err
	}
	buf := make([]byte, pageSize)
	for no := uint64(1); no < hdr.PageCount; no++ {
		if err := snap.Read(no, buf); err != nil {
			return fmt.Errorf("backup: page %d: %w", no, err)
		}
		if _, err := f.WriteAt(buf, int64(no)*int64(pageSize)); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
