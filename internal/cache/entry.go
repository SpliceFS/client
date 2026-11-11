package cache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// persistEvery is how many newly written blocks of one entry are written to disk before
// its metadata is persisted again.
const persistEvery = 64

// entry is the shared state of one cached file. Several Entry handles can pin it.
type entry struct {
	key     Key
	base    string
	version Version

	// Guarded by Cache.mu.
	bitmap    []byte
	present   int64
	bytes     int64
	lastRead  time.Time
	validated time.Time
	pins      int
	f         dataFile
	fresh     int
	seq       uint64

	// pmu serializes writing and removing the entry's files, so that a late persist
	// cannot resurrect the metadata of a removed entry.
	pmu       sync.Mutex
	persisted uint64
	// removed is written holding both Cache.mu and pmu and read holding either.
	removed bool
}

func newEntry(dir string, key Key, v Version, now time.Time) *entry {
	return &entry{
		key:       key,
		base:      baseName(dir, key),
		version:   v,
		bitmap:    make([]byte, bitmapLen(v.blocks())),
		lastRead:  now,
		validated: now,
	}
}

// openLocked opens the data file if it is not open yet.
func (e *entry) openLocked(fsys fileSystem, flag int) error {
	if e.f != nil {
		return nil
	}
	f, err := fsys.OpenFile(e.base+dataExt, flag)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	e.f = f
	return nil
}

func (e *entry) closeFileLocked() error {
	f := e.f
	e.f = nil
	if f == nil {
		return nil
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close data file: %w", err)
	}
	return nil
}

// pinLocked returns the entry for key and version with its data file open and one more
// pin. The caller has already removed any entry of a different version.
func (c *Cache) pinLocked(key Key, v Version) (*entry, error) {
	e, ok := c.entries[key]
	if ok {
		if err := e.openLocked(c.fs, os.O_RDWR); err != nil {
			// A data file that vanished would otherwise be recreated empty while
			// the bitmap still claims its blocks.
			c.log.Warn("cached entry unusable, discarding", "cluster", key.Cluster, "path", key.Path, "error", err)
			if err := c.removeLocked(e); err != nil {
				return nil, fmt.Errorf("discard unusable entry: %w", err)
			}
			ok = false
		}
	}
	if !ok {
		var err error
		if e, err = c.createLocked(key, v); err != nil {
			return nil, err
		}
	}
	e.pins++
	return e, nil
}

func (c *Cache) createLocked(key Key, v Version) (*entry, error) {
	e := newEntry(c.opts.Dir, key, v, c.opts.Now())
	if err := c.fs.MkdirAll(filepath.Dir(e.base)); err != nil {
		return nil, fmt.Errorf("create cache subdirectory: %w", err)
	}
	if err := e.openLocked(c.fs, os.O_RDWR|os.O_CREATE|os.O_TRUNC); err != nil {
		return nil, err
	}
	c.entries[key] = e
	return e, nil
}

// drop removes one pin and closes the data file when the last one is gone.
func (c *Cache) drop(e *entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.pins--
	if e.pins > 0 {
		return nil
	}
	return e.closeFileLocked()
}

// Entry is one holder's pin on a cached file. It is safe for concurrent use, except that
// no method may be called concurrently with or after Release.
type Entry struct {
	c        *Cache
	e        *entry
	released atomic.Bool
}

// live returns the open data file, or an error if the handle or the cache is finished.
func (h *Entry) live() (dataFile, error) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	switch {
	case h.released.Load():
		return nil, ErrReleased
	case h.c.closed:
		return nil, ErrClosed
	}
	return h.e.f, nil
}

// Version returns the version of the file this entry holds.
func (h *Entry) Version() Version { return h.e.version }

// Has reports whether block is present. It is false for a block out of range.
func (h *Entry) Has(block int64) bool {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.e.version.blockLen(block) >= 0 && hasBit(h.e.bitmap, block)
}

