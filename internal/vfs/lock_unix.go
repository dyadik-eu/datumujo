//go:build unix

package vfs

import (
	"errors"
	"os"
	"syscall"
)

// Lock takes flock(2) with LOCK_EX|LOCK_NB on the named file. flock
// belongs to the open file, so a second Lock in the same process fails
// too, and the kernel releases it when the process ends.
func (o OS) Lock(name string) (func() error, error) {
	f, err := o.Open(name)
	if err != nil {
		return nil, err
	}
	fd := f.(osFile).File
	if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fd.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, &os.PathError{Op: "flock", Path: name, Err: err}
	}
	return fd.Close, nil
}
