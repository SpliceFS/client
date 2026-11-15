//go:build linux || freebsd

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

// testSettings are timeouts that differ from each other and from the defaults, so that a
// test can tell which one a value came from.
func testSettings() settings {
	return settings{
		entryTimeout:    11 * time.Second,
		attrTimeout:     12 * time.Second,
		negativeTimeout: 3 * time.Second,
	}
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
		raw:  fs.NewNodeFS(root, nodeOptions(testSettings(), log)),
		log:  &logs,
	}
}

func hdr(id uint64) *fuse.InHeader { return &fuse.InHeader{NodeId: id} }

func (rig *harness) lookupStatus(parent uint64, name string) (uint64, *fuse.EntryOut, fuse.Status) {
	var out fuse.EntryOut
	status := rig.raw.Lookup(nil, hdr(parent), name, &out)
	return out.NodeId, &out, status
}

// lookup returns the node id of name below parent.
func (rig *harness) lookup(parent uint64, name string) uint64 {
	rig.t.Helper()
	id, _, status := rig.lookupStatus(parent, name)
	if status != fuse.OK || id == 0 {
		rig.t.Fatalf("Lookup(%q) = node %d, %v", name, id, status)
	}
	return id
}

func (rig *harness) getattr(id uint64) (*fuse.AttrOut, fuse.Status) {
	var out fuse.AttrOut
	status := rig.raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: *hdr(id)}, &out)
	return &out, status
}

func (rig *harness) open(id uint64, flags uint32) (uint64, *fuse.OpenOut, fuse.Status) {
	var out fuse.OpenOut
	status := rig.raw.Open(nil, &fuse.OpenIn{InHeader: *hdr(id), Flags: flags}, &out)
	return out.Fh, &out, status
}

func (rig *harness) read(id, fileHandle, off uint64, size int) ([]byte, fuse.Status) {
	buf := make([]byte, size)
	in := &fuse.ReadIn{InHeader: *hdr(id), Fh: fileHandle, Offset: off, Size: uint32(size)}
	res, status := rig.raw.Read(nil, in, buf)
	if status != fuse.OK {
		return nil, status
	}
	data, status := res.Bytes(buf)
	return append([]byte(nil), data...), status
}

// readDir lists a directory, including the entries go-fuse adds for "." and "..".
func (rig *harness) readDir(id uint64) ([]fuse.DirEntry, fuse.Status) {
	var open fuse.OpenOut
	if status := rig.raw.OpenDir(nil, &fuse.OpenIn{InHeader: *hdr(id)}, &open); status != fuse.OK {
		return nil, status
	}
	defer rig.raw.ReleaseDir(&fuse.ReleaseIn{InHeader: *hdr(id), Fh: open.Fh})
	list := fuse.NewDirEntryList(make([]byte, 64<<10), 0)
	in := &fuse.ReadIn{InHeader: *hdr(id), Fh: open.Fh, Size: 64 << 10}
	if status := rig.raw.ReadDir(nil, in, list); status != fuse.OK {
		return nil, status
	}
	entries, err := fuse.ParseDirEntries(list.Bytes())
	if err != nil {
		rig.t.Fatal(err)
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
	rig := newHarness(t)
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
			t.Parallel()
			_, out, status := rig.lookupStatus(rootID, tt.name)
			if status != fuse.OK {
				t.Fatalf("Lookup = %v", status)
			}
			want := fuse.Attr{
				Ino: fakeInode(tt.path), Size: tt.wantSize, Blocks: tt.blocks, Mode: tt.wantMode, Nlink: 1,
				Owner:   fuse.Owner{Uid: 1000, Gid: 1001},
				Atime:   uint64(fakeMTime().Unix()),
				Mtime:   uint64(fakeMTime().Unix()),
				Ctime:   uint64(fakeMTime().Unix()),
				Blksize: blockSize,
			}
			want.Atimensec, want.Mtimensec, want.Ctimensec = 891_000_000, 891_000_000, 891_000_000
			if out.Attr != want {
				t.Fatalf("attr =\n%+v\nwant\n%+v", out.Attr, want)
			}
			if out.EntryTimeout() != testSettings().entryTimeout || out.AttrTimeout() != testSettings().attrTimeout {
				t.Fatalf("timeouts = %v, %v; want %v, %v",
					out.EntryTimeout(), out.AttrTimeout(), testSettings().entryTimeout, testSettings().attrTimeout)
			}
		})
	}
}

