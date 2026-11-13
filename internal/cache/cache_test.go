package cache

import (
	"bytes"
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
			if c, err := openWith(e.opts, tt.fsys); err == nil {
				closeCache(t, c)
				t.Fatal("open succeeded")
			}
		})
	}
}

func TestOpenCreatesPrivateDirectoryAndUsesDefaults(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a", "cache")
	cache, err := Open(Options{Dir: dir, MaxSize: 1 << 40, MaxAge: time.Hour, MinFree: 1})
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
	h := acquire(t, cache, testKey("a"), testVersion(4))
	writeBlocks(t, h, 0)
	release(t, h)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHasFollowsBlocksWritten(t *testing.T) {
	t.Parallel()
	version := testVersion(10) // blocks of 4, 4 and 2 bytes
	handle := acquire(t, newEnv(t).open(), testKey("a"), version)

	if handle.Has(0) || handle.Complete() {
		t.Fatal("new entry reports data")
	}
	writeBlocks(t, handle, 2, 0)
	if !handle.Has(0) || handle.Has(1) || !handle.Has(2) || handle.Has(3) || handle.Has(-1) {
		t.Fatal("Has does not match the blocks written")
	}
	release(t, handle)
}

func TestReadAcrossHoleReturnsNoData(t *testing.T) {
	t.Parallel()
	handle := acquire(t, newEnv(t).open(), testKey("a"), testVersion(10))
	writeBlocks(t, handle, 2, 0)

	buf := make([]byte, 4)
	if _, err := handle.ReadAt(buf, 2); !errors.Is(err, ErrNotCached) {
		t.Fatalf("read across a hole: err = %v, want ErrNotCached", err)
	}
	if buf[0] != 0 {
		t.Error("a failed read returned partial data")
	}
	release(t, handle)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	cache := newEnv(t).open()
	handle := acquire(t, cache, testKey("a"), testVersion(10)) // blocks of 4, 4 and 2 bytes
	writeBlocks(t, handle, 2, 0, 1)

	if !handle.Complete() {
		t.Fatal("entry is not complete")
	}
	want := content(10)
	if got := readAll(t, handle); !bytes.Equal(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	buf := make([]byte, 4)
	n, err := handle.ReadAt(buf, 3) // crosses blocks 0 and 1
	if err != nil || !bytes.Equal(buf[:n], want[3:7]) {
		t.Fatalf("cross-boundary read = %v, %v; want %v", buf[:n], err, want[3:7])
	}
	if u := cache.Usage(); u.Entries != 1 || u.Bytes != 10 {
		t.Errorf("Usage = %+v, want 1 entry and 10 bytes", u)
	}
	release(t, handle)
}

func TestReadAtEnd(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	version := testVersion(10)
	handle := acquire(t, c, testKey("a"), version)
	writeBlocks(t, handle, 0, 1, 2)
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
		n, err := handle.ReadAt(buf, tt.off)
		if n != tt.wantN || !errors.Is(err, tt.wantErr) {
			t.Errorf("%s: ReadAt = %d, %v; want %d, %v", tt.name, n, err, tt.wantN, tt.wantErr)
		}
		if n > 0 && !bytes.Equal(buf[:n], want[tt.off:int(tt.off)+n]) {
			t.Errorf("%s: wrong bytes %v", tt.name, buf[:n])
		}
	}
	if _, err := handle.ReadAt(make([]byte, 1), -1); err == nil {
		t.Error("negative offset accepted")
	}
	release(t, handle)
}

func TestEmptyFile(t *testing.T) {
	t.Parallel()
	cache := newEnv(t).open()
	handle := acquire(t, cache, testKey("empty"), testVersion(0))
	if !handle.Complete() {
		t.Error("an empty file is complete")
	}
	if n, err := handle.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, errEOF) {
		t.Errorf("ReadAt = %d, %v; want 0, EOF", n, err)
	}
	if info, ok := cache.Peek(testKey("empty")); !ok || !info.Complete {
		t.Errorf("Peek = %+v, %v", info, ok)
	}
	release(t, handle)
}

