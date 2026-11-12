package core

import (
	"errors"
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
// gets the new limits, which the next eviction pass uses; a new one is opened. Whatever
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
	oldClusters, oldCaches := c.clusters, c.caches
	c.mu.Unlock()

	clusters, caches, err := build(cfg, c.opts, oldClusters, oldCaches)
	if err != nil {
		return err
	}

	var done retired
	c.mu.Lock()
	c.clusters, c.caches = clusters, caches
	for _, m := range cfg.Mounts {
		if d, ok := oldCaches[m.Cache.Dir]; ok {
			d.SetLimits(int64(m.Cache.MaxSize), m.Cache.MaxAge.Std(), int64(m.Cache.MinFree))
		}
	}
	for name, cl := range oldClusters {
		if clusters[name] == cl {
			continue
		}
		cl.retired = true
		if cl.holds == 0 {
			done.clusters = append(done.clusters, cl)
		} else {
			c.retiredClusters = append(c.retiredClusters, cl)
		}
	}
	for dir, d := range oldCaches {
		if caches[dir] == d {
			continue
		}
		d.retired = true
		if d.holds == 0 {
			done.caches = append(done.caches, d)
		} else {
			c.retiredCaches = append(c.retiredCaches, d)
		}
	}
	c.mu.Unlock()

	c.closeRetired(done)
	for dir, d := range caches {
		if oldCaches[dir] != d {
			d.maybeEvict()
		}
	}
	return nil
}

// Release says that the mount that used fs has ended. It drops the view's hold on its
// cluster and cache, and closes the ones that a reload replaced and that nothing holds
// any more. A value that is not a view of this client, or a view that was released
// already, is ignored.
func (c *Client) Release(fs FS) {
	v, ok := fs.(*view)
	if !ok || v.client != c {
		return
	}
	var done retired
	c.mu.Lock()
	if v.released {
		c.mu.Unlock()
		return
	}
	v.released = true
	v.c.holds--
	v.cache.holds--
	if v.c.retired && v.c.holds == 0 && !c.closed {
		c.retiredClusters = slices.DeleteFunc(c.retiredClusters,
			func(cl *cluster) bool { return cl == v.c })
		done.clusters = append(done.clusters, v.c)
	}
	if v.cache.retired && v.cache.holds == 0 && !c.closed {
		c.retiredCaches = slices.DeleteFunc(c.retiredCaches,
			func(d *cacheDir) bool { return d == v.cache })
		done.caches = append(done.caches, v.cache)
	}
	c.mu.Unlock()
	c.closeRetired(done)
}

// Current reports whether the cluster and the cache of fs are still the ones the client
// would hand out for the same mount now, which is false after Apply replaced or removed
// either of them, and for a value that is not a view of this client.
func (c *Client) Current(fs FS) bool {
	v, ok := fs.(*view)
	if !ok || v.client != c {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clusters[v.mount.Cluster] == v.c && c.caches[v.mount.Cache.Dir] == v.cache
}

// closeRetired closes what a reload retired. It runs without the client's lock, because
// closing a cache writes its metadata. A cache that fails to close is logged: the reload
// or the unmount that caused it has happened already and cannot be undone.
func (c *Client) closeRetired(r retired) {
	for _, cl := range r.clusters {
		cl.close()
	}
	for _, d := range r.caches {
		if err := d.Close(); err != nil {
			c.opts.Logger.Warn("close replaced cache", "dir", d.dir, "error", err)
		}
	}
}
