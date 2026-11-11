package nfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/file"

	"github.com/SpliceFS/client/core"
)

// Permission bits every entry reports: nothing can be written, and a program that can
// see the mount may read all of it.
const (
	fileMode fs.FileMode = 0o444
	dirMode  fs.FileMode = 0o555
)

// billyFS is a read-only billy.Filesystem over a core.FS. billy has no contexts, so
// every call gets one from base with a time limit: when the mount ends, base is
// cancelled and no call outlives it.
//
// go-nfs refuses every write to a filesystem that does not report billy.WriteCapability
// (it answers NFS3ERR_ROFS before the filesystem is asked), so the mutating methods
// below are only a second line of defence and return billy.ErrReadOnly.
type billyFS struct {
	base    context.Context
	fsys    core.FS
	timeout time.Duration
	owner   owner
	log     *slog.Logger
}

var (
	_ billy.Filesystem = (*billyFS)(nil)
	_ billy.Capable    = (*billyFS)(nil)
)

// Capabilities reports a read-only filesystem. Not having billy.WriteCapability is what
// makes go-nfs answer NFS3ERR_ROFS to create, write, remove, rename, setattr, link and
// symlink requests.
func (b *billyFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

func (b *billyFS) callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(b.base, b.timeout)
}

// corePath converts a billy path to a core name. Paths come from Join, which cleans
// them, so the only unusable ones are those that climb out of the root.
func corePath(op, name string) (string, error) {
	if name == "" || name == "." {
		return "", nil
	}
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return name, nil
}

// mapError turns a core error into the error go-nfs expects. go-nfs tests errors with
// os.IsNotExist, which does not unwrap, so a not-exist error must be a *fs.PathError
// around fs.ErrNotExist itself. Everything that is not a precise answer, and above all
// core.ErrUnavailable, becomes an I/O error: an outage must not look like missing files.
func (b *billyFS) mapError(op, name string, err error) error {
	switch {
	case errors.Is(err, core.ErrNotExist):
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	case errors.Is(err, core.ErrNotDir):
		return &fs.PathError{Op: op, Path: name, Err: syscall.ENOTDIR}
	case errors.Is(err, core.ErrIsDir):
		return &fs.PathError{Op: op, Path: name, Err: syscall.EISDIR}
	default:
		b.log.Warn("core call failed", "op", op, "name", name, "err", err)
		return &fs.PathError{Op: op, Path: name, Err: fmt.Errorf("%w: %w", syscall.EIO, err)}
	}
}

func readOnly(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: billy.ErrReadOnly}
}

// Join joins path elements with slashes and cleans the result.
func (b *billyFS) Join(elem ...string) string {
	return path.Join(elem...)
}

