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
	env    *harness
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
func wireHealth(env *harness, failures int, timeout time.Duration) *healthRig {
	rig := &healthRig{
		env:    env,
		probes: newProbes(),
		ticks:  make(chan time.Time),
		beats:  make(chan struct{}, 64),
	}
	env.t.Cleanup(rig.probes.release)
	notify := env.opts.Notify
	env.opts.Notify = func(state string) error {
		err := notify(state)
		if state == "WATCHDOG=1" {
			rig.beats <- struct{}{}
		}
		return err
	}
	env.opts.newTicker = func(d time.Duration) tickSource {
		rig.interval = d
		return tickSource{ticks: rig.ticks, stop: func() {}}
	}
	env.opts.lstat = rig.probes.lstat
	env.opts.probeFailures = failures
	env.opts.probeTimeout = timeout
	return rig
}

func newHealthRig(t *testing.T, failures int, timeout time.Duration) *healthRig {
	t.Helper()
	return wireHealth(newHarness(t), failures, timeout)
}

func (rig *healthRig) start() {
	rig.env.t.Helper()
	rig.run = rig.env.start()
}

// tick starts a round without waiting for it.
func (rig *healthRig) tick() {
	rig.env.t.Helper()
	rig.rounds++
	select {
	case rig.ticks <- time.Now():
	case <-time.After(reloadTimeout):
		rig.env.t.Fatal("the run did not take the tick")
	}
}

// round sends one tick and waits for the heartbeat that ends the round.
func (rig *healthRig) round() {
	rig.env.t.Helper()
	rig.tick()
	select {
	case <-rig.beats:
	case <-time.After(reloadTimeout):
		rig.env.t.Fatal("the round did not send its heartbeat")
	}
}

// wantBeats checks that every round sent exactly one heartbeat.
func (rig *healthRig) wantBeats() {
	rig.env.t.Helper()
	n := 0
	for _, s := range rig.env.notes.got() {
		if s == "WATCHDOG=1" {
			n++
		}
	}
	if n != rig.rounds {
		rig.env.t.Errorf("heartbeats = %d after %d rounds, want one each", n, rig.rounds)
	}
}

func (rig *healthRig) wantCalls(target string, want int) {
	rig.env.t.Helper()
	if got := rig.probes.count(target); got != want {
		rig.env.t.Errorf("%s was asked %d times, want %d", target, got, want)
	}
}

func (rig *healthRig) wantMountCalls(want ...string) {
	rig.env.t.Helper()
	if got := rig.env.mounter.targets(); !slices.Equal(got, want) {
		rig.env.t.Errorf("mount calls = %v, want %v", got, want)
	}
}

func (rig *healthRig) wantUnmounts(f *fakeMounted, want ...bool) {
	rig.env.t.Helper()
	if got := f.unmountCalls(); !slices.Equal(got, want) {
		rig.env.t.Errorf("unmount calls (force) = %v, want %v", got, want)
	}
}

// wantUntouched checks that the mounts of these targets were never unmounted.
func (rig *healthRig) wantUntouched(targets ...string) {
	rig.env.t.Helper()
	for _, target := range targets {
		rig.wantUnmounts(rig.env.mounter.get(target))
	}
}

func (rig *healthRig) wantLogged(parts ...string) {
	rig.env.t.Helper()
	out := rig.env.log.String()
	for _, part := range parts {
		if !strings.Contains(out, part) {
			rig.env.t.Errorf("log lacks %q:\n%s", part, out)
		}
	}
}

func (rig *healthRig) wantNotLogged(part string) {
	rig.env.t.Helper()
	if out := rig.env.log.String(); strings.Contains(out, part) {
		rig.env.t.Errorf("log has %q:\n%s", part, out)
	}
}

const (
	hungLog     = `level=ERROR msg="mount does not answer, mounting it again" target=/mnt/a`
	hungLogText = "mount does not answer"
)

