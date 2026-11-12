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

var (
	reloadA  = config.Mount{Cluster: "example", Path: "/media", Target: "/mnt/a"}
	reloadA2 = config.Mount{Cluster: "example", Path: "/other", Target: "/mnt/a"}
	reloadB  = config.Mount{Cluster: "example", Path: "/", Target: "/mnt/b"}
	reloadC  = config.Mount{Cluster: "other", Path: "/docs", Target: "/mnt/c"}
	reloadD  = config.Mount{Cluster: "bad", Path: "/", Target: "/mnt/d"}
	reloadE  = config.Mount{Cluster: "bad", Path: "/x", Target: "/mnt/e"}
)

// reloadRig is a running client that a test tells to reload.
type reloadRig struct {
	h       *harness
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
	g := &reloadRig{h: newHarness(t), signals: make(chan os.Signal)}
	g.h.cfg = &config.Config{Mounts: mounts}
	g.h.opts.Reload = g.signals
	g.h.opts.Load = g.load
	g.run = g.h.start(targets...)
	return g
}

func (g *reloadRig) load() (*config.Config, error) {
	g.mu.Lock()
	g.loads++
	cfg, err, hook := g.next, g.nextErr, g.onLoad
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	return cfg, err
}

func (g *reloadRig) loadCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.loads
}

// finished counts the reloads that have logged their outcome.
func (g *reloadRig) finished() int {
	out := g.h.log.String()
	return strings.Count(out, "configuration applied") +
		strings.Count(out, "configuration not applied")
}

// signal sends one reload request. The channel has no buffer, so it returns once the run
// loop has taken the request, which is after the previous reload ended.
func (g *reloadRig) signal() {
	g.h.t.Helper()
	select {
	case g.signals <- syscall.SIGHUP:
	case <-time.After(reloadTimeout):
		g.h.t.Fatal("the run did not take the reload request")
	}
}

// reloadWith makes Load return cfg and err, asks for a reload and waits until it is done.
func (g *reloadRig) reloadWith(cfg *config.Config, err error) {
	g.h.t.Helper()
	done := g.finished()
	g.mu.Lock()
	g.next, g.nextErr = cfg, err
	g.mu.Unlock()
	g.signal()
	waitFor(g.h.t, "the reload to finish", func() bool { return g.finished() > done })
}

func (g *reloadRig) reload(mounts ...config.Mount) *config.Config {
	g.h.t.Helper()
	cfg := &config.Config{Mounts: mounts}
	g.reloadWith(cfg, nil)
	return cfg
}

// markStale makes the core report the views of these targets as replaced.
func (g *reloadRig) markStale(targets ...string) {
	g.h.core.mu.Lock()
	defer g.h.core.mu.Unlock()
	g.h.core.stale = make(map[string]bool)
	for _, target := range targets {
		g.h.core.stale[target] = true
	}
}

// live returns the targets the client reports as mounted, sorted.
func (g *reloadRig) live() []string {
	g.h.t.Helper()
	live := []string{}
	for _, m := range g.h.status().Mounts {
		if m.State == StateMounted {
			live = append(live, m.Target)
		}
	}
	slices.Sort(live)
	return live
}

func (g *reloadRig) wantLive(want ...string) {
	g.h.t.Helper()
	if got := g.live(); !slices.Equal(got, append([]string{}, want...)) {
		g.h.t.Errorf("live mounts = %v, want %v", got, want)
	}
}

func (g *reloadRig) wantMountCalls(want ...string) {
	g.h.t.Helper()
	if got := g.h.mounter.targets(); !slices.Equal(got, want) {
		g.h.t.Errorf("mount calls = %v, want %v", got, want)
	}
}

func (g *reloadRig) wantLogged(parts ...string) {
	g.h.t.Helper()
	out := g.h.log.String()
	for _, part := range parts {
		if !strings.Contains(out, part) {
			g.h.t.Errorf("log lacks %q:\n%s", part, out)
		}
	}
}

func (g *reloadRig) stop() {
	g.h.t.Helper()
	if err := g.run.stop(); err != nil {
		g.h.t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
}

func TestRunRejectsReloadWithoutLoad(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.opts.Reload = make(chan os.Signal)
	err := h.runSync()
	if err == nil || !strings.Contains(err.Error(), "reload needs") {
		t.Fatalf("Run = %v, want an error that Load is missing", err)
	}
	if got := h.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v", got)
	}
}

