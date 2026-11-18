package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveDir(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/u", nil }
	tests := []struct {
		name, built, fallback, want string
	}{
		{"system default", "", "/etc/splicefs/client", "/etc/splicefs/client"},
		{"home default", "", "~/.splicefs/client", "/home/u/.splicefs/client"},
		{"build setting wins", "/opt/x", "/etc/splicefs/client", "/opt/x"},
		{"build setting in the home directory", "~/x/y", "/etc/splicefs/client", "/home/u/x/y"},
		{"a tilde alone is a name", "~x", "/etc", "~x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveDir(tt.built, tt.fallback, home)
			if err != nil || got != tt.want {
				t.Errorf("resolveDir = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestResolveDirWithoutHome(t *testing.T) {
	t.Parallel()
	noHome := errors.New("no home")
	failing := func() (string, error) { return "", noHome }
	_, err := resolveDir("", "~/.splicefs/client", failing)
	const want = "find the home directory for ~/.splicefs/client: no home"
	if !errors.Is(err, noHome) || err.Error() != want {
		t.Errorf("err = %v", err)
	}
	// A directory outside the home directory needs no home.
	if got, err := resolveDir("/x", "", failing); err != nil || got != "/x" {
		t.Errorf("resolveDir = %q, %v", got, err)
	}
}

func TestDefaultDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	oldConfig, oldCache := defaultConfigDir, defaultCacheDir
	t.Cleanup(func() { defaultConfigDir, defaultCacheDir = oldConfig, oldCache })

	// What a build sets is the folder of a system-wide installation, which only root, or a
	// system without the XDG convention, uses.
	defaultConfigDir, defaultCacheDir = "/c", "/s"
	wantConfig, wantCache := "/c", "/s"
	if systemHost().ordinaryUser() {
		wantConfig = filepath.Join(home, ".config", "splicefs", "client")
		wantCache = filepath.Join(home, ".cache", "splicefs")
	}
	if got, err := DefaultConfigDir(); err != nil || got != wantConfig {
		t.Errorf("DefaultConfigDir = %q, %v, want %q", got, err, wantConfig)
	}
	if got, err := DefaultCacheDir(); err != nil || got != wantCache {
		t.Errorf("DefaultCacheDir = %q, %v, want %q", got, err, wantCache)
	}
}

func TestSystemHostFollowsXDGWhereTheConventionIsUsed(t *testing.T) {
	t.Parallel()
	want := runtime.GOOS == "linux" || runtime.GOOS == "freebsd"
	h := systemHost()
	if h.xdg != want {
		t.Errorf("systemHost().xdg = %v on %s, want %v", h.xdg, runtime.GOOS, want)
	}
	if h.geteuid() != os.Geteuid() || h.getenv("PATH") != os.Getenv("PATH") {
		t.Error("systemHost does not read the running system")
	}
	if got, err := h.home(); err != nil || got == "" {
		t.Errorf("home = %q, %v", got, err)
	}
}

// folderOf describes a folder like the configuration folder, with the build setting given.
func folderOf(built string) folder {
	return folder{
		built: built, system: "/etc/splicefs/client",
		xdgEnv: "XDG_CONFIG_HOME", xdgBase: ".config", name: "splicefs/client",
	}
}

func hostOf(xdg bool, euid int, env map[string]string) host {
	return host{
		getenv:  func(key string) string { return env[key] },
		geteuid: func() int { return euid },
		home:    func() (string, error) { return "/home/u", nil },
		xdg:     xdg,
	}
}

func TestDefaultDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		host   host
		folder folder
		want   string
	}{
		{"ordinary user, XDG variable set", hostOf(true, 1000, map[string]string{"XDG_CONFIG_HOME": "/x/cfg"}), folderOf(""), "/x/cfg/splicefs/client"},
		{"ordinary user, no XDG variable", hostOf(true, 1000, nil), folderOf(""), "/home/u/.config/splicefs/client"},
		{"ordinary user, relative XDG variable is ignored", hostOf(true, 1000, map[string]string{"XDG_CONFIG_HOME": "cfg"}), folderOf(""), "/home/u/.config/splicefs/client"},
		{"ordinary user, the build setting does not apply", hostOf(true, 1000, nil), folderOf("/usr/local/etc/splicefs/client"), "/home/u/.config/splicefs/client"},
		{"root keeps the system folder", hostOf(true, 0, map[string]string{"XDG_CONFIG_HOME": "/x/cfg"}), folderOf(""), "/etc/splicefs/client"},
		{"root keeps the build setting", hostOf(true, 0, nil), folderOf("/usr/local/etc/splicefs/client"), "/usr/local/etc/splicefs/client"},
		{"system without the convention, ordinary user", hostOf(false, 1000, map[string]string{"XDG_CONFIG_HOME": "/x/cfg"}), folderOf(""), "/etc/splicefs/client"},
		{"system without the convention, the build setting", hostOf(false, 1000, nil), folderOf("~/b"), "/home/u/b"},
		{"cache folder of an ordinary user", hostOf(true, 1000, map[string]string{"XDG_CACHE_HOME": "/x/cache"}), cacheFolder(""), "/x/cache/splicefs"},
		{"cache folder without the variable", hostOf(true, 1000, nil), cacheFolder(""), "/home/u/.cache/splicefs"},
		{"cache folder of root", hostOf(true, 0, nil), cacheFolder(""), "/var/cache/splicefs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := defaultDir(tt.host, tt.folder)
			if err != nil || got != tt.want {
				t.Errorf("defaultDir = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

// cacheFolder describes a folder like the cache folder, with the build setting given.
func cacheFolder(built string) folder {
	return folder{
		built: built, system: "/var/cache/splicefs",
		xdgEnv: "XDG_CACHE_HOME", xdgBase: ".cache", name: "splicefs",
	}
}

func TestDefaultDirWithoutHome(t *testing.T) {
	t.Parallel()
	noHome := errors.New("no home")
	h := hostOf(true, 1000, nil)
	h.home = func() (string, error) { return "", noHome }
	_, err := defaultDir(h, folderOf(""))
	const want = "find the home directory for ~/.config/splicefs/client: no home"
	if !errors.Is(err, noHome) || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	// A folder named by the XDG variable needs no home directory.
	h = hostOf(true, 1000, map[string]string{"XDG_CONFIG_HOME": "/x"})
	h.home = func() (string, error) { return "", noHome }
	if got, err := defaultDir(h, folderOf("")); err != nil || got != "/x/splicefs/client" {
		t.Errorf("defaultDir = %q, %v", got, err)
	}
}
