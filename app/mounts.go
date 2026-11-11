package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BlueRival/splicefs/client/config"
	"github.com/BlueRival/splicefs/client/mount"
)

// selectMounts returns the mounts a run is responsible for: all of them when targets is
// empty, otherwise the named ones in configuration order. Targets are compared as
// cleaned absolute paths.
func (s system) selectMounts(cfg *config.Config, targets []string) ([]config.Mount, error) {
	if len(cfg.Mounts) == 0 {
		return nil, errors.New("the configuration has no mounts")
	}
	if len(targets) == 0 {
		return slices.Clone(cfg.Mounts), nil
	}
	wanted, err := s.absTargets(targets)
	if err != nil {
		return nil, err
	}
	var selected []config.Mount
	for _, m := range cfg.Mounts {
		if slices.Contains(wanted, m.Target) {
			selected = append(selected, m)
		}
	}
	for _, t := range wanted {
		if !slices.ContainsFunc(selected, func(m config.Mount) bool { return m.Target == t }) {
			return nil, fmt.Errorf("%s is not a mount in the configuration", t)
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

	// unmounting serializes unmounts: a stop request and an operator's umount may
	// arrive together, and a mount layer is not required to handle that.
	unmounting sync.Mutex
}

func (l *liveMount) ended() bool {
	select {
	case <-l.mounted.Done():
		return true
	default:
		return false
	}
}

func (l *liveMount) unmount(ctx context.Context, force bool) error {
	l.unmounting.Lock()
	defer l.unmounting.Unlock()
	return l.mounted.Unmount(ctx, force)
}

// isBusy tells a refusal because the mount is in use from other failures. The FUSE
// layer wraps EBUSY; the macOS layer only has the text of umount, which says "busy".
func isBusy(err error) bool {
	return errors.Is(err, syscall.EBUSY) || strings.Contains(strings.ToLower(err.Error()), "busy")
}

// mountSet is the mounts of a run. The slice does not change after it is built.
type mountSet struct {
	mounts []*liveMount
}

func (s *mountSet) find(target string) (*liveMount, bool) {
	for _, m := range s.mounts {
		if m.target == target {
			return m, true
		}
	}
	return nil, false
}

// pending returns the mounts that have not ended.
func (s *mountSet) pending() []*liveMount {
	var open []*liveMount
	for _, m := range s.mounts {
		if !m.ended() {
			open = append(open, m)
		}
	}
	return open
}

// watchEnd returns a channel that is closed once every mount has ended, and a function
// that waits for the goroutines doing the watching. They stop when stop is closed.
func (s *mountSet) watchEnd(stop <-chan struct{}) (<-chan struct{}, func()) {
	all := make(chan struct{})
	var mu sync.Mutex
	remaining := len(s.mounts)
	var wg sync.WaitGroup
	for _, m := range s.mounts {
		wg.Go(func() {
			select {
			case <-m.mounted.Done():
				mu.Lock()
				remaining--
				last := remaining == 0
				mu.Unlock()
				if last {
					close(all)
				}
			case <-stop:
			}
		})
	}
	return all, wg.Wait
}

// unmountPending unmounts every mount that has not ended and returns the failures.
func (s *mountSet) unmountPending(ctx context.Context, force bool) []error {
	var errs []error
	for _, m := range s.pending() {
		if err := m.unmount(ctx, force); err != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w", m.target, err))
		}
	}
	return errs
}

// stopAll takes every mount down. A stop request must always succeed, and a mount left
// behind blocks the next start, so a mount that refuses a polite unmount is forced. It
// returns why any mount could not be removed or ended with an error.
func (s *mountSet) stopAll(log *slog.Logger, limits stopLimits) error {
	gentle, cancelGentle := context.WithTimeout(context.Background(), limits.graceful)
	defer cancelGentle()
	for _, err := range s.unmountPending(gentle, false) {
		log.Warn("unmount refused, forcing it", "err", err)
	}
	return s.forceAll(limits.force)
}

// forceAll unmounts what is left with force and waits for every mount to end.
func (s *mountSet) forceAll(limit time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	errs := s.unmountPending(ctx, true)
	return errors.Join(append(errs, s.endErrors(ctx)...)...)
}

// endErrors waits for each mount to end and reports the ones that ended with an error
// or did not end before ctx expired.
func (s *mountSet) endErrors(ctx context.Context) []error {
	var errs []error
	for _, m := range s.mounts {
		select {
		case <-m.mounted.Done():
			if err := m.mounted.Err(); err != nil {
				errs = append(errs, fmt.Errorf("mount %s: %w", m.target, err))
			}
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("mount %s did not stop: %w", m.target, ctx.Err()))
		}
	}
	return errs
}

// unmountTarget unmounts one mount for a control request.
func (s *mountSet) unmountTarget(ctx context.Context, target string, force bool) UnmountResult {
	m, ok := s.find(target)
	if !ok {
		return UnmountResult{Target: target, Result: ResultUnknown}
	}
	err := m.unmount(ctx, force)
	switch {
	case err == nil:
		return UnmountResult{Target: target, Result: ResultUnmounted}
	case !force && isBusy(err):
		return UnmountResult{Target: target, Result: ResultBusy}
	}
	return UnmountResult{Target: target, Result: ResultFailed, Error: err.Error()}
}

func (s *mountSet) report() []MountReport {
	reports := make([]MountReport, 0, len(s.mounts))
	for _, m := range s.mounts {
		state := StateMounted
		if m.ended() {
			state = StateEnded
		}
		reports = append(reports, MountReport{Target: m.target, Source: m.source, State: state})
	}
	return reports
}
