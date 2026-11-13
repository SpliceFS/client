package cache

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestReadAtFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fault   func(op, name string) error
		corrupt func(t *testing.T, path string)
		want    error
	}{
		{name: "read error", fault: failOn("readat", dataExt), want: errInjected},
		{
			name: "data file shorter than the bitmap claims",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			},
			want: io.EOF,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newEnv(t)
			var armed atomicFlag
			cache := env.openFS(faultFS{fail: func(op, name string) error {
				if tt.fault == nil || !armed.on() {
					return nil
				}
				return tt.fault(op, name)
			}})
			key := testKey("a")
			handle := acquire(t, cache, key, testVersion(8))
			writeBlocks(t, handle, 0, 1)
			if tt.corrupt != nil {
				tt.corrupt(t, baseName(env.opts.Dir, key)+dataExt)
			}
			armed.set()
			if _, err := handle.ReadAt(make([]byte, 4), 0); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWriteBlockDiskFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		wantSpace bool
	}{
		{"disk full", syscall.ENOSPC, true},
		{"other error", errInjected, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			var armed atomicFlag
			cache := e.openFS(faultFS{fail: func(op, name string) error {
				if op == "writeat" && armed.on() && strings.HasSuffix(name, dataExt) {
					return tt.err
				}
				return nil
			}})
			handle := acquire(t, cache, testKey("a"), testVersion(8))
			armed.set()
			err := handle.WriteBlock(0, block(handle.Version(), 0))
			if !errors.Is(err, tt.err) || errors.Is(err, ErrNoSpace) != tt.wantSpace {
				t.Errorf("err = %v, want %v (ErrNoSpace: %v)", err, tt.err, tt.wantSpace)
			}
			if handle.Has(0) || cache.Usage().Bytes != 0 {
				t.Error("a block that was not written is marked present")
			}
		})
	}
}

func TestReleaseReportsFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		op   string
		sfx  string
	}{
		{"sync data file", "sync", dataExt},
		{"create temporary file", "open", tmpExt},
		{"write temporary file", "writeat", tmpExt},
		{"sync temporary file", "sync", tmpExt},
		{"close temporary file", "close", tmpExt},
		{"rename", "rename", metaExt},
		{"close data file", "close", dataExt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newEnv(t)
			var armed atomicFlag
			fail := failOn(tt.op, tt.sfx)
			cache := env.openFS(faultFS{fail: func(op, name string) error {
				if !armed.on() {
					return nil
				}
				return fail(op, name)
			}})
			key := testKey("a")
			h := acquire(t, cache, key, testVersion(8))
			writeBlocks(t, h, 0)
			armed.set()
			if err := h.Release(); !errors.Is(err, errInjected) {
				t.Errorf("Release = %v, want the injected failure", err)
			}
			mustNotExist(t, baseName(env.opts.Dir, key)+tmpExt)
		})
	}
}

func TestReleaseReportsUnencodableMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.clock.t = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // outside what JSON can encode
	cache := e.open()
	key := testKey("a")
	h := acquire(t, cache, key, testVersion(8))
	if err := h.Release(); err == nil || !strings.Contains(err.Error(), "encode metadata") {
		t.Errorf("Release = %v", err)
	}
	// Close at the end of the test would fail to encode this entry too.
	if err := cache.Discard(key); err != nil {
		t.Errorf("Discard: %v", err)
	}
}

func TestStaleSnapshotIsSkipped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var renames atomic.Int32
	cache := e.openFS(faultFS{fail: func(op, _ string) error {
		if op == "rename" {
			renames.Add(1)
		}
		return nil
	}})
	h := acquire(t, cache, testKey("a"), testVersion(8))
	cache.mu.Lock()
	older := h.e.snapshotLocked()
	newer := h.e.snapshotLocked()
	cache.mu.Unlock()
	for _, s := range []snapshot{newer, older} {
		if err := s.write(cache.fs); err != nil {
			t.Fatal(err)
		}
	}
	if got := renames.Load(); got != 1 {
		t.Errorf("metadata written %d times, want once", got)
	}
}

func TestConcurrentWritesOfOneBlockCountOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var other *Entry
	var armed atomicFlag
	cache := e.openFS(faultFS{fail: func(op, name string) error {
		if op == "writeat" && armed.on() && strings.HasSuffix(name, dataExt) {
			armed.v.Store(false)
			// The same block lands while this write is still in flight.
			if err := other.WriteBlock(0, block(other.Version(), 0)); err != nil {
				return err
			}
		}
		return nil
	}})
	v := testVersion(8)
	h := acquire(t, cache, testKey("a"), v)
	other = acquire(t, cache, testKey("a"), v)
	armed.set()
	writeBlocks(t, h, 0)
	if u := cache.Usage(); u.Bytes != 4 {
		t.Errorf("Usage = %+v, want 4 bytes", u)
	}
	if info, _ := cache.Peek(testKey("a")); info.Bytes != 4 {
		t.Errorf("Peek = %+v, want 4 bytes", info)
	}
}

func TestWriteLogsBackgroundFailures(t *testing.T) {
	t.Parallel()
	t.Run("persist", func(t *testing.T) {
		t.Parallel()
		env := newEnv(t)
		c := env.openFS(faultFS{fail: failOn("rename", metaExt)})
		h := acquire(t, c, testKey("a"), testVersion(4*persistEvery))
		for i := range int64(persistEvery) {
			writeBlocks(t, h, i)
		}
		mustContain(t, env.logs, "persist metadata")
		if !h.Complete() {
			t.Error("a failed persist lost a stored block")
		}
	})
	t.Run("eviction", func(t *testing.T) {
		t.Parallel()
		env := newEnv(t)
		env.opts.MaxSize = 4
		var armed atomicFlag
		cache := env.openFS(faultFS{fail: func(op, name string) error {
			if armed.on() {
				return failOn("remove", dataExt)(op, name)
			}
			return nil
		}})
		fill(t, cache, testKey("old"), testVersion(4), 0)
		h := acquire(t, cache, testKey("new"), testVersion(4))
		armed.set()
		writeBlocks(t, h, 0)
		mustContain(t, env.logs, "evict over size limit")
	})
}
