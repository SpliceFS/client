package nfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs/file"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

var testOwner = owner{uid: 501, gid: 20}

func newTestBilly(t *testing.T, fsys core.FS) *billyFS {
	t.Helper()
	return newBillyFS(t.Context(), fsys, time.Minute, testOwner, slog.New(slog.DiscardHandler))
}

func treeFS() *fakeFS {
	return newFakeFS().
		addFile("dir/hello.txt", []byte("hello world")).
		addDir("empty")
}

func TestBillyCapabilities(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	if billy.CapabilityCheck(b, billy.WriteCapability) {
		t.Error("write capability is reported; go-nfs would not answer NFS3ERR_ROFS")
	}
	if !billy.CapabilityCheck(b, billy.ReadCapability) {
		t.Error("read capability is missing")
	}
}

func TestBillyJoin(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	tests := []struct {
		elem []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b", "c.txt"}, "a/b/c.txt"},
		{[]string{"a", "..", "b"}, "b"},
	}
	for _, tt := range tests {
		if got := b.Join(tt.elem...); got != tt.want {
			t.Errorf("Join(%q) = %q, want %q", tt.elem, got, tt.want)
		}
	}
}

func TestBillyStat(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	b := newTestBilly(t, fsys)
	inode := func(name string) uint64 {
		n, _ := fsys.node(name)
		return n.attr.Inode
	}
	tests := []struct {
		name     string
		path     string
		wantName string
		isDir    bool
		size     int64
		mode     fs.FileMode
		inode    uint64
	}{
		{"file", "dir/hello.txt", "hello.txt", false, 11, 0o444, inode("dir/hello.txt")},
		{"directory", "dir", "dir", true, 0, fs.ModeDir | 0o555, inode("dir")},
		{"root", "", "", true, 0, fs.ModeDir | 0o555, 1},
		{"root as dot", ".", "", true, 0, fs.ModeDir | 0o555, 1},
	}
	for _, tt := range tests {
		for _, stat := range []func(string) (os.FileInfo, error){b.Stat, b.Lstat} {
			info, err := stat(tt.path)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if info.Name() != tt.wantName || info.IsDir() != tt.isDir || info.Size() != tt.size ||
				info.Mode() != tt.mode || !info.ModTime().Equal(fakeMTime) {
				t.Errorf("%s: got name %q dir %v size %d mode %v mtime %v", tt.name,
					info.Name(), info.IsDir(), info.Size(), info.Mode(), info.ModTime())
			}
			want := file.FileInfo{Nlink: 1, UID: 501, GID: 20, Fileid: tt.inode}
			if got := info.Sys(); got != want {
				t.Errorf("%s: Sys() = %+v, want %+v", tt.name, got, want)
			}
		}
	}
}

func TestBillyRejectsPathsOutsideTheRoot(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	b := newTestBilly(t, fsys)
	for _, name := range []string{"..", "../dir", "/dir", "dir/../../x"} {
		if _, err := b.Stat(name); !os.IsNotExist(err) {
			t.Errorf("Stat(%q) = %v, want not-exist", name, err)
		}
		if _, err := b.ReadDir(name); !os.IsNotExist(err) {
			t.Errorf("ReadDir(%q) = %v, want not-exist", name, err)
		}
		if _, err := b.Open(name); !os.IsNotExist(err) {
			t.Errorf("Open(%q) = %v, want not-exist", name, err)
		}
	}
	if len(fsys.calls) != 0 {
		t.Errorf("core.FS was called for an invalid path: %v", fsys.calls)
	}
}

// rootStats counts the calls the core got for the attributes of the root.
func rootStats(f *fakeFS) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == "stat:" {
			n++
		}
	}
	return n
}

// failRoot makes every stat of the root fail with err, or work again when err is nil.
func failRoot(f *fakeFS, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = func(op, name string) error {
		if op == "stat" && name == "" {
			return err
		}
		return nil
	}
}

// pendingRoot returns the channel that is closed when the running root call ends.
func pendingRoot(t *testing.T, b *billyFS) chan struct{} {
	t.Helper()
	b.root.mu.Lock()
	defer b.root.mu.Unlock()
	if b.root.pending == nil {
		t.Fatal("no root call is running")
	}
	return b.root.pending
}

