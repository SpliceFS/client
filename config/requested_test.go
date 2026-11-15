package config

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// testConfig returns a fresh configuration of one cluster and one file mount, with the
// cache directory set by a file. Every call returns values that share nothing.
func testConfig() *Config {
	return &Config{
		Cache: expectedCache(),
		Clusters: map[string]Cluster{"example": {
			URL: "https://files.example.com/api", User: "alice", PasswordFile: "/etc/p",
			BlockSize: defaultBlockSize, StreamsPerNode: defaultStreams,
			ReadAhead: defaultReadAhead,
		}},
		Mounts: []Mount{{
			Name: "files", Cluster: "example", Path: "/", Target: "/mnt/files",
			Offline: true, Cache: expectedCache(),
		}},
		Sources: map[string]string{"cache.dir": "conf.d/a.yaml"},
	}
}

func requestedMount(target string, cache Cache) Mount {
	return Mount{
		Name: RequestedName(target), Cluster: "example", Path: "/", Target: target,
		Offline: true, Cache: cache,
	}
}

func TestWithMountsAdds(t *testing.T) {
	t.Parallel()
	base := testConfig()
	tests := []struct {
		name  string
		extra []Requested
		want  []Mount
	}{
		{"none", nil, nil},
		{
			"valid add",
			[]Requested{{
				Cluster: "example", Path: "/media", Target: "/mnt/media", AllowOther: true,
			}},
			[]Mount{{
				Name: RequestedName("/mnt/media"), Cluster: "example", Path: "/media",
				Target: "/mnt/media", AllowOther: true, Offline: true, Cache: base.Cache,
			}},
		},
		{
			"defaults applied",
			[]Requested{{Cluster: "example", Target: "/mnt/media"}},
			[]Mount{requestedMount("/mnt/media", base.Cache)},
		},
		{
			"offline override",
			[]Requested{{Cluster: "example", Target: "/mnt/media", Offline: new(false)}},
			[]Mount{{
				Name: RequestedName("/mnt/media"), Cluster: "example", Path: "/",
				Target: "/mnt/media", Cache: base.Cache,
			}},
		},
		{
			"offline stated true",
			[]Requested{{Cluster: "example", Target: "/mnt/media", Offline: new(true)}},
			[]Mount{requestedMount("/mnt/media", base.Cache)},
		},
		{
			"path and target cleaned",
			[]Requested{{Cluster: "example", Path: "/a//b/", Target: "/mnt/x/../media/"}},
			[]Mount{{
				Name: RequestedName("/mnt/media"), Cluster: "example", Path: "/a/b",
				Target: "/mnt/media", Offline: true, Cache: base.Cache,
			}},
		},
		{
			"two mounts keep their order",
			[]Requested{
				{Cluster: "example", Target: "/mnt/b"},
				{Cluster: "example", Target: "/mnt/a"},
			},
			[]Mount{requestedMount("/mnt/b", base.Cache), requestedMount("/mnt/a", base.Cache)},
		},
		{
			"sibling with the same prefix",
			[]Requested{{Cluster: "example", Target: "/mnt/files2"}},
			[]Mount{requestedMount("/mnt/files2", base.Cache)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			got, err := WithMounts(cfg, tt.extra...)
			if err != nil {
				t.Fatal(err)
			}
			want := testConfig()
			want.Mounts = append(want.Mounts, tt.want...)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(cfg, testConfig()) {
				t.Errorf("cfg was changed: %+v", cfg)
			}
		})
	}
}

func TestWithMountsRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		extra   []Requested
		want    []string
		notWant []string
	}{
		{
			"unknown cluster",
			[]Requested{{Cluster: "nope", Target: "/mnt/media"}},
			[]string{`"nope" is not a cluster`, "(requested mount /mnt/media)"},
			[]string{"(file "},
		},
		{
			"empty cluster",
			[]Requested{{Target: "/mnt/media"}},
			[]string{`"" is not a cluster`, "(requested mount /mnt/media)"},
			nil,
		},
		{
			"relative target",
			[]Requested{{Cluster: "example", Target: "mnt/media"}},
			[]string{`"mnt/media" is not an absolute path`, "(requested mount mnt/media)"},
			nil,
		},
		{
			"empty target",
			[]Requested{{Cluster: "example"}},
			[]string{"target: must not be empty", "(requested mount )"},
			nil,
		},
		{
			"relative path",
			[]Requested{{Cluster: "example", Path: "media", Target: "/mnt/media"}},
			[]string{`"media" must start with "/"`, "(requested mount /mnt/media)"},
			nil,
		},
		{
			"path with parent segment",
			[]Requested{{Cluster: "example", Path: "/a/../b", Target: "/mnt/media"}},
			[]string{`must not contain ".."`, "(requested mount /mnt/media)"},
			nil,
		},
		{
			"target equal to a file mount",
			[]Requested{{Cluster: "example", Target: "/mnt/files/"}},
			[]string{
				"/mnt/files is already the target of another mount (mounts.files)",
				"(requested mount /mnt/files)",
			},
			[]string{"(file mounts.d"},
		},
		{
			"target inside a file mount",
			[]Requested{{Cluster: "example", Target: "/mnt/files/sub"}},
			[]string{
				"/mnt/files/sub is inside the target /mnt/files of another mount",
				"(requested mount /mnt/files/sub)",
			},
			nil,
		},
		{
			"target containing a file mount",
			[]Requested{{Cluster: "example", Target: "/mnt"}},
			[]string{
				"/mnt contains the target /mnt/files of another mount",
				"(requested mount /mnt)",
			},
			nil,
		},
		{
			"target inside the cache folder",
			[]Requested{{Cluster: "example", Target: "/var/cache/test/media"}},
			[]string{
				"cache.dir: /var/cache/test must not be, lie inside or contain the target",
				"(requested mount /var/cache/test/media)",
			},
			[]string{"conf.d/a.yaml"},
		},
		{
			"target equal to the cache folder",
			[]Requested{{Cluster: "example", Target: "/var/cache/test"}},
			[]string{"cache.dir: ", "(requested mount /var/cache/test)"},
			[]string{"conf.d/a.yaml"},
		},
		{
			"target containing the cache folder",
			[]Requested{{Cluster: "example", Target: "/var"}},
			[]string{"cache.dir: ", "(requested mount /var)"},
			nil,
		},
		{
			"same target twice",
			[]Requested{
				{Cluster: "example", Target: "/mnt/media"},
				{Cluster: "example", Target: "/mnt/media"},
			},
			[]string{
				"/mnt/media is already the target of another mount",
				"(requested mount /mnt/media)",
			},
			nil,
		},
		{
			"every problem is listed",
			[]Requested{
				{Cluster: "nope", Target: "/mnt/a"},
				{Cluster: "example", Target: "rel"},
				{Cluster: "example", Target: "/mnt/files"},
			},
			[]string{
				`"nope" is not a cluster`, "(requested mount /mnt/a)",
				`"rel" is not an absolute path`, "(requested mount rel)",
				"already the target of another mount", "(requested mount /mnt/files)",
			},
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			got, err := WithMounts(cfg, tt.extra...)
			if err == nil {
				t.Fatalf("got %+v, want an error", got)
			}
			if got != nil {
				t.Errorf("got a configuration and the error %v", err)
			}
			checkErrorContains(t, err, tt.want)
			checkErrorOmits(t, err, tt.notWant)
			if !reflect.DeepEqual(cfg, testConfig()) {
				t.Errorf("cfg was changed: %+v", cfg)
			}
		})
	}
}

func TestWithMountsKeepsFileProblemsNamingTheirFile(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Mounts[0].Cluster = "gone"
	_, err := WithMounts(cfg, Requested{Cluster: "example", Target: "/mnt/media"})
	if err == nil || !strings.Contains(err.Error(), "(file mounts.d/files.yaml)") {
		t.Fatalf("got %v, want a problem that names the file of the mount", err)
	}
}

func TestWithMountsSharesNothing(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	// Spare capacity would let an append write into the slice of cfg.
	cfg.Mounts = append(make([]Mount, 0, 8), cfg.Mounts...)
	got, err := WithMounts(cfg, Requested{Cluster: "example", Target: "/mnt/media"})
	if err != nil {
		t.Fatal(err)
	}
	got.Mounts[0].Name = "changed"
	got.Mounts = append(got.Mounts[:1], Mount{Name: "other"})
	got.Clusters["example"] = Cluster{}
	got.Clusters["new"] = Cluster{}
	got.Sources["cache.dir"] = "changed"
	got.Sources["new"] = "changed"
	want := testConfig()
	cfg.Mounts = cfg.Mounts[:1:1]
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("cfg was changed: %+v", cfg)
	}
	if again, err := WithMounts(cfg); err != nil || !reflect.DeepEqual(again, want) {
		t.Errorf("got %+v, %v, want a copy of cfg", again, err)
	}
}

func TestWithMountsKeepsNilMaps(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.Sources = nil
	got, err := WithMounts(cfg)
	if err != nil || !reflect.DeepEqual(got, cfg) {
		t.Errorf("got %+v, %v, want an equal copy", got, err)
	}
}

func TestRequestedName(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^requested-[0-9a-f]{12}$`)
	if got := RequestedName("/mnt/a"); got != "requested-f72353b3dcd8" {
		t.Errorf("RequestedName(/mnt/a) = %q", got)
	}
	seen := map[string]string{}
	for _, target := range []string{"/mnt/a", "/mnt/b", "/mnt/a/b", "/", "mnt/a", ""} {
		name := RequestedName(target)
		if !shape.MatchString(name) || !namePattern.MatchString(name) {
			t.Errorf("RequestedName(%q) = %q has the wrong form", target, name)
		}
		if name != RequestedName(target) {
			t.Errorf("RequestedName(%q) is not stable", target)
		}
		if other, ok := seen[name]; ok {
			t.Errorf("%q and %q both give %q", other, target, name)
		}
		seen[name] = target
	}
	for _, same := range []string{"/mnt/a/", "/mnt//a", "/mnt/b/../a"} {
		if RequestedName(same) != RequestedName("/mnt/a") {
			t.Errorf("RequestedName(%q) differs from the cleaned target", same)
		}
	}
}
