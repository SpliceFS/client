package cache

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
)

const (
	sharedBlocks  = 200
	sharedWorkers = 8
	// sharedValidateEvery is how many blocks a worker handles between two validations.
	sharedValidateEvery = 50
)

// sharedFile is one file that several workers write and read at the same time.
type sharedFile struct {
	cache   *Cache
	key     Key
	version Version
	want    []byte
}

// useBlock writes one block, reads it back and calls the other operations a reader
// would, so that they overlap with the writes of the other workers.
func (f sharedFile) useBlock(t *testing.T, handle *Entry, buf []byte, index, step int64) {
	t.Helper()
	if err := handle.WriteBlock(index, block(f.version, index)); err != nil {
		t.Errorf("WriteBlock(%d): %v", index, err)
	}
	off := index * f.version.BlockSize
	n, err := handle.ReadAt(buf, off)
	if err != nil && !errors.Is(err, ErrNotCached) && !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt(%d): %v", off, err)
	}
	if !bytes.Equal(buf[:n], f.want[off:off+int64(n)]) {
		t.Errorf("ReadAt(%d) returned wrong data", off)
	}
	if step%sharedValidateEvery == 0 {
		f.cache.MarkValidated(f.key)
	}
	f.cache.Peek(f.key)
	f.cache.Usage()
}

// work holds its own pin and writes all blocks, each worker starting at a different
// place, so that writes of one block overlap.
func (f sharedFile) work(t *testing.T, worker int) {
	t.Helper()
	handle, err := f.cache.Acquire(f.key, f.version)
	if err != nil {
		t.Errorf("Acquire: %v", err)
		return
	}
	buf := make([]byte, 6)
	for step := range int64(sharedBlocks) {
		f.useBlock(t, handle, buf, (step+int64(worker)*sharedBlocks/sharedWorkers)%sharedBlocks, step)
	}
	if err := handle.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxSize = 1 << 20
	cache := e.open()
	version := testVersion(4*sharedBlocks - 1) // short last block
	file := sharedFile{cache: cache, key: testKey("shared"), version: version, want: content(version.Size)}

	var group sync.WaitGroup
	for worker := range sharedWorkers {
		group.Go(func() { file.work(t, worker) })
	}
	group.Wait()

	info, ok := cache.Peek(file.key)
	if !ok || !info.Complete || info.Bytes != version.Size {
		t.Fatalf("Peek = %+v, %v", info, ok)
	}
	handle := acquire(t, cache, file.key, version)
	if got := readAll(t, handle); !bytes.Equal(got, file.want) {
		t.Error("final content differs")
	}
	release(t, handle)
}

// churn acquires, fills and releases the file named by worker many times, evicting after
// each round.
func churn(t *testing.T, cache *Cache, worker int) {
	t.Helper()
	key, version := testKey(string(rune('a'+worker))), testVersion(8)
	for range 25 {
		handle, err := cache.Acquire(key, version)
		if err != nil {
			t.Errorf("Acquire: %v", err)
			return
		}
		for index := range int64(2) {
			if err := handle.WriteBlock(index, block(version, index)); err != nil {
				t.Errorf("WriteBlock: %v", err)
			}
		}
		if err := handle.Release(); err != nil {
			t.Errorf("Release: %v", err)
		}
		if err := cache.Evict(); err != nil {
			t.Errorf("Evict: %v", err)
		}
	}
}

func TestConcurrentAcquireAndEvict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxSize = 16
	cache := e.open()
	var group sync.WaitGroup
	for worker := range 4 {
		group.Go(func() { churn(t, cache, worker) })
	}
	group.Wait()
	if usage := cache.Usage(); usage.Bytes > 16 {
		t.Errorf("Usage = %+v exceeds the limit", usage)
	}
}
