package config

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/SpliceFS/common/units"
)

// Limits for values that have them.
const (
	minBlockSize      units.Size = 64 << 10
	maxBlockSize      units.Size = 1 << 30
	minStreamsPerNode            = 1
	maxStreamsPerNode            = 64
)

// namePattern is the form of a cluster or mount name, which is the rule for share names.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// validate returns every problem in cfg, each naming its key and the file the value came
// from. A cluster or mount is its file, so its problems always name that file; a problem in
// the top-level cache names the file that set the key, and none when the default is wrong.
// The mounts from index own on were asked for while the client runs and have no file: their
// problems name the mount by its target, and the top-level cache is not blamed for them.
func validate(cfg *Config, own int) []error {
	problems := slices.Concat(validateClusters(cfg), validateMounts(cfg, own))
	top := validateCache("cache", cfg.Cache, largestBlockSize(cfg.Clusters))
	top = append(top, validateCacheDir("cache.dir", cfg.Cache.Dir, cfg.Mounts[:own])...)
	for _, problem := range top {
		problems = append(problems, attribute(problem, cfg.Sources))
	}
	return problems
}

// attribute adds the name of the file that set the value an error is about. The error text
// starts with the key.
func attribute(err error, sources map[string]string) error {
	key, _, _ := strings.Cut(err.Error(), ":")
	if file, ok := sources[key]; ok {
		return fmt.Errorf("%w (file %s)", err, file)
	}
	return err
}

// inFile adds the origin of a cluster or mount to each problem.
func inFile(origin string, problems []error) []error {
	for i, problem := range problems {
		problems[i] = fmt.Errorf("%w (%s)", problem, origin)
	}
	return problems
}

// fileOrigin names the file of a cluster or mount.
func fileOrigin(sub, name string) string {
	return "file " + source(sub, name+yamlExt)
}

// mountOrigin names where the mount at index came from. The mounts from index own on were
// requested and have no file, and their generated names would mean nothing to the person
// who asked, so the target names them.
func mountOrigin(mount *Mount, index, own int) string {
	if index >= own {
		return "requested mount " + mount.Target
	}
	return fileOrigin(mountsDirName, mount.Name)
}

// normalize puts valid values in their final form: no trailing slash on a
// cluster URL, a cleaned mount path. It runs after validation because
// cleaning would hide a ".." that validation must reject.
func normalize(cfg *Config) {
	for name, cluster := range cfg.Clusters {
		cluster.URL = strings.TrimRight(cluster.URL, "/")
		cfg.Clusters[name] = cluster
	}
	for i := range cfg.Mounts {
		cfg.Mounts[i].Path = path.Clean(cfg.Mounts[i].Path)
	}
}

// required reports an empty value.
func required(key, value string) []error {
	if value == "" {
		return []error{fmt.Errorf("%s: %w", key, ErrEmpty)}
	}
	return nil
}

// requiredAbsolute reports an empty value and a value that is not an absolute path.
func requiredAbsolute(key, value string) []error {
	if value != "" && !filepath.IsAbs(value) {
		return []error{fmt.Errorf("%s: %q %w", key, value, ErrNotAbsolute)}
	}
	return required(key, value)
}

func validateClusters(cfg *Config) []error {
	names := cfg.clusterNames()
	problems := make([]error, 0, len(names))
	for _, name := range names {
		found := validateCluster(name, cfg.Clusters[name])
		problems = append(problems, inFile(fileOrigin(clustersDirName, name), found)...)
	}
	return problems
}

