package cache

import (
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
func diskOf(t *testing.T, capacity int64) func(string) (uint64, error) {
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
func threeEntries(t *testing.T, e *env, c *Cache) {
	t.Helper()
	v := testVersion(8)
	for _, n := range []string{"a", "b", "c"} {
		fill(t, c, testKey(n), v, 0, 1)
		e.clock.Advance(time.Hour)
	}
	h := acquire(t, c, testKey("a"), v)
	if _, err := h.ReadAt(make([]byte, 1), 0); err != nil {
		t.Fatal(err)
	}
	release(t, h)
}

func TestEvictLeastRecentlyReadFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	threeEntries(t, e, c)
	names := []string{"a", "b", "c"}

	e.clock.Advance(time.Hour)
	c.opts.MaxSize = 16
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, c, names, true, false, true)
	if u := c.Usage(); u.Entries != 2 || u.Bytes != 16 {
		t.Errorf("Usage = %+v", u)
	}
	mustNotExist(t, baseName(e.opts.Dir, testKey("b"))+dataExt)
	mustNotExist(t, baseName(e.opts.Dir, testKey("b"))+metaExt)

	c.opts.MaxSize = 8
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, c, names, true, false, false)
}

func TestEvictByAge(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxAge = time.Hour
	c := e.open()
	v := testVersion(8)
	fill(t, c, testKey("old"), v, 0)
	e.clock.Advance(30 * time.Minute)
	fill(t, c, testKey("new"), v, 0)

	e.clock.Advance(30 * time.Minute) // old: one hour, which is not older than the limit
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, c, []string{"old", "new"}, true, true)

	e.clock.Advance(time.Second)
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, c, []string{"old", "new"}, false, true)
}

func TestEvictNeverRemovesPinnedEntries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	threeEntries(t, e, c)
	e.clock.Advance(time.Hour)
	c.opts.MaxSize = 0
	c.opts.MaxAge = 0
	held := acquire(t, c, testKey("b"), testVersion(8))
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, c, []string{"a", "b", "c"}, false, true, false)
	if got := readAll(t, held); string(got) != string(content(8)) {
		t.Errorf("pinned entry lost data: %v", got)
	}
	release(t, held)
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	if u := c.Usage(); u.Entries != 0 || u.Bytes != 0 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestUnlimitedSettingsNeverEvict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxSize = math.MaxInt64
	e.opts.MaxAge = math.MaxInt64
	e.opts.MinFree = 0
	e.opts.FreeSpace = func(string) (uint64, error) {
		t.Error("free space read although MinFree is 0")
		return 0, nil
	}
	c := e.open()
	threeEntries(t, e, c)
	e.clock.Advance(100 * 365 * 24 * time.Hour)
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	if u := c.Usage(); u.Entries != 3 || u.Bytes != 24 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestEvictTiesBreakByName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.open()
	v := testVersion(8)
	fill(t, c, testKey("x"), v, 0, 1)
	fill(t, c, testKey("y"), v, 0, 1)
	first, second := "x", "y"
	if baseName(e.opts.Dir, testKey(first)) > baseName(e.opts.Dir, testKey(second)) {
		first, second = second, first
	}
	c.opts.MaxSize = 8
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	wantCached(t, c, []string{first, second}, false, true)
}

func TestEvictReportsRemovalFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.openFS(faultFS{fail: failOn("remove", dataExt)})
	fill(t, c, testKey("a"), testVersion(8), 0)
	c.opts.MaxSize = 0
	if err := c.Evict(); !errors.Is(err, errInjected) {
		t.Errorf("Evict = %v, want the injected failure", err)
	}
	if u := c.Usage(); u.Entries != 0 {
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

	d := acquire(t, c, testKey("d"), testVersion(8))
	writeBlocks(t, d, 0)
	wantCached(t, c, []string{"a", "b", "c", "d"}, true, false, true, true)
	if !d.Has(0) {
		t.Error("block not stored after eviction made room")
	}
	release(t, d)
}

func TestMinFreeReturnsErrNoSpace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.FreeSpace = diskOf(t, 32)
	e.opts.MinFree = 8
	c := e.open()
	threeEntries(t, e, c)
	var pins []*Entry
	for _, n := range []string{"a", "b", "c"} {
		pins = append(pins, acquire(t, c, testKey(n), testVersion(8)))
	}
	d := acquire(t, c, testKey("d"), testVersion(8))
	if err := d.WriteBlock(0, block(d.Version(), 0)); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("WriteBlock = %v, want ErrNoSpace", err)
	}
	if d.Has(0) || c.Usage().Bytes != 24 {
		t.Error("a refused block was stored")
	}
	for _, p := range pins {
		release(t, p)
	}
	// With the others unpinned, eviction makes room and the same write succeeds.
	if err := d.WriteBlock(0, block(d.Version(), 0)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	release(t, d)
}

func TestMinFreeEvictionFailureStillRefuses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.FreeSpace = diskOf(t, 32)
	e.opts.MinFree = 8
	var armed atomicFlag
	c := e.openFS(faultFS{fail: func(op, name string) error {
		if armed.on() {
			return failOn("remove", dataExt)(op, name)
		}
		return nil
	}})
	threeEntries(t, e, c)
	d := acquire(t, c, testKey("d"), testVersion(8))
	armed.set()
	if err := d.WriteBlock(0, block(d.Version(), 0)); !errors.Is(err, ErrNoSpace) {
		t.Errorf("WriteBlock = %v, want ErrNoSpace", err)
	}
	mustContain(t, e.logs, "evict for free space")
}

func TestFreeSpaceFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MinFree = 8
	e.opts.MaxSize = 0
	e.opts.FreeSpace = func(string) (uint64, error) { return 0, errInjected }
	c := e.open()
	h := acquire(t, c, testKey("a"), testVersion(8))
	err := h.WriteBlock(0, block(h.Version(), 0))
	if !errors.Is(err, ErrNoSpace) || !errors.Is(err, errInjected) {
		t.Errorf("WriteBlock = %v, want ErrNoSpace wrapping the cause", err)
	}

	// Eviction cannot tell whether space is short, so it only applies the other limits.
	c.opts.MaxSize = math.MaxInt64
	release(t, h)
	if err := c.Evict(); err != nil {
		t.Fatal(err)
	}
	mustContain(t, e.logs, "read free space")
	if u := c.Usage(); u.Entries != 1 {
		t.Errorf("Usage = %+v", u)
	}
}

func TestWriteOverMaxSizeEvicts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxSize = 8
	c := e.open()
	fill(t, c, testKey("old"), testVersion(8), 0, 1)
	h := acquire(t, c, testKey("new"), testVersion(8))
	writeBlocks(t, h, 0)
	wantCached(t, c, []string{"old", "new"}, false, true)
	if u := c.Usage(); u.Bytes != 4 {
		t.Errorf("Usage = %+v", u)
	}
	release(t, h)
}

func TestRoomForClampsHugeFreeSpace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MinFree = 1
	e.opts.FreeSpace = func(string) (uint64, error) { return math.MaxUint64, nil }
	c := e.open()
	if room, err := c.roomFor(1 << 40); err != nil || !room {
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
