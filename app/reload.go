package app

import (
	"context"
	"errors"
	"os"
	"slices"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
)

// reloader applies the configuration again each time the run is told to. Its methods run
// in the goroutine of the run loop only, so a reload never overlaps another.
type reloader struct {
	cc   Core
	set  *mountSet
	opts Options
	// targets are the absolute targets the run was started with. Empty means every mount
	// of the configuration.
	targets []string
}

// wait applies the configuration for each value on signals until ctx ends. A closed
// channel is dropped, because receiving from it would never block again.
func (r *reloader) wait(ctx context.Context, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-signals:
			if !ok {
				signals = nil
				continue
			}
			r.reload(ctx)
		}
	}
}

// reload brings the mounts in line with the configuration on disk. A configuration that
// cannot be read or applied changes nothing, and a mount that cannot be changed does not
// hold up the others.
func (r *reloader) reload(ctx context.Context) {
	cfg, err := r.loadAndApply()
	if err != nil {
		r.opts.Logger.Error("configuration not applied", "err", err)
		return
	}
	wanted := wantedMounts(cfg, r.targets)
	r.detachStale(ctx, wanted)
	r.mountMissing(ctx, wanted)
	r.opts.Logger.Info("configuration applied", "mounts", len(r.set.list()))
}

func (r *reloader) loadAndApply() (*config.Config, error) {
	cfg, err := r.opts.Load()
	if err != nil {
		return nil, err
	}
	if err := r.cc.Apply(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
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
	limit, cancel := context.WithTimeout(ctx, r.opts.limits.force)
	defer cancel()
	if err := m.unmount(limit, true); err != nil {
		r.opts.Logger.Error("mount not detached", "target", m.target, "err", err)
		return
	}
	r.set.remove(m)
}

// mountMissing mounts every wanted mount that is not live.
func (r *reloader) mountMissing(ctx context.Context, wanted []config.Mount) {
	usable := make(map[string]bool)
	for _, w := range wanted {
		if ctx.Err() != nil {
			return
		}
		if _, live := r.set.find(w.Target); live {
			continue
		}
		ok, seen := usable[w.Cluster]
		if !seen {
			ok = r.connect(ctx, w.Cluster)
			usable[w.Cluster] = ok
		}
		if ok {
			r.mount(w)
		}
	}
}

// connect logs in to a cluster before its mounts are made. It reports whether they may be
// made: an unreachable cluster is only a warning, as at start, but any other failure,
// such as rejected credentials, skips the mounts of that cluster.
func (r *reloader) connect(ctx context.Context, cluster string) bool {
	err := connectOne(ctx, r.cc, cluster, r.opts.ConnectTimeout)
	switch {
	case err == nil:
	case errors.Is(err, core.ErrUnavailable):
		r.opts.Logger.Warn("cluster unreachable, mounting anyway", "cluster", cluster, "err", err)
	default:
		r.opts.Logger.Error("cluster not usable, its mounts are skipped", "cluster", cluster, "err", err)
		return false
	}
	return true
}

func (r *reloader) mount(m config.Mount) {
	live, err := mountOne(r.cc, m, r.opts)
	if err != nil {
		r.opts.Logger.Error("mount not made", "target", m.Target, "err", err)
		return
	}
	r.set.add(live)
}
