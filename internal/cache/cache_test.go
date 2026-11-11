package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		opts func(*Options)
		fsys fileSystem
	}{
		{"no directory", func(o *Options) { o.Dir = "" }, osFS{}},
		{"directory under a file", func(o *Options) { o.Dir = filepath.Join(notADir, "sub") }, osFS{}},
		{"unreadable directory", func(*Options) {}, faultFS{fail: failOn("readdir", "cache")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			tt.opts(&e.opts)
			if c, err := open(e.opts, tt.fsys); err == nil {
				_ = c.Close()
				t.Fatal("open succeeded")
			}
		})
	}
}

func TestOpenCreatesPrivateDirectoryAndUsesDefaults(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a", "cache")
	c, err := Open(Options{Dir: dir, MaxSize: 1 << 40, MaxAge: time.Hour, MinFree: 1})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want 700", got)
	}
	// MinFree makes the write read the real free space of the disk.
	h := acquire(t, c, testKey("a"), testVersion(4))
	writeBlocks(t, h, 0)
	release(t, h)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	v := testVersion(10) // blocks of 4, 4 and 2 bytes
	h := acquire(t, c, testKey("a"), v)

	if h.Has(0) || h.Complete() {
		t.Fatal("new entry reports data")
	}
	writeBlocks(t, h, 2, 0)
	if !h.Has(0) || h.Has(1) || !h.Has(2) || h.Has(3) || h.Has(-1) {
		t.Fatal("Has does not match the blocks written")
	}

	buf := make([]byte, 4)
	if _, err := h.ReadAt(buf, 2); !errors.Is(err, ErrNotCached) {
		t.Fatalf("read across a hole: err = %v, want ErrNotCached", err)
	}
	if buf[0] != 0 {
		t.Error("a failed read returned partial data")
	}

	writeBlocks(t, h, 1)
	if !h.Complete() {
		t.Fatal("entry is not complete")
	}
	want := content(10)
	if got := readAll(t, h); string(got) != string(want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	n, err := h.ReadAt(buf, 3) // crosses blocks 0 and 1
	if err != nil || string(buf[:n]) != string(want[3:7]) {
		t.Fatalf("cross-boundary read = %v, %v; want %v", buf[:n], err, want[3:7])
	}
	if u := c.Usage(); u.Entries != 1 || u.Bytes != 10 {
		t.Errorf("Usage = %+v, want 1 entry and 10 bytes", u)
	}
	release(t, h)
}

func TestReadAtEnd(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	v := testVersion(10)
	h := acquire(t, c, testKey("a"), v)
	writeBlocks(t, h, 0, 1, 2)
	want := content(10)

	tests := []struct {
		name    string
		size    int
		off     int64
		wantN   int
		wantErr error
	}{
		{"exactly to the end", 4, 6, 4, nil},
		{"past the end", 20, 6, 4, errEOF},
		{"at the end", 4, 10, 0, errEOF},
		{"beyond the end", 4, 50, 0, errEOF},
		{"empty read", 0, 3, 0, nil},
	}
	for _, tt := range tests {
		buf := make([]byte, tt.size)
		n, err := h.ReadAt(buf, tt.off)
		if n != tt.wantN || !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: ReadAt = %d, %v; want %d, %v", tt.name, n, err, tt.wantN, tt.wantErr)
		}
		if n > 0 && string(buf[:n]) != string(want[tt.off:int(tt.off)+n]) {
			t.Errorf("%s: wrong bytes %v", tt.name, buf[:n])
		}
	}
	if _, err := h.ReadAt(make([]byte, 1), -1); err == nil {
		t.Error("negative offset accepted")
	}
	release(t, h)
}

