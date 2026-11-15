package config

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// FuzzLoadDir feeds arbitrary file contents to LoadDir, because the configuration is typed
// by people and edited by tools that may leave it damaged.
func FuzzLoadDir(f *testing.F) {
	const (
		cluster = "url: https://files.example.com/api\nuser: alice\npassword_file: p\n"
		mount   = "cluster: east\ntarget: /mnt/files\n"
	)
	dir := f.TempDir()
	for _, sub := range []string{confDirName, clustersDirName, mountsDirName} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			f.Fatal(err)
		}
	}
	settingsFile := filepath.Join(dir, confDirName, "a.yaml")
	clusterFile := filepath.Join(dir, clustersDirName, "east.yaml")
	mountFile := filepath.Join(dir, mountsDirName, "files.yaml")
	for _, seed := range [][3]string{
		{"", cluster, mount},
		{"cache: {max_size: unlimited, max_age: 30d}\n", cluster, mount},
		{"cache: {dir: /mnt/files/cache}\n", cluster, mount},
		{"cache: [", cluster, mount},
		{"a: 1\n---\nb: 2\n", cluster, mount},
		{"x: &a [*a]\n", cluster, mount},
		{"", "", ""},
		{"", "name: east\n" + cluster, mount},
		{"", "url: 'http://[::1'\nuser: a\npassword_file: p\n", mount},
		{"", strings.Replace(cluster, "/api", "/api//", 1), mount},
		{"", cluster, mount + "path: /a/../b\n"},
		{"", cluster, strings.Replace(mount, "/mnt/files", "/mnt/files/../x", 1)},
		{"", cluster, mount + "cache: {dir: /mnt}\n"},
		{"", cluster + "---\n" + cluster, mount},
		{"\x00\xff\xfe", cluster, mount},
	} {
		f.Add([]byte(seed[0]), []byte(seed[1]), []byte(seed[2]))
	}
	f.Fuzz(func(t *testing.T, settings, cl, mnt []byte) {
		for file, data := range map[string][]byte{settingsFile: settings, clusterFile: cl, mountFile: mnt} {
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := loadWithCacheDir(dir, testCacheDir)
		if err != nil {
			// A rejected configuration must not come with a half built one.
			if cfg != nil {
				t.Fatalf("loadWithCacheDir returned a configuration and the error %v", err)
			}
			return
		}
		checkLoaded(t, cfg)
	})
}

// checkLoaded states what every accepted configuration guarantees to the code that
// mounts and caches with it, because nothing downstream checks these again.
func checkLoaded(t *testing.T, cfg *Config) {
	t.Helper()
	for name, cluster := range cfg.Clusters {
		checkLoadedCluster(t, name, &cluster)
	}
	for index := range cfg.Mounts {
		checkLoadedMount(t, cfg, index)
	}
}

func checkLoadedCluster(t *testing.T, name string, cluster *Cluster) {
	t.Helper()
	if !namePattern.MatchString(name) || cluster.User == "" || cluster.PasswordFile == "" {
		t.Fatalf("cluster %q accepted with bad fields: %+v", name, cluster)
	}
	if strings.HasSuffix(cluster.URL, "/") || strings.ContainsAny(cluster.URL, "?#") {
		t.Fatalf("cluster %q has an unnormalized URL %q", name, cluster.URL)
	}
	checkLoadedClusterLimits(t, name, cluster)
}

func checkLoadedClusterLimits(t *testing.T, name string, cluster *Cluster) {
	t.Helper()
	if cluster.BlockSize < minBlockSize || cluster.BlockSize > maxBlockSize ||
		cluster.ReadAhead < cluster.BlockSize {
		t.Fatalf("cluster %q has block size %d, read ahead %d", name, cluster.BlockSize, cluster.ReadAhead)
	}
	if cluster.StreamsPerNode < minStreamsPerNode || cluster.StreamsPerNode > maxStreamsPerNode {
		t.Fatalf("cluster %q has %d streams per node", name, cluster.StreamsPerNode)
	}
}

func checkLoadedMount(t *testing.T, cfg *Config, index int) {
	t.Helper()
	mount := &cfg.Mounts[index]
	if !namePattern.MatchString(mount.Name) {
		t.Fatalf("mount %d has the name %q", index, mount.Name)
	}
	if _, ok := cfg.Clusters[mount.Cluster]; !ok {
		t.Fatalf("mount %d names the unknown cluster %q", index, mount.Cluster)
	}
	// A mount path with a dot segment could reach outside the directory it names.
	if !strings.HasPrefix(mount.Path, "/") || path.Clean(mount.Path) != mount.Path || hasDotDot(mount.Path) {
		t.Fatalf("mount %d has the path %q", index, mount.Path)
	}
	if !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) != mount.Target {
		t.Fatalf("mount %d has the target %q", index, mount.Target)
	}
	checkLoadedMountTargets(t, cfg.Mounts, index)
	checkLoadedMountCaches(t, cfg.Cache.Dir, cfg.Mounts, index)
}

// checkLoadedMountTargets checks the target of one mount against the mounts before it, so
// that each pair is checked once.
func checkLoadedMountTargets(t *testing.T, mounts []Mount, index int) {
	t.Helper()
	target := mounts[index].Target
	for earlier := range mounts[:index] {
		other := mounts[earlier].Target
		if contains(other, target) || contains(target, other) {
			t.Fatalf("mounts %d and %d overlap: %q and %q", earlier, index, other, target)
		}
	}
}

// checkLoadedMountCaches checks that no cache folder of a mount lies in or around a target.
// A cache inside a mount would cache the files it is serving into itself.
func checkLoadedMountCaches(t *testing.T, topCacheDir string, mounts []Mount, index int) {
	t.Helper()
	target := mounts[index].Target
	for _, dir := range []string{topCacheDir, mounts[index].Cache.Dir} {
		if !filepath.IsAbs(dir) || contains(target, dir) || contains(dir, target) {
			t.Fatalf("mount %d target %q conflicts with cache dir %q", index, target, dir)
		}
	}
}

// FuzzWithMounts feeds arbitrary values to a mount request, because they come from a
// command line that any user may type.
func FuzzWithMounts(f *testing.F) {
	for _, seed := range [][3]string{
		{"example", "/media", "/mnt/media"},
		{"example", "", "/mnt/media"},
		{"example", "/a/../b", "/mnt/media"},
		{"example", "media", "/mnt/media"},
		{"example", "//a//b/", "/mnt/x/../media/"},
		{"example", "/", "relative"},
		{"example", "/", ""},
		{"nope", "/", "/mnt/media"},
		{"", "", ""},
		{"example", "/", "/mnt/files/sub"},
		{"example", "/", "/mnt"},
		{"example", "/", "/var/cache/test/x"},
		{"example", "/", "/"},
		{"example", "/\x00", "/mnt/\xff"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, cluster, p, target string) {
		cfg := testConfig()
		got, err := WithMounts(cfg, Requested{Cluster: cluster, Path: p, Target: target})
		if !reflect.DeepEqual(cfg, testConfig()) {
			t.Fatalf("cfg was changed: %+v", cfg)
		}
		if err != nil {
			if got != nil {
				t.Fatalf("WithMounts returned a configuration and the error %v", err)
			}
			return
		}
		checkLoaded(t, got)
		m := got.Mounts[len(got.Mounts)-1]
		if !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target ||
			!strings.HasPrefix(m.Path, "/") || m.Name != RequestedName(target) {
			t.Fatalf("accepted the mount %+v", m)
		}
	})
}