func wantRootDir(t *testing.T, b *billyFS, wantMTime time.Time) {
	t.Helper()
	info, err := b.Stat("")
	if err != nil {
		t.Errorf("Stat of the root = %v, want an answer", err)
		return
	}
	want := file.FileInfo{Nlink: 1, UID: 501, GID: 20, Fileid: rootInode}
	if !info.IsDir() || info.Mode() != fs.ModeDir|0o555 || !info.ModTime().Equal(wantMTime) ||
		info.Sys() != want {
		t.Errorf("root = dir %v mode %v mtime %v sys %+v, want a 0555 directory, mtime %v, sys %+v",
			info.IsDir(), info.Mode(), info.ModTime(), info.Sys(), wantMTime, want)
	}
}

// blockRoot makes the first stat of the root wait until release is closed. started is
// closed when it has begun.
func blockRoot(f *fakeFS) (started <-chan struct{}, release func()) {
	s, r := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = func(op, name string) error {
		if op == "stat" && name == "" {
			once.Do(func() { close(s) })
			<-r
		}
		return nil
	}
	return s, sync.OnceFunc(func() { close(r) })
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestBillyRootAttrFollowsAHealthyCore(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	b := newTestBilly(t, fsys)
	wantRootDir(t, b, fakeMTime)
	later := fakeMTime.Add(time.Hour)
	fsys.mu.Lock()
	fsys.nodes[""].attr.MTime = later
	fsys.mu.Unlock()
	wantRootDir(t, b, later)
	if n := rootStats(fsys); n != 2 {
		t.Errorf("the core was asked %d times, want once per request", n)
	}
}

func TestBillyRootAttrWithAFailingCoreAndNoAnswerYetIsFixed(t *testing.T) {
	t.Parallel()
	for _, err := range []error{core.ErrUnavailable, errors.New("disk on fire"), core.ErrNotExist} {
		fsys := treeFS()
		failRoot(fsys, err)
		b := newTestBilly(t, fsys)
		b.created = time.Unix(1_600_000_000, 0)
		wantRootDir(t, b, b.created)
		wantRootDir(t, b, b.created)
		if info, err := b.Lstat("."); err != nil || !info.ModTime().Equal(b.created) {
			t.Errorf("Lstat of the root = %v, %v", info, err)
		}
	}
}

func TestBillyRootAttrAfterAFailureIsTheLastAnswer(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	b := newTestBilly(t, fsys)
	b.created = time.Unix(1_600_000_000, 0)
	wantRootDir(t, b, fakeMTime)
	failRoot(fsys, core.ErrUnavailable)
	wantRootDir(t, b, fakeMTime)
	failRoot(fsys, nil)
	fsys.mu.Lock()
	fsys.nodes[""].attr.MTime = fakeMTime.Add(time.Minute)
	fsys.mu.Unlock()
	wantRootDir(t, b, fakeMTime.Add(time.Minute))
}

func TestBillyRootAttrOtherPathsStillFail(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	fsys.fail = func(op, _ string) error {
		if op == "stat" || op == "readdir" {
			return core.ErrUnavailable
		}
		return nil
	}
	b := newTestBilly(t, fsys)
	if _, err := b.Stat("dir"); !errors.Is(err, syscall.EIO) {
		t.Errorf("Stat of a directory = %v, want an I/O error", err)
	}
	if _, err := b.ReadDir(""); !errors.Is(err, syscall.EIO) {
		t.Errorf("ReadDir of the root = %v, want an I/O error", err)
	}
}

func TestBillyRootAttrAnswersWithinTheBudgetAndKeepsTheLateAnswer(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	started, release := blockRoot(fsys)
	t.Cleanup(release)
	b := newTestBilly(t, fsys)
	b.rootBudget = 10 * time.Millisecond
	b.created = time.Unix(1_600_000_000, 0)

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		wantRootDir(t, b, b.created)
	}()
	waitFor(t, started, "the core call to start")
	waitFor(t, answered, "an answer while the core is blocked")
	pending := pendingRoot(t, b)

	late := fakeMTime.Add(time.Hour)
	fsys.mu.Lock()
	fsys.nodes[""].attr.MTime = late
	fsys.mu.Unlock()
	release()
	waitFor(t, pending, "the late call to end")

	// The core fails from now on, so what comes back can only be the late answer.
	failRoot(fsys, core.ErrUnavailable)
	wantRootDir(t, b, late)
}