func TestLookupBuildsPathsFromTheTree(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	docs := rig.lookup(rootID, "docs")
	sub := rig.lookup(docs, "sub")
	deep := rig.lookup(sub, "deep.txt")
	attr, status := rig.getattr(deep)
	if status != fuse.OK || attr.Ino != fakeInode("docs/sub/deep.txt") {
		t.Fatalf("Getattr(docs/sub/deep.txt) = ino %d, %v; want ino %d", attr.Ino, status, fakeInode("docs/sub/deep.txt"))
	}
}

func TestLookupOfMissingNameIsANegativeEntry(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)

	id, out, status := rig.lookupStatus(rootID, "missing")
	if status != fuse.OK || id != 0 {
		t.Fatalf("Lookup = node %d, %v; want the node id 0 of a negative entry", id, status)
	}
	if out.EntryTimeout() != testSettings().negativeTimeout {
		t.Fatalf("negative entry timeout = %v, want %v", out.EntryTimeout(), testSettings().negativeTimeout)
	}

	var entry fuse.EntryOut
	if _, errno := rig.root.Lookup(context.Background(), "missing", &entry); errno != syscall.ENOENT {
		t.Fatalf("node Lookup = %v, want ENOENT", errno)
	}
}

func TestProbeNameInTheRootIsNotFoundWithoutTheCore(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)

	id, out, status := rig.lookupStatus(rootID, mount.ProbeName)
	if status != fuse.OK || id != 0 {
		t.Fatalf("Lookup = node %d, %v; want the node id 0 of a negative entry", id, status)
	}
	if out.EntryTimeout() != testSettings().negativeTimeout {
		t.Fatalf("negative entry timeout = %v, want %v", out.EntryTimeout(), testSettings().negativeTimeout)
	}
	var entry fuse.EntryOut
	if _, errno := rig.root.Lookup(context.Background(), mount.ProbeName, &entry); errno != syscall.ENOENT {
		t.Fatalf("node Lookup = %v, want ENOENT", errno)
	}
	if n := rig.fake.calls.Load(); n != 0 {
		t.Fatalf("the probe name reached the core %d times, want 0", n)
	}
	entries, status := rig.readDir(rootID)
	if status != fuse.OK {
		t.Fatal(status)
	}
	for _, e := range entries {
		if e.Name == mount.ProbeName {
			t.Fatal("the probe name is listed")
		}
	}
}

func TestProbeNameIsAnsweredWhileACoreCallIsBlocked(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	id := rig.lookup(rootID, "hello.txt")
	fileHandle, _, status := rig.open(id, syscall.O_RDONLY)
	if status != fuse.OK {
		t.Fatal(status)
	}
	rig.fake.blockReadsOf("hello.txt")
	cancel := make(chan struct{})
	result := make(chan fuse.Status, 1)
	go func() {
		buf := make([]byte, 16)
		_, status := rig.raw.Read(cancel, &fuse.ReadIn{InHeader: *hdr(id), Fh: fileHandle, Size: 16}, buf)
		result <- status
	}()
	select {
	case <-rig.fake.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never reached the core")
	}
	before := rig.fake.calls.Load()

	answered := make(chan fuse.Status, 1)
	go func() {
		_, _, status := rig.lookupStatus(rootID, mount.ProbeName)
		answered <- status
	}()
	select {
	case status := <-answered:
		if status != fuse.OK {
			t.Fatalf("Lookup = %v, want a negative entry", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe name waited for the blocked core call")
	}
	if n := rig.fake.calls.Load(); n != before {
		t.Fatalf("the probe name reached the core: %d calls, want %d", n, before)
	}
	close(cancel)
	if err := <-rig.fake.cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("core saw %v, want context.Canceled", err)
	}
	<-result
}

func TestProbeNameInASubdirectoryIsAnOrdinaryName(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	rig.fake.files["docs/"+mount.ProbeName] = []byte("x")
	docs := rig.lookup(rootID, "docs")

	before := rig.fake.calls.Load()
	if id, _, status := rig.lookupStatus(docs, mount.ProbeName); status != fuse.OK || id == 0 {
		t.Fatalf("Lookup of an existing file named like the probe = node %d, %v", id, status)
	}
	if rig.fake.calls.Load() == before {
		t.Fatal("the lookup in a subdirectory did not ask the core")
	}
	delete(rig.fake.files, "docs/"+mount.ProbeName)
	if id, _, status := rig.lookupStatus(docs, mount.ProbeName); status != fuse.OK || id != 0 {
		t.Fatalf("Lookup of a missing file in a subdirectory = node %d, %v", id, status)
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
			assertErrorMappedEverywhere(t, tt.err, tt.want)
		})
	}
}

