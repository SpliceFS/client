//go:build linux

package fuse

import (
	"context"
	"errors"
	"io"
	"path"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/BlueRival/splicefs/client/core"
)

// nameMax is the longest file name reported by statfs.
const nameMax = 255

// env is the state shared by all nodes of one mount.
type env struct {
	fsys   core.FS
	owner  owner
	mapper *errorMapper
	files  *openFiles
}

// newNode returns the node for something the core described.
func (e *env) newNode(a core.Attr) fs.InodeEmbedder {
	if a.Dir {
		return &dirNode{node{env: e}}
	}
	return &fileNode{node{env: e}}
}

// stableAttr gives a node the inode number the core assigned to its path.
func stableAttr(a core.Attr) fs.StableAttr {
	mode := uint32(syscall.S_IFREG)
	if a.Dir {
		mode = syscall.S_IFDIR
	}
	return fs.StableAttr{Mode: mode, Ino: a.Inode}
}

func (e *env) fill(out *fuse.Attr, a core.Attr) {
	t := newAttributes(a, e.owner)
	out.Ino = t.ino
	out.Mode = t.mode
	out.Nlink = t.nlink
	out.Uid, out.Gid = t.owner.uid, t.owner.gid
	out.Size = t.size
	out.Blocks = t.blocks
	out.Blksize = blockSize
	out.Atime, out.Mtime, out.Ctime = t.mtimeSec, t.mtimeSec, t.mtimeSec
	out.Atimensec, out.Mtimensec, out.Ctimensec = t.mtimeNsec, t.mtimeNsec, t.mtimeNsec
}

// join appends a child name to the path of a directory; the root is "".
func join(dir, name string) string {
	return path.Join(dir, name)
}

// node is what files and directories have in common. Its place in the tree is its path:
// the core is asked about names, so no state about the file is kept here.
type node struct {
	fs.Inode
	readOnly
	env *env
}

func (n *node) path() string { return n.Path(nil) }

var (
	_ fs.NodeGetattrer = (*node)(nil)
	_ fs.NodeStatfser  = (*node)(nil)
)

// Getattr implements fs.NodeGetattrer.
func (n *node) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	a, err := n.env.fsys.Stat(ctx, n.path())
	if err != nil {
		return n.env.mapper.errno(err)
	}
	n.env.fill(&out.Attr, a)
	return 0
}

// Statfs implements fs.NodeStatfser. When the cluster cannot be reached it reports an
// empty filesystem: failing here would make df fail for the whole machine.
func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	space, err := n.env.fsys.StatFS(ctx)
	switch {
	case errors.Is(err, core.ErrUnavailable):
		space = core.Space{}
	case err != nil:
		return n.env.mapper.errno(err)
	}
	c := capacityOf(space)
	*out = fuse.StatfsOut{
		Blocks:  c.blocks,
		Bfree:   c.free,
		Bavail:  c.free,
		Bsize:   blockSize,
		Frsize:  blockSize,
		NameLen: nameMax,
	}
	return 0
}

// dirNode is a directory.
type dirNode struct{ node }

var (
	_ fs.NodeLookuper  = (*dirNode)(nil)
	_ fs.NodeReaddirer = (*dirNode)(nil)
)

// Lookup implements fs.NodeLookuper. A missing name is answered with ENOENT, which the
// bridge turns into a cached negative entry.
func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	a, err := d.env.fsys.Stat(ctx, join(d.path(), name))
	if err != nil {
		return nil, d.env.mapper.errno(err)
	}
	d.env.fill(&out.Attr, a)
	return d.NewInode(ctx, d.env.newNode(a), stableAttr(a)), 0
}

// Readdir implements fs.NodeReaddirer.
func (d *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	attrs, err := d.env.fsys.ReadDir(ctx, d.path())
	if err != nil {
		return nil, d.env.mapper.errno(err)
	}
	entries := make([]fuse.DirEntry, len(attrs))
	for i, a := range attrs {
		entries[i] = fuse.DirEntry{Name: a.Name, Mode: stableAttr(a).Mode, Ino: a.Inode}
	}
	return fs.NewListDirStream(entries), 0
}

// fileNode is a regular file.
type fileNode struct{ node }

var _ fs.NodeOpener = (*fileNode)(nil)

// Open implements fs.NodeOpener. The kernel is told to keep cached file data across
// opens, because the contents of a file never change under an open handle, and the
// kernel itself drops the cache when it sees the size or time of the file change.
func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, errReadOnly
	}
	file, err := f.env.fsys.Open(ctx, f.path())
	if err != nil {
		return nil, 0, f.env.mapper.errno(err)
	}
	h := &handle{file: file, mapper: f.env.mapper, files: f.env.files}
	f.env.files.add(h)
	return h, fuse.FOPEN_KEEP_CACHE, 0
}

// handle is an open file.
type handle struct {
	file   core.File
	mapper *errorMapper
	files  *openFiles
}

var (
	_ fs.FileReader   = (*handle)(nil)
	_ fs.FileReleaser = (*handle)(nil)
)

// Read implements fs.FileReader. The context of the request ends when the kernel
// interrupts it, which cancels the read in the core.
//
// A read that returned some bytes before failing is answered with those bytes: the
// kernel asks again for the rest and gets the error then, which is how read(2) behaves.
func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.file.ReadAt(ctx, dest, off)
	if err != nil && !errors.Is(err, io.EOF) && n == 0 {
		return nil, h.mapper.errno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

// Release implements fs.FileReleaser.
func (h *handle) Release(context.Context) syscall.Errno {
	h.files.remove(h)
	if err := h.file.Close(); err != nil {
		return h.mapper.errno(err)
	}
	return 0
}
