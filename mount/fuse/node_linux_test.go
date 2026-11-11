//go:build linux

package fuse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

const rootID = 1 // the node id of the root of every FUSE filesystem

var testSettings = settings{
	entryTimeout:    11 * time.Second,
	attrTimeout:     12 * time.Second,
	negativeTimeout: 3 * time.Second,
}

// harness drives the node layer through go-fuse's raw interface, the same entry points
// the kernel requests arrive at, without a kernel.
type harness struct {
	t    *testing.T
	fake *fakeFS
	root *dirNode
	raw  fuse.RawFileSystem
	log  *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeFS()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	root := &dirNode{node{env: &env{
		fsys:   fake,
		owner:  owner{uid: 1000, gid: 1001},
		mapper: newErrorMapper(log),
		files:  newOpenFiles(),
	}}}
	return &harness{
		t:    t,
		fake: fake,
		root: root,
		raw:  fs.NewNodeFS(root, nodeOptions(testSettings, log)),
		log:  &logs,
	}
}

func hdr(id uint64) *fuse.InHeader { return &fuse.InHeader{NodeId: id} }

func (h *harness) lookupStatus(parent uint64, name string) (uint64, *fuse.EntryOut, fuse.Status) {
	var out fuse.EntryOut
	st := h.raw.Lookup(nil, hdr(parent), name, &out)
	return out.NodeId, &out, st
}

// lookup returns the node id of name below parent.
func (h *harness) lookup(parent uint64, name string) uint64 {
	h.t.Helper()
	id, _, st := h.lookupStatus(parent, name)
	if st != fuse.OK || id == 0 {
		h.t.Fatalf("Lookup(%q) = node %d, %v", name, id, st)
	}
	return id
}

func (h *harness) getattr(id uint64) (*fuse.AttrOut, fuse.Status) {
	var out fuse.AttrOut
	st := h.raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: *hdr(id)}, &out)
	return &out, st
}

func (h *harness) open(id uint64, flags uint32) (uint64, *fuse.OpenOut, fuse.Status) {
	var out fuse.OpenOut
	st := h.raw.Open(nil, &fuse.OpenIn{InHeader: *hdr(id), Flags: flags}, &out)
	return out.Fh, &out, st
}

func (h *harness) read(id, fh uint64, off uint64, size int) ([]byte, fuse.Status) {
	buf := make([]byte, size)
	res, st := h.raw.Read(nil, &fuse.ReadIn{InHeader: *hdr(id), Fh: fh, Offset: off, Size: uint32(size)}, buf)
	if st != fuse.OK {
		return nil, st
	}
	data, st := res.Bytes(buf)
	return append([]byte(nil), data...), st
}

// readDir lists a directory, including the entries go-fuse adds for "." and "..".
func (h *harness) readDir(id uint64) ([]fuse.DirEntry, fuse.Status) {
	var open fuse.OpenOut
	if st := h.raw.OpenDir(nil, &fuse.OpenIn{InHeader: *hdr(id)}, &open); st != fuse.OK {
		return nil, st
	}
	defer h.raw.ReleaseDir(&fuse.ReleaseIn{InHeader: *hdr(id), Fh: open.Fh})
	list := fuse.NewDirEntryList(make([]byte, 64<<10), 0)
	if st := h.raw.ReadDir(nil, &fuse.ReadIn{InHeader: *hdr(id), Fh: open.Fh, Size: 64 << 10}, list); st != fuse.OK {
		return nil, st
	}
	entries, err := fuse.ParseDirEntries(list.Bytes())
	if err != nil {
		h.t.Fatal(err)
	}
	return entries, fuse.OK
}

