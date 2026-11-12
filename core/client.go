package core

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/fetch"
	"github.com/SpliceFS/common/units"
)

// Options are the parts of a Client that do not come from the configuration. Every
// zero value selects the default named on its field.
type Options struct {
	// Logger receives operational messages. Nil discards them.
	Logger *slog.Logger
	// HTTPClient carries every request to every node. Nil builds one client, shared by
	// all clusters, with a transport tuned for block reads.
	HTTPClient *http.Client
	// Now is the clock of logins, listings, the cache, eviction and free space. Nil is
	// time.Now.
	Now func() time.Time
	// ReadPassword returns the password stored at path. Nil reads the file and strips
	// one trailing newline.
	ReadPassword func(path string) (string, error)
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ReadPassword == nil {
		o.ReadPassword = readPasswordFile
	}
	return o
}

// readerFunc builds the reader of an open file; it is fetch.New outside of tests.
type readerFunc func(fetch.Options, []fetch.Source, fetch.Store, fetch.Meter) (*fetch.Reader, error)

// Client is the SpliceFS client for one configuration: a connection to every
// configured cluster and the caches the mounts use. It hands out one FS per mount and
// can be given a new configuration while mounts use it (Apply). It is safe for
// concurrent use.
type Client struct {
	opts      Options
	clock     fetch.Clock // the time source of block reads
	newReader readerFunc

	// applyMu makes Apply and Close run one at a time. Apply works without mu while it
	// reads password files and opens caches, and must not be overtaken meanwhile.
	applyMu sync.Mutex

	// mu guards everything below and the hold counts and retired flags of the clusters
	// and caches. The two maps are replaced as a whole and never edited, so a map that
	// was read under mu can be walked after it is released.
	mu              sync.Mutex
	clusters        map[string]*cluster
	caches          map[string]*cacheDir
	retiredClusters []*cluster // replaced or removed, but a view still uses them
	retiredCaches   []*cacheDir
	closed          bool
}

// New prepares a Client for cfg. It reads each cluster's password file once and opens
// one cache per distinct cache directory of the mounts, applying the cache limits to
// what an earlier run left there, but does not use the network: clusters are contacted
// on first use, or by Connect. Two mounts that share a cache directory must have the
// same cache limits.
func New(cfg *config.Config, opts Options) (*Client, error) {
	return newClient(cfg, opts, fetch.SystemClock{})
}

// newClient is New with the time source of block reads, which tests replace.
func newClient(cfg *config.Config, opts Options, clock fetch.Clock) (*Client, error) {
	opts = opts.withDefaults()
	clusters, caches, err := build(cfg, opts, nil, nil)
	if err != nil {
		return nil, err
	}
	for _, d := range caches {
		d.maybeEvict()
	}
	return &Client{opts: opts, clock: clock, clusters: clusters, caches: caches, newReader: fetch.New}, nil
}

// build makes the clusters and caches that cfg asks for. A cluster or cache of old that
// still matches is reused, so New and Apply share this path. It changes nothing in old,
// and when it fails it closes whatever it opened itself.
func build(cfg *config.Config, opts Options, oldClusters map[string]*cluster,
	oldCaches map[string]*cacheDir) (map[string]*cluster, map[string]*cacheDir, error) {
	clusters, err := newClusters(cfg, opts, oldClusters)
	if err != nil {
		return nil, nil, err
	}
	caches, err := openCaches(cfg.Mounts, opts, oldCaches)
	if err != nil {
		for name, cl := range clusters {
			if oldClusters[name] != cl {
				cl.close()
			}
		}
		return nil, nil, err
	}
	return clusters, caches, nil
}

// View returns the filesystem of one configured mount: the mount's directory of its
// cluster, read with the mount's cache settings. All mounts of a cluster share one
// merged tree, so its listings are reused for the shortest listing_ttl among them. The
// view keeps its cluster and cache open until Release, even if Apply replaces them.
func (c *Client) View(m config.Mount) (FS, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl, ok := c.clusters[m.Cluster]
	if !ok {
		return nil, fmt.Errorf("mount %s: cluster %q is not configured", m.Target, m.Cluster)
	}
	store, ok := c.caches[m.Cache.Dir]
	if !ok {
		return nil, fmt.Errorf("mount %s: cache directory %s is not one of the configured mounts", m.Target, m.Cache.Dir)
	}
	cl.holds++
	store.holds++
	return &view{
		client: c,
		c:      cl,
		prefix: strings.TrimPrefix(path.Clean("/"+m.Path), "/"),
		mount:  m,
		cache:  store,
	}, nil
}

