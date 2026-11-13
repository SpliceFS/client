package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

// reloadTimeout bounds every wait for the run loop, so that a broken reload fails the
// test instead of hanging it.
const reloadTimeout = 5 * time.Second

// The mounts that the reload tests ask for. They are functions so that no test can change
// a value that the others use.
func reloadA() config.Mount {
	return config.Mount{Cluster: "example", Path: "/media", Target: "/mnt/a"}
}

func reloadA2() config.Mount {
	return config.Mount{Cluster: "example", Path: "/other", Target: "/mnt/a"}
}

func reloadB() config.Mount {
	return config.Mount{Cluster: "example", Path: "/", Target: "/mnt/b"}
}

func reloadC() config.Mount {
	return config.Mount{Cluster: "other", Path: "/docs", Target: "/mnt/c"}
}

func reloadD() config.Mount {
	return config.Mount{Cluster: "bad", Path: "/", Target: "/mnt/d"}
}

func reloadE() config.Mount {
	return config.Mount{Cluster: "bad", Path: "/x", Target: "/mnt/e"}
}

// reloadRig is a running client that a test tells to reload.
type reloadRig struct {
	env     *harness
	run     *running
	signals chan os.Signal

	mu      sync.Mutex
	next    *config.Config
	nextErr error
	loads   int
	// onLoad, if set, runs inside every Load.
	onLoad func()
}

// newReloadRig starts a client with mounts, and with targets if there are any.
func newReloadRig(t *testing.T, mounts []config.Mount, targets ...string) *reloadRig {
	t.Helper()
	rig := &reloadRig{env: newHarness(t), signals: make(chan os.Signal)}
	rig.env.cfg = &config.Config{Mounts: mounts}
	rig.env.opts.Reload = rig.signals
	rig.env.opts.Load = rig.load
	rig.run = rig.env.start(targets...)
	return rig
}

func (rig *reloadRig) load() (*config.Config, error) {
	rig.mu.Lock()
	rig.loads++
	cfg, err, hook := rig.next, rig.nextErr, rig.onLoad
	rig.mu.Unlock()
	if hook != nil {
		hook()
	}
	return cfg, err
}

func (rig *reloadRig) loadCount() int {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rig.loads
}

// finished counts the reloads that have logged their outcome.
func (rig *reloadRig) finished() int {
	out := rig.env.log.String()
	return strings.Count(out, "configuration applied") +
		strings.Count(out, "configuration not applied")
}

// signal sends one reload request. The channel has no buffer, so it returns once the run
// loop has taken the request, which is after the previous reload ended.
func (rig *reloadRig) signal() {
	rig.env.t.Helper()
	select {
	case rig.signals <- syscall.SIGHUP:
	case <-time.After(reloadTimeout):
		rig.env.t.Fatal("the run did not take the reload request")
	}
}

// reloadWith makes Load return cfg and err, asks for a reload and waits until it is done.
func (rig *reloadRig) reloadWith(cfg *config.Config, err error) {
	rig.env.t.Helper()
	done := rig.finished()
	rig.mu.Lock()
	rig.next, rig.nextErr = cfg, err
	rig.mu.Unlock()
	rig.signal()
	waitFor(rig.env.t, "the reload to finish", func() bool { return rig.finished() > done })
}

func (rig *reloadRig) reload(mounts ...config.Mount) *config.Config {
	rig.env.t.Helper()
	cfg := &config.Config{Mounts: mounts}
	rig.reloadWith(cfg, nil)
	return cfg
}

// markStale makes the core report the views of these targets as replaced.
func (rig *reloadRig) markStale(targets ...string) {
	rig.env.core.mu.Lock()
	defer rig.env.core.mu.Unlock()
	rig.env.core.stale = make(map[string]bool)
	for _, target := range targets {
		rig.env.core.stale[target] = true
	}
}

// live returns the targets the client reports as mounted, sorted.
func (rig *reloadRig) live() []string {
	rig.env.t.Helper()
	live := []string{}
	for _, m := range rig.env.status().Mounts {
		if m.State == StateMounted {
			live = append(live, m.Target)
		}
	}
	slices.Sort(live)
	return live
}

