package cache

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errInjected = errors.New("injected failure")

var epoch = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncBuffer is a log sink that is safe for concurrent use.
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

// faultFS is the real file system with failures injected by fail, which is called with
// the operation ("mkdir", "readdir", "readfile", "open", "remove", "rename", "readat",
// "writeat", "sync", "close") and the file name.
type faultFS struct {
	osFS
	fail func(op, name string) error
}

// failOn fails every operation op on a file whose name ends with suffix.
func failOn(op, suffix string) func(string, string) error {
	return func(gotOp, name string) error {
		if gotOp == op && strings.HasSuffix(name, suffix) {
			return errInjected
		}
		return nil
	}
}

func (f faultFS) check(op, name string) error {
	if f.fail == nil {
		return nil
	}
	return f.fail(op, name)
}

func (f faultFS) MkdirAll(path string) error {
	if err := f.check("mkdir", path); err != nil {
		return err
	}
	return f.osFS.MkdirAll(path)
}

func (f faultFS) ReadDir(name string) ([]os.DirEntry, error) {
	if err := f.check("readdir", name); err != nil {
		return nil, err
	}
	return f.osFS.ReadDir(name)
}

func (f faultFS) ReadFile(name string) ([]byte, error) {
	if err := f.check("readfile", name); err != nil {
		return nil, err
	}
	return f.osFS.ReadFile(name)
}

func (f faultFS) OpenFile(name string, flag int) (dataFile, error) {
	if err := f.check("open", name); err != nil {
		return nil, err
	}
	file, err := f.osFS.OpenFile(name, flag)
	if err != nil {
		return nil, err
	}
	return faultFile{dataFile: file, name: name, fs: f}, nil
}

func (f faultFS) Remove(name string) error {
	if err := f.check("remove", name); err != nil {
		return err
	}
	return f.osFS.Remove(name)
}

func (f faultFS) Rename(oldPath, newPath string) error {
	if err := f.check("rename", newPath); err != nil {
		return err
	}
	return f.osFS.Rename(oldPath, newPath)
}

type faultFile struct {
	dataFile
	name string
	fs   faultFS
}

func (f faultFile) ReadAt(p []byte, off int64) (int, error) {
	if err := f.fs.check("readat", f.name); err != nil {
		return 0, err
	}
	return f.dataFile.ReadAt(p, off)
}

func (f faultFile) WriteAt(p []byte, off int64) (int, error) {
	if err := f.fs.check("writeat", f.name); err != nil {
		return 0, err
	}
	return f.dataFile.WriteAt(p, off)
}

func (f faultFile) Sync() error {
	if err := f.fs.check("sync", f.name); err != nil {
		return err
	}
	return f.dataFile.Sync()
}

func (f faultFile) Close() error {
	err := f.dataFile.Close()
	if injected := f.fs.check("close", f.name); injected != nil {
		return injected
	}
	return err
}

// env is a cache configuration for one test.
type env struct {
	t     *testing.T
	opts  Options
	clock *clock
	logs  *syncBuffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, clock: &clock{t: epoch}, logs: &syncBuffer{}}
	e.opts = Options{
		Dir:       filepath.Join(t.TempDir(), "cache"),
		MaxSize:   math.MaxInt64,
		MaxAge:    math.MaxInt64,
		Now:       e.clock.Now,
		FreeSpace: func(string) (uint64, error) { return 1 << 40, nil },
		Logger:    slog.New(slog.NewTextHandler(e.logs, nil)),
	}
	return e
}

func (e *env) open() *Cache {
	e.t.Helper()
	return e.openFS(osFS{})
}

func (e *env) openFS(fsys fileSystem) *Cache {
	e.t.Helper()
	c, err := open(e.opts, fsys)
	if err != nil {
		e.t.Fatalf("open: %v", err)
	}
	e.t.Cleanup(func() { _ = c.Close() })
	return c
}

func testKey(name string) Key { return Key{Cluster: "example", Path: "/" + name} }

// testVersion has blocks of 4 bytes.
func testVersion(size int64) Version {
	return Version{Size: size, ETag: "tag", HeadDigest: "digest", BlockSize: 4}
}

// content is the bytes of a file of the given size: every offset has a known value.
func content(size int64) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

// block returns the bytes of block i of a file of version v.
func block(v Version, i int64) []byte {
	start := i * v.BlockSize
	return content(v.Size)[start : start+v.blockLen(i)]
}

func acquire(t *testing.T, c *Cache, key Key, v Version) *Entry {
	t.Helper()
	h, err := c.Acquire(key, v)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return h
}

func writeBlocks(t *testing.T, h *Entry, blocks ...int64) {
	t.Helper()
	for _, b := range blocks {
		if err := h.WriteBlock(b, block(h.Version(), b)); err != nil {
			t.Fatalf("WriteBlock(%d): %v", b, err)
		}
	}
}

func release(t *testing.T, h *Entry) {
	t.Helper()
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// fill caches the given blocks of a file and releases it.
func fill(t *testing.T, c *Cache, key Key, v Version, blocks ...int64) {
	t.Helper()
	h := acquire(t, c, key, v)
	writeBlocks(t, h, blocks...)
	release(t, h)
}

// readAll reads the whole file through h.
func readAll(t *testing.T, h *Entry) []byte {
	t.Helper()
	buf := make([]byte, h.Version().Size)
	n, err := h.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	return buf[:n]
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s should exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s should not exist, stat error: %v", path, err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}

func mustContain(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log does not contain %q:\n%s", want, logs.String())
	}
}

// atomicFlag switches fault injection on after a test has finished its setup.
type atomicFlag struct{ v atomic.Bool }

func (f *atomicFlag) set()     { f.v.Store(true) }
func (f *atomicFlag) on() bool { return f.v.Load() }

var errEOF = io.EOF
