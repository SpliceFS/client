package core

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/session"
	"github.com/SpliceFS/common/units"
)

// cloneConfig copies cfg deeply enough that a test can change the copy.
func cloneConfig(cfg *config.Config) *config.Config {
	clone := *cfg
	clone.Clusters = make(map[string]config.Cluster, len(cfg.Clusters))
	for name, cl := range cfg.Clusters {
		clone.Clusters[name] = cl
	}
	clone.Mounts = append([]config.Mount(nil), cfg.Mounts...)
	return &clone
}

func clusterClosed(cl *cluster) bool {
	_, _, err := cl.nodes(context.Background())
	return errors.Is(err, session.ErrClosed)
}

var errPasswordRead = errors.New("password file unreadable")

func cacheClosed(d *cacheDir) bool { return errors.Is(d.Evict(), cache.ErrClosed) }

// withMount adds a mount of cluster name at target, with its own cache directory.
func (h *harness) withMount(cluster, target string) config.Mount {
	m := config.Mount{Cluster: cluster, Path: "/", Target: target, Offline: true,
		Cache: testCache(h.t.TempDir())}
	h.cfg.Mounts = append(h.cfg.Mounts, m)
	return m
}

func (h *harness) apply(cfg *config.Config) {
	h.t.Helper()
	if err := h.client.Apply(cfg); err != nil {
		h.t.Fatalf("Apply: %v", err)
	}
}

func TestApplyKeepsWhatIsUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, 10), fileTime)
	fsys := h.view(0)
	if _, err := fsys.ReadDir(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	cl, dir := h.client.clusters[testCluster], h.client.caches[h.cfg.Cache.Dir]

	h.apply(cloneConfig(h.cfg))
	if h.client.clusters[testCluster] != cl || h.client.caches[h.cfg.Cache.Dir] != dir {
		t.Error("Apply replaced a cluster or a cache that had not changed")
	}
	if !h.client.Current(fsys) {
		t.Error("Current = false after an Apply that changed nothing")
	}
	if _, err := fsys.ReadDir(context.Background(), "media"); err != nil {
		t.Fatal(err)
	}
	if n := h.fc.count("login"); n != 1 {
		t.Errorf("%d logins, want the one of the first listing: the session was replaced", n)
	}
}

func TestApplyReplacesChangedCluster(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(h *harness, cfg *config.Config)
	}{
		{"url", func(h *harness, cfg *config.Config) {
			cl := cfg.Clusters[testCluster]
			cl.URL = h.fc.nodes[1].url()
			cfg.Clusters[testCluster] = cl
		}},
		{"user", func(_ *harness, cfg *config.Config) {
			cl := cfg.Clusters[testCluster]
			cl.User = "bob"
			cfg.Clusters[testCluster] = cl
		}},
		{"password", func(h *harness, _ *config.Config) {
			h.client.opts.ReadPassword = func(string) (string, error) { return "other", nil }
		}},
		{"block size", func(_ *harness, cfg *config.Config) {
			cl := cfg.Clusters[testCluster]
			cl.BlockSize *= 2
			cfg.Clusters[testCluster] = cl
		}},
		{"streams per node", func(_ *harness, cfg *config.Config) {
			cl := cfg.Clusters[testCluster]
			cl.StreamsPerNode++
			cfg.Clusters[testCluster] = cl
		}},
		{"read ahead", func(_ *harness, cfg *config.Config) {
			cl := cfg.Clusters[testCluster]
			cl.ReadAhead *= 2
			cfg.Clusters[testCluster] = cl
		}},
		{"listing ttl", func(_ *harness, cfg *config.Config) {
			cfg.Mounts[0].Cache.ListingTTL = units.Duration(time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "a", "b")
			fsys := h.view(0)
			old := h.client.clusters[testCluster]
			cfg := cloneConfig(h.cfg)
			tc.edit(h, cfg)

			h.apply(cfg)
			if h.client.clusters[testCluster] == old {
				t.Fatal("the cluster was kept")
			}
			if h.client.Current(fsys) {
				t.Error("Current = true for a view of the replaced cluster")
			}
			if clusterClosed(old) {
				t.Error("the replaced cluster was closed while a view holds it")
			}
			h.client.Release(fsys)
			if !clusterClosed(old) {
				t.Error("the replaced cluster stayed open after its last view was released")
			}
			if clusterClosed(h.client.clusters[testCluster]) {
				t.Error("the new cluster is closed")
			}
		})
	}
}

