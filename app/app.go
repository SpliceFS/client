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

// ErrReloadWithoutLoad means Options.Reload was set without Options.Load, so a reload would
// have nothing to read.
var ErrReloadWithoutLoad = errors.New("reload needs a function that loads the configuration")

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

// newCoreClient fills Options.NewCore, whose type returns the Core interface so that tests
// can replace the core client.
//
//nolint:ireturn // the NewCore function type it implements returns the interface
func newCoreClient(cfg *config.Config, opts core.Options) (Core, error) {
	client, err := core.New(cfg, opts)
	if err != nil {
		return nil, err //nolint:wrapcheck // Run adds "start client: ", the core package says the rest.
	}
	return client, nil
}

// Options are the parts of a run that do not come from the configuration file.
type Options struct {
	// Logger receives the client's log. Nil discards it.
	Logger *slog.Logger
	// Mounter is the mount layer of this operating system. It must not be nil.
	Mounter Mounter
	// Notify tells the service manager "READY=1", "WATCHDOG=1" and "STOPPING=1". Nil does
	// nothing.
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

	// The health round: where its ticks come from, how it asks a mount for the probe name,
	// and how long and how often a mount may fail before it is counted as hung.
	newTicker     func(time.Duration) tickSource
	lstat         func(string) (os.FileInfo, error)
	probeTimeout  time.Duration
	probeFailures int
}

// stopLimits bound the two steps of a stop.
type stopLimits struct {
	graceful time.Duration
	force    time.Duration
}

func (o Options) withDefaults() Options {
	return o.withPublicDefaults().withHostDefaults()
}

// withPublicDefaults fills the options that a caller of Run may leave out.
func (o Options) withPublicDefaults() Options {
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
	return o
}

// withHostDefaults fills the options that only tests replace: what the run takes from the
// operating system, and the limits of the stop and of the health round.
func (o Options) withHostDefaults() Options {
	if o.sys == nil {
		sys := systemHost()
		o.sys = &sys
	}
	if o.limits == (stopLimits{}) {
		o.limits = stopLimits{graceful: gracefulStopTimeout, force: forceStopTimeout}
	}
	if o.newTicker == nil {
		o.newTicker = systemTicker
	}
	if o.lstat == nil {
		o.lstat = os.Lstat
	}
	if o.probeTimeout == 0 {
		o.probeTimeout = defaultProbeTimeout
	}
	if o.probeFailures == 0 {
		o.probeFailures = defaultProbeFailures
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
		return ErrReloadWithoutLoad
	}
	socketPath, err := opts.controlSocket()
	if err != nil {
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
	sock, err := opts.sys.claim(ctx, opts.SocketPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sock.Close()) }()
	return serve(ctx, sock, cfg, selection{targets: abs, mounts: selected}, opts)
}

// selection is what a run was asked to mount: the mounts, and the absolute targets that
// were named, which are empty when every mount was asked for.
type selection struct {
	targets []string
	mounts  []config.Mount
}

// serve builds the core client, mounts the selection and keeps it mounted until ctx ends.
// The control socket is already claimed.
func serve(ctx context.Context, sock *socket, cfg *config.Config, chosen selection,
	opts Options,
) (err error) {
	coreClient, err := opts.NewCore(cfg, core.Options{Logger: opts.Logger})
	if err != nil {
		return fmt.Errorf("start client: %w", err)
	}
	defer func() { err = errors.Join(err, coreClient.Close()) }()

	if err = connectClusters(ctx, coreClient, chosen.mounts, opts); err != nil {
		if ctx.Err() != nil {
			// Being told to stop while still connecting is a successful stop.
			return nil
		}
		return err
	}
	store := newRequestedStore(requestedPath(opts.SocketPath), opts.Logger)
	store.load()
	set, err := mountAll(ctx, coreClient, chosen.mounts, store, opts)
	if err != nil {
		return err
	}
	run := &reloader{
		coreClient: coreClient, set: set, opts: opts, targets: chosen.targets, store: store,
		last: cfg, requests: make(chan mountRequest),
	}
	return supervise(ctx, run, sock)
}

