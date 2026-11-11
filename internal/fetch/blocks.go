package fetch

import (
	"context"
	"slices"
	"time"
)

// block is the Reader's state for one block that is in flight, held in
// memory, or waited for. Blocks with none of those are dropped from r.blocks.
type block struct {
	index      int64
	fetches    []*fetch // in flight; two while hedged
	arriving   bool     // a fetch won and its data is being stored
	buf        *[]byte  // pooled buffer behind data
	data       []byte   // the block's bytes while held in memory
	waiters    int      // reads waiting for this block
	used       uint64   // r.tick at the last arrival or read
	hedgeTimer *timer
}

func (b *block) inFlight() bool { return len(b.fetches) > 0 || b.arriving }

func (b *block) idle() bool { return !b.inFlight() && b.data == nil && b.waiters == 0 }

func (b *block) fetchingFrom(s *source) bool {
	return slices.ContainsFunc(b.fetches, func(f *fetch) bool { return f.src == s })
}

// fetch is one request for a block.
type fetch struct {
	src       *source
	block     *block
	hedge     bool // runs in the source's extra hedge slot
	cancel    context.CancelFunc
	cancelled bool // by the Reader; the outcome is ignored
	buf       *[]byte
	data      []byte
	started   time.Time
}

// slot is the counter of the source slot f occupies.
func (f *fetch) slot() *int {
	if f.hedge {
		return &f.src.hedges
	}
	return &f.src.active
}

// readBlocks copies the blocks covering p from memory, waiting for missing
// ones, and returns the blocks to be copied from the Store. Called with r.mu
// held.
func (r *Reader) readBlocks(o *op, p []byte, off int64) ([]int64, error) {
	pending := pieces(off, off+int64(len(p)), r.opts.BlockSize)
	for _, b := range pending {
		r.block(b).waiters++
	}
	defer func() { r.abandon(pending) }()
	var stored []int64
	for {
		before := len(pending)
		rest, inStore := r.collect(p, off, pending)
		pending, stored = rest, append(stored, inStore...)
		// Scheduled even when the read is complete: the read may have moved
		// the window, and the read-ahead must follow it.
		r.schedule()
		if len(pending) == 0 {
			return stored, nil
		}
		if err := r.opErr(o); err != nil {
			return nil, err
		}
		if r.allGone() && !r.anyInFlight(pending) {
			return nil, ErrUnavailable
		}
		if len(pending) < before {
			r.progressed(o)
		}
		r.noteLowestWaited()
		r.wait(o)
	}
}

// collect copies the pending blocks that are in memory into p and stops
// waiting for them and for those in the Store. It returns the blocks still
// missing and the blocks to copy from the Store.
func (r *Reader) collect(p []byte, off int64, pending []int64) (missing, inStore []int64) {
	for _, b := range pending {
		switch {
		case r.copyResident(p, off, b):
			r.unwait(b)
		case r.hasStored(b):
			inStore = append(inStore, b)
			r.unwait(b)
		default:
			missing = append(missing, b)
		}
	}
	return missing, inStore
}

// abandon stops waiting for the blocks a read ends without. Their fetches
// are cancelled unless another read or the read-ahead window needs them, so
// that an interrupted read does not leave its requests running.
func (r *Reader) abandon(blocks []int64) {
	first, last := r.window()
	for _, i := range blocks {
		r.unwait(i)
		if b := r.blocks[i]; b != nil {
			r.cancelUnneeded(b, first, last)
		}
	}
	r.noteLowestWaited()
}

// covered reports whether every block of [off, end) is in memory or stored.
func (r *Reader) covered(off, end int64) bool {
	for _, b := range pieces(off, end, r.opts.BlockSize) {
		if !r.inMemory(b) && !r.hasStored(b) {
			return false
		}
	}
	return true
}

func (r *Reader) inMemory(b int64) bool {
	e := r.blocks[b]
	return e != nil && e.data != nil
}

func (r *Reader) hasStored(b int64) bool {
	return r.store != nil && r.store.Has(b)
}

func (r *Reader) anyInFlight(blocks []int64) bool {
	return slices.ContainsFunc(blocks, func(b int64) bool { return r.blocks[b].inFlight() })
}

// block returns the state of block b, creating it.
func (r *Reader) block(b int64) *block {
	e := r.blocks[b]
	if e == nil {
		e = &block{index: b}
		r.blocks[b] = e
	}
	return e
}

