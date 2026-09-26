// Package vfs is the only way the engine reaches files. It has two
// implementations: the operating system (OS) and a simulated disk (Sim)
// that can lose power. The crash test of requirement P-1 runs the engine
// on Sim.
package vfs

import "errors"

// File is an open file. The calls are those of os.File that the engine
// needs, and nothing else.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	// Sync makes all writes and truncations before it durable.
	Sync() error
	Truncate(size int64) error
	Size() (int64, error)
	Close() error
}

// FS opens and removes files by name.
type FS interface {
	// Open opens the named file for reading and writing and creates it
	// if it does not exist. A file it creates survives a power loss once
	// Open returns, even while it is still empty.
	Open(name string) (File, error)
	// Remove removes the named file. The removal survives a power loss
	// once Remove returns.
	Remove(name string) error
	// Exists reports whether the named file exists.
	Exists(name string) (bool, error)
	// Rename gives the file oldName the name newName, and replaces a file
	// of that name. The step is atomic and survives a power loss once
	// Rename returns: after it, newName is either the old file or the
	// renamed one, never a mix.
	Rename(oldName, newName string) error
}

// ErrCrashed is returned by every call on a Sim after its call budget is
// used up: the simulated process has stopped.
var ErrCrashed = errors.New("vfs: simulated crash")
