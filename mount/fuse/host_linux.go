//go:build linux

package fuse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// kernelHost is the host backed by the running Linux kernel.
type kernelHost struct {
	// unmountCall is the umount2 system call.
	unmountCall func(target string, flags int) error
	// findHelper returns the path of the fusermount program.
	findHelper func() (string, error)
}

var _ host = kernelHost{}

func newKernelHost() kernelHost {
	return kernelHost{unmountCall: syscall.Unmount, findHelper: findFusermount}
}

// Stat implements host.
func (kernelHost) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

// Unmount implements host. The system call needs privilege; an ordinary user gets EPERM
// and goes through the setuid fusermount helper, the same way the mount was made.
func (h kernelHost) Unmount(ctx context.Context, target string, detach bool) error {
	flags := 0
	if detach {
		flags = syscall.MNT_DETACH
	}
	switch err := h.unmountCall(target, flags); {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EPERM):
		return h.helperUnmount(ctx, target, detach)
	default:
		return fmt.Errorf("unmount %s: %w", target, err)
	}
}

// helperUnmount unmounts with fusermount. The helper reports a busy mount only in its
// message, so the message is what turns the failure into an error wrapping EBUSY.
func (h kernelHost) helperUnmount(ctx context.Context, target string, detach bool) error {
	bin, err := h.findHelper()
	if err != nil {
		return err
	}
	args := []string{"-u"}
	if detach {
		args = append(args, "-z")
	}
	args = append(args, "--", target)
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput() //nolint:gosec // bin comes from PATH lookup of fixed names and target is a mount point chosen by the operator
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(string(out))
	if strings.Contains(strings.ToLower(message), strings.ToLower(syscall.EBUSY.Error())) {
		return fmt.Errorf("unmount %s: %w: %s", target, syscall.EBUSY, message)
	}
	return fmt.Errorf("unmount %s with %s: %w: %s", target, bin, err, message)
}

// findFusermount looks for the helper under both of its names: fusermount3 is the
// current one, fusermount the name used by version 2 of the package.
func findFusermount() (string, error) {
	for _, name := range []string{"fusermount3", "fusermount"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no fusermount3 or fusermount program found in PATH")
}