// Close ends every cluster session, closes every cache, replaced ones included, and
// drops the idle connections of the HTTP clients. Files that are still open stop working.
func (c *Client) Close() error {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.mu.Lock()
	c.closed = true
	clusters := slices.Concat(sortedValues(c.clusters), c.retiredClusters)
	caches := slices.Concat(sortedValues(c.caches), c.retiredCaches)
	c.retiredClusters, c.retiredCaches = nil, nil
	c.mu.Unlock()

	for _, cl := range clusters {
		cl.close()
	}
	if c.opts.HTTPClient != nil {
		c.opts.HTTPClient.CloseIdleConnections()
	}
	var errs []error
	for _, d := range caches {
		if err := d.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close cache %s: %w", d.dir, err))
		}
	}
	return errors.Join(errs...)
}

func sortedValues[V any](m map[string]V) []V {
	out := make([]V, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return out
}

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

// newClusters makes the clusters of cfg. A cluster of old whose settings, password and
// listing TTL are all unchanged is reused as it is, because its session and measured
// node speeds are worth keeping. Every password file is read before anything is built,
// so a failure leaves nothing to clean up.
func newClusters(cfg *config.Config, opts Options,
	old map[string]*cluster) (map[string]*cluster, error) {
	names := slices.Sorted(maps.Keys(cfg.Clusters))
	passwords := make(map[string]string, len(names))
	for _, name := range names {
		password, err := opts.ReadPassword(cfg.Clusters[name].PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: read password file: %w", name, err)
		}
		passwords[name] = password
	}
	clusters := make(map[string]*cluster, len(names))
	for _, name := range names {
		cc, ttl := cfg.Clusters[name], listingTTL(cfg, name)
		if kept, ok := old[name]; ok && kept.matches(cc, passwords[name], ttl) {
			clusters[name] = kept
			continue
		}
		clusters[name] = newCluster(name, cc, passwords[name], ttl, opts)
	}
	return clusters, nil
}

// listingTTL is the shortest listing TTL of the mounts of a cluster, because they all
// share the cluster's one tree and none may see listings older than it asked for.
func listingTTL(cfg *config.Config, clusterName string) time.Duration {
	ttl, found := cfg.Cache.ListingTTL, false
	for _, m := range cfg.Mounts {
		if m.Cluster != clusterName {
			continue
		}
		if !found || m.Cache.ListingTTL < ttl {
			ttl, found = m.Cache.ListingTTL, true
		}
	}
	return ttl.Std()
}

// limits are the cache settings that belong to a cache directory rather than to a mount.
type limits struct {
	maxSize units.Size
	maxAge  units.Duration
	minFree units.Size
}

func limitsOf(c config.Cache) limits {
	return limits{maxSize: c.MaxSize, maxAge: c.MaxAge, minFree: c.MinFree}
}

// checkSharedCaches reports two mounts that use one cache directory with different
// limits: the directory has one eviction policy, so one of them would be ignored.
func checkSharedCaches(mounts []config.Mount) error {
	first := make(map[string]config.Mount)
	for _, m := range mounts {
		earlier, ok := first[m.Cache.Dir]
		if !ok {
			first[m.Cache.Dir] = m
			continue
		}
		if limitsOf(earlier.Cache) != limitsOf(m.Cache) {
			return fmt.Errorf("mounts %s and %s share the cache directory %s but set different max_size, max_age or min_free",
				earlier.Target, m.Target, m.Cache.Dir)
		}
	}
	return nil
}

// openCaches opens one cache per distinct cache directory of the mounts. A directory
// that old has open is reused, and its limits are applied by Apply once the swap is
// certain, not here.
func openCaches(mounts []config.Mount, opts Options,
	old map[string]*cacheDir) (map[string]*cacheDir, error) {
	if err := checkSharedCaches(mounts); err != nil {
		return nil, err
	}
	caches := make(map[string]*cacheDir)
	var opened []*cacheDir
	for _, m := range mounts {
		dir := m.Cache.Dir
		if _, ok := caches[dir]; ok {
			continue
		}
		if kept, ok := old[dir]; ok {
			caches[dir] = kept
			continue
		}
		c, err := cache.Open(cache.Options{
			Dir:     dir,
			MaxSize: int64(m.Cache.MaxSize),
			MaxAge:  m.Cache.MaxAge.Std(),
			MinFree: int64(m.Cache.MinFree),
			Now:     opts.Now,
			Logger:  opts.Logger,
		})
		if err != nil {
			err = fmt.Errorf("mount %s: open cache %s: %w", m.Target, dir, err)
			for _, d := range opened {
				err = errors.Join(err, d.Close())
			}
			return nil, err
		}
		d := &cacheDir{Cache: c, dir: dir, now: opts.Now, log: opts.Logger}
		caches[dir] = d
		opened = append(opened, d)
	}
	return caches, nil
}
