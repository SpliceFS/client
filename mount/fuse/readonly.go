//go:build linux || freebsd

package fuse

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// readOnly answers every operation that would change the filesystem, or something the
// kernel could otherwise cache a refusal for, with the right error.
//
// The mount is made read-only, so the kernel answers writes with EROFS before they reach
// the filesystem. These methods are the second line: go-fuse's defaults for several of
// them are ENOTSUP or even success (Unlink and Rmdir), which would be wrong if a request
// ever got through.
type readOnly struct{}

var (
	_ fs.NodeCreater        = readOnly{}
	_ fs.NodeMkdirer        = readOnly{}
	_ fs.NodeMknoder        = readOnly{}
	_ fs.NodeSymlinker      = readOnly{}
	_ fs.NodeLinker         = readOnly{}
	_ fs.NodeUnlinker       = readOnly{}
	_ fs.NodeRmdirer        = readOnly{}
	_ fs.NodeRenamer        = readOnly{}
	_ fs.NodeTmpfiler       = readOnly{}
	_ fs.NodeSetattrer      = readOnly{}
	_ fs.NodeWriter         = readOnly{}
	_ fs.NodeWritever       = readOnly{}
	_ fs.NodeAllocater      = readOnly{}
	_ fs.NodeCopyFileRanger = readOnly{}
	_ fs.NodeSetxattrer     = readOnly{}
	_ fs.NodeRemovexattrer  = readOnly{}
	_ fs.NodeGetxattrer     = readOnly{}
	_ fs.NodeListxattrer    = readOnly{}
)

const errReadOnly = syscall.EROFS

func (readOnly) Create(context.Context, string, uint32, uint32, *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, errReadOnly
}

func (readOnly) Mkdir(context.Context, string, uint32, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errReadOnly
}

func (readOnly) Mknod(context.Context, string, uint32, uint32, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errReadOnly
}

func (readOnly) Symlink(context.Context, string, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errReadOnly
}

func (readOnly) Link(context.Context, fs.InodeEmbedder, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errReadOnly
}

func (readOnly) Unlink(context.Context, string) syscall.Errno { return errReadOnly }

func (readOnly) Rmdir(context.Context, string) syscall.Errno { return errReadOnly }

func (readOnly) Rename(context.Context, string, fs.InodeEmbedder, string, uint32) syscall.Errno {
	return errReadOnly
}

func (readOnly) Tmpfile(context.Context, uint32, uint32, *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, errReadOnly
}

func (readOnly) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return errReadOnly
}

func (readOnly) Write(context.Context, fs.FileHandle, []byte, int64) (uint32, syscall.Errno) {
	return 0, errReadOnly
}

func (readOnly) Writev(context.Context, fs.FileHandle, [][]byte, int64) (uint32, syscall.Errno) {
	return 0, errReadOnly
}

func (readOnly) Allocate(context.Context, fs.FileHandle, uint64, uint64, uint32) syscall.Errno {
	return errReadOnly
}

func (readOnly) CopyFileRange(context.Context, fs.FileHandle, uint64, *fs.Inode, fs.FileHandle, uint64, uint64, uint64) (uint32, syscall.Errno) {
	return 0, errReadOnly
}

func (readOnly) Setxattr(context.Context, string, []byte, uint32) syscall.Errno { return errReadOnly }

func (readOnly) Removexattr(context.Context, string) syscall.Errno { return errReadOnly }

// Getxattr and Listxattr answer ENOSYS, not ENODATA or an empty list, which go-fuse
// would otherwise return. The kernel remembers ENOSYS for the life of the mount and
// answers every later getxattr and listxattr call itself with EOPNOTSUPP, without a
// round trip. Tools such as ls -l ask for extended attributes on every file.
//
// Locks need no code. go-fuse asks the kernel to forward them only when
// MountOptions.EnableLocks is set; without it the kernel keeps flock and POSIX locks
// local to this machine, which is the right behavior for a read-only mount and is never
// refused.
func (readOnly) Getxattr(context.Context, string, []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS
}

func (readOnly) Listxattr(context.Context, []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENOSYS
}