func (rig *reloadRig) wantLive(want ...string) {
	rig.env.t.Helper()
	if got := rig.live(); !slices.Equal(got, append([]string{}, want...)) {
		rig.env.t.Errorf("live mounts = %v, want %v", got, want)
	}
}

func (rig *reloadRig) wantMountCalls(want ...string) {
	rig.env.t.Helper()
	if got := rig.env.mounter.targets(); !slices.Equal(got, want) {
		rig.env.t.Errorf("mount calls = %v, want %v", got, want)
	}
}

// mountsStarted returns the mounts that were started so far, by target.
func (rig *reloadRig) mountsStarted() map[string]*fakeMounted {
	started := make(map[string]*fakeMounted)
	for _, target := range rig.env.mounter.targets() {
		started[target] = rig.env.mounter.get(target)
	}
	return started
}

// wantForcedUnmounts checks that the mounts in forced were unmounted once with force, and the others not at all.
func (rig *reloadRig) wantForcedUnmounts(started map[string]*fakeMounted, forced []string) {
	rig.env.t.Helper()
	for target, mounted := range started {
		want := []bool(nil)
		if slices.Contains(forced, target) {
			want = []bool{true}
		}
		if got := mounted.unmountCalls(); !slices.Equal(got, want) {
			rig.env.t.Errorf("%s unmount calls (force) = %v, want %v", target, got, want)
		}
	}
}

// wantAppliedOnce checks that the core was given cfg, and nothing else.
func (rig *reloadRig) wantAppliedOnce(cfg *config.Config) {
	rig.env.t.Helper()
	rig.env.core.mu.Lock()
	applied := slices.Clone(rig.env.core.applied)
	rig.env.core.mu.Unlock()
	if len(applied) != 1 || applied[0] != cfg {
		rig.env.t.Errorf("applied %v, want the loaded configuration once", applied)
	}
}

func (rig *reloadRig) wantLogged(parts ...string) {
	rig.env.t.Helper()
	out := rig.env.log.String()
	for _, part := range parts {
		if !strings.Contains(out, part) {
			rig.env.t.Errorf("log lacks %q:\n%s", part, out)
		}
	}
}

func (rig *reloadRig) stop() {
	rig.env.t.Helper()
	if err := rig.run.stop(); err != nil {
		rig.env.t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
}

func TestRunRejectsReloadWithoutLoad(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.opts.Reload = make(chan os.Signal)
	err := env.runSync()
	if err == nil || !strings.Contains(err.Error(), "reload needs") {
		t.Fatalf("Run = %v, want an error that Load is missing", err)
	}
	if got := env.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v", got)
	}
}

