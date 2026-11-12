package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/mount"
)

// hungTimeout is how long a round waits for a mount that does not answer. It is long
// enough that a mount that answers at once cannot miss it on a busy machine.
const hungTimeout = 250 * time.Millisecond

// probeAnswer is what the test lstat answers for one target.
type probeAnswer struct {
	err error
	// hang makes the answer wait until the test releases every waiting lstat.
	hang bool
}

// probes is the lstat of a test. A target without an answer set does not have the probe
// name, which is what a working mount says.
type probes struct {
	mu      sync.Mutex
	answers map[string]probeAnswer
	calls   map[string]int
	paths   map[string]string
	started chan string
	hold    chan struct{}
	once    sync.Once
}

func newProbes() *probes {
	return &probes{
		answers: make(map[string]probeAnswer),
		calls:   make(map[string]int),
		paths:   make(map[string]string),
		started: make(chan string, 64),
		hold:    make(chan struct{}),
	}
}

func (p *probes) lstat(path string) (os.FileInfo, error) {
	target := filepath.Dir(path)
	p.mu.Lock()
	p.calls[target]++
	p.paths[target] = path
	answer, set := p.answers[target]
	p.mu.Unlock()
	select {
	case p.started <- target:
	default:
	}
	if !set {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: syscall.ENOENT}
	}
	if answer.hang {
		<-p.hold
	}
	return nil, answer.err
}

func (p *probes) set(target string, answer probeAnswer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answers[target] = answer
}

func (p *probes) count(target string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[target]
}

func (p *probes) path(target string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paths[target]
}

// release lets every waiting lstat answer, now and later.
func (p *probes) release() { p.once.Do(func() { close(p.hold) }) }

// healthRig is a run whose health rounds the test starts by hand.
type healthRig struct {
	h      *harness
	probes *probes
	ticks  chan time.Time
	beats  chan struct{}
	run    *running
	// rounds counts the ticks sent.
	rounds int
	// interval is what the run asked of its ticker. Read it after the first round, whose
	// tick the run took after it asked.
	interval time.Duration
}

// wireHealth replaces the clock, the lstat and the limits of h, and counts the heartbeats.
func wireHealth(h *harness, failures int, timeout time.Duration) *healthRig {
	g := &healthRig{
		h:      h,
		probes: newProbes(),
		ticks:  make(chan time.Time),
		beats:  make(chan struct{}, 64),
	}
	h.t.Cleanup(g.probes.release)
	notify := h.opts.Notify
	h.opts.Notify = func(state string) error {
		err := notify(state)
		if state == "WATCHDOG=1" {
			g.beats <- struct{}{}
		}
		return err
	}
	h.opts.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		g.interval = d
		return g.ticks, func() {}
	}
	h.opts.lstat = g.probes.lstat
	h.opts.probeFailures = failures
	h.opts.probeTimeout = timeout
	return g
}

func newHealthRig(t *testing.T, failures int, timeout time.Duration) *healthRig {
	t.Helper()
	return wireHealth(newHarness(t), failures, timeout)
}

func (g *healthRig) start() {
	g.h.t.Helper()
	g.run = g.h.start()
}

// tick starts a round without waiting for it.
func (g *healthRig) tick() {
	g.h.t.Helper()
	g.rounds++
	select {
	case g.ticks <- time.Now():
	case <-time.After(reloadTimeout):
		g.h.t.Fatal("the run did not take the tick")
	}
}

// round sends one tick and waits for the heartbeat that ends the round.
func (g *healthRig) round() {
	g.h.t.Helper()
	g.tick()
	select {
	case <-g.beats:
	case <-time.After(reloadTimeout):
		g.h.t.Fatal("the round did not send its heartbeat")
	}
}

// wantBeats checks that every round sent exactly one heartbeat.
func (g *healthRig) wantBeats() {
	g.h.t.Helper()
	n := 0
	for _, s := range g.h.notes.got() {
		if s == "WATCHDOG=1" {
			n++
		}
	}
	if n != g.rounds {
		g.h.t.Errorf("heartbeats = %d after %d rounds, want one each", n, g.rounds)
	}
}

func (g *healthRig) wantCalls(target string, want int) {
	g.h.t.Helper()
	if got := g.probes.count(target); got != want {
		g.h.t.Errorf("%s was asked %d times, want %d", target, got, want)
	}
}

func (g *healthRig) wantMountCalls(want ...string) {
	g.h.t.Helper()
	if got := g.h.mounter.targets(); !slices.Equal(got, want) {
		g.h.t.Errorf("mount calls = %v, want %v", got, want)
	}
}

func (g *healthRig) wantUnmounts(f *fakeMounted, want ...bool) {
	g.h.t.Helper()
	if got := f.unmountCalls(); !slices.Equal(got, want) {
		g.h.t.Errorf("unmount calls (force) = %v, want %v", got, want)
	}
}

// wantUntouched checks that the mounts of these targets were never unmounted.
func (g *healthRig) wantUntouched(targets ...string) {
	g.h.t.Helper()
	for _, target := range targets {
		g.wantUnmounts(g.h.mounter.get(target))
	}
}

