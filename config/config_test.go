package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/units"
)

const (
	validCluster = "url: https://files.example.com/api\nuser: alice\npassword_file: secrets/example\n"
	validMount   = "cluster: example\ntarget: /mnt/files\n"
)

var errNoHome = errors.New("no home")

func testCacheDir() (string, error)    { return "/var/cache/test", nil }
func failingCacheDir() (string, error) { return "", errNoHome }

// files maps a path relative to the configuration directory to its content.
type files map[string]string

// writeDir creates a configuration directory holding the given files.
func writeDir(t *testing.T, f files) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range f {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, f files) (*Config, error) {
	t.Helper()
	return loadDir(writeDir(t, f), testCacheDir)
}

// valid returns the files of a minimal configuration: cluster example and mount files.
func valid() files {
	return files{"clusters.d/example.yaml": validCluster, "mounts.d/files.yaml": validMount}
}

// with returns valid() with the given files added or replaced.
func with(extra files) files {
	f := valid()
	for k, v := range extra {
		f[k] = v
	}
	return f
}

// mutate replaces old with replacement in the content of file of valid() and fails if old
// is not there.
func mutate(t *testing.T, file, old, replacement string) files {
	t.Helper()
	f := valid()
	if !strings.Contains(f[file], old) {
		t.Fatalf("%s does not contain %q", file, old)
	}
	f[file] = strings.Replace(f[file], old, replacement, 1)
	return f
}

func expectedCache() Cache {
	return Cache{
		Dir:        "/var/cache/test",
		MaxSize:    10 << 30,
		MaxAge:     units.UnlimitedDuration,
		MinFree:    1 << 30,
		Revalidate: units.Duration(time.Hour),
		ListingTTL: units.Duration(30 * time.Second),
	}
}

func TestLoadDirDefaults(t *testing.T) {
	t.Parallel()
	dir := writeDir(t, valid())
	cfg, err := loadDir(dir, testCacheDir)
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Cache: expectedCache(),
		Clusters: map[string]Cluster{"example": {
			URL: "https://files.example.com/api", User: "alice",
			PasswordFile: filepath.Join(dir, "secrets/example"),
			BlockSize:    5 << 20, StreamsPerNode: 4, ReadAhead: 64 << 20,
		}},
		Mounts: []Mount{{
			Name: "files", Cluster: "example", Path: "/", Target: "/mnt/files", Offline: true,
			Cache: expectedCache(),
		}},
		Sources: map[string]string{
			"clusters.example.url":           "clusters.d/example.yaml",
			"clusters.example.user":          "clusters.d/example.yaml",
			"clusters.example.password_file": "clusters.d/example.yaml",
			"mounts.files.cluster":           "mounts.d/files.yaml",
			"mounts.files.target":            "mounts.d/files.yaml",
		},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("LoadDir =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestLoadDirEmptyAndMissing(t *testing.T) {
	t.Parallel()
	tests := map[string]files{
		"empty directory":   {},
		"unrelated file":    {"other/x.yaml": "a: 1"},
		"empty folders":     {"conf.d/.keep": "", "clusters.d/.keep": "", "mounts.d/.keep": ""},
		"empty file":        {"conf.d/a.yaml": ""},
		"comment only file": {"conf.d/a.yaml": "# nothing\n"},
		"null document":     {"conf.d/a.yaml": "null\n"},
		"null values":       {"conf.d/a.yaml": "cache:\n"},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := load(t, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Clusters) != 0 || len(cfg.Mounts) != 0 || cfg.Cache != expectedCache() ||
				len(cfg.Sources) != 0 {
				t.Errorf("got %+v", cfg)
			}
		})
	}
	// The path that does not exist at all.
	cfg, err := loadDir(filepath.Join(t.TempDir(), "missing"), testCacheDir)
	if err != nil || len(cfg.Mounts) != 0 {
		t.Errorf("missing directory: %+v, %v", cfg, err)
	}
}