func TestHealthHealthyMountsPassRoundAfterRound(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 2, 0)
	rig.start()

	targets := []string{"/mnt/a", "/mnt/b", "/mnt/c"}
	for round := 1; round <= 4; round++ {
		rig.round()
		for _, target := range targets {
			rig.wantCalls(target, round)
		}
	}

	if rig.interval != probeInterval {
		t.Errorf("ticker interval = %v, want %v", rig.interval, probeInterval)
	}
	for _, target := range targets {
		want := filepath.Join(target, mount.ProbeName)
		if got := rig.probes.path(target); got != want {
			t.Errorf("probe path = %q, want %q", got, want)
		}
	}
	rig.wantBeats()
	rig.wantUntouched(targets...)
	rig.wantMountCalls(targets...)
	rig.wantNotLogged(hungLogText)
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
			rig := newHealthRig(t, 2, hungTimeout)
			rig.start()
			old := rig.env.mounter.get("/mnt/a")
			rig.probes.set("/mnt/a", tt.answer)

			rig.round()
			// One failure is below the limit, so nothing is done yet.
			rig.wantUnmounts(old)
			rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
			rig.wantNotLogged(hungLogText)

			rig.round()
			rig.wantUnmounts(old, true)
			rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
			rig.wantLogged(hungLog)
			if rig.env.mounter.get("/mnt/a") == old {
				t.Error("the mount at /mnt/a is still the one that did not answer")
			}
			rig.wantUntouched("/mnt/b", "/mnt/c")
			rig.wantNotLogged("could not abort")
			rig.wantBeats()
		})
	}
}

func TestHealthHungMountThatCanBeAbortedIsAbortedOnceAfterItsUnmount(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 2, hungTimeout)
	rig.env.mounter.abortable = true
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	rig.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})

	rig.round()
	if got := old.abortCalls(); len(got) != 0 {
		t.Errorf("abort calls after one failure = %v, want none", got)
	}
	rig.round()

	rig.wantUnmounts(old, true)
	// One Abort, made when the one forced unmount had been made.
	if got, want := old.abortCalls(), []int{1}; !slices.Equal(got, want) {
		t.Errorf("abort calls = %v, want %v", got, want)
	}
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
	rig.wantNotLogged("could not abort")
	for _, target := range []string{"/mnt/b", "/mnt/c"} {
		if got := rig.env.mounter.get(target).abortCalls(); len(got) != 0 {
			t.Errorf("%s was aborted: %v", target, got)
		}
	}
	rig.wantBeats()
}

func TestHealthFailedAbortIsLoggedAndTheMountIsMadeAgain(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 1, hungTimeout)
	rig.env.mounter.abortable = true
	rig.env.mounter.configure = func(f *fakeMounted) { f.abortErr = errors.New("permission denied") }
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	rig.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})

	rig.round()

	rig.wantUnmounts(old, true)
	if got, want := old.abortCalls(), []int{1}; !slices.Equal(got, want) {
		t.Errorf("abort calls = %v, want %v", got, want)
	}
	rig.wantLogged(`level=WARN msg="could not abort the connection of a hung mount" ` +
		`target=/mnt/a err="permission denied"`)
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
	rig.wantBeats()
}

func TestHealthMountThatWasNotDetachedIsNotAborted(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 1, hungTimeout)
	rig.env.mounter.abortable = true
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	old.setBehavior(func(bool) error { return errors.New("device is busy") })
	rig.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})

	rig.round()

	rig.wantUnmounts(old, true)
	if got := old.abortCalls(); len(got) != 0 {
		t.Errorf("abort calls = %v, want none for a mount that is still attached", got)
	}
	// Let the mount go at the end, or the stop of the run waits out its whole force limit.
	old.setBehavior(func(bool) error { return nil })
}

func TestReloadAndStopDetachMountsWithoutAbortingThem(t *testing.T) {
	t.Parallel()
	rig := &reloadRig{env: newHarness(t), signals: make(chan os.Signal)}
	rig.env.mounter.abortable = true
	rig.env.cfg = &config.Config{Mounts: []config.Mount{reloadA(), reloadB()}}
	rig.env.opts.Reload = rig.signals
	rig.env.opts.Load = rig.load
	rig.run = rig.env.start()
	mountedA, mountedB := rig.env.mounter.get("/mnt/a"), rig.env.mounter.get("/mnt/b")

	// The reload detaches /mnt/a with a forced unmount, which open files must survive.
	rig.reload(reloadB())
	if got, want := mountedA.unmountCalls(), []bool{true}; !slices.Equal(got, want) {
		t.Fatalf("/mnt/a unmount calls (force) = %v, want %v", got, want)
	}
	rig.stop()

	for target, mounted := range map[string]*fakeMounted{"/mnt/a": mountedA, "/mnt/b": mountedB} {
		if got := mounted.abortCalls(); len(got) != 0 {
			t.Errorf("%s was aborted: %v", target, got)
		}
	}
}

