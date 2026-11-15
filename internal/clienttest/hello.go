package clienttest

import (
	"context"
	"io"
	"time"

	"github.com/SpliceFS/client/core"
)

const (
	// HelloText is the content of the one file of HelloFS.
	HelloText = "hello from the cluster\n"
	// HelloName is the name of the one file of HelloFS.
	HelloName = "hello.txt"
	// helloUnix is the modification time of every entry, a fixed second so that runs compare equal.
	helloUnix = 1_700_000_000
	// The inode numbers of the root and of the file.
	rootInode = 1
	fileInode = 2
	// The size reported for the filesystem: a gibibyte, half of it free.
	spaceTotal = 1 << 30
	spaceFree  = 1 << 29
)

// helloRoot is the attributes of the root folder of HelloFS.
func helloRoot() core.Attr {
	return core.Attr{Dir: true, MTime: time.Unix(helloUnix, 0), Inode: rootInode}
}

// helloFile is the attributes of the one file in HelloFS.
func helloFile() core.Attr {
	return core.Attr{Name: HelloName, Size: int64(len(HelloText)), MTime: time.Unix(helloUnix, 0), Inode: fileInode}
}

// HelloFS is a filesystem with one file in its root, for the tests that mount for real.
type HelloFS struct{}

// Stat answers for the root and the file.
func (HelloFS) Stat(_ context.Context, name string) (core.Attr, error) {
	switch name {
	case "":
		return helloRoot(), nil
	case HelloName:
		return helloFile(), nil
	}
	return core.Attr{}, core.ErrNotExist
}

// ReadDir lists the file in the root.
func (HelloFS) ReadDir(_ context.Context, name string) ([]core.Attr, error) {
	if name != "" {
		return nil, core.ErrNotExist
	}
	return []core.Attr{helloFile()}, nil
}

// Open opens the file.
//
//nolint:ireturn // it implements FS.Open, which returns the File interface.
func (HelloFS) Open(_ context.Context, name string) (core.File, error) {
	if name != HelloName {
		return nil, core.ErrNotExist
	}
	return helloOpen{}, nil
}

// StatFS reports a fixed size.
func (HelloFS) StatFS(context.Context) (core.Space, error) {
	return core.Space{Total: spaceTotal, Free: spaceFree}, nil
}

type helloOpen struct{}

func (helloOpen) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if off >= int64(len(HelloText)) {
		return 0, io.EOF
	}
	n := copy(p, HelloText[off:])
	if off+int64(n) == int64(len(HelloText)) {
		return n, io.EOF
	}
	return n, nil
}

func (helloOpen) Attr() core.Attr { return helloFile() }
func (helloOpen) Close() error    { return nil }
