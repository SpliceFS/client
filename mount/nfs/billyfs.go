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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/file"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

// Permission bits every entry reports: nothing can be written, and a program that can
// see the mount may read all of it.
const (
	fileMode fs.FileMode = 0o444
	dirMode  fs.FileMode = 0o555
)

const (
	// rootAttrBudget is how long a request for the attributes of the root waits for the
	// core before it is answered without it.
	rootAttrBudget = time.Second

	// rootInode is the file id of the root, whether the core has answered or not: the
	// kernel must not see the id of the root change when the first real answer arrives.
	// The core never gives this number to anything else.
	rootInode = 1
)

// billyFS is a read-only billy.Filesystem over a core.FS. billy has no contexts, so
// every call gets one from base with a time limit: when the mount ends, base is
// cancelled and no call outlives it.
//
// go-nfs refuses every write to a filesystem that does not report billy.WriteCapability
// (it answers NFS3ERR_ROFS before the filesystem is asked), so the mutating methods
// below are only a second line of defence and return billy.ErrReadOnly.
//
// The attributes of the root never fail and never take longer than rootBudget, because
// the kernel asks for them before it looks up any name and before it accepts a mount:
// with the cluster unreachable they would otherwise refuse the mount and delay even the
// probe name, which is meant to be answered without the cluster.
type billyFS struct {
	//nolint:containedctx // billy has no contexts: the call context of every method comes from here.
	base    context.Context
	fsys    core.FS
	timeout time.Duration
	owner   owner
	log     *slog.Logger

	rootBudget time.Duration
	created    time.Time // modification time of the root until the core has answered
	root       rootAttr

	spaceMu   sync.Mutex
	lastSpace core.Space // the last capacity the core reported; zero until it has

	// pins keeps the opened file of each path that is being read; see pinnedFiles.
	pins *pinnedFiles
}

// rootAttr is the state of the root's attributes: the last answer of the core and the
// call that is asking for the next one. At most one call runs at a time, so a core that
// never answers costs one goroutine, which ends with the call context.
type rootAttr struct {
	mu      sync.Mutex
	last    core.Attr
	known   bool
	pending chan struct{} // closed when the running call has ended; nil if none runs
	started time.Time
}

func newBillyFS(
	base context.Context, fsys core.FS, timeout time.Duration, fileOwner owner, log *slog.Logger,
) *billyFS {
	return &billyFS{
		base:       base,
		fsys:       fsys,
		timeout:    timeout,
		owner:      fileOwner,
		log:        log,
		rootBudget: rootAttrBudget,
		created:    time.Now(),
		pins:       newPinnedFiles(fsys, log),
	}
}

var (
	_ billy.Filesystem = (*billyFS)(nil)
	_ billy.Capable    = (*billyFS)(nil)
)

