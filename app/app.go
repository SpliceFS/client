// Package app is the composition root of the SpliceFS client program: it builds the core
// client from a configuration, mounts the configured directories through the mount layer
// of the operating system, serves a control socket for the umount and status commands,
// and unmounts everything on shutdown. The commands in client/cmd/splicefs are thin
// wrappers over it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

const (
	// defaultConnectTimeout is how long a start waits for one cluster. At boot the
	// network may not be up, and the mount recovers by itself later, so the wait is
	// short.
	defaultConnectTimeout = 15 * time.Second
	// gracefulStopTimeout is how long a stop waits for mounts to unmount without
	// force, before forcing them.
	gracefulStopTimeout = 5 * time.Second
	// forceStopTimeout bounds the forced unmount and the wait for the mounts to end.
	forceStopTimeout = 30 * time.Second
)

// Mounter mounts fsys at opts.Target with the mount layer of the operating system and
// returns once the mount is usable.
type Mounter func(fsys core.FS, opts mount.Options) (mount.Mounted, error)

// Core is the part of the core client that Run uses. *core.Client implements it.
type Core interface {
	// Connect logs in to a cluster and fetches its nodes.
	Connect(ctx context.Context, cluster string) error
	// View returns the filesystem of one mount.
	View(m config.Mount) (core.FS, error)
	// Status reports clusters, nodes and cache use without using the network.
	Status() core.Status
	// Apply makes the client follow a new configuration, or changes nothing and returns
	// an error.
	Apply(cfg *config.Config) error
	// Release says that the mount that used fs has ended.
	Release(fs core.FS)
	// Current reports whether fs still uses the clusters and caches Apply would hand
	// out for the same mount now.
	Current(fs core.FS) bool
	// Close releases the clusters and caches.
	Close() error
}

// NewCore builds a Core for a configuration.
type NewCore func(cfg *config.Config, opts core.Options) (Core, error)

func newCoreClient(cfg *config.Config, opts core.Options) (Core, error) {
	c, err := core.New(cfg, opts)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Options are the parts of a run that do not come from the configuration file.
type Options struct {
	// Logger receives the client's log. Nil discards it.
	Logger *slog.Logger
	// Mounter is the mount layer of this operating system. It must not be nil.
	Mounter Mounter
	// Notify tells the service manager "READY=1" and "STOPPING=1". Nil does nothing.
	Notify func(state string) error
	// ConfigDir is the directory the configuration was loaded from. It names the
	// control socket when SocketPath is empty. Empty means the default configuration
	// directory, config.DefaultConfigDir.
	ConfigDir string
	// SocketPath is the control socket. Empty derives it from ConfigDir with
	// SocketPath.
	SocketPath string
	// ConnectTimeout is how long Run waits for each cluster at start. Zero means 15s.
	ConnectTimeout time.Duration
	// NewCore builds the core client. Nil uses core.New.
	NewCore NewCore
	// Reload delivers a value each time the configuration is to be applied again. Nil
	// means it never is.
	Reload <-chan os.Signal
	// Load reads the configuration again for a reload. It must be set when Reload is.
	Load func() (*config.Config, error)

	sys    *system
	limits stopLimits
}

// stopLimits bound the two steps of a stop.
type stopLimits struct {
	graceful time.Duration
	force    time.Duration
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Notify == nil {
		o.Notify = NotifyFunc("")
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = defaultConnectTimeout
	}
	if o.NewCore == nil {
		o.NewCore = newCoreClient
	}
	if o.sys == nil {
		sys := systemHost()
		o.sys = &sys
	}
	if o.limits == (stopLimits{}) {
		o.limits = stopLimits{graceful: gracefulStopTimeout, force: forceStopTimeout}
	}
	return o
}

// controlSocket returns SocketPath, or the socket of ConfigDir when that is empty. The
// default configuration directory is looked up only here, because finding it can fail and
// withDefaults has no way to report that.
func (o Options) controlSocket() (string, error) {
	if o.SocketPath != "" {
		return o.SocketPath, nil
	}
	dir := o.ConfigDir
	if dir == "" {
		var err error
		if dir, err = o.sys.configDir(); err != nil {
			return "", err
		}
	}
	return o.sys.socketPath(dir), nil
}

// Run mounts the directories of cfg named by targets, or all of them when targets is
// empty, and keeps going until ctx is cancelled. It then unmounts whatever is live, with
// force if it has to, and returns nil. A mount that ends earlier, because it was
// unmounted through the control socket or by the administrator, stays unmounted until
// the next reload, and the run goes on, with no mounts at all if need be. A mount asked
// for over the control socket (OpMount) is kept in a list next to the socket, mounted
// again at the next start, and dropped when it ends while the run goes on. Each value on
// opts.Reload reads the configuration again with opts.Load, applies it to the core and
// then unmounts the mounts that were removed or changed and mounts the ones that are
// missing; a failure there is logged and the mounts stay as they are. It returns an
// error if the selection is invalid, another client already uses the control socket, a
// cluster rejects the credentials, or a mount cannot be made (the earlier ones are
// unmounted again). An unreachable cluster is only logged, because the mounts recover
// once it is back. When it returns, everything it started has stopped.
func Run(ctx context.Context, cfg *config.Config, targets []string, opts Options) (err error) {
	opts = opts.withDefaults()
	if opts.Reload != nil && opts.Load == nil {
		return errors.New("reload needs a function that loads the configuration")
	}
	var socketPath string
	if socketPath, err = opts.controlSocket(); err != nil {
		return err
	}
	opts.SocketPath = socketPath
	abs, err := opts.sys.absTargets(targets)
	if err != nil {
		return err
	}
	selected, err := selectMounts(cfg, abs)
	if err != nil {
		return err
	}
	// The socket is claimed before anything is mounted, so that a second client for the
	// same configuration fails here and not after it has mounted over the first.
	sock, err := opts.sys.claim(opts.SocketPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sock.Close()) }()

	cc, err := opts.NewCore(cfg, core.Options{Logger: opts.Logger})
	if err != nil {
		return fmt.Errorf("start client: %w", err)
	}
	defer func() { err = errors.Join(err, cc.Close()) }()

	if err := connectClusters(ctx, cc, selected, opts); err != nil {
		if ctx.Err() != nil {
			// Being told to stop while still connecting is a successful stop.
			return nil
		}
		return err
	}
	store := newRequestedStore(requestedPath(opts.SocketPath), opts.Logger)
	store.load()
	set, err := mountAll(cc, selected, store, opts)
	if err != nil {
		return err
	}
	r := &reloader{
		cc: cc, set: set, opts: opts, targets: abs, store: store, last: cfg,
		requests: make(chan mountRequest),
	}
	return supervise(ctx, r, sock)
}

