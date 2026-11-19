//go:build !darwin

package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOrderedSyncIsFullSync(t *testing.T) {
	t.Parallel()
	file, err := os.Create(filepath.Join(t.TempDir(), "file"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orderedSync(file); err != nil {
		t.Fatalf("orderedSync on an open file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := orderedSync(file); !errors.Is(err, os.ErrClosed) {
		t.Errorf("orderedSync on a closed file: %v, want the full sync's error", err)
	}
}
