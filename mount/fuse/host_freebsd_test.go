//go:build freebsd

package fuse

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SpliceFS/client/mount"
)

func TestKernelHostStat(t *testing.T) {
	t.Parallel()
	info, err := newKernelHost().Stat(t.TempDir())
	if err != nil || !info.IsDir() {
		t.Fatalf("Stat = %v, %v", info, err)
	}
	if _, err := newKernelHost().Stat(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat of a missing path = %v, want ErrNotExist", err)
	}
}

func TestKernelHostCannotControlConnections(t *testing.T) {
	t.Parallel()
	h := newKernelHost()
	if _, err := h.ConnectionID(t.TempDir()); !errors.Is(err, errNoConnectionControl) {
		t.Fatalf("ConnectionID = %v, want errNoConnectionControl", err)
	}
	if err := h.AbortConnection(7); !errors.Is(err, errNoConnectionControl) {
		t.Fatalf("AbortConnection = %v, want errNoConnectionControl", err)
	}
}

// A folder is not a mount point, so the kernel refuses to unmount it with or without force.
func TestKernelHostUnmountRefusesWhatIsNotAMount(t *testing.T) {
	t.Parallel()
	for _, detach := range []bool{false, true} {
		target := t.TempDir()
		err := newKernelHost().Unmount(t.Context(), target, detach)
		if err == nil || !strings.Contains(err.Error(), "unmount "+target) {
			t.Errorf("Unmount(detach=%v) = %v, want an error that names %s", detach, err, target)
		}
	}
}

func TestMountOptionsOnFreeBSD(t *testing.T) {
	t.Parallel()
	got := mountOptions(mount.Options{Target: "/mnt/x", Source: "splicefs:example/media"}, slog.New(slog.DiscardHandler))
	if got.DirectMount || got.DirectMountFlags != 0 {
		t.Fatalf("DirectMount = %v, flags %#x; want no direct mount on FreeBSD", got.DirectMount, got.DirectMountFlags)
	}
	if !slices.Equal(got.Options, []string{"ro", "noatime"}) {
		t.Fatalf("Options = %v, want [ro noatime]", got.Options)
	}
}