func TestWriteBlockValidation(t *testing.T) {
	t.Parallel()
	cache := newEnv(t).open()
	handle := acquire(t, cache, testKey("a"), testVersion(10))
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
		if err := handle.WriteBlock(tt.block, tt.data); !errors.Is(err, ErrInvalidBlock) {
			t.Errorf("%s: err = %v, want ErrInvalidBlock", tt.name, err)
		}
	}
	if u := cache.Usage(); u.Bytes != 0 {
		t.Errorf("rejected writes were counted: %+v", u)
	}
	release(t, handle)
}

func TestWriteExistingBlockIsNoop(t *testing.T) {
	t.Parallel()
	cache := newEnv(t).open()
	version := testVersion(8)
	handle := acquire(t, cache, testKey("a"), version)
	writeBlocks(t, handle, 0)
	if err := handle.WriteBlock(0, []byte("zzzz")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := handle.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, block(version, 0)) {
		t.Errorf("block was overwritten: %v, %v", buf, err)
	}
	if u := cache.Usage(); u.Bytes != 4 {
		t.Errorf("Usage = %+v, want 4 bytes", u)
	}
	release(t, handle)
}

func TestSharedEntryAndPins(t *testing.T) {
	t.Parallel()
	cache := newEnv(t).open()
	v := testVersion(8)
	first := acquire(t, cache, testKey("a"), v)
	second := acquire(t, cache, testKey("a"), v)
	writeBlocks(t, first, 0)
	if !second.Has(0) {
		t.Fatal("holders do not share the entry")
	}
	release(t, first)
	// The data file stays open for the remaining holder.
	writeBlocks(t, second, 1)
	if got := readAll(t, second); !bytes.Equal(got, content(8)) {
		t.Errorf("read %v", got)
	}
	release(t, second)
	if u := cache.Usage(); u.Entries != 1 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestReleasedEntryReturnsErrors(t *testing.T) {
	t.Parallel()
	c := newEnv(t).open()
	handle := acquire(t, c, testKey("a"), testVersion(8))
	release(t, handle)
	if err := handle.Release(); !errors.Is(err, ErrReleased) {
		t.Errorf("second Release: %v", err)
	}
	if _, err := handle.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrReleased) {
		t.Errorf("ReadAt: %v", err)
	}
	if err := handle.WriteBlock(0, make([]byte, 4)); !errors.Is(err, ErrReleased) {
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

func TestRestartKeepsMetadata(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	key, version := testKey("a"), testVersion(10)
	fill(t, cache, key, version, 0, 2)
	env.clock.Advance(time.Hour)
	handle := acquire(t, cache, key, version)
	if _, err := handle.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatal(err)
	}
	release(t, handle)
	env.clock.Advance(time.Hour)
	cache.MarkValidated(key)
	before, _ := cache.Peek(key)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	after, ok := env.open().Peek(key)
	if !ok || after != before {
		t.Fatalf("after restart Peek = %+v, %v; want %+v", after, ok, before)
	}
	if !after.LastRead.Equal(epoch().Add(time.Hour)) || !after.Validated.Equal(epoch().Add(2*time.Hour)) {
		t.Errorf("times not preserved: %+v", after)
	}
	if after.Bytes != 6 || after.Complete {
		t.Errorf("bitmap not preserved: %+v", after)
	}
}

func TestRestartKeepsData(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	first := env.open()
	key, version := testKey("a"), testVersion(10)
	fill(t, first, key, version, 0, 2)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := env.open()
	handle := acquire(t, reopened, key, version)
	if !handle.Has(0) || handle.Has(1) || !handle.Has(2) {
		t.Error("bitmap differs after restart")
	}
	buf := make([]byte, 2)
	if _, err := handle.ReadAt(buf, 8); err != nil || !bytes.Equal(buf, content(10)[8:]) {
		t.Errorf("data differs after restart: %v, %v", buf, err)
	}
	release(t, handle)
	if usage := reopened.Usage(); usage.Entries != 1 || usage.Bytes != 6 {
		t.Errorf("Usage = %+v", usage)
	}
}

func TestCrashNeverInventsBlocks(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	key, version := testKey("a"), testVersion(20)

	// Before the first persist the entry has data but no metadata.
	handle := acquire(t, cache, key, version)
	writeBlocks(t, handle, 0)
	crashed := filepath.Join(t.TempDir(), "crash0")
	copyTree(t, env.opts.Dir, crashed)
	if _, ok := openCopy(t, env.opts, crashed).Peek(key); ok {
		t.Error("an entry that never persisted came back")
	}
	release(t, handle)

	// After a persist, later blocks are lost in a crash but the old ones stay.
	handle = acquire(t, cache, key, version)
	writeBlocks(t, handle, 1, 2)
	crashed = filepath.Join(t.TempDir(), "crash1")
	copyTree(t, env.opts.Dir, crashed)
	afterCrash := acquire(t, openCopy(t, env.opts, crashed), key, version)
	if !afterCrash.Has(0) || afterCrash.Has(1) || afterCrash.Has(2) {
		t.Errorf("after crash: blocks 0, 1 and 2 present = %v, %v, %v; want true, false, false",
			afterCrash.Has(0), afterCrash.Has(1), afterCrash.Has(2))
	}
	if _, err := afterCrash.ReadAt(make([]byte, 4), 4); !errors.Is(err, ErrNotCached) {
		t.Errorf("lost block read: %v", err)
	}
	release(t, afterCrash)
	release(t, handle)
}

func TestMetadataPersistedEvery64Blocks(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	c := env.open()
	key, version := testKey("a"), testVersion(4*persistEvery*2)
	handle := acquire(t, c, key, version)
	blocks := make([]int64, persistEvery)
	for i := range blocks {
		blocks[i] = int64(i)
	}
	writeBlocks(t, handle, blocks[:persistEvery-1]...)
	crashed := filepath.Join(t.TempDir(), "before")
	copyTree(t, env.opts.Dir, crashed)
	rel, err := filepath.Rel(env.opts.Dir, baseName(env.opts.Dir, key))
	if err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(crashed, rel+metaExt))

	writeBlocks(t, handle, blocks[persistEvery-1])
	crashed = filepath.Join(t.TempDir(), "after")
	copyTree(t, env.opts.Dir, crashed)
	if info, ok := openCopy(t, env.opts, crashed).Peek(key); !ok || info.Bytes != 4*persistEvery {
		t.Errorf("Peek = %+v, %v; want %d bytes", info, ok, 4*persistEvery)
	}
	release(t, handle)
}

