package core

import (
	"log/slog"
	"sync"
	"time"

	"github.com/SpliceFS/client/internal/cache"
)

// evictInterval is the shortest time between two eviction passes of one cache. A pass
// walks every entry, so it runs now and then rather than on every open; the size and
// free space limits are also applied by the cache itself whenever it stores a block.
const evictInterval = 10 * time.Minute

// cacheDir is one opened cache directory and the schedule of its eviction passes.
type cacheDir struct {
	*cache.Cache
	dir string
	now func() time.Time
	log *slog.Logger

	mu  sync.Mutex
	due time.Time // the earliest time of the next pass
}

// maybeEvict runs an eviction pass if one is due. The caller that finds it due runs
// it; callers arriving meanwhile go on at once instead of waiting for it. A failed
// pass is logged: eviction keeps the disk in bounds, and reading must not fail for it.
func (d *cacheDir) maybeEvict() {
	now := d.now()
	d.mu.Lock()
	if now.Before(d.due) {
		d.mu.Unlock()
		return
	}
	d.due = now.Add(evictInterval)
	d.mu.Unlock()
	if err := d.Evict(); err != nil {
		d.log.Warn("cache eviction failed", "dir", d.dir, "error", err)
	}
}
