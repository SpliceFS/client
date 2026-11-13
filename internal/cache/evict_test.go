package cache

import (
	"bytes"
	"errors"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// diskOf returns a FreeSpace function for a disk of the given capacity whose only
// contents are the cache's data files.
//
//nolint:unparam // the capacity belongs to each test, even while they all use the same one
func diskOf(t *testing.T, capacity int64) func(string) (uint64, error) {
	t.Helper()
	return func(dir string) (uint64, error) {
		var used int64
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, dataExt) {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			used += info.Size()
			return nil
		})
		if err != nil {
			t.Errorf("measure disk: %v", err)
		}
		return uint64(capacity - used), nil
	}
}

func cached(c *Cache, names ...string) []bool {
	out := make([]bool, len(names))
	for i, n := range names {
		_, out[i] = c.Peek(testKey(n))
	}
	return out
}

func wantCached(t *testing.T, c *Cache, names []string, want ...bool) {
	t.Helper()
	got := cached(c, names...)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %s cached = %v, want %v", names[i], got[i], want[i])
		}
	}
}

// threeEntries caches a, b and c (8 bytes each), created an hour apart, then reads a so
// that b is the least recently read.
func threeEntries(t *testing.T, e *env, cache *Cache) {
	t.Helper()
	v := testVersion(8)
	for _, n := range []string{"a", "b", "c"} {
		fill(t, cache, testKey(n), v, 0, 1)
		e.clock.Advance(time.Hour)
	}
	h := acquire(t, cache, testKey("a"), v)
	if _, err := h.ReadAt(make([]byte, 1), 0); err != nil {
		t.Fatal(err)
	}
	release(t, h)
}

func TestEvictLeastRecentlyReadFirst(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	threeEntries(t, env, cache)
	names := []string{"a", "b", "c"}

	env.clock.Advance(time.Hour)
	cache.opts.MaxSize = 16
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, cache, names, true, false, true)
	if u := cache.Usage(); u.Entries != 2 || u.Bytes != 16 {
		t.Errorf("Usage = %+v", u)
	}
	mustNotExist(t, baseName(env.opts.Dir, testKey("b"))+dataExt)
	mustNotExist(t, baseName(env.opts.Dir, testKey("b"))+metaExt)

	cache.opts.MaxSize = 8
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, cache, names, true, false, false)
}

func TestEvictByAge(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	env.opts.MaxAge = time.Hour
	cache := env.open()
	v := testVersion(8)
	fill(t, cache, testKey("old"), v, 0)
	env.clock.Advance(30 * time.Minute)
	fill(t, cache, testKey("new"), v, 0)

	env.clock.Advance(30 * time.Minute) // old: one hour, which is not older than the limit
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, cache, []string{"old", "new"}, true, true)

	env.clock.Advance(time.Second)
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, cache, []string{"old", "new"}, false, true)
}

func TestEvictNeverRemovesPinnedEntries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cache := e.open()
	threeEntries(t, e, cache)
	e.clock.Advance(time.Hour)
	cache.opts.MaxSize = 0
	cache.opts.MaxAge = 0
	held := acquire(t, cache, testKey("b"), testVersion(8))
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, cache, []string{"a", "b", "c"}, false, true, false)
	if got := readAll(t, held); !bytes.Equal(got, content(8)) {
		t.Errorf("pinned entry lost data: %v", got)
	}
	release(t, held)
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	if u := cache.Usage(); u.Entries != 0 || u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestUnlimitedSettingsNeverEvict(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	env.opts.MaxSize = math.MaxInt64
	env.opts.MaxAge = math.MaxInt64
	env.opts.MinFree = 0
	env.opts.FreeSpace = func(string) (uint64, error) {
		t.Error("free space read although MinFree is 0")
		return 0, nil
	}
	cache := env.open()
	threeEntries(t, env, cache)
	env.clock.Advance(100 * 365 * 24 * time.Hour)
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	if u := cache.Usage(); u.Entries != 3 || u.Bytes != 24 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestEvictTiesBreakByName(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	cache := env.open()
	v := testVersion(8)
	fill(t, cache, testKey("x"), v, 0, 1)
	fill(t, cache, testKey("y"), v, 0, 1)
	first, second := "x", "y"
	if baseName(env.opts.Dir, testKey(first)) > baseName(env.opts.Dir, testKey(second)) {
		first, second = second, first
	}
	cache.opts.MaxSize = 8
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, cache, []string{first, second}, false, true)
}

func TestEvictReportsRemovalFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cache := e.openFS(faultFS{fail: failOn("remove", dataExt)})
	fill(t, cache, testKey("a"), testVersion(8), 0)
	cache.opts.MaxSize = 0
	if err := cache.Evict(); !errors.Is(err, errInjected) {
		t.Errorf("Evict = %v, want the injected failure", err)
	}
	if u := cache.Usage(); u.Entries != 0 {
		t.Errorf("Usage = %+v: the entry stayed counted", u)
	}
}

func TestMinFreeEvictsToMakeRoom(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.FreeSpace = diskOf(t, 32)
	e.opts.MinFree = 8
	c := e.open()
	threeEntries(t, e, c) // 24 bytes used, 8 free: b is the least recently read

	handleD := acquire(t, c, testKey("d"), testVersion(8))
	writeBlocks(t, handleD, 0)
	wantCached(t, c, []string{"a", "b", "c", "d"}, true, false, true, true)
	if !handleD.Has(0) {
		t.Error("block not stored after eviction made room")
	}
	release(t, handleD)
}