func TestHealthWaitingCheckCountsWithoutAskingAgain(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 3, hungTimeout)
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	rig.probes.set("/mnt/a", probeAnswer{hang: true})

	for round := 1; round <= 2; round++ {
		rig.round()
		rig.wantCalls("/mnt/a", 1)
		rig.wantUnmounts(old)
	}
	rig.round()

	// The second and third round counted a failure each without asking, which is how the
	// third one reached the limit.
	rig.wantCalls("/mnt/a", 1)
	rig.wantUnmounts(old, true)
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
	rig.wantCalls("/mnt/b", 3)
}

func TestHealthOnePassResetsTheCount(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 2, hungTimeout)
	rig.start()
	old := rig.env.mounter.get("/mnt/a")

	fail := probeAnswer{err: syscall.EIO}
	pass := probeAnswer{err: fs.ErrNotExist}
	for _, answer := range []probeAnswer{fail, pass, fail, pass, fail} {
		rig.probes.set("/mnt/a", answer)
		rig.round()
		rig.wantUnmounts(old)
	}
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")

	// Two in a row after the last pass still reach the limit.
	rig.probes.set("/mnt/a", fail)
	rig.round()
	rig.wantUnmounts(old, true)
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
}

func TestHealthLateAnswerIsDiscarded(t *testing.T) {
	t.Parallel()
	// The limit is far away so that rounds that find the answer not yet in are harmless.
	rig := newHealthRig(t, 1000, hungTimeout)
	rig.start()
	rig.probes.set("/mnt/a", probeAnswer{err: fs.ErrNotExist, hang: true})

	rig.round()
	rig.wantCalls("/mnt/a", 1)
	rig.probes.release()

	// The answer is sent right after the release, and the test cannot see it. A round
	// before it counts a failure without asking, so rounds go on until one asks again.
	for i := 0; rig.probes.count("/mnt/a") < 2; i++ {
		if i == 1000 {
			t.Fatal("no new check was started after the late answer")
		}
		rig.round()
	}
	// With the late answer used up, every round asks anew.
	asked := rig.probes.count("/mnt/a")
	rig.round()
	rig.wantCalls("/mnt/a", asked+1)
	rig.wantUntouched("/mnt/a", "/mnt/b", "/mnt/c")
	rig.wantBeats()
}

func TestHealthHungRequestedMountStaysRequested(t *testing.T) {
	t.Parallel()
	requested := newRequestRig(t)
	rig := wireHealth(requested.env, 2, hungTimeout)
	rig.run = requested.begin()
	requested.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
	old := requested.env.mounter.get("/mnt/tv")
	rig.probes.set("/mnt/tv", probeAnswer{hang: true})

	rig.round()
	rig.round()

	rig.wantUnmounts(old, true)
	rig.wantMountCalls("/mnt/a", "/mnt/tv", "/mnt/tv")
	// The watcher of the old mount takes it off the requested list unless it sees the mount
	// was detached. It logs the end last, so the list can be read once that is there.
	waitFor(t, "the old mount to end", func() bool {
		return strings.Contains(requested.env.log.String(), `msg="mount ended" target=/mnt/tv`)
	})
	if got := requested.listedTargets(); !slices.Equal(got, []string{"/mnt/tv"}) {
		t.Errorf("requested list = %v, want [/mnt/tv]", got)
	}
	requested.wantLive("/mnt/a", "/mnt/tv")
}

func TestHealthUnmountThatFailsLeavesTheMount(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 2, hungTimeout)
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	old.setBehavior(func(bool) error { return errors.New("device is busy") })
	rig.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})

	rig.round()
	rig.round()

	rig.wantUnmounts(old, true)
	// The mount is still in the set, so it is not made again over itself.
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	rig.wantLogged(`level=ERROR msg="mount not detached" target=/mnt/a`)
	if got := rig.env.mounter.get("/mnt/a"); got != old {
		t.Error("the mount at /mnt/a was replaced")
	}
	rig.wantBeats()
	// Let the mount go at the end, or the stop of the run waits out its whole force limit.
	old.setBehavior(func(bool) error { return nil })
}

