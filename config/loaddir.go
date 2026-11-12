package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	confDirName     = "conf.d"
	clustersDirName = "clusters.d"
	mountsDirName   = "mounts.d"
)

// rawSettings is the shape of the files in conf.d. Only the cache settings exist today.
type rawSettings struct {
	Cache rawCache `yaml:"cache"`
}

// LoadDir reads conf.d/*.yaml, clusters.d/*.yaml and mounts.d/*.yaml of dir, merges the
// settings in file name order, applies defaults, resolves relative paths against dir and
// validates the result. Missing directories are empty ones, so an empty dir is a valid
// configuration without clusters and mounts. It does not read the password files or check
// that the files and directories the configuration names exist. When the configuration is
// invalid the error lists every problem found and names the file a bad value came from.
func LoadDir(dir string) (*Config, error) {
	return loadDir(dir, DefaultCacheDir)
}

// loadDir is LoadDir with the lookup of the default cache directory passed in, so tests
// need not depend on the machine they run on.
func loadDir(dir string, cacheDir func() (string, error)) (*Config, error) {
	sources := make(map[string]string)
	settings, problems := readSettings(dir, sources)
	clusters, found := readNamed[rawCluster](dir, clustersDirName, "clusters", sources)
	problems = append(problems, found...)
	mounts, found := readNamed[rawMount](dir, mountsDirName, "mounts", sources)
	problems = append(problems, found...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("read client config %s: %w", dir, errors.Join(problems...))
	}
	base, err := defaultCache(settings.Cache, cacheDir)
	if err != nil {
		return nil, err
	}
	cfg := build(dir, base, settings, clusters, mounts)
	cfg.Sources = sources
	if problems := validate(cfg, len(cfg.Mounts)); len(problems) > 0 {
		return nil, fmt.Errorf("invalid client config %s: %w", dir, errors.Join(problems...))
	}
	normalize(cfg)
	return cfg, nil
}

// defaultCache returns the defaults of the cache settings. The default directory is only
// looked up when no file sets one, so a missing home directory does not matter then.
func defaultCache(set rawCache, cacheDir func() (string, error)) (Cache, error) {
	base := baseCache()
	if set.Dir != nil {
		return base, nil
	}
	d, err := cacheDir()
	if err != nil {
		return Cache{}, fmt.Errorf("cache.dir: default unavailable, set it in a file: %w", err)
	}
	base.Dir = d
	return base, nil
}

// source names a file relative to the configuration directory.
func source(sub, file string) string {
	return sub + "/" + file
}

// readSettings decodes the files of conf.d in order onto one value and records, for every
// value a file sets, which file it was.
func readSettings(dir string, sources map[string]string) (rawSettings, []error) {
	var raw rawSettings
	sub := filepath.Join(dir, confDirName)
	names, err := yamlFiles(sub)
	if err != nil {
		return raw, []error{fmt.Errorf("%s: %w", sub, err)}
	}
	var problems []error
	for _, name := range names {
		keys, err := decodeFile(filepath.Join(sub, name), &raw)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for _, key := range keys {
			sources[key] = source(confDirName, name)
		}
	}
	return raw, problems
}

// readNamed reads one value per file of the sub directory of dir; the file name without
// .yaml is the name of the value. It records the file of every key that was set.
func readNamed[T any](dir, sub, kind string, sources map[string]string) (map[string]T, []error) {
	items := make(map[string]T)
	subDir := filepath.Join(dir, sub)
	files, err := yamlFiles(subDir)
	if err != nil {
		return items, []error{fmt.Errorf("%s: %w", subDir, err)}
	}
	var problems []error
	for _, file := range files {
		var item T
		keys, err := decodeFile(filepath.Join(subDir, file), &item)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		name := strings.TrimSuffix(file, yamlExt)
		for _, key := range keys {
			sources[kind+"."+name+"."+key] = source(sub, file)
		}
		items[name] = item
	}
	return items, problems
}
