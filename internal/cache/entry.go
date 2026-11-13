package cache

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
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

func newEntry(dir string, key Key, version Version, now time.Time) *entry {
	return &entry{
		key:       key,
		base:      baseName(dir, key),
		version:   version,
		bitmap:    make([]byte, bitmapLen(version.blocks())),
		lastRead:  now,
		validated: now,
	}
}

// openLocked opens the data file if it is not open yet.
func (e *entry) openLocked(fsys fileSystem, flag int) error {
	if e.f != nil {
		return nil
	}
	file, err := fsys.OpenFile(e.base+dataExt, flag)
	if err != nil {
		return fmt.Errorf("open data file: %w", err)
	}
	e.f = file
	return nil
}

func (e *entry) closeFileLocked() error {
	file := e.f
	e.f = nil
	if file == nil {
		return nil
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close data file: %w", err)
	}
	return nil
}

// pinLocked returns the entry for key and version with its data file open and one more
// pin. The caller has already removed any entry of a different version.
func (c *Cache) pinLocked(key Key, version Version) (*entry, error) {
	cached, ok := c.entries[key]
	if ok {
		if err := cached.openLocked(c.fs, os.O_RDWR); err != nil {
			// A data file that vanished would otherwise be recreated empty while
			// the bitmap still claims its blocks.
			c.log.Warn("cached entry unusable, discarding",
				slog.String("cluster", key.Cluster), slog.String("path", key.Path), slog.Any("error", err))
			if err := c.removeLocked(cached); err != nil {
				return nil, fmt.Errorf("discard unusable entry: %w", err)
			}
			ok = false
		}
	}
	if !ok {
		var err error
		if cached, err = c.createLocked(key, version); err != nil {
			return nil, err
		}
	}
	cached.pins++
	return cached, nil
}

func (c *Cache) createLocked(key Key, version Version) (*entry, error) {
	created := newEntry(c.opts.Dir, key, version, c.opts.Now())
	if err := c.fs.MkdirAll(filepath.Dir(created.base)); err != nil {
		return nil, fmt.Errorf("create cache subdirectory: %w", err)
	}
	if err := created.openLocked(c.fs, os.O_RDWR|os.O_CREATE|os.O_TRUNC); err != nil {
		return nil, err
	}
	c.entries[key] = created
	return created, nil
}

// drop removes one pin and closes the data file when the last one is gone.
func (c *Cache) drop(cached *entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached.pins--
	if cached.pins > 0 {
		return nil
	}
	return cached.closeFileLocked()
}

// Entry is one holder's pin on a cached file. It is safe for concurrent use, except that
// no method may be called concurrently with or after Release.
type Entry struct {
	c        *Cache
	e        *entry
	released atomic.Bool
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
func (h *Entry) ReadAt(dst []byte, off int64) (int, error) {
	file, err := h.live()
	if err != nil {
		return 0, err
	}
	version := h.e.version
	switch {
	case off < 0:
		return 0, fmt.Errorf("%w %d", ErrNegativeOffset, off)
	case len(dst) == 0:
		return 0, nil
	case off >= version.Size:
		return 0, io.EOF
	}
	buf := dst[:min(int64(len(dst)), version.Size-off)]
	if err := h.c.touch(h.e, off/version.BlockSize, (off+int64(len(buf))-1)/version.BlockSize); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(io.NewSectionReader(file, off, int64(len(buf))), buf); err != nil {
		return 0, fmt.Errorf("read data file: %w", err)
	}
	if len(buf) < len(dst) {
		return len(buf), io.EOF
	}
	return len(buf), nil
}

// touch records a read of blocks first through last, or returns ErrNotCached if any of
// them is missing.
func (c *Cache) touch(cached *entry, first, last int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for b := first; b <= last; b++ {
		if !hasBit(cached.bitmap, b) {
			return fmt.Errorf("%w: block %d of %s", ErrNotCached, b, cached.key.Path)
		}
	}
	cached.lastRead = c.opts.Now()
	return nil
}

// WriteBlock stores a block. data must be exactly BlockSize long, or the true length of
// the last block. Writing a block that is already present does nothing. It returns
// ErrNoSpace, after trying to evict, when storing the block would leave less than MinFree
// bytes free or the disk is full; the caller then passes the data through.
func (h *Entry) WriteBlock(block int64, data []byte) error {
	file, err := h.live()
	if err != nil {
		return err
	}
	version := h.e.version
	want := version.blockLen(block)
	if want < 0 || int64(len(data)) != want {
		return fmt.Errorf("%w: block %d with %d bytes", ErrInvalidBlock, block, len(data))
	}
	if h.Has(block) {
		return nil
	}
	if err := h.c.reserve(want); err != nil {
		return err
	}
	if _, err := file.WriteAt(data, block*version.BlockSize); err != nil {
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
func (c *Cache) stored(cached *entry, block, size int64) {
	c.mu.Lock()
	if hasBit(cached.bitmap, block) {
		c.mu.Unlock()
		return
	}
	setBit(cached.bitmap, block)
	cached.present++
	cached.bytes += size
	cached.fresh++
	if !cached.removed {
		c.total += size
	}
	var snap snapshot
	persist := cached.fresh >= persistEvery
	if persist {
		snap = cached.snapshotLocked()
	}
	over := c.total > c.opts.MaxSize
	c.mu.Unlock()

	// A failure here does not fail the write: the block is stored, and the metadata or
	// the eviction is tried again later.
	if persist {
		if err := snap.write(c.fs); err != nil {
			c.log.Warn("persist metadata", slog.String("path", cached.key.Path), slog.Any("error", err))
		}
	}
	if over {
		if err := c.Evict(); err != nil {
			c.log.Warn("evict over size limit", slog.Any("error", err))
		}
	}
}

// Release unpins the entry and persists its metadata. Using the Entry afterwards returns
// ErrReleased.
func (h *Entry) Release() error {
	if !h.released.CompareAndSwap(false, true) {
		return ErrReleased
	}
	cache := h.c
	cache.mu.Lock()
	if cache.closed {
		cache.mu.Unlock()
		return ErrClosed
	}
	snap := h.e.snapshotLocked()
	cache.mu.Unlock()
	return errors.Join(snap.write(cache.fs), cache.drop(h.e))
}

// live returns the open data file, or an error if the handle or the cache is finished.
//
//nolint:ireturn // the data file is an interface so that tests can inject failures
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