func TestRunWithoutMountsIdles(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.cfg.Mounts = nil
	r := env.start()
	select {
	case err := <-r.result:
		t.Fatalf("Run ended with %v, want it to idle until cancelled", err)
	default:
	}
	if got := len(env.status().Mounts); got != 0 {
		t.Errorf("report lists %d mounts, want none", got)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if got := env.notes.got(); !slices.Equal(got, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("notifications = %v", got)
	}
}

func TestRunReportsAMountThatEndsWhileStopping(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start("/mnt/a")
	lost := errors.New("connection to the kernel was lost")
	m := env.mounter.get("/mnt/a")
	m.setBehavior(func(bool) error {
		m.end(lost)
		return nil
	})
	err := r.stop()
	errIs(t, err, lost)
	if !strings.Contains(err.Error(), "mount /mnt/a") {
		t.Errorf("error does not name the target: %v", err)
	}
}

func TestReloadChangesTheMounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		start   []config.Mount
		targets []string
		stale   []string
		next    []config.Mount
		// wantMounted lists every mount call, the first ones from the start.
		wantMounted []string
		wantLive    []string
		// wantForced were unmounted with force, the rest of the started mounts not at all.
		wantForced   []string
		wantReleased int
	}{
		{
			name: "new mount", start: []config.Mount{reloadA()},
			next:        []config.Mount{reloadA(), reloadB()},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{"/mnt/a", "/mnt/b"},
		},
		{
			name: "removed mount", start: []config.Mount{reloadA(), reloadB()},
			next:        []config.Mount{reloadA()},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{"/mnt/a"},
			wantForced: []string{"/mnt/b"}, wantReleased: 1,
		},
		{
			name: "every mount removed", start: []config.Mount{reloadA(), reloadB()},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{},
			wantForced: []string{"/mnt/a", "/mnt/b"}, wantReleased: 2,
		},
		{
			name: "changed mount", start: []config.Mount{reloadA(), reloadB()},
			next:        []config.Mount{reloadA2(), reloadB()},
			wantMounted: []string{"/mnt/a", "/mnt/b", "/mnt/a"},
			wantLive:    []string{"/mnt/a", "/mnt/b"}, wantForced: []string{"/mnt/a"},
			wantReleased: 1,
		},
		{
			name: "unchanged mounts", start: []config.Mount{reloadA(), reloadB()},
			next:        []config.Mount{reloadA(), reloadB()},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{"/mnt/a", "/mnt/b"},
		},
		{
			name: "cluster or cache replaced", start: []config.Mount{reloadA(), reloadB()},
			stale: []string{"/mnt/a"}, next: []config.Mount{reloadA(), reloadB()},
			wantMounted: []string{"/mnt/a", "/mnt/b", "/mnt/a"},
			wantLive:    []string{"/mnt/a", "/mnt/b"}, wantForced: []string{"/mnt/a"},
			wantReleased: 1,
		},
		{
			name: "only the started targets", start: []config.Mount{reloadA(), reloadB(), reloadC()},
			targets:     []string{"/mnt/a"},
			next:        []config.Mount{reloadA(), reloadB(), reloadC()},
			wantMounted: []string{"/mnt/a"}, wantLive: []string{"/mnt/a"},
		},
		{
			name: "started target no longer configured", start: []config.Mount{reloadA(), reloadB()},
			targets:     []string{"/mnt/a"},
			next:        []config.Mount{reloadB(), reloadC()},
			wantMounted: []string{"/mnt/a"}, wantLive: []string{},
			wantForced: []string{"/mnt/a"}, wantReleased: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newReloadRig(t, tt.start, tt.targets...)
			before := rig.mountsStarted()
			rig.markStale(tt.stale...)
			cfg := rig.reload(tt.next...)

			rig.wantMountCalls(tt.wantMounted...)
			rig.wantLive(tt.wantLive...)
			rig.wantForcedUnmounts(before, tt.wantForced)
			waitFor(t, "the ended mounts to be released", func() bool {
				return rig.env.core.releasedCount() == tt.wantReleased
			})
			rig.wantAppliedOnce(cfg)
			rig.wantLogged("configuration applied", fmt.Sprintf("mounts=%d", len(tt.wantLive)))
			rig.stop()
		})
	}
}

func TestReloadRemountsWhatTheAdministratorUnmounted(t *testing.T) {
	t.Parallel()
	rig := newReloadRig(t, []config.Mount{reloadA(), reloadB()})
	rig.env.mounter.get("/mnt/a").end(nil)
	waitFor(t, "the view to be released", func() bool { return rig.env.core.releasedCount() == 1 })
	rig.reload(reloadA(), reloadB())
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/a")
	rig.wantLive("/mnt/a", "/mnt/b")
	if calls := rig.env.mounter.get("/mnt/b").unmountCalls(); len(calls) != 0 {
		t.Errorf("the intact mount was unmounted: %v", calls)
	}
	rig.stop()
}

