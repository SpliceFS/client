package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
)

// Name of a requested mount: the prefix and this many hex digits of the target's hash.
const (
	requestedPrefix = "requested-"
	requestedDigits = 12
)

// Requested describes a mount that was asked for while the client runs, not by a file.
type Requested struct {
	Cluster string
	// Path is the cluster directory to mount. Empty means "/".
	Path string
	// Target is the local directory to mount at. It must be absolute.
	Target     string
	AllowOther bool
	// Offline overrides the default of a mount file when it is not nil.
	Offline *bool
}

// RequestedName returns the name such a mount gets: "requested-" and the first 12 hex
// characters of the SHA-256 of the cleaned target. The name follows from the target alone
// so that asking again for the same target finds the same mount.
func RequestedName(target string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(target)))
	return requestedPrefix + hex.EncodeToString(sum[:])[:requestedDigits]
}

// mount returns the mount a mount file with the same values would give. A relative target
// is left as it is so that validation rejects it: there is no directory to join it to.
func (r Requested) mount(cache Cache) Mount {
	target := r.Target
	if filepath.IsAbs(target) {
		target = filepath.Clean(target)
	}
	mountPath := r.Path
	if mountPath == "" {
		mountPath = defaultMountPath
	}
	return Mount{
		Name:       RequestedName(r.Target),
		Cluster:    r.Cluster,
		Path:       mountPath,
		Target:     target,
		AllowOther: r.AllowOther,
		Offline:    orDefault(r.Offline, defaultOffline),
		Cache:      cache,
	}
}

// WithMounts returns a copy of cfg with the requested mounts added after its own, and
// validates the result with the rules of the files. cfg is not changed. On an invalid
// result the error lists every problem; a problem caused by a requested mount names it as
// "requested mount <target>".
func WithMounts(cfg *Config, extra ...Requested) (*Config, error) {
	own := len(cfg.Mounts)
	out := &Config{
		Cache:    cfg.Cache,
		Clusters: maps.Clone(cfg.Clusters),
		Mounts:   slices.Clone(cfg.Mounts),
		Sources:  maps.Clone(cfg.Sources),
	}
	for _, r := range extra {
		out.Mounts = append(out.Mounts, r.mount(cfg.Cache))
	}
	if problems := validate(out, own); len(problems) > 0 {
		return nil, fmt.Errorf("invalid client config with requested mounts: %w",
			errors.Join(problems...))
	}
	// Cleaning waits for validation because it would hide a ".." that validation rejects.
	// The mounts of cfg are left alone so that without extra mounts the result equals cfg.
	for i := own; i < len(out.Mounts); i++ {
		out.Mounts[i].Path = path.Clean(out.Mounts[i].Path)
	}
	return out, nil
}