func TestBillyRootAttrNeverRunsTwoCallsAtOnce(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	started, release := blockRoot(fsys)
	t.Cleanup(release)
	b := newTestBilly(t, fsys)
	b.rootBudget = 10 * time.Millisecond
	b.created = time.Unix(1_600_000_000, 0)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { wantRootDir(t, b, b.created) })
	}
	waitFor(t, started, "the core call to start")
	wg.Wait()
	wantRootDir(t, b, b.created)
	if n := rootStats(fsys); n != 1 {
		t.Errorf("the core got %d root calls while the first was running, want 1", n)
	}

	release()
	waitFor(t, pendingRoot(t, b), "the call to end")
	wantRootDir(t, b, fakeMTime)
	if n := rootStats(fsys); n != 2 {
		t.Errorf("the core got %d root calls after the first ended, want 2", n)
	}
}

func TestBillyRootAttrLogsAFailure(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	failRoot(fsys, core.ErrUnavailable)
	var logged bytes.Buffer
	b := newTestBilly(t, fsys)
	b.log = slog.New(slog.NewTextHandler(&logged, nil))
	wantRootDir(t, b, b.created)
	if !strings.Contains(logged.String(), core.ErrUnavailable.Error()) {
		t.Errorf("the failure was not logged: %q", logged.String())
	}
}

func TestBillyProbeNameInTheRootIsNotFoundWithoutTheCore(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	b := newTestBilly(t, fsys)
	name := mount.ProbeName
	if _, err := b.Stat(name); !os.IsNotExist(err) {
		t.Errorf("Stat(%q) = %v, want not-exist", name, err)
	}
	if _, err := b.Lstat(name); !os.IsNotExist(err) {
		t.Errorf("Lstat(%q) = %v, want not-exist", name, err)
	}
	if _, err := b.ReadDir(name); !os.IsNotExist(err) {
		t.Errorf("ReadDir(%q) = %v, want not-exist", name, err)
	}
	if _, err := b.Open(name); !os.IsNotExist(err) {
		t.Errorf("Open(%q) = %v, want not-exist", name, err)
	}
	if len(fsys.calls) != 0 {
		t.Errorf("core.FS was called for the probe name: %v", fsys.calls)
	}
}

func TestBillyProbeNameIsAnsweredWhileACoreCallIsBlocked(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	started, release := make(chan struct{}), make(chan struct{})
	fsys.fail = func(op, name string) error {
		if op == "stat" && name == "dir/hello.txt" {
			close(started)
			<-release
		}
		return nil
	}
	b := newTestBilly(t, fsys)
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		_, _ = b.Stat("dir/hello.txt")
	}()
	t.Cleanup(func() { close(release); <-blocked })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stat never reached the core")
	}

	answered := make(chan error, 1)
	go func() {
		_, err := b.Lstat(mount.ProbeName)
		answered <- err
	}()
	select {
	case err := <-answered:
		if !os.IsNotExist(err) {
			t.Fatalf("Lstat = %v, want not-exist", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe name waited for the blocked core call")
	}
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if len(fsys.calls) != 1 {
		t.Fatalf("core calls = %v, want only the blocked stat", fsys.calls)
	}
}

func TestBillyProbeNameInASubdirectoryIsAnOrdinaryName(t *testing.T) {
	t.Parallel()
	fsys := treeFS().addFile("dir/"+mount.ProbeName, []byte("x"))
	b := newTestBilly(t, fsys)
	if _, err := b.Stat("dir/" + mount.ProbeName); err != nil {
		t.Fatalf("Stat of an existing file named like the probe = %v", err)
	}
	if _, err := b.Stat("empty/" + mount.ProbeName); !os.IsNotExist(err) {
		t.Fatalf("Stat of a missing file = %v, want not-exist", err)
	}
	if want := []string{"stat:dir/" + mount.ProbeName, "stat:empty/" + mount.ProbeName}; !slices.Equal(fsys.calls, want) {
		t.Fatalf("core calls = %v, want %v", fsys.calls, want)
	}
}

func TestBillyErrorMapping(t *testing.T) {
	t.Parallel()
	other := errors.New("disk on fire")
	tests := []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{"not exist", core.ErrNotExist, os.IsNotExist},
		{"not a directory", core.ErrNotDir, func(e error) bool { return errors.Is(e, syscall.ENOTDIR) }},
		{"is a directory", core.ErrIsDir, func(e error) bool { return errors.Is(e, syscall.EISDIR) }},
		{"unavailable", core.ErrUnavailable, func(e error) bool {
			return errors.Is(e, syscall.EIO) && errors.Is(e, core.ErrUnavailable) && !os.IsNotExist(e)
		}},
		{"anything else", other, func(e error) bool {
			return errors.Is(e, syscall.EIO) && errors.Is(e, other) && !os.IsNotExist(e)
		}},
		{"wrapped not exist", errors.Join(errors.New("node a"), core.ErrNotExist), os.IsNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := treeFS()
			fsys.fail = func(string, string) error { return tt.err }
			b := newTestBilly(t, fsys)
			calls := map[string]func() error{
				"stat":    func() error { _, err := b.Stat("dir"); return err },
				"readdir": func() error { _, err := b.ReadDir("dir"); return err },
				"open":    func() error { _, err := b.Open("dir/hello.txt"); return err },
				"space":   func() error { _, err := b.space(t.Context()); return err },
			}
			for op, call := range calls {
				if err := call(); err == nil || !tt.check(err) {
					t.Errorf("%s: error %v does not satisfy the expectation", op, err)
				}
			}
		})
	}
}

