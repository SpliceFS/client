//go:build darwin

package nfs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BlueRival/splicefs/client/core"
	"github.com/BlueRival/splicefs/client/mount"
)

const (
	mountNFSPath = "/sbin/mount_nfs"
	umountPath   = "/sbin/umount"

	// loopbackHost is the address the kernel connects to. It is written as an address,
	// not "localhost", so that no name lookup is involved and IPv6 is never tried.
	loopbackHost = "127.0.0.1"

	// mountTimeout bounds one mount_nfs run. mount_nfs tries twice and each try waits
	// up to 30 seconds for the server (see retrycnt in mount_nfs(8)).
	mountTimeout = 90 * time.Second

	// pollInterval is how often the mount table is read to notice an unmount done
	// outside the program, for example by Finder or umount.
	pollInterval = 2 * time.Second

	// mntNoWait is MNT_NOWAIT: getfsstat(2) then returns cached data instead of asking
	// each filesystem, so reading the table never talks to our own server.
	mntNoWait = 2
)

// mountOptions is the option list for mount_nfs(8), which also accepts the generic
// mount(8) options. Each has one reason. Nothing here is a tunable for a congested
// network, because the server is on this machine.
func mountOptions(port int) string {
	p := strconv.Itoa(port)
	return strings.Join([]string{
		"vers=3",         // the only version go-nfs speaks; stops the client probing v2 and v4
		"tcp",            // go-nfs listens on TCP only
		"port=" + p,      // the NFS service is on our port, so no portmapper is needed
		"mountport=" + p, // so is the mount service: go-nfs serves both on one port
		"rdonly",         // the kernel refuses writes with EROFS before any request is sent
		"noatime",        // reads never update access times, on the mount or on the nodes
		"nobrowse",       // keeps Finder from listing the mount as a network volume
		"nodev",          // a view of remote files has no device nodes
		"nosuid",         // and no set-user-id binaries
		"soft",           // I/O errors instead of a hang when this program is gone
		"intr",           // a program waiting on the mount can be interrupted
		"actimeo=30",     // file and directory attributes cached 30 s, like the FUSE layer
		"locallocks",     // locks succeed locally; files cannot change, so they cannot conflict
		"noquota",        // there is no rquota service, so do not look for one
		"rsize=1048576",  // 1 MiB reads: go-nfs answers one request at a time, so fewer, larger requests
	}, ",")
}

// mountArgs is the argument list of mount_nfs: the options, the source shown in the
// mount table and the directory to mount on.
func mountArgs(port int, from, target string) []string {
	return []string{"-o", mountOptions(port), from, target}
}

// mountFrom is the source mount_nfs is given, and the one the mount table shows. The
// kernel needs a host and a path; the host is this machine, and the path carries the
// name the caller wants to see. go-nfs accepts any path.
func mountFrom(source string) string {
	return loopbackHost + ":/" + strings.TrimLeft(source, "/")
}

// commandRunner runs a program and returns its combined output. It is injected so
// that mounting is tested without mounting anything.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	//nolint:gosec // name is always one of the two system tool paths above.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// environment is everything Mount takes from the system, so tests can replace it.
type environment struct {
	run       commandRunner
	owner     owner
	listen    func(context.Context) (net.Listener, error)
	isMounted func(target, from string) (bool, error)
	newTicker func(time.Duration) (<-chan time.Time, func())
}

func systemEnvironment() environment {
	return environment{
		run:    execCommand,
		owner:  processOwner(),
		listen: listenLoopback,
		isMounted: func(target, from string) (bool, error) {
			return mountTableHas(syscall.Getfsstat, target, from)
		},
		newTicker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
	}
}

// Mount serves fsys read-only over NFSv3 on a loopback port and mounts it on
// opts.Target with the NFS client of macOS.
//
// The target must be a directory the user running the client owns: macOS lets an
// ordinary user mount over such a directory without root. Done closes when the server
// stops, when the mount disappears or after Unmount, and no goroutine of this package
// is left by then.
//
// opts.AllowOther has no effect. NFS has no such option: access is decided by the
// permission bits of the files, and every file is world-readable (0444, directories
// 0555), so any user who can reach the mount point can read what is mounted. The
// server itself is open to every local process on 127.0.0.1, the same as with
// AllowOther. Keeping other users out is a matter of the permissions of the
// directories above the mount point.
func Mount(fsys core.FS, opts mount.Options) (mount.Mounted, error) {
	return mountWith(fsys, opts, systemEnvironment())
}

func mountWith(fsys core.FS, opts mount.Options, env environment) (mount.Mounted, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// The mount table lists the real path, so symbolic links in the target (such as
	// /var -> /private/var) must be resolved to recognize the mount in it.
	target, err := filepath.EvalSymlinks(opts.Target)
	if err != nil {
		return nil, fmt.Errorf("resolve mount target: %w", err)
	}
	ln, err := env.listen(context.Background())
	if err != nil {
		return nil, err
	}
	srv, err := startServer(fsys, ln, serverConfig{owner: env.owner, logger: log})
	if err != nil {
		return nil, err
	}
	from := mountFrom(opts.Source)
	if err := runMount(env.run, srv.Port(), from, target); err != nil {
		srv.Close()
		return nil, err
	}
	m := &mounted{env: env, log: log, target: target, from: from, srv: srv,
		unmounted: make(chan struct{}), done: make(chan struct{})}
	ticks, stop := m.pollTicks()
	go m.supervise(ticks, stop)
	return m, nil
}

