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
			e := newEnv(t)
			var armed atomicFlag
			c := e.openFS(faultFS{fail: func(op, name string) error {
				if tt.fault == nil || !armed.on() {
					return nil
				}
				return tt.fault(op, name)
			}})
			key := testKey("a")
			h := acquire(t, c, key, testVersion(8))
			writeBlocks(t, h, 0, 1)
			if tt.corrupt != nil {
				tt.corrupt(t, baseName(e.opts.Dir, key)+dataExt)
			}
			armed.set()
			if _, err := h.ReadAt(make([]byte, 4), 0); !errors.Is(err, tt.want) {
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
			c := e.openFS(faultFS{fail: func(op, name string) error {
				if op == "writeat" && armed.on() && strings.HasSuffix(name, dataExt) {
					return tt.err
				}
				return nil
			}})
			h := acquire(t, c, testKey("a"), testVersion(8))
			armed.set()
			err := h.WriteBlock(0, block(h.Version(), 0))
			if !errors.Is(err, tt.err) || errors.Is(err, ErrNoSpace) != tt.wantSpace {
				t.Errorf("err = %v, want %v (ErrNoSpace: %v)", err, tt.err, tt.wantSpace)
			}
			if h.Has(0) || c.Usage().Bytes != 0 {
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
			e := newEnv(t)
			var armed atomicFlag
			fail := failOn(tt.op, tt.sfx)
			c := e.openFS(faultFS{fail: func(op, name string) error {
				if !armed.on() {
					return nil
				}
				return fail(op, name)
			}})
			key := testKey("a")
			h := acquire(t, c, key, testVersion(8))
			writeBlocks(t, h, 0)
			armed.set()
			if err := h.Release(); !errors.Is(err, errInjected) {
				t.Errorf("Release = %v, want the injected failure", err)
			}
			mustNotExist(t, baseName(e.opts.Dir, key)+tmpExt)
		})
	}
}

func TestReleaseReportsUnencodableMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.clock.t = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // outside what JSON can encode
	c := e.open()
	h := acquire(t, c, testKey("a"), testVersion(8))
	if err := h.Release(); err == nil || !strings.Contains(err.Error(), "encode metadata") {
		t.Errorf("Release = %v", err)
	}
}

func TestStaleSnapshotIsSkipped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var renames atomic.Int32
	c := e.openFS(faultFS{fail: func(op, _ string) error {
		if op == "rename" {
			renames.Add(1)
		}
		return nil
	}})
	h := acquire(t, c, testKey("a"), testVersion(8))
	c.mu.Lock()
	older := h.e.snapshotLocked()
	newer := h.e.snapshotLocked()
	c.mu.Unlock()
	for _, s := range []snapshot{newer, older} {
		if err := s.write(c.fs); err != nil {
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
	c := e.openFS(faultFS{fail: func(op, name string) error {
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
	h := acquire(t, c, testKey("a"), v)
	other = acquire(t, c, testKey("a"), v)
	armed.set()
	writeBlocks(t, h, 0)
	if u := c.Usage(); u.Bytes != 4 {
		t.Errorf("Usage = %+v, want 4 bytes", u)
	}
	if info, _ := c.Peek(testKey("a")); info.Bytes != 4 {
		t.Errorf("Peek = %+v, want 4 bytes", info)
	}
}

func TestWriteLogsBackgroundFailures(t *testing.T) {
	t.Parallel()
	t.Run("persist", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		c := e.openFS(faultFS{fail: failOn("rename", metaExt)})
		h := acquire(t, c, testKey("a"), testVersion(4*persistEvery))
		for i := range int64(persistEvery) {
			writeBlocks(t, h, i)
		}
		mustContain(t, e.logs, "persist metadata")
		if !h.Complete() {
			t.Error("a failed persist lost a stored block")
		}
	})
	t.Run("eviction", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.opts.MaxSize = 4
		var armed atomicFlag
		c := e.openFS(faultFS{fail: func(op, name string) error {
			if armed.on() {
				return failOn("remove", dataExt)(op, name)
			}
			return nil
		}})
		fill(t, c, testKey("old"), testVersion(4), 0)
		h := acquire(t, c, testKey("new"), testVersion(4))
		armed.set()
		writeBlocks(t, h, 0)
		mustContain(t, e.logs, "evict over size limit")
	})
}
