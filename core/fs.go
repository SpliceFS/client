// Package core is the SpliceFS client as a library: it connects to clusters, merges
// their directory trees, reads files from several nodes at once and caches blocks on
// local disk. It knows nothing about mounting. A mount layer, or any other program,
// drives it through the FS interface.
package core

import (
	"context"
	"errors"
	"time"
)

// FS is a read-only view of part of a cluster. Names are slash-separated paths
// relative to the root of the view; "" is the root. All methods are safe for
// concurrent use.
type FS interface {
	// Stat describes the file or directory at name.
	Stat(ctx context.Context, name string) (Attr, error)
	// ReadDir lists the directory at name, sorted by name.
	ReadDir(ctx context.Context, name string) ([]Attr, error)
	// Open opens the file at name for reading.
	Open(ctx context.Context, name string) (File, error)
	// StatFS reports the capacity of the view.
	StatFS(ctx context.Context) (Space, error)
}

// File is an open file. ReadAt may be called concurrently and out of order.
type File interface {
	// ReadAt has io.ReaderAt semantics with a context.
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	// Attr describes the version of the file that was opened. It does not change
	// for the life of the File, even if the file changes on the nodes.
	Attr() Attr
	// Close releases the file. It is an error to use the File afterwards.
	Close() error
}

// Attr describes a file or directory.
type Attr struct {
	// Name is the base name; "" for the root of the view.
	Name string
	// Dir is true for directories.
	Dir bool
	// Size is the size in bytes, 0 for directories.
	Size int64
	// MTime is the modification time reported by the nodes.
	MTime time.Time
	// Inode identifies the path. It is stable across restarts, because programs
	// such as backup tools and media libraries remember files by inode number.
	Inode uint64
}

// Space is the capacity of a view, in bytes.
type Space struct {
	Total uint64
	Free  uint64
}

var (
	// ErrNotExist means there is no file or directory with that name.
	ErrNotExist = errors.New("file does not exist")
	// ErrNotDir means a directory operation was applied to a file.
	ErrNotDir = errors.New("not a directory")
	// ErrIsDir means a file operation was applied to a directory.
	ErrIsDir = errors.New("is a directory")
	// ErrUnavailable means the nodes could not be reached and the answer is not
	// cached. Mount layers report it as an I/O error, never as "not found": a
	// network outage must not look like deleted files.
	ErrUnavailable = errors.New("cluster unavailable")
)
