//go:build linux || freebsd

package fuse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

// These tests mount for real and use ordinary system calls on the mount. They need
// /dev/fuse and either root or a setuid fusermount3.

const (
	testSource  = "splicefs:example/media"
	waitTimeout = 20 * time.Second
)

func skipWithoutFuse(t *testing.T) {
	t.Helper()
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		t.Skipf("FUSE is not available: %v", err)
	} else {
		_ = f.Close()
	}
}

// syncBuffer is a log destination safe for the concurrent writers of a mount.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	t.Cleanup(cancel)
	return ctx
}

// mountFake mounts fake on a fresh directory and unmounts it when the test ends.
func mountFake(t *testing.T, fake *fakeFS, s settings, logger *slog.Logger) (string, mount.Mounted) {
	t.Helper()
	skipWithoutFuse(t)
	target := t.TempDir()
	opts := mount.Options{Target: target, Source: testSource, Logger: logger}
	m, err := mountWith(fake, opts, s, newKernelHost())
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() { endMount(t, m) })
	return target, m
}

// endMount detaches a mount, if it is still there, and waits for the serve loop.
func endMount(t *testing.T, m mount.Mounted) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := m.Unmount(ctx, true); err != nil {
		t.Errorf("cleanup unmount: %v", err)
	}
	select {
	case <-m.Done():
	case <-ctx.Done():
		t.Error("mount did not end")
	}
}

func noCache() settings { return settings{} }

func statOf(t *testing.T, name string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t)
}

func TestPublicMountAndCleanUnmount(t *testing.T) {
	skipWithoutFuse(t)
	target := t.TempDir()
	m, err := Mount(newFakeFS(), mount.Options{Target: target, Source: testSource})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { endMount(t, m) })

	got, err := os.ReadFile(filepath.Join(target, "hello.txt"))
	if err != nil || string(got) != fakeContent {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := m.Unmount(testContext(t), false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Done is not closed after Unmount returned")
	}
	if m.Err() != nil {
		t.Fatalf("Err = %v, want nil after a clean unmount", m.Err())
	}
	if isMountPoint(t, target) {
		t.Fatal("target is still a mount point")
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("target after unmount: %v, %v; want the empty directory underneath", entries, err)
	}
}

func TestStatReportsFixedAttributes(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())

	tests := []struct {
		name     string
		wantMode os.FileMode
		wantType uint32
		wantSize int64
	}{
		{"hello.txt", 0o444, syscall.S_IFREG, int64(len(fakeContent))},
		{"big.bin", 0o444, syscall.S_IFREG, bigSize},
		{"docs", 0o555, syscall.S_IFDIR, 0},
		{"docs/sub/deep.txt", 0o444, syscall.S_IFREG, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := os.Stat(filepath.Join(target, tt.name))
			if err != nil {
				t.Fatal(err)
			}
			st := info.Sys().(*syscall.Stat_t)
			if info.Mode().Perm() != tt.wantMode || fileType(st) != tt.wantType {
				t.Errorf("mode = %v (%#o), want %v type %#o", info.Mode(), st.Mode, tt.wantMode, tt.wantType)
			}
			if st.Nlink != 1 || st.Uid != uid || st.Gid != gid {
				t.Errorf("nlink %d owner %d:%d, want 1 and %d:%d", st.Nlink, st.Uid, st.Gid, uid, gid)
			}
			if tt.wantType == syscall.S_IFREG && info.Size() != tt.wantSize {
				t.Errorf("size = %d, want %d", info.Size(), tt.wantSize)
			}
			if want := (info.Size() + 511) / 512; st.Blocks != want {
				t.Errorf("blocks = %d, want %d", st.Blocks, want)
			}
			if st.Ino != fakeInode(tt.name) {
				t.Errorf("inode = %d, want %d", st.Ino, fakeInode(tt.name))
			}
			if !info.ModTime().Equal(fakeMTime) {
				t.Errorf("mtime = %v, want %v", info.ModTime(), fakeMTime)
			}
			atime, ctime := accessTime(st), changeTime(st)
			if !atime.Equal(fakeMTime) || !ctime.Equal(fakeMTime) {
				t.Errorf("atime %v ctime %v, want both %v", atime, ctime, fakeMTime)
			}
		})
	}
}