func TestNodesImplementTheRightOperations(t *testing.T) {
	t.Parallel()
	var (
		_ fs.NodeCreater        = (*dirNode)(nil)
		_ fs.NodeMkdirer        = (*dirNode)(nil)
		_ fs.NodeMknoder        = (*dirNode)(nil)
		_ fs.NodeSymlinker      = (*dirNode)(nil)
		_ fs.NodeLinker         = (*dirNode)(nil)
		_ fs.NodeUnlinker       = (*dirNode)(nil)
		_ fs.NodeRmdirer        = (*dirNode)(nil)
		_ fs.NodeRenamer        = (*dirNode)(nil)
		_ fs.NodeTmpfiler       = (*dirNode)(nil)
		_ fs.NodeSetattrer      = (*dirNode)(nil)
		_ fs.NodeSetxattrer     = (*dirNode)(nil)
		_ fs.NodeRemovexattrer  = (*dirNode)(nil)
		_ fs.NodeGetxattrer     = (*dirNode)(nil)
		_ fs.NodeListxattrer    = (*dirNode)(nil)
		_ fs.NodeGetattrer      = (*dirNode)(nil)
		_ fs.NodeStatfser       = (*dirNode)(nil)
		_ fs.NodeWriter         = (*fileNode)(nil)
		_ fs.NodeWritever       = (*fileNode)(nil)
		_ fs.NodeAllocater      = (*fileNode)(nil)
		_ fs.NodeCopyFileRanger = (*fileNode)(nil)
		_ fs.NodeSetattrer      = (*fileNode)(nil)
		_ fs.NodeGetattrer      = (*fileNode)(nil)
		_ fs.NodeStatfser       = (*fileNode)(nil)
	)
}

func TestLookupDescribesFilesAndDirectories(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tests := []struct {
		path     string
		parent   string
		name     string
		wantMode uint32
		wantSize uint64
		blocks   uint64
	}{
		{"hello.txt", "", "hello.txt", syscall.S_IFREG | 0o444, uint64(len(fakeContent)), 1},
		{"big.bin", "", "big.bin", syscall.S_IFREG | 0o444, bigSize, (bigSize + 511) / 512},
		{"empty.txt", "", "empty.txt", syscall.S_IFREG | 0o444, 0, 0},
		{"docs", "", "docs", syscall.S_IFDIR | 0o555, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			_, out, st := h.lookupStatus(rootID, tt.name)
			if st != fuse.OK {
				t.Fatalf("Lookup = %v", st)
			}
			want := fuse.Attr{
				Ino: fakeInode(tt.path), Size: tt.wantSize, Blocks: tt.blocks, Mode: tt.wantMode, Nlink: 1,
				Owner:   fuse.Owner{Uid: 1000, Gid: 1001},
				Atime:   uint64(fakeMTime.Unix()),
				Mtime:   uint64(fakeMTime.Unix()),
				Ctime:   uint64(fakeMTime.Unix()),
				Blksize: blockSize,
			}
			want.Atimensec, want.Mtimensec, want.Ctimensec = 891_000_000, 891_000_000, 891_000_000
			if out.Attr != want {
				t.Fatalf("attr =\n%+v\nwant\n%+v", out.Attr, want)
			}
			if out.EntryTimeout() != testSettings.entryTimeout || out.AttrTimeout() != testSettings.attrTimeout {
				t.Fatalf("timeouts = %v, %v; want %v, %v", out.EntryTimeout(), out.AttrTimeout(), testSettings.entryTimeout, testSettings.attrTimeout)
			}
		})
	}
}

func TestLookupBuildsPathsFromTheTree(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	docs := h.lookup(rootID, "docs")
	sub := h.lookup(docs, "sub")
	deep := h.lookup(sub, "deep.txt")
	attr, st := h.getattr(deep)
	if st != fuse.OK || attr.Ino != fakeInode("docs/sub/deep.txt") {
		t.Fatalf("Getattr(docs/sub/deep.txt) = ino %d, %v; want ino %d", attr.Ino, st, fakeInode("docs/sub/deep.txt"))
	}
}

func TestLookupOfMissingNameIsANegativeEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	id, out, st := h.lookupStatus(rootID, "missing")
	if st != fuse.OK || id != 0 {
		t.Fatalf("Lookup = node %d, %v; want the node id 0 of a negative entry", id, st)
	}
	if out.EntryTimeout() != testSettings.negativeTimeout {
		t.Fatalf("negative entry timeout = %v, want %v", out.EntryTimeout(), testSettings.negativeTimeout)
	}

	var entry fuse.EntryOut
	if _, errno := h.root.Lookup(context.Background(), "missing", &entry); errno != syscall.ENOENT {
		t.Fatalf("node Lookup = %v, want ENOENT", errno)
	}
}

