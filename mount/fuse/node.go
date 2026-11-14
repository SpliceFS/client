//go:build linux || freebsd

package fuse

import (
	"context"
	"errors"
	"io"
	"path"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
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

// newNode returns the node for something the core described. The result is an
// interface because the node type depends on a.Dir.
//
//nolint:ireturn // fs.NodeFS takes an fs.InodeEmbedder and the concrete type differs per file kind.
func (e *env) newNode(a core.Attr) fs.InodeEmbedder {
	if a.Dir {
		return &dirNode{node{env: e}}
	}
	return &fileNode{node: node{env: e}}
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
	if err != nil {
		if !errors.Is(err, core.ErrUnavailable) {
			return n.env.mapper.errno(err)
		}
		space = core.Space{}
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

func (n *node) path() string { return n.Path(nil) }

// dirNode is a directory.
type dirNode struct{ node }

var (
	_ fs.NodeLookuper  = (*dirNode)(nil)
	_ fs.NodeReaddirer = (*dirNode)(nil)
)

// Lookup implements fs.NodeLookuper. A missing name is answered with ENOENT, which the
// bridge turns into a cached negative entry. The probe name is answered before anything
// else so that no lock and no call into the core can delay it; it only counts directly
// below the root, whose path is empty.
func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name == mount.ProbeName && d.path() == "" {
		return nil, syscall.ENOENT
	}
	a, err := d.env.fsys.Stat(ctx, join(d.path(), name))
	if err != nil {
		return nil, d.env.mapper.errno(err)
	}
	d.env.fill(&out.Attr, a)
	return d.NewInode(ctx, d.env.newNode(a), stableAttr(a)), 0
}

// Readdir implements fs.NodeReaddirer.
//
//nolint:ireturn // fs.NodeReaddirer fixes this signature.
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

// fileNode is a regular file. It is the one place that remembers something about a file:
// the version of its last open. The kernel drops its cached pages of a file when it
// forgets the file, and go-fuse then drops this node, so the two always live as long as
// each other.
type fileNode struct {
	node

	mu      sync.Mutex
	opened  bool
	version core.Version
}

var _ fs.NodeOpener = (*fileNode)(nil)

// Open implements fs.NodeOpener. The kernel is told to keep cached file data across
// opens, because the contents of a file never change under an open handle, and the
// kernel itself drops the cache when it sees the size or time of the file change. A file
// can also be replaced by one of the same size and time. The core then opens another
// version, and the kernel is not told to keep its pages, which are of the old copy.
//
//nolint:ireturn // fs.NodeOpener fixes this signature.
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
	if f.sameVersion(file.Attr().Version) {
		return h, fuse.FOPEN_KEEP_CACHE, 0
	}
	return h, 0, 0
}

// sameVersion records the version of an open and reports whether the previous open of
// this file, if there was one, had the same version.
func (f *fileNode) sameVersion(version core.Version) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	same := !f.opened || f.version == version
	f.opened, f.version = true, version
	return same
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
//
//nolint:ireturn // fs.FileReader fixes this signature.
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
