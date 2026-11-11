// Package cache is the on-disk block cache of the SpliceFS client.
//
// Each cached remote file is one sparse data file plus one small metadata file that
// records which blocks are present. The package stores and evicts; it never talks to the
// network and knows nothing about nodes or mounts. The remote file is identified by a Key
// and a particular version of it by a Version, so a file that changes gets a fresh entry.
//
// The bitmap stored on disk never claims a block whose data has not been written: a crash
// can lose recent blocks, which are then fetched again, but cannot invent them.
package cache

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Errors returned by the cache. Compare them with errors.Is.
var (
	// ErrNoSpace means a block was not stored because of the MinFree limit or a full
	// disk. The caller passes the data through without caching it.
	ErrNoSpace = errors.New("cache: no space for block")
	// ErrNotCached means a read touched a block that is not present.
	ErrNotCached = errors.New("cache: block not cached")
	// ErrClosed means the cache has been closed.
	ErrClosed = errors.New("cache: closed")
	// ErrReleased means an Entry was used after its Release.
	ErrReleased = errors.New("cache: entry released")
	// ErrInvalidBlock means a block index is out of range or its data has the wrong length.
	ErrInvalidBlock = errors.New("cache: invalid block")
	// ErrInvalidVersion means a Version cannot describe a cacheable file.
	ErrInvalidVersion = errors.New("cache: invalid version")
)

// Options configures a Cache.
type Options struct {
	// Dir is the cache directory. It is created with mode 0700 if it does not exist.
	Dir string
	// MaxSize is the most bytes of cached data to keep. math.MaxInt64 means no limit.
	MaxSize int64
	// MaxAge evicts entries not read for this long. math.MaxInt64 means never.
	MaxAge time.Duration
	// MinFree is the number of bytes to keep free on the cache disk. Zero disables the
	// check.
	MinFree int64
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// FreeSpace returns the bytes available to the cache on the disk holding dir. Nil
	// means the real statfs.
	FreeSpace func(dir string) (uint64, error)
	// Logger receives diagnostics. Nil discards them.
	Logger *slog.Logger
}

// Usage is the size of the cache.
type Usage struct {
	// Entries is the number of cached files.
	Entries int
	// Bytes is the number of bytes of blocks present, not the apparent file sizes.
	Bytes int64
}

// Info describes what is cached for one key.
type Info struct {
	// Version is the version of the file that is cached.
	Version Version
	// Validated is when the version was last confirmed against the nodes.
	Validated time.Time
	// LastRead is when the entry was last read.
	LastRead time.Time
	// Complete is true when every block of the file is present.
	Complete bool
	// Bytes is the number of bytes of blocks present.
	Bytes int64
}

// Cache is a directory of cached files. It is safe for concurrent use.
//
// Lock order: Cache.mu, then entry.pmu. Nothing takes Cache.mu while holding pmu.
type Cache struct {
	opts Options
	fs   fileSystem
	log  *slog.Logger

	mu      sync.Mutex
	closed  bool
	entries map[Key]*entry
	total   int64
}

// Open opens the cache in opts.Dir, creating the directory if needed, and loads the
// entries found there. Corrupt, orphaned and temporary files that the cache would have
// created are removed; other files are left alone. A damaged entry is dropped and logged;
// it never makes Open fail.
func Open(opts Options) (*Cache, error) {
	return open(opts, osFS{})
}

func open(opts Options, fsys fileSystem) (*Cache, error) {
	if opts.Dir == "" {
		return nil, errors.New("cache: directory not set")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FreeSpace == nil {
		opts.FreeSpace = statfsFree
	}
	c := &Cache{
		opts:    opts,
		fs:      fsys,
		log:     opts.Logger,
		entries: make(map[Key]*entry),
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if err := fsys.MkdirAll(opts.Dir); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	if err := c.scan(); err != nil {
		return nil, err
	}
	return c, nil
}

// Acquire returns the entry for exactly this version of the file, pinned until its
// Release. An existing entry for the key with a different version is discarded first;
// holders of that old entry keep reading their open files until they release.
func (c *Cache) Acquire(key Key, v Version) (*Entry, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if old, ok := c.entries[key]; ok && old.version != v {
		if err := c.removeLocked(old); err != nil {
			return nil, fmt.Errorf("discard old version: %w", err)
		}
	}
	e, err := c.pinLocked(key, v)
	if err != nil {
		return nil, err
	}
	return &Entry{c: c, e: e}, nil
}

// Peek reports what is cached for key without pinning it.
func (c *Cache) Peek(key Key) (Info, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.closed {
		return Info{}, false
	}
	return Info{
		Version:   e.version,
		Validated: e.validated,
		LastRead:  e.lastRead,
		Complete:  e.present == e.version.blocks(),
		Bytes:     e.bytes,
	}, true
}

// MarkValidated records that the cached version of key was just confirmed against the
// nodes, and persists that. It does nothing for an unknown key.
func (c *Cache) MarkValidated(key Key) {
	c.mu.Lock()
	e, ok := c.entries[key]
	if !ok || c.closed {
		c.mu.Unlock()
		return
	}
	e.validated = c.opts.Now()
	snap := e.snapshotLocked()
	// The pin keeps the data handle open while the snapshot is written.
	e.pins++
	c.mu.Unlock()

	err := errors.Join(snap.write(c.fs), c.drop(e))
	if err != nil {
		c.log.Warn("persist validation time", "cluster", key.Cluster, "path", key.Path, "error", err)
	}
}

// Discard removes the entry for key. If holders still have it pinned, its files are
// unlinked now and they keep working on their open handles until they release.
func (c *Cache) Discard(key Key) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	return c.removeLocked(e)
}

// Usage reports the number of entries and the bytes of blocks they hold.
func (c *Cache) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Usage{Entries: len(c.entries), Bytes: c.total}
}

// Close persists every entry and releases the cache's file handles. Entries still held
// by callers become unusable. Operations on a closed cache return ErrClosed.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.closed = true
	snaps := make([]snapshot, 0, len(c.entries))
	for _, e := range c.entries {
		snaps = append(snaps, e.snapshotLocked())
	}
	c.mu.Unlock()

	var errs []error
	for _, s := range snaps {
		errs = append(errs, s.write(c.fs))
	}
	c.mu.Lock()
	for _, e := range c.entries {
		errs = append(errs, e.closeFileLocked())
	}
	c.mu.Unlock()
	return errors.Join(errs...)
}