// assertErrorMappedEverywhere makes the core fail with failure for a file and a
// directory and checks that every operation that reaches the core reports want.
func assertErrorMappedEverywhere(t *testing.T, failure error, want fuse.Status) {
	t.Helper()
	rig := newHarness(t)
	file := rig.lookup(rootID, "hello.txt")
	dir := rig.lookup(rootID, "docs")
	rig.fake.setFailPath("hello.txt", failure)
	rig.fake.setFailPath("docs", failure)
	rig.fake.setFailPath("docs/readme.md", failure)

	if _, status := rig.getattr(file); status != want {
		t.Errorf("Getattr(file) = %v, want %v", status, want)
	}
	if _, status := rig.getattr(dir); status != want {
		t.Errorf("Getattr(dir) = %v, want %v", status, want)
	}
	if _, status := rig.readDir(dir); status != want {
		t.Errorf("ReadDir = %v, want %v", status, want)
	}
	if _, _, status := rig.open(file, syscall.O_RDONLY); status != want {
		t.Errorf("Open = %v, want %v", status, want)
	}
	if _, _, status := rig.lookupStatus(dir, "readme.md"); status != want && want != fuse.Status(syscall.ENOENT) {
		t.Errorf("Lookup = %v, want %v", status, want)
	}
}

func TestUnexpectedErrorIsLoggedOncePerText(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	file := rig.lookup(rootID, "hello.txt")
	rig.fake.setFailPath("hello.txt", errors.New("boom"))
	for range 3 {
		if _, status := rig.getattr(file); status != fuse.Status(syscall.EIO) {
			t.Fatalf("Getattr = %v, want EIO", status)
		}
	}
	if got := bytes.Count(rig.log.Bytes(), []byte("boom")); got != 1 {
		t.Fatalf("error text logged %d times, want 1:\n%s", got, rig.log.String())
	}
}

func TestGetattrOfRoot(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	out, status := rig.getattr(rootID)
	if status != fuse.OK {
		t.Fatal(status)
	}
	if out.Mode != syscall.S_IFDIR|0o555 || out.Nlink != 1 || out.Uid != 1000 || out.Gid != 1001 {
		t.Fatalf("root attr = %+v", out.Attr)
	}
	if out.Timeout() != testSettings().attrTimeout {
		t.Fatalf("attr timeout = %v, want %v", out.Timeout(), testSettings().attrTimeout)
	}
}

func TestReadDirListsEntriesInOrderWithTypesAndInodes(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	entries, status := rig.readDir(rootID)
	if status != fuse.OK {
		t.Fatal(status)
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
	rig := newHarness(t)
	entries, status := rig.readDir(rig.lookup(rootID, "vacant"))
	if status != fuse.OK {
		t.Fatal(status)
	}
	for _, e := range entries {
		if e.Name != "." && e.Name != ".." {
			t.Fatalf("unexpected entry %q", e.Name)
		}
	}
}

//nolint:tparallel,paralleltest // the subtests read through one open file, which the parent releases after them.
func TestOpenReadRelease(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	id := rig.lookup(rootID, "big.bin")
	fileHandle, out, status := rig.open(id, syscall.O_RDONLY)
	if status != fuse.OK {
		t.Fatal(status)
	}
	if out.OpenFlags&fuse.FOPEN_KEEP_CACHE == 0 {
		t.Fatalf("open flags = %#x, want FOPEN_KEEP_CACHE", out.OpenFlags)
	}
	if got := rig.fake.open.Load(); got != 1 {
		t.Fatalf("%d files open, want 1", got)
	}

	want := rig.fake.files["big.bin"]
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
			got, status := rig.read(id, fileHandle, tt.off, tt.size)
			if status != fuse.OK {
				t.Fatalf("Read = %v", status)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("read %d bytes, want %d (content differs)", len(got), len(tt.want))
			}
		})
	}

	rig.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(id), Fh: fileHandle})
	if got := rig.fake.open.Load(); got != 0 {
		t.Fatalf("%d files still open after Release", got)
	}
}