func TestReadDirThroughTheKernel(t *testing.T) {
	target, _ := mountFake(t, newFakeFS(), defaultSettings(), nil)
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, fmt.Sprintf("%s dir=%v", e.Name(), e.IsDir()))
	}
	want := []string{"big.bin dir=false", "docs dir=true", "empty.txt dir=false", "hello.txt dir=false", "vacant dir=true"}
	if !slices.Equal(got, want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	sub, err := os.ReadDir(filepath.Join(target, "docs", "sub"))
	if err != nil || len(sub) != 1 || sub[0].Name() != "deep.txt" {
		t.Fatalf("docs/sub = %v, %v", sub, err)
	}
	if vacant, err := os.ReadDir(filepath.Join(target, "vacant")); err != nil || len(vacant) != 0 {
		t.Fatalf("vacant = %v, %v", vacant, err)
	}
}

func TestSequentialReadMatchesByteForByte(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)
	got, err := os.ReadFile(filepath.Join(target, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fake.files["big.bin"]) {
		t.Fatalf("content differs (%d bytes read, %d expected)", len(got), len(fake.files["big.bin"]))
	}
	if empty, err := os.ReadFile(filepath.Join(target, "empty.txt")); err != nil || len(empty) != 0 {
		t.Fatalf("empty file = %q, %v", empty, err)
	}
}

// randomReads reads from f at random offsets and sizes and compares with want.
func randomReads(f *os.File, want []byte, seed uint64, count int) error {
	r := rand.New(rand.NewPCG(seed, seed+1))
	for range count {
		off := r.IntN(len(want))
		size := 1 + r.IntN(300_000)
		buf := make([]byte, size)
		n, err := f.ReadAt(buf, int64(off))
		end := min(off+size, len(want))
		switch {
		case err != nil && !errors.Is(err, io.EOF):
			return fmt.Errorf("read %d bytes at %d: %w", size, off, err)
		case n != end-off || !bytes.Equal(buf[:n], want[off:end]):
			return fmt.Errorf("read %d bytes at %d: got %d bytes with different content", size, off, n)
		}
	}
	return nil
}

func TestRandomAndConcurrentReadsMatchByteForByte(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)
	f, err := os.Open(filepath.Join(target, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(f)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = randomReads(f, fake.files["big.bin"], uint64(i)+10, 60) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
}

func TestReadingDoesNotChangeAccessTime(t *testing.T) {
	target, _ := mountFake(t, newFakeFS(), defaultSettings(), nil)
	name := filepath.Join(target, "hello.txt")
	if _, err := os.ReadFile(name); err != nil {
		t.Fatal(err)
	}
	st := statOf(t, name)
	if got := accessTime(st); !got.Equal(fakeMTime) {
		t.Fatalf("atime = %v after a read, want %v", got, fakeMTime)
	}
}

func TestFileDataStaysCachedAcrossOpens(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)
	name := filepath.Join(target, "hello.txt")
	if _, err := os.ReadFile(name); err != nil {
		t.Fatal(err)
	}
	reads := fake.readCalls.Load()
	if reads == 0 {
		t.Fatal("first read did not reach the core")
	}
	got, err := os.ReadFile(name)
	if err != nil || string(got) != fakeContent {
		t.Fatalf("second read = %q, %v", got, err)
	}
	if now := fake.readCalls.Load(); now != reads {
		t.Fatalf("second open reached the core again: %d reads, then %d", reads, now)
	}
}

func TestMissingNamesAreENOENT(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)
	missing := filepath.Join(target, "nope")

	if _, err := os.Stat(missing); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("Stat = %v, want ENOENT", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("second Stat = %v, want ENOENT", err)
	}
	if _, err := os.Stat(filepath.Join(target, "hello.txt", "inside")); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("Stat below a file = %v, want ENOTDIR", err)
	}
}

func TestUnavailableClusterIsEIONotENOENT(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, noCache(), nil)
	fake.setFailAll(core.ErrUnavailable)

	ops := map[string]func() error{
		"stat": func() error { _, err := os.Stat(filepath.Join(target, "hello.txt")); return err },
		"stat of an unknown name": func() error {
			_, err := os.Stat(filepath.Join(target, "never-seen"))
			return err
		},
		"readdir": func() error { _, err := os.ReadDir(target); return err },
		"open":    func() error { _, err := os.ReadFile(filepath.Join(target, "hello.txt")); return err },
	}
	for name, op := range ops {
		err := op()
		if !errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ENOENT) {
			t.Errorf("%s = %v, want EIO and not ENOENT", name, err)
		}
	}

	fake.setFailAll(nil)
	if got, err := os.ReadFile(filepath.Join(target, "hello.txt")); err != nil || string(got) != fakeContent {
		t.Fatalf("after recovery: %q, %v", got, err)
	}
}