func TestEmptyFile(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	h := acquire(t, c, testKey("empty"), testVersion(0))
	if !h.Complete() {
		t.Error("an empty file is complete")
	}
	if n, err := h.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, errEOF) {
		t.Errorf("ReadAt = %d, %v; want 0, EOF", n, err)
	}
	if info, ok := c.Peek(testKey("empty")); !ok || !info.Complete {
		t.Errorf("Peek = %+v, %v", info, ok)
	}
	release(t, h)
}

func TestWriteBlockValidation(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	h := acquire(t, c, testKey("a"), testVersion(10))
	tests := []struct {
		name  string
		block int64
		data  []byte
	}{
		{"negative block", -1, make([]byte, 4)},
		{"block past the end", 3, make([]byte, 4)},
		{"short full block", 0, make([]byte, 3)},
		{"long full block", 0, make([]byte, 5)},
		{"full-size last block", 2, make([]byte, 4)},
		{"empty", 1, nil},
	}
	for _, tt := range tests {
		if err := h.WriteBlock(tt.block, tt.data); !errors.Is(err, ErrInvalidBlock) {
			t.Errorf("%s: err = %v, want ErrInvalidBlock", tt.name, err)
		}
	}
	if u := c.Usage(); u.Bytes != 0 {
		t.Errorf("rejected writes were counted: %+v", u)
	}
	release(t, h)
}

func TestWriteExistingBlockIsNoop(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	v := testVersion(8)
	h := acquire(t, c, testKey("a"), v)
	writeBlocks(t, h, 0)
	if err := h.WriteBlock(0, []byte("zzzz")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := h.ReadAt(buf, 0); err != nil || string(buf) != string(block(v, 0)) {
		t.Errorf("block was overwritten: %v, %v", buf, err)
	}
	if u := c.Usage(); u.Bytes != 4 {
		t.Errorf("Usage = %+v, want 4 bytes", u)
	}
	release(t, h)
}

func TestSharedEntryAndPins(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	v := testVersion(8)
	a := acquire(t, c, testKey("a"), v)
	b := acquire(t, c, testKey("a"), v)
	writeBlocks(t, a, 0)
	if !b.Has(0) {
		t.Fatal("holders do not share the entry")
	}
	release(t, a)
	// The data file stays open for the remaining holder.
	writeBlocks(t, b, 1)
	if got := readAll(t, b); string(got) != string(content(8)) {
		t.Errorf("read %v", got)
	}
	release(t, b)
	if u := c.Usage(); u.Entries != 1 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestReleasedEntryReturnsErrors(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	h := acquire(t, c, testKey("a"), testVersion(8))
	release(t, h)
	if err := h.Release(); !errors.Is(err, ErrReleased) {
		t.Errorf("second Release: %v", err)
	}
	if _, err := h.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrReleased) {
		t.Errorf("ReadAt: %v", err)
	}
	if err := h.WriteBlock(0, make([]byte, 4)); !errors.Is(err, ErrReleased) {
		t.Errorf("WriteBlock: %v", err)
	}
}

func TestAcquireRejectsInvalidVersion(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	if _, err := c.Acquire(testKey("a"), Version{Size: 4}); !errors.Is(err, ErrInvalidVersion) {
		t.Errorf("err = %v, want ErrInvalidVersion", err)
	}
}

func TestRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key, v := testKey("a"), testVersion(10)
	fill(t, c, key, v, 0, 2)
	e.clock.Advance(time.Hour)
	h := acquire(t, c, key, v)
	if _, err := h.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatal(err)
	}
	release(t, h)
	e.clock.Advance(time.Hour)
	c.MarkValidated(key)
	before, _ := c.Peek(key)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2 := e.open()
	after, ok := c2.Peek(key)
	if !ok || after != before {
		t.Fatalf("after restart Peek = %+v, %v; want %+v", after, ok, before)
	}
	if !after.LastRead.Equal(epoch.Add(time.Hour)) || !after.Validated.Equal(epoch.Add(2*time.Hour)) {
		t.Errorf("times not preserved: %+v", after)
	}
	if after.Bytes != 6 || after.Complete {
		t.Errorf("bitmap not preserved: %+v", after)
	}
	h2 := acquire(t, c2, key, v)
	if !h2.Has(0) || h2.Has(1) || !h2.Has(2) {
		t.Error("bitmap differs after restart")
	}
	buf := make([]byte, 2)
	if _, err := h2.ReadAt(buf, 8); err != nil || string(buf) != string(content(10)[8:]) {
		t.Errorf("data differs after restart: %v, %v", buf, err)
	}
	release(t, h2)
	if u := c2.Usage(); u.Entries != 1 || u.Bytes != 6 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestCrashNeverInventsBlocks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key, v := testKey("a"), testVersion(20)

	// Before the first persist the entry has data but no metadata.
	h := acquire(t, c, key, v)
	writeBlocks(t, h, 0)
	crashed := filepath.Join(t.TempDir(), "crash0")
	copyTree(t, e.opts.Dir, crashed)
	opts := e.opts
	opts.Dir = crashed
	c0, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c0.Peek(key); ok {
		t.Error("an entry that never persisted came back")
	}
	_ = c0.Close()
	release(t, h)

	// After a persist, later blocks are lost in a crash but the old ones stay.
	h = acquire(t, c, key, v)
	writeBlocks(t, h, 1, 2)
	crashed = filepath.Join(t.TempDir(), "crash1")
	copyTree(t, e.opts.Dir, crashed)
	opts.Dir = crashed
	c1, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c1.Close() }()
	h1 := acquire(t, c1, key, v)
	if !h1.Has(0) || h1.Has(1) || h1.Has(2) {
		t.Errorf("after crash: Has(0..2) = %v %v %v, want true false false", h1.Has(0), h1.Has(1), h1.Has(2))
	}
	if _, err := h1.ReadAt(make([]byte, 4), 4); !errors.Is(err, ErrNotCached) {
		t.Errorf("lost block read: %v", err)
	}
	release(t, h1)
	release(t, h)
}