func runMount(run commandRunner, port int, from, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	defer cancel()
	if out, err := run(ctx, mountNFSPath, mountArgs(port, from, target)...); err != nil {
		return commandError("mount_nfs", err, out)
	}
	return nil
}

// commandError reports a failed tool with what it printed, which is where the reason is.
func commandError(tool string, err error, out []byte) error {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%s: %w: %s", tool, err, msg)
	}
	return fmt.Errorf("%s: %w", tool, err)
}

// mounted is a live mount. One goroutine, supervise, owns its end.
type mounted struct {
	env    environment
	log    *slog.Logger
	target string
	from   string
	srv    *server

	unmounted   chan struct{} // closed when umount succeeded
	unmountOnce sync.Once
	done        chan struct{}
	err         error // written before done is closed
}

var _ mount.Mounted = (*mounted)(nil)

// pollTicks returns the ticks that trigger a look at the mount table. A mount that is
// not found in the table right after it succeeded (for example because the table
// spells its path differently from the target) is never polled: reporting a mount as
// gone when it is not would stop the server under a live mount, and the only thing
// lost by not polling is noticing an unmount done from outside.
func (m *mounted) pollTicks() (<-chan time.Time, func()) {
	present, err := m.env.isMounted(m.target, m.from)
	if err != nil || !present {
		m.log.Warn("mount not found in the mount table; unmounts from outside will not be noticed",
			"target", m.target, "err", err)
		return nil, func() {}
	}
	return m.env.newTicker(pollInterval)
}

// Done is closed when the mount has ended.
func (m *mounted) Done() <-chan struct{} { return m.done }

// Err is nil after a clean unmount and the reason otherwise. Call it after Done.
func (m *mounted) Err() error { return m.err }

// Unmount runs umount, with -f when force is set, and returns after the server has
// stopped. Without force, umount refuses a mount that is in use.
func (m *mounted) Unmount(ctx context.Context, force bool) error {
	select {
	case <-m.done:
		return nil
	default:
	}
	args := []string{m.target}
	if force {
		args = []string{"-f", m.target}
	}
	if out, err := m.env.run(ctx, umountPath, args...); err != nil {
		select {
		case <-m.done: // it was unmounted from outside in the meantime
			return nil
		default:
		}
		return commandError("umount", err, out)
	}
	m.unmountOnce.Do(func() { close(m.unmounted) })
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for the server to stop: %w", ctx.Err())
	}
}

// supervise ends the mount when the server fails, the mount disappears or Unmount
// succeeded, then stops the server and closes done.
func (m *mounted) supervise(ticks <-chan time.Time, stopTicks func()) {
	defer close(m.done)
	defer stopTicks()
	m.err = m.wait(ticks)
	m.srv.Close()
}

func (m *mounted) wait(ticks <-chan time.Time) error {
	for {
		select {
		case <-m.srv.Done():
			return m.serverFailed()
		case <-m.unmounted:
			return nil
		case <-ticks:
			present, err := m.env.isMounted(m.target, m.from)
			if err != nil {
				m.log.Warn("read the mount table", "err", err)
			} else if !present {
				return nil
			}
		}
	}
}

// serverFailed clears the mount of a server that stopped on its own. A mount whose
// server is gone only ever returns I/O errors, so it is forced off instead of left
// behind.
func (m *mounted) serverFailed() error {
	ctx, cancel := context.WithTimeout(context.Background(), mountTimeout)
	defer cancel()
	var uerr error
	if out, err := m.env.run(ctx, umountPath, "-f", m.target); err != nil {
		uerr = commandError("umount", err, out)
	}
	return errors.Join(m.srv.Err(), uerr)
}

// mountTableHas reports whether the mount table lists an NFS mount of from on target.
func mountTableHas(getfsstat func([]syscall.Statfs_t, int) (int, error), target, from string) (bool, error) {
	n, err := getfsstat(nil, mntNoWait)
	if err != nil {
		return false, fmt.Errorf("count mounts: %w", err)
	}
	// A few spare entries cover a mount that appears between the two calls.
	table := make([]syscall.Statfs_t, n+4)
	n, err = getfsstat(table, mntNoWait)
	if err != nil {
		return false, fmt.Errorf("read mounts: %w", err)
	}
	return slices.ContainsFunc(table[:min(n, len(table))], func(st syscall.Statfs_t) bool {
		return isOurMount(&st, target, from)
	}), nil
}

func isOurMount(st *syscall.Statfs_t, target, from string) bool {
	return cString(st.Fstypename[:]) == "nfs" &&
		cString(st.Mntonname[:]) == target &&
		cString(st.Mntfromname[:]) == from
}

// cString converts a NUL-terminated C char array to a string.
func cString(chars []int8) string {
	b := make([]byte, 0, len(chars))
	for _, c := range chars {
		if c == 0 {
			break
		}
		b = append(b, byte(c)) //nolint:gosec // a C char is reinterpreted as a byte, not converted
	}
	return string(b)
}