func TestErrorsAreMappedForEveryOperation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want fuse.Status
	}{
		{"unavailable is EIO, not ENOENT", core.ErrUnavailable, fuse.Status(syscall.EIO)},
		{"not exist", core.ErrNotExist, fuse.Status(syscall.ENOENT)},
		{"not a directory", core.ErrNotDir, fuse.Status(syscall.ENOTDIR)},
		{"is a directory", core.ErrIsDir, fuse.Status(syscall.EISDIR)},
		{"cancelled", context.Canceled, fuse.Status(syscall.EINTR)},
		{"deadline", context.DeadlineExceeded, fuse.Status(syscall.EIO)},
		{"other", errors.New("boom"), fuse.Status(syscall.EIO)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			file := h.lookup(rootID, "hello.txt")
			dir := h.lookup(rootID, "docs")
			h.fake.setFailPath("hello.txt", tt.err)
			h.fake.setFailPath("docs", tt.err)
			h.fake.setFailPath("docs/readme.md", tt.err)

			if _, st := h.getattr(file); st != tt.want {
				t.Errorf("Getattr(file) = %v, want %v", st, tt.want)
			}
			if _, st := h.getattr(dir); st != tt.want {
				t.Errorf("Getattr(dir) = %v, want %v", st, tt.want)
			}
			if _, st := h.readDir(dir); st != tt.want {
				t.Errorf("ReadDir = %v, want %v", st, tt.want)
			}
			if _, _, st := h.open(file, syscall.O_RDONLY); st != tt.want {
				t.Errorf("Open = %v, want %v", st, tt.want)
			}
			if _, _, st := h.lookupStatus(dir, "readme.md"); st != tt.want && tt.want != fuse.Status(syscall.ENOENT) {
				t.Errorf("Lookup = %v, want %v", st, tt.want)
			}
		})
	}
}

func TestUnexpectedErrorIsLoggedOncePerText(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	file := h.lookup(rootID, "hello.txt")
	h.fake.setFailPath("hello.txt", errors.New("boom"))
	for range 3 {
		if _, st := h.getattr(file); st != fuse.Status(syscall.EIO) {
			t.Fatalf("Getattr = %v, want EIO", st)
		}
	}
	if got := bytes.Count(h.log.Bytes(), []byte("boom")); got != 1 {
		t.Fatalf("error text logged %d times, want 1:\n%s", got, h.log.String())
	}
}

func TestGetattrOfRoot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	out, st := h.getattr(rootID)
	if st != fuse.OK {
		t.Fatal(st)
	}
	if out.Mode != syscall.S_IFDIR|0o555 || out.Nlink != 1 || out.Uid != 1000 || out.Gid != 1001 {
		t.Fatalf("root attr = %+v", out.Attr)
	}
	if out.Timeout() != testSettings.attrTimeout {
		t.Fatalf("attr timeout = %v, want %v", out.Timeout(), testSettings.attrTimeout)
	}
}

func TestReadDirListsEntriesInOrderWithTypesAndInodes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entries, st := h.readDir(rootID)
	if st != fuse.OK {
		t.Fatal(st)
	}
	type entry struct {
		name string
		mode uint32
		ino  uint64
	}
	var got []entry
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		got = append(got, entry{e.Name, e.Mode, e.Ino})
	}
	want := []entry{
		{"big.bin", syscall.S_IFREG, fakeInode("big.bin")},
		{"docs", syscall.S_IFDIR, fakeInode("docs")},
		{"empty.txt", syscall.S_IFREG, fakeInode("empty.txt")},
		{"hello.txt", syscall.S_IFREG, fakeInode("hello.txt")},
		{"vacant", syscall.S_IFDIR, fakeInode("vacant")},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadDirOfEmptyDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	entries, st := h.readDir(h.lookup(rootID, "vacant"))
	if st != fuse.OK {
		t.Fatal(st)
	}
	for _, e := range entries {
		if e.Name != "." && e.Name != ".." {
			t.Fatalf("unexpected entry %q", e.Name)
		}
	}
}

func TestOpenReadRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.lookup(rootID, "big.bin")
	fh, out, st := h.open(id, syscall.O_RDONLY)
	if st != fuse.OK {
		t.Fatal(st)
	}
	if out.OpenFlags&fuse.FOPEN_KEEP_CACHE == 0 {
		t.Fatalf("open flags = %#x, want FOPEN_KEEP_CACHE", out.OpenFlags)
	}
	if got := h.fake.open.Load(); got != 1 {
		t.Fatalf("%d files open, want 1", got)
	}

	want := h.fake.files["big.bin"]
	tests := []struct {
		name string
		off  uint64
		size int
		want []byte
	}{
		{"start", 0, 4096, want[:4096]},
		{"unaligned middle", 1234567, 65536, want[1234567 : 1234567+65536]},
		{"short read at the end", bigSize - 100, 4096, want[bigSize-100:]},
		{"at the end", bigSize, 4096, nil},
		{"past the end", bigSize + 10, 4096, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, st := h.read(id, fh, tt.off, tt.size)
			if st != fuse.OK {
				t.Fatalf("Read = %v", st)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("read %d bytes, want %d (content differs)", len(got), len(tt.want))
			}
		})
	}

	h.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(id), Fh: fh})
	if got := h.fake.open.Load(); got != 0 {
		t.Fatalf("%d files still open after Release", got)
	}
}

func TestOpenForWritingIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.lookup(rootID, "hello.txt")
	for _, flags := range []uint32{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_WRONLY | syscall.O_TRUNC} {
		if _, _, st := h.open(id, flags); st != fuse.Status(syscall.EROFS) {
			t.Errorf("Open(flags %#x) = %v, want EROFS", flags, st)
		}
	}
	if got := h.fake.open.Load(); got != 0 {
		t.Fatalf("a refused open left %d files open", got)
	}
}

func TestReadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		fault    readFault
		wantSt   fuse.Status
		wantData string
	}{
		{"error without data", readFault{n: 0, err: core.ErrUnavailable}, fuse.Status(syscall.EIO), ""},
		{"interrupted", readFault{n: 0, err: context.Canceled}, fuse.Status(syscall.EINTR), ""},
		{"error after some data returns the data", readFault{n: 5, err: core.ErrUnavailable}, fuse.OK, "hello"},
		{"end of file with data", readFault{n: 5, err: io.EOF}, fuse.OK, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			id := h.lookup(rootID, "hello.txt")
			fh, _, st := h.open(id, syscall.O_RDONLY)
			if st != fuse.OK {
				t.Fatal(st)
			}
			h.fake.setReadFault(&tt.fault)
			got, st := h.read(id, fh, 0, 4096)
			if st != tt.wantSt || string(got) != tt.wantData {
				t.Fatalf("Read = %q, %v; want %q, %v", got, st, tt.wantData, tt.wantSt)
			}
		})
	}
}

func TestReadReceivesTheRequestContext(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.lookup(rootID, "hello.txt")
	fh, _, st := h.open(id, syscall.O_RDONLY)
	if st != fuse.OK {
		t.Fatal(st)
	}
	h.fake.blockReadsOf("hello.txt")

	cancel := make(chan struct{})
	result := make(chan fuse.Status, 1)
	go func() {
		buf := make([]byte, 16)
		_, st := h.raw.Read(cancel, &fuse.ReadIn{InHeader: *hdr(id), Fh: fh, Size: 16}, buf)
		result <- st
	}()
	<-h.fake.reading
	close(cancel)
	if err := <-h.fake.cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("core saw %v, want context.Canceled", err)
	}
	if st := <-result; st != fuse.Status(syscall.EINTR) {
		t.Fatalf("Read = %v, want EINTR", st)
	}
}

func TestReleaseErrorIsMapped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.lookup(rootID, "hello.txt")
	fh, _, st := h.open(id, syscall.O_RDONLY)
	if st != fuse.OK {
		t.Fatal(st)
	}
	h.fake.setCloseError(core.ErrUnavailable)
	handle := &handle{file: mustOpen(t, h.fake, "hello.txt"), mapper: h.root.env.mapper, files: h.root.env.files}
	if errno := handle.Release(context.Background()); errno != syscall.EIO {
		t.Fatalf("Release = %v, want EIO", errno)
	}
	h.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(id), Fh: fh})
}

func mustOpen(t *testing.T, fsys core.FS, name string) core.File {
	t.Helper()
	f, err := fsys.Open(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOpenFilesAreClosedWhenTheMountEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.lookup(rootID, "hello.txt")
	released, _, st := h.open(id, syscall.O_RDONLY)
	if st != fuse.OK {
		t.Fatal(st)
	}
	for range 2 {
		if _, _, st := h.open(id, syscall.O_RDONLY); st != fuse.OK {
			t.Fatal(st)
		}
	}
	h.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(id), Fh: released})
	if got := h.fake.open.Load(); got != 2 {
		t.Fatalf("%d files open, want the 2 not released", got)
	}

	h.root.env.files.closeAll()
	if got := h.fake.open.Load(); got != 0 {
		t.Fatalf("%d files open after closeAll, want 0", got)
	}
	h.root.env.files.closeAll()
	if got := h.fake.open.Load(); got != 0 {
		t.Fatalf("a second closeAll closed files again: %d open", got)
	}
}

func TestCloseAllLogsUnexpectedCloseErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, _, st := h.open(h.lookup(rootID, "hello.txt"), syscall.O_RDONLY); st != fuse.OK {
		t.Fatal(st)
	}
	h.fake.setCloseError(errors.New("close boom"))
	h.root.env.files.closeAll()
	if !bytes.Contains(h.log.Bytes(), []byte("close boom")) {
		t.Fatalf("close error not logged:\n%s", h.log.String())
	}
}

func TestStatfs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		space   core.Space
		err     error
		want    fuse.StatfsOut
		wantSt  fuse.Status
		failAll error
	}{
		{
			name:  "capacity in 4096 byte blocks",
			space: core.Space{Total: fakeTotal, Free: fakeFree},
			want:  fuse.StatfsOut{Blocks: 1000, Bfree: 250, Bavail: 250, Bsize: 4096, Frsize: 4096, NameLen: 255},
		},
		{
			name:  "unavailable reports zeros instead of failing",
			space: core.Space{Total: fakeTotal, Free: fakeFree},
			err:   core.ErrUnavailable,
			want:  fuse.StatfsOut{Bsize: 4096, Frsize: 4096, NameLen: 255},
		},
		{
			name:   "other errors are I/O errors",
			err:    errors.New("boom"),
			wantSt: fuse.Status(syscall.EIO),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.fake.setSpace(tt.space, tt.err)
			var out fuse.StatfsOut
			st := h.raw.StatFs(nil, hdr(rootID), &out)
			if st != tt.wantSt {
				t.Fatalf("StatFs = %v, want %v", st, tt.wantSt)
			}
			if st == fuse.OK && out != tt.want {
				t.Fatalf("StatFs =\n%+v\nwant\n%+v", out, tt.want)
			}
		})
	}
}

func TestEveryMutationIsRefusedWithEROFS(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	file := h.lookup(rootID, "hello.txt")
	fh, _, st := h.open(file, syscall.O_RDONLY)
	if st != fuse.OK {
		t.Fatal(st)
	}
	defer h.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(file), Fh: fh})
	setattr := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: *hdr(file)}}

	tests := []struct {
		name string
		do   func() fuse.Status
	}{
		{"create", func() fuse.Status {
			return h.raw.Create(nil, &fuse.CreateIn{InHeader: *hdr(rootID)}, "new", &fuse.CreateOut{})
		}},
		{"tmpfile", func() fuse.Status {
			return h.raw.Tmpfile(nil, &fuse.CreateIn{InHeader: *hdr(rootID)}, &fuse.CreateOut{})
		}},
		{"mkdir", func() fuse.Status {
			return h.raw.Mkdir(nil, &fuse.MkdirIn{InHeader: *hdr(rootID)}, "new", &fuse.EntryOut{})
		}},
		{"mknod", func() fuse.Status {
			return h.raw.Mknod(nil, &fuse.MknodIn{InHeader: *hdr(rootID)}, "new", &fuse.EntryOut{})
		}},
		{"unlink", func() fuse.Status { return h.raw.Unlink(nil, hdr(rootID), "hello.txt") }},
		{"rmdir", func() fuse.Status { return h.raw.Rmdir(nil, hdr(rootID), "docs") }},
		{"rename", func() fuse.Status {
			return h.raw.Rename(nil, &fuse.RenameIn{InHeader: *hdr(rootID), Newdir: rootID}, "hello.txt", "renamed")
		}},
		{"link", func() fuse.Status {
			return h.raw.Link(nil, &fuse.LinkIn{InHeader: *hdr(rootID), Oldnodeid: file}, "link", &fuse.EntryOut{})
		}},
		{"symlink", func() fuse.Status {
			return h.raw.Symlink(nil, hdr(rootID), "target", "link", &fuse.EntryOut{})
		}},
		{"setattr of a file", func() fuse.Status { return h.raw.SetAttr(nil, setattr, &fuse.AttrOut{}) }},
		{"setattr of a directory", func() fuse.Status {
			in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: *hdr(rootID)}}
			return h.raw.SetAttr(nil, in, &fuse.AttrOut{})
		}},
		{"write", func() fuse.Status {
			_, st := h.raw.Write(nil, &fuse.WriteIn{InHeader: *hdr(file), Fh: fh}, []byte("x"))
			return st
		}},
		{"writev", func() fuse.Status {
			_, st := h.raw.Writev(nil, &fuse.WriteIn{InHeader: *hdr(file), Fh: fh}, [][]byte{[]byte("x"), []byte("y")})
			return st
		}},
		{"fallocate", func() fuse.Status {
			return h.raw.Fallocate(nil, &fuse.FallocateIn{InHeader: *hdr(file), Fh: fh})
		}},
		{"setxattr", func() fuse.Status {
			return h.raw.SetXAttr(nil, &fuse.SetXAttrIn{InHeader: *hdr(file)}, "user.x", []byte("v"))
		}},
		{"removexattr", func() fuse.Status { return h.raw.RemoveXAttr(nil, hdr(file), "user.x") }},
		{"copy file range", func() fuse.Status {
			_, st := h.raw.CopyFileRange(nil, &fuse.CopyFileRangeIn{
				InHeader: *hdr(file), FhIn: fh, NodeIdOut: file, FhOut: fh, Len: 1,
			})
			return st
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if st := tt.do(); st != fuse.Status(syscall.EROFS) {
				t.Fatalf("%s = %v, want EROFS", tt.name, st)
			}
		})
	}
}

