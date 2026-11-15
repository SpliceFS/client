package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

// ErrNotConfigured completes a sentence that starts with a target: the configuration has
// no mount there.
var ErrNotConfigured = errors.New("is not a mount in the configuration")

// selectMounts returns the mounts a run is responsible for: all of them when wanted is
// empty, otherwise the ones whose targets are in it, in configuration order. The targets
// in wanted are absolute and cleaned, as absTargets makes them.
func selectMounts(cfg *config.Config, wanted []string) ([]config.Mount, error) {
	if len(wanted) == 0 {
		return slices.Clone(cfg.Mounts), nil
	}
	selected := slices.DeleteFunc(slices.Clone(cfg.Mounts), func(mountCfg config.Mount) bool {
		return !slices.Contains(wanted, mountCfg.Target)
	})
	for _, t := range wanted {
		if !slices.ContainsFunc(selected, func(mountCfg config.Mount) bool { return mountCfg.Target == t }) {
			return nil, fmt.Errorf("%s %w", t, ErrNotConfigured)
		}
	}
	return selected, nil
}

func (s system) absTargets(targets []string) ([]string, error) {
	abs := make([]string, 0, len(targets))
	for _, t := range targets {
		p, err := s.absPath(t)
		if err != nil {
			return nil, err
		}
		abs = append(abs, p)
	}
	return abs, nil
}

// liveMount is a mount this run made.
type liveMount struct {
	target  string
	source  string
	mounted mount.Mounted
	// cfg is what the mount was made from, so that a reload can tell a changed mount
	// from an unchanged one.
	cfg config.Mount
	// fs is the view the mount serves, which the core releases when the mount has ended.
	fs core.FS

	// detached is set before a reload unmounts the mount, so that its watcher can tell a
	// mount the client ended from one that ended by itself.
	detached atomic.Bool

	// unmounting serializes unmounts: a stop request and an operator's umount may
	// arrive together, and a mount layer is not required to handle that.
	unmounting sync.Mutex

	// failures and pending are the health state, which only the run loop touches.
	// failures counts the checks in a row that failed. pending is the answer of a check
	// that is still waiting, or nil.
	failures int
	pending  chan error
}

func (l *liveMount) ended() bool {
	select {
	case <-l.mounted.Done():
		return true
	default:
		return false
	}
}

// unmount returns the error of the mount layer as it is: each caller says which target it
// was unmounting, and the text of the error goes to the client that asked for the unmount.
func (l *liveMount) unmount(ctx context.Context, force bool) error {
	l.unmounting.Lock()
	defer l.unmounting.Unlock()
	return l.mounted.Unmount(ctx, force) //nolint:wrapcheck // see above
}

// isBusy tells a refusal because the mount is in use from other failures. The FUSE
// layer wraps EBUSY; the macOS layer only has the text of umount, which says "busy".
func isBusy(err error) bool {
	return errors.Is(err, syscall.EBUSY) || strings.Contains(strings.ToLower(err.Error()), "busy")
}

// mountSet is the mounts of a run. It changes while the run goes on: a reload adds and
// removes mounts, and the watcher of a mount that ended removes it. The mutex guards the
// slice only; the mounts themselves guard their own unmounts.
type mountSet struct {
	mu     sync.Mutex
	mounts []*liveMount

	// release and log belong to the watchers; the set built by newMountSet has them.
	release func(core.FS)
	log     *slog.Logger
	// requested is the list of requested mounts. A watcher takes the mount that ended
	// off it. Nil when the run keeps no such list.
	requested *requestedStore
	// stopping is set before a stop begins to unmount, so that the mounts it ends stay
	// on the requested list whatever order their watchers run in.
	stopping atomic.Bool
	// watchers ends with stop, and endWatchers waits for it.
	watchers sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
}

func newMountSet(release func(core.FS), log *slog.Logger) *mountSet {
	return &mountSet{release: release, log: log, stop: make(chan struct{})}
}

// add puts a mount in the set and starts its watcher.
func (s *mountSet) add(live *liveMount) {
	s.mu.Lock()
	s.mounts = append(s.mounts, live)
	s.mu.Unlock()
	s.watchers.Go(func() { s.watch(live) })
}

// remove takes live out of the set if it is there. It compares pointers, because a reload
// may already have mounted something new at the same target.
func (s *mountSet) remove(live *liveMount) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.mounts, live); i >= 0 {
		s.mounts = slices.Delete(s.mounts, i, i+1)
	}
}

// list returns a copy of the mounts, so that the caller can use it without the mutex.
func (s *mountSet) list() []*liveMount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.mounts)
}

func (s *mountSet) find(target string) (*liveMount, bool) {
	for _, live := range s.list() {
		if live.target == target {
			return live, true
		}
	}
	return nil, false
}