func TestRunWithoutMountsIdles(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Mounts = nil
	r := h.start()
	select {
	case err := <-r.result:
		t.Fatalf("Run ended with %v, want it to idle until cancelled", err)
	default:
	}
	if got := len(h.status().Mounts); got != 0 {
		t.Errorf("report lists %d mounts, want none", got)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if got := h.notes.got(); !slices.Equal(got, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("notifications = %v", got)
	}
}

func TestRunReportsAMountThatEndsWhileStopping(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start("/mnt/a")
	lost := errors.New("connection to the kernel was lost")
	m := h.mounter.get("/mnt/a")
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
			name: "new mount", start: []config.Mount{reloadA},
			next:        []config.Mount{reloadA, reloadB},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{"/mnt/a", "/mnt/b"},
		},
		{
			name: "removed mount", start: []config.Mount{reloadA, reloadB},
			next:        []config.Mount{reloadA},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{"/mnt/a"},
			wantForced: []string{"/mnt/b"}, wantReleased: 1,
		},
		{
			name: "every mount removed", start: []config.Mount{reloadA, reloadB},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{},
			wantForced: []string{"/mnt/a", "/mnt/b"}, wantReleased: 2,
		},
		{
			name: "changed mount", start: []config.Mount{reloadA, reloadB},
			next:        []config.Mount{reloadA2, reloadB},
			wantMounted: []string{"/mnt/a", "/mnt/b", "/mnt/a"},
			wantLive:    []string{"/mnt/a", "/mnt/b"}, wantForced: []string{"/mnt/a"},
			wantReleased: 1,
		},
		{
			name: "unchanged mounts", start: []config.Mount{reloadA, reloadB},
			next:        []config.Mount{reloadA, reloadB},
			wantMounted: []string{"/mnt/a", "/mnt/b"}, wantLive: []string{"/mnt/a", "/mnt/b"},
		},
		{
			name: "cluster or cache replaced", start: []config.Mount{reloadA, reloadB},
			stale: []string{"/mnt/a"}, next: []config.Mount{reloadA, reloadB},
			wantMounted: []string{"/mnt/a", "/mnt/b", "/mnt/a"},
			wantLive:    []string{"/mnt/a", "/mnt/b"}, wantForced: []string{"/mnt/a"},
			wantReleased: 1,
		},
		{
			name: "only the started targets", start: []config.Mount{reloadA, reloadB, reloadC},
			targets:     []string{"/mnt/a"},
			next:        []config.Mount{reloadA, reloadB, reloadC},
			wantMounted: []string{"/mnt/a"}, wantLive: []string{"/mnt/a"},
		},
		{
			name: "started target no longer configured", start: []config.Mount{reloadA, reloadB},
			targets:     []string{"/mnt/a"},
			next:        []config.Mount{reloadB, reloadC},
			wantMounted: []string{"/mnt/a"}, wantLive: []string{},
			wantForced: []string{"/mnt/a"}, wantReleased: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newReloadRig(t, tt.start, tt.targets...)
			before := make(map[string]*fakeMounted)
			for _, target := range g.h.mounter.targets() {
				before[target] = g.h.mounter.get(target)
			}
			g.markStale(tt.stale...)
			cfg := g.reload(tt.next...)

			g.wantMountCalls(tt.wantMounted...)
			g.wantLive(tt.wantLive...)
			for target, m := range before {
				want := []bool(nil)
				if slices.Contains(tt.wantForced, target) {
					want = []bool{true}
				}
				if got := m.unmountCalls(); !slices.Equal(got, want) {
					t.Errorf("%s unmount calls (force) = %v, want %v", target, got, want)
				}
			}
			waitFor(t, "the ended mounts to be released", func() bool {
				return g.h.core.releasedCount() == tt.wantReleased
			})
			g.h.core.mu.Lock()
			applied := slices.Clone(g.h.core.applied)
			g.h.core.mu.Unlock()
			if len(applied) != 1 || applied[0] != cfg {
				t.Errorf("applied %v, want the loaded configuration once", applied)
			}
			g.wantLogged("configuration applied", fmt.Sprintf("mounts=%d", len(tt.wantLive)))
			g.stop()
		})
	}
}

func TestReloadRemountsWhatTheAdministratorUnmounted(t *testing.T) {
	t.Parallel()
	g := newReloadRig(t, []config.Mount{reloadA, reloadB})
	g.h.mounter.get("/mnt/a").end(nil)
	waitFor(t, "the view to be released", func() bool { return g.h.core.releasedCount() == 1 })
	g.reload(reloadA, reloadB)
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/a")
	g.wantLive("/mnt/a", "/mnt/b")
	if calls := g.h.mounter.get("/mnt/b").unmountCalls(); len(calls) != 0 {
		t.Errorf("the intact mount was unmounted: %v", calls)
	}
	g.stop()
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
			g := newReloadRig(t, []config.Mount{reloadA})
			g.h.core.applyErr = tt.applyErr
			g.reloadWith(&config.Config{Mounts: []config.Mount{reloadB}}, tt.loadErr)

			g.wantMountCalls("/mnt/a")
			g.wantLive("/mnt/a")
			if calls := g.h.mounter.get("/mnt/a").unmountCalls(); len(calls) != 0 {
				t.Errorf("the mount was unmounted: %v", calls)
			}
			want := tt.loadErr
			if want == nil {
				want = tt.applyErr
			}
			g.wantLogged("configuration not applied", want.Error())
			if strings.Contains(g.h.log.String(), "configuration applied") {
				t.Errorf("a configuration that was not applied is reported as applied")
			}
			g.stop()
		})
	}
}

