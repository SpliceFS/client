package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
)

// errStopping answers a mount request that the client could not serve before it stopped.
var errStopping = errors.New("the client is stopping")

// errNotMounted completes a sentence that starts with a target: a mount that was made did
// not stay.
var errNotMounted = errors.New("is not mounted")

// mountRequest is a mount asked for over the control socket, handed to the run loop.
type mountRequest struct {
	mount config.Requested
	// reply receives the outcome. It holds one value, so the run loop never waits for a
	// server that gave up.
	reply chan error
}

// reloader applies the configuration again each time the run is told to, and mounts what
// is requested over the control socket. Its methods run in the goroutine of the run loop
// only, so changes to the mounts never overlap.
type reloader struct {
	coreClient Core
	set        *mountSet
	opts       Options
	// targets are the absolute targets the run was started with. Empty means every mount
	// of the configuration.
	targets []string
	// store lists the requested mounts.
	store *requestedStore
	// last is the file configuration applied last, which a request is added to without
	// reading the files again.
	last *config.Config
	// requests delivers mount requests. Nil means none come.
	requests chan mountRequest
}

// wait applies the configuration for each value on signals, and each mount request, and
// checks the health of the mounts on each tick, until ctx ends. A closed channel is
// dropped, because receiving from it would never block again.
func (r *reloader) wait(ctx context.Context, signals <-chan os.Signal) {
	source := r.opts.newTicker(probeInterval)
	defer source.stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-source.ticks:
			r.healthRound(ctx)
		case req := <-r.requests:
			req.reply <- r.request(ctx, req.mount)
		case _, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			r.reload(ctx)
		}
	}
}

// reload brings the mounts in line with the configuration on disk and the requested
// mounts that are still mounted. A configuration that cannot be read or applied changes
// nothing, and a mount that cannot be changed does not hold up the others.
func (r *reloader) reload(ctx context.Context) {
	loaded, err := r.loadEffective()
	if err == nil {
		err = r.apply(loaded.effective)
	}
	if err != nil {
		r.opts.Logger.ErrorContext(ctx, "configuration not applied", slog.Any("err", err))
		return
	}
	r.last = loaded.files
	r.converge(ctx, loaded.effective)
	r.opts.Logger.InfoContext(ctx, "configuration applied", slog.Int("mounts", len(r.set.list())))
}

// loadedConfig is a configuration read for a reload.
type loadedConfig struct {
	// files is what the configuration files say.
	files *config.Config
	// effective is files with the requested mounts that are still mounted added.
	effective *config.Config
}

func (r *reloader) loadEffective() (loadedConfig, error) {
	files, err := r.opts.Load()
	if err != nil {
		return loadedConfig{}, err
	}
	effective, err := r.effective(files, r.mountedRequests())
	return loadedConfig{files: files, effective: effective}, err
}

// apply hands the configuration to the core client. The message goes to the log and over the
// control socket as the core package wrote it.
func (r *reloader) apply(eff *config.Config) error {
	//nolint:wrapcheck // The core package says what was wrong, and a prefix would change the message.
	return r.coreClient.Apply(eff)
}

// mountedRequests returns the requested mounts that are still mounted. One that ended
// is not wanted any more, and is taken off the list in case its watcher is late.
func (r *reloader) mountedRequests() []config.Requested {
	var mounted []config.Requested
	for _, req := range r.store.list() {
		if live, found := r.set.find(filepath.Clean(req.Target)); found && !live.ended() {
			mounted = append(mounted, req)
		} else {
			r.store.remove(req.Target)
		}
	}
	return mounted
}

// effective adds the requested mounts to cfg. If that is invalid only because of them,
// for example their cluster was removed, they are dropped one at a time, the last asked
// first, until it is valid. The normal stale check detaches their live mounts.
func (r *reloader) effective(cfg *config.Config, reqs []config.Requested) (*config.Config, error) {
	if len(reqs) == 0 {
		// Load already validated the files, so there is nothing to add or check.
		return cfg, nil
	}
	eff, err := withRequested(cfg, reqs...)
	if _, fileErr := withRequested(cfg); err == nil || fileErr != nil {
		return eff, err
	}
	return r.dropUntilValid(cfg, reqs, err)
}

// withRequested adds the requested mounts to cfg, as config.WithMounts does. The message goes
// over the control socket and to the log as the config package wrote it.
func withRequested(cfg *config.Config, reqs ...config.Requested) (*config.Config, error) {
	//nolint:wrapcheck // The config package names the mount and what was wrong, a prefix would change it.
	return config.WithMounts(cfg, reqs...)
}

// dropUntilValid drops requested mounts, the last asked first, until the rest is valid
// with cfg, which is valid alone, so that this ends at the latest with none left.
func (r *reloader) dropUntilValid(cfg *config.Config, reqs []config.Requested,
	err error,
) (*config.Config, error) {
	for len(reqs) > 0 {
		last := reqs[len(reqs)-1]
		reqs = reqs[:len(reqs)-1]
		r.opts.Logger.Warn("requested mount dropped",
			slog.String("target", last.Target), slog.Any("err", err))
		r.store.remove(last.Target)
		var eff *config.Config
		if eff, err = withRequested(cfg, reqs...); err == nil {
			return eff, nil
		}
	}
	return nil, err
}

// request mounts one requested mount. It stays on the list only if it is mounted
// afterwards, and the error says why it is not.
func (r *reloader) request(ctx context.Context, req config.Requested) error {
	// The request is checked against the files and the other requested mounts before it is
	// remembered, so that an invalid one answers why and changes nothing. A request for a
	// target that was requested before replaces the earlier one.
	others := slices.DeleteFunc(r.store.list(), func(o config.Requested) bool {
		return filepath.Clean(o.Target) == filepath.Clean(req.Target)
	})
	if _, err := withRequested(r.last, append(others, req)...); err != nil {
		return err
	}
	r.store.add(req)
	err := r.applyRequested(ctx, req)
	if err != nil {
		r.store.remove(req.Target)
	}
	return err
}

