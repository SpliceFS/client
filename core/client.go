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

var (
	// The messages of the next three continue a sentence that names the mount, the
	// cluster or the directory, so they stand for the end of it.
	errNotConfigured     = errors.New("is not configured")
	errCacheNotMounted   = errors.New("is not one of the configured mounts")
	errCacheLimitsDiffer = errors.New("set different max_size, max_age or min_free")
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
	first, err := build(cfg, opts, generation{})
	if err != nil {
		return nil, err
	}
	for _, store := range first.caches {
		store.maybeEvict()
	}
	return &Client{
		opts:      opts,
		clock:     clock,
		clusters:  first.clusters,
		caches:    first.caches,
		newReader: fetch.New,
	}, nil
}

// generation is the clusters and caches that one configuration asks for.
type generation struct {
	clusters map[string]*cluster
	caches   map[string]*cacheDir
	// retiredCaches is only set on the generation that build starts from. They are caches
	// that an earlier reload dropped and that a view still holds, so they are open and
	// their folders are locked. A mount that comes back on such a folder takes the open
	// cache again; opening the folder a second time would fail on its own lock.
	retiredCaches []*cacheDir
}

// reusableCache returns the cache of dir that this generation has open, or nil.
func (g generation) reusableCache(dir string) *cacheDir {
	if kept, ok := g.caches[dir]; ok {
		return kept
	}
	for _, held := range g.retiredCaches {
		if held.dir == dir {
			return held
		}
	}
	return nil
}

// build makes the clusters and caches that cfg asks for. A cluster or cache of old that
// still matches is reused, so New and Apply share this path. It changes nothing in old,
// and when it fails it closes whatever it opened itself.
func build(cfg *config.Config, opts Options, old generation) (generation, error) {
	clusters, err := newClusters(cfg, opts, old.clusters)
	if err != nil {
		return generation{}, err
	}
	caches, err := openCaches(cfg.Mounts, opts, old)
	if err != nil {
		for name, clus := range clusters {
			if old.clusters[name] != clus {
				clus.close()
			}
		}
		return generation{}, err
	}
	return generation{clusters: clusters, caches: caches}, nil
}

// View returns the filesystem of one configured mount: the mount's directory of its
// cluster, read with the mount's cache settings. All mounts of a cluster share one
// merged tree, so its listings are reused for the shortest listing_ttl among them. The
// view keeps its cluster and cache open until Release, even if Apply replaces them.
//
//nolint:ireturn // FS is the interface that mount layers and their tests program against.
func (c *Client) View(mount config.Mount) (FS, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clus, ok := c.clusters[mount.Cluster]
	if !ok {
		return nil, fmt.Errorf("mount %s: cluster %q %w", mount.Target, mount.Cluster, errNotConfigured)
	}
	store, ok := c.caches[mount.Cache.Dir]
	if !ok {
		return nil, fmt.Errorf("mount %s: cache directory %s %w", mount.Target, mount.Cache.Dir, errCacheNotMounted)
	}
	clus.holds++
	store.holds++
	return &view{
		client: c,
		c:      clus,
		prefix: strings.TrimPrefix(path.Clean("/"+mount.Path), "/"),
		mount:  mount,
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

	for _, clus := range clusters {
		clus.close()
	}
	if c.opts.HTTPClient != nil {
		c.opts.HTTPClient.CloseIdleConnections()
	}
	var errs []error
	for _, store := range caches {
		if err := store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close cache %s: %w", store.dir, err))
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

func readPasswordFile(passwordFile string) (string, error) {
	data, err := os.ReadFile(passwordFile)
	if err != nil {
		return "", err //nolint:wrapcheck // The error of the operating system already names the file.
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

// newClusters makes the clusters of cfg. A cluster of old whose settings, password and
// listing TTL are all unchanged is reused as it is, because its session and measured
// node speeds are worth keeping. Every password file is read before anything is built,
// so a failure leaves nothing to clean up.
func newClusters(
	cfg *config.Config, opts Options, old map[string]*cluster,
) (map[string]*cluster, error) {
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
	for i := range cfg.Mounts {
		mount := &cfg.Mounts[i]
		if mount.Cluster != clusterName {
			continue
		}
		if !found || mount.Cache.ListingTTL < ttl {
			ttl, found = mount.Cache.ListingTTL, true
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
	first := make(map[string]*config.Mount)
	for i := range mounts {
		mount := &mounts[i]
		earlier, ok := first[mount.Cache.Dir]
		if !ok {
			first[mount.Cache.Dir] = mount
			continue
		}
		if limitsOf(earlier.Cache) != limitsOf(mount.Cache) {
			return fmt.Errorf("mounts %s and %s share the cache directory %s but %w",
				earlier.Target, mount.Target, mount.Cache.Dir, errCacheLimitsDiffer)
		}
	}
	return nil
}

// openCaches opens one cache per distinct cache directory of the mounts. A directory
// that old has open, retired or not, is reused and never opened a second time, because a
// process may hold a cache folder only once. Its limits are applied by Apply once the swap
// is certain, not here.
func openCaches(mounts []config.Mount, opts Options,
	old generation,
) (map[string]*cacheDir, error) {
	if err := checkSharedCaches(mounts); err != nil {
		return nil, err
	}
	caches := make(map[string]*cacheDir)
	var opened []*cacheDir
	for i := range mounts {
		mount := &mounts[i]
		dir := mount.Cache.Dir
		if _, ok := caches[dir]; ok {
			continue
		}
		if kept := old.reusableCache(dir); kept != nil {
			caches[dir] = kept
			continue
		}
		diskCache, err := cache.Open(cache.Options{
			Dir:     dir,
			MaxSize: int64(mount.Cache.MaxSize),
			MaxAge:  mount.Cache.MaxAge.Std(),
			MinFree: int64(mount.Cache.MinFree),
			Now:     opts.Now,
			Logger:  opts.Logger,
		})
		if err != nil {
			err = fmt.Errorf("mount %s: open cache %s: %w", mount.Target, dir, err)
			for _, store := range opened {
				err = errors.Join(err, store.Close())
			}
			return nil, err
		}
		store := &cacheDir{Cache: diskCache, dir: dir, now: opts.Now, log: opts.Logger}
		caches[dir] = store
		opened = append(opened, store)
	}
	return caches, nil
}
