// Package mount defines what every mount layer offers the client program. A mount layer
// (FUSE on Linux and FreeBSD, a local NFS server on macOS) turns kernel requests into calls on a
// core.FS. Keeping this contract in its own package lets the program choose a layer per
// operating system without the layers knowing about each other.
package mount

import (
	"context"
	"log/slog"
)

// ProbeName is a name directly inside the root of every mount that never exists. Both
// mount layers answer a lookup of it with "not found" at once, without asking the
// core.FS, so that a lookup travels from the kernel to the client and back without
// depending on the network, a cache or a lock a slow read could hold. The program uses
// it to tell a mount whose connection has stopped answering from one that is only slow.
// In a subdirectory the name is an ordinary name.
const ProbeName = ".splicefs-probe"

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
	// RecordPath is the file where macOS keeps a list of the mounts it made, so that a
	// mount left behind by a client that died can be found and removed later. Empty
	// keeps no list. Other systems ignore it, because FUSE clears a dead mount itself.
	RecordPath string
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

// Aborter is implemented by a mount whose kernel connection can be cut. Abort makes every
// request that waits on the connection fail at once. It is for a mount that has stopped
// answering; a healthy mount is ended with Unmount, which lets open files finish.
type Aborter interface {
	Abort() error
}
