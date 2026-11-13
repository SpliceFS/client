package cache

import (
	"errors"
	"io/fs"
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