// Stat describes the file or directory at name.
func (b *billyFS) Stat(name string) (os.FileInfo, error) {
	cname, err := corePath("stat", name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := b.callContext()
	defer cancel()
	attr, err := b.fsys.Stat(ctx, cname)
	if err != nil {
		return nil, b.mapError("stat", name, err)
	}
	return fileInfo{attr: attr, owner: b.owner}, nil
}

// Lstat is Stat: there are no symbolic links.
func (b *billyFS) Lstat(name string) (os.FileInfo, error) {
	return b.Stat(name)
}

// ReadDir lists the directory at name.
func (b *billyFS) ReadDir(name string) ([]os.FileInfo, error) {
	cname, err := corePath("readdir", name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := b.callContext()
	defer cancel()
	attrs, err := b.fsys.ReadDir(ctx, cname)
	if err != nil {
		return nil, b.mapError("readdir", name, err)
	}
	infos := make([]os.FileInfo, len(attrs))
	for i, attr := range attrs {
		infos[i] = fileInfo{attr: attr, owner: b.owner}
	}
	return infos, nil
}

// Open opens the file at name for reading.
func (b *billyFS) Open(name string) (billy.File, error) {
	cname, err := corePath("open", name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := b.callContext()
	defer cancel()
	f, err := b.fsys.Open(ctx, cname)
	if err != nil {
		return nil, b.mapError("open", name, err)
	}
	return &billyFile{fs: b, name: name, file: f}, nil
}

// OpenFile opens name for reading and refuses every flag that could change it.
func (b *billyFS) OpenFile(name string, flag int, _ os.FileMode) (billy.File, error) {
	const mutating = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC
	if flag&mutating != 0 {
		return nil, readOnly("open", name)
	}
	return b.Open(name)
}

// Create is refused: the filesystem is read-only.
func (b *billyFS) Create(name string) (billy.File, error) {
	return nil, readOnly("create", name)
}

// Rename is refused: the filesystem is read-only.
func (b *billyFS) Rename(oldpath, _ string) error {
	return readOnly("rename", oldpath)
}

// Remove is refused: the filesystem is read-only.
func (b *billyFS) Remove(name string) error {
	return readOnly("remove", name)
}

// TempFile is refused: the filesystem is read-only.
func (b *billyFS) TempFile(dir, _ string) (billy.File, error) {
	return nil, readOnly("tempfile", dir)
}

// MkdirAll is refused: the filesystem is read-only.
func (b *billyFS) MkdirAll(name string, _ os.FileMode) error {
	return readOnly("mkdir", name)
}

// Symlink is refused: the filesystem is read-only.
func (b *billyFS) Symlink(_, link string) error {
	return readOnly("symlink", link)
}

// Readlink fails because nothing is a symbolic link.
func (b *billyFS) Readlink(link string) (string, error) {
	if _, err := b.Stat(link); err != nil {
		return "", err
	}
	return "", &fs.PathError{Op: "readlink", Path: link, Err: syscall.EINVAL}
}

// Chroot is not supported: the whole view is always served.
func (b *billyFS) Chroot(string) (billy.Filesystem, error) {
	return nil, billy.ErrNotSupported
}

// Root is the root of the view.
func (b *billyFS) Root() string {
	return "/"
}

// space reports the capacity of the view.
func (b *billyFS) space(ctx context.Context) (core.Space, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	sp, err := b.fsys.StatFS(ctx)
	if err != nil {
		return core.Space{}, b.mapError("statfs", "", err)
	}
	return sp, nil
}

// fileInfo presents a core.Attr as an os.FileInfo with the attributes the mount
// promises: mode 0444 for files and 0555 for directories, one link, and the owner of
// the client process. go-nfs reads link count, owner and file id from Sys.
type fileInfo struct {
	attr  core.Attr
	owner owner
}

func (i fileInfo) Name() string       { return i.attr.Name }
func (i fileInfo) Size() int64        { return i.attr.Size }
func (i fileInfo) ModTime() time.Time { return i.attr.MTime }
func (i fileInfo) IsDir() bool        { return i.attr.Dir }

func (i fileInfo) Mode() fs.FileMode {
	if i.attr.Dir {
		return fs.ModeDir | dirMode
	}
	return fileMode
}

// Sys carries the fields go-nfs cannot get from os.FileInfo. The inode becomes the NFS
// file id, which programs use to recognize a file again.
func (i fileInfo) Sys() any {
	return file.FileInfo{Nlink: 1, UID: i.owner.uid, GID: i.owner.gid, Fileid: i.attr.Inode}
}

// billyFile is an open core.File. go-nfs only uses ReadAt and Close; Read and Seek
// exist to satisfy billy.File.
type billyFile struct {
	fs   *billyFS
	name string
	file core.File

	mu     sync.Mutex
	offset int64
}

func (f *billyFile) Name() string { return f.name }

// ReadAt has io.ReaderAt semantics. io.EOF is passed through untouched because go-nfs
// compares against it to set the end-of-file flag.
func (f *billyFile) ReadAt(p []byte, off int64) (int, error) {
	ctx, cancel := f.fs.callContext()
	defer cancel()
	n, err := f.file.ReadAt(ctx, p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, f.fs.mapError("read", f.name, err)
	}
	return n, err
}

func (f *billyFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.ReadAt(p, f.offset)
	f.offset += int64(n)
	return n, err
}

func (f *billyFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.offset
	case io.SeekEnd:
		base = f.file.Attr().Size
	default:
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: syscall.EINVAL}
	}
	if base+offset < 0 {
		return 0, &fs.PathError{Op: "seek", Path: f.name, Err: syscall.EINVAL}
	}
	f.offset = base + offset
	return f.offset, nil
}

func (f *billyFile) Close() error {
	if err := f.file.Close(); err != nil {
		return f.fs.mapError("close", f.name, err)
	}
	return nil
}

// Write is refused: the filesystem is read-only.
func (f *billyFile) Write([]byte) (int, error) {
	return 0, readOnly("write", f.name)
}

// Truncate is refused: the filesystem is read-only.
func (f *billyFile) Truncate(int64) error {
	return readOnly("truncate", f.name)
}

// Lock does nothing: the file cannot change, so there is nothing to protect.
func (f *billyFile) Lock() error { return nil }

// Unlock does nothing; see Lock.
func (f *billyFile) Unlock() error { return nil }
