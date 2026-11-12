package config

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzLoad feeds arbitrary file contents to Load, because the configuration is typed by
// people and edited by tools that may leave it damaged.
func FuzzLoad(f *testing.F) {
	for _, s := range []string{
		validYAML,
		"",
		"# nothing\n",
		"clusters: [",
		validYAML + "---\n" + validYAML,
		validYAML + "log: {}\n",
		validYAML + "cache:\n  max_size: unlimited\n  max_age: 30d\n",
		validYAML + "cache:\n  dir: /mnt/files/cache\n",
		strings.Replace(validYAML, "target: /mnt/files", "target: /mnt/files/../x", 1),
		strings.Replace(validYAML, "mounts:",
			"mounts:\n  - cluster: example\n    path: /a/../b\n    target: /mnt/b", 1),
		strings.Replace(validYAML, "https://files.example.com/api", "https://files.example.com/api//", 1),
		"clusters: &a {x: *a}\nmounts: *a\n",
		"clusters:\n  example: {url: 'http://[::1', user: a, password_file: p}\n",
		"\x00\xff\xfe",
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	file := filepath.Join(dir, FileName)
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(dir)
		if err != nil {
			// A rejected file must not come with a half built configuration.
			if cfg != nil {
				t.Fatalf("Load returned a configuration and the error %v", err)
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
	if len(cfg.Clusters) == 0 || len(cfg.Mounts) == 0 {
		t.Fatalf("accepted a configuration without clusters or mounts: %+v", cfg)
	}
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
