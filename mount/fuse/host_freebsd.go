//go:build freebsd

package fuse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// errNoConnectionControl says that FreeBSD has no control directory for FUSE connections.
// The caller then knows no abort is possible, which only costs the cut of a dead mount.
var errNoConnectionControl = errors.New("FUSE connections cannot be aborted on FreeBSD")

// mntForce is MNT_FORCE from sys/mount.h; the syscall package does not export it for FreeBSD.
const mntForce = 0x80000

// kernelHost is the host backed by the running FreeBSD kernel.
type kernelHost struct{}

var _ host = kernelHost{}

func newKernelHost() kernelHost { return kernelHost{} }

// Stat implements host.
func (kernelHost) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

// ConnectionID implements host. FreeBSD has no equivalent of the Linux control directory.
func (kernelHost) ConnectionID(string) (uint32, error) { return 0, errNoConnectionControl }

// AbortConnection implements host. It is never called, because ConnectionID fails.
func (kernelHost) AbortConnection(uint32) error { return errNoConnectionControl }

// Unmount implements host. FreeBSD has no lazy unmount, so detach means MNT_FORCE, which
// also makes open files fail. A mount that is in use is refused with EBUSY otherwise.
func (kernelHost) Unmount(_ context.Context, target string, detach bool) error {
	flags := 0
	if detach {
		flags = mntForce
	}
	if err := syscall.Unmount(target, flags); err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}
