package config

import (
	"path/filepath"
	"time"

	"github.com/SpliceFS/client/internal/units"
)

// Sizes used by the defaults.
const (
	mib units.Size = 1 << 20
	gib units.Size = 1 << 30
)

// Defaults applied to keys that are absent.
const (
	defaultCacheDir     = "/var/cache/splicefs"
	defaultCacheMaxSize = 10 * gib
	defaultCacheMinFree = gib
	defaultRevalidate   = units.Duration(time.Hour)
	defaultListingTTL   = units.Duration(30 * time.Second)
	defaultBlockSize    = 5 * mib
	defaultStreams      = 4
	defaultReadAhead    = 64 * mib
	defaultMountPath    = "/"
	defaultOffline      = true
)

// rawConfig is the shape of the file. Every key whose absence must be told
// apart from an explicit zero or false is a pointer: a zero max_age is an error
// where an absent one means unlimited, and a mount's cache section must only
// override the keys it names.
type rawConfig struct {
	Cache    rawCache              `yaml:"cache"`
	Clusters map[string]rawCluster `yaml:"clusters"`
	Mounts   []rawMount            `yaml:"mounts"`
}

type rawCache struct {
	Dir         *string         `yaml:"dir"`
	MaxSize     *units.Size     `yaml:"max_size"`
	MaxAge      *units.Duration `yaml:"max_age"`
	MinFree     *units.Size     `yaml:"min_free"`
	MinFileSize *units.Size     `yaml:"min_file_size"`
	Revalidate  *units.Duration `yaml:"revalidate"`
	ListingTTL  *units.Duration `yaml:"listing_ttl"`
}

type rawCluster struct {
	URL            string      `yaml:"url"`
	User           string      `yaml:"user"`
	PasswordFile   string      `yaml:"password_file"`
	BlockSize      *units.Size `yaml:"block_size"`
	StreamsPerNode *int        `yaml:"streams_per_node"`
	ReadAhead      *units.Size `yaml:"read_ahead"`
}

type rawMount struct {
	Cluster    string   `yaml:"cluster"`
	Path       *string  `yaml:"path"`
	Target     string   `yaml:"target"`
	AllowOther bool     `yaml:"allow_other"`
	Offline    *bool    `yaml:"offline"`
	Cache      rawCache `yaml:"cache"`
}

// orDefault returns *p, or def when the key was absent.
func orDefault[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// build applies the defaults and resolves relative paths against dir.
func (r *rawConfig) build(dir string) *Config {
	cache := r.Cache.over(defaultCache(), dir)
	cfg := &Config{Cache: cache, Clusters: make(map[string]Cluster, len(r.Clusters))}
	for name, c := range r.Clusters {
		cfg.Clusters[name] = c.build(dir)
	}
	for _, m := range r.Mounts {
		cfg.Mounts = append(cfg.Mounts, m.build(cache, dir))
	}
	return cfg
}

func defaultCache() Cache {
	return Cache{
		Dir:        defaultCacheDir,
		MaxSize:    defaultCacheMaxSize,
		MaxAge:     units.UnlimitedDuration,
		MinFree:    defaultCacheMinFree,
		Revalidate: defaultRevalidate,
		ListingTTL: defaultListingTTL,
	}
}

// over returns base with the keys present in r laid over it.
func (r rawCache) over(base Cache, dir string) Cache {
	return Cache{
		Dir:         resolve(dir, orDefault(r.Dir, base.Dir)),
		MaxSize:     orDefault(r.MaxSize, base.MaxSize),
		MaxAge:      orDefault(r.MaxAge, base.MaxAge),
		MinFree:     orDefault(r.MinFree, base.MinFree),
		MinFileSize: orDefault(r.MinFileSize, base.MinFileSize),
		Revalidate:  orDefault(r.Revalidate, base.Revalidate),
		ListingTTL:  orDefault(r.ListingTTL, base.ListingTTL),
	}
}

func (r rawCluster) build(dir string) Cluster {
	return Cluster{
		URL:            r.URL,
		User:           r.User,
		PasswordFile:   resolve(dir, r.PasswordFile),
		BlockSize:      orDefault(r.BlockSize, defaultBlockSize),
		StreamsPerNode: orDefault(r.StreamsPerNode, defaultStreams),
		ReadAhead:      orDefault(r.ReadAhead, defaultReadAhead),
	}
}

func (r rawMount) build(cache Cache, dir string) Mount {
	return Mount{
		Cluster:    r.Cluster,
		Path:       orDefault(r.Path, defaultMountPath),
		Target:     resolve(dir, r.Target),
		AllowOther: r.AllowOther,
		Offline:    orDefault(r.Offline, defaultOffline),
		Cache:      r.Cache.over(cache, dir),
	}
}

// resolve makes p relative to dir unless it is absolute, and cleans it. An
// empty path stays empty so that validation can report it as missing.
func resolve(dir, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}