func (g *healthRig) wantLogged(parts ...string) {
	g.h.t.Helper()
	out := g.h.log.String()
	for _, part := range parts {
		if !strings.Contains(out, part) {
			g.h.t.Errorf("log lacks %q:\n%s", part, out)
		}
	}
}

func (g *healthRig) wantNotLogged(part string) {
	g.h.t.Helper()
	if out := g.h.log.String(); strings.Contains(out, part) {
		g.h.t.Errorf("log has %q:\n%s", part, out)
	}
}

const (
	hungLog     = `level=ERROR msg="mount does not answer, mounting it again" target=/mnt/a`
	hungLogText = "mount does not answer"
)

func TestHealthHealthyMountsPassRoundAfterRound(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 2, 0)
	g.start()

	targets := []string{"/mnt/a", "/mnt/b", "/mnt/c"}
	for round := 1; round <= 4; round++ {
		g.round()
		for _, target := range targets {
			g.wantCalls(target, round)
		}
	}

	if g.interval != probeInterval {
		t.Errorf("ticker interval = %v, want %v", g.interval, probeInterval)
	}
	for _, target := range targets {
		want := filepath.Join(target, mount.ProbeName)
		if got := g.probes.path(target); got != want {
			t.Errorf("probe path = %q, want %q", got, want)
		}
	}
	g.wantBeats()
	g.wantUntouched(targets...)
	g.wantMountCalls(targets...)
	g.wantNotLogged(hungLogText)
}

func TestHealthFailingMountIsMountedAgain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer probeAnswer
	}{
		{"the probe name exists", probeAnswer{}},
		{"another error", probeAnswer{err: syscall.EIO}},
		{"no answer in time", probeAnswer{hang: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newHealthRig(t, 2, hungTimeout)
			g.start()
			old := g.h.mounter.get("/mnt/a")
			g.probes.set("/mnt/a", tt.answer)

			g.round()
			// One failure is below the limit, so nothing is done yet.
			g.wantUnmounts(old)
			g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
			g.wantNotLogged(hungLogText)

			g.round()
			g.wantUnmounts(old, true)
			g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
			g.wantLogged(hungLog)
			if g.h.mounter.get("/mnt/a") == old {
				t.Error("the mount at /mnt/a is still the one that did not answer")
			}
			g.wantUntouched("/mnt/b", "/mnt/c")
			g.wantBeats()
		})
	}
}

func TestHealthWaitingCheckCountsWithoutAskingAgain(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 3, hungTimeout)
	g.start()
	old := g.h.mounter.get("/mnt/a")
	g.probes.set("/mnt/a", probeAnswer{hang: true})

	for round := 1; round <= 2; round++ {
		g.round()
		g.wantCalls("/mnt/a", 1)
		g.wantUnmounts(old)
	}
	g.round()

	// The second and third round counted a failure each without asking, which is how the
	// third one reached the limit.
	g.wantCalls("/mnt/a", 1)
	g.wantUnmounts(old, true)
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
	g.wantCalls("/mnt/b", 3)
}

func TestHealthOnePassResetsTheCount(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 2, hungTimeout)
	g.start()
	old := g.h.mounter.get("/mnt/a")

	fail := probeAnswer{err: syscall.EIO}
	pass := probeAnswer{err: fs.ErrNotExist}
	for _, answer := range []probeAnswer{fail, pass, fail, pass, fail} {
		g.probes.set("/mnt/a", answer)
		g.round()
		g.wantUnmounts(old)
	}
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")

	// Two in a row after the last pass still reach the limit.
	g.probes.set("/mnt/a", fail)
	g.round()
	g.wantUnmounts(old, true)
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
}

func TestHealthLateAnswerIsDiscarded(t *testing.T) {
	t.Parallel()
	// The limit is far away so that rounds that find the answer not yet in are harmless.
	g := newHealthRig(t, 1000, hungTimeout)
	g.start()
	g.probes.set("/mnt/a", probeAnswer{err: fs.ErrNotExist, hang: true})

	g.round()
	g.wantCalls("/mnt/a", 1)
	g.probes.release()

	// The answer is sent right after the release, and the test cannot see it. A round
	// before it counts a failure without asking, so rounds go on until one asks again.
	for i := 0; g.probes.count("/mnt/a") < 2; i++ {
		if i == 1000 {
			t.Fatal("no new check was started after the late answer")
		}
		g.round()
	}
	// With the late answer used up, every round asks anew.
	asked := g.probes.count("/mnt/a")
	g.round()
	g.wantCalls("/mnt/a", asked+1)
	g.wantUntouched("/mnt/a", "/mnt/b", "/mnt/c")
	g.wantBeats()
}

