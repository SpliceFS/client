package cache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestOSFSMissingPath checks that every operation of the real file system reports a
// missing path as an error that still matches fs.ErrNotExist after wrapping.
func TestOSFSMissingPath(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	target := filepath.Join(t.TempDir(), "target")
	ops := map[string]func(osFS) error{
		"ReadDir": func(fsys osFS) error {
			_, err := fsys.ReadDir(missing)
			return err
		},
		"ReadFile": func(fsys osFS) error {
			_, err := fsys.ReadFile(missing)
			return err
		},
		"OpenFile": func(fsys osFS) error {
			_, err := fsys.OpenFile(filepath.Join(missing, "file"), 0)
			return err
		},
		"Remove": func(fsys osFS) error { return fsys.Remove(missing) },
		"Rename": func(fsys osFS) error { return fsys.Rename(missing, target) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := op(osFS{}); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s of a missing path = %v, want fs.ErrNotExist", name, err)
			}
		})
	}
}

func TestUnsyncedFS(t *testing.T) {
	t.Parallel()
	fsys := unsyncedFS{}
	if _, err := fsys.OpenFile(filepath.Join(t.TempDir(), "missing", "file"), 0); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenFile of a missing path = %v, want fs.ErrNotExist", err)
	}
	file, err := fsys.OpenFile(filepath.Join(t.TempDir(), "file"), os.O_WRONLY|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Errorf("Sync = %v, want nil", err)
	}
	if err := file.Close(); err != nil {
		t.Error(err)
	}
}

func TestOpenUnsynced(t *testing.T) {
	t.Parallel()
	c, err := OpenUnsynced(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}