func TestUnexpectedCoreErrorIsEIOAndLoggedOnce(t *testing.T) {
	fake := newFakeFS()
	logs := &syncBuffer{}
	target, _ := mountFake(t, fake, noCache(), slog.New(slog.NewTextHandler(logs, nil)))
	fake.setFailPath("hello.txt", errors.New("kaboom from the core"))

	for range 3 {
		if _, err := os.Stat(filepath.Join(target, "hello.txt")); !errors.Is(err, syscall.EIO) {
			t.Fatalf("Stat = %v, want EIO", err)
		}
	}
	if got := strings.Count(logs.String(), "kaboom from the core"); got != 1 {
		t.Fatalf("error text logged %d times, want 1:\n%s", got, logs.String())
	}
}

func TestStatfsReportsTheCapacity(t *testing.T) {
	fake := newFakeFS()
	target, _ := mountFake(t, fake, defaultSettings(), nil)

	var st syscall.Statfs_t
	if err := syscall.Statfs(target, &st); err != nil {
		t.Fatal(err)
	}
	if !isFuseStatfs(&st) || st.Bsize != 4096 || st.Blocks != 1000 || st.Bfree != 250 || st.Bavail != 250 {
		t.Fatalf("statfs = type %#x bsize %d blocks %d free %d avail %d; want FUSE, 4096, 1000, 250, 250",
			st.Type, st.Bsize, st.Blocks, st.Bfree, st.Bavail)
	}

	fake.setSpace(core.Space{}, core.ErrUnavailable)
	if err := syscall.Statfs(target, &st); err != nil {
		t.Fatalf("statfs with the cluster unavailable failed: %v", err)
	}
	if st.Blocks != 0 || st.Bfree != 0 || st.Bavail != 0 || st.Bsize != 4096 {
		t.Fatalf("statfs unavailable = blocks %d free %d avail %d bsize %d; want zeros and 4096", st.Blocks, st.Bfree, st.Bavail, st.Bsize)
	}
}

func TestWritesAreRefusedWithEROFS(t *testing.T) {
	target, _ := mountFake(t, newFakeFS(), defaultSettings(), nil)
	hello := filepath.Join(target, "hello.txt")
	tests := []struct {
		name string
		do   func() error
	}{
		{"create", func() error { f, err := os.Create(filepath.Join(target, "new")); closeQuietly(f); return err }},
		{"open for writing", func() error { f, err := os.OpenFile(hello, os.O_WRONLY, 0); closeQuietly(f); return err }},
		{"open for appending", func() error { f, err := os.OpenFile(hello, os.O_RDWR|os.O_APPEND, 0); closeQuietly(f); return err }},
		{"mkdir", func() error { return os.Mkdir(filepath.Join(target, "dir"), 0o755) }},
		{"unlink", func() error { return os.Remove(hello) }},
		{"rmdir", func() error { return os.Remove(filepath.Join(target, "vacant")) }},
		{"rename", func() error { return os.Rename(hello, filepath.Join(target, "renamed")) }},
		{"chmod", func() error { return os.Chmod(hello, 0o644) }},
		{"chown", func() error { return os.Chown(hello, os.Getuid(), os.Getgid()) }},
		{"utimes", func() error { return os.Chtimes(hello, time.Now(), time.Now()) }},
		{"truncate", func() error { return os.Truncate(hello, 0) }},
		{"symlink", func() error { return os.Symlink("hello.txt", filepath.Join(target, "sym")) }},
		{"link", func() error { return os.Link(hello, filepath.Join(target, "hard")) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.do(); !errors.Is(err, syscall.EROFS) {
				t.Fatalf("%s = %v, want EROFS", tt.name, err)
			}
		})
	}
	if got, err := os.ReadFile(hello); err != nil || string(got) != fakeContent {
		t.Fatalf("file changed: %q, %v", got, err)
	}
}

