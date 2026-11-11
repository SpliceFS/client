package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/client/internal/units"
)

const validYAML = `clusters:
  example:
    url: https://files.example.com/api
    user: alice
    password_file: secrets/example
mounts:
  - cluster: example
    target: /mnt/files
`

// writeConfig writes content as config.yaml in a new directory and returns it.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mutate replaces old with new in validYAML and fails if old is not there.
func mutate(t *testing.T, old, replacement string) string {
	t.Helper()
	if !strings.Contains(validYAML, old) {
		t.Fatalf("validYAML does not contain %q", old)
	}
	return strings.Replace(validYAML, old, replacement, 1)
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	dir := writeConfig(t, validYAML)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cache := Cache{
		Dir:         "/var/cache/splicefs",
		MaxSize:     10 << 30,
		MaxAge:      units.UnlimitedDuration,
		MinFree:     1 << 30,
		MinFileSize: 0,
		Revalidate:  units.Duration(time.Hour),
		ListingTTL:  units.Duration(30 * time.Second),
	}
	want := &Config{
		Cache: cache,
		Clusters: map[string]Cluster{"example": {
			URL:            "https://files.example.com/api",
			User:           "alice",
			PasswordFile:   filepath.Join(dir, "secrets/example"),
			BlockSize:      5 << 20,
			StreamsPerNode: 4,
			ReadAhead:      64 << 20,
		}},
		Mounts: []Mount{{Cluster: "example", Path: "/", Target: "/mnt/files", Offline: true, Cache: cache}},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestLoadFull(t *testing.T) {
	t.Parallel()
	dir := writeConfig(t, `cache:
  dir: cache
  max_size: 200GiB
  max_age: 30d
  min_free: 10GiB
  min_file_size: 4KiB
  revalidate: 2h
  listing_ttl: 0
clusters:
  example:
    url: http://192.0.2.10:8480/api/
    user: alice
    password_file: /etc/secret
    block_size: 1MiB
    streams_per_node: 8
    read_ahead: 16MiB
  other.v2:
    url: https://other.example.com
    user: bob
    password_file: ./other/../secret
mounts:
  - cluster: example
    path: /media//movies/
    target: /srv/movies/
    allow_other: true
    offline: false
    cache:
      dir: /data/movies
      max_size: unlimited
      max_age: unlimited
  - cluster: other.v2
    path: /
    target: mnt/other
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	top := Cache{
		Dir:         filepath.Join(dir, "cache"),
		MaxSize:     200 << 30,
		MaxAge:      units.Duration(30 * 24 * time.Hour),
		MinFree:     10 << 30,
		MinFileSize: 4 << 10,
		Revalidate:  units.Duration(2 * time.Hour),
		ListingTTL:  0,
	}
	override := top
	override.Dir = "/data/movies"
	override.MaxSize = units.UnlimitedSize
	override.MaxAge = units.UnlimitedDuration
	want := &Config{
		Cache: top,
		Clusters: map[string]Cluster{
			"example": {
				URL: "http://192.0.2.10:8480/api", User: "alice", PasswordFile: "/etc/secret",
				BlockSize: 1 << 20, StreamsPerNode: 8, ReadAhead: 16 << 20,
			},
			"other.v2": {
				URL: "https://other.example.com", User: "bob", PasswordFile: filepath.Join(dir, "secret"),
				BlockSize: 5 << 20, StreamsPerNode: 4, ReadAhead: 64 << 20,
			},
		},
		Mounts: []Mount{
			{Cluster: "example", Path: "/media/movies", Target: "/srv/movies", AllowOther: true, Cache: override},
			{Cluster: "other.v2", Path: "/", Target: filepath.Join(dir, "mnt/other"), Offline: true, Cache: top},
		},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestLoadExplicitZeroValuesKeepTheirMeaning(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, validYAML+"    offline: false\n    cache:\n      min_file_size: 0\n      listing_ttl: 0\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m := cfg.Mounts[0]; m.Offline || m.Cache.MinFileSize != 0 || m.Cache.ListingTTL != 0 {
		t.Errorf("mount = %+v, want offline false, min_file_size 0 and listing_ttl 0", m)
	}
}

func TestLoadMountCacheInheritsKeyByKey(t *testing.T) {
	t.Parallel()
	yamlText := "cache:\n  max_size: 50GiB\n  min_free: 2GiB\n" + validYAML + "    cache:\n      max_size: 1TiB\n"
	cfg, err := Load(writeConfig(t, yamlText))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Mounts[0].Cache
	want := cfg.Cache
	want.MaxSize = 1 << 40
	if got != want {
		t.Errorf("mount cache = %+v, want %+v", got, want)
	}
	if cfg.Cache.MaxSize != 50<<30 || got.MinFree != 2<<30 {
		t.Errorf("top-level cache = %+v, mount cache = %+v", cfg.Cache, got)
	}
}

func TestLoadNormalizes(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, old, replacement, wantURL, wantPath string }{
		{"url trailing slash", "/api\n", "/api/\n", "https://files.example.com/api", "/"},
		{"url root slash", "/api\n", "/\n", "https://files.example.com", "/"},
		{"url without path", "/api\n", "\n", "https://files.example.com", "/"},
		{"path cleaned", "cluster: example\n", "cluster: example\n    path: /a//b/./c/\n", "https://files.example.com/api", "/a/b/c"},
		{"path with dots in names", "cluster: example\n", "cluster: example\n    path: /a..b/..c\n", "https://files.example.com/api", "/a..b/..c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(writeConfig(t, mutate(t, tc.old, tc.replacement)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Clusters["example"].URL; got != tc.wantURL {
				t.Errorf("URL = %q, want %q", got, tc.wantURL)
			}
			if got := cfg.Mounts[0].Path; got != tc.wantPath {
				t.Errorf("Path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	const twoMounts = "  - cluster: example\n    target: "
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{"unknown key", mutate(t, "user:", "usr:"), []string{"usr"}},
		{"unknown top-level key", validYAML + "log: {}\n", []string{"log"}},
		{"unknown cache key", validYAML + "    cache:\n      size: 1GiB\n", []string{"size"}},
		{"syntax error", "clusters: [", []string{"parse"}},
		{"empty file", "", []string{"file is empty"}},
		{"only a comment", "# nothing\n", []string{"file is empty"}},
		{"two documents", validYAML + "---\n" + validYAML, []string{"more than one YAML document"}},
		{"broken second document", validYAML + "---\nclusters: [\n", []string{"parse"}},
		{"unparsable size", validYAML + "cache:\n  max_size: big\n", []string{"invalid size"}},
		{"unparsable duration", validYAML + "cache:\n  revalidate: 1h30m\n", []string{"invalid duration"}},
		{"no clusters", "mounts:\n  - cluster: example\n    target: /mnt/files\n", []string{"clusters: at least one cluster is required"}},
		{"no mounts", "clusters:\n  example:\n    url: https://files.example.com\n    user: a\n    password_file: p\n", []string{"mounts: at least one mount is required"}},
		{"bad cluster name", mutate(t, "  example:", "  Example:"), []string{"clusters.Example: name must match"}},
		{"empty url", mutate(t, "url: https://files.example.com/api", "url: ''"), []string{"clusters.example.url: must not be empty"}},
		{"unparsable url", mutate(t, "https://files.example.com/api", "http://[::1"), []string{"clusters.example.url"}},
		{"relative url", mutate(t, "https://files.example.com/api", "files.example.com/api"), []string{"clusters.example.url", "not an absolute http or https URL"}},
		{"ftp url", mutate(t, "https://", "ftp://"), []string{"clusters.example.url", "not an absolute http or https URL"}},
		{"url without host", mutate(t, "https://files.example.com/api", "https:///api"), []string{"clusters.example.url", "with a host"}},
		{"url with query", mutate(t, "/api\n", "/api?x=1\n"), []string{"clusters.example.url", "must not have a query or fragment"}},
		{"url with empty query", mutate(t, "/api\n", "/api?\n"), []string{"must not have a query or fragment"}},
		{"url with fragment", mutate(t, "/api\n", "/api#top\n"), []string{"must not have a query or fragment"}},
		{"url with empty fragment", mutate(t, "/api\n", "/api/#\n"), []string{"must not have a query or fragment"}},
		{"missing user", mutate(t, "    user: alice\n", ""), []string{"clusters.example.user: must not be empty"}},
		{"missing password file", mutate(t, "    password_file: secrets/example\n", ""), []string{"clusters.example.password_file: must not be empty"}},
		{"small block size", mutate(t, "user: alice\n", "user: alice\n    block_size: 32KiB\n"), []string{"clusters.example.block_size: 32KiB is not allowed", "64KiB to 1GiB"}},
		{"large block size", mutate(t, "user: alice\n", "user: alice\n    block_size: 2GiB\n    read_ahead: 4GiB\n"), []string{"clusters.example.block_size: 2GiB is not allowed"}},
		{"unlimited block size", mutate(t, "user: alice\n", "user: alice\n    block_size: unlimited\n"), []string{"clusters.example.block_size: unlimited is not allowed"}},
		{"zero streams", mutate(t, "user: alice\n", "user: alice\n    streams_per_node: 0\n"), []string{"clusters.example.streams_per_node: 0 is not between 1 and 64"}},
		{"too many streams", mutate(t, "user: alice\n", "user: alice\n    streams_per_node: 65\n"), []string{"clusters.example.streams_per_node: 65 is not between 1 and 64"}},
		{"read ahead below a block", mutate(t, "user: alice\n", "user: alice\n    read_ahead: 1MiB\n"), []string{"clusters.example.read_ahead: 1MiB is not allowed", "(5MiB)"}},
		{"unlimited read ahead", mutate(t, "user: alice\n", "user: alice\n    read_ahead: unlimited\n"), []string{"clusters.example.read_ahead: unlimited is not allowed"}},
		{"unknown cluster", mutate(t, "cluster: example", "cluster: nope"), []string{`mounts[0].cluster: "nope" is not a cluster`}},
		{"missing cluster", mutate(t, "cluster: example\n", ""), []string{`mounts[0].cluster: "" is not a cluster`}},
		{"path without slash", mutate(t, "cluster: example\n", "cluster: example\n    path: media\n"), []string{`mounts[0].path: "media" must start with "/"`}},
		{"path with dot dot", mutate(t, "cluster: example\n", "cluster: example\n    path: /a/../b\n"), []string{`mounts[0].path: "/a/../b" must not contain ".."`}},
		{"path escaping the root", mutate(t, "cluster: example\n", "cluster: example\n    path: /..\n"), []string{`must not contain ".."`}},
		{"missing target", mutate(t, "    target: /mnt/files\n", ""), []string{"mounts[0].target: must not be empty"}},
		{"duplicate target", validYAML + twoMounts + "/mnt/files/\n", []string{"mounts[1].target: /mnt/files is already the target of another mount (mounts[0])"}},
		{"target inside another", validYAML + twoMounts + "/mnt/files/sub\n", []string{"mounts[1].target: /mnt/files/sub is inside the target /mnt/files of another mount (mounts[0])"}},
		{"target contains another", validYAML + twoMounts + "/mnt\n", []string{"mounts[1].target: /mnt contains the target /mnt/files of another mount (mounts[0])"}},
		{"unlimited min_free", validYAML + "cache:\n  min_free: unlimited\n", []string{"cache.min_free: unlimited is not allowed"}},
		{"unlimited min_file_size", validYAML + "cache:\n  min_file_size: unlimited\n", []string{"cache.min_file_size: unlimited is not allowed"}},
		{"zero max_age", validYAML + "cache:\n  max_age: 0\n", []string{"cache.max_age: 0 is not allowed", `"unlimited"`}},
		{"zero revalidate", validYAML + "cache:\n  revalidate: 0\n", []string{"cache.revalidate: 0 is not allowed"}},
		{"unlimited revalidate", validYAML + "cache:\n  revalidate: unlimited\n", []string{"cache.revalidate: unlimited is not allowed"}},
		{"unlimited listing_ttl", validYAML + "cache:\n  listing_ttl: unlimited\n", []string{"cache.listing_ttl: unlimited is not allowed"}},
		{"empty cache dir", validYAML + "cache:\n  dir: ''\n", []string{"cache.dir: must not be empty"}},
		{"max_size below a block", validYAML + "cache:\n  max_size: 4MiB\n", []string{"cache.max_size: 4MiB is smaller than one block of the largest cluster (5MiB)"}},
		{"cache dir is a target", validYAML + "cache:\n  dir: /mnt/files\n", []string{"cache.dir: /mnt/files must not be, lie inside or contain the target /mnt/files of mounts[0]"}},
		{"cache dir inside a target", validYAML + "cache:\n  dir: /mnt/files/cache\n", []string{"cache.dir: /mnt/files/cache"}},
		{"cache dir contains a target", validYAML + "cache:\n  dir: /mnt\n", []string{"cache.dir: /mnt must not be"}},
		{"root cache dir", validYAML + "cache:\n  dir: /\n", []string{"cache.dir: / must not be"}},
		{
			"mount cache problems",
			validYAML + "    cache:\n      dir: /mnt/files/cache\n      max_age: 0\n      max_size: 1MiB\n",
			[]string{"mounts[0].cache.dir: /mnt/files/cache", "mounts[0].cache.max_age", "mounts[0].cache.max_size"},
		},
		{
			"mount cache dir contains another mount",
			validYAML + twoMounts + "/srv/data\n    cache:\n      dir: /srv\n",
			[]string{"mounts[1].cache.dir: /srv must not be, lie inside or contain the target /srv/data of mounts[1]"},
		},
		{
			"every problem at once",
			mutate(t, "user: alice\n", "user: alice\n    streams_per_node: 0\n") + "cache:\n  revalidate: 0\n",
			[]string{"streams_per_node", "cache.revalidate"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("Load succeeded: %+v", cfg)
			}
			if cfg != nil {
				t.Errorf("Load returned a config with an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not contain %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestLoadReportsInheritedProblemOnce(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, validYAML+"cache:\n  max_age: 0\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if n := strings.Count(err.Error(), "max_age"); n != 1 {
		t.Errorf("max_age is reported %d times:\n%v", n, err)
	}
}

func TestLoadSiblingDirectoryIsNotInside(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, validYAML+"cache:\n  dir: /mnt/files2\n"))
	if err != nil {
		t.Errorf("a cache directory next to a target was refused: %v", err)
	}
}

func TestLoadRelativeConfigDirectory(t *testing.T) {
	t.Parallel()
	dir := writeConfig(t, mutate(t, "/mnt/files", "mnt")+"  - cluster: example\n    target: mnt\ncache:\n  dir: cache\n")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(rel)
	if err == nil {
		t.Fatal("Load succeeded with relative paths that stay relative")
	}
	for _, want := range []string{"mounts[0].target", "cache.dir", "is not an absolute path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
}

func TestLoadErrorNamesTheFile(t *testing.T) {
	t.Parallel()
	dir := writeConfig(t, validYAML+"cache:\n  max_age: 0\n")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, FileName)) {
		t.Errorf("Load error = %v, want it to name the file", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load of an empty directory: %v, want os.ErrNotExist", err)
	}
}

func TestConfigMount(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, validYAML+"  - cluster: example\n    path: /media\n    target: /srv/movies\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tests := []struct {
		target string
		want   string
		found  bool
	}{
		{"/mnt/files", "/", true},
		{"/mnt/files/", "/", true},
		{"/mnt/../srv//movies", "/media", true},
		{"/srv", "", false},
		{"/mnt/files2", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		m, ok := cfg.Mount(tc.target)
		if ok != tc.found || m.Path != tc.want {
			t.Errorf("Mount(%q) = %+v, %v, want path %q, %v", tc.target, m, ok, tc.want, tc.found)
		}
	}
}

func TestClusterNamesSorted(t *testing.T) {
	t.Parallel()
	cfg := &Config{Clusters: map[string]Cluster{"b": {}, "c": {}, "a": {}}}
	if got, want := cfg.clusterNames(), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("clusterNames = %v, want %v", got, want)
	}
}

func TestValidateCacheNegativeListingTTL(t *testing.T) {
	t.Parallel()
	cache := defaultCache()
	cache.ListingTTL = units.Duration(-time.Second)
	errs := validateCache("cache", cache, 0)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "cache.listing_ttl") {
		t.Errorf("validateCache = %v", errs)
	}
}

func TestContains(t *testing.T) {
	t.Parallel()
	tests := []struct {
		outer, inner string
		want         bool
	}{
		{"/mnt/files", "/mnt/files", true},
		{"/mnt/files", "/mnt/files/a/b", true},
		{"/mnt/files", "/mnt/files2", false},
		{"/mnt/files", "/mnt", false},
		{"/mnt/files", "/other", false},
		{"/", "/", true},
		{"/", "/mnt", true},
	}
	for _, tc := range tests {
		if got := contains(tc.outer, tc.inner); got != tc.want {
			t.Errorf("contains(%q, %q) = %v, want %v", tc.outer, tc.inner, got, tc.want)
		}
	}
}