func TestApplyRemovesClusterAndCache(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{false, true} {
		h := newHarness(t, "a")
		h.cfg.Clusters["other"] = h.cfg.Clusters[testCluster]
		other := h.withMount("other", "/mnt/other")
		h.start()
		fsys, err := h.client.View(other)
		if err != nil {
			t.Fatal(err)
		}
		cl, dir := h.client.clusters["other"], h.client.caches[other.Cache.Dir]
		if !held {
			h.client.Release(fsys)
		}
		cfg := cloneConfig(h.cfg)
		delete(cfg.Clusters, "other")
		cfg.Mounts = cfg.Mounts[:1]

		h.apply(cfg)
		if h.client.Current(fsys) {
			t.Errorf("held %v: Current = true for a view of a removed mount", held)
		}
		if s := h.client.Status(); len(s.Clusters) != 1 || len(s.Caches) != 1 {
			t.Errorf("held %v: Status = %+v, want the one remaining cluster and cache", held, s)
		}
		if clusterClosed(cl) == held || cacheClosed(dir) == held {
			t.Errorf("held %v: removed cluster closed %v, cache closed %v at Apply",
				held, clusterClosed(cl), cacheClosed(dir))
		}
		h.client.Release(fsys)
		if !clusterClosed(cl) || !cacheClosed(dir) {
			t.Errorf("held %v: removed cluster or cache still open after the release", held)
		}
	}
}

func TestApplyKeepsCacheWithNewLimits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	readFile(t, h.view(0), "media/file")
	dir := h.client.caches[h.cfg.Cache.Dir]
	cfg := cloneConfig(h.cfg)
	cfg.Mounts[0].Cache.MaxSize = 1
	newDir := h.withMount(testCluster, "/mnt/new").Cache.Dir
	cfg.Mounts = append(cfg.Mounts, h.cfg.Mounts[1])

	if err := h.cache(0).Evict(); err != nil || h.client.Status().Caches[0].Entries != 1 {
		t.Fatalf("Evict with the old limits = %v, entries %+v", err, h.client.Status().Caches)
	}
	h.apply(cfg)
	if h.client.caches[h.cfg.Cache.Dir] != dir {
		t.Error("the cache folder was reopened")
	}
	if h.client.caches[newDir] == nil {
		t.Error("the new cache folder was not opened")
	}
	if err := dir.Evict(); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.client.Status().Caches {
		if c.Dir == h.cfg.Cache.Dir && c.Entries != 0 {
			t.Errorf("%d entries left: the new max_size was not applied", c.Entries)
		}
	}
	if len(h.client.Status().Caches) != 2 {
		t.Errorf("Status caches = %+v, want both folders", h.client.Status().Caches)
	}
}

func TestApplyFailureChangesNothing(t *testing.T) {
	t.Parallel()
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(h *harness, cfg *config.Config)
		want string
	}{
		{"password file", func(h *harness, _ *config.Config) {
			h.client.opts.ReadPassword = func(string) (string, error) { return "", errPasswordRead }
		}, "read password file"},
		{"cache open", func(_ *harness, cfg *config.Config) {
			good := testCache(filepath.Join(t.TempDir(), "ok"))
			bad := testCache(filepath.Join(notADir, "cache"))
			cfg.Mounts = append(cfg.Mounts,
				config.Mount{Cluster: testCluster, Path: "/", Target: "/mnt/b", Cache: good},
				config.Mount{Cluster: testCluster, Path: "/", Target: "/mnt/c", Cache: bad})
		}, "open cache"},
		{"shared cache", func(_ *harness, cfg *config.Config) {
			other := cfg.Mounts[0].Cache
			other.MaxSize = units.Size(1 << 30)
			cfg.Mounts = append(cfg.Mounts,
				config.Mount{Cluster: testCluster, Path: "/", Target: "/mnt/b", Cache: other})
		}, "share the cache directory"},
	} {
		h := newHarness(t, "a", "b")
		h.fc.put("media/file", pattern(1, testSize), fileTime)
		fsys := h.view(0)
		readFile(t, fsys, "media/file")
		cl, dir := h.client.clusters[testCluster], h.client.caches[h.cfg.Cache.Dir]
		cfg := cloneConfig(h.cfg)
		c := cfg.Clusters[testCluster]
		c.URL = h.fc.nodes[1].url() // a cluster that would be built new
		cfg.Clusters[testCluster] = c
		cfg.Mounts[0].Cache.MaxSize = 1 // limits that must not be applied
		tc.edit(h, cfg)

		err := h.client.Apply(cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: Apply = %v, want an error containing %q", tc.name, err, tc.want)
		}
		if h.client.clusters[testCluster] != cl || h.client.caches[h.cfg.Cache.Dir] != dir ||
			len(h.client.caches) != 1 {
			t.Errorf("%s: the client changed", tc.name)
		}
		if !h.client.Current(fsys) || clusterClosed(cl) || cacheClosed(dir) {
			t.Errorf("%s: the old cluster or cache was retired or closed", tc.name)
		}
		if err := dir.Evict(); err != nil || h.client.Status().Caches[0].Entries != 1 {
			t.Errorf("%s: Evict = %v, entries %+v: the new limits were applied",
				tc.name, err, h.client.Status().Caches)
		}
	}
}

