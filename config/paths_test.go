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
	sys := systemHost()
	if sys.xdg != want {
		t.Errorf("systemHost().xdg = %v on %s, want %v", sys.xdg, runtime.GOOS, want)
	}
	if sys.geteuid() != os.Geteuid() || sys.getenv("PATH") != os.Getenv("PATH") {
		t.Error("systemHost does not read the running system")
	}
	if got, err := sys.home(); err != nil || got == "" {
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

// cacheFolder describes a folder like the cache folder, with no build setting.
func cacheFolder() folder {
	return folder{
		system: "/var/cache/splicefs",
		xdgEnv: "XDG_CACHE_HOME", xdgBase: ".cache", name: "splicefs",
	}
}

func systemOf(xdg bool, euid int, env map[string]string) system {
	return system{
		getenv:  func(key string) string { return env[key] },
		geteuid: func() int { return euid },
		home:    func() (string, error) { return "/home/u", nil },
		xdg:     xdg,
	}
}

func TestDefaultDir(t *testing.T) {
	t.Parallel()
	const built = "/usr/local/etc/splicefs/client"
	config := map[string]string{"XDG_CONFIG_HOME": "/x/cfg"}
	relative := map[string]string{"XDG_CONFIG_HOME": "cfg"}
	cache := map[string]string{"XDG_CACHE_HOME": "/x/cache"}
	tests := []struct {
		name   string
		sys    system
		folder folder
		want   string
	}{
		{"ordinary user, XDG variable set", systemOf(true, 1000, config), folderOf(""), "/x/cfg/splicefs/client"},
		{"ordinary user, no XDG variable", systemOf(true, 1000, nil), folderOf(""), "/home/u/.config/splicefs/client"},
		{
			"ordinary user, relative XDG variable is ignored",
			systemOf(true, 1000, relative), folderOf(""), "/home/u/.config/splicefs/client",
		},
		{
			"ordinary user, the build setting does not apply",
			systemOf(true, 1000, nil), folderOf(built), "/home/u/.config/splicefs/client",
		},
		{"root keeps the system folder", systemOf(true, 0, config), folderOf(""), "/etc/splicefs/client"},
		{"root keeps the build setting", systemOf(true, 0, nil), folderOf(built), built},
		{
			"system without the convention, ordinary user",
			systemOf(false, 1000, config), folderOf(""), "/etc/splicefs/client",
		},
		{"system without the convention, the build setting", systemOf(false, 1000, nil), folderOf("~/b"), "/home/u/b"},
		{"cache folder of an ordinary user", systemOf(true, 1000, cache), cacheFolder(), "/x/cache/splicefs"},
		{"cache folder without the variable", systemOf(true, 1000, nil), cacheFolder(), "/home/u/.cache/splicefs"},
		{"cache folder of root", systemOf(true, 0, nil), cacheFolder(), "/var/cache/splicefs"},
		{"cache folder of root ignores XDG", systemOf(true, 0, cache), cacheFolder(), "/var/cache/splicefs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := defaultDir(tt.sys, tt.folder)
			if err != nil || got != tt.want {
				t.Errorf("defaultDir = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestDefaultDirWithoutHome(t *testing.T) {
	t.Parallel()
	noHome := errors.New("no home")
	sys := systemOf(true, 1000, nil)
	sys.home = func() (string, error) { return "", noHome }
	_, err := defaultDir(sys, folderOf(""))
	const want = "find the home directory for ~/.config/splicefs/client: no home"
	if !errors.Is(err, noHome) || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	// A folder named by the XDG variable needs no home directory.
	sys = systemOf(true, 1000, map[string]string{"XDG_CONFIG_HOME": "/x"})
	sys.home = func() (string, error) { return "", noHome }
	if got, err := defaultDir(sys, folderOf("")); err != nil || got != "/x/splicefs/client" {
		t.Errorf("defaultDir = %q, %v", got, err)
	}
}