func TestMetadataPersistedEvery64Blocks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key, v := testKey("a"), testVersion(4*persistEvery*2)
	h := acquire(t, c, key, v)
	blocks := make([]int64, persistEvery)
	for i := range blocks {
		blocks[i] = int64(i)
	}
	writeBlocks(t, h, blocks[:persistEvery-1]...)
	crashed := filepath.Join(t.TempDir(), "before")
	copyTree(t, e.opts.Dir, crashed)
	rel, err := filepath.Rel(e.opts.Dir, baseName(e.opts.Dir, key))
	if err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(crashed, rel+metaExt))

	writeBlocks(t, h, blocks[persistEvery-1])
	crashed = filepath.Join(t.TempDir(), "after")
	copyTree(t, e.opts.Dir, crashed)
	opts := e.opts
	opts.Dir = crashed
	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if info, ok := c2.Peek(key); !ok || info.Bytes != 4*persistEvery {
		t.Errorf("Peek = %+v, %v; want %d bytes", info, ok, 4*persistEvery)
	}
	release(t, h)
}

func TestVersionChangeDiscardsOldData(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	key := testKey("a")
	v1 := testVersion(8)
	old := acquire(t, c, key, v1)
	writeBlocks(t, old, 0, 1)

	v2 := v1
	v2.ETag = "changed"
	fresh := acquire(t, c, key, v2)
	if fresh.Has(0) {
		t.Fatal("new version inherited blocks")
	}
	if info, _ := c.Peek(key); info.Version != v2 || info.Bytes != 0 {
		t.Errorf("Peek = %+v", info)
	}
	if got := readAll(t, old); string(got) != string(content(8)) {
		t.Errorf("old holder reads %v", got)
	}
	writeBlocks(t, old, 0) // a late write by the old holder must not touch the new entry
	if u := c.Usage(); u.Entries != 1 || u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
	release(t, old)
	writeBlocks(t, fresh, 1)
	release(t, fresh)
	info, _ := c.Peek(key)
	if info.Version != v2 || info.Bytes != 4 {
		t.Errorf("old holder's release disturbed the new entry: %+v", info)
	}
}