func TestReloadKeepsAMountThatCannotBeDetached(t *testing.T) {
	t.Parallel()
	g := newReloadRig(t, []config.Mount{reloadA})
	stuck := errors.New("device is stuck")
	m := g.h.mounter.get("/mnt/a")
	m.setBehavior(func(bool) error { return stuck })
	g.reload(reloadA2)

	g.wantLogged("mount not detached", "target=/mnt/a", stuck.Error(), "configuration applied")
	g.wantMountCalls("/mnt/a")
	g.wantLive("/mnt/a")
	// The stop has to be able to remove the mount.
	m.setBehavior(nil)
	g.stop()
	if calls := m.unmountCalls(); !slices.Equal(calls, []bool{true, false}) {
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
			g := newReloadRig(t, []config.Mount{reloadA})
			g.h.core.connectErr["bad"] = tt.err
			g.reload(reloadA, reloadD, reloadE, reloadC)

			g.wantLive(tt.wantLive...)
			g.wantLogged(tt.wantLog, "cluster=bad", "configuration applied")
			// One connection per new cluster, and none for the one that is mounted.
			want := []string{"bad", "example", "other"}
			if got := g.h.core.connectedClusters(); !slices.Equal(got, want) {
				t.Errorf("connected to %v, want %v", got, want)
			}
			g.stop()
		})
	}
}

func TestReloadSkipsAMountThatCannotBeMade(t *testing.T) {
	t.Parallel()
	g := newReloadRig(t, []config.Mount{reloadA})
	boom := errors.New("fuse is not available")
	g.h.mounter.fail["/mnt/b"] = boom
	g.reload(reloadA, reloadB, reloadC)

	g.wantLive("/mnt/a", "/mnt/c")
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	g.wantLogged("mount not made", "target=/mnt/b", boom.Error(), "configuration applied")
	// The mount never existed, so the failed attempt gives its view back at once.
	waitFor(t, "the failed view to be released", func() bool {
		return g.h.core.releasedCount() == 1
	})
	g.stop()
}

func TestReloadSeveralTimes(t *testing.T) {
	t.Parallel()
	g := newReloadRig(t, []config.Mount{reloadA})
	a := g.h.mounter.get("/mnt/a")
	g.reload(reloadA, reloadB)
	g.reload(reloadB, reloadC)
	c := g.h.mounter.get("/mnt/c")
	g.reload(reloadB, reloadC)

	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	g.wantLive("/mnt/b", "/mnt/c")
	if calls := a.unmountCalls(); !slices.Equal(calls, []bool{true}) {
		t.Errorf("/mnt/a unmount calls (force) = %v, want one", calls)
	}
	for _, m := range []*fakeMounted{g.h.mounter.get("/mnt/b"), c} {
		if calls := m.unmountCalls(); len(calls) != 0 {
			t.Errorf("a mount that stayed was unmounted: %v", calls)
		}
	}
	if got := g.finished(); got != 3 {
		t.Errorf("%d reloads finished, want 3", got)
	}
	g.stop()
}

func TestRunIgnoresAClosedReloadChannel(t *testing.T) {
	t.Parallel()
	g := newReloadRig(t, []config.Mount{reloadA})
	close(g.signals)
	// A closed channel always has a value to receive, so a loop that did not drop it
	// would reload without end or never look at the context again.
	if got := g.live(); !slices.Equal(got, []string{"/mnt/a"}) {
		t.Errorf("live mounts = %v", got)
	}
	g.stop()
	if got := g.loadCount(); got != 0 {
		t.Errorf("the configuration was loaded %d times after the channel closed", got)
	}
}

func TestReloadEndingWithTheContext(t *testing.T) {
	t.Parallel()
	g := newReloadRig(t, []config.Mount{reloadA})
	a := g.h.mounter.get("/mnt/a")
	// Load runs inside the reload, so the context ends in the middle of it.
	g.mu.Lock()
	g.onLoad = g.run.cancel
	g.mu.Unlock()
	g.reload(reloadA, reloadB)

	g.wantMountCalls("/mnt/a")
	if err := g.run.wait(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if calls := a.unmountCalls(); !slices.Equal(calls, []bool{false}) {
		t.Errorf("unmount calls (force) = %v, want the stop's polite unmount", calls)
	}
}

func TestReloadReplacesAMountThatEndedButIsStillListed(t *testing.T) {
	t.Parallel()
	// The watcher of an ended mount removes it a moment later, so a reload can meet it in
	// the set. A set without watchers makes that moment last.
	log := &logBuffer{}
	fc := newFakeCore()
	mounter := newFakeMounter()
	set := newMountSet(fc.Release, log.logger())
	t.Cleanup(set.endWatchers)
	gone := newFakeMounted()
	gone.end(nil)
	set.mounts = []*liveMount{{
		target: reloadA.Target, mounted: gone, cfg: reloadA, fs: namedFS{name: reloadA.Target},
	}}
	r := &reloader{cc: fc, set: set, opts: Options{
		Logger:         log.logger(),
		Mounter:        mounter.Mount,
		ConnectTimeout: reloadTimeout,
		limits:         stopLimits{graceful: reloadTimeout, force: reloadTimeout},
		Load: func() (*config.Config, error) {
			return &config.Config{Mounts: []config.Mount{reloadA}}, nil
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
