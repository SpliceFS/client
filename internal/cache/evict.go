package cache

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Evict removes entries that are too old, then the least recently read ones while the
// cache is over MaxSize or the disk has less than MinFree free. Entries that are pinned
// are never removed.
func (c *Cache) Evict() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictLocked(0)
}

// evictLocked evicts until the limits hold with need more bytes about to be written.
func (c *Cache) evictLocked(need int64) error {
	if c.closed {
		return ErrClosed
	}
	now := c.opts.Now()
	var victims []*entry
	for _, e := range c.entries {
		if e.pins == 0 {
			victims = append(victims, e)
		}
	}
	// Sorting by last read puts the entries that are too old first, so the first
	// entry that is neither too old nor needed to meet a limit ends the pass.
	slices.SortFunc(victims, func(a, b *entry) int {
		if d := a.lastRead.Compare(b.lastRead); d != 0 {
			return d
		}
		return strings.Compare(a.base, b.base)
	})
	var errs []error
	for _, e := range victims {
		if now.Sub(e.lastRead) <= c.opts.MaxAge && !c.overLimitLocked(need) {
			break
		}
		if err := c.removeLocked(e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Cache) overLimitLocked(need int64) bool {
	if c.total > c.opts.MaxSize {
		return true
	}
	if c.opts.MinFree <= 0 {
		return false
	}
	room, err := c.roomFor(need, c.opts.MinFree)
	if err != nil {
		c.log.Warn("read free space", "dir", c.opts.Dir, "error", err)
		return false
	}
	return !room
}

// roomFor reports whether need more bytes fit while leaving minFree bytes free. The limit
// is passed in because SetLimits may change it, and only some callers hold the lock.
func (c *Cache) roomFor(need, minFree int64) (bool, error) {
	free, err := c.opts.FreeSpace(c.opts.Dir)
	if err != nil {
		return false, err
	}
	avail := int64(min(free, math.MaxInt64))
	return avail-need >= minFree, nil
}

// reserve makes room for a block of size bytes, evicting once if the disk is short.
func (c *Cache) reserve(size int64) error {
	c.mu.Lock()
	minFree := c.opts.MinFree
	c.mu.Unlock()
	if minFree <= 0 {
		return nil
	}
	room, err := c.roomFor(size, minFree)
	if err == nil && !room {
		c.mu.Lock()
		c.warn("evict for free space", c.evictLocked(size))
		c.mu.Unlock()
		room, err = c.roomFor(size, minFree)
	}
	switch {
	case err != nil:
		return fmt.Errorf("%w: read free space: %w", ErrNoSpace, err)
	case !room:
		return ErrNoSpace
	}
	return nil
}