// Capabilities reports a read-only filesystem. Not having billy.WriteCapability is what
// makes go-nfs answer NFS3ERR_ROFS to create, write, remove, rename, setattr, link and
// symlink requests.
func (*billyFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

// corePath converts a billy path to a core name. Paths come from Join, which cleans
// them, so the only unusable ones are those that climb out of the root. The probe name
// directly in the root never exists and is refused here, before any call into the core
// could be made for it; below the root it is an ordinary name.
func corePath(operation, name string) (string, error) {
	if name == "" || name == "." {
		return "", nil
	}
	if !fs.ValidPath(name) || name == mount.ProbeName {
		return "", &fs.PathError{Op: operation, Path: name, Err: fs.ErrNotExist}
	}
	return name, nil
}

func readOnly(operation, name string) error {
	return &fs.PathError{Op: operation, Path: name, Err: billy.ErrReadOnly}
}

// Join joins path elements with slashes and cleans the result.
func (*billyFS) Join(elem ...string) string {
	return path.Join(elem...)
}

// Stat describes the file or directory at name.
func (b *billyFS) Stat(name string) (os.FileInfo, error) {
	cname, err := corePath("stat", name)
	if err != nil {
		return nil, err
	}
	if cname == "" {
		return fileInfo{attr: b.rootAttr(), owner: b.owner}, nil
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

// Open opens the file at name for reading. go-nfs calls it for every READ request, so
// the core.File is shared by the READs of one path (see pinnedFiles) and every READ of a
// busy file reads the one version that was chosen when it was first opened.
//
//nolint:ireturn // the signature is fixed by billy.Filesystem.
func (b *billyFS) Open(name string) (billy.File, error) {
	cname, err := corePath("open", name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := b.callContext()
	defer cancel()
	p, err := b.pins.acquire(ctx, cname)
	if err != nil {
		return nil, b.mapError("open", name, err)
	}
	return &billyFile{fs: b, name: name, pin: p, file: p.file}, nil
}

// OpenFile opens name for reading and refuses every flag that could change it.
//
//nolint:ireturn // the signature is fixed by billy.Filesystem.
func (b *billyFS) OpenFile(name string, flag int, _ os.FileMode) (billy.File, error) {
	const mutating = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC
	if flag&mutating != 0 {
		return nil, readOnly("open", name)
	}
	return b.Open(name)
}

// Create is refused: the filesystem is read-only.
//
//nolint:ireturn // The signature is fixed by billy.Filesystem.
func (*billyFS) Create(name string) (billy.File, error) {
	return nil, readOnly("create", name)
}

// Rename is refused: the filesystem is read-only.
func (*billyFS) Rename(oldpath, _ string) error {
	return readOnly("rename", oldpath)
}

// Remove is refused: the filesystem is read-only.
func (*billyFS) Remove(name string) error {
	return readOnly("remove", name)
}

// TempFile is refused: the filesystem is read-only.
//
//nolint:ireturn // The signature is fixed by billy.Filesystem.
func (*billyFS) TempFile(dir, _ string) (billy.File, error) {
	return nil, readOnly("tempfile", dir)
}

// MkdirAll is refused: the filesystem is read-only.
func (*billyFS) MkdirAll(name string, _ os.FileMode) error {
	return readOnly("mkdir", name)
}

// Symlink is refused: the filesystem is read-only.
func (*billyFS) Symlink(_, link string) error {
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
//
//nolint:ireturn // The signature is fixed by billy.Filesystem.
func (*billyFS) Chroot(string) (billy.Filesystem, error) {
	return nil, billy.ErrNotSupported
}

// Root is the root of the view.
func (*billyFS) Root() string {
	return "/"
}

// close ends the sharing of opened files and closes the idle ones. It is called when the
// mount ends.
func (b *billyFS) close() {
	b.pins.closeAll()
}

func (b *billyFS) callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(b.base, b.timeout)
}

// mapError turns a core error into the error go-nfs expects. go-nfs tests errors with
// os.IsNotExist, which does not unwrap, so a not-exist error must be a *fs.PathError
// around fs.ErrNotExist itself. Everything that is not a precise answer, and above all
// core.ErrUnavailable, becomes an I/O error: an outage must not look like missing files.
func (b *billyFS) mapError(operation, name string, err error) error {
	switch {
	case errors.Is(err, core.ErrNotExist):
		return &fs.PathError{Op: operation, Path: name, Err: fs.ErrNotExist}
	case errors.Is(err, core.ErrNotDir):
		return &fs.PathError{Op: operation, Path: name, Err: syscall.ENOTDIR}
	case errors.Is(err, core.ErrIsDir):
		return &fs.PathError{Op: operation, Path: name, Err: syscall.EISDIR}
	default:
		b.warnCoreFailure(operation, name, err)
		return &fs.PathError{Op: operation, Path: name, Err: fmt.Errorf("%w: %w", syscall.EIO, err)}
	}
}

func (b *billyFS) warnCoreFailure(operation, name string, err error) {
	b.log.Warn("core call failed",
		slog.String("op", operation), slog.String("name", name), slog.Any("err", err))
}

// rootAttr answers within rootBudget whatever the core does. It asks the core and
// waits for the answer; if the core fails or is too slow it returns the last answer it
// got, or a fixed read-only directory if it never got one. The real modification time is
// kept whenever the core answers because macOS decides from it whether a cached listing
// is still good.
//
// A call that is too slow is not abandoned: it keeps running and its result, if it
// succeeds, is remembered for the next request. Requests that arrive meanwhile wait for
// that same call instead of starting another, and only for what is left of its budget.
func (b *billyFS) rootAttr() core.Attr {
	r := &b.root
	r.mu.Lock()
	if r.pending == nil {
		r.pending = make(chan struct{})
		r.started = time.Now()
		go b.refreshRoot(r.pending)
	}
	pending, wait := r.pending, b.rootBudget-time.Since(r.started)
	r.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-pending:
	case <-timer.C:
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known {
		return r.last
	}
	return core.Attr{Dir: true, MTime: b.created, Inode: rootInode}
}

// refreshRoot is the one running call for the attributes of the root. It ends the call
// by closing done.
func (b *billyFS) refreshRoot(done chan struct{}) {
	ctx, cancel := b.callContext()
	defer cancel()
	attr, err := b.fsys.Stat(ctx, "")
	if err != nil {
		// The kernel asks for the root many times a second. The session already warns
		// that the cluster cannot be reached, so this line is for debugging only.
		b.log.DebugContext(ctx, "core call failed, answering with the last known root",
			slog.Any("err", err))
	}
	r := &b.root
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		attr.Inode = rootInode
		r.last, r.known = attr, true
	}
	r.pending = nil
	close(done)
}

// space reports the capacity of the view. It never fails: the kernel asks for it while it
// mounts, and a mount must succeed while no node can be reached. When the core cannot
// answer, the last capacity it reported is given, which is zero before the first answer.
func (b *billyFS) space(ctx context.Context) core.Space {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	capacity, err := b.fsys.StatFS(ctx)
	b.spaceMu.Lock()
	defer b.spaceMu.Unlock()
	if err != nil {
		// mapError is called for its log line, which only an answer that is not precise gets;
		// the error itself is not passed on.
		_ = b.mapError("statfs", "", err) //nolint:errcheck // See above, the error is not passed on.
		return b.lastSpace
	}
	b.lastSpace = capacity
	return capacity
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

// billyFile is one use of a shared open core.File. go-nfs only uses ReadAt and Close;
// Read and Seek exist to satisfy billy.File. Close ends the use; the core.File itself is
// closed by pinnedFiles.
type billyFile struct {
	fs       *billyFS
	name     string
	pin      *pin
	file     core.File
	released atomic.Bool

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
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		return n, io.EOF
	default:
		// The next READ must open the file again instead of meeting this failure
		// again on the same open.
		f.fs.pins.discard(f.pin)
		return n, f.fs.mapError("read", f.name, err)
	}
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

// Close gives the file back. A second Close does nothing, so the use is never counted
// out twice.
func (f *billyFile) Close() error {
	if f.released.CompareAndSwap(false, true) {
		f.fs.pins.release(f.pin)
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
func (*billyFile) Lock() error { return nil }

// Unlock does nothing; see Lock.
func (*billyFile) Unlock() error { return nil }
