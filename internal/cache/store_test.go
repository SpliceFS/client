package cache

import (
	"slices"
	"strings"
	"sync"
	"testing"
)

// diskLog records the syncs and renames a cache makes, by operation and file extension, so
// a test can check which writes are made durable and in which order.
type diskLog struct {
	mu   sync.Mutex
	ops  []string
	fail map[string]bool
}

func (l *diskLog) record(operation, name string) error {
	if operation != "sync" && operation != "rename" {
		return nil
	}
	exts := []string{tmpExt, metaExt, dataExt}
	i := slices.IndexFunc(exts, func(ext string) bool { return strings.HasSuffix(name, ext) })
	l.mu.Lock()
	defer l.mu.Unlock()
	got := operation + " " + exts[i]
	if l.fail[got] {
		delete(l.fail, got)
		return errInjected
	}
	l.ops = append(l.ops, got)
	return nil
}

// take returns the operations recorded since the last call.
func (l *diskLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ops := l.ops
	l.ops = nil
	return ops
}

// durableWrite is what a metadata write that records new blocks does to the disk.
func durableWrite() []string {
	return []string{"sync " + dataExt, "sync " + tmpExt, "rename " + metaExt}
}

// timeWrite is what a metadata write that changes only times does to the disk.
func timeWrite() []string { return []string{"rename " + metaExt} }

func expectOps(t *testing.T, log *diskLog, step string, want []string) {
	t.Helper()
	if got := log.take(); !slices.Equal(got, want) {
		t.Errorf("%s: disk operations %q, want %q", step, got, want)
	}
}

func TestOnlyNewBlocksAreSynced(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	log := &diskLog{}
	cache := env.openFS(faultFS{fail: log.record})
	key := testKey("a")
	version := testVersion(8)

	handle := acquire(t, cache, key, version)
	writeBlocks(t, handle, 0)
	release(t, handle)
	expectOps(t, log, "release after a new block", durableWrite())

	release(t, acquire(t, cache, key, version))
	expectOps(t, log, "release of a cached file", timeWrite())

	cache.MarkValidated(key)
	expectOps(t, log, "validation", timeWrite())

	handle = acquire(t, cache, key, version)
	writeBlocks(t, handle, 1)
	release(t, handle)
	expectOps(t, log, "release after another new block", durableWrite())
}

func TestFailedBlockWriteIsSyncedLater(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	log := &diskLog{fail: map[string]bool{"rename " + metaExt: true}}
	cache := env.openFS(faultFS{fail: log.record})
	key := testKey("a")
	version := testVersion(8)

	handle := acquire(t, cache, key, version)
	writeBlocks(t, handle, 0)
	if err := handle.Release(); err == nil {
		t.Fatal("Release succeeded despite the injected failure")
	}
	log.take()

	// The metadata on disk still names no block, so this write is the first that does.
	release(t, acquire(t, cache, key, version))
	expectOps(t, log, "release after a failed write", durableWrite())
}

func TestLoadedEntryIsNotResynced(t *testing.T) {
	t.Parallel()
	env := newEnv(t)
	key := testKey("a")
	version := testVersion(8)
	first := env.open()
	fill(t, first, key, version, 0, 1)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	log := &diskLog{}
	cache := env.openFS(faultFS{fail: log.record})
	release(t, acquire(t, cache, key, version))
	expectOps(t, log, "release of a loaded entry", timeWrite())
}
