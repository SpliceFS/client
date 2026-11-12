// Package config loads and validates the configuration of the SpliceFS client: the
// directories conf.d, clusters.d and mounts.d of the configuration directory.
package config

import (
	"maps"
	"path/filepath"
	"slices"

	"github.com/SpliceFS/common/units"
)

// Config is the client configuration.
type Config struct {
	// Cache holds the cache settings that mounts inherit.
	Cache Cache `yaml:"cache"`
	// Clusters maps a cluster name to its settings.
	Clusters map[string]Cluster `yaml:"clusters"`
	// Mounts lists the directories to mount, ordered by name.
	Mounts []Mount `yaml:"mounts"`
	// Sources maps the key path of every value that a file set, such as cache.max_size or
	// clusters.east.url, to that file relative to the configuration directory. Values left
	// at their defaults are not listed.
	Sources map[string]string `yaml:"-"`
}

// Cache says where and how file contents are cached and when they are evicted.
type Cache struct {
	// Dir is the cache directory. It must not be a mount target, lie inside one
	// or contain one.
	Dir string `yaml:"dir"`
	// MaxSize is the total size beyond which the least recently read files are
	// evicted.
	MaxSize units.Size `yaml:"max_size"`
	// MaxAge evicts files that have not been read for this long.
	MaxAge units.Duration `yaml:"max_age"`
	// MinFree is the free space always left on the cache disk.
	MinFree units.Size `yaml:"min_free"`
	// MinFileSize is the size below which files are never cached.
	MinFileSize units.Size `yaml:"min_file_size"`
	// Revalidate is how long a cached file is trusted before it is checked
	// against the nodes again.
	Revalidate units.Duration `yaml:"revalidate"`
	// ListingTTL is how long a directory listing is reused. Zero disables reuse.
	ListingTTL units.Duration `yaml:"listing_ttl"`
}

// Cluster is one cluster the client can mount from.
type Cluster struct {
	// URL is the address of any node, or of a name that resolves to several. It
	// has no trailing slash.
	URL string `yaml:"url"`
	// User is the login name.
	User string `yaml:"user"`
	// PasswordFile is the file that holds the password.
	PasswordFile string `yaml:"password_file"`
	// BlockSize is the size of the byte ranges that are requested and cached.
	BlockSize units.Size `yaml:"block_size"`
	// StreamsPerNode is the number of simultaneous requests to one node.
	StreamsPerNode int `yaml:"streams_per_node"`
	// ReadAhead is how far ahead of the reader blocks are requested.
	ReadAhead units.Size `yaml:"read_ahead"`
}

// Mount is one cluster directory mounted at a local directory.
type Mount struct {
	// Name is the name of the file the mount is read from, without .yaml.
	Name string `yaml:"-"`
	// Cluster names an entry of Config.Clusters.
	Cluster string `yaml:"cluster"`
	// Path is the directory of the cluster that is mounted, cleaned and starting
	// with a slash.
	Path string `yaml:"path"`
	// Target is the local directory to mount at, absolute and cleaned.
	Target string `yaml:"target"`
	// AllowOther lets users other than the one running the client read the mount.
	AllowOther bool `yaml:"allow_other"`
	// Offline keeps serving completely cached files when no node can be reached.
	Offline bool `yaml:"offline"`
	// Cache is the cache configuration of this mount: the top-level one with the
	// keys of the mount's own cache section laid over it.
	Cache Cache `yaml:"cache"`
}

// Mount returns the mount whose target is target, compared after cleaning.
func (c *Config) Mount(target string) (Mount, bool) {
	target = filepath.Clean(target)
	for _, m := range c.Mounts {
		if m.Target == target {
			return m, true
		}
	}
	return Mount{}, false
}

// clusterNames returns the names of the clusters in sorted order.
func (c *Config) clusterNames() []string {
	return slices.Sorted(maps.Keys(c.Clusters))
}
