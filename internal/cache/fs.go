package cache

import (
	"fmt"
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

func (osFS) MkdirAll(path string) error {
	if err := os.MkdirAll(path, dirPerm); err != nil {
		return fmt.Errorf("make directory: %w", err)
	}
	return nil
}

func (osFS) ReadDir(name string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(name)
	if err != nil {
		return nil, fmt.Errorf("list directory: %w", err)
	}
	return entries, nil
}

func (osFS) ReadFile(name string) ([]byte, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return raw, nil
}

// OpenFile returns an untyped nil on failure; returning the *os.File directly would
// produce a non-nil interface holding a nil pointer. It returns the dataFile interface
// because it implements fileSystem, whose tests substitute their own file type.
//
//nolint:ireturn // required by the fileSystem interface so that tests can inject failures
func (osFS) OpenFile(name string, flag int) (dataFile, error) {
	file, err := os.OpenFile(name, flag, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	return file, nil
}

func (osFS) Remove(name string) error {
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

func (osFS) Rename(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	return nil
}

// statfsFree returns the bytes available to an unprivileged process on the disk holding dir.
func statfsFree(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("read file system statistics: %w", err)
	}
	return asUint64(stat.Bavail) * asUint64(stat.Bsize), nil
}

// asUint64 exists because the fields of Statfs_t have another type on every system: the
// block size is int64 on Linux, uint32 on macOS and uint64 on FreeBSD, and the free blocks
// are uint64 on Linux and macOS and int64 on FreeBSD. Neither number is ever negative when
// the kernel reports it, so the conversion cannot wrap.
func asUint64[T int32 | int64 | uint32 | uint64](v T) uint64 { return uint64(v) }
