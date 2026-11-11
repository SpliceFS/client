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

// readProbe serves p from the probe chunks covering it, fetching missing ones
// from the best ready source and failing over to the next on error. A chunk
// another read is already fetching is waited for rather than fetched twice.
// Called with r.mu held.
func (r *Reader) readProbe(o *op, p []byte, off int64) error {
	pending := pieces(off, off+int64(len(p)), r.opts.ProbeSize)
	for {
		before := len(pending)
		pending = r.copyChunks(p, off, pending)
		if len(pending) == 0 {
			return nil
		}
		if err := r.opErr(o); err != nil {
			return err
		}
		if len(pending) < before {
			r.progressed(o)
		}
		c := slices.IndexFunc(pending, func(c int64) bool { return !r.probing[c] })
		s := r.probeSource()
		switch {
		case c >= 0 && s != nil:
			r.loadChunk(o, pending[c], s)
		case r.allGone():
			return ErrUnavailable
		default:
			r.wait(o)
		}
	}
}

// copyChunks copies the cached chunks of pending into p and returns the
// chunks still missing.
func (r *Reader) copyChunks(p []byte, off int64, pending []int64) []int64 {
	return slices.DeleteFunc(pending, func(c int64) bool {
		data, ok := r.probes.get(c)
		if ok {
			copyOverlap(p, off, data, c*r.opts.ProbeSize)
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

// loadChunk fetches probe chunk c from s into the probe cache. Probe fetches
// are not reported to the Meter on success: a small fetch measures latency
// rather than throughput and would make the node serving probes look slow.
// Called with r.mu held; releases it during the fetch.
func (r *Reader) loadChunk(o *op, c int64, s *source) {
	r.probing[c] = true
	r.armDeadline(o)
	start, length := span(c, r.opts.ProbeSize, r.opts.Size)
	buf := make([]byte, length)
	r.mu.Unlock()
	err := s.src.Fetch(o.ctx, start, buf)
	r.mu.Lock()
	delete(r.probing, c)
	r.broadcast()
	switch {
	case err == nil:
		s.failures = 0
		r.probes.put(c, buf)
	case o.ctx.Err() == nil:
		r.sourceFailed(s, err)
	}
}
