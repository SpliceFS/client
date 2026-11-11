package fuse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"time"

	"github.com/SpliceFS/client/mount"
)

// abortTimeout bounds the cleanup of a mount that never became ready.
const abortTimeout = 10 * time.Second

// host is what a mount needs from the operating system. It is an interface so that the
// life cycle logic can be tested without a kernel.
type host interface {
	// Stat is os.Stat.
	Stat(path string) (fs.FileInfo, error)
	// Unmount detaches the mount at target. With detach set the mount point is removed
	// from the tree at once and the filesystem is released when its last user is gone;
	// without it, a mount that is in use is refused with an error wrapping EBUSY.
	Unmount(ctx context.Context, target string, detach bool) error
}

// serveLoop is the part of a go-fuse server that a mounted drives.
type serveLoop interface {
	// Serve answers kernel requests until the mount ends.
	Serve()
	// WaitMount returns when the mount is ready for use.
	WaitMount() error
}

// prepareTarget checks that target can be mounted on. A mount left behind by a crashed
// client answers every access with ENOTCONN; it is detached so that a new mount can
// take its place.
func prepareTarget(ctx context.Context, h host, target string) error {
	info, err := h.Stat(target)
	if errors.Is(err, syscall.ENOTCONN) {
		if err := h.Unmount(ctx, target, true); err != nil {
			return fmt.Errorf("clear dead mount: %w", err)
		}
		info, err = h.Stat(target)
	}
	if err != nil {
		return fmt.Errorf("mount target: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("mount target %s: %w", target, syscall.ENOTDIR)
	}
	return nil
}

// mounted is a live mount. It implements mount.Mounted.
type mounted struct {
	target string
	host   host
	done   chan struct{}
	err    error // written before done is closed
}

var _ mount.Mounted = (*mounted)(nil)

// start runs the serve loop and waits until the mount is ready. If it never becomes
// ready the mount is taken down again so that nothing is left behind.
func start(loop serveLoop, h host, target string) (mount.Mounted, error) {
	m := &mounted{target: target, host: h, done: make(chan struct{})}
	go m.run(loop.Serve)
	if err := loop.WaitMount(); err != nil {
		return nil, errors.Join(fmt.Errorf("wait for mount of %s: %w", target, err), m.abort())
	}
	return m, nil
}

func (m *mounted) run(serve func()) {
	serve()
	m.err = m.endError()
	close(m.done)
}

// endError tells a clean end from a lost connection. A serve loop that stops while the
// mount is still attached leaves a dead mount behind, which answers with ENOTCONN. After
// an unmount, from here or from outside, the target is an ordinary directory again.
func (m *mounted) endError() error {
	if _, err := m.host.Stat(m.target); errors.Is(err, syscall.ENOTCONN) {
		return fmt.Errorf("connection to the kernel for %s was lost: %w", m.target, err)
	}
	return nil
}

func (m *mounted) abort() error {
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()
	return m.Unmount(ctx, false)
}

// Done implements mount.Mounted.
func (m *mounted) Done() <-chan struct{} { return m.done }

// Err implements mount.Mounted.
func (m *mounted) Err() error { return m.err }

// Unmount implements mount.Mounted. Without force it returns after the serve loop has
// ended. With force it returns once the mount is detached; the loop then ends when the
// last program using the mount lets go of it.
func (m *mounted) Unmount(ctx context.Context, force bool) error {
	select {
	case <-m.done:
		return nil
	default:
	}
	if err := m.unmount(ctx, force); err != nil {
		return err
	}
	if force {
		return nil
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for %s to unmount: %w", m.target, ctx.Err())
	}
}

// unmount runs the host's unmount in its own goroutine because the system call cannot
// be interrupted and ctx must still be honoured. The goroutine ends when the call
// returns.
func (m *mounted) unmount(ctx context.Context, detach bool) error {
	result := make(chan error, 1)
	go func() { result <- m.host.Unmount(ctx, m.target, detach) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("unmount %s: %w", m.target, ctx.Err())
	}
}