// mountAll mounts every selected mount. If one fails, the ones already mounted are
// unmounted with force, because a mount left behind would block the next start.
func mountAll(ctx context.Context, coreClient Core, selected []config.Mount,
	store *requestedStore, opts Options,
) (*mountSet, error) {
	set := newMountSet(coreClient.Release, opts.Logger)
	// Set before the first watcher starts, which reads it.
	set.requested = store
	for i := range selected {
		live, err := mountOne(coreClient, selected[i], opts)
		if err != nil {
			return nil, errors.Join(err, set.forceAll(ctx, opts.limits.force))
		}
		set.add(live)
	}
	return set, nil
}

func mountOne(coreClient Core, mountCfg config.Mount, opts Options) (*liveMount, error) {
	fsys, err := coreClient.View(mountCfg)
	if err != nil {
		return nil, err //nolint:wrapcheck // The core package names the mount and what was wrong.
	}
	source := "splicefs:" + mountCfg.Cluster + mountCfg.Path
	mounted, err := opts.Mounter(fsys, mount.Options{
		Target:     mountCfg.Target,
		Source:     source,
		AllowOther: mountCfg.AllowOther,
		Logger:     opts.Logger,
		RecordPath: nfsRecordPath(opts.SocketPath),
	})
	if err != nil {
		// The mount never existed, so nothing else will give the view back.
		coreClient.Release(fsys)
		return nil, fmt.Errorf("mount %s: %w", mountCfg.Target, err)
	}
	return &liveMount{
		target: mountCfg.Target, source: source, mounted: mounted, cfg: mountCfg, fs: fsys,
	}, nil
}

// supervise reports readiness, keeps the control socket going and applies reloads until
// the context ends, then stops everything.
func supervise(ctx context.Context, run *reloader, sock *socket) error {
	opts := run.opts
	ctl := startControl(ctx, sock, run.set, run.coreClient, opts.Logger, run.requests)
	run.restoreRequested(ctx)
	if err := opts.Notify("READY=1"); err != nil {
		ctl.stop()
		return errors.Join(fmt.Errorf("tell the service manager the client is ready: %w", err),
			run.set.forceAll(ctx, opts.limits.force))
	}
	run.wait(ctx, opts.Reload)

	if err := opts.Notify("STOPPING=1"); err != nil {
		opts.Logger.WarnContext(ctx, "could not tell the service manager the client is stopping",
			slog.Any("err", err))
	}
	ctl.stop()
	return run.set.stopAll(ctx, opts.Logger, opts.limits)
}

// connectClusters logs in to every cluster the selected mounts use, all at once, each
// within the connect timeout. Rejected credentials are fatal: a wrong password should
// fail loudly and not produce a mount full of I/O errors. An unreachable cluster is only
// a warning.
func connectClusters(ctx context.Context, coreClient Core, selected []config.Mount,
	opts Options,
) error {
	names := clusterNames(selected)
	results := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { results[i] = connectOne(ctx, coreClient, name, opts.ConnectTimeout) })
	}
	wg.Wait()
	for i, err := range results {
		switch {
		case err == nil:
		case errors.Is(err, core.ErrUnavailable):
			opts.Logger.WarnContext(ctx, "cluster unreachable, mounting anyway",
				slog.String("cluster", names[i]), slog.Any("err", err))
		default:
			return err
		}
	}
	return nil
}

func connectOne(ctx context.Context, coreClient Core, name string, timeout time.Duration) error {
	attempt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := coreClient.Connect(attempt, name)
	if err != nil && attempt.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("connect to cluster %q: %w: %w", name, core.ErrUnavailable, err)
	}
	return err //nolint:wrapcheck // The core package already starts the message with the cluster.
}

func clusterNames(selected []config.Mount) []string {
	set := make(map[string]struct{})
	for i := range selected {
		set[selected[i].Cluster] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}