func TestAcquireDiscardsEntryWithMissingDataFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key, v := testKey("a"), testVersion(8)
	fill(t, c, key, v, 0, 1)
	if err := os.Remove(baseName(e.opts.Dir, key) + dataExt); err != nil {
		t.Fatal(err)
	}
	h := acquire(t, c, key, v)
	if h.Has(0) {
		t.Error("blocks survived the loss of their data file")
	}
	if u := c.Usage(); u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
	release(t, h)
	mustExist(t, baseName(e.opts.Dir, key)+dataExt)
}

func TestAcquireFailures(t *testing.T) {
	t.Parallel()
	key, v := testKey("a"), testVersion(8)
	v2 := v
	v2.ETag = "changed"
	tests := []struct {
		name  string
		setup func(c *Cache, e *env)
		fault func(op, name string) error
		do    func(c *Cache) error
	}{
		{
			name:  "cannot create subdirectory",
			fault: failOn("mkdir", ""),
			do:    func(c *Cache) error { _, err := c.Acquire(key, v); return err },
		},
		{
			name:  "cannot create data file",
			fault: failOn("open", dataExt),
			do:    func(c *Cache) error { _, err := c.Acquire(key, v); return err },
		},
		{
			name:  "cannot remove old version",
			setup: func(c *Cache, _ *env) { fill(t, c, key, v, 0) },
			fault: failOn("remove", dataExt),
			do:    func(c *Cache) error { _, err := c.Acquire(key, v2); return err },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			var armed atomicFlag
			fsys := faultFS{fail: func(op, name string) error {
				if !armed.on() {
					return nil
				}
				return tt.fault(op, name)
			}}
			c := e.openFS(fsys)
			if tt.setup != nil {
				tt.setup(c, e)
			}
			armed.set()
			if err := tt.do(c); !errors.Is(err, errInjected) {
				t.Errorf("err = %v, want the injected failure", err)
			}
		})
	}
}

func TestAcquireRemovalFailureOfDamagedEntry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var armed atomicFlag
	c := e.openFS(faultFS{fail: func(op, _ string) error {
		if armed.on() && op == "remove" {
			return errInjected
		}
		return nil
	}})
	key, v := testKey("a"), testVersion(8)
	fill(t, c, key, v, 0)
	if err := os.Remove(baseName(e.opts.Dir, key) + dataExt); err != nil {
		t.Fatal(err)
	}
	armed.set()
	if _, err := c.Acquire(key, v); !errors.Is(err, errInjected) {
		t.Errorf("err = %v, want the injected failure", err)
	}
}

func TestPeekAndMarkValidated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key := testKey("a")
	if _, ok := c.Peek(key); ok {
		t.Fatal("Peek of unknown key")
	}
	c.MarkValidated(key) // no effect, no panic
	h := acquire(t, c, key, testVersion(8))
	writeBlocks(t, h, 0)
	e.clock.Advance(time.Minute)
	c.MarkValidated(key) // while pinned
	info, ok := c.Peek(key)
	if !ok || !info.Validated.Equal(epoch.Add(time.Minute)) || info.Bytes != 4 || info.Complete {
		t.Errorf("Peek = %+v, %v", info, ok)
	}
	release(t, h)
	e.clock.Advance(time.Minute)
	c.MarkValidated(key) // while unpinned
	if info, _ := c.Peek(key); !info.Validated.Equal(epoch.Add(2 * time.Minute)) {
		t.Errorf("Validated = %v", info.Validated)
	}
}

func TestMarkValidatedLogsPersistFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var armed atomicFlag
	c := e.openFS(faultFS{fail: func(op, _ string) error {
		if armed.on() && op == "rename" {
			return errInjected
		}
		return nil
	}})
	key := testKey("a")
	fill(t, c, key, testVersion(8), 0)
	armed.set()
	c.MarkValidated(key)
	mustContain(t, e.logs, "persist validation time")
}