func TestReloadKeepsTheConfigurationWhenItCannotBeUsed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		loadErr  error
		applyErr error
	}{
		{name: "load fails", loadErr: errors.New("example.yaml: bad yaml")},
		{name: "apply fails", applyErr: errors.New("cache directory is not writable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newReloadRig(t, []config.Mount{reloadA()})
			rig.env.core.applyErr = tt.applyErr
			rig.reloadWith(&config.Config{Mounts: []config.Mount{reloadB()}}, tt.loadErr)

			rig.wantMountCalls("/mnt/a")
			rig.wantLive("/mnt/a")
			if calls := rig.env.mounter.get("/mnt/a").unmountCalls(); len(calls) != 0 {
				t.Errorf("the mount was unmounted: %v", calls)
			}
			want := tt.loadErr
			if want == nil {
				want = tt.applyErr
			}
			rig.wantLogged("configuration not applied", want.Error())
			if strings.Contains(rig.env.log.String(), "configuration applied") {
				t.Error("a configuration that was not applied is reported as applied")
			}
			rig.stop()
		})
	}
}

func TestReloadKeepsAMountThatCannotBeDetached(t *testing.T) {
	t.Parallel()
	rig := newReloadRig(t, []config.Mount{reloadA()})
	stuck := errors.New("device is stuck")
	mounted := rig.env.mounter.get("/mnt/a")
	mounted.setBehavior(func(bool) error { return stuck })
	rig.reload(reloadA2())

	rig.wantLogged("mount not detached", "target=/mnt/a", stuck.Error(), "configuration applied")
	rig.wantMountCalls("/mnt/a")
	rig.wantLive("/mnt/a")
	// The stop has to be able to remove the mount.
	mounted.setBehavior(nil)
	rig.stop()
	if calls := mounted.unmountCalls(); !slices.Equal(calls, []bool{true, false}) {
		t.Errorf("unmount calls (force) = %v, want the forced detach, then the stop", calls)
	}
}

func TestReloadConnectsToNewClusters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantLive []string
		wantLog  string
	}{
		{
			name:     "rejected credentials skip the cluster",
			err:      fmt.Errorf("connect: %w", core.ErrBadCredentials),
			wantLive: []string{"/mnt/a", "/mnt/c"},
			wantLog:  "cluster not usable, its mounts are skipped",
		},
		{
			name:     "unreachable cluster mounts anyway",
			err:      fmt.Errorf("connect: %w", core.ErrUnavailable),
			wantLive: []string{"/mnt/a", "/mnt/c", "/mnt/d", "/mnt/e"},
			wantLog:  "cluster unreachable, mounting anyway",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newReloadRig(t, []config.Mount{reloadA()})
			rig.env.core.connectErr["bad"] = tt.err
			rig.reload(reloadA(), reloadD(), reloadE(), reloadC())

			rig.wantLive(tt.wantLive...)
			rig.wantLogged(tt.wantLog, "cluster=bad", "configuration applied")
			// One connection per new cluster, and none for the one that is mounted.
			want := []string{"bad", "example", "other"}
			if got := rig.env.core.connectedClusters(); !slices.Equal(got, want) {
				t.Errorf("connected to %v, want %v", got, want)
			}
			rig.stop()
		})
	}
}

func TestReloadSkipsAMountThatCannotBeMade(t *testing.T) {
	t.Parallel()
	rig := newReloadRig(t, []config.Mount{reloadA()})
	boom := errors.New("fuse is not available")
	rig.env.mounter.fail["/mnt/b"] = boom
	rig.reload(reloadA(), reloadB(), reloadC())

	rig.wantLive("/mnt/a", "/mnt/c")
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	rig.wantLogged("mount not made", "target=/mnt/b", boom.Error(), "configuration applied")
	// The mount never existed, so the failed attempt gives its view back at once.
	waitFor(t, "the failed view to be released", func() bool {
		return rig.env.core.releasedCount() == 1
	})
	rig.stop()
}

func TestReloadSeveralTimes(t *testing.T) {
	t.Parallel()
	rig := newReloadRig(t, []config.Mount{reloadA()})
	mountedA := rig.env.mounter.get("/mnt/a")
	rig.reload(reloadA(), reloadB())
	rig.reload(reloadB(), reloadC())
	mountedC := rig.env.mounter.get("/mnt/c")
	rig.reload(reloadB(), reloadC())

	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	rig.wantLive("/mnt/b", "/mnt/c")
	if calls := mountedA.unmountCalls(); !slices.Equal(calls, []bool{true}) {
		t.Errorf("/mnt/a unmount calls (force) = %v, want one", calls)
	}
	for _, mounted := range []*fakeMounted{rig.env.mounter.get("/mnt/b"), mountedC} {
		if calls := mounted.unmountCalls(); len(calls) != 0 {
			t.Errorf("a mount that stayed was unmounted: %v", calls)
		}
	}
	if got := rig.finished(); got != 3 {
		t.Errorf("%d reloads finished, want 3", got)
	}
	rig.stop()
}

