package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/common/units"
	"github.com/SpliceFS/common/wire"
)

func TestNewWithDefaultOptions(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl := env.cfg.Clusters[testCluster]
	cl.PasswordFile = passwordFile
	env.cfg.Clusters[testCluster] = cl

	created, err := New(env.cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFile(t, created)
	fsys, err := created.View(env.cfg.Mounts[0])
	if err != nil {
		t.Fatal(err)
	}
	// The fake node accepts the password only without the newline of the file, and is
	// reached through the client's own transport and dialer.
	if a, err := fsys.Stat(context.Background(), "media/file"); err != nil || a.Size != 10 {
		t.Errorf("Stat = %+v, %v", a, err)
	}
}

func TestNewFailures(t *testing.T) {
	t.Parallel()
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name string
		edit func(*config.Config, *Options)
		want string
	}{
		{"missing password file", func(cfg *config.Config, opts *Options) {
			opts.ReadPassword = nil
			cl := cfg.Clusters[testCluster]
			cl.PasswordFile = filepath.Join(t.TempDir(), "none")
			cfg.Clusters[testCluster] = cl
		}, "read password file"},
		{"cache directory cannot be made", func(cfg *config.Config, _ *Options) {
			cfg.Mounts = append(cfg.Mounts, config.Mount{
				Cluster: testCluster, Path: "/", Target: "/mnt/other", Cache: testCache(filepath.Join(notADir, "cache")),
			})
		}, "open cache"},
		{"shared cache with other limits", func(cfg *config.Config, _ *Options) {
			other := cfg.Mounts[0].Cache
			other.MaxSize = units.Size(1 << 30)
			cfg.Mounts = append(cfg.Mounts, config.Mount{
				Cluster: testCluster, Path: "/media", Target: "/mnt/other", Cache: other,
			})
		}, "/mnt/files and /mnt/other share the cache directory"},
	} {
		h := newHarness(t, "a")
		opts := h.options()
		testCase.edit(h.cfg, &opts)
		_, err := New(h.cfg, opts)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: New = %v, want an error containing %q", testCase.name, err, testCase.want)
		}
	}
}

func TestMountsShareACacheDirectory(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	env.addMount("/media", "/mnt/media")
	env.cfg.Mounts[1].Cache.MinFileSize = 1 // a per-mount setting, not a limit
	readFile(t, env.view(0), "media/file")
	readFile(t, env.view(1), "file")

	s := env.client.Status()
	if len(s.Caches) != 1 || s.Caches[0].Dir != env.cfg.Cache.Dir || s.Caches[0].Entries != 1 {
		t.Errorf("Status caches = %+v, want one directory with one entry", s.Caches)
	}
}

func TestViewFailures(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.start()
	unknownCluster := env.cfg.Mounts[0]
	unknownCluster.Cluster = "other"
	unknownCache := env.cfg.Mounts[0]
	unknownCache.Cache.Dir = filepath.Join(t.TempDir(), "elsewhere")
	for _, m := range []config.Mount{unknownCluster, unknownCache} {
		if _, err := env.client.View(m); err == nil {
			t.Errorf("View(%+v) succeeded", m)
		}
	}
}

func TestStatusBeforeAnyUse(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	env.cfg.Clusters["idle"] = env.cfg.Clusters[testCluster]
	env.start()

	before := env.client.Status()
	if len(before.Clusters) != 2 || before.Clusters[0].Name != testCluster || len(before.Clusters[0].Nodes) != 0 {
		t.Errorf("Status before any use = %+v, want both clusters without nodes", before.Clusters)
	}
}

func TestStatusAfterUse(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	env.fc.extra = []wire.Node{{Name: "broken", URL: "ftp://192.0.2.1/api"}}
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	env.cfg.Clusters["idle"] = env.cfg.Clusters[testCluster]
	fsys := env.view(0)

	readFile(t, fsys, "media/file")
	env.client.clusters[testCluster].meter.Observe("b", 1000, time.Second)

	status := env.client.Status()
	want := []NodeStatus{{Name: "a"}, {Name: "b", BytesPerSecond: 1000, Measured: true}}
	if got := status.Clusters[0].Nodes; !slices.Equal(got, want) {
		t.Errorf("Status nodes = %+v, want %+v", got, want)
	}
	if status.Clusters[1].Name != "idle" || len(status.Clusters[1].Nodes) != 0 {
		t.Errorf("Status of the idle cluster = %+v", status.Clusters[1])
	}
	if len(status.Caches) != 1 || status.Caches[0].Entries != 1 || status.Caches[0].Bytes != testSize {
		t.Errorf("Status caches = %+v, want the one file that was read, all %d bytes", status.Caches, testSize)
	}
	if !strings.Contains(env.logs.String(), "cannot dial node") {
		t.Error("the node with a bad URL was not reported")
	}
}

func TestListingTTL(t *testing.T) {
	t.Parallel()
	mount := func(cluster string, ttl time.Duration) config.Mount {
		return config.Mount{Cluster: cluster, Cache: config.Cache{ListingTTL: units.Duration(ttl)}}
	}
	cfg := &config.Config{
		Cache:  config.Cache{ListingTTL: units.Duration(30 * time.Second)},
		Mounts: []config.Mount{mount("a", time.Minute), mount("a", 0), mount("a", time.Hour), mount("b", time.Hour)},
	}
	for cluster, want := range map[string]time.Duration{"a": 0, "b": time.Hour, "c": 30 * time.Second} {
		if got := listingTTL(cfg, cluster); got != want {
			t.Errorf("listingTTL(%s) = %v, want %v", cluster, got, want)
		}
	}
}

func TestCloseEndsSessions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	fsys := h.view(0)
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDir(context.Background(), ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ReadDir after Close = %v, want ErrUnavailable", err)
	}
}

// TestReadPasswordFileErrorNamesTheFile checks that the message of the operating system,
// which names the file, is not given another prefix.
func TestReadPasswordFileErrorNamesTheFile(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "none")
	_, err := readPasswordFile(missing)
	if want := "open " + missing + ": no such file or directory"; err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}
