package fetch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Reader serves reads of one remote file from a set of sources. It is safe
// for concurrent use; the kernel issues read-ahead in parallel and out of
// order, and the Reader is built for that.
//
// All state is guarded by one mutex. Fetch goroutines do their network call
// without the lock and take it to record the result; timers call back under
// the same lock. Functions documented as "called with r.mu held" may release
// it while they wait.
type Reader struct {
	opts   Options
	store  Store
	meter  Meter
	clock  Clock
	trace  tracer
	memCap int // blocks kept in memory, unless reads wait for more
	bufs   sync.Pool
	wg     sync.WaitGroup // every goroutine and armed timer of the Reader

	mu           sync.Mutex
	closed       bool
	changed      chan struct{} // closed and replaced on every state change
	sources      []*source
	streak       streak
	blocks       map[int64]*block
	resident     int    // blocks holding data in memory
	storeFailed  bool   // a Store write failed; backfill stops
	tick         uint64 // use counter for least recently used eviction
	lowestWaited int64  // lowest block a read waits for, or -1
	ops          map[*op]struct{}
	probes       probeCache
	probing      map[int64]bool // probe chunks being fetched
}

// tracer observes the Reader's decisions. Tests use it to synchronize with
// the Reader without sleeping; in production it does nothing.
type tracer interface {
	fetchStarted(id string, block int64, hedge bool)
	fetchSettled(id string, block int64)
	waiting()
}

type noTrace struct{}

func (noTrace) fetchStarted(string, int64, bool) {}
func (noTrace) fetchSettled(string, int64)       {}
func (noTrace) waiting()                         {}

// New returns a Reader for a file of opts.Size bytes held by sources. store
// may be nil, in which case nothing is cached. meter is required.
func New(opts Options, sources []Source, store Store, meter Meter) (*Reader, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if meter == nil {
		return nil, errors.New("fetch: no meter")
	}
	opts = opts.withDefaults()
	r := &Reader{
		opts:         opts,
		store:        store,
		meter:        meter,
		clock:        opts.Clock,
		trace:        noTrace{},
		memCap:       windowSpan(opts.ReadAhead, opts.BlockSize) + 2,
		changed:      make(chan struct{}),
		blocks:       make(map[int64]*block),
		lowestWaited: -1,
		ops:          make(map[*op]struct{}),
		probing:      make(map[int64]bool),
	}
	r.bufs.New = func() any {
		buf := make([]byte, opts.BlockSize)
		return &buf
	}
	for _, s := range sources {
		r.sources = append(r.sources, &source{src: s, id: s.ID()})
	}
	return r, nil
}