func validateCluster(name string, cluster Cluster) []error {
	key := "clusters." + name
	var problems []error
	if !namePattern.MatchString(name) {
		problems = append(problems, fmt.Errorf("%s: %w %s", key, ErrNamePattern, namePattern))
	}
	if err := validateURL(cluster.URL); err != nil {
		problems = append(problems, fmt.Errorf("%s.url: %w", key, err))
	}
	problems = append(problems, required(key+".user", cluster.User)...)
	problems = append(problems, required(key+".password_file", cluster.PasswordFile)...)
	if cluster.BlockSize < minBlockSize || cluster.BlockSize > maxBlockSize {
		problems = append(problems, fmt.Errorf("%s.block_size: %s %w %s to %s",
			key, cluster.BlockSize, ErrBlockSizeRange, minBlockSize, maxBlockSize))
	}
	if cluster.StreamsPerNode < minStreamsPerNode || cluster.StreamsPerNode > maxStreamsPerNode {
		problems = append(problems, fmt.Errorf("%s.streams_per_node: %d %w %d and %d",
			key, cluster.StreamsPerNode, ErrStreamsRange, minStreamsPerNode, maxStreamsPerNode))
	}
	if cluster.ReadAhead.Unlimited() || cluster.ReadAhead < cluster.BlockSize {
		problems = append(problems, fmt.Errorf("%s.read_ahead: %s %w (%s)",
			key, cluster.ReadAhead, ErrReadAheadTooSmall, cluster.BlockSize))
	}
	return problems
}

// validateURL checks for an absolute http or https URL with a host and without
// a query or fragment.
func validateURL(raw string) error {
	if raw == "" {
		return ErrEmpty
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return err //nolint:wrapcheck // The caller adds the key, the url package error says the rest.
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Errorf("%q %w", raw, ErrURLNotAbsolute)
	}
	// A bare "?" or "#" parses to an empty query or fragment, so the text is
	// checked as well.
	if strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("%q %w", raw, ErrURLQuery)
	}
	return nil
}

func validateMounts(cfg *Config, own int) []error {
	var problems []error
	for i := range cfg.Mounts {
		mount := &cfg.Mounts[i]
		found := validateMount(mount, cfg)
		found = append(found, validateMountCache(mount, cfg)...)
		if i >= own {
			// The cache directory is the same for every mount, so a conflict with a requested
			// mount is that mount's doing and must not be blamed on the file that set the
			// directory.
			found = append(found, validateCacheDir("cache.dir", cfg.Cache.Dir, cfg.Mounts[i:i+1])...)
		}
		problems = append(problems, inFile(mountOrigin(mount, i, own), found)...)
	}
	return append(problems, validateTargets(cfg.Mounts, own)...)
}

func validateMount(mount *Mount, cfg *Config) []error {
	key := "mounts." + mount.Name
	var problems []error
	if !namePattern.MatchString(mount.Name) {
		problems = append(problems, fmt.Errorf("%s: %w %s", key, ErrNamePattern, namePattern))
	}
	if _, ok := cfg.Clusters[mount.Cluster]; !ok {
		problems = append(problems, fmt.Errorf("%s.cluster: %q %w", key, mount.Cluster, ErrUnknownCluster))
	}
	if !strings.HasPrefix(mount.Path, "/") {
		problems = append(problems, fmt.Errorf("%s.path: %q %w", key, mount.Path, ErrPathNoSlash))
	}
	if hasDotDot(mount.Path) {
		problems = append(problems, fmt.Errorf("%s.path: %q %w", key, mount.Path, ErrPathDotDot))
	}
	return append(problems, requiredAbsolute(key+".target", mount.Target)...)
}

func hasDotDot(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "..")
}

// validateTargets reports targets that are shared or nested. A mount over a
// directory that holds another mount would hide it. The mounts from index own on were
// requested.
func validateTargets(mounts []Mount, own int) []error {
	var problems []error
	for laterIndex := range mounts {
		later := &mounts[laterIndex]
		for earlierIndex := range laterIndex {
			earlier := &mounts[earlierIndex]
			err := checkTargets(earlier.Target, later.Target)
			if err == nil {
				continue
			}
			found := []error{fmt.Errorf("mounts.%s.target: %w (mounts.%s)", later.Name, err, earlier.Name)}
			problems = append(problems, inFile(mountOrigin(later, laterIndex, own), found)...)
		}
	}
	return problems
}

func checkTargets(earlier, later string) error {
	switch {
	case !filepath.IsAbs(earlier) || !filepath.IsAbs(later):
		return nil
	case earlier == later:
		return fmt.Errorf("%s %w", later, ErrTargetShared)
	case contains(earlier, later):
		return fmt.Errorf("%s %w %s of another mount", later, ErrTargetInside, earlier)
	case contains(later, earlier):
		return fmt.Errorf("%s %w %s of another mount", later, ErrTargetContains, earlier)
	}
	return nil
}

