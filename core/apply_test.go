package core

import (
	"context"
	"errors"
	"maps"
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
	maps.Copy(clone.Clusters, cfg.Clusters)
	clone.Mounts = append([]config.Mount(nil), cfg.Mounts...)
	return &clone
}

func clusterClosed(cl *cluster) bool {
	_, err := cl.nodes(context.Background())
	return errors.Is(err, session.ErrClosed)
}

var errPasswordRead = errors.New("password file unreadable")

func cacheClosed(d *cacheDir) bool { return errors.Is(d.Evict(), cache.ErrClosed) }

// withMount adds a mount of cluster name at target, with its own cache directory.
func (h *harness) withMount(cluster, target string) config.Mount {
	m := config.Mount{
		Cluster: cluster, Path: "/", Target: target, Offline: true,
		Cache: testCache(h.t.TempDir()),
	}
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
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	fsys := env.view(0)
	if _, err := fsys.ReadDir(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	cl, dir := env.client.clusters[testCluster], env.client.caches[env.cfg.Cache.Dir]

	env.apply(cloneConfig(env.cfg))
	if env.client.clusters[testCluster] != cl || env.client.caches[env.cfg.Cache.Dir] != dir {
		t.Error("Apply replaced a cluster or a cache that had not changed")
	}
	if !env.client.Current(fsys) {
		t.Error("Current = false after an Apply that changed nothing")
	}
	if _, err := fsys.ReadDir(context.Background(), "media"); err != nil {
		t.Fatal(err)
	}
	if n := env.fc.count("login"); n != 1 {
		t.Errorf("%d logins, want the one of the first listing: the session was replaced", n)
	}
}

func TestApplyReplacesChangedCluster(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
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
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			checkClusterReplaced(t, testCase.edit)
		})
	}
}

// checkClusterReplaced applies the configuration that edit makes and checks that the
// cluster is replaced, and that the old one stays open until its last view is released.
func checkClusterReplaced(t *testing.T, edit func(env *harness, cfg *config.Config)) {
	t.Helper()
	env := newHarness(t, "a", "b")
	fsys := env.view(0)
	old := env.client.clusters[testCluster]
	cfg := cloneConfig(env.cfg)
	edit(env, cfg)

	env.apply(cfg)
	if env.client.clusters[testCluster] == old {
		t.Fatal("the cluster was kept")
	}
	if env.client.Current(fsys) {
		t.Error("Current = true for a view of the replaced cluster")
	}
	if clusterClosed(old) {
		t.Error("the replaced cluster was closed while a view holds it")
	}
	env.client.Release(fsys)
	if !clusterClosed(old) {
		t.Error("the replaced cluster stayed open after its last view was released")
	}
	if clusterClosed(env.client.clusters[testCluster]) {
		t.Error("the new cluster is closed")
	}
}

// viewUse says whether a view of a mount is still in use when the mount is removed.
type viewUse int

const (
	viewReleased viewUse = iota
	viewHeld
)

func TestApplyRemovesClusterAndCache(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		use  viewUse
	}{{"released", viewReleased}, {"held", viewHeld}} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			checkRemovedMount(t, testCase.use)
		})
	}
}

// checkRemovedMount removes a mount with its own cluster and cache. A view that still
// holds the mount keeps them open until it is released.
func checkRemovedMount(t *testing.T, use viewUse) {
	t.Helper()
	held := use == viewHeld
	env := newHarness(t, "a")
	env.cfg.Clusters["other"] = env.cfg.Clusters[testCluster]
	other := env.withMount("other", "/mnt/other")
	env.start()
	fsys, err := env.client.View(other)
	if err != nil {
		t.Fatal(err)
	}
	clus, dir := env.client.clusters["other"], env.client.caches[other.Cache.Dir]
	if !held {
		env.client.Release(fsys)
	}
	cfg := cloneConfig(env.cfg)
	delete(cfg.Clusters, "other")
	cfg.Mounts = cfg.Mounts[:1]

	env.apply(cfg)
	if env.client.Current(fsys) {
		t.Error("Current = true for a view of a removed mount")
	}
	if status := env.client.Status(); len(status.Clusters) != 1 || len(status.Caches) != 1 {
		t.Errorf("Status = %+v, want the one remaining cluster and cache", status)
	}
	if clusterClosed(clus) == held || cacheClosed(dir) == held {
		t.Errorf("removed cluster closed %v, cache closed %v at Apply", clusterClosed(clus), cacheClosed(dir))
	}
	env.client.Release(fsys)
	if !clusterClosed(clus) || !cacheClosed(dir) {
		t.Error("removed cluster or cache still open after the release")
	}
}

