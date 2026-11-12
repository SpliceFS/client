package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// ErrBusy is wrapped by the error Unmount returns for a mount that is in use and was not
// forced. The mount is still in place.
var ErrBusy = errors.New("mount is in use")

// Unmount asks the running client that listens on socketPath to unmount the mounts at
// targets, or all of its mounts when targets is empty. Without force a mount that is in
// use is refused and stays mounted. It returns an error naming every target that could
// not be unmounted; a refusal because the mount is in use wraps ErrBusy.
func Unmount(ctx context.Context, socketPath string, targets []string, force bool) error {
	return systemHost().unmount(ctx, socketPath, targets, force)
}

// Status asks the running client that listens on socketPath for its state. A missing
// client is ErrNotRunning.
func Status(ctx context.Context, socketPath string) (Report, error) {
	reply, err := systemHost().call(ctx, socketPath, Request{Op: OpStatus})
	if err != nil {
		return Report{}, err
	}
	if reply.Report == nil {
		return Report{}, errors.New("the client sent no report")
	}
	return *reply.Report, nil
}

// RequestMount asks the running client that listens on socketPath for one more mount, as
// the mount helper does for mount(8) and fstab. A missing client is ErrNotRunning, with a
// hint to start the service because fstab runs the helper at boot, before anyone looks.
func RequestMount(ctx context.Context, socketPath string, req MountRequest) error {
	return systemHost().requestMount(ctx, socketPath, req)
}

func (s system) requestMount(ctx context.Context, socketPath string, req MountRequest) error {
	_, err := s.call(ctx, socketPath, Request{Op: OpMount, Mount: &req})
	if errors.Is(err, ErrNotRunning) {
		return fmt.Errorf("%w: start the splicefs service first", err)
	}
	return err
}

func (s system) unmount(ctx context.Context, socketPath string, targets []string, force bool) error {
	// The client resolves relative paths against its own directory, which is not the
	// caller's.
	abs, err := s.absTargets(targets)
	if err != nil {
		return err
	}
	reply, err := s.call(ctx, socketPath, Request{Op: OpUnmount, Targets: abs, Force: force})
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range reply.Results {
		if err := resultError(r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func resultError(r UnmountResult) error {
	switch r.Result {
	case ResultUnmounted:
		return nil
	case ResultBusy:
		return fmt.Errorf("%s: %w", r.Target, ErrBusy)
	case ResultUnknown:
		return fmt.Errorf("%s is not a mount of the running client", r.Target)
	}
	return fmt.Errorf("%s: %s", r.Target, r.Error)
}

// call sends one request to the client on socketPath and returns its reply.
func (s system) call(ctx context.Context, socketPath string, req Request) (Reply, error) {
	conn, err := s.dial(ctx, socketPath)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return Reply{}, fmt.Errorf("%w (socket %s)", ErrNotRunning, socketPath)
	}
	if err != nil {
		return Reply{}, fmt.Errorf("connect to the client: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Closing the connection is what interrupts a blocked read when ctx ends.
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Reply{}, fmt.Errorf("send request: %w", err)
	}
	var reply Reply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return Reply{}, fmt.Errorf("read reply: %w", errors.Join(err, ctx.Err()))
	}
	if reply.Error != "" {
		return Reply{}, fmt.Errorf("the client refused the request: %s", reply.Error)
	}
	return reply, nil
}