func TestHealthHungRequestedMountStaysRequested(t *testing.T) {
	t.Parallel()
	rig := newRequestRig(t)
	g := wireHealth(rig.h, 2, hungTimeout)
	g.run = rig.begin()
	rig.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
	old := rig.h.mounter.get("/mnt/tv")
	g.probes.set("/mnt/tv", probeAnswer{hang: true})

	g.round()
	g.round()

	g.wantUnmounts(old, true)
	g.wantMountCalls("/mnt/a", "/mnt/tv", "/mnt/tv")
	// The watcher of the old mount takes it off the requested list unless it sees the mount
	// was detached. It logs the end last, so the list can be read once that is there.
	waitFor(t, "the old mount to end", func() bool {
		return strings.Contains(rig.h.log.String(), `msg="mount ended" target=/mnt/tv`)
	})
	if got := rig.listedTargets(); !slices.Equal(got, []string{"/mnt/tv"}) {
		t.Errorf("requested list = %v, want [/mnt/tv]", got)
	}
	rig.wantLive("/mnt/a", "/mnt/tv")
}

func TestHealthUnmountThatFailsLeavesTheMount(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 2, hungTimeout)
	g.start()
	old := g.h.mounter.get("/mnt/a")
	old.setBehavior(func(bool) error { return errors.New("device is busy") })
	g.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})

	g.round()
	g.round()

	g.wantUnmounts(old, true)
	// The mount is still in the set, so it is not made again over itself.
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	g.wantLogged(`level=ERROR msg="mount not detached" target=/mnt/a`)
	if got := g.h.mounter.get("/mnt/a"); got != old {
		t.Error("the mount at /mnt/a was replaced")
	}
	g.wantBeats()
	// Let the mount go at the end, or the stop of the run waits out its whole force limit.
	old.setBehavior(func(bool) error { return nil })
}

func TestHealthMountThatCannotBeMadeAgainIsLogged(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 2, hungTimeout)
	g.start()
	old := g.h.mounter.get("/mnt/a")
	g.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})
	g.h.mounter.mu.Lock()
	g.h.mounter.fail["/mnt/a"] = errors.New("no such device")
	g.h.mounter.mu.Unlock()

	g.round()
	g.round()

	g.wantUnmounts(old, true)
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
	g.wantLogged(`level=ERROR msg="mount not made" target=/mnt/a`)

	// The run goes on: the mounts that are left are still checked.
	g.round()
	g.wantCalls("/mnt/a", 2)
	g.wantCalls("/mnt/b", 3)
	g.wantCalls("/mnt/c", 3)
	g.wantBeats()
	if err := g.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}
}

func TestHealthMountAgainStopsWhenTheConfigurationIsNotUsable(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 1, hungTimeout)
	// The configuration of this run names clusters it does not define, so the requested
	// mount left by an earlier run cannot be added to it. It stays on the list.
	list := `[{"cluster":"example","path":"/","target":"/mnt/r"}]`
	if err := os.WriteFile(requestedPath(g.h.socket), []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	g.start()
	old := g.h.mounter.get("/mnt/a")
	g.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})

	g.round()

	g.wantUnmounts(old, true)
	g.wantLogged(`level=ERROR msg="hung mounts not made again"`)
	g.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	g.wantBeats()
}

func TestHealthHeartbeatIsSentWhateverTheRoundFound(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 2, hungTimeout)
	g.h.notes.fail["WATCHDOG=1"] = errors.New("no socket")
	g.start()
	old := g.h.mounter.get("/mnt/a")
	g.probes.set("/mnt/a", probeAnswer{hang: true})

	// A hung mount and a refused heartbeat in every round change nothing for the rest.
	for range 3 {
		g.round()
	}

	g.wantBeats()
	g.wantUnmounts(old, true)
	g.wantLogged(`level=DEBUG msg="could not send the heartbeat to the service manager"`)
	g.wantCalls("/mnt/b", 3)
	if err := g.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}
}

func TestHealthRunWithoutMountsSendsHeartbeats(t *testing.T) {
	t.Parallel()
	g := newHealthRig(t, 1, 0)
	g.h.cfg = &config.Config{}
	g.start()

	g.round()
	g.round()

	g.wantBeats()
	g.wantCalls("/mnt/a", 0)
	g.wantMountCalls()
	if err := g.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}
}

func TestHealthContextEndingDuringARoundCountsNoFailure(t *testing.T) {
	t.Parallel()
	// With a limit of one, a failure counted for the answer not waited for would unmount.
	g := newHealthRig(t, 1, reloadTimeout)
	g.start()
	old := g.h.mounter.get("/mnt/a")
	g.probes.set("/mnt/a", probeAnswer{hang: true})

	g.tick()
	select {
	case <-g.probes.started:
	case <-time.After(reloadTimeout):
		t.Fatal("the round did not ask the mount")
	}
	if err := g.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}

	// The stop unmounts politely. A forced unmount would be the round's doing.
	g.wantUnmounts(old, false)
	g.wantNotLogged(hungLogText)
}

func TestSystemTicker(t *testing.T) {
	t.Parallel()
	ticks, stop := systemTicker(5 * time.Millisecond)
	select {
	case <-ticks:
	case <-time.After(reloadTimeout):
		t.Fatal("no tick arrived")
	}
	stop()
	// A stopped timer sends nothing more, and the wait is long enough for several ticks.
	select {
	case <-ticks:
		t.Error("a tick arrived after stop")
	case <-time.After(40 * time.Millisecond):
	}
}
