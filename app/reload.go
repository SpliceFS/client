package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
)

// errStopping answers a mount request that the client could not serve before it stopped.
var errStopping = errors.New("the client is stopping")

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
	cc   Core
	set  *mountSet
	opts Options
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

// wait applies the configuration for each value on signals, and each mount request, until
// ctx ends. A closed channel is dropped, because receiving from it would never block
// again.
func (r *reloader) wait(ctx context.Context, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
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
	cfg, eff, err := r.loadEffective()
	if err == nil {
		err = r.cc.Apply(eff)
	}
	if err != nil {
		r.opts.Logger.Error("configuration not applied", "err", err)
		return
	}
	r.last = cfg
	r.converge(ctx, eff)
	r.opts.Logger.Info("configuration applied", "mounts", len(r.set.list()))
}

func (r *reloader) loadEffective() (cfg, eff *config.Config, err error) {
	if cfg, err = r.opts.Load(); err != nil {
		return nil, nil, err
	}
	eff, err = r.effective(cfg, r.mountedRequests())
	return cfg, eff, err
}

// mountedRequests returns the requested mounts that are still mounted. One that ended
// is not wanted any more, and is taken off the list in case its watcher is late.
func (r *reloader) mountedRequests() []config.Requested {
	var mounted []config.Requested
	for _, req := range r.store.list() {
		if m, live := r.set.find(filepath.Clean(req.Target)); live && !m.ended() {
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
	eff, err := config.WithMounts(cfg, reqs...)
	if _, fileErr := config.WithMounts(cfg); err == nil || fileErr != nil {
		return eff, err
	}
	return r.dropUntilValid(cfg, reqs, err)
}

// dropUntilValid drops requested mounts, the last asked first, until the rest is valid
// with cfg, which is valid alone, so that this ends at the latest with none left.
func (r *reloader) dropUntilValid(cfg *config.Config, reqs []config.Requested,
	err error) (*config.Config, error) {
	for len(reqs) > 0 {
		last := reqs[len(reqs)-1]
		reqs = reqs[:len(reqs)-1]
		r.opts.Logger.Warn("requested mount dropped", "target", last.Target, "err", err)
		r.store.remove(last.Target)
		var eff *config.Config
		if eff, err = config.WithMounts(cfg, reqs...); err == nil {
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
	if _, err := config.WithMounts(r.last, append(others, req)...); err != nil {
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
		err = r.cc.Apply(eff)
	}
	if err != nil {
		return err
	}
	failed := r.converge(ctx, eff)
	target := filepath.Clean(req.Target)
	if err := failed[target]; err != nil {
		return err
	}
	if m, live := r.set.find(target); !live || m.ended() {
		return fmt.Errorf("%s is not mounted", target)
	}
	return nil
}

// restoreRequested mounts the requested mounts left by an earlier run, the way a request
// is. One that fails is logged and dropped.
func (r *reloader) restoreRequested(ctx context.Context) {
	for _, req := range r.store.list() {
		if err := r.request(ctx, req); err != nil {
			r.opts.Logger.Warn("requested mount dropped", "target", req.Target, "err", err)
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
	return slices.DeleteFunc(slices.Clone(cfg.Mounts), func(m config.Mount) bool {
		return !slices.Contains(targets, m.Target)
	})
}

// detachStale unmounts every live mount that is no longer wanted, was changed, or serves a
// cluster or cache that Apply replaced.
func (r *reloader) detachStale(ctx context.Context, wanted []config.Mount) {
	for _, m := range r.set.list() {
		if r.upToDate(m, wanted) {
			continue
		}
		r.detach(ctx, m)
	}
}

func (r *reloader) upToDate(m *liveMount, wanted []config.Mount) bool {
	if m.ended() {
		return false
	}
	i := slices.IndexFunc(wanted, func(w config.Mount) bool { return w.Target == m.target })
	return i >= 0 && wanted[i] == m.cfg && r.cc.Current(m.fs)
}

// detach takes m out of the set at once, so that the mount made for the new
// configuration can use its target, and unmounts it. Its watcher gives the view back when
// it has really ended. An unmount that fails leaves m in place.
func (r *reloader) detach(ctx context.Context, m *liveMount) {
	if m.ended() {
		r.set.remove(m)
		return
	}
	m.detached.Store(true)
	limit, cancel := context.WithTimeout(ctx, r.opts.limits.force)
	defer cancel()
	if err := m.unmount(limit, true); err != nil {
		r.opts.Logger.Error("mount not detached", "target", m.target, "err", err)
		return
	}
	r.set.remove(m)
}

// mountMissing mounts every wanted mount that is not live and returns the reason for each
// one that could not be made, by target.
func (r *reloader) mountMissing(ctx context.Context, wanted []config.Mount) map[string]error {
	failed := make(map[string]error)
	usable := make(map[string]error)
	for _, w := range wanted {
		if ctx.Err() != nil {
			return failed
		}
		if _, live := r.set.find(w.Target); live {
			continue
		}
		clusterErr, seen := usable[w.Cluster]
		if !seen {
			clusterErr = r.connect(ctx, w.Cluster)
			usable[w.Cluster] = clusterErr
		}
		if clusterErr == nil {
			clusterErr = r.mount(w)
		}
		if clusterErr != nil {
			failed[w.Target] = clusterErr
		}
	}
	return failed
}

// connect logs in to a cluster before its mounts are made. It returns nil when they may
// be made: an unreachable cluster is only a warning, as at start, but any other failure,
// such as rejected credentials, skips the mounts of that cluster.
func (r *reloader) connect(ctx context.Context, cluster string) error {
	err := connectOne(ctx, r.cc, cluster, r.opts.ConnectTimeout)
	switch {
	case err == nil:
	case errors.Is(err, core.ErrUnavailable):
		r.opts.Logger.Warn("cluster unreachable, mounting anyway", "cluster", cluster, "err", err)
		return nil
	default:
		r.opts.Logger.Error("cluster not usable, its mounts are skipped", "cluster", cluster, "err", err)
	}
	return err
}

func (r *reloader) mount(m config.Mount) error {
	live, err := mountOne(r.cc, m, r.opts)
	if err != nil {
		r.opts.Logger.Error("mount not made", "target", m.Target, "err", err)
		return err
	}
	r.set.add(live)
	return nil
}