func closeQuietly(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

func TestLocksAreGrantedLocally(t *testing.T) {
	target, _ := mountFake(t, newFakeFS(), defaultSettings(), nil)
	f, err := os.Open(filepath.Join(target, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(f)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Errorf("flock = %v, want the kernel to grant it locally", err)
	}
	lock := syscall.Flock_t{Type: syscall.F_RDLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
		t.Errorf("fcntl lock = %v, want the kernel to grant it locally", err)
	}
}

func TestInodeNumbersSurviveARemount(t *testing.T) {
	skipWithoutFuse(t)
	names := []string{"", "hello.txt", "docs", "docs/sub/deep.txt", "big.bin"}
	inodes := func() map[string]uint64 {
		fake := newFakeFS()
		target := t.TempDir()
		m, err := mountWith(fake, mount.Options{Target: target, Source: testSource}, defaultSettings(), newKernelHost())
		if err != nil {
			t.Fatal(err)
		}
		defer endMount(t, m)
		got := map[string]uint64{}
		for _, name := range names {
			got[name] = statOf(t, filepath.Join(target, name)).Ino
		}
		if err := m.Unmount(testContext(t), false); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first, second := inodes(), inodes()
	for _, name := range names {
		if first[name] != second[name] {
			t.Errorf("inode of %q changed across a remount: %d, then %d", name, first[name], second[name])
		}
		if name != "" && first[name] != fakeInode(name) {
			t.Errorf("inode of %q = %d, want the core's %d", name, first[name], fakeInode(name))
		}
	}
}

func TestUnmountIsRefusedWhileBusy(t *testing.T) {
	target, m := mountFake(t, newFakeFS(), defaultSettings(), nil)
	f, err := os.Open(filepath.Join(target, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(f)

	if err := m.Unmount(testContext(t), false); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Unmount of a busy mount = %v, want one wrapping EBUSY", err)
	}
	if !isMountPoint(t, target) {
		t.Fatal("refused unmount removed the mount")
	}
	select {
	case <-m.Done():
		t.Fatal("refused unmount ended the mount")
	default:
	}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatal("timed out")
	}
}

func TestUnmountFromOutsideEndsTheMountCleanly(t *testing.T) {
	target, m := mountFake(t, newFakeFS(), defaultSettings(), nil)
	if err := newKernelHost().Unmount(testContext(t), target, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m.Done())
	if m.Err() != nil {
		t.Fatalf("Err = %v, want nil", m.Err())
	}
}

func TestNoGoroutineOutlivesAnEndedMount(t *testing.T) {
	skipWithoutFuse(t)
	target := t.TempDir()
	m, err := mountWith(newFakeFS(), mount.Options{Target: target, Source: testSource}, defaultSettings(), newKernelHost())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(target, "big.bin")); err != nil {
		t.Fatal(err)
	}
	if err := m.Unmount(testContext(t), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m.Done())

	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"go-fuse", "mounted).run", "mounted).unmount"} {
		if strings.Contains(buf.String(), marker) {
			t.Fatalf("goroutine mentioning %q remains after Done:\n%s", marker, buf.String())
		}
	}
}

func TestInvalidTargetsFailBeforeMounting(t *testing.T) {
	skipWithoutFuse(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		target string
		want   error
	}{
		{"missing", filepath.Join(t.TempDir(), "missing"), fs.ErrNotExist},
		{"a file", file, syscall.ENOTDIR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Mount(newFakeFS(), mount.Options{Target: tt.target, Source: testSource})
			if err == nil {
				endMount(t, m)
				t.Fatal("Mount succeeded")
			}
			if m != nil || !errors.Is(err, tt.want) {
				t.Fatalf("Mount = %v, %v; want nil and an error wrapping %v", m, err, tt.want)
			}
		})
	}
}

func TestMountFailureIsReported(t *testing.T) {
	skipWithoutFuse(t)
	// The host claims a directory that does not exist, so the kernel mount itself fails.
	h := &fakeHost{statResults: []statResult{{info: dirInfo(t)}}}
	missing := filepath.Join(t.TempDir(), "missing")
	m, err := mountWith(newFakeFS(), mount.Options{Target: missing, Source: testSource}, defaultSettings(), h)
	if err == nil {
		endMount(t, m)
		t.Fatal("mount of a missing directory succeeded")
	}
	if m != nil {
		t.Fatalf("mount = %v, want nil with an error", m)
	}
}