// openFlags opens a file for reading, releases it and returns the flags the kernel was
// given for the open.
func (rig *harness) openFlags(id uint64) uint32 {
	rig.t.Helper()
	fileHandle, out, status := rig.open(id, syscall.O_RDONLY)
	if status != fuse.OK {
		rig.t.Fatal(status)
	}
	rig.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(id), Fh: fileHandle})
	return out.OpenFlags
}

func TestOpenOfTheSameVersionKeepsTheKernelCache(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	rig.fake.setVersion("hello.txt", core.Version{ETag: "a", HeadDigest: "h", TailDigest: "t"})
	id := rig.lookup(rootID, "hello.txt")
	for i := range 3 {
		if got := rig.openFlags(id); got&fuse.FOPEN_KEEP_CACHE == 0 {
			t.Fatalf("open %d of an unchanged file has flags %#x, want FOPEN_KEEP_CACHE", i+1, got)
		}
	}
}

func TestOpenOfAnotherVersionDropsTheKernelCache(t *testing.T) {
	t.Parallel()
	first := core.Version{ETag: "a", HeadDigest: "h", TailDigest: "t"}
	tests := []struct {
		name string
		next core.Version
	}{
		{"tag", core.Version{ETag: "b", HeadDigest: "h", TailDigest: "t"}},
		{"head digest", core.Version{ETag: "a", HeadDigest: "other", TailDigest: "t"}},
		{"tail digest", core.Version{ETag: "a", HeadDigest: "h", TailDigest: "other"}},
		{"digests missing", core.Version{ETag: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newHarness(t)
			rig.fake.setVersion("hello.txt", first)
			id := rig.lookup(rootID, "hello.txt")
			rig.openFlags(id)

			rig.fake.setVersion("hello.txt", tt.next)
			if got := rig.openFlags(id); got != 0 {
				t.Fatalf("open of a replaced file has flags %#x, want none", got)
			}
			if got := rig.openFlags(id); got&fuse.FOPEN_KEEP_CACHE == 0 {
				t.Fatalf("the next open of the new version has flags %#x, want FOPEN_KEEP_CACHE", got)
			}
		})
	}
}

func TestOpenOfAnotherFileIsNotAffectedByAReplacedOne(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	replaced, other := rig.lookup(rootID, "big.bin"), rig.lookup(rootID, "hello.txt")
	rig.openFlags(replaced)
	rig.openFlags(other)

	rig.fake.setVersion("big.bin", core.Version{ETag: "b"})
	if got := rig.openFlags(replaced); got != 0 {
		t.Fatalf("open of the replaced file has flags %#x, want none", got)
	}
	if got := rig.openFlags(other); got&fuse.FOPEN_KEEP_CACHE == 0 {
		t.Fatalf("open of an unchanged file has flags %#x, want FOPEN_KEEP_CACHE", got)
	}
}

func TestOpenForWritingIsRefused(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	id := rig.lookup(rootID, "hello.txt")
	for _, flags := range []uint32{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_WRONLY | syscall.O_TRUNC} {
		if _, _, status := rig.open(id, flags); status != fuse.Status(syscall.EROFS) {
			t.Errorf("Open(flags %#x) = %v, want EROFS", flags, status)
		}
	}
	if got := rig.fake.open.Load(); got != 0 {
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
			rig := newHarness(t)
			id := rig.lookup(rootID, "hello.txt")
			fileHandle, _, status := rig.open(id, syscall.O_RDONLY)
			if status != fuse.OK {
				t.Fatal(status)
			}
			rig.fake.setReadFault(&tt.fault)
			got, status := rig.read(id, fileHandle, 0, 4096)
			if status != tt.wantSt || string(got) != tt.wantData {
				t.Fatalf("Read = %q, %v; want %q, %v", got, status, tt.wantData, tt.wantSt)
			}
		})
	}
}

func TestReadReceivesTheRequestContext(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	id := rig.lookup(rootID, "hello.txt")
	fileHandle, _, status := rig.open(id, syscall.O_RDONLY)
	if status != fuse.OK {
		t.Fatal(status)
	}
	rig.fake.blockReadsOf("hello.txt")

	cancel := make(chan struct{})
	result := make(chan fuse.Status, 1)
	go func() {
		buf := make([]byte, 16)
		_, status := rig.raw.Read(cancel, &fuse.ReadIn{InHeader: *hdr(id), Fh: fileHandle, Size: 16}, buf)
		result <- status
	}()
	<-rig.fake.reading
	close(cancel)
	if err := <-rig.fake.cancelled; !errors.Is(err, context.Canceled) {
		t.Fatalf("core saw %v, want context.Canceled", err)
	}
	if status := <-result; status != fuse.Status(syscall.EINTR) {
		t.Fatalf("Read = %v, want EINTR", status)
	}
}