func TestDiscard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key, v := testKey("a"), testVersion(8)
	if err := c.Discard(key); err != nil {
		t.Fatalf("Discard of unknown key: %v", err)
	}
	fill(t, c, key, v, 0)
	if err := c.Discard(key); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, baseName(e.opts.Dir, key)+dataExt)
	mustNotExist(t, baseName(e.opts.Dir, key)+metaExt)
	if _, ok := c.Peek(key); ok {
		t.Error("discarded entry is still cached")
	}

	// A pinned entry is unlinked at once; its holder keeps working.
	h := acquire(t, c, key, v)
	writeBlocks(t, h, 0)
	if err := c.Discard(key); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, baseName(e.opts.Dir, key)+dataExt)
	if !h.Has(0) {
		t.Error("holder lost its data")
	}
	writeBlocks(t, h, 1)
	if got := readAll(t, h); string(got) != string(content(8)) {
		t.Errorf("holder reads %v", got)
	}
	release(t, h)
	mustNotExist(t, baseName(e.opts.Dir, key)+metaExt) // a late persist must not resurrect it
	if u := c.Usage(); u.Entries != 0 || u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestDiscardReportsRemovalFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var armed atomicFlag
	c := e.openFS(faultFS{fail: func(op, _ string) error {
		if armed.on() && op == "remove" {
			return errInjected
		}
		return nil
	}})
	key := testKey("a")
	fill(t, c, key, testVersion(8), 0)
	armed.set()
	if err := c.Discard(key); !errors.Is(err, errInjected) {
		t.Errorf("err = %v, want the injected failure", err)
	}
	if _, ok := c.Peek(key); ok {
		t.Error("entry stayed cached after a failed unlink")
	}
}

func TestCloseAndAfter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	key, v := testKey("a"), testVersion(8)
	held := acquire(t, c, key, v)
	writeBlocks(t, held, 0)
	fill(t, c, testKey("b"), v, 0)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close: %v", err)
	}
	if _, err := c.Acquire(key, v); !errors.Is(err, ErrClosed) {
		t.Errorf("Acquire: %v", err)
	}
	if err := c.Discard(key); !errors.Is(err, ErrClosed) {
		t.Errorf("Discard: %v", err)
	}
	if err := c.Evict(); !errors.Is(err, ErrClosed) {
		t.Errorf("Evict: %v", err)
	}
	if _, ok := c.Peek(key); ok {
		t.Error("Peek on a closed cache")
	}
	c.MarkValidated(key)
	if _, err := held.ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrClosed) {
		t.Errorf("ReadAt: %v", err)
	}
	if err := held.WriteBlock(1, make([]byte, 4)); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteBlock: %v", err)
	}
	if err := held.Release(); !errors.Is(err, ErrClosed) {
		t.Errorf("Release: %v", err)
	}

	// Close persisted the entry that was still pinned.
	c2 := e.open()
	if info, ok := c2.Peek(key); !ok || info.Bytes != 4 {
		t.Errorf("pinned entry after Close: %+v, %v", info, ok)
	}
}

func TestCloseReportsFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		op   string
		sfx  string
	}{
		{"persist", "rename", metaExt},
		{"close data file", "close", dataExt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			var armed atomicFlag
			fail := failOn(tt.op, tt.sfx)
			c := e.openFS(faultFS{fail: func(op, name string) error {
				if !armed.on() {
					return nil
				}
				return fail(op, name)
			}})
			h := acquire(t, c, testKey("a"), testVersion(8))
			writeBlocks(t, h, 0)
			armed.set()
			if err := c.Close(); !errors.Is(err, errInjected) {
				t.Errorf("Close = %v, want the injected failure", err)
			}
		})
	}
}