// TestApplyTakesBackARetiredCache removes a mount whose cache a view still holds and adds
// a mount on the same folder again. The held cache is still open and its folder locked, so
// the folder must not be opened a second time.
func TestApplyTakesBackARetiredCache(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	other := env.withMount(testCluster, "/mnt/other")
	env.start()
	fsys, err := env.client.View(other)
	if err != nil {
		t.Fatal(err)
	}
	dir := env.client.caches[other.Cache.Dir]
	without := cloneConfig(env.cfg)
	without.Mounts = without.Mounts[:1]
	env.apply(without)
	if !dir.retired || len(env.client.retiredCaches) != 1 {
		t.Fatalf("retired = %v, retired caches %d, want the held cache retired", dir.retired, len(env.client.retiredCaches))
	}

	again := cloneConfig(env.cfg)
	again.Mounts[1].Cache.MaxSize = 1
	env.apply(again)
	checkCacheInUseAgain(t, env, dir, fsys)

	env.client.Release(fsys)
	if cacheClosed(dir) {
		t.Error("the release of the old view closed a cache that is in use again")
	}
	if err := env.client.Close(); err != nil {
		t.Errorf("Close = %v, want the cache closed once", err)
	}
	if !cacheClosed(dir) {
		t.Error("Close left the cache open")
	}
}

// checkCacheInUseAgain checks that the client took back dir, the cache that fsys holds,
// instead of opening its folder anew.
func checkCacheInUseAgain(t *testing.T, env *harness, dir *cacheDir, fsys FS) {
	t.Helper()
	if env.client.caches[dir.dir] != dir {
		t.Error("the folder was opened a second time")
	}
	if dir.retired || len(env.client.retiredCaches) != 0 || !env.client.Current(fsys) {
		t.Errorf("retired = %v, retired caches %d, Current = %v, want the cache back in use",
			dir.retired, len(env.client.retiredCaches), env.client.Current(fsys))
	}
}

func TestApplyKeepsCacheWithNewLimits(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	readFile(t, env.view(0), "media/file")
	dir := env.client.caches[env.cfg.Cache.Dir]
	cfg := cloneConfig(env.cfg)
	cfg.Mounts[0].Cache.MaxSize = 1
	newDir := env.withMount(testCluster, "/mnt/new").Cache.Dir
	cfg.Mounts = append(cfg.Mounts, env.cfg.Mounts[1])

	if err := env.cache(0).Evict(); err != nil || env.client.Status().Caches[0].Entries != 1 {
		t.Fatalf("Evict with the old limits = %v, entries %+v", err, env.client.Status().Caches)
	}
	env.apply(cfg)
	if env.client.caches[env.cfg.Cache.Dir] != dir {
		t.Error("the cache folder was reopened")
	}
	if env.client.caches[newDir] == nil {
		t.Error("the new cache folder was not opened")
	}
	if err := dir.Evict(); err != nil {
		t.Fatal(err)
	}
	for _, c := range env.client.Status().Caches {
		if c.Dir == env.cfg.Cache.Dir && c.Entries != 0 {
			t.Errorf("%d entries left: the new max_size was not applied", c.Entries)
		}
	}
	if len(env.client.Status().Caches) != 2 {
		t.Errorf("Status caches = %+v, want both folders", env.client.Status().Caches)
	}
}

func TestApplyFailureChangesNothing(t *testing.T) {
	t.Parallel()
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
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
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			checkFailedApplyChangesNothing(t, testCase.edit, testCase.want)
		})
	}
}

