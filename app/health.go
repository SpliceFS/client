package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/SpliceFS/client/mount"
)

const (
	// probeInterval is the time between two health rounds. A hung mount is found within
	// probeFailures rounds, and the service manager hears from the run loop every round.
	probeInterval = 10 * time.Second
	// defaultProbeTimeout is how long a round waits for the answer of one mount.
	defaultProbeTimeout = 5 * time.Second
	// defaultProbeFailures is how many checks in a row a mount may fail before it is
	// counted as hung. One slow answer must not end a working mount.
	defaultProbeFailures = 3
)

// systemTicker is the default source of ticks, shaped as a function so that tests can
// replace it with ticks they send by hand.
func systemTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// startCheck asks for the probe name in the root of target in a goroutine of its own, so
// that a mount that never answers holds up nothing but that goroutine. The channel has
// room for the answer, so the goroutine ends whenever the answer comes, even if nobody
// waits for it any more.
func startCheck(lstat func(string) (os.FileInfo, error), target string) chan error {
	answer := make(chan error, 1)
	go func() {
		_, err := lstat(filepath.Join(target, mount.ProbeName))
		answer <- err
	}()
	return answer
}

// healthRound checks every live mount, mounts again the ones that are hung, and then
// tells the service manager that the run loop is alive. It runs in the run loop, so
// nothing else changes the mounts meanwhile.
func (r *reloader) healthRound(ctx context.Context) {
	if r.checkMounts(ctx) {
		r.mountAgain(ctx)
	}
	// The heartbeat proves the run loop is running, not that every mount is healthy, so it
	// is sent whatever the round found.
	if err := r.opts.Notify("WATCHDOG=1"); err != nil {
		r.opts.Logger.Debug("could not send the heartbeat to the service manager", "err", err)
	}
}

// checkMounts checks the mounts in the order of the set and detaches each one that has
// failed too many checks in a row. It returns whether any was found hung, which is when
// the missing mounts are to be made. It stops early when ctx ends, because an answer not
// waited for is no failure of the mount.
func (r *reloader) checkMounts(ctx context.Context) (hung bool) {
	for _, m := range r.set.list() {
		passed := r.check(ctx, m)
		if ctx.Err() != nil {
			return hung
		}
		if passed {
			m.failures = 0
			continue
		}
		m.failures++
		if m.failures >= r.opts.probeFailures {
			hung = true
			r.opts.Logger.Error("mount does not answer, mounting it again", "target", m.target)
			// detach marks the mount, so that a requested mount stays on the requested list.
			r.detach(ctx, m)
			r.abortHung(m)
		}
	}
	return hung
}

// abortHung cuts the kernel connection of a hung mount that detach has taken out of the
// set, if its mount layer can. The detach alone leaves the requests that already wait on
// a mount that does not answer waiting for good. Only a hung mount is aborted: a reload
// or a stop detaches working mounts too, and their open files must be able to finish. A
// mount still in the set was not detached, so it is left as it is. A failed abort changes
// nothing else, because the mount is detached and is made again either way.
func (r *reloader) abortHung(m *liveMount) {
	aborter, ok := m.mounted.(mount.Aborter)
	if !ok || slices.Contains(r.set.list(), m) {
		return
	}
	if err := aborter.Abort(); err != nil {
		r.opts.Logger.Warn("could not abort the connection of a hung mount",
			"target", m.target, "err", err)
	}
}

// check reports whether m answered "does not exist" for the probe name in time. Any
// other answer, or none, is a failure. A check that is still waiting from an earlier
// round fails the mount without a second one, so that a hung mount does not collect a
// goroutine per round.
func (r *reloader) check(ctx context.Context, m *liveMount) bool {
	if m.pending != nil {
		select {
		case <-m.pending: //nolint:errcheck // an old answer, discarded on purpose
			// It answered after its round stopped waiting. That answer is old, so a new
			// check is made.
			m.pending = nil
		default:
			return false
		}
	}
	m.pending = startCheck(r.opts.lstat, m.target)
	timer := time.NewTimer(r.opts.probeTimeout)
	defer timer.Stop()
	select {
	case err := <-m.pending:
		m.pending = nil
		return errors.Is(err, fs.ErrNotExist)
	case <-timer.C:
	case <-ctx.Done():
	}
	return false
}

// mountAgain mounts what is missing from the configuration applied last and the requested
// mounts, the same step that a reload and a mount request end with. The files are not read
// again, because a hung mount is no reason to believe they changed.
func (r *reloader) mountAgain(ctx context.Context) {
	eff, err := r.effective(r.last, r.store.list())
	if err != nil {
		r.opts.Logger.Error("hung mounts not made again", "err", err)
		return
	}
	r.mountMissing(ctx, r.wanted(eff))
}
