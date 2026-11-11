package cache

import (
	"errors"
	"io"
	"sync"
	"testing"
)

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxSize = 1 << 20
	c := e.open()
	const (
		blocks  = 200
		workers = 8
	)
	key, v := testKey("shared"), testVersion(4*blocks-1) // short last block
	want := content(v.Size)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every worker holds its own pin and writes all blocks, each starting
			// at a different place, so that writes of one block overlap.
			h, err := c.Acquire(key, v)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			buf := make([]byte, 6)
			for i := range int64(blocks) {
				b := (i + int64(w)*blocks/workers) % blocks
				if err := h.WriteBlock(b, block(v, b)); err != nil {
					t.Errorf("WriteBlock(%d): %v", b, err)
				}
				off := b * v.BlockSize
				n, err := h.ReadAt(buf, off)
				if err != nil && !errors.Is(err, ErrNotCached) && !errors.Is(err, io.EOF) {
					t.Errorf("ReadAt(%d): %v", off, err)
				}
				if string(buf[:n]) != string(want[off:off+int64(n)]) {
					t.Errorf("ReadAt(%d) returned wrong data", off)
				}
				if i%50 == 0 {
					c.MarkValidated(key)
				}
				c.Peek(key)
				c.Usage()
			}
			if err := h.Release(); err != nil {
				t.Errorf("Release: %v", err)
			}
		}()
	}
	wg.Wait()

	info, ok := c.Peek(key)
	if !ok || !info.Complete || info.Bytes != v.Size {
		t.Fatalf("Peek = %+v, %v", info, ok)
	}
	h := acquire(t, c, key, v)
	if got := readAll(t, h); string(got) != string(want) {
		t.Error("final content differs")
	}
	release(t, h)
}

func TestConcurrentAcquireAndEvict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.opts.MaxSize = 16
	c := e.open()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, v := testKey(string(rune('a'+w))), testVersion(8)
			for range 25 {
				h, err := c.Acquire(key, v)
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				for b := range int64(2) {
					if err := h.WriteBlock(b, block(v, b)); err != nil {
						t.Errorf("WriteBlock: %v", err)
					}
				}
				if err := h.Release(); err != nil {
					t.Errorf("Release: %v", err)
				}
				if err := c.Evict(); err != nil {
					t.Errorf("Evict: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if u := c.Usage(); u.Bytes > 16 {
		t.Errorf("Usage = %+v exceeds the limit", u)
	}
}