func TestCurrentAndReleaseIgnoreOtherValues(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	other := newHarness(t, "a")
	fsys, foreign := h.view(0), other.view(0)
	cl := h.client.clusters[testCluster]

	if h.client.Current(foreign) || h.client.Current(nil) || h.client.Current(struct{ FS }{}) {
		t.Error("Current = true for a value that is not a view of this client")
	}
	h.client.Release(foreign)
	h.client.Release(nil)
	h.client.Release(struct{ FS }{})
	if cl.holds != 1 || other.client.clusters[testCluster].holds != 1 {
		t.Errorf("holds %d and %d, want 1 and 1: a foreign value was released",
			cl.holds, other.client.clusters[testCluster].holds)
	}
	h.client.Release(fsys)
	h.client.Release(fsys)
	if cl.holds != 0 || h.client.caches[h.cfg.Cache.Dir].holds != 0 {
		t.Errorf("holds %d and %d after releasing twice, want 0 and 0",
			cl.holds, h.client.caches[h.cfg.Cache.Dir].holds)
	}
}

func TestCloseClosesReplacedAndApplyAfterCloseFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	other := h.withMount(testCluster, "/mnt/other")
	fsys, err := func() (FS, error) { h.start(); return h.client.View(other) }()
	if err != nil {
		t.Fatal(err)
	}
	cl, dir := h.client.clusters[testCluster], h.client.caches[other.Cache.Dir]
	cfg := cloneConfig(h.cfg)
	cfg.Mounts = cfg.Mounts[:1]
	changed := cfg.Clusters[testCluster]
	changed.User = "bob"
	cfg.Clusters[testCluster] = changed
	h.apply(cfg)

	if err := h.client.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if !clusterClosed(cl) || !cacheClosed(dir) {
		t.Error("Close left a replaced cluster or cache open")
	}
	h.client.Release(fsys) // must not close them a second time
	if strings.Contains(h.logs.String(), "close replaced cache") {
		t.Error("Release closed a cache that Close had closed")
	}
	if err := h.client.Apply(cfg); err == nil {
		t.Error("Apply succeeded on a closed client")
	}
}

func TestApplyLogsCacheThatFailsToClose(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	other := h.withMount(testCluster, "/mnt/other")
	h.start()
	if err := h.client.caches[other.Cache.Dir].Close(); err != nil {
		t.Fatal(err)
	}
	cfg := cloneConfig(h.cfg)
	cfg.Mounts = cfg.Mounts[:1]

	h.apply(cfg)
	if !strings.Contains(h.logs.String(), "close replaced cache") {
		t.Errorf("no warning in the log: %s", h.logs.String())
	}
}

func TestEachClusterHasItsOwnHTTPClient(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	wide := h.cfg.Clusters[testCluster]
	wide.StreamsPerNode = 8
	h.cfg.Clusters["wide"] = wide
	h.cfg.Mounts = append(h.cfg.Mounts, config.Mount{Cluster: "wide", Path: "/", Target: "/mnt/wide",
		Offline: true, Cache: h.cfg.Cache})
	opts := h.options()
	opts.HTTPClient = nil
	c, err := newClient(h.cfg, opts, h.clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	narrow, wide8 := c.clusters[testCluster], c.clusters["wide"]
	if narrow.opts.HTTPClient == wide8.opts.HTTPClient || !narrow.ownHTTP || c.opts.HTTPClient != nil {
		t.Fatal("the clusters share an HTTP client")
	}
	for cl, streams := range map[*cluster]int{narrow: 2, wide8: 8} {
		got := cl.opts.HTTPClient.Transport.(*http.Transport).MaxConnsPerHost
		if want := streams + connHeadroom; got != want {
			t.Errorf("cluster %s: MaxConnsPerHost = %d, want %d", cl.name, got, want)
		}
	}

	shared := newHarness(t, "a")
	shared.start()
	cl := shared.client.clusters[testCluster]
	if cl.ownHTTP || cl.opts.HTTPClient != shared.client.opts.HTTPClient {
		t.Error("a cluster did not use the HTTP client that was passed in")
	}
}

func TestViewApplyReleaseStatusConcurrently(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	h.start()
	cfgs := [2]*config.Config{cloneConfig(h.cfg), cloneConfig(h.cfg)}
	c := cfgs[1].Clusters[testCluster]
	c.URL = h.fc.nodes[1].url()
	cfgs[1].Clusters[testCluster] = c
	cfgs[1].Mounts[0].Cache.MaxSize = 1 << 30

	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 40 {
				if g == 0 {
					if err := h.client.Apply(cfgs[i%2]); err != nil {
						t.Errorf("Apply: %v", err)
					}
					continue
				}
				fsys, err := h.client.View(h.cfg.Mounts[0])
				if err != nil {
					t.Errorf("View: %v", err)
					return
				}
				h.client.Current(fsys)
				_ = h.client.Status()
				_, _ = fsys.ReadDir(context.Background(), "")
				h.client.Release(fsys)
			}
		}()
	}
	wg.Wait()
	cl := h.client.clusters[testCluster]
	if cl.holds != 0 || len(h.client.retiredClusters) != 0 || len(h.client.retiredCaches) != 0 {
		t.Errorf("holds %d, retired %d clusters and %d caches, want none",
			cl.holds, len(h.client.retiredClusters), len(h.client.retiredCaches))
	}
}
