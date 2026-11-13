package fetch

import "slices"

// probeChunks is how many probe chunks are kept, so that the small adjacent
// reads of a header or index parser do not fetch the same chunk again.
const probeChunks = 16

// probeCache holds the most recently used probe chunks.
type probeCache struct {
	chunks []probeChunk // least recently used first
}

type probeChunk struct {
	index int64
	data  []byte
}

// get returns chunk index and marks it as used.
func (c *probeCache) get(index int64) ([]byte, bool) {
	i := slices.IndexFunc(c.chunks, func(ch probeChunk) bool { return ch.index == index })
	if i < 0 {
		return nil, false
	}
	ch := c.chunks[i]
	c.chunks = append(slices.Delete(c.chunks, i, i+1), ch)
	return ch.data, true
}

// put adds a chunk that is not yet held, dropping the least recently used one
// when full.
func (c *probeCache) put(index int64, data []byte) {
	if len(c.chunks) == probeChunks {
		c.chunks = slices.Delete(c.chunks, 0, 1)
	}
	c.chunks = append(c.chunks, probeChunk{index: index, data: data})
}

// readProbe serves dest from the probe chunks covering it, fetching missing
// ones from the best ready source and failing over to the next on error. A
// chunk another read is already fetching is waited for rather than fetched
// twice. Called with r.mu held.
func (r *Reader) readProbe(operation *op, dest []byte, off int64) error {
	pending := pieces(off, off+int64(len(dest)), r.opts.ProbeSize)
	for {
		before := len(pending)
		pending = r.copyChunks(dest, off, pending)
		if len(pending) == 0 {
			return nil
		}
		if err := r.opErr(operation); err != nil {
			return err
		}
		if len(pending) < before {
			r.progressed(operation)
		}
		c := slices.IndexFunc(pending, func(c int64) bool { return !r.probing[c] })
		s := r.probeSource()
		switch {
		case c >= 0 && s != nil:
			r.loadChunk(operation, pending[c], s)
		case r.allGone():
			return ErrUnavailable
		default:
			r.wait(operation)
		}
	}
}

// copyChunks copies the cached chunks of pending into dest and returns the
// chunks still missing.
func (r *Reader) copyChunks(dest []byte, off int64, pending []int64) []int64 {
	return slices.DeleteFunc(pending, func(c int64) bool {
		data, ok := r.probes.get(c)
		if ok {
			copyOverlap(dest, off, data, c*r.opts.ProbeSize)
		}
		return ok
	})
}

// probeSource is the best ready source, or nil.
func (r *Reader) probeSource() *source {
	order := rank(r.views((*source).ready))
	if len(order) == 0 {
		return nil
	}
	return r.sources[order[0].index]
}

// loadChunk fetches probe chunk number chunk from origin into the probe
// cache. Probe fetches are not reported to the Meter on success: a small fetch
// measures latency rather than throughput and would make the node serving
// probes look slow. Called with r.mu held; releases it during the fetch.
func (r *Reader) loadChunk(operation *op, chunk int64, origin *source) {
	r.probing[chunk] = true
	r.armDeadline(operation)
	piece := span(chunk, r.opts.ProbeSize, r.opts.Size)
	buf := make([]byte, piece.length)
	r.mu.Unlock()
	err := origin.src.Fetch(operation.ctx, piece.start, buf)
	r.mu.Lock()
	delete(r.probing, chunk)
	r.broadcast()
	switch {
	case err == nil:
		origin.failures = 0
		r.probes.put(chunk, buf)
	case operation.ctx.Err() == nil:
		r.sourceFailed(origin, err)
	default:
		// The read ended while the fetch ran, so the error says nothing about
		// the source.
	}
}