func TestBillyReadDir(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	infos, err := b.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range infos {
		names = append(names, i.Name())
	}
	if len(names) != 2 || names[0] != "dir" || names[1] != "empty" {
		t.Errorf("names = %q", names)
	}
	if !infos[0].IsDir() || infos[0].Mode() != fs.ModeDir|0o555 {
		t.Errorf("entry is %v", infos[0].Mode())
	}
}

func TestBillyOpenAndRead(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	f, err := b.Open("dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name() != "dir/hello.txt" {
		t.Errorf("Name() = %q", f.Name())
	}
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 6); n != 5 || err != nil || string(buf) != "world" {
		t.Errorf("ReadAt = %d, %v, %q", n, err, buf)
	}
	// io.EOF must come through unchanged: go-nfs compares against it.
	if n, err := f.ReadAt(buf, 8); n != 3 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v; want 3, io.EOF", n, err)
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
	if err := f.Lock(); err != nil {
		t.Error(err)
	}
	if err := f.Unlock(); err != nil {
		t.Error(err)
	}
}

func TestBillyOpenErrors(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	if _, err := b.Open("dir"); !errors.Is(err, syscall.EISDIR) {
		t.Errorf("Open(dir) = %v, want EISDIR", err)
	}
	if _, err := b.Open("missing"); !os.IsNotExist(err) {
		t.Errorf("Open(missing) = %v, want not-exist", err)
	}
}

func TestBillyFileReadFailureAndCloseFailure(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	b := newTestBilly(t, fsys)
	f, err := b.Open("dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	fsys.fail = func(op, _ string) error {
		if op == "read" || op == "close" {
			return core.ErrUnavailable
		}
		return nil
	}
	if _, err := f.ReadAt(make([]byte, 4), 0); !errors.Is(err, syscall.EIO) {
		t.Errorf("ReadAt = %v, want EIO", err)
	}
	if err := f.Close(); !errors.Is(err, syscall.EIO) {
		t.Errorf("Close = %v, want EIO", err)
	}
}

func TestBillyFileSequentialReadAndSeek(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	f, err := b.Open("dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, err := f.Read(buf); n != 5 || err != nil || string(buf) != "hello" {
		t.Fatalf("first Read = %d, %v, %q", n, err, buf)
	}
	if n, err := f.Read(buf); n != 5 || err != nil || string(buf) != " worl" {
		t.Fatalf("second Read = %d, %v, %q", n, err, buf)
	}
	tests := []struct {
		name    string
		offset  int64
		whence  int
		want    int64
		wantErr bool
	}{
		{"from start", 3, io.SeekStart, 3, false},
		{"from current", 2, io.SeekCurrent, 5, false},
		{"from end", -4, io.SeekEnd, 7, false},
		{"before the start", -1, io.SeekStart, 0, true},
		{"bad whence", 0, 99, 0, true},
	}
	for _, tt := range tests {
		got, err := f.Seek(tt.offset, tt.whence)
		if (err != nil) != tt.wantErr || (err == nil && got != tt.want) {
			t.Errorf("%s: Seek = %d, %v", tt.name, got, err)
		}
	}
	if _, err := f.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Read(buf); n != 4 || !errors.Is(err, io.EOF) || string(buf[:n]) != "orld" {
		t.Errorf("Read to the end = %d, %v, %q", n, err, buf[:n])
	}
}