func TestReleaseErrorIsMapped(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	id := rig.lookup(rootID, "hello.txt")
	fileHandle, _, status := rig.open(id, syscall.O_RDONLY)
	if status != fuse.OK {
		t.Fatal(status)
	}
	rig.fake.setCloseError(core.ErrUnavailable)
	handle := &handle{file: mustOpen(t, rig.fake, "hello.txt"), mapper: rig.root.env.mapper, files: rig.root.env.files}
	if errno := handle.Release(context.Background()); errno != syscall.EIO {
		t.Fatalf("Release = %v, want EIO", errno)
	}
	rig.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(id), Fh: fileHandle})
}

//nolint:ireturn // core.File is what core.FS.Open returns.
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
	rig := newHarness(t)
	id := rig.lookup(rootID, "hello.txt")
	released, _, status := rig.open(id, syscall.O_RDONLY)
	if status != fuse.OK {
		t.Fatal(status)
	}
	for range 2 {
		if _, _, status := rig.open(id, syscall.O_RDONLY); status != fuse.OK {
			t.Fatal(status)
		}
	}
	rig.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(id), Fh: released})
	if got := rig.fake.open.Load(); got != 2 {
		t.Fatalf("%d files open, want the 2 not released", got)
	}

	rig.root.env.files.closeAll()
	if got := rig.fake.open.Load(); got != 0 {
		t.Fatalf("%d files open after closeAll, want 0", got)
	}
	rig.root.env.files.closeAll()
	if got := rig.fake.open.Load(); got != 0 {
		t.Fatalf("a second closeAll closed files again: %d open", got)
	}
}

func TestCloseAllLogsUnexpectedCloseErrors(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	if _, _, status := rig.open(rig.lookup(rootID, "hello.txt"), syscall.O_RDONLY); status != fuse.OK {
		t.Fatal(status)
	}
	rig.fake.setCloseError(errors.New("close boom"))
	rig.root.env.files.closeAll()
	if !bytes.Contains(rig.log.Bytes(), []byte("close boom")) {
		t.Fatalf("close error not logged:\n%s", rig.log.String())
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
			rig := newHarness(t)
			rig.fake.setSpace(tt.space, tt.err)
			var out fuse.StatfsOut
			status := rig.raw.StatFs(nil, hdr(rootID), &out)
			if status != tt.wantSt {
				t.Fatalf("StatFs = %v, want %v", status, tt.wantSt)
			}
			if status == fuse.OK && out != tt.want {
				t.Fatalf("StatFs =\n%+v\nwant\n%+v", out, tt.want)
			}
		})
	}
}