// contains reports whether inner is outer or lies below it. It compares whole
// path segments, so /mnt/files2 is not inside /mnt/files. Both paths must be
// clean and absolute.
func contains(outer, inner string) bool {
	if outer == inner {
		return true
	}
	prefix := outer
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(inner, prefix)
}

// validateMountCache checks the cache of a mount when it differs from the top-level one. A
// mount that inherits the top-level cache unchanged would only repeat its problems.
func validateMountCache(mount *Mount, cfg *Config) []error {
	key := "mounts." + mount.Name + ".cache"
	var problems []error
	if mount.Cache != cfg.Cache {
		problems = validateCache(key, mount.Cache, largestBlockSize(cfg.Clusters))
	}
	if mount.Cache.Dir != cfg.Cache.Dir {
		problems = append(problems, validateCacheDir(key+".dir", mount.Cache.Dir, cfg.Mounts)...)
	}
	return problems
}

func largestBlockSize(clusters map[string]Cluster) units.Size {
	var largest units.Size
	for _, cluster := range clusters {
		largest = max(largest, cluster.BlockSize)
	}
	return largest
}

func validateCache(key string, cache Cache, largestBlock units.Size) []error {
	problems := requiredAbsolute(key+".dir", cache.Dir)
	problems = append(problems, validateCacheEviction(key, cache, largestBlock)...)
	problems = append(problems, validateCacheMinimums(key, cache)...)
	return append(problems, validateCacheFreshness(key, cache)...)
}

// validateCacheEviction checks the limits that decide when cached files are evicted.
func validateCacheEviction(key string, cache Cache, largestBlock units.Size) []error {
	var problems []error
	if !cache.MaxSize.Unlimited() && cache.MaxSize < largestBlock {
		problems = append(problems, fmt.Errorf("%s.max_size: %s %w (%s), use at least that or \"unlimited\"",
			key, cache.MaxSize, ErrMaxSizeTooSmall, largestBlock))
	}
	if !cache.MaxAge.Unlimited() && cache.MaxAge <= 0 {
		problems = append(problems, fmt.Errorf("%s.max_age: %s %w", key, cache.MaxAge, ErrMaxAge))
	}
	return problems
}

// validateCacheMinimums checks the sizes that only have a meaning when finite.
func validateCacheMinimums(key string, cache Cache) []error {
	var problems []error
	if cache.MinFree.Unlimited() {
		problems = append(problems, fmt.Errorf("%s.min_free: %w", key, ErrUnlimitedSize))
	}
	if cache.MinFileSize.Unlimited() {
		problems = append(problems, fmt.Errorf("%s.min_file_size: %w", key, ErrUnlimitedSize))
	}
	return problems
}

// validateCacheFreshness checks the times that decide how long cached data is trusted.
func validateCacheFreshness(key string, cache Cache) []error {
	var problems []error
	if cache.Revalidate.Unlimited() || cache.Revalidate <= 0 {
		problems = append(problems, fmt.Errorf("%s.revalidate: %s %w", key, cache.Revalidate, ErrRevalidate))
	}
	if cache.ListingTTL.Unlimited() || cache.ListingTTL < 0 {
		problems = append(problems, fmt.Errorf("%s.listing_ttl: %s %w", key, cache.ListingTTL, ErrListingTTL))
	}
	return problems
}

// validateCacheDir applies the rule that a cache directory must not be a mount target, lie
// inside one or contain one: the client would cache the files it is serving into the
// mount, or hide the cache under it.
func validateCacheDir(key, dir string, mounts []Mount) []error {
	if !filepath.IsAbs(dir) {
		return nil
	}
	var problems []error
	for i := range mounts {
		mount := &mounts[i]
		if filepath.IsAbs(mount.Target) && (contains(mount.Target, dir) || contains(dir, mount.Target)) {
			problems = append(problems, fmt.Errorf("%s: %s %w %s of mounts.%s",
				key, dir, ErrCacheDirOverlap, mount.Target, mount.Name))
		}
	}
	return problems
}
