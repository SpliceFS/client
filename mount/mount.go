// Package mount defines what every mount layer offers the client program. A mount layer
// (FUSE on Linux, a local NFS server on macOS) turns kernel requests into calls on a
// core.FS. Keeping this contract in its own package lets the program choose a layer per
// operating system without the layers knowing about each other.
package mount

import (
	"context"
	"log/slog"
)

// Options describes one mount.
type Options struct {
	// Target is the directory to mount on. It must exist.
	Target string
	// Source is the name shown for the filesystem in mount tables, for example
	// "splicefs:example/media".
	Source string
	// AllowOther lets users other than the one running the client read the mount.
	AllowOther bool
	// Logger receives diagnostics. Nil discards them.
	Logger *slog.Logger
}

// Mounted is a live mount.
type Mounted interface {
	// Done is closed when the mount has ended, whether through Unmount, an unmount
	// from outside the program, or a failure.
	Done() <-chan struct{}
	// Err reports why the mount ended. It is nil after a clean unmount and must only
	// be called after Done is closed.
	Err() error
	// Unmount ends the mount. A mount that is still in use is refused unless force is
	// set, because cutting off a program in the middle of a read should be a decision.
	Unmount(ctx context.Context, force bool) error
}
