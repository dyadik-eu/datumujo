//go:build !unix

package vfs

import "errors"

// Lock is not built for this system; the supported targets (S-3) are all
// unix.
func (OS) Lock(name string) (func() error, error) {
	return nil, errors.New("vfs: file locks are not supported on this system")
}
