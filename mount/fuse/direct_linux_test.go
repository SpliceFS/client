//go:build linux

package fuse

import (
	"log/slog"
	"syscall"
	"testing"

	"github.com/SpliceFS/client/mount"
)

func TestDirectMountIsReadOnlyByBothRoutes(t *testing.T) {
	t.Parallel()
	got := mountOptions(mount.Options{Target: "/mnt/x", Source: "splicefs:example/media"}, slog.New(slog.DiscardHandler))
	if !got.DirectMount || got.DirectMountFlags&syscall.MS_RDONLY == 0 {
		t.Fatalf("DirectMount = %v, flags %#x; want a direct mount with MS_RDONLY", got.DirectMount, got.DirectMountFlags)
	}
	// Linux adds no option of its own: the read-only setting is all the helper gets.
	if len(got.Options) != 1 || got.Options[0] != "ro" {
		t.Fatalf("Options = %v, want [ro]", got.Options)
	}
}
