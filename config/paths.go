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
// They name the folders of the installation, so an ordinary user of a system that follows the
// XDG convention (paths_xdg.go) does not get them.
var (
	defaultConfigDir string //nolint:gochecknoglobals // set by the build with -ldflags -X
	defaultCacheDir  string //nolint:gochecknoglobals // set by the build with -ldflags -X
)

// homePrefix marks a directory that is relative to the home directory of the user running
// the program, which a build cannot know.
const homePrefix = "~/"

// system is what the default folders depend on, so that a test need not run as a particular
// user or on a particular system.
type system struct {
	getenv  func(string) string
	geteuid func() int
	home    func() (string, error)
	// xdg tells that an ordinary user of this system follows the XDG Base Directory convention.
	xdg bool
}

func systemHost() system {
	return system{getenv: os.Getenv, geteuid: os.Geteuid, home: os.UserHomeDir, xdg: ordinaryUserFollowsXDG}
}

// ordinaryUser reports whether the program runs as a user who keeps files in the home
// directory by the XDG convention. Root, and every user of a system without the convention,
// get the folders of the installation.
func (sys system) ordinaryUser() bool {
	return sys.xdg && sys.geteuid() != 0
}

// folder describes one default folder: the setting of the build and the default of the
// operating system, which a system-wide installation uses, and where an ordinary user keeps
// the folder by the XDG convention: $xdgEnv/name, or ~/xdgBase/name when the variable is not
// an absolute path.
type folder struct {
	built, system   string
	xdgEnv, xdgBase string
	name            string
}

// defaultDir finds the folder for the user the program runs as.
func defaultDir(sys system, dir folder) (string, error) {
	if !sys.ordinaryUser() {
		return resolveDir(dir.built, dir.system, sys.home)
	}
	if base := sys.getenv(dir.xdgEnv); filepath.IsAbs(base) {
		return filepath.Join(base, dir.name), nil
	}
	return resolveDir("", homePrefix+dir.xdgBase+"/"+dir.name, sys.home)
}

// DefaultConfigDir returns the configuration directory used when none is given: for root,
// the folder of the installation; for an ordinary user on Linux and FreeBSD,
// $XDG_CONFIG_HOME/splicefs/client, else ~/.config/splicefs/client. It fails when the
// directory is under the home directory and that cannot be found.
func DefaultConfigDir() (string, error) {
	return defaultDir(systemHost(), folder{
		built: defaultConfigDir, system: osConfigDir,
		xdgEnv: "XDG_CONFIG_HOME", xdgBase: ".config", name: "splicefs/client",
	})
}

// DefaultCacheDir returns the cache directory used when the configuration sets none: for
// root, the folder of the installation; for an ordinary user on Linux and FreeBSD,
// $XDG_CACHE_HOME/splicefs, else ~/.cache/splicefs. It fails when the directory is under the
// home directory and that cannot be found.
func DefaultCacheDir() (string, error) {
	return defaultDir(systemHost(), folder{
		built: defaultCacheDir, system: osCacheDir,
		xdgEnv: "XDG_CACHE_HOME", xdgBase: ".cache", name: "splicefs",
	})
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