func TestLoadDirFull(t *testing.T) {
	t.Parallel()
	dir := writeDir(t, files{
		"conf.d/10-cache.yaml": "cache:\n  dir: cache\n  max_size: 200GiB\n  max_age: 30d\n" +
			"  min_free: 10GiB\n  min_file_size: 4KiB\n  revalidate: 2h\n  listing_ttl: 0\n",
		"clusters.d/example.yaml": "url: http://192.0.2.10:8480/api/\nuser: alice\n" +
			"password_file: /etc/secret\nblock_size: 1MiB\nstreams_per_node: 8\nread_ahead: 16MiB\n",
		"clusters.d/other.v2.yaml": "url: https://other.example.com\nuser: bob\n" +
			"password_file: ./other/../secret\n",
		"mounts.d/movies.yaml": "cluster: example\npath: /media//movies/\ntarget: /srv/movies/\n" +
			"allow_other: true\noffline: false\ncache:\n  dir: /data/movies\n" +
			"  max_size: unlimited\n  max_age: unlimited\n",
		"mounts.d/a-other.yaml": "cluster: other.v2\npath: /\ntarget: mnt/other\n",
	})
	cfg, err := loadDir(dir, testCacheDir)
	if err != nil {
		t.Fatal(err)
	}
	top := Cache{
		Dir: filepath.Join(dir, "cache"), MaxSize: 200 << 30,
		MaxAge: units.Duration(30 * 24 * time.Hour), MinFree: 10 << 30, MinFileSize: 4 << 10,
		Revalidate: units.Duration(2 * time.Hour), ListingTTL: 0,
	}
	override := top
	override.Dir = "/data/movies"
	override.MaxSize = units.UnlimitedSize
	override.MaxAge = units.UnlimitedDuration
	wantClusters := map[string]Cluster{
		"example": {
			URL: "http://192.0.2.10:8480/api", User: "alice", PasswordFile: "/etc/secret",
			BlockSize: 1 << 20, StreamsPerNode: 8, ReadAhead: 16 << 20,
		},
		"other.v2": {
			URL: "https://other.example.com", User: "bob",
			PasswordFile: filepath.Join(dir, "secret"),
			BlockSize:    5 << 20, StreamsPerNode: 4, ReadAhead: 64 << 20,
		},
	}
	wantMounts := []Mount{
		{Name: "a-other", Cluster: "other.v2", Path: "/", Target: filepath.Join(dir, "mnt/other"),
			Offline: true, Cache: top},
		{Name: "movies", Cluster: "example", Path: "/media/movies", Target: "/srv/movies",
			AllowOther: true, Cache: override},
	}
	if cfg.Cache != top || !reflect.DeepEqual(cfg.Clusters, wantClusters) ||
		!reflect.DeepEqual(cfg.Mounts, wantMounts) {
		t.Errorf("LoadDir =\n%+v\nwant cache %+v clusters %+v mounts %+v",
			cfg, top, wantClusters, wantMounts)
	}
	wantSources := map[string]string{
		"cache.dir": "conf.d/10-cache.yaml", "cache.listing_ttl": "conf.d/10-cache.yaml",
		"clusters.other.v2.url": "clusters.d/other.v2.yaml", "mounts.movies.cache.max_age": "mounts.d/movies.yaml",
		"mounts.movies.allow_other": "mounts.d/movies.yaml",
	}
	for k, v := range wantSources {
		if cfg.Sources[k] != v {
			t.Errorf("Sources[%q] = %q, want %q", k, cfg.Sources[k], v)
		}
	}
}