//nolint:tparallel,paralleltest // the subtests use one open file, which the parent releases after them.
func TestEveryMutationIsRefusedWithEROFS(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	file := rig.lookup(rootID, "hello.txt")
	fileHandle, _, status := rig.open(file, syscall.O_RDONLY)
	if status != fuse.OK {
		t.Fatal(status)
	}
	defer rig.raw.Release(nil, &fuse.ReleaseIn{InHeader: *hdr(file), Fh: fileHandle})
	setattr := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: *hdr(file)}}

	tests := []struct {
		name string
		do   func() fuse.Status
	}{
		{"create", func() fuse.Status {
			return rig.raw.Create(nil, &fuse.CreateIn{InHeader: *hdr(rootID)}, "new", &fuse.CreateOut{})
		}},
		{"tmpfile", func() fuse.Status {
			return rig.raw.Tmpfile(nil, &fuse.CreateIn{InHeader: *hdr(rootID)}, &fuse.CreateOut{})
		}},
		{"mkdir", func() fuse.Status {
			return rig.raw.Mkdir(nil, &fuse.MkdirIn{InHeader: *hdr(rootID)}, "new", &fuse.EntryOut{})
		}},
		{"mknod", func() fuse.Status {
			return rig.raw.Mknod(nil, &fuse.MknodIn{InHeader: *hdr(rootID)}, "new", &fuse.EntryOut{})
		}},
		{"unlink", func() fuse.Status { return rig.raw.Unlink(nil, hdr(rootID), "hello.txt") }},
		{"rmdir", func() fuse.Status { return rig.raw.Rmdir(nil, hdr(rootID), "docs") }},
		{"rename", func() fuse.Status {
			return rig.raw.Rename(nil, &fuse.RenameIn{InHeader: *hdr(rootID), Newdir: rootID}, "hello.txt", "renamed")
		}},
		{"link", func() fuse.Status {
			return rig.raw.Link(nil, &fuse.LinkIn{InHeader: *hdr(rootID), Oldnodeid: file}, "link", &fuse.EntryOut{})
		}},
		{"symlink", func() fuse.Status {
			return rig.raw.Symlink(nil, hdr(rootID), "target", "link", &fuse.EntryOut{})
		}},
		{"setattr of a file", func() fuse.Status { return rig.raw.SetAttr(nil, setattr, &fuse.AttrOut{}) }},
		{"setattr of a directory", func() fuse.Status {
			dirAttr := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{InHeader: *hdr(rootID)}}
			return rig.raw.SetAttr(nil, dirAttr, &fuse.AttrOut{})
		}},
		{"write", func() fuse.Status {
			_, status := rig.raw.Write(nil, &fuse.WriteIn{InHeader: *hdr(file), Fh: fileHandle}, []byte("x"))
			return status
		}},
		{"writev", func() fuse.Status {
			in := &fuse.WriteIn{InHeader: *hdr(file), Fh: fileHandle}
			_, status := rig.raw.Writev(nil, in, [][]byte{[]byte("x"), []byte("y")})
			return status
		}},
		{"fallocate", func() fuse.Status {
			return rig.raw.Fallocate(nil, &fuse.FallocateIn{InHeader: *hdr(file), Fh: fileHandle})
		}},
		{"setxattr", func() fuse.Status {
			return rig.raw.SetXAttr(nil, &fuse.SetXAttrIn{InHeader: *hdr(file)}, "user.x", []byte("v"))
		}},
		{"removexattr", func() fuse.Status { return rig.raw.RemoveXAttr(nil, hdr(file), "user.x") }},
		{"copy file range", func() fuse.Status {
			_, status := rig.raw.CopyFileRange(nil, &fuse.CopyFileRangeIn{
				InHeader: *hdr(file), FhIn: fileHandle, NodeIdOut: file, FhOut: fileHandle, Len: 1,
			})
			return status
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status := tt.do(); status != fuse.Status(syscall.EROFS) {
				t.Fatalf("%s = %v, want EROFS", tt.name, status)
			}
		})
	}
}

func TestExtendedAttributesAnswerENOSYSSoTheKernelStopsAsking(t *testing.T) {
	t.Parallel()
	rig := newHarness(t)
	file := rig.lookup(rootID, "hello.txt")
	if _, status := rig.raw.GetXAttr(nil, hdr(file), "user.x", make([]byte, 16)); status != fuse.Status(syscall.ENOSYS) {
		t.Errorf("GetXAttr = %v, want ENOSYS", status)
	}
	if _, status := rig.raw.ListXAttr(nil, hdr(file), make([]byte, 16)); status != fuse.Status(syscall.ENOSYS) {
		t.Errorf("ListXAttr = %v, want ENOSYS", status)
	}
}

func TestMountOptions(t *testing.T) {
	t.Parallel()
	options := mount.Options{Target: "/mnt/x", Source: "splicefs:example/media", AllowOther: true}
	got := mountOptions(options, slog.New(slog.DiscardHandler))
	if got.FsName != "splicefs:example/media" || got.Name != "splicefs" || !got.AllowOther {
		t.Fatalf("mount options = %+v", got)
	}
	// Every system mounts read-only; what a system adds is checked options its own test.
	if len(got.Options) == 0 || got.Options[0] != "ro" {
		t.Fatalf("Options = %v, want ro first", got.Options)
	}
	options.AllowOther = false
	if mountOptions(options, slog.New(slog.DiscardHandler)).AllowOther {
		t.Fatal("AllowOther set although not asked for")
	}
}

func TestNodeOptionsCarryTheTimeouts(t *testing.T) {
	t.Parallel()
	got := nodeOptions(testSettings(), slog.New(slog.DiscardHandler))
	want := testSettings()
	if *got.EntryTimeout != want.entryTimeout || *got.AttrTimeout != want.attrTimeout ||
		*got.NegativeTimeout != want.negativeTimeout {
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