func TestExtendedAttributesAnswerENOSYSSoTheKernelStopsAsking(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	file := h.lookup(rootID, "hello.txt")
	if _, st := h.raw.GetXAttr(nil, hdr(file), "user.x", make([]byte, 16)); st != fuse.Status(syscall.ENOSYS) {
		t.Errorf("GetXAttr = %v, want ENOSYS", st)
	}
	if _, st := h.raw.ListXAttr(nil, hdr(file), make([]byte, 16)); st != fuse.Status(syscall.ENOSYS) {
		t.Errorf("ListXAttr = %v, want ENOSYS", st)
	}
}

func TestMountOptions(t *testing.T) {
	t.Parallel()
	in := mount.Options{Target: "/mnt/x", Source: "splicefs:example/media", AllowOther: true}
	got := mountOptions(in, slog.New(slog.DiscardHandler))
	if got.FsName != "splicefs:example/media" || got.Name != "splicefs" || !got.AllowOther || !got.DirectMount {
		t.Fatalf("mount options = %+v", got)
	}
	if len(got.Options) != 1 || got.Options[0] != "ro" || got.DirectMountFlags&syscall.MS_RDONLY == 0 {
		t.Fatalf("Options = %v, flags %#x; want ro by both routes", got.Options, got.DirectMountFlags)
	}
	in.AllowOther = false
	if mountOptions(in, slog.New(slog.DiscardHandler)).AllowOther {
		t.Fatal("AllowOther set although not asked for")
	}
}

func TestNodeOptionsCarryTheTimeouts(t *testing.T) {
	t.Parallel()
	got := nodeOptions(testSettings, slog.New(slog.DiscardHandler))
	if *got.EntryTimeout != testSettings.entryTimeout || *got.AttrTimeout != testSettings.attrTimeout || *got.NegativeTimeout != testSettings.negativeTimeout {
		t.Fatalf("timeouts = %v %v %v", *got.EntryTimeout, *got.AttrTimeout, *got.NegativeTimeout)
	}
}

func TestLoggerOrDiscard(t *testing.T) {
	t.Parallel()
	if loggerOrDiscard(nil) == nil {
		t.Fatal("nil logger was not replaced")
	}
	l := slog.New(slog.DiscardHandler)
	if loggerOrDiscard(l) != l {
		t.Fatal("given logger was replaced")
	}
}

func TestCurrentOwnerIsTheRunningUser(t *testing.T) {
	t.Parallel()
	o := currentOwner()
	if int(o.uid) != os.Getuid() || int(o.gid) != os.Getgid() {
		t.Fatalf("owner = %+v, want %d:%d", o, os.Getuid(), os.Getgid())
	}
}
