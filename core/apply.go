package core

import (
	"errors"
	"log/slog"
	"slices"

	"github.com/SpliceFS/client/config"
)

var errClosed = errors.New("client is closed")

// retired is what a reload replaced or removed and nothing holds any more, so it can be
// closed.
type retired struct {
	clusters []*cluster
	caches   []*cacheDir
}

// Apply makes the clusters and caches match cfg while mounts keep using the client. A
// cluster whose settings, password and listing TTL are unchanged is kept, with its
// session; any other is built new. A cache directory that is already open is kept and
// gets the new limits, which the next eviction pass uses, and so does one that an earlier
// reload dropped but a view still holds; a new one is opened. Whatever
// can fail happens before anything changes: on an error the client is as it was and what
// this call opened is closed. Clusters and caches that are replaced or removed go on
// serving the views that hold them and are closed when the last of those is released,
// at once if there is none.
func (c *Client) Apply(cfg *config.Config) error {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errClosed
	}
	old := generation{clusters: c.clusters, caches: c.caches, retiredCaches: slices.Clone(c.retiredCaches)}
	c.mu.Unlock()

	next, err := build(cfg, c.opts, old)
	if err != nil {
		return err
	}

	c.mu.Lock()
	done := c.install(cfg, old, next)
	c.mu.Unlock()

	c.closeRetired(done)
	for dir, store := range next.caches {
		if old.caches[dir] != store {
			store.maybeEvict()
		}
	}
	return nil
}

// install makes next the current generation and returns what the swap left unused. The
// caller holds c.mu.
func (c *Client) install(cfg *config.Config, old, next generation) retired {
	c.clusters, c.caches = next.clusters, next.caches
	for i := range cfg.Mounts {
		cacheCfg := &cfg.Mounts[i].Cache
		next.caches[cacheCfg.Dir].SetLimits(int64(cacheCfg.MaxSize), cacheCfg.MaxAge.Std(), int64(cacheCfg.MinFree))
	}
	c.reviveCaches(next.caches)
	return retired{
		clusters: c.retireClusters(old.clusters, next.clusters),
		caches:   c.retireCaches(old.caches, next.caches),
	}
}

// reviveCaches takes the caches that next has taken back off the list of retired ones. A
// cache on both lists would be closed twice, and a retired one is closed when its last
// view is released. The caller holds c.mu.
func (c *Client) reviveCaches(next map[string]*cacheDir) {
	for _, store := range next {
		if store.retired {
			store.retired = false
			c.retiredCaches = slices.DeleteFunc(c.retiredCaches,
				func(held *cacheDir) bool { return held == store })
		}
	}
}

// retireClusters marks the clusters that next dropped as retired. Those that no view holds
// are returned to be closed; the others wait in the client until the last view is
// released. The caller holds c.mu.
func (c *Client) retireClusters(old, next map[string]*cluster) []*cluster {
	var unused []*cluster
	for name, clus := range old {
		if next[name] == clus {
			continue
		}
		clus.retired = true
		if clus.holds == 0 {
			unused = append(unused, clus)
		} else {
			c.retiredClusters = append(c.retiredClusters, clus)
		}
	}
	return unused
}

// retireCaches does for cache directories what retireClusters does for clusters.
func (c *Client) retireCaches(old, next map[string]*cacheDir) []*cacheDir {
	var unused []*cacheDir
	for dir, store := range old {
		if next[dir] == store {
			continue
		}
		store.retired = true
		if store.holds == 0 {
			unused = append(unused, store)
		} else {
			c.retiredCaches = append(c.retiredCaches, store)
		}
	}
	return unused
}

// Release says that the mount that used fs has ended. It drops the view's hold on its
// cluster and cache, and closes the ones that a reload replaced and that nothing holds
// any more. A value that is not a view of this client, or a view that was released
// already, is ignored.
func (c *Client) Release(fs FS) {
	mountView, ok := fs.(*view)
	if !ok || mountView.client != c {
		return
	}
	// A cache that leaves the list of retired ones here is closed a moment later. Apply must
	// not look at that list in between: it would not find the cache, open its folder anew
	// and fail on the lock that the cache still holds.
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	var done retired
	c.mu.Lock()
	if mountView.released {
		c.mu.Unlock()
		return
	}
	mountView.released = true
	mountView.c.holds--
	mountView.cache.holds--
	if mountView.c.retired && mountView.c.holds == 0 && !c.closed {
		c.retiredClusters = slices.DeleteFunc(c.retiredClusters,
			func(clus *cluster) bool { return clus == mountView.c })
		done.clusters = append(done.clusters, mountView.c)
	}
	if mountView.cache.retired && mountView.cache.holds == 0 && !c.closed {
		c.retiredCaches = slices.DeleteFunc(c.retiredCaches,
			func(store *cacheDir) bool { return store == mountView.cache })
		done.caches = append(done.caches, mountView.cache)
	}
	c.mu.Unlock()
	c.closeRetired(done)
}

// Current reports whether the cluster and the cache of fs are still the ones the client
// would hand out for the same mount now, which is false after Apply replaced or removed
// either of them, and for a value that is not a view of this client.
func (c *Client) Current(fs FS) bool {
	mountView, ok := fs.(*view)
	if !ok || mountView.client != c {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clusters[mountView.mount.Cluster] == mountView.c &&
		c.caches[mountView.mount.Cache.Dir] == mountView.cache
}

// closeRetired closes what a reload retired. It runs without the client's lock, because
// closing a cache writes its metadata. A cache that fails to close is logged: the reload
// or the unmount that caused it has happened already and cannot be undone.
func (c *Client) closeRetired(done retired) {
	for _, clus := range done.clusters {
		clus.close()
	}
	for _, store := range done.caches {
		if err := store.Close(); err != nil {
			c.opts.Logger.Warn("close replaced cache",
				slog.String("dir", store.dir), slog.Any("error", err))
		}
	}
}
