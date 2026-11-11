package cache

import (
	"io"
	"os"
	"syscall"
)

const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// dataFile is the part of *os.File the cache uses.
type dataFile interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Close() error
}

// fileSystem is the part of package os the cache uses, so tests can inject failures.
type fileSystem interface {
	MkdirAll(path string) error
	ReadDir(name string) ([]os.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	OpenFile(name string, flag int) (dataFile, error)
	Remove(name string) error
	Rename(oldPath, newPath string) error
}

type osFS struct{}

func (osFS) MkdirAll(path string) error { return os.MkdirAll(path, dirPerm) }

func (osFS) ReadDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }

func (osFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// OpenFile returns an untyped nil on failure; returning the *os.File directly would
// produce a non-nil interface holding a nil pointer.
func (osFS) OpenFile(name string, flag int) (dataFile, error) {
	f, err := os.OpenFile(name, flag, filePerm)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (osFS) Remove(name string) error { return os.Remove(name) }

func (osFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

// statfsFree returns the bytes available to an unprivileged process on the disk holding dir.
func statfsFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	block := uint64(st.Bsize) //nolint:gosec // a block size reported by the kernel is never negative
	return st.Bavail * block, nil
}