func (r *Reader) unwait(b int64) {
	e := r.blocks[b]
	e.waiters--
	r.tidy(e)
}

// tidy forgets b once nothing refers to it.
func (r *Reader) tidy(b *block) {
	if b.idle() {
		r.stop(b.hedgeTimer)
		b.hedgeTimer = nil
		delete(r.blocks, b.index)
	}
}

// copyResident copies the part of p in block b if b is in memory.
func (r *Reader) copyResident(p []byte, off, b int64) bool {
	e := r.blocks[b]
	if e.data == nil {
		return false
	}
	copyOverlap(p, off, e.data, b*r.opts.BlockSize)
	r.touch(e)
	return true
}

func (r *Reader) touch(b *block) {
	r.tick++
	b.used = r.tick
}

// copyOverlap copies the bytes that data, which starts at file offset at,
// shares with p, which starts at off.
func copyOverlap(p []byte, off int64, data []byte, at int64) {
	lo := max(off, at)
	hi := min(off+int64(len(p)), at+int64(len(data)))
	copy(p[lo-off:hi-off], data[lo-at:hi-at])
}

// window returns the read-ahead window, which exists only in block mode.
func (r *Reader) window() (first, last int64) {
	if r.streak.length() < r.opts.SwitchAfter {
		return 0, -1
	}
	return windowAt(r.streak.pos, r.opts.ReadAhead, r.opts.BlockSize, r.opts.Size)
}

// wanted lists, in ascending order, the blocks to fetch: those of the window
// and those reads wait for, unless they are stored, in memory or in flight.
func (r *Reader) wanted() []int64 {
	var blocks []int64
	first, last := r.window()
	for b := first; b <= last; b++ {
		blocks = append(blocks, b)
	}
	for b, e := range r.blocks {
		if e.waiters > 0 {
			blocks = append(blocks, b)
		}
	}
	slices.Sort(blocks)
	return slices.DeleteFunc(slices.Compact(blocks), func(b int64) bool {
		e := r.blocks[b]
		return e != nil && (e.inFlight() || e.data != nil) || r.hasStored(b)
	})
}

// schedule starts fetches for wanted blocks on free slots. Called with r.mu
// held after anything that frees a slot or changes what is wanted.
func (r *Reader) schedule() {
	if r.closed {
		return
	}
	wanted := r.wanted()
	if len(wanted) == 0 {
		return
	}
	for _, a := range assign(wanted, r.views((*source).ready), r.opts.Streams) {
		s, b := r.sources[a.source], r.block(a.block)
		r.launch(&fetch{src: s, block: b})
		r.armHedge(b, s)
	}
}

// launch starts f in its own goroutine. Called with r.mu held.
func (r *Reader) launch(f *fetch) {
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.buf = r.bufs.Get().(*[]byte)
	_, length := span(f.block.index, r.opts.BlockSize, r.opts.Size)
	f.data = (*f.buf)[:length]
	f.started = r.clock.Now()
	f.block.fetches = append(f.block.fetches, f)
	*f.slot()++
	r.trace.fetchStarted(f.src.id, f.block.index, f.hedge)
	r.wg.Add(1)
	go r.run(ctx, f)
}

// run performs f and publishes its block if it won. The Store write happens
// without the lock, between claiming the block and publishing it.
func (r *Reader) run(ctx context.Context, f *fetch) {
	defer r.wg.Done()
	defer f.cancel()
	err := f.src.src.Fetch(ctx, f.block.index*r.opts.BlockSize, f.data)
	elapsed := r.clock.Now().Sub(f.started)
	r.mu.Lock()
	won := r.settle(f, err, elapsed)
	r.mu.Unlock()
	if !won {
		r.bufs.Put(f.buf)
		return
	}
	// A failed write only means the block is not cached; the data still
	// reaches the reader from memory.
	stored := r.store != nil && r.store.WriteBlock(f.block.index, f.data) == nil
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publish(f, stored)
}

// settle records the end of f and reports whether f delivered its block
// first. Called with r.mu held.
func (r *Reader) settle(f *fetch, err error, elapsed time.Duration) bool {
	*f.slot()--
	// Deferred in this order so that tests told of the settlement also see
	// the fetches it led to.
	defer r.trace.fetchSettled(f.src.id, f.block.index)
	defer r.schedule()
	if f.cancelled {
		return false
	}
	b := f.block
	b.fetches = slices.DeleteFunc(b.fetches, func(g *fetch) bool { return g == f })
	if err != nil {
		r.sourceFailed(f.src, err)
		r.tidy(b)
		r.broadcast()
		return false
	}
	r.meter.Observe(f.src.id, len(f.data), elapsed)
	f.src.failures = 0
	r.cancelFetches(b)
	b.arriving = true
	return true
}