func TestMinFreeReturnsErrNoSpace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.FreeSpace = diskOf(t, 32)
	e.opts.MinFree = 8
	cache := e.open()
	threeEntries(t, e, cache)
	pins := make([]*Entry, 0, 3)
	for _, n := range []string{"a", "b", "c"} {
		pins = append(pins, acquire(t, cache, testKey(n), testVersion(8)))
	}
	handleD := acquire(t, cache, testKey("d"), testVersion(8))
	if err := handleD.WriteBlock(0, block(handleD.Version(), 0)); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("WriteBlock = %v, want ErrNoSpace", err)
	}
	if handleD.Has(0) || cache.Usage().Bytes != 24 {
		t.Error("a refused block was stored")
	}
	for _, p := range pins {
		release(t, p)
	}
	// With the others unpinned, eviction makes room and the same write succeeds.
	if err := handleD.WriteBlock(0, block(handleD.Version(), 0)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	release(t, handleD)
}

func TestMinFreeEvictionFailureStillRefuses(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	env.opts.FreeSpace = diskOf(t, 32)
	env.opts.MinFree = 8
	var armed atomicFlag
	cache := env.openFS(faultFS{fail: func(op, name string) error {
		if armed.on() {
			return failOn("remove", dataExt)(op, name)
		}
		return nil
	}})
	threeEntries(t, env, cache)
	d := acquire(t, cache, testKey("d"), testVersion(8))
	armed.set()
	if err := d.WriteBlock(0, block(d.Version(), 0)); !errors.Is(err, ErrNoSpace) {
		t.Errorf("WriteBlock = %v, want ErrNoSpace", err)
	}
	mustContain(t, env.logs, "evict for free space")
}

func TestFreeSpaceFailure(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	env.opts.MinFree = 8
	env.opts.MaxSize = 0
	env.opts.FreeSpace = func(string) (uint64, error) { return 0, errInjected }
	cache := env.open()
	handle := acquire(t, cache, testKey("a"), testVersion(8))
	err := handle.WriteBlock(0, block(handle.Version(), 0))
	if !errors.Is(err, ErrNoSpace) || !errors.Is(err, errInjected) {
		t.Errorf("WriteBlock = %v, want ErrNoSpace wrapping the cause", err)
	}

	// Eviction cannot tell whether space is short, so it only applies the other limits.
	cache.opts.MaxSize = math.MaxInt64
	release(t, handle)
	if err := cache.Evict(); err != nil {
		t.Fatal(err)
	}
	mustContain(t, env.logs, "read free space")
	if u := cache.Usage(); u.Entries != 1 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestWriteOverMaxSizeEvicts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxSize = 8
	c := e.open()
	fill(t, c, testKey("old"), testVersion(8), 0, 1)
	handle := acquire(t, c, testKey("new"), testVersion(8))
	writeBlocks(t, handle, 0)
	wantCached(t, c, []string{"old", "new"}, false, true)
	if u := c.Usage(); u.Bytes != 4 {
		t.Errorf("Usage = %+v", u)
	}
	release(t, handle)
}

func TestRoomForClampsHugeFreeSpace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MinFree = 1
	e.opts.FreeSpace = func(string) (uint64, error) { return math.MaxUint64, nil }
	c := e.open()
	if room, err := c.roomFor(1<<40, 1); err != nil || !room {
		t.Errorf("roomFor = %v, %v", room, err)
	}
}

func TestStatfsFree(t *testing.T) {
	t.Parallel()
	if free, err := statfsFree(t.TempDir()); err != nil || free == 0 {
		t.Errorf("statfsFree = %d, %v", free, err)
	}
	if _, err := statfsFree(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("statfsFree of a missing directory succeeded")
	}
}

func TestSetLimitsTakesEffectAtNextEvict(t *testing.T) {
	t.Parallel()
	names := []string{"a", "b", "c"}
	for _, tt := range []struct {
		name    string
		maxSize int64
		maxAge  time.Duration
		minFree int64
		want    []bool
	}{
		{"unchanged limits evict nothing", math.MaxInt64, math.MaxInt64, 0, []bool{true, true, true}},
		{"max size", 16, math.MaxInt64, 0, []bool{true, false, true}},
		{"max age", math.MaxInt64, 90 * time.Minute, 0, []bool{true, false, false}},
		{"min free", math.MaxInt64, math.MaxInt64, 16, []bool{true, false, true}},
	} {
		e := newEnv(t)
		e.opts.FreeSpace = diskOf(t, 32)
		cache := e.open()
		threeEntries(t, e, cache) // 24 bytes used; b, c and a were last read 3h, 2h and 1h ago
		e.clock.Advance(time.Hour)

		cache.SetLimits(tt.maxSize, tt.maxAge, tt.minFree)
		wantCached(t, cache, names, true, true, true) // a limit is applied by a pass, not by SetLimits
		if err := cache.Evict(); err != nil {
			t.Fatalf("%s: Evict: %v", tt.name, err)
		}
		wantCached(t, cache, names, tt.want...)
	}
}
