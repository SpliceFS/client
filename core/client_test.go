package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BlueRival/splicefs/client/config"
	"github.com/BlueRival/splicefs/internal/units"
	"github.com/BlueRival/splicefs/internal/wire"
)

func TestNewWithDefaultOptions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, 10), fileTime)
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte(testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl := h.cfg.Clusters[testCluster]
	cl.PasswordFile = passwordFile
	h.cfg.Clusters[testCluster] = cl

	c, err := New(h.cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	fsys, err := c.View(h.cfg.Mounts[0])
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
	for _, tc := range []struct {
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
			cfg.Mounts = append(cfg.Mounts, config.Mount{Cluster: testCluster, Path: "/", Target: "/mnt/other", Cache: testCache(filepath.Join(notADir, "cache"))})
		}, "open cache"},
		{"shared cache with other limits", func(cfg *config.Config, _ *Options) {
			other := cfg.Mounts[0].Cache
			other.MaxSize = units.Size(1 << 30)
			cfg.Mounts = append(cfg.Mounts, config.Mount{Cluster: testCluster, Path: "/media", Target: "/mnt/other", Cache: other})
		}, "/mnt/files and /mnt/other share the cache directory"},
	} {
		h := newHarness(t, "a")
		opts := h.options()
		tc.edit(h.cfg, &opts)
		_, err := New(h.cfg, opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestMountsShareACacheDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	h.addMount("/media", "/mnt/media")
	h.cfg.Mounts[1].Cache.MinFileSize = 1 // a per-mount setting, not a limit
	readFile(t, h.view(0), "media/file")
	readFile(t, h.view(1), "file")

	s := h.client.Status()
	if len(s.Caches) != 1 || s.Caches[0].Dir != h.cfg.Cache.Dir || s.Caches[0].Entries != 1 {
		t.Errorf("Status caches = %+v, want one directory with one entry", s.Caches)
	}
}

func TestViewFailures(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.start()
	unknownCluster := h.cfg.Mounts[0]
	unknownCluster.Cluster = "other"
	unknownCache := h.cfg.Mounts[0]
	unknownCache.Cache.Dir = filepath.Join(t.TempDir(), "elsewhere")
	for _, m := range []config.Mount{unknownCluster, unknownCache} {
		if _, err := h.client.View(m); err == nil {
			t.Errorf("View(%+v) succeeded", m)
		}
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	h.fc.extra = []wire.Node{{Name: "broken", URL: "ftp://192.0.2.1/api"}}
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	h.cfg.Clusters["idle"] = h.cfg.Clusters[testCluster]
	fsys := h.view(0)

	before := h.client.Status()
	if len(before.Clusters) != 2 || before.Clusters[0].Name != testCluster || len(before.Clusters[0].Nodes) != 0 {
		t.Errorf("Status before any use = %+v, want both clusters without nodes", before.Clusters)
	}
	readFile(t, fsys, "media/file")
	h.client.clusters[testCluster].meter.Observe("b", 1000, time.Second)

	s := h.client.Status()
	want := []NodeStatus{{Name: "a"}, {Name: "b", BytesPerSecond: 1000, Measured: true}}
	got := s.Clusters[0].Nodes
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Status nodes = %+v, want %+v", got, want)
	}
	if s.Clusters[1].Name != "idle" || len(s.Clusters[1].Nodes) != 0 {
		t.Errorf("Status of the idle cluster = %+v", s.Clusters[1])
	}
	if len(s.Caches) != 1 || s.Caches[0].Entries != 1 || s.Caches[0].Bytes != testSize {
		t.Errorf("Status caches = %+v, want the one file that was read, all %d bytes", s.Caches, testSize)
	}
	if !strings.Contains(h.logs.String(), "cannot dial node") {
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
