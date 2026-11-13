//go:build linux

package fuse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// connectionsDir is the control directory of the FUSE driver; it has one directory per
// connection, named by the connection number.
const connectionsDir = "/sys/fs/fuse/connections"

// mountTable is the list of the mounts of this process, one line per mount.
const mountTable = "/proc/self/mountinfo"

// errNotListed says that the mount table has no FUSE mount at the asked point.
var errNotListed = errors.New("no FUSE mount listed")

// kernelHost is the host backed by the running Linux kernel.
type kernelHost struct {
	// unmountCall is the umount2 system call.
	unmountCall func(target string, flags int) error
	// findHelper returns the path of the fusermount program.
	findHelper func() (string, error)
	// dir is the control directory of the FUSE driver.
	dir string
	// mountinfo is the file that lists the mounts.
	mountinfo string
}

var _ host = kernelHost{}

func newKernelHost() kernelHost {
	return kernelHost{
		unmountCall: syscall.Unmount,
		findHelper:  findFusermount,
		dir:         connectionsDir,
		mountinfo:   mountTable,
	}
}

// Stat implements host.
func (kernelHost) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

// ConnectionID implements host. The number comes from the mount table and never from the
// mount point itself, so that asking does not disturb how a dead mount answers.
func (h kernelHost) ConnectionID(target string) (uint32, error) {
	table, err := os.ReadFile(h.mountinfo)
	if err != nil {
		return 0, fmt.Errorf("read the mount table: %w", err)
	}
	id, ok := fuseConnectionID(string(table), target)
	if !ok {
		return 0, fmt.Errorf("connection of %s: %w", target, errNotListed)
	}
	return id, nil
}

// AbortConnection implements host. The file must already exist: opening without
// creating keeps a connection that is gone from being mistaken for one to abort.
func (h kernelHost) AbortConnection(id uint32) error {
	path := filepath.Join(h.dir, strconv.FormatUint(uint64(id), 10), "abort")
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("abort connection %d: %w", id, err)
	}
	_, err = f.WriteString("1")
	return errors.Join(err, f.Close())
}

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
