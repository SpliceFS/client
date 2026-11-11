package fetch

import "errors"

// source is the Reader's state for one Source.
type source struct {
	src       Source
	id        string
	gone      bool // never used again by this Reader
	cooling   bool // resting after a failure
	failures  int  // consecutive failures, for the cooldown length
	coolTimer *timer
	active    int // regular fetches in flight
	hedges    int // hedge fetches in flight, at most one
}

// ready reports whether s may be given work now.
func (s *source) ready() bool {
	return !s.gone && !s.cooling
}

// sourceFailed records a failed fetch from s. Called with r.mu held.
//
// ErrGone is not reported to the Meter: it says this node lost one version of
// one file, not that the node is slow, and the Meter outlives this file.
// Failures while s is already cooling down do not lengthen the cooldown,
// since the parallel streams of one outage would otherwise count as many
// consecutive failures.
func (r *Reader) sourceFailed(s *source, err error) {
	if errors.Is(err, ErrGone) {
		s.gone = true
		r.stop(s.coolTimer)
		s.coolTimer = nil
		return
	}
	r.meter.Failed(s.id)
	if s.cooling {
		return
	}
	s.failures++
	s.cooling = true
	s.coolTimer = r.afterFunc(cooldown(s.failures), func() {
		s.cooling = false
		s.coolTimer = nil
		r.schedule()
		r.broadcast()
	})
}

// views describes every source to the policy functions; eligible decides
// which may take the work at hand. Called with r.mu held.
func (r *Reader) views(eligible func(*source) bool) []sourceView {
	views := make([]sourceView, len(r.sources))
	for i, s := range r.sources {
		rate, known := r.meter.Rate(s.id)
		views[i] = sourceView{rate: rate, known: known, free: r.opts.Streams - s.active, usable: eligible(s)}
	}
	return views
}

// allGone reports whether no source can ever deliver again.
func (r *Reader) allGone() bool {
	for _, s := range r.sources {
		if !s.gone {
			return false
		}
	}
	return true
}

// fastestKnownRate is the best measured rate of any source still in use, or 0.
func (r *Reader) fastestKnownRate() float64 {
	fastest := 0.0
	for _, s := range r.sources {
		if rate, known := r.meter.Rate(s.id); known && !s.gone {
			fastest = max(fastest, rate)
		}
	}
	return fastest
}
