package fetch

import (
	"cmp"
	"slices"
	"time"
)

// This file holds the scheduling policy as pure functions over plain data, so
// that each rule can be tested without goroutines or a Reader.

const (
	firstCooldown = time.Second
	maxCooldown   = 30 * time.Second
)

// streak is the sequential run of reads the Reader is following.
type streak struct {
	started bool
	start   int64 // offset of the read that began the streak
	pos     int64 // highest end offset reached
}

// joins reports whether a read of [off, end) continues the streak. The kernel
// issues read-ahead concurrently, so reads that land within slack of the
// position on either side still count as sequential.
func (s streak) joins(off, end, slack int64) bool {
	return s.started && off <= s.pos+slack && end >= s.pos-slack
}

// length is how far the streak has advanced.
func (s streak) length() int64 { return s.pos - s.start }

// windowAt returns the first and last block of the read-ahead window at read
// position pos, clipped to the file. The window is empty when first > last.
func windowAt(pos, readAhead, blockSize, size int64) (first, last int64) {
	return pos / blockSize, min((pos+readAhead)/blockSize, ceilDiv(size, blockSize)-1)
}

// windowSpan is the most blocks a read-ahead window can cover.
func windowSpan(readAhead, blockSize int64) int {
	return int(ceilDiv(readAhead, blockSize)) + 1
}

// sourceView is what the policy needs to know about one source.
type sourceView struct {
	rate   float64 // bytes per second; meaningful only when known
	known  bool
	free   int  // regular fetch slots not in use
	usable bool // may be given work now
}

// ranked is a usable source in rank order.
type ranked struct {
	index int // position in the views passed to rank
	fast  bool
}

// rank orders the usable sources fastest first; ties keep their order. A
// source without a measurement counts as fast as the fastest measured one, so
// a new node gets work and is measured quickly. A source is fast unless its
// rate is below half of the fastest.
func rank(views []sourceView) []ranked {
	fastest := 0.0
	for _, v := range views {
		if v.usable && v.known {
			fastest = max(fastest, v.rate)
		}
	}
	rate := func(i int) float64 {
		if views[i].known {
			return views[i].rate
		}
		return fastest
	}
	var order []int
	for i, v := range views {
		if v.usable {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(rate(b), rate(a)) })
	out := make([]ranked, len(order))
	for i, idx := range order {
		out[i] = ranked{index: idx, fast: rate(idx) >= fastest/2}
	}
	return out
}

// assignment gives one block to one source, by index into the views.
type assignment struct {
	source int
	block  int64
}

// assign hands wanted blocks, in ascending order, to free fetch slots. A fast
// source takes the lowest wanted block. A slow source never takes one of the
// first F wanted blocks, F being the slots of all fast sources: the block the
// reader needs next is then always with a fast node, while slow nodes still
// add throughput further ahead, where lateness costs nothing.
func assign(wanted []int64, views []sourceView, streams int) []assignment {
	order := rank(views)
	reserved := 0
	for _, s := range order {
		if s.fast {
			reserved += streams
		}
	}
	taken := make([]bool, len(wanted))
	var out []assignment
	for _, s := range order {
		from := 0
		if !s.fast {
			from = reserved
		}
		for n := views[s.index].free; n > 0; n-- {
			i := firstUntaken(taken, from)
			if i < 0 {
				break
			}
			taken[i] = true
			out = append(out, assignment{source: s.index, block: wanted[i]})
		}
	}
	return out
}

func firstUntaken(taken []bool, from int) int {
	for i := from; i < len(taken); i++ {
		if !taken[i] {
			return i
		}
	}
	return -1
}

// hedgeDelay is how long a fetch of n bytes from a source of the given rate
// may run before it is hedged: twice the expected time, never below floor. A
// rate that is not positive means nothing is known, which gives floor. The
// delay is capped at ceiling, the read timeout, because a later hedge could
// not help the waiting read; the cap also keeps a near-zero rate from
// overflowing the duration.
func hedgeDelay(n int64, rate float64, floor, ceiling time.Duration) time.Duration {
	if rate <= 0 {
		return floor
	}
	expected := 2 * float64(n) / rate * float64(time.Second)
	return max(floor, time.Duration(min(expected, float64(ceiling))))
}

// cooldown is how long a source rests after its n-th consecutive failure:
// one second, doubling each time, at most thirty.
func cooldown(n int) time.Duration {
	d := firstCooldown
	for i := 1; i < n && d < maxCooldown; i++ {
		d *= 2
	}
	return min(d, maxCooldown)
}

// ceilDiv divides a non-negative a by a positive b, rounding up.
func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}

// pieces lists the indexes of the unit-sized pieces that cover [off, end).
func pieces(off, end, unit int64) []int64 {
	var out []int64
	for i := off / unit; i <= (end-1)/unit; i++ {
		out = append(out, i)
	}
	return out
}

// span returns the offset and length of piece index when a file of size bytes
// is cut into pieces of unit bytes.
func span(index, unit, size int64) (start, length int64) {
	start = index * unit
	return start, min(unit, size-start)
}
