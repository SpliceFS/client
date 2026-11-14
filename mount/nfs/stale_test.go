package nfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTargetResolvesTheFolderAndNeverTheTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	folder := filepath.Join(dir, "real")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(folder, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// A link to nothing stands for a dead mount: resolving it would fail.
	if err := os.Symlink(filepath.Join(dir, "nothing"), filepath.Join(folder, "dead")); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(folder)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		filepath.Join(dir, "link", "dead"):    filepath.Join(resolved, "dead"),
		filepath.Join(dir, "link") + "/dead/": filepath.Join(resolved, "dead"),
		filepath.Join(dir, "link", "absent"):  filepath.Join(resolved, "absent"),
		"/":                                   "/",
	}
	for target, want := range tests {
		if got := ResolveTarget(target); got != want {
			t.Errorf("ResolveTarget(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestResolveTargetKeepsATargetWhoseFolderIsMissing(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "missing", "x", "..", "y")
	want := filepath.Clean(target)
	if got := ResolveTarget(target); got != want {
		t.Errorf("ResolveTarget(%q) = %q, want %q", target, got, want)
	}
}
