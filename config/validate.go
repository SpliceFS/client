package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
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
	var problems []error
	problems = append(problems, validateClusters(cfg)...)
	problems = append(problems, validateMounts(cfg, own)...)
	top := validateCache("cache", cfg.Cache, largestBlockSize(cfg.Clusters))
	top = append(top, validateCacheDir("cache.dir", cfg.Cache.Dir, cfg.Mounts[:own])...)
	for _, p := range top {
		problems = append(problems, attribute(p, cfg.Sources))
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
	for i, p := range problems {
		problems[i] = fmt.Errorf("%w (%s)", p, origin)
	}
	return problems
}

// fileOrigin names the file of a cluster or mount.
func fileOrigin(sub, name string) string {
	return "file " + source(sub, name+yamlExt)
}

// mountOrigin names where a mount came from. A requested mount has no file, and its
// generated name would mean nothing to the person who asked, so the target names it.
func mountOrigin(m Mount, requested bool) string {
	if requested {
		return "requested mount " + m.Target
	}
	return fileOrigin(mountsDirName, m.Name)
}

// normalize puts valid values in their final form: no trailing slash on a
// cluster URL, a cleaned mount path. It runs after validation because
// cleaning would hide a ".." that validation must reject.
func normalize(cfg *Config) {
	for name, c := range cfg.Clusters {
		c.URL = strings.TrimRight(c.URL, "/")
		cfg.Clusters[name] = c
	}
	for i := range cfg.Mounts {
		cfg.Mounts[i].Path = path.Clean(cfg.Mounts[i].Path)
	}
}

// required reports an empty value.
func required(key, value string) []error {
	if value == "" {
		return []error{fmt.Errorf("%s: must not be empty", key)}
	}
	return nil
}

func validateClusters(cfg *Config) []error {
	var problems []error
	for _, name := range cfg.clusterNames() {
		found := validateCluster(name, cfg.Clusters[name])
		problems = append(problems, inFile(fileOrigin(clustersDirName, name), found)...)
	}
	return problems
}

func validateCluster(name string, c Cluster) []error {
	key := "clusters." + name
	var problems []error
	if !namePattern.MatchString(name) {
		problems = append(problems, fmt.Errorf("%s: name must match %s", key, namePattern))
	}
	if err := validateURL(c.URL); err != nil {
		problems = append(problems, fmt.Errorf("%s.url: %w", key, err))
	}
	problems = append(problems, required(key+".user", c.User)...)
	problems = append(problems, required(key+".password_file", c.PasswordFile)...)
	if c.BlockSize < minBlockSize || c.BlockSize > maxBlockSize {
		problems = append(problems, fmt.Errorf("%s.block_size: %s is not allowed, use a size from %s to %s",
			key, c.BlockSize, minBlockSize, maxBlockSize))
	}
	if c.StreamsPerNode < minStreamsPerNode || c.StreamsPerNode > maxStreamsPerNode {
		problems = append(problems, fmt.Errorf("%s.streams_per_node: %d is not between %d and %d",
			key, c.StreamsPerNode, minStreamsPerNode, maxStreamsPerNode))
	}
	if c.ReadAhead.Unlimited() || c.ReadAhead < c.BlockSize {
		problems = append(problems, fmt.Errorf("%s.read_ahead: %s is not allowed, use a finite size of at least one block (%s)",
			key, c.ReadAhead, c.BlockSize))
	}
	return problems
}