func TestVersionChangeDiscardsOldData(t *testing.T) {
	t.Parallel()
	cache := newEnv(t).open()
	key := testKey("a")
	v1 := testVersion(8)
	old := acquire(t, cache, key, v1)
	writeBlocks(t, old, 0, 1)

	changedVersion := v1
	changedVersion.ETag = "changed"
	fresh := acquire(t, cache, key, changedVersion)
	if fresh.Has(0) {
		t.Fatal("new version inherited blocks")
	}
	if info, _ := cache.Peek(key); info.Version != changedVersion || info.Bytes != 0 {
		t.Errorf("Peek = %+v", info)
	}
	if got := readAll(t, old); !bytes.Equal(got, content(8)) {
		t.Errorf("old holder reads %v", got)
	}
	writeBlocks(t, old, 0) // a late write by the old holder must not touch the new entry
	if u := cache.Usage(); u.Entries != 1 || u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
	release(t, old)
	writeBlocks(t, fresh, 1)
	release(t, fresh)
	info, _ := cache.Peek(key)
	if info.Version != changedVersion || info.Bytes != 4 {
		t.Errorf("old holder's release disturbed the new entry: %+v", info)
	}
}

func TestAcquireDiscardsEntryWithMissingDataFile(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	key, version := testKey("a"), testVersion(8)
	fill(t, cache, key, version, 0, 1)
	if err := os.Remove(baseName(env.opts.Dir, key) + dataExt); err != nil {
		t.Fatal(err)
	}
	handle := acquire(t, cache, key, version)
	if handle.Has(0) {
		t.Error("blocks survived the loss of their data file")
	}
	if u := cache.Usage(); u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
	release(t, handle)
	mustExist(t, baseName(env.opts.Dir, key)+dataExt)
}