// checkFailedApplyChangesNothing applies a configuration that edit makes invalid and
// checks that the error contains want and that the old clusters and caches stay in use.
func checkFailedApplyChangesNothing(t *testing.T, edit func(env *harness, cfg *config.Config), want string) {
	t.Helper()
	env := newHarness(t, "a", "b")
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	fsys := env.view(0)
	readFile(t, fsys, "media/file")
	clus, dir := env.client.clusters[testCluster], env.client.caches[env.cfg.Cache.Dir]
	cfg := cloneConfig(env.cfg)
	clusterCfg := cfg.Clusters[testCluster]
	clusterCfg.URL = env.fc.nodes[1].url() // a cluster that would be built new
	cfg.Clusters[testCluster] = clusterCfg
	cfg.Mounts[0].Cache.MaxSize = 1 // limits that must not be applied
	edit(env, cfg)

	err := env.client.Apply(cfg)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Apply = %v, want an error containing %q", err, want)
	}
	checkOldGenerationInUse(t, env, fsys, clus, dir)
}

// checkOldGenerationInUse checks that the client still uses the cluster and cache it had
// before a failed Apply, and that the old cache limits are in force.
func checkOldGenerationInUse(t *testing.T, env *harness, fsys FS, clus *cluster, dir *cacheDir) {
	t.Helper()
	if env.client.clusters[testCluster] != clus || env.client.caches[env.cfg.Cache.Dir] != dir ||
		len(env.client.caches) != 1 {
		t.Error("the client changed")
	}
	if !env.client.Current(fsys) || clusterClosed(clus) || cacheClosed(dir) {
		t.Error("the old cluster or cache was retired or closed")
	}
	if err := dir.Evict(); err != nil || env.client.Status().Caches[0].Entries != 1 {
		t.Errorf("Evict = %v, entries %+v: the new limits were applied", err, env.client.Status().Caches)
	}
}

func TestCurrentAndReleaseIgnoreOtherValues(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	other := newHarness(t, "a")
	fsys, foreign := env.view(0), other.view(0)
	clus := env.client.clusters[testCluster]

	if env.client.Current(foreign) || env.client.Current(nil) || env.client.Current(struct{ FS }{}) {
		t.Error("Current = true for a value that is not a view of this client")
	}
	env.client.Release(foreign)
	env.client.Release(nil)
	env.client.Release(struct{ FS }{})
	if clus.holds != 1 || other.client.clusters[testCluster].holds != 1 {
		t.Errorf("holds %d and %d, want 1 and 1: a foreign value was released",
			clus.holds, other.client.clusters[testCluster].holds)
	}
	env.client.Release(fsys)
	env.client.Release(fsys)
	if clus.holds != 0 || env.client.caches[env.cfg.Cache.Dir].holds != 0 {
		t.Errorf("holds %d and %d after releasing twice, want 0 and 0",
			clus.holds, env.client.caches[env.cfg.Cache.Dir].holds)
	}
}

func TestCloseClosesReplacedAndApplyAfterCloseFails(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	other := env.withMount(testCluster, "/mnt/other")
	fsys, err := func() (FS, error) { env.start(); return env.client.View(other) }()
	if err != nil {
		t.Fatal(err)
	}
	clus, dir := env.client.clusters[testCluster], env.client.caches[other.Cache.Dir]
	cfg := cloneConfig(env.cfg)
	cfg.Mounts = cfg.Mounts[:1]
	changed := cfg.Clusters[testCluster]
	changed.User = "bob"
	cfg.Clusters[testCluster] = changed
	env.apply(cfg)

	if err := env.client.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if !clusterClosed(clus) || !cacheClosed(dir) {
		t.Error("Close left a replaced cluster or cache open")
	}
	env.client.Release(fsys) // must not close them a second time
	if strings.Contains(env.logs.String(), "close replaced cache") {
		t.Error("Release closed a cache that Close had closed")
	}
	if err := env.client.Apply(cfg); err == nil {
		t.Error("Apply succeeded on a closed client")
	}
}