func (r *reloader) applyRequested(ctx context.Context, req config.Requested) error {
	eff, err := r.effective(r.last, r.store.list())
	if err == nil {
		err = r.apply(eff)
	}
	if err != nil {
		return err
	}
	failed := r.converge(ctx, eff)
	target := filepath.Clean(req.Target)
	if err := failed[target]; err != nil {
		return err
	}
	if live, found := r.set.find(target); !found || live.ended() {
		return fmt.Errorf("%s %w", target, errNotMounted)
	}
	return nil
}

// restoreRequested mounts the requested mounts left by an earlier run, the way a request
// is. One that fails is logged and dropped.
func (r *reloader) restoreRequested(ctx context.Context) {
	for _, req := range r.store.list() {
		if err := r.request(ctx, req); err != nil {
			r.opts.Logger.WarnContext(ctx, "requested mount dropped",
				slog.String("target", req.Target), slog.Any("err", err))
		}
	}
}

// converge detaches what is stale and mounts what is missing. It returns why the mounts
// that could not be made were not, by target.
func (r *reloader) converge(ctx context.Context, eff *config.Config) map[string]error {
	wanted := r.wanted(eff)
	r.detachStale(ctx, wanted)
	return r.mountMissing(ctx, wanted)
}

// wanted limits the mounts of the files to the targets of the run. The requested mounts,
// which follow them, are always wanted.
func (r *reloader) wanted(eff *config.Config) []config.Mount {
	own := len(r.last.Mounts)
	files := wantedMounts(&config.Config{Mounts: eff.Mounts[:own]}, r.targets)
	return slices.Concat(files, eff.Mounts[own:])
}

// wantedMounts limits the configured mounts to the targets the run was started with. A
// started target that is no longer configured is simply not wanted any more.
func wantedMounts(cfg *config.Config, targets []string) []config.Mount {
	if len(targets) == 0 {
		return cfg.Mounts
	}
	return slices.DeleteFunc(slices.Clone(cfg.Mounts), func(mountCfg config.Mount) bool {
		return !slices.Contains(targets, mountCfg.Target)
	})
}

// detachStale unmounts every live mount that is no longer wanted, was changed, or serves a
// cluster or cache that Apply replaced.
func (r *reloader) detachStale(ctx context.Context, wanted []config.Mount) {
	for _, live := range r.set.list() {
		if r.upToDate(live, wanted) {
			continue
		}
		r.detach(ctx, live)
	}
}

func (r *reloader) upToDate(live *liveMount, wanted []config.Mount) bool {
	if live.ended() {
		return false
	}
	i := slices.IndexFunc(wanted, func(mountCfg config.Mount) bool { return mountCfg.Target == live.target })
	return i >= 0 && wanted[i] == live.cfg && r.coreClient.Current(live.fs)
}

// detach takes live out of the set at once, so that the mount made for the new
// configuration can use its target, and unmounts it. Its watcher gives the view back when
// it has really ended. An unmount that fails leaves live in place.
func (r *reloader) detach(ctx context.Context, live *liveMount) {
	if live.ended() {
		r.set.remove(live)
		return
	}
	live.detached.Store(true)
	limit, cancel := context.WithTimeout(ctx, r.opts.limits.force)
	defer cancel()
	if err := live.unmount(limit, true); err != nil {
		r.opts.Logger.ErrorContext(ctx, "mount not detached",
			slog.String("target", live.target), slog.Any("err", err))
		return
	}
	r.set.remove(live)
}

// mountMissing mounts every wanted mount that is not live and returns the reason for each
// one that could not be made, by target.
func (r *reloader) mountMissing(ctx context.Context, wanted []config.Mount) map[string]error {
	failed := make(map[string]error)
	usable := make(map[string]error)
	for i := range wanted {
		if ctx.Err() != nil {
			return failed
		}
		mountCfg := &wanted[i]
		if _, found := r.set.find(mountCfg.Target); found {
			continue
		}
		clusterErr, seen := usable[mountCfg.Cluster]
		if !seen {
			clusterErr = r.connect(ctx, mountCfg.Cluster)
			usable[mountCfg.Cluster] = clusterErr
		}
		if clusterErr == nil {
			clusterErr = r.mount(*mountCfg)
		}
		if clusterErr != nil {
			failed[mountCfg.Target] = clusterErr
		}
	}
	return failed
}

// connect logs in to a cluster before its mounts are made. It returns nil when they may
// be made: an unreachable cluster is only a warning, as at start, but any other failure,
// such as rejected credentials, skips the mounts of that cluster.
func (r *reloader) connect(ctx context.Context, cluster string) error {
	err := connectOne(ctx, r.coreClient, cluster, r.opts.ConnectTimeout)
	switch {
	case err == nil:
	case errors.Is(err, core.ErrUnavailable):
		r.opts.Logger.WarnContext(ctx, "cluster unreachable, mounting anyway",
			slog.String("cluster", cluster), slog.Any("err", err))
		return nil
	default:
		r.opts.Logger.ErrorContext(ctx, "cluster not usable, its mounts are skipped",
			slog.String("cluster", cluster), slog.Any("err", err))
	}
	return err
}

func (r *reloader) mount(mountCfg config.Mount) error {
	live, err := mountOne(r.coreClient, mountCfg, r.opts)
	if err != nil {
		r.opts.Logger.Error("mount not made",
			slog.String("target", mountCfg.Target), slog.Any("err", err))
		return err
	}
	r.set.add(live)
	return nil
}