// ReadAt reads len(p) bytes at off, with io.ReaderAt semantics: at the end of
// the file it returns the bytes there with io.EOF, and at or past the end 0
// and io.EOF. It fails with ErrUnavailable when no source delivers in time,
// with the context's error when ctx ends, and with ErrClosed after Close.
func (r *Reader) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fetch: read at negative offset %d", off)
	}
	r.mu.Lock()
	n, stored, err := r.serve(ctx, p, off)
	r.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if err := r.readStored(p[:n], off, stored); err != nil {
		return 0, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// serve fills as much of p as the file holds and returns how much that is and
// which blocks are still to be copied from the Store. Store reads are left to
// the caller so that disk I/O does not hold the lock. Called with r.mu held.
func (r *Reader) serve(ctx context.Context, p []byte, off int64) (int, []int64, error) {
	if r.closed {
		return 0, nil, ErrClosed
	}
	if off >= r.opts.Size {
		return 0, nil, io.EOF
	}
	n := min(int64(len(p)), r.opts.Size-off)
	if n == 0 {
		return 0, nil, nil
	}
	p = p[:n]
	sequential := r.track(off, off+n)
	o := r.beginOp(ctx)
	defer r.endOp(o)
	if sequential || r.covered(off, off+n) {
		stored, err := r.readBlocks(o, p, off)
		return len(p), stored, err
	}
	return len(p), nil, r.readProbe(o, p, off)
}

// track feeds a read into sequential detection and reports whether the
// Reader is in block mode. A read that starts a new streak cancels the block
// fetches nobody needs any more, backfill of the old streak included.
func (r *Reader) track(off, end int64) bool {
	if r.streak.joins(off, end, r.opts.Slack) {
		r.streak.pos = max(r.streak.pos, end)
	} else {
		r.streak = streak{started: true, start: off, pos: end}
		r.cancelOutside(r.keepRange())
	}
	return r.blockMode()
}

// readStored copies the parts of p that lie in the given blocks from the
// Store.
func (r *Reader) readStored(p []byte, off int64, blocks []int64) error {
	for _, b := range blocks {
		start, length := span(b, r.opts.BlockSize, r.opts.Size)
		lo, hi := max(off, start), min(off+int64(len(p)), start+length)
		if n, err := r.store.ReadAt(p[lo-off:hi-off], lo); int64(n) < hi-lo {
			return fmt.Errorf("fetch: read block %d from cache: %w", b, cmp.Or(err, io.ErrUnexpectedEOF))
		}
	}
	return nil
}

// Close cancels every fetch, stops every timer and makes waiting and future
// reads return ErrClosed. It returns once every goroutine the Reader started
// has exited. Closing twice does nothing.
func (r *Reader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	for _, b := range r.blocks {
		r.cancelFetches(b)
	}
	for _, s := range r.sources {
		r.stop(s.coolTimer)
		s.coolTimer = nil
	}
	for o := range r.ops {
		o.cancel()
	}
	r.broadcast()
	r.mu.Unlock()
	r.wg.Wait()
	return nil
}

// broadcast wakes every waiting read. Called with r.mu held.
func (r *Reader) broadcast() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// timer is an armed Clock timer that the Reader accounts for in r.wg until it
// has either fired or been stopped, so Close can wait for callbacks that are
// already running.
type timer struct {
	t       Timer
	stopped bool
}

// afterFunc arms a timer that calls f with r.mu held. Called with r.mu held,
// which also keeps f from running before t.t is set.
func (r *Reader) afterFunc(d time.Duration, f func()) *timer {
	t := &timer{}
	r.wg.Add(1)
	t.t = r.clock.AfterFunc(d, func() {
		defer r.wg.Done()
		r.mu.Lock()
		defer r.mu.Unlock()
		if !t.stopped {
			f()
		}
	})
	return t
}

// stop disarms t, which may be nil. A callback already waiting for the lock
// sees the stopped flag and does nothing. Called with r.mu held.
func (r *Reader) stop(t *timer) {
	if t == nil {
		return
	}
	t.stopped = true
	if t.t.Stop() {
		r.wg.Done()
	}
}

// op is one ReadAt call in progress.
type op struct {
	parent   context.Context
	ctx      context.Context // ends on parent's end, on timeout and on Close
	cancel   context.CancelFunc
	deadline *timer
	timedOut bool
}

func (r *Reader) beginOp(parent context.Context) *op {
	ctx, cancel := context.WithCancel(parent)
	o := &op{parent: parent, ctx: ctx, cancel: cancel}
	r.ops[o] = struct{}{}
	return o
}

func (r *Reader) endOp(o *op) {
	r.stop(o.deadline)
	delete(r.ops, o)
	o.cancel()
}

// armDeadline starts the timeout of o unless it is running. It runs only
// while o waits, and restarts with every piece of progress.
func (r *Reader) armDeadline(o *op) {
	if o.deadline != nil {
		return
	}
	o.deadline = r.afterFunc(r.opts.BlockTimeout, func() {
		o.deadline = nil
		o.timedOut = true
		o.cancel()
	})
}

// progressed restarts the timeout of o.
func (r *Reader) progressed(o *op) {
	r.stop(o.deadline)
	o.deadline = nil
}

// opErr returns why o must stop, or nil.
func (r *Reader) opErr(o *op) error {
	switch {
	case r.closed:
		return ErrClosed
	case o.timedOut:
		return ErrUnavailable
	default:
		return o.parent.Err()
	}
}

// wait blocks until the Reader's state changes or o ends. Called with r.mu
// held; releases it while blocked.
func (r *Reader) wait(o *op) {
	r.armDeadline(o)
	changed := r.changed
	r.trace.waiting()
	r.mu.Unlock()
	select {
	case <-changed:
	case <-o.ctx.Done():
	}
	r.mu.Lock()
}
