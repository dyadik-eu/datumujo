package vfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// OS is the file system of the operating system.
type OS struct{}

type osFile struct{ *os.File }

func (f osFile) Size() (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Open opens or creates the file. When it creates the file, it also calls
// fsync on the directory: without that, a new file can vanish in a power
// loss even after fsync on the file itself.
func (OS) Open(name string) (File, error) {
	_, err := os.Stat(name)
	created := errors.Is(err, fs.ErrNotExist)
	if err != nil && !created {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if created {
		if err := syncDir(filepath.Dir(name)); err != nil {
			f.Close()
			return nil, err
		}
	}
	return osFile{f}, nil
}

// Remove removes the file and calls fsync on the directory.
func (OS) Remove(name string) error {
	if err := os.Remove(name); err != nil {
		return err
	}
	return syncDir(filepath.Dir(name))
}

// Exists reports whether the file exists. An error other than "does not
// exist" is returned, not reported as false.
func (OS) Exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