func TestLoadDirSettingsMerge(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, with(files{
		"conf.d/20-b.yaml":     "cache:\n  max_size: 2GiB\n  revalidate: 3h\n",
		"conf.d/10-a.yaml":     "cache:\n  max_size: 1GiB\n  min_free: 2GiB\n",
		"conf.d/05-first.yaml": "cache:\n  max_size: 3GiB\n  max_age: 1d\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := expectedCache()
	want.MaxSize, want.MinFree, want.MaxAge = 2<<30, 2<<30, units.Duration(24*time.Hour)
	want.Revalidate = units.Duration(3 * time.Hour)
	if cfg.Cache != want {
		t.Errorf("cache = %+v, want %+v", cfg.Cache, want)
	}
	wantSources := map[string]string{
		"cache.max_size": "conf.d/20-b.yaml", "cache.revalidate": "conf.d/20-b.yaml",
		"cache.min_free": "conf.d/10-a.yaml", "cache.max_age": "conf.d/05-first.yaml",
	}
	for k, v := range wantSources {
		if cfg.Sources[k] != v {
			t.Errorf("Sources[%q] = %q, want %q", k, cfg.Sources[k], v)
		}
	}
}

func TestLoadDirByteOrder(t *testing.T) {
	t.Parallel()
	// "B" sorts before "a" in byte order, so a.yaml wins.
	cfg, err := load(t, files{
		"conf.d/a.yaml": "cache: {max_size: 1GiB}\n", "conf.d/B.yaml": "cache: {max_size: 2GiB}\n",
	})
	if err != nil || cfg.Cache.MaxSize != 1<<30 || cfg.Sources["cache.max_size"] != "conf.d/a.yaml" {
		t.Fatalf("%+v, %v", cfg, err)
	}
}

func TestLoadDirIgnoredEntries(t *testing.T) {
	t.Parallel()
	f := with(files{
		"conf.d/.hidden.yaml":      "bad: [",
		"conf.d/a.yaml~":           "bad: [",
		"conf.d/a.yml":             "bad: [",
		"conf.d/x.yaml/in.yaml":    "bad: [",
		"clusters.d/.example.yaml": "bad: [",
		"clusters.d/c.txt":         "bad: [",
		"clusters.d/d.yaml/p.yaml": "bad: [",
		"mounts.d/.m.yaml":         "bad: [",
		"mounts.d/m.yaml~":         "bad: [",
		"mounts.d/dir.yaml/m.yaml": "bad: [",
	})
	cfg, err := load(t, f)
	if err != nil || len(cfg.Clusters) != 1 || len(cfg.Mounts) != 1 {
		t.Fatalf("%+v, %v", cfg, err)
	}
}

func TestLoadDirMountOrder(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, with(files{
		"mounts.d/b.yaml": "cluster: example\ntarget: /mnt/b\n",
		"mounts.d/a.yaml": "cluster: example\ntarget: /mnt/a\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range cfg.Mounts {
		names = append(names, m.Name)
	}
	if got := strings.Join(names, ","); got != "a,b,files" {
		t.Errorf("mount order = %s", got)
	}
}

func TestLoadDirExplicitZeroValuesKeepTheirMeaning(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, with(files{"mounts.d/files.yaml": validMount +
		"offline: false\ncache:\n  min_file_size: 0\n  listing_ttl: 0\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Mounts[0]; m.Offline || m.Cache.MinFileSize != 0 || m.Cache.ListingTTL != 0 {
		t.Errorf("mount = %+v, want offline false, min_file_size 0 and listing_ttl 0", m)
	}
}

func TestLoadDirMountCacheInheritsKeyByKey(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, with(files{
		"conf.d/a.yaml":       "cache:\n  max_size: 50GiB\n  min_free: 2GiB\n",
		"mounts.d/files.yaml": validMount + "cache:\n  max_size: 1TiB\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Mounts[0].Cache
	want := cfg.Cache
	want.MaxSize = 1 << 40
	if got != want || cfg.Cache.MaxSize != 50<<30 || got.MinFree != 2<<30 {
		t.Errorf("top-level cache = %+v, mount cache = %+v", cfg.Cache, got)
	}
}

func TestLoadDirNormalizes(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, file, old, replacement, wantURL, wantPath string }{
		{"url trailing slash", "clusters.d/example.yaml", "/api\n", "/api/\n", "https://files.example.com/api", "/"},
		{"url root slash", "clusters.d/example.yaml", "/api\n", "/\n", "https://files.example.com", "/"},
		{"url without path", "clusters.d/example.yaml", "/api\n", "\n", "https://files.example.com", "/"},
		{"path cleaned", "mounts.d/files.yaml", "cluster: example\n", "cluster: example\npath: /a//b/./c/\n", "https://files.example.com/api", "/a/b/c"},
		{"path with dots in names", "mounts.d/files.yaml", "cluster: example\n", "cluster: example\npath: /a..b/..c\n", "https://files.example.com/api", "/a..b/..c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := load(t, mutate(t, tc.file, tc.old, tc.replacement))
			if err != nil {
				t.Fatal(err)
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

func TestLoadDirDecodeErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		f    files
		want []string
	}{
		"unknown cluster key": {mutate(t, "clusters.d/example.yaml", "user:", "usr:"),
			[]string{"clusters.d/example.yaml", "usr"}},
		"cluster name key": {with(files{"clusters.d/example.yaml": validCluster + "name: x\n"}),
			[]string{"clusters.d/example.yaml", "name"}},
		"mount name key": {with(files{"mounts.d/files.yaml": validMount + "name: x\n"}),
			[]string{"mounts.d/files.yaml", "name"}},
		"unknown setting":   {files{"conf.d/a.yaml": "log: {}\n"}, []string{"conf.d/a.yaml", "log"}},
		"unknown cache key": {files{"conf.d/a.yaml": "cache: {size: 1GiB}\n"}, []string{"a.yaml", "size"}},
		"unknown mount cache key": {with(files{"mounts.d/files.yaml": validMount + "cache: {size: 1}\n"}),
			[]string{"files.yaml", "size"}},
		"two documents":       {files{"conf.d/a.yaml": "a: 1\n---\nb: 2\n"}, []string{"a.yaml", "more than one YAML document"}},
		"broken second doc":   {files{"conf.d/a.yaml": "cache: {}\n---\n[\n"}, []string{"a.yaml"}},
		"syntax error":        {files{"clusters.d/c.yaml": "url: ["}, []string{"clusters.d/c.yaml"}},
		"unparsable size":     {files{"conf.d/a.yaml": "cache:\n  max_size: big\n"}, []string{"a.yaml", "invalid size"}},
		"unparsable duration": {files{"conf.d/a.yaml": "cache:\n  revalidate: 1h30m\n"}, []string{"invalid duration"}},
		"problems together": {files{"conf.d/a.yaml": "x: 1\n", "clusters.d/c.yaml": "y: 1\n", "mounts.d/m.yaml": "z: 1\n"},
			[]string{"a.yaml", "c.yaml", "m.yaml"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := load(t, tt.f)
			if err == nil || cfg != nil {
				t.Fatalf("got %+v, %v", cfg, err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestLoadDirValidation(t *testing.T) {
	t.Parallel()
	const (
		cl = "clusters.d/example.yaml"
		mo = "mounts.d/files.yaml"
		c2 = "mounts.d/other.yaml"
	)
	conf := func(s string) files { return with(files{"conf.d/a.yaml": s}) }
	tests := []struct {
		name string
		f    files
		want []string
	}{
		{"bad cluster name", with(files{"clusters.d/Bad Name.yaml": validCluster}),
			[]string{"clusters.Bad Name: name must match", "(file clusters.d/Bad Name.yaml)"}},
		{"bad mount name", with(files{"mounts.d/Bad Name.yaml": "cluster: example\ntarget: /mnt/b\n"}),
			[]string{"mounts.Bad Name: name must match", "(file mounts.d/Bad Name.yaml)"}},
		{"empty url", mutate(t, cl, "url: https://files.example.com/api", "url: ''"),
			[]string{"clusters.example.url: must not be empty", "(file clusters.d/example.yaml)"}},
		{"unparsable url", mutate(t, cl, "https://files.example.com/api", "http://[::1"), []string{"clusters.example.url"}},
		{"relative url", mutate(t, cl, "https://files.example.com/api", "files.example.com/api"), []string{"clusters.example.url", "not an absolute http or https URL"}},
		{"ftp url", mutate(t, cl, "https://", "ftp://"), []string{"not an absolute http or https URL"}},
		{"url without host", mutate(t, cl, "https://files.example.com/api", "https:///api"), []string{"with a host"}},
		{"url with query", mutate(t, cl, "/api\n", "/api?x=1\n"), []string{"must not have a query or fragment"}},
		{"url with empty query", mutate(t, cl, "/api\n", "/api?\n"), []string{"must not have a query or fragment"}},
		{"url with fragment", mutate(t, cl, "/api\n", "/api#top\n"), []string{"must not have a query or fragment"}},
		{"url with empty fragment", mutate(t, cl, "/api\n", "/api/#\n"), []string{"must not have a query or fragment"}},
		{"missing user", mutate(t, cl, "user: alice\n", ""), []string{"clusters.example.user: must not be empty"}},
		{"missing password file", mutate(t, cl, "password_file: secrets/example\n", ""), []string{"clusters.example.password_file: must not be empty"}},
		{"small block size", mutate(t, cl, "user: alice\n", "user: alice\nblock_size: 32KiB\n"), []string{"clusters.example.block_size: 32KiB is not allowed", "64KiB to 1GiB"}},
		{"large block size", mutate(t, cl, "user: alice\n", "user: alice\nblock_size: 2GiB\nread_ahead: 4GiB\n"), []string{"clusters.example.block_size: 2GiB is not allowed"}},
		{"unlimited block size", mutate(t, cl, "user: alice\n", "user: alice\nblock_size: unlimited\n"), []string{"block_size: unlimited is not allowed"}},
		{"zero streams", mutate(t, cl, "user: alice\n", "user: alice\nstreams_per_node: 0\n"), []string{"streams_per_node: 0 is not between 1 and 64"}},
		{"too many streams", mutate(t, cl, "user: alice\n", "user: alice\nstreams_per_node: 65\n"), []string{"streams_per_node: 65 is not between 1 and 64"}},
		{"read ahead below a block", mutate(t, cl, "user: alice\n", "user: alice\nread_ahead: 1MiB\n"), []string{"read_ahead: 1MiB is not allowed", "(5MiB)"}},
		{"unlimited read ahead", mutate(t, cl, "user: alice\n", "user: alice\nread_ahead: unlimited\n"), []string{"read_ahead: unlimited is not allowed"}},
		{"unknown cluster", mutate(t, mo, "cluster: example", "cluster: nope"), []string{`mounts.files.cluster: "nope" is not a cluster`, "(file mounts.d/files.yaml)"}},
		{"missing cluster", mutate(t, mo, "cluster: example\n", ""), []string{`mounts.files.cluster: "" is not a cluster`}},
		{"path without slash", mutate(t, mo, "cluster: example\n", "cluster: example\npath: media\n"), []string{`mounts.files.path: "media" must start with "/"`}},
		{"path with dot dot", mutate(t, mo, "cluster: example\n", "cluster: example\npath: /a/../b\n"), []string{`mounts.files.path: "/a/../b" must not contain ".."`}},
		{"path escaping the root", mutate(t, mo, "cluster: example\n", "cluster: example\npath: /..\n"), []string{`must not contain ".."`}},
		{"missing target", mutate(t, mo, "target: /mnt/files\n", ""), []string{"mounts.files.target: must not be empty"}},
		{"duplicate target", with(files{c2: "cluster: example\ntarget: /mnt/files/\n"}),
			[]string{"mounts.other.target: /mnt/files is already the target of another mount (mounts.files)", "(file mounts.d/other.yaml)"}},
		{"target inside another", with(files{c2: "cluster: example\ntarget: /mnt/files/sub\n"}),
			[]string{"mounts.other.target: /mnt/files/sub is inside the target /mnt/files of another mount (mounts.files)"}},
		{"target contains another", with(files{"mounts.d/a.yaml": "cluster: example\ntarget: /mnt\n"}),
			[]string{"mounts.files.target: /mnt/files is inside the target /mnt of another mount (mounts.a)"}},
		{"target contains a later one", with(files{c2: "cluster: example\ntarget: /mnt\n"}),
			[]string{"mounts.other.target: /mnt contains the target /mnt/files of another mount (mounts.files)"}},
		{"unlimited min_free", conf("cache: {min_free: unlimited}\n"), []string{"cache.min_free: unlimited is not allowed", "(file conf.d/a.yaml)"}},
		{"unlimited min_file_size", conf("cache: {min_file_size: unlimited}\n"), []string{"cache.min_file_size: unlimited is not allowed"}},
		{"zero max_age", conf("cache: {max_age: 0}\n"), []string{"cache.max_age: 0 is not allowed", `"unlimited"`}},
		{"zero revalidate", conf("cache: {revalidate: 0}\n"), []string{"cache.revalidate: 0 is not allowed"}},
		{"unlimited revalidate", conf("cache: {revalidate: unlimited}\n"), []string{"cache.revalidate: unlimited is not allowed"}},
		{"unlimited listing_ttl", conf("cache: {listing_ttl: unlimited}\n"), []string{"cache.listing_ttl: unlimited is not allowed"}},
		{"empty cache dir", conf("cache: {dir: ''}\n"), []string{"cache.dir: must not be empty"}},
		{"max_size below a block", conf("cache: {max_size: 4MiB}\n"), []string{"cache.max_size: 4MiB is smaller than one block of the largest cluster (5MiB)", "(file conf.d/a.yaml)"}},
		{"cache dir is a target", conf("cache: {dir: /mnt/files}\n"), []string{"cache.dir: /mnt/files must not be, lie inside or contain the target /mnt/files of mounts.files", "(file conf.d/a.yaml)"}},
		{"cache dir inside a target", conf("cache: {dir: /mnt/files/cache}\n"), []string{"cache.dir: /mnt/files/cache"}},
		{"cache dir contains a target", conf("cache: {dir: /mnt}\n"), []string{"cache.dir: /mnt must not be"}},
		{"root cache dir", conf("cache: {dir: /}\n"), []string{"cache.dir: / must not be"}},
		{"mount cache problems",
			with(files{mo: validMount + "cache:\n  dir: /mnt/files/cache\n  max_age: 0\n  max_size: 1MiB\n"}),
			[]string{"mounts.files.cache.dir: /mnt/files/cache", "mounts.files.cache.max_age", "mounts.files.cache.max_size", "(file mounts.d/files.yaml)"}},
		{"mount cache dir contains another mount",
			with(files{c2: "cluster: example\ntarget: /srv/data\ncache:\n  dir: /srv\n"}),
			[]string{"mounts.other.cache.dir: /srv must not be, lie inside or contain the target /srv/data of mounts.other"}},
		{"every problem at once",
			with(files{cl: validCluster + "streams_per_node: 0\n", "conf.d/a.yaml": "cache: {revalidate: 0}\n"}),
			[]string{"streams_per_node", "cache.revalidate", "(file clusters.d/example.yaml)", "(file conf.d/a.yaml)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := load(t, tc.f)
			if err == nil || cfg != nil {
				t.Fatalf("got %+v, %v", cfg, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not contain %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestLoadDirReportsInheritedProblemOnce(t *testing.T) {
	t.Parallel()
	_, err := load(t, with(files{"conf.d/a.yaml": "cache: {max_age: 0}\n"}))
	if err == nil {
		t.Fatal("LoadDir succeeded")
	}
	if n := strings.Count(err.Error(), "max_age"); n != 1 {
		t.Errorf("max_age is reported %d times:\n%v", n, err)
	}
}

func TestLoadDirDefaultProblemHasNoFile(t *testing.T) {
	t.Parallel()
	// The default cache directory contains this target, and no file set the directory.
	_, err := load(t, with(files{"mounts.d/files.yaml": "cluster: example\ntarget: /var/cache/test/x\n"}))
	if err == nil || !strings.Contains(err.Error(), "cache.dir") ||
		strings.Contains(err.Error(), "conf.d") {
		t.Errorf("error = %v, want a cache.dir problem that names no conf.d file", err)
	}
}

func TestLoadDirSiblingDirectoryIsNotInside(t *testing.T) {
	t.Parallel()
	if _, err := load(t, with(files{"conf.d/a.yaml": "cache: {dir: /mnt/files2}\n"})); err != nil {
		t.Errorf("a cache directory next to a target was refused: %v", err)
	}
}

func TestLoadDirRelativeConfigDirectory(t *testing.T) {
	t.Parallel()
	dir := writeDir(t, files{
		"clusters.d/example.yaml": validCluster,
		"mounts.d/files.yaml":     "cluster: example\ntarget: mnt\n",
		"mounts.d/more.yaml":      "cluster: example\ntarget: mnt\n",
		"conf.d/a.yaml":           "cache: {dir: cache}\n",
	})
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadDir(rel)
	if err == nil {
		t.Fatal("LoadDir succeeded with relative paths that stay relative")
	}
	for _, want := range []string{"mounts.files.target", "cache.dir", "is not an absolute path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
}

func TestLoadDirDefaultCacheDirUnavailable(t *testing.T) {
	t.Parallel()
	_, err := loadDir(writeDir(t, valid()), failingCacheDir)
	if !errors.Is(err, errNoHome) || !strings.Contains(err.Error(), "cache.dir") {
		t.Errorf("error = %v", err)
	}
	// A configured directory never asks for the default.
	f := with(files{"conf.d/a.yaml": "cache: {dir: /var/c}\n"})
	if _, err := loadDir(writeDir(t, f), failingCacheDir); err != nil {
		t.Errorf("configured cache dir: %v", err)
	}
	// The exported function uses the real default.
	if _, err := LoadDir(writeDir(t, files{})); err != nil && !errors.Is(err, errNoHome) &&
		!strings.Contains(err.Error(), "home directory") {
		t.Errorf("LoadDir: %v", err)
	}
}

func TestLoadDirUnreadable(t *testing.T) {
	t.Parallel()
	// A link to nothing is listed like a file and cannot be read. Taking away read
	// permission would not stop root, which runs the tests on Linux.
	for _, rel := range []string{"conf.d/a.yaml", "clusters.d/c.yaml", "mounts.d/m.yaml"} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			dir := writeDir(t, files{filepath.Join(filepath.Dir(rel), "other.txt"): ""})
			file := filepath.Join(dir, rel)
			if err := os.Symlink(filepath.Join(dir, "nowhere"), file); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDir(dir, testCacheDir); err == nil || !strings.Contains(err.Error(), file) {
				t.Errorf("error %v does not name %s", err, file)
			}
		})
	}
	// A file where a folder belongs cannot be listed, by any user.
	for _, sub := range []string{"conf.d", "clusters.d", "mounts.d"} {
		t.Run("dir "+sub, func(t *testing.T) {
			t.Parallel()
			dir := writeDir(t, files{sub: "a file where a folder belongs"})
			if _, err := loadDir(dir, testCacheDir); err == nil || !strings.Contains(err.Error(), sub) {
				t.Errorf("error %v does not name %s", err, sub)
			}
		})
	}
}

func TestAttribute(t *testing.T) {
	t.Parallel()
	sources := map[string]string{"cache.max_size": "conf.d/a.yaml"}
	tests := []struct{ msg, want string }{
		{"cache.max_size: bad", "cache.max_size: bad (file conf.d/a.yaml)"},
		{"cache.max_age: bad", "cache.max_age: bad"},
		{"no key here", "no key here"},
	}
	for _, tt := range tests {
		if got := attribute(errors.New(tt.msg), sources).Error(); got != tt.want {
			t.Errorf("attribute(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestConfigMount(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, with(files{"mounts.d/movies.yaml": "cluster: example\npath: /media\ntarget: /srv/movies\n"}))
	if err != nil {
		t.Fatal(err)
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
	cache := expectedCache()
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
