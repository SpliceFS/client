// Package fetch serves reads of one remote file by fetching it from several
// nodes at once.
//
// A Reader starts in probe mode: each read fetches only the small aligned
// chunks that cover it, so a media scanner reading headers and indexes across
// a large library does not download a block per file. Probe chunks are not
// cached, so isolated reads leave no trace in the Store.
//
// Once reads advance sequentially it switches to block mode and keeps whole
// blocks fetched ahead of the read position, spread over the sources by
// measured speed, with the block the reader needs next always on a fast
// source. Every block goes to the Store. On the switch, the blocks the streak
// had read through probe chunks are fetched whole as backfill, at lower
// priority than anything a read needs, so that a file read from start to end
// becomes a complete cache entry. A file no larger than the switch distance
// skips probe mode altogether, so a small file that is read is cached.
//
// A block that is late while a read waits for it is requested again from
// another source, and the first complete answer wins.
package fetch

import (
	"context"
	"errors"
	"time"
)

// Source is one node's copy of the file being read.
type Source interface {
	// ID names the node. It keys the node's entry in the Meter.
	ID() string
	// Fetch fills buf with the bytes at [off, off+len(buf)). It returns nil
	// only when buf is full, and returns promptly once ctx ends.
	Fetch(ctx context.Context, off int64, buf []byte) error
}

// Store is the file's cache entry. The Reader calls it while holding its own
// lock, so Has must be cheap and no method may call back into the Reader.
type Store interface {
	// Has reports whether the block with this index is present.
	Has(block int64) bool
	// ReadAt reads cached bytes. It is only valid across present blocks.
	ReadAt(p []byte, off int64) (int, error)
	// WriteBlock stores a complete block. It may fail, for example when the
	// disk is full; the Reader then keeps the data in memory.
	WriteBlock(block int64, data []byte) error
}

// Meter is the shared, long-lived record of how fast each source is. It
// outlives Readers and must be safe for concurrent use.
type Meter interface {
	// Observe records a successful fetch of bytes that took elapsed.
	Observe(id string, bytes int, elapsed time.Duration)
	// Failed records a failed fetch.
	Failed(id string)
	// Rate returns the estimated throughput of id and whether there is one.
	Rate(id string) (bytesPerSecond float64, known bool)
}

// Clock is the time source, so that tests control time.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// AfterFunc calls f in its own goroutine once d has passed.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc call.
type Timer interface {
	// Stop prevents the call and reports whether it did so. It returns false
	// when the call has already started or finished.
	Stop() bool
}

var (
	// ErrGone is returned by a Source that no longer has the version of the
	// file being read. The Reader never uses that source again.
	ErrGone = errors.New("fetch: source no longer has this version of the file")
	// ErrUnavailable means that no source could deliver the data in time.
	ErrUnavailable = errors.New("fetch: data unavailable from every source")
	// ErrClosed is returned by reads on a closed Reader.
	ErrClosed = errors.New("fetch: reader closed")
)