func TestRunIgnoresAClosedReloadChannel(t *testing.T) {
	t.Parallel()
	rig := newReloadRig(t, []config.Mount{reloadA()})
	close(rig.signals)
	// A closed channel always has a value to receive, so a loop that did not drop it
	// would reload without end or never look at the context again.
	if got := rig.live(); !slices.Equal(got, []string{"/mnt/a"}) {
		t.Errorf("live mounts = %v", got)
	}
	rig.stop()
	if got := rig.loadCount(); got != 0 {
		t.Errorf("the configuration was loaded %d times after the channel closed", got)
	}
}

func TestReloadEndingWithTheContext(t *testing.T) {
	t.Parallel()
	rig := newReloadRig(t, []config.Mount{reloadA()})
	mountedA := rig.env.mounter.get("/mnt/a")
	// Load runs inside the reload, so the context ends in the middle of it.
	rig.mu.Lock()
	rig.onLoad = rig.run.cancel
	rig.mu.Unlock()
	rig.reload(reloadA(), reloadB())

	rig.wantMountCalls("/mnt/a")
	if err := rig.run.wait(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if calls := mountedA.unmountCalls(); !slices.Equal(calls, []bool{false}) {
		t.Errorf("unmount calls (force) = %v, want the stop's polite unmount", calls)
	}
}

func TestReloadReplacesAMountThatEndedButIsStillListed(t *testing.T) {
	t.Parallel()
	// The watcher of an ended mount removes it a moment later, so a reload can meet it in
	// the set. A set without watchers makes that moment last.
	log := &logBuffer{}
	coreClient := newFakeCore()
	mounter := newFakeMounter()
	set := newMountSet(coreClient.Release, log.logger())
	t.Cleanup(set.endWatchers)
	gone := newFakeMounted()
	gone.end(nil)
	set.mounts = []*liveMount{{
		target: reloadA().Target, mounted: gone, cfg: reloadA(), fs: namedFS{name: reloadA().Target},
	}}
	r := &reloader{coreClient: coreClient, set: set, opts: Options{
		Logger:         log.logger(),
		Mounter:        mounter.Mount,
		ConnectTimeout: reloadTimeout,
		limits:         stopLimits{graceful: reloadTimeout, force: reloadTimeout},
		Load: func() (*config.Config, error) {
			return &config.Config{Mounts: []config.Mount{reloadA()}}, nil
		},
	}}
	r.reload(context.Background())

	if got := mounter.targets(); !slices.Equal(got, []string{"/mnt/a"}) {
		t.Errorf("mount calls = %v, want the mount made again", got)
	}
	live := set.list()
	if len(live) != 1 || live[0].mounted == mount.Mounted(gone) {
		t.Errorf("the set holds %v, want only the new mount", live)
	}
	if calls := gone.unmountCalls(); len(calls) != 0 {
		t.Errorf("an ended mount was unmounted: %v", calls)
	}
}

func TestReportShowsAMountThatEnded(t *testing.T) {
	t.Parallel()
	ended := newFakeMounted()
	ended.end(nil)
	set := &mountSet{mounts: []*liveMount{
		{target: "/mnt/a", source: "splicefs:example/media", mounted: ended},
		{target: "/mnt/b", source: "splicefs:example/", mounted: newFakeMounted()},
	}}
	got := set.report()
	want := []MountReport{
		{Target: "/mnt/a", Source: "splicefs:example/media", State: StateEnded},
		{Target: "/mnt/b", Source: "splicefs:example/", State: StateMounted},
	}
	if !slices.Equal(got, want) {
		t.Errorf("report = %+v, want %+v", got, want)
	}
}
