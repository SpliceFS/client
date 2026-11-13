package config

import (
	"errors"
	"os"
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
	t.Setenv("HOME", t.TempDir())
	oldConfig, oldCache := defaultConfigDir, defaultCacheDir
	t.Cleanup(func() { defaultConfigDir, defaultCacheDir = oldConfig, oldCache })

	defaultConfigDir, defaultCacheDir = "", ""
	wantConfig := resolveOSDir(t, osConfigDir)
	wantCache := resolveOSDir(t, osCacheDir)
	if got, err := DefaultConfigDir(); err != nil || got != wantConfig {
		t.Errorf("DefaultConfigDir = %q, %v, want %q", got, err, wantConfig)
	}
	if got, err := DefaultCacheDir(); err != nil || got != wantCache {
		t.Errorf("DefaultCacheDir = %q, %v, want %q", got, err, wantCache)
	}

	defaultConfigDir, defaultCacheDir = "/c", "/s"
	if got, err := DefaultConfigDir(); err != nil || got != "/c" {
		t.Errorf("DefaultConfigDir = %q, %v, want the build setting", got, err)
	}
	if got, err := DefaultCacheDir(); err != nil || got != "/s" {
		t.Errorf("DefaultCacheDir = %q, %v, want the build setting", got, err)
	}
}

// resolveOSDir resolves the operating system default of a directory for the real home directory.
func resolveOSDir(t *testing.T, fallback string) string {
	t.Helper()
	dir, err := resolveDir("", fallback, os.UserHomeDir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