// publish makes the block of f available: from the Store when stored is
// true, from memory otherwise. Called with r.mu held.
func (r *Reader) publish(f *fetch, stored bool) {
	b := f.block
	b.arriving = false
	if stored {
		r.bufs.Put(f.buf)
	} else {
		b.buf, b.data = f.buf, f.data
		r.resident++
		r.touch(b)
		r.evict()
	}
	r.tidy(b)
	r.broadcast()
}

// evict drops blocks from memory until at most r.memCap remain, or until
// only blocks that reads wait for are left.
func (r *Reader) evict() {
	for r.resident > r.memCap {
		v := r.victim()
		if v == nil {
			return
		}
		r.bufs.Put(v.buf)
		v.buf, v.data = nil, nil
		r.resident--
		r.tidy(v)
	}
}

// victim picks the block to drop from memory: never one a read waits for,
// blocks outside the read-ahead window before those in it, and among those
// the least recently used. Plain LRU would drop unread blocks ahead of the
// reader, which arrived before the blocks it has just read.
func (r *Reader) victim() *block {
	first, last := r.window()
	inWindow := func(b *block) bool { return b.index >= first && b.index <= last }
	before := func(b, v *block) bool {
		if inWindow(b) != inWindow(v) {
			return !inWindow(b)
		}
		return b.used < v.used
	}
	var v *block
	for _, b := range r.blocks {
		if b.data != nil && b.waiters == 0 && (v == nil || before(b, v)) {
			v = b
		}
	}
	return v
}

// cancelFetches cancels every fetch of b. Their goroutines release their
// slots when they return. Called with r.mu held.
func (r *Reader) cancelFetches(b *block) {
	for _, f := range b.fetches {
		f.cancelled = true
		f.cancel()
	}
	b.fetches = nil
	r.stop(b.hedgeTimer)
	b.hedgeTimer = nil
}

// cancelOutside cancels the fetches of blocks outside [first, last] that no
// read waits for.
func (r *Reader) cancelOutside(first, last int64) {
	for _, b := range r.blocks {
		r.cancelUnneeded(b, first, last)
	}
}

// cancelUnneeded cancels the fetches of b if it lies outside [first, last]
// and no read waits for it.
func (r *Reader) cancelUnneeded(b *block, first, last int64) {
	if (b.index < first || b.index > last) && b.waiters == 0 && len(b.fetches) > 0 {
		r.cancelFetches(b)
		r.tidy(b)
	}
}

// noteLowestWaited gives the lowest block a read waits for a fresh hedge
// timer when that block changes: it is the one whose lateness costs the most.
func (r *Reader) noteLowestWaited() {
	lowest := int64(-1)
	for i, b := range r.blocks {
		if b.waiters > 0 && (lowest < 0 || i < lowest) {
			lowest = i
		}
	}
	if lowest == r.lowestWaited {
		return
	}
	r.lowestWaited = lowest
	if b := r.blocks[lowest]; b != nil && len(b.fetches) == 1 {
		r.armHedge(b, b.fetches[0].src)
	}
}

// armHedge (re)starts the hedge timer of b, whose fetch runs on s.
func (r *Reader) armHedge(b *block, s *source) {
	rate, known := r.meter.Rate(s.id)
	if !known {
		rate = r.fastestKnownRate()
	}
	_, length := span(b.index, r.opts.BlockSize, r.opts.Size)
	r.stop(b.hedgeTimer)
	b.hedgeTimer = r.afterFunc(hedgeDelay(length, rate, r.opts.HedgeFloor, r.opts.BlockTimeout), func() {
		b.hedgeTimer = nil
		r.hedge(b)
	})
}

// hedge requests b a second time if it is still in flight once, a read waits
// for it, and a source not fetching it has its hedge slot free.
func (r *Reader) hedge(b *block) {
	if b.waiters == 0 || len(b.fetches) != 1 {
		return
	}
	order := rank(r.views(func(s *source) bool {
		return s.ready() && s.hedges == 0 && !b.fetchingFrom(s)
	}))
	if len(order) == 0 {
		return
	}
	r.launch(&fetch{src: r.sources[order[0].index], block: b, hedge: true})
}