// validateURL checks for an absolute http or https URL with a host and without
// a query or fragment.
func validateURL(raw string) error {
	if raw == "" {
		return errors.New("must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("%q is not an absolute http or https URL with a host", raw)
	}
	// A bare "?" or "#" parses to an empty query or fragment, so the text is
	// checked as well.
	if strings.ContainsAny(raw, "?#") {
		return fmt.Errorf("%q must not have a query or fragment", raw)
	}
	return nil
}

func validateMounts(cfg *Config, own int) []error {
	var problems []error
	for i, m := range cfg.Mounts {
		requested := i >= own
		found := validateMount(m, cfg)
		found = append(found, validateMountCache(m, cfg)...)
		if requested {
			// The cache directory is the same for every mount, so a conflict with a requested
			// mount is that mount's doing and must not be blamed on the file that set the
			// directory.
			found = append(found, validateCacheDir("cache.dir", cfg.Cache.Dir, []Mount{m})...)
		}
		problems = append(problems, inFile(mountOrigin(m, requested), found)...)
	}
	return append(problems, validateTargets(cfg.Mounts, own)...)
}

func validateMount(m Mount, cfg *Config) []error {
	key := "mounts." + m.Name
	var problems []error
	if !namePattern.MatchString(m.Name) {
		problems = append(problems, fmt.Errorf("%s: name must match %s", key, namePattern))
	}
	if _, ok := cfg.Clusters[m.Cluster]; !ok {
		problems = append(problems, fmt.Errorf("%s.cluster: %q is not a cluster defined in clusters", key, m.Cluster))
	}
	if !strings.HasPrefix(m.Path, "/") {
		problems = append(problems, fmt.Errorf("%s.path: %q must start with \"/\"", key, m.Path))
	}
	if hasDotDot(m.Path) {
		problems = append(problems, fmt.Errorf("%s.path: %q must not contain \"..\"", key, m.Path))
	}
	switch {
	case m.Target == "":
		problems = append(problems, fmt.Errorf("%s.target: must not be empty", key))
	case !filepath.IsAbs(m.Target):
		problems = append(problems, fmt.Errorf("%s.target: %q is not an absolute path", key, m.Target))
	}
	return problems
}

func hasDotDot(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// validateTargets reports targets that are shared or nested. A mount over a
// directory that holds another mount would hide it. The mounts from index own on were
// requested.
func validateTargets(mounts []Mount, own int) []error {
	var problems []error
	for j, later := range mounts {
		for _, earlier := range mounts[:j] {
			if err := checkTargets(earlier.Target, later.Target); err != nil {
				found := []error{fmt.Errorf("mounts.%s.target: %w (mounts.%s)", later.Name, err, earlier.Name)}
				problems = append(problems, inFile(mountOrigin(later, j >= own), found)...)
			}
		}
	}
	return problems
}

func checkTargets(earlier, later string) error {
	switch {
	case !filepath.IsAbs(earlier) || !filepath.IsAbs(later):
		return nil
	case earlier == later:
		return fmt.Errorf("%s is already the target of another mount", later)
	case contains(earlier, later):
		return fmt.Errorf("%s is inside the target %s of another mount", later, earlier)
	case contains(later, earlier):
		return fmt.Errorf("%s contains the target %s of another mount", later, earlier)
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
func validateMountCache(m Mount, cfg *Config) []error {
	key := "mounts." + m.Name + ".cache"
	var problems []error
	if m.Cache != cfg.Cache {
		problems = validateCache(key, m.Cache, largestBlockSize(cfg.Clusters))
	}
	if m.Cache.Dir != cfg.Cache.Dir {
		problems = append(problems, validateCacheDir(key+".dir", m.Cache.Dir, cfg.Mounts)...)
	}
	return problems
}

func largestBlockSize(clusters map[string]Cluster) units.Size {
	var largest units.Size
	for _, c := range clusters {
		largest = max(largest, c.BlockSize)
	}
	return largest
}

func validateCache(key string, c Cache, largestBlock units.Size) []error {
	problems := required(key+".dir", c.Dir)
	if c.Dir != "" && !filepath.IsAbs(c.Dir) {
		problems = append(problems, fmt.Errorf("%s.dir: %q is not an absolute path", key, c.Dir))
	}
	if !c.MaxSize.Unlimited() && c.MaxSize < largestBlock {
		problems = append(problems, fmt.Errorf("%s.max_size: %s is smaller than one block of the largest cluster (%s), use at least that or \"unlimited\"",
			key, c.MaxSize, largestBlock))
	}
	if !c.MaxAge.Unlimited() && c.MaxAge <= 0 {
		problems = append(problems, fmt.Errorf("%s.max_age: %s is not allowed, use a duration above 0 or \"unlimited\"", key, c.MaxAge))
	}
	if c.MinFree.Unlimited() {
		problems = append(problems, fmt.Errorf("%s.min_free: unlimited is not allowed, use a finite size", key))
	}
	if c.MinFileSize.Unlimited() {
		problems = append(problems, fmt.Errorf("%s.min_file_size: unlimited is not allowed, use a finite size", key))
	}
	if c.Revalidate.Unlimited() || c.Revalidate <= 0 {
		problems = append(problems, fmt.Errorf("%s.revalidate: %s is not allowed, use a finite duration above 0", key, c.Revalidate))
	}
	if c.ListingTTL.Unlimited() || c.ListingTTL < 0 {
		problems = append(problems, fmt.Errorf("%s.listing_ttl: %s is not allowed, use a finite duration of at least 0", key, c.ListingTTL))
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
	for _, m := range mounts {
		if filepath.IsAbs(m.Target) && (contains(m.Target, dir) || contains(dir, m.Target)) {
			problems = append(problems, fmt.Errorf("%s: %s must not be, lie inside or contain the target %s of mounts.%s",
				key, dir, m.Target, m.Name))
		}
	}
	return problems
}