func TestBillyMutationsAreRefused(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	f, err := b.Open("dir/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]func() error{
		"Create":    func() error { _, err := b.Create("x"); return err },
		"Rename":    func() error { return b.Rename("dir/hello.txt", "y") },
		"Remove":    func() error { return b.Remove("dir/hello.txt") },
		"TempFile":  func() error { _, err := b.TempFile("dir", "t"); return err },
		"MkdirAll":  func() error { return b.MkdirAll("a/b", 0o755) },
		"Symlink":   func() error { return b.Symlink("x", "y") },
		"Write":     func() error { _, err := f.Write([]byte("x")); return err },
		"Truncate":  func() error { return f.Truncate(0) },
		"O_WRONLY":  func() error { _, err := b.OpenFile("dir/hello.txt", os.O_WRONLY, 0); return err },
		"O_RDWR":    func() error { _, err := b.OpenFile("dir/hello.txt", os.O_RDWR, 0); return err },
		"O_APPEND":  func() error { _, err := b.OpenFile("dir/hello.txt", os.O_APPEND, 0); return err },
		"O_CREATE":  func() error { _, err := b.OpenFile("new", os.O_CREATE, 0o644); return err },
		"O_TRUNC":   func() error { _, err := b.OpenFile("dir/hello.txt", os.O_TRUNC, 0); return err },
		"O_RDONLY+": func() error { _, err := b.OpenFile("dir/hello.txt", os.O_RDONLY|os.O_CREATE, 0); return err },
	}
	for name, call := range refused {
		if err := call(); !errors.Is(err, billy.ErrReadOnly) {
			t.Errorf("%s = %v, want a read-only error", name, err)
		}
	}
}

func TestBillyOpenFileForReading(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	f, err := b.OpenFile("dir/hello.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
}

func TestBillyReadlink(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	if _, err := b.Readlink("dir/hello.txt"); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("Readlink(file) = %v, want EINVAL", err)
	}
	if _, err := b.Readlink("missing"); !os.IsNotExist(err) {
		t.Errorf("Readlink(missing) = %v, want not-exist", err)
	}
}

func TestBillyChrootAndRoot(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	if _, err := b.Chroot("dir"); !errors.Is(err, billy.ErrNotSupported) {
		t.Errorf("Chroot = %v, want ErrNotSupported", err)
	}
	if b.Root() != "/" {
		t.Errorf("Root() = %q", b.Root())
	}
}

func TestBillySpace(t *testing.T) {
	t.Parallel()
	b := newTestBilly(t, treeFS())
	got, err := b.space(t.Context())
	if err != nil || got != (core.Space{Total: 1000, Free: 400}) {
		t.Errorf("space = %+v, %v", got, err)
	}
}

// waitingFS blocks every call until its context ends.
type waitingFS struct{ core.FS }

func (waitingFS) Stat(ctx context.Context, _ string) (core.Attr, error) {
	<-ctx.Done()
	return core.Attr{}, ctx.Err()
}

func TestBillyCallsAreBoundedByTimeoutAndMountLifetime(t *testing.T) {
	t.Parallel()
	t.Run("time limit", func(t *testing.T) {
		t.Parallel()
		b := newTestBilly(t, waitingFS{})
		b.timeout = time.Millisecond
		if _, err := b.Stat("x"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Stat = %v, want a deadline error", err)
		}
	})
	t.Run("mount ended", func(t *testing.T) {
		t.Parallel()
		base, end := context.WithCancel(t.Context())
		b := newTestBilly(t, waitingFS{})
		b.base = base
		end()
		if _, err := b.Stat("x"); !errors.Is(err, context.Canceled) {
			t.Errorf("Stat = %v, want a cancellation error", err)
		}
	})
}