func TestHealthMountThatCannotBeMadeAgainIsLogged(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 2, hungTimeout)
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	rig.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})
	rig.env.mounter.mu.Lock()
	rig.env.mounter.fail["/mnt/a"] = errors.New("no such device")
	rig.env.mounter.mu.Unlock()

	rig.round()
	rig.round()

	rig.wantUnmounts(old, true)
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c", "/mnt/a")
	rig.wantLogged(`level=ERROR msg="mount not made" target=/mnt/a`)

	// The run goes on: the mounts that are left are still checked.
	rig.round()
	rig.wantCalls("/mnt/a", 2)
	rig.wantCalls("/mnt/b", 3)
	rig.wantCalls("/mnt/c", 3)
	rig.wantBeats()
	if err := rig.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}
}

func TestHealthMountAgainStopsWhenTheConfigurationIsNotUsable(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 1, hungTimeout)
	// The configuration of this run names clusters it does not define, so the requested
	// mount left by an earlier run cannot be added to it. It stays on the list.
	list := `[{"cluster":"example","path":"/","target":"/mnt/r"}]`
	if err := os.WriteFile(requestedPath(rig.env.socket), []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	rig.probes.set("/mnt/a", probeAnswer{err: syscall.EIO})

	rig.round()

	rig.wantUnmounts(old, true)
	rig.wantLogged(`level=ERROR msg="hung mounts not made again"`)
	rig.wantMountCalls("/mnt/a", "/mnt/b", "/mnt/c")
	rig.wantBeats()
}

func TestHealthHeartbeatIsSentWhateverTheRoundFound(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 2, hungTimeout)
	rig.env.notes.fail["WATCHDOG=1"] = errors.New("no socket")
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	rig.probes.set("/mnt/a", probeAnswer{hang: true})

	// A hung mount and a refused heartbeat in every round change nothing for the rest.
	for range 3 {
		rig.round()
	}

	rig.wantBeats()
	rig.wantUnmounts(old, true)
	rig.wantLogged(`level=DEBUG msg="could not send the heartbeat to the service manager"`)
	rig.wantCalls("/mnt/b", 3)
	if err := rig.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}
}

func TestHealthRunWithoutMountsSendsHeartbeats(t *testing.T) {
	t.Parallel()
	rig := newHealthRig(t, 1, 0)
	rig.env.cfg = &config.Config{}
	rig.start()

	rig.round()
	rig.round()

	rig.wantBeats()
	rig.wantCalls("/mnt/a", 0)
	rig.wantMountCalls()
	if err := rig.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}
}

func TestHealthContextEndingDuringARoundCountsNoFailure(t *testing.T) {
	t.Parallel()
	// With a limit of one, a failure counted for the answer not waited for would unmount.
	rig := newHealthRig(t, 1, reloadTimeout)
	rig.start()
	old := rig.env.mounter.get("/mnt/a")
	rig.probes.set("/mnt/a", probeAnswer{hang: true})

	rig.tick()
	select {
	case <-rig.probes.started:
	case <-time.After(reloadTimeout):
		t.Fatal("the round did not ask the mount")
	}
	if err := rig.run.stop(); err != nil {
		t.Errorf("Run = %v, want nil after a clean stop", err)
	}

	// The stop unmounts politely. A forced unmount would be the round's doing.
	rig.wantUnmounts(old, false)
	rig.wantNotLogged(hungLogText)
}

func TestSystemTicker(t *testing.T) {
	t.Parallel()
	source := systemTicker(5 * time.Millisecond)
	select {
	case <-source.ticks:
	case <-time.After(reloadTimeout):
		t.Fatal("no tick arrived")
	}
	source.stop()
	// A stopped timer sends nothing more, and the wait is long enough for several ticks.
	select {
	case <-source.ticks:
		t.Error("a tick arrived after stop")
	case <-time.After(40 * time.Millisecond):
	}
}
