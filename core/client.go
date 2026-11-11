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
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/fetch"
	"github.com/SpliceFS/client/internal/units"
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

func (o Options) withDefaults(cfg *config.Config) Options {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = newHTTPClient(largestStreams(cfg), systemDialer())
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
// configured cluster and the caches the mounts use. It hands out one FS per mount.
// It is safe for concurrent use.
type Client struct {
	opts      Options
	clock     fetch.Clock // the time source of block reads
	clusters  map[string]*cluster
	caches    map[string]*cacheDir
	newReader readerFunc
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
	opts = opts.withDefaults(cfg)
	clusters, err := newClusters(cfg, opts)
	if err != nil {
		return nil, err
	}
	caches, err := openCaches(cfg.Mounts, opts)
	if err != nil {
		return nil, err
	}
	for _, d := range caches {
		d.maybeEvict()
	}
	return &Client{opts: opts, clock: clock, clusters: clusters, caches: caches, newReader: fetch.New}, nil
}

// View returns the filesystem of one configured mount: the mount's directory of its
// cluster, read with the mount's cache settings. All mounts of a cluster share one
// merged tree, so its listings are reused for the shortest listing_ttl among them.
func (c *Client) View(m config.Mount) (FS, error) {
	cl, ok := c.clusters[m.Cluster]
	if !ok {
		return nil, fmt.Errorf("mount %s: cluster %q is not configured", m.Target, m.Cluster)
	}
	store, ok := c.caches[m.Cache.Dir]
	if !ok {
		return nil, fmt.Errorf("mount %s: cache directory %s is not one of the configured mounts", m.Target, m.Cache.Dir)
	}
	return &view{
		client: c,
		c:      cl,
		prefix: strings.TrimPrefix(path.Clean("/"+m.Path), "/"),
		mount:  m,
		cache:  store,
	}, nil
}

// Close ends every cluster session, closes every cache and drops the idle connections
// of the HTTP client. Files that are still open stop working.
func (c *Client) Close() error {
	for _, cl := range c.clusters {
		cl.session.Close()
	}
	c.opts.HTTPClient.CloseIdleConnections()
	var errs []error
	for _, dir := range slices.Sorted(maps.Keys(c.caches)) {
		if err := c.caches[dir].Close(); err != nil {
			errs = append(errs, fmt.Errorf("close cache %s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(data), "\n"), nil
}

func largestStreams(cfg *config.Config) int {
	largest := 0
	for _, cl := range cfg.Clusters {
		largest = max(largest, cl.StreamsPerNode)
	}
	return largest
}

func newClusters(cfg *config.Config, opts Options) (map[string]*cluster, error) {
	clusters := make(map[string]*cluster, len(cfg.Clusters))
	for _, name := range slices.Sorted(maps.Keys(cfg.Clusters)) {
		cc := cfg.Clusters[name]
		password, err := opts.ReadPassword(cc.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: read password file: %w", name, err)
		}
		clusters[name] = newCluster(name, cc, password, listingTTL(cfg, name), opts)
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

func openCaches(mounts []config.Mount, opts Options) (map[string]*cacheDir, error) {
	if err := checkSharedCaches(mounts); err != nil {
		return nil, err
	}
	caches := make(map[string]*cacheDir)
	for _, m := range mounts {
		if _, ok := caches[m.Cache.Dir]; ok {
			continue
		}
		c, err := cache.Open(cache.Options{
			Dir:     m.Cache.Dir,
			MaxSize: int64(m.Cache.MaxSize),
			MaxAge:  m.Cache.MaxAge.Std(),
			MinFree: int64(m.Cache.MinFree),
			Now:     opts.Now,
			Logger:  opts.Logger,
		})
		if err != nil {
			err = fmt.Errorf("mount %s: open cache %s: %w", m.Target, m.Cache.Dir, err)
			for _, open := range caches {
				err = errors.Join(err, open.Close())
			}
			return nil, err
		}
		caches[m.Cache.Dir] = &cacheDir{Cache: c, dir: m.Cache.Dir, now: opts.Now, log: opts.Logger}
	}
	return caches, nil
}