func TestApplyLogsCacheThatFailsToClose(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	other := env.withMount(testCluster, "/mnt/other")
	env.start()
	if err := env.client.caches[other.Cache.Dir].Close(); err != nil {
		t.Fatal(err)
	}
	cfg := cloneConfig(env.cfg)
	cfg.Mounts = cfg.Mounts[:1]

	env.apply(cfg)
	if !strings.Contains(env.logs.String(), "close replaced cache") {
		t.Errorf("no warning in the log: %s", env.logs.String())
	}
}

func TestEachClusterHasItsOwnHTTPClient(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	wide := env.cfg.Clusters[testCluster]
	wide.StreamsPerNode = 8
	env.cfg.Clusters["wide"] = wide
	env.cfg.Mounts = append(env.cfg.Mounts, config.Mount{
		Cluster: "wide", Path: "/", Target: "/mnt/wide",
		Offline: true, Cache: env.cfg.Cache,
	})
	opts := env.options()
	opts.HTTPClient = nil
	client, err := newClient(env.cfg, opts, env.clock)
	if err != nil {
		t.Fatal(err)
	}
	closeAfter(t, client)

	narrow, wide8 := client.clusters[testCluster], client.clusters["wide"]
	if narrow.opts.HTTPClient == wide8.opts.HTTPClient || !narrow.ownHTTP || client.opts.HTTPClient != nil {
		t.Fatal("the clusters share an HTTP client")
	}
	for clus, streams := range map[*cluster]int{narrow: 2, wide8: 8} {
		transport, ok := clus.opts.HTTPClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("cluster %s: the transport is a %T, want an *http.Transport", clus.name, clus.opts.HTTPClient.Transport)
		}
		if want := streams + connHeadroom; transport.MaxConnsPerHost != want {
			t.Errorf("cluster %s: MaxConnsPerHost = %d, want %d", clus.name, transport.MaxConnsPerHost, want)
		}
	}

	shared := newHarness(t, "a")
	shared.start()
	clus := shared.client.clusters[testCluster]
	if clus.ownHTTP || clus.opts.HTTPClient != shared.client.opts.HTTPClient {
		t.Error("a cluster did not use the HTTP client that was passed in")
	}
}

func TestViewApplyReleaseStatusConcurrently(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	env.start()
	cfgs := [2]*config.Config{cloneConfig(env.cfg), cloneConfig(env.cfg)}
	clusterCfg := cfgs[1].Clusters[testCluster]
	clusterCfg.URL = env.fc.nodes[1].url()
	cfgs[1].Clusters[testCluster] = clusterCfg
	cfgs[1].Mounts[0].Cache.MaxSize = 1 << 30

	var group sync.WaitGroup
	group.Go(func() { applyRepeatedly(t, env, cfgs) })
	for range 3 {
		group.Go(func() { viewRepeatedly(t, env) })
	}
	group.Wait()
	clus := env.client.clusters[testCluster]
	if clus.holds != 0 || len(env.client.retiredClusters) != 0 || len(env.client.retiredCaches) != 0 {
		t.Errorf("holds %d, retired %d clusters and %d caches, want none",
			clus.holds, len(env.client.retiredClusters), len(env.client.retiredCaches))
	}
}

// concurrentRounds is how often each goroutine of the concurrency test repeats its work.
const concurrentRounds = 40

func applyRepeatedly(t *testing.T, env *harness, cfgs [2]*config.Config) {
	t.Helper()
	for round := range concurrentRounds {
		if err := env.client.Apply(cfgs[round%2]); err != nil {
			t.Errorf("Apply: %v", err)
		}
	}
}

func viewRepeatedly(t *testing.T, env *harness) {
	t.Helper()
	for range concurrentRounds {
		fsys, err := env.client.View(env.cfg.Mounts[0])
		if err != nil {
			t.Errorf("View: %v", err)
			return
		}
		env.client.Current(fsys)
		_ = env.client.Status()
		//nolint:errcheck // Only the race detector matters here; the listing may fail while Apply replaces the cluster.
		_, _ = fsys.ReadDir(context.Background(), "")
		env.client.Release(fsys)
	}
}
