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
	for _, s := range [][3]string{
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
		f.Add([]byte(s[0]), []byte(s[1]), []byte(s[2]))
	}
	f.Fuzz(func(t *testing.T, settings, cl, mnt []byte) {
		for file, data := range map[string][]byte{settingsFile: settings, clusterFile: cl, mountFile: mnt} {
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := loadDir(dir, testCacheDir)
		if err != nil {
			// A rejected configuration must not come with a half built one.
			if cfg != nil {
				t.Fatalf("loadDir returned a configuration and the error %v", err)
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
	for name, c := range cfg.Clusters {
		if !namePattern.MatchString(name) || c.User == "" || c.PasswordFile == "" {
			t.Fatalf("cluster %q accepted with bad fields: %+v", name, c)
		}
		if strings.HasSuffix(c.URL, "/") || strings.ContainsAny(c.URL, "?#") {
			t.Fatalf("cluster %q has an unnormalized URL %q", name, c.URL)
		}
		if c.BlockSize < minBlockSize || c.BlockSize > maxBlockSize || c.ReadAhead < c.BlockSize {
			t.Fatalf("cluster %q has block size %d, read ahead %d", name, c.BlockSize, c.ReadAhead)
		}
		if c.StreamsPerNode < minStreamsPerNode || c.StreamsPerNode > maxStreamsPerNode {
			t.Fatalf("cluster %q has %d streams per node", name, c.StreamsPerNode)
		}
	}
	for i, m := range cfg.Mounts {
		if !namePattern.MatchString(m.Name) {
			t.Fatalf("mount %d has the name %q", i, m.Name)
		}
		if _, ok := cfg.Clusters[m.Cluster]; !ok {
			t.Fatalf("mount %d names the unknown cluster %q", i, m.Cluster)
		}
		// A mount path with a dot segment could reach outside the directory it names.
		if !strings.HasPrefix(m.Path, "/") || path.Clean(m.Path) != m.Path || hasDotDot(m.Path) {
			t.Fatalf("mount %d has the path %q", i, m.Path)
		}
		if !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target {
			t.Fatalf("mount %d has the target %q", i, m.Target)
		}
		for j, other := range cfg.Mounts[:i] {
			if contains(other.Target, m.Target) || contains(m.Target, other.Target) {
				t.Fatalf("mounts %d and %d overlap: %q and %q", j, i, other.Target, m.Target)
			}
		}
		// A cache inside a mount would cache the files it is serving into itself.
		for _, dir := range []string{cfg.Cache.Dir, m.Cache.Dir} {
			if !filepath.IsAbs(dir) || contains(m.Target, dir) || contains(dir, m.Target) {
				t.Fatalf("mount %d target %q conflicts with cache dir %q", i, m.Target, dir)
			}
		}
	}
}

// FuzzWithMounts feeds arbitrary values to a mount request, because they come from a
// command line that any user may type.
func FuzzWithMounts(f *testing.F) {
	for _, s := range [][3]string{
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
		f.Add(s[0], s[1], s[2])
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
