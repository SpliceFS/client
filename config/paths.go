package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A build may set these (-ldflags -X github.com/SpliceFS/client/config.defaultConfigDir=...)
// for a system whose packages keep their files elsewhere. Empty means the default of the
// operating system, which is chosen at compile time in paths_default.go and paths_darwin.go.
var (
	defaultConfigDir string
	defaultCacheDir  string
)

// homePrefix marks a directory that is relative to the home directory of the user running
// the program, which a build cannot know.
const homePrefix = "~/"

// DefaultConfigDir returns the configuration directory used when none is given. It fails
// when the directory is under the home directory and that cannot be found.
func DefaultConfigDir() (string, error) {
	return resolveDir(defaultConfigDir, osConfigDir, os.UserHomeDir)
}

// DefaultCacheDir returns the cache directory used when the configuration sets none. It
// fails when the directory is under the home directory and that cannot be found.
func DefaultCacheDir() (string, error) {
	return resolveDir(defaultCacheDir, osCacheDir, os.UserHomeDir)
}

// resolveDir returns built, or fallback when the build set nothing, with a leading "~/"
// replaced by the home directory. The operating system defaults are arguments so that a
// test covers both variants on any system.
func resolveDir(built, fallback string, home func() (string, error)) (string, error) {
	dir := built
	if dir == "" {
		dir = fallback
	}
	rest, underHome := strings.CutPrefix(dir, homePrefix)
	if !underHome {
		return dir, nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("find the home directory for %s: %w", dir, err)
	}
	return filepath.Join(h, rest), nil
}