// mountAll mounts every selected mount. If one fails, the ones already mounted are
// unmounted with force, because a mount left behind would block the next start.
func mountAll(cc Core, selected []config.Mount, store *requestedStore,
	opts Options) (*mountSet, error) {
	set := newMountSet(cc.Release, opts.Logger)
	// Set before the first watcher starts, which reads it.
	set.requested = store
	for _, m := range selected {
		live, err := mountOne(cc, m, opts)
		if err != nil {
			return nil, errors.Join(err, set.forceAll(opts.limits.force))
		}
		set.add(live)
	}
	return set, nil
}

func mountOne(cc Core, m config.Mount, opts Options) (*liveMount, error) {
	fsys, err := cc.View(m)
	if err != nil {
		return nil, err
	}
	source := "splicefs:" + m.Cluster + m.Path
	mounted, err := opts.Mounter(fsys, mount.Options{
		Target:     m.Target,
		Source:     source,
		AllowOther: m.AllowOther,
		Logger:     opts.Logger,
	})
	if err != nil {
		// The mount never existed, so nothing else will give the view back.
		cc.Release(fsys)
		return nil, fmt.Errorf("mount %s: %w", m.Target, err)
	}
	return &liveMount{target: m.Target, source: source, mounted: mounted, cfg: m, fs: fsys}, nil
}

// supervise reports readiness, keeps the control socket going and applies reloads until
// the context ends, then stops everything.
func supervise(ctx context.Context, r *reloader, sock *socket) error {
	opts := r.opts
	ctl := startControl(sock, r.set, r.cc, opts.Logger, r.requests)
	r.restoreRequested(ctx)
	if err := opts.Notify("READY=1"); err != nil {
		ctl.stop()
		return errors.Join(fmt.Errorf("tell the service manager the client is ready: %w", err),
			r.set.forceAll(opts.limits.force))
	}
	r.wait(ctx, opts.Reload)

	if err := opts.Notify("STOPPING=1"); err != nil {
		opts.Logger.Warn("could not tell the service manager the client is stopping", "err", err)
	}
	ctl.stop()
	return r.set.stopAll(opts.Logger, opts.limits)
}

// connectClusters logs in to every cluster the selected mounts use, all at once, each
// within the connect timeout. Rejected credentials are fatal: a wrong password should
// fail loudly and not produce a mount full of I/O errors. An unreachable cluster is only
// a warning.
func connectClusters(ctx context.Context, cc Core, selected []config.Mount, opts Options) error {
	names := clusterNames(selected)
	results := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { results[i] = connectOne(ctx, cc, name, opts.ConnectTimeout) })
	}
	wg.Wait()
	for i, err := range results {
		switch {
		case err == nil:
		case errors.Is(err, core.ErrUnavailable):
			opts.Logger.Warn("cluster unreachable, mounting anyway", "cluster", names[i], "err", err)
		default:
			return err
		}
	}
	return nil
}

func connectOne(ctx context.Context, cc Core, name string, timeout time.Duration) error {
	attempt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := cc.Connect(attempt, name)
	if err != nil && attempt.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("connect to cluster %q: %w: %w", name, core.ErrUnavailable, err)
	}
	return err
}

func clusterNames(selected []config.Mount) []string {
	set := make(map[string]struct{})
	for _, m := range selected {
		set[m.Cluster] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}