func TestAcquireFailures(t *testing.T) {
	t.Parallel()
	key, version := testKey("a"), testVersion(8)
	changedVersion := version
	changedVersion.ETag = "changed"
	tests := []struct {
		name  string
		setup func(c *Cache, e *env)
		fault func(op, name string) error
		do    func(c *Cache) error
	}{
		{
			name:  "cannot create subdirectory",
			fault: failOn("mkdir", ""),
			do:    func(c *Cache) error { _, err := c.Acquire(key, version); return err },
		},
		{
			name:  "cannot create data file",
			fault: failOn("open", dataExt),
			do:    func(c *Cache) error { _, err := c.Acquire(key, version); return err },
		},
		{
			name:  "cannot remove old version",
			setup: func(c *Cache, _ *env) { fill(t, c, key, version, 0) },
			fault: failOn("remove", dataExt),
			do:    func(c *Cache) error { _, err := c.Acquire(key, changedVersion); return err },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newEnv(t)
			var armed atomicFlag
			fsys := faultFS{fail: func(op, name string) error {
				if !armed.on() {
					return nil
				}
				return tt.fault(op, name)
			}}
			c := env.openFS(fsys)
			if tt.setup != nil {
				tt.setup(c, env)
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
	env := newEnv(t)
	var armed atomicFlag
	cache := env.openFS(faultFS{fail: func(op, _ string) error {
		if armed.on() && op == "remove" {
			return errInjected
		}
		return nil
	}})
	key, version := testKey("a"), testVersion(8)
	fill(t, cache, key, version, 0)
	if err := os.Remove(baseName(env.opts.Dir, key) + dataExt); err != nil {
		t.Fatal(err)
	}
	armed.set()
	if _, err := cache.Acquire(key, version); !errors.Is(err, errInjected) {
		t.Errorf("err = %v, want the injected failure", err)
	}
}

func TestPeekAndMarkValidated(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	key := testKey("a")
	if _, ok := cache.Peek(key); ok {
		t.Fatal("Peek of unknown key")
	}
	cache.MarkValidated(key) // no effect, no panic
	handle := acquire(t, cache, key, testVersion(8))
	writeBlocks(t, handle, 0)
	env.clock.Advance(time.Minute)
	cache.MarkValidated(key) // while pinned
	info, ok := cache.Peek(key)
	if !ok || !info.Validated.Equal(epoch().Add(time.Minute)) || info.Bytes != 4 || info.Complete {
		t.Errorf("Peek = %+v, %v", info, ok)
	}
	release(t, handle)
	env.clock.Advance(time.Minute)
	cache.MarkValidated(key) // while unpinned
	if info, _ := cache.Peek(key); !info.Validated.Equal(epoch().Add(2 * time.Minute)) {
		t.Errorf("Validated = %v", info.Validated)
	}
}

func TestMarkValidatedLogsPersistFailure(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	var armed atomicFlag
	cache := env.openFS(faultFS{fail: func(op, _ string) error {
		if armed.on() && op == "rename" {
			return errInjected
		}
		return nil
	}})
	key := testKey("a")
	fill(t, cache, key, testVersion(8), 0)
	armed.set()
	cache.MarkValidated(key)
	mustContain(t, env.logs, "persist validation time")
}

func TestDiscard(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	key, version := testKey("a"), testVersion(8)
	if err := cache.Discard(key); err != nil {
		t.Fatalf("Discard of unknown key: %v", err)
	}
	fill(t, cache, key, version, 0)
	if err := cache.Discard(key); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, baseName(env.opts.Dir, key)+dataExt)
	mustNotExist(t, baseName(env.opts.Dir, key)+metaExt)
	if _, ok := cache.Peek(key); ok {
		t.Error("discarded entry is still cached")
	}

	// A pinned entry is unlinked at once; its holder keeps working.
	handle := acquire(t, cache, key, version)
	writeBlocks(t, handle, 0)
	if err := cache.Discard(key); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, baseName(env.opts.Dir, key)+dataExt)
	if !handle.Has(0) {
		t.Error("holder lost its data")
	}
	writeBlocks(t, handle, 1)
	if got := readAll(t, handle); !bytes.Equal(got, content(8)) {
		t.Errorf("holder reads %v", got)
	}
	release(t, handle)
	mustNotExist(t, baseName(env.opts.Dir, key)+metaExt) // a late persist must not resurrect it
	if u := cache.Usage(); u.Entries != 0 || u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestDiscardReportsRemovalFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var armed atomicFlag
	cache := e.openFS(faultFS{fail: func(op, _ string) error {
		if armed.on() && op == "remove" {
			return errInjected
		}
		return nil
	}})
	key := testKey("a")
	fill(t, cache, key, testVersion(8), 0)
	armed.set()
	if err := cache.Discard(key); !errors.Is(err, errInjected) {
		t.Errorf("err = %v, want the injected failure", err)
	}
	if _, ok := cache.Peek(key); ok {
		t.Error("entry stayed cached after a failed unlink")
	}
}

// closedCache returns a cache that has been closed, and an entry that was still held when
// it closed.
func closedCache(t *testing.T, env *env, key Key, version Version) (*Cache, *Entry) {
	t.Helper()
	cache := env.open()
	held := acquire(t, cache, key, version)
	writeBlocks(t, held, 0)
	fill(t, cache, testKey("b"), version, 0)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	return cache, held
}

func TestCacheOperationsAfterClose(t *testing.T) {
	t.Parallel()
	key, version := testKey("a"), testVersion(8)
	cache, _ := closedCache(t, newEnv(t), key, version)

	if err := cache.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close: %v", err)
	}
	if _, err := cache.Acquire(key, version); !errors.Is(err, ErrClosed) {
		t.Errorf("Acquire: %v", err)
	}
	if err := cache.Discard(key); !errors.Is(err, ErrClosed) {
		t.Errorf("Discard: %v", err)
	}
	if err := cache.Evict(); !errors.Is(err, ErrClosed) {
		t.Errorf("Evict: %v", err)
	}
	if _, ok := cache.Peek(key); ok {
		t.Error("Peek on a closed cache")
	}
	cache.MarkValidated(key)
}

func TestEntryOperationsAfterClose(t *testing.T) {
	t.Parallel()
	_, held := closedCache(t, newEnv(t), testKey("a"), testVersion(8))

	if _, err := held.ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrClosed) {
		t.Errorf("ReadAt: %v", err)
	}
	if err := held.WriteBlock(1, make([]byte, 4)); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteBlock: %v", err)
	}
	if err := held.Release(); !errors.Is(err, ErrClosed) {
		t.Errorf("Release: %v", err)
	}
}

func TestClosePersistsPinnedEntry(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	key := testKey("a")
	closedCache(t, env, key, testVersion(8))

	if info, ok := env.open().Peek(key); !ok || info.Bytes != 4 {
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
			cache := e.openFS(faultFS{fail: func(op, name string) error {
				if !armed.on() {
					return nil
				}
				return fail(op, name)
			}})
			h := acquire(t, cache, testKey("a"), testVersion(8))
			writeBlocks(t, h, 0)
			armed.set()
			if err := cache.Close(); !errors.Is(err, errInjected) {
				t.Errorf("Close = %v, want the injected failure", err)
			}
		})
	}
}