// Complete reports whether every block of the file is present.
func (h *Entry) Complete() bool {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.e.present == h.e.version.blocks()
}

// ReadAt reads from the file like os.File.ReadAt, serving only blocks that are present.
// If any block in the range is missing it returns ErrNotCached and no data. A read that
// reaches the end of the file returns the bytes available and io.EOF.
func (h *Entry) ReadAt(p []byte, off int64) (int, error) {
	f, err := h.live()
	if err != nil {
		return 0, err
	}
	v := h.e.version
	switch {
	case off < 0:
		return 0, fmt.Errorf("cache: negative offset %d", off)
	case len(p) == 0:
		return 0, nil
	case off >= v.Size:
		return 0, io.EOF
	}
	buf := p[:min(int64(len(p)), v.Size-off)]
	if err := h.c.touch(h.e, off/v.BlockSize, (off+int64(len(buf))-1)/v.BlockSize); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(io.NewSectionReader(f, off, int64(len(buf))), buf); err != nil {
		return 0, fmt.Errorf("read data file: %w", err)
	}
	if len(buf) < len(p) {
		return len(buf), io.EOF
	}
	return len(buf), nil
}

// touch records a read of blocks first through last, or returns ErrNotCached if any of
// them is missing.
func (c *Cache) touch(e *entry, first, last int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for b := first; b <= last; b++ {
		if !hasBit(e.bitmap, b) {
			return fmt.Errorf("%w: block %d of %s", ErrNotCached, b, e.key.Path)
		}
	}
	e.lastRead = c.opts.Now()
	return nil
}

// WriteBlock stores a block. data must be exactly BlockSize long, or the true length of
// the last block. Writing a block that is already present does nothing. It returns
// ErrNoSpace, after trying to evict, when storing the block would leave less than MinFree
// bytes free or the disk is full; the caller then passes the data through.
func (h *Entry) WriteBlock(block int64, data []byte) error {
	f, err := h.live()
	if err != nil {
		return err
	}
	v := h.e.version
	want := v.blockLen(block)
	if want < 0 || int64(len(data)) != want {
		return fmt.Errorf("%w: block %d with %d bytes", ErrInvalidBlock, block, len(data))
	}
	if h.Has(block) {
		return nil
	}
	if err := h.c.reserve(want); err != nil {
		return err
	}
	if _, err := f.WriteAt(data, block*v.BlockSize); err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			return fmt.Errorf("%w: %w", ErrNoSpace, err)
		}
		return fmt.Errorf("write block %d: %w", block, err)
	}
	h.c.stored(h.e, block, want)
	return nil
}

// stored marks a block present after its data was written, then persists and evicts if
// that is due.
func (c *Cache) stored(e *entry, block, size int64) {
	c.mu.Lock()
	if hasBit(e.bitmap, block) {
		c.mu.Unlock()
		return
	}
	setBit(e.bitmap, block)
	e.present++
	e.bytes += size
	e.fresh++
	if !e.removed {
		c.total += size
	}
	var snap snapshot
	persist := e.fresh >= persistEvery
	if persist {
		snap = e.snapshotLocked()
	}
	over := c.total > c.opts.MaxSize
	c.mu.Unlock()

	if persist {
		c.warn("persist metadata", snap.write(c.fs), "path", e.key.Path)
	}
	if over {
		c.warn("evict over size limit", c.Evict())
	}
}

// warn logs err, if there is one, as a failure that does not fail the operation.
func (c *Cache) warn(what string, err error, attrs ...any) {
	if err != nil {
		c.log.Warn(what, append(attrs, "error", err)...)
	}
}

// Release unpins the entry and persists its metadata. Using the Entry afterwards returns
// ErrReleased.
func (h *Entry) Release() error {
	if !h.released.CompareAndSwap(false, true) {
		return ErrReleased
	}
	c := h.c
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	snap := h.e.snapshotLocked()
	c.mu.Unlock()
	return errors.Join(snap.write(c.fs), c.drop(h.e))
}