// watch waits for live to end, whatever the cause: an unmount through the control socket,
// an unmount by the administrator or a reload. It then drops live from the set if it is
// still there and gives its view back to the core, which closes what a reload replaced
// once nothing uses it. A mount that ended by itself stays unmounted until the next
// reload or start.
func (s *mountSet) watch(live *liveMount) {
	select {
	case <-live.mounted.Done():
	case <-s.stop:
		return
	}
	s.remove(live)
	s.release(live.fs)
	s.dropRequested(live)
	if err := live.mounted.Err(); err != nil {
		s.log.Warn("mount ended with an error",
			slog.String("target", live.target), slog.Any("err", err))
		return
	}
	s.log.Info("mount ended", slog.String("target", live.target))
}

// dropRequested takes a requested mount that ended on its own off the requested list, so
// that it is not brought back. A mount ended by a stop stays on it, for the next start.
// One that a reload detached was already dealt with, and its target may be in use again.
func (s *mountSet) dropRequested(live *liveMount) {
	if s.stopping.Load() || live.detached.Load() {
		return
	}
	s.requested.remove(live.target)
}

// endWatchers stops the watchers and waits for them. The core is closed after that, which
// also gives back the views of mounts whose watcher did not get to it.
func (s *mountSet) endWatchers() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.watchers.Wait()
}

// notEnded returns the mounts that have not ended.
func notEnded(mounts []*liveMount) []*liveMount {
	var open []*liveMount
	for _, live := range mounts {
		if !live.ended() {
			open = append(open, live)
		}
	}
	return open
}

// unmountPending unmounts every one of mounts that has not ended and returns the
// failures.
func unmountPending(ctx context.Context, mounts []*liveMount, force bool) []error {
	var errs []error
	for _, live := range notEnded(mounts) {
		if err := live.unmount(ctx, force); err != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w", live.target, err))
		}
	}
	return errs
}

// stopAll takes every mount down. A stop request must always succeed, and a mount left
// behind blocks the next start, so a mount that refuses a polite unmount is forced. It
// returns why any mount could not be removed or ended with an error. The mounts are
// listed once, before any of them ends, because the watchers remove the ended ones.
//
// ctx is the context of the run, which has ended by now, so the stop keeps its values and
// not its cancellation.
func (s *mountSet) stopAll(ctx context.Context, log *slog.Logger, limits stopLimits) error {
	s.stopping.Store(true)
	ctx = context.WithoutCancel(ctx)
	all := s.list()
	gentle, cancelGentle := context.WithTimeout(ctx, limits.graceful)
	defer cancelGentle()
	for _, err := range unmountPending(gentle, all, false) {
		log.WarnContext(ctx, "unmount refused, forcing it", slog.Any("err", err))
	}
	return s.forceMounts(ctx, all, limits.force)
}

// forceAll unmounts what is left with force and waits for every mount to end. Like
// stopAll, it is not cut short by the cancellation of ctx.
func (s *mountSet) forceAll(ctx context.Context, limit time.Duration) error {
	s.stopping.Store(true)
	return s.forceMounts(context.WithoutCancel(ctx), s.list(), limit)
}

func (s *mountSet) forceMounts(ctx context.Context, mounts []*liveMount, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	errs := unmountPending(ctx, mounts, true)
	errs = append(errs, endErrors(ctx, mounts)...)
	s.endWatchers()
	return errors.Join(errs...)
}

// endErrors waits for each mount to end and reports the ones that ended with an error
// or did not end before ctx expired.
func endErrors(ctx context.Context, mounts []*liveMount) []error {
	var errs []error
	for _, live := range mounts {
		select {
		case <-live.mounted.Done():
			if err := live.mounted.Err(); err != nil {
				errs = append(errs, fmt.Errorf("mount %s: %w", live.target, err))
			}
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("mount %s did not stop: %w", live.target, ctx.Err()))
		}
	}
	return errs
}

// unmountTarget unmounts one mount for a control request.
func (s *mountSet) unmountTarget(ctx context.Context, target string, force bool) UnmountResult {
	live, ok := s.find(target)
	if !ok {
		return UnmountResult{Target: target, Result: ResultUnknown}
	}
	err := live.unmount(ctx, force)
	switch {
	case err == nil:
		return UnmountResult{Target: target, Result: ResultUnmounted}
	case !force && isBusy(err):
		return UnmountResult{Target: target, Result: ResultBusy}
	}
	return UnmountResult{Target: target, Result: ResultFailed, Error: err.Error()}
}

func (s *mountSet) report() []MountReport {
	mounts := s.list()
	reports := make([]MountReport, 0, len(mounts))
	for _, live := range mounts {
		state := StateMounted
		if live.ended() {
			state = StateEnded
		}
		reports = append(reports, MountReport{Target: live.target, Source: live.source, State: state})
	}
	return reports
}
