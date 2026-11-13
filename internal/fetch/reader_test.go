package fetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
)

func TestProbeReadsFetchOnlyChunks(t *testing.T) {
	t.Parallel()
	opts := baseOptions(10000)
	opts.SwitchAfter = 200
	env := newHarness(t, opts, nil, nil, "a")
	env.src["a"].setRespond(always(nil))
	env.mustRead(0, 10)
	env.mustRead(9990, 10)
	env.mustRead(3, 6)   // chunk 0 is still cached
	env.mustRead(10, 10) // chunk 0 cached, chunk 1 fetched
	calls := env.src["a"].snapshot().calls
	offs := make([]int64, 0, len(calls))
	for _, fetched := range calls {
		if fetched.n != 16 {
			t.Errorf("fetch of %d bytes at %d, want only 16-byte chunks", fetched.n, fetched.off)
		}
		offs = append(offs, fetched.off)
	}
	if want := []int64{0, 9984, 16}; !slices.Equal(offs, want) {
		t.Errorf("fetched chunks at %v, want %v", offs, want)
	}
}

func TestProbeCacheKeepsRecentChunks(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(20000), nil, nil, "a")
	env.src["a"].setRespond(always(nil))
	for i := range int64(probeChunks + 1) {
		env.mustRead(i*1000, 8)
	}
	env.mustRead(probeChunks*1000, 8)
	if calls := env.src["a"].snapshot().calls; len(calls) != probeChunks+1 {
		t.Fatalf("%d fetches, want %d: the most recent chunk was fetched again", len(calls), probeChunks+1)
	}
	env.mustRead(0, 8)
	if calls := env.src["a"].snapshot().calls; len(calls) != probeChunks+2 {
		t.Fatalf("%d fetches, want %d: the oldest chunk was kept", len(calls), probeChunks+2)
	}
}

func TestProbeServedFromStore(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(10000), newFakeStore(10000, 3), nil, "a")
	env.mustRead(310, 10)
	if calls := env.src["a"].snapshot().calls; len(calls) != 0 {
		t.Errorf("%d fetches for a read the Store covers", len(calls))
	}
}

func TestConcurrentProbeReadsShareAChunk(t *testing.T) {
	t.Parallel()
	opts := baseOptions(10000)
	opts.SwitchAfter = 200
	env := newHarness(t, opts, nil, nil, "a")
	first := env.start(context.Background(), 0, 4)
	c := env.src["a"].call(0)
	second := env.start(context.Background(), 8, 4)
	env.rec.awaitWaits(1)
	c.succeed()
	env.check(<-first, 0, 4)
	env.check(<-second, 8, 4)
	if calls := env.src["a"].snapshot().calls; len(calls) != 1 {
		t.Errorf("%d fetches, want 1", len(calls))
	}
}

func TestProbeFailsOver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantFailed  []string
		wantGone    bool
		wantCooling bool
	}{
		{"transient", errTransient, []string{"a"}, false, true},
		{"gone", ErrGone, nil, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newHarness(t, baseOptions(10000), nil, map[string]float64{"a": 100, "b": 50}, "a", "b")
			env.src["a"].setRespond(always(tt.err))
			env.src["b"].setRespond(always(nil))
			env.mustRead(0, 10)
			if got := env.meter.failures(); !slices.Equal(got, tt.wantFailed) {
				t.Errorf("failures %v, want %v", got, tt.wantFailed)
			}
			env.state(func(r *Reader) {
				if s := r.sources[0]; s.gone != tt.wantGone || s.cooling != tt.wantCooling {
					t.Errorf("source a gone=%v cooling=%v, want %v %v", s.gone, s.cooling, tt.wantGone, tt.wantCooling)
				}
			})
		})
	}
}

func TestProbeWaitsForCooldown(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(10000), nil, nil, "a")
	res := env.start(context.Background(), 0, 10)
	env.src["a"].call(0).fail(errTransient)
	env.rec.awaitWaits(1)
	env.clock.Advance(time.Second)
	env.src["a"].call(0).succeed()
	env.check(<-res, 0, 10)
}

func TestSequentialReadingKeepsWindowFilled(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 300
	env := newHarness(t, opts, nil, nil, "a", "b")
	sourceA, b := env.src["a"], env.src["b"]
	res := env.start(context.Background(), 0, 60)
	a0, b0 := sourceA.call(0), b.call(100)
	a0.succeed()
	env.check(<-res, 0, 60)
	a1 := sourceA.call(200)
	b0.succeed()
	b.call(300)
	env.mustRead(60, 100) // blocks 0 and 1 are in memory
	a1.succeed()
	sourceA.call(400)
	want := []started{{"a", 0, false}, {"b", 1, false}, {"a", 2, false}, {"b", 3, false}, {"a", 4, false}}
	if got := env.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want %v", got, want)
	}
	for id, s := range env.src {
		if peak := s.snapshot().peak; peak > opts.Streams {
			t.Errorf("source %s ran %d fetches at once, limit %d", id, peak, opts.Streams)
		}
	}
}

func TestSequentialReadingWholeFile(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 300
	opts.Streams = 2
	opts.SwitchAfter = 200
	env := newHarness(t, opts, nil, nil, "a", "b")
	for _, s := range env.src {
		s.setRespond(always(nil))
	}
	for off := int64(0); off < opts.Size; off += 50 {
		env.mustRead(off, 50)
	}
	perBlock := map[int64]int{}
	for id, s := range env.src {
		seen := s.snapshot()
		if seen.peak > opts.Streams {
			t.Errorf("source %s ran %d fetches at once, limit %d", id, seen.peak, opts.Streams)
		}
		for _, c := range seen.calls {
			if c.n == 100 {
				perBlock[c.off/100]++
			}
		}
	}
	for b := int64(1); b < 20; b++ {
		if perBlock[b] != 1 {
			t.Errorf("block %d fetched %d times, want once", b, perBlock[b])
		}
	}
	if perBlock[0] != 0 {
		t.Error("block 0 fetched whole during probe mode")
	}
}

func TestOutOfOrderReadsStaySequential(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.SwitchAfter = 200
	env := newHarness(t, opts, nil, nil, "a")
	env.src["a"].setRespond(always(nil))
	for _, off := range []int64{100, 0, 200, 50, 150} {
		env.mustRead(off, 50)
	}
	env.state(func(r *Reader) {
		if r.streak.start != 100 || r.streak.pos != 250 {
			t.Errorf("streak %+v, want one streak from 100 to 250", r.streak)
		}
	})
	env.mustRead(250, 50)
	calls := env.src["a"].snapshot().calls
	if !slices.ContainsFunc(calls, func(c *call) bool { return c.n == 100 && c.off == 200 }) {
		t.Error("block 2 not fetched whole: still in probe mode")
	}
}

func TestSlowSourceNeverTakesNextBlocks(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 400
	opts.Streams = 2
	env := newHarness(t, opts, nil, map[string]float64{"a": 100, "b": 10}, "a", "b")
	sourceA, b := env.src["a"], env.src["b"]
	res := env.start(context.Background(), 0, 60)
	sourceA.call(0).succeed()
	env.check(<-res, 0, 60)
	b.call(200).succeed()
	env.rec.awaitSettled(2)
	res = env.start(context.Background(), 100, 100) // moves the window to blocks 2 to 6
	sourceA.call(100).succeed()
	env.check(<-res, 100, 100)
	env.rec.awaitSettled(3)
	want := []started{{"a", 0, false}, {"a", 1, false}, {"b", 2, false}, {"b", 3, false}, {"a", 4, false}, {"a", 5, false}}
	if got := env.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want %v", got, want)
	}
}

func TestHedgeLateBlock(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 100
	env := newHarness(t, opts, nil, nil, "a", "b")
	sourceA, b := env.src["a"], env.src["b"]
	res := env.start(context.Background(), 0, 60)
	first, other := sourceA.call(0), b.call(100)
	env.rec.awaitWaits(1)
	env.clock.Advance(opts.HedgeFloor)
	b.call(0).succeed() // b is busy with block 1; the hedge uses its extra slot
	env.check(<-res, 0, 60)
	if !first.cancelled() {
		t.Error("the late fetch was not cancelled")
	}
	if other.ctx.Err() != nil {
		t.Error("the fetch of block 1 was cancelled")
	}
	want := []started{{"a", 0, false}, {"b", 1, false}, {"b", 0, true}}
	if got := env.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want %v", got, want)
	}
}

func TestNoHedgeWithoutWaiter(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 100
	opts.Streams = 2
	env := newHarness(t, opts, nil, nil, "a", "b")
	sourceA, b := env.src["a"], env.src["b"]
	res := env.start(context.Background(), 0, 60)
	block0, block1 := sourceA.call(0), sourceA.call(100)
	block0.succeed()
	env.check(<-res, 0, 60)
	env.clock.Advance(time.Second)
	want := append(make([]started, 0, 4), started{"a", 0, false}, started{"a", 1, false})
	if got := env.rec.startList(); !slices.Equal(got, want) {
		t.Fatalf("fetches %v, want %v: a block nobody waits for was hedged", got, want)
	}
	// A read now waits for block 1, which becomes the lowest waited block
	// and gets a fresh timer.
	var waits int
	env.rec.await(func() bool { waits = env.rec.waits; return true })
	res = env.start(context.Background(), 100, 10)
	env.rec.awaitWaits(waits + 1)
	env.clock.Advance(opts.HedgeFloor)
	b.call(100).succeed()
	env.check(<-res, 100, 10)
	if !block1.cancelled() {
		t.Error("the late fetch was not cancelled")
	}
	want = append(want, started{"a", 2, false}, started{"b", 1, true})
	if got := env.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want %v", got, want)
	}
}

func TestHedgeTimerUsesSourceRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rates map[string]float64
		want  time.Duration
	}{
		{"own rate", map[string]float64{"a": 1000, "b": 10}, 200 * time.Millisecond},
		{"fastest known when own unknown", map[string]float64{"b": 500}, 400 * time.Millisecond},
		{"floor when nothing known", nil, 10 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := baseOptions(2000)
			opts.HedgeFloor = 10 * time.Millisecond
			env := newHarness(t, opts, nil, tt.rates, "a", "b")
			env.state(func(r *Reader) { r.armHedge(r.block(0), r.sources[0]) })
			if got := env.clock.pending(); !slices.Equal(got, []time.Duration{tt.want}) {
				t.Errorf("timers %v, want [%v]", got, tt.want)
			}
		})
	}
}

func TestTransientFailureRetriesAndCoolsDown(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(2000), nil, map[string]float64{"a": 100, "b": 60}, "a", "b")
	sourceA, b := env.src["a"], env.src["b"]
	expectCooling := func(step string, want bool) {
		t.Helper()
		if got := env.cooling("a"); got != want {
			t.Fatalf("%s: a cooling = %v, want %v", step, got, want)
		}
	}

	res := env.start(context.Background(), 0, 60)
	sourceA.call(0).fail(errTransient)
	b.call(0).succeed()
	env.check(<-res, 0, 60)
	if got := env.meter.failures(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("failures %v, want [a]", got)
	}
	expectCooling("after the first failure", true)
	env.clock.Advance(999 * time.Millisecond)
	expectCooling("before one second", true)
	env.clock.Advance(time.Millisecond)
	expectCooling("after one second", false)

	res = env.start(context.Background(), 100, 60)
	sourceA.call(100).fail(errTransient)
	b.call(100).succeed()
	env.check(<-res, 100, 60)
	env.clock.Advance(time.Second)
	expectCooling("one second into the doubled cooldown", true)
	env.clock.Advance(time.Second)
	expectCooling("after two seconds", false)

	res = env.start(context.Background(), 200, 60)
	sourceA.call(200).succeed()
	env.check(<-res, 200, 60)
	res = env.start(context.Background(), 300, 60)
	sourceA.call(300).fail(errTransient)
	b.call(300).succeed()
	env.check(<-res, 300, 60)
	env.clock.Advance(time.Second)
	expectCooling("after a success reset the backoff", false)
}

func TestFailuresDuringCooldownDoNotLengthenIt(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 100
	opts.Streams = 2
	env := newHarness(t, opts, nil, nil, "a")
	sourceA := env.src["a"]
	res := env.start(context.Background(), 0, 60)
	sourceA.call(0).fail(errTransient)
	sourceA.call(100).fail(errTransient)
	env.rec.awaitSettled(2)
	env.clock.Advance(time.Second)
	if env.cooling("a") {
		t.Fatal("two failures of one outage doubled the cooldown")
	}
	sourceA.call(0).succeed()
	sourceA.call(100).succeed()
	env.check(<-res, 0, 60)
	if got := env.meter.failures(); len(got) != 2 {
		t.Errorf("failures %v, want both reported", got)
	}
}

func TestAllSourcesGone(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.Slack = 100
	env := newHarness(t, opts, newFakeStore(2000, 5), nil, "a", "b")
	res := env.start(context.Background(), 0, 60)
	env.src["a"].call(0).fail(ErrGone)
	env.src["b"].call(0).fail(ErrGone)
	if r := <-res; !errors.Is(r.err, ErrUnavailable) {
		t.Fatalf("read with every source gone: %v, want ErrUnavailable", r.err)
	}
	if got := env.meter.failures(); len(got) != 0 {
		t.Errorf("ErrGone reported to the meter as %v", got)
	}
	env.mustRead(510, 50) // the Store has block 5
	if r := <-env.start(context.Background(), 600, 60); !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("block mode read: %v, want ErrUnavailable", r.err)
	}
	if r := <-env.start(context.Background(), 1500, 10); !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("probe read: %v, want ErrUnavailable", r.err)
	}
}

func TestNoSources(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(2000), nil, nil)
	if r := <-env.start(context.Background(), 0, 10); !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("read: %v, want ErrUnavailable", r.err)
	}
}

func TestBlockTimeout(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(2000), nil, nil, "a")
	res := env.start(context.Background(), 0, 60)
	env.src["a"].call(0)
	env.rec.awaitWaits(1)
	env.clock.Advance(time.Minute)
	if r := <-res; !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("read: %v, want ErrUnavailable", r.err)
	}
}

func TestTimeoutRestartsOnProgress(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.Streams = 2
	env := newHarness(t, opts, nil, nil, "a")
	sourceA := env.src["a"]
	res := env.start(context.Background(), 50, 100) // blocks 0 and 1
	first, second := sourceA.call(0), sourceA.call(100)
	env.rec.awaitWaits(1)
	env.clock.Advance(59 * time.Second)
	first.succeed()
	env.rec.awaitWaits(2)
	env.clock.Advance(59 * time.Second)
	second.succeed()
	env.check(<-res, 50, 100)
}

func TestProbeTimeout(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(10000), nil, nil, "a")
	res := env.start(context.Background(), 0, 10)
	c := env.src["a"].call(0)
	env.clock.Advance(time.Minute)
	if r := <-res; !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("read: %v, want ErrUnavailable", r.err)
	}
	if !c.cancelled() {
		t.Error("the probe fetch was not cancelled")
	}
	if got := env.meter.failures(); len(got) != 0 {
		t.Errorf("the timeout was reported as failures %v", got)
	}
}

func TestCancelledContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		off  int64
		n    int
	}{
		{"block mode", 0, 60},
		{"probe mode", 0, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newHarness(t, baseOptions(2000), nil, nil, "a")
			ctx, cancel := context.WithCancel(context.Background())
			res := env.start(ctx, tt.off, tt.n)
			env.src["a"].call(0)
			cancel()
			if r := <-res; !errors.Is(r.err, context.Canceled) {
				t.Errorf("read: %v, want context.Canceled", r.err)
			}
			if got := env.meter.failures(); len(got) != 0 {
				t.Errorf("the cancellation was reported as failures %v", got)
			}
		})
	}
}

func TestInterruptedReadCancelsItsFetch(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(10000), nil, nil, "a")
	sourceA := env.src["a"]
	ctx, cancel := context.WithCancel(context.Background())
	blockRead := env.start(ctx, 0, 60)
	fetch := sourceA.call(0)
	probeRead := env.start(context.Background(), 5000, 8) // a new streak in probe mode
	sourceA.call(4992).succeed()
	env.check(<-probeRead, 5000, 8)
	if fetch.ctx.Err() != nil {
		t.Fatal("the seek cancelled a fetch that a read waits for")
	}
	cancel()
	if r := <-blockRead; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("read: %v, want context.Canceled", r.err)
	}
	if !fetch.cancelled() {
		t.Error("the interrupted read left its fetch running")
	}
}

func TestSeekCancelsFetchesOutsideWindow(t *testing.T) {
	t.Parallel()
	opts := baseOptions(10000)
	opts.ReadAhead = 300
	opts.Streams = 2
	env := newHarness(t, opts, nil, nil, "a")
	sourceA := env.src["a"]
	res := env.start(context.Background(), 0, 60)
	block0, block1 := sourceA.call(0), sourceA.call(100)
	block0.succeed()
	env.check(<-res, 0, 60)
	block2 := sourceA.call(200)

	res = env.start(context.Background(), 5000, 8)
	probe := sourceA.call(4992)
	if probe.n != 16 {
		t.Errorf("fetch of %d bytes after the seek, want a 16-byte chunk", probe.n)
	}
	probe.succeed()
	env.check(<-res, 5000, 8)
	if !block1.cancelled() || !block2.cancelled() {
		t.Error("fetches outside the new window were not cancelled")
	}
	env.rec.awaitSettled(3)
	env.state(func(r *Reader) {
		if r.streak.start != 5000 || r.streak.length() >= opts.SwitchAfter {
			t.Errorf("streak %+v, want a new streak in probe mode at 5000", r.streak)
		}
	})
	if got := env.meter.failures(); len(got) != 0 {
		t.Errorf("cancelled fetches reported as failures %v", got)
	}

	env.mustRead(10, 20) // block 0 is still in memory
	if calls := sourceA.snapshot().calls; len(calls) != 4 {
		t.Errorf("%d fetches, want 4", len(calls))
	}
	if got := env.rec.startList(); len(got) != 3 {
		t.Errorf("block fetches %v, want none after the seek", got)
	}
}

func TestBlocksGoToStore(t *testing.T) {
	t.Parallel()
	store := newFakeStore(2000)
	env := newHarness(t, baseOptions(2000), store, nil, "a")
	env.src["a"].setRespond(always(nil))
	env.mustRead(0, 60)
	env.mustRead(60, 90)
	if store.count() != 2 {
		t.Errorf("Store holds %d blocks, want 2", store.count())
	}
	env.state(func(r *Reader) {
		if r.resident != 0 {
			t.Errorf("%d blocks kept in memory although they are stored", r.resident)
		}
	})
}

func TestSequentialReadFillsStore(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 300
	opts.Streams = 2
	opts.SwitchAfter = 200
	store := newFakeStore(2000)
	env := newHarness(t, opts, store, map[string]float64{"a": 100, "b": 10}, "a", "b")
	for _, s := range env.src {
		s.setRespond(always(nil))
	}
	for off := int64(0); off < opts.Size; off += 50 {
		env.mustRead(off, 50)
	}
	store.awaitCount(20) // blocks 0 and 1 were read through probe chunks
	store.check(t, opts.Size)
}

func TestSmallFileSkipsProbeMode(t *testing.T) {
	t.Parallel()
	store := newFakeStore(40)
	env := newHarness(t, baseOptions(40), store, nil, "a")
	env.src["a"].setRespond(always(nil))
	env.mustRead(0, 10)
	if !store.Has(0) {
		t.Error("the small file is not in the Store after a read")
	}
	calls := env.src["a"].snapshot().calls
	if len(calls) != 1 || calls[0].n != 40 {
		t.Errorf("fetches %d, want one fetch of the whole 40-byte file", len(calls))
	}
}

// backfillHarness reads [0, 100) through probe chunks and then [100, 160),
// which switches to block mode with block 0 left to backfill.
func backfillHarness(t *testing.T, rates map[string]float64, ids ...string) *harness {
	t.Helper()
	opts := baseOptions(10000)
	opts.SwitchAfter = 150
	env := newHarness(t, opts, newFakeStore(10000), rates, ids...)
	for _, s := range env.src {
		s.setRespond(always(nil))
	}
	env.mustRead(0, 100)
	for _, s := range env.src {
		s.manual()
	}
	return env
}

func TestBackfillYieldsToFastSource(t *testing.T) {
	t.Parallel()
	env := backfillHarness(t, map[string]float64{"a": 100, "b": 10}, "a", "b")
	res := env.start(context.Background(), 100, 60)
	env.src["a"].call(100).succeed()
	env.check(<-res, 100, 60)
	want := []started{{"a", 1, false}, {"b", 0, false}}
	if got := env.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want the read's block on the fast source and backfill on the slow one", got)
	}
}

func TestStuckBackfillDoesNotDelayReads(t *testing.T) {
	t.Parallel()
	env := backfillHarness(t, nil, "a")
	sourceA := env.src["a"]
	res := env.start(context.Background(), 100, 60)
	sourceA.call(100).succeed()
	env.check(<-res, 100, 60)
	backfill := sourceA.call(0) // the only slot is now busy with block 0

	res = env.start(context.Background(), 200, 60)
	if !backfill.cancelled() {
		t.Fatal("the backfill fetch kept the slot a read needs")
	}
	sourceA.call(200).succeed()
	env.check(<-res, 200, 60)
	sourceA.call(0).succeed() // backfill resumes once the read is served
	env.store.awaitCount(3)
	env.store.check(t, 10000)
}

func TestSeekCancelsBackfill(t *testing.T) {
	t.Parallel()
	env := backfillHarness(t, nil, "a")
	sourceA := env.src["a"]
	res := env.start(context.Background(), 100, 60)
	sourceA.call(100).succeed()
	env.check(<-res, 100, 60)
	backfill := sourceA.call(0)

	res = env.start(context.Background(), 5000, 8)
	sourceA.call(4992).succeed()
	env.check(<-res, 5000, 8)
	if !backfill.cancelled() {
		t.Error("the seek left the old streak's backfill running")
	}
}

func TestFailingStoreWriteStillDelivers(t *testing.T) {
	t.Parallel()
	store := newFakeStore(2000)
	store.writeErr = errors.New("no space left on device")
	env := newHarness(t, baseOptions(2000), store, nil, "a")
	env.src["a"].setRespond(always(nil))
	env.mustRead(0, 60)
	env.mustRead(60, 90)
	if store.count() != 0 {
		t.Errorf("Store holds %d blocks after failed writes", store.count())
	}
}

func TestStoreReadError(t *testing.T) {
	t.Parallel()
	store := newFakeStore(2000, 0)
	store.readErr = errors.New("input/output error")
	env := newHarness(t, baseOptions(2000), store, nil, "a")
	if r := <-env.start(context.Background(), 0, 10); !errors.Is(r.err, store.readErr) {
		t.Errorf("read: %v, want the Store's error", r.err)
	}
}

func TestEvictKeepsWaitedBlocks(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(2000), nil, nil, "a")
	env.state(func(r *Reader) {
		for i := range int64(5) {
			b := r.block(i)
			b.waiters = 1
			b.buf = r.getBuffer()
			b.data = (*b.buf)[:100]
			r.resident++
		}
		r.evict()
		if r.resident != 5 {
			t.Errorf("%d blocks resident, want all 5 that reads wait for", r.resident)
		}
		for _, b := range r.blocks {
			b.waiters = 0
		}
		r.evict()
		if r.resident != r.memCap {
			t.Errorf("%d blocks resident, want %d", r.resident, r.memCap)
		}
	})
}

func TestGetBufferIgnoresForeignPoolValues(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(2000), nil, nil, "a")
	env.state(func(r *Reader) {
		// With the race detector, sync.Pool drops some Puts at random, so
		// the foreign value is offered often enough to be seen.
		const offers = 64
		for range offers {
			r.bufs.Put(new(int))
			buf := r.getBuffer()
			if buf == nil || int64(len(*buf)) != r.opts.BlockSize {
				t.Fatalf("getBuffer = %v, want a buffer of one block", buf)
			}
		}
	})
}

func TestVictimOrder(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(2000), nil, nil, "a")
	env.state(func(r *Reader) {
		r.streak = streak{started: true, start: 0, pos: 200} // window is block 2
		for i, used := range map[int64]uint64{1: 5, 2: 1, 3: 3} {
			b := r.block(i)
			b.used = used
			b.buf = r.getBuffer()
			b.data = (*b.buf)[:100]
			r.resident++
		}
		for _, want := range []int64{3, 1, 2} {
			r.memCap = r.resident - 1
			r.evict()
			if _, kept := r.blocks[want]; kept {
				t.Fatalf("block %d kept, want it dropped next", want)
			}
		}
	})
}

func TestReadAtEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		size    int64
		off     int64
		n       int
		wantN   int
		wantErr error
	}{
		{"short read at the end", 250, 240, 20, 10, io.EOF},
		{"read ending at the end", 250, 200, 50, 50, nil},
		{"at the end", 250, 250, 10, 0, io.EOF},
		{"past the end", 250, 300, 10, 0, io.EOF},
		{"zero length", 250, 0, 0, 0, nil},
		{"zero length at the end", 250, 250, 0, 0, io.EOF},
		{"file smaller than a block", 50, 0, 60, 50, io.EOF},
		{"empty file", 0, 0, 10, 0, io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newHarness(t, baseOptions(tt.size), nil, nil, "a")
			env.src["a"].setRespond(always(nil))
			r := <-env.start(context.Background(), tt.off, tt.n)
			if r.n != tt.wantN || !errors.Is(r.err, tt.wantErr) || (tt.wantErr == nil && r.err != nil) {
				t.Fatalf("ReadAt = %d, %v; want %d, %v", r.n, r.err, tt.wantN, tt.wantErr)
			}
			if !bytes.Equal(r.data, fileBytes(tt.off, int64(tt.wantN))) {
				t.Error("wrong bytes")
			}
		})
	}
}

func TestReadAtNegativeOffset(t *testing.T) {
	t.Parallel()
	env := newHarness(t, baseOptions(100), nil, nil, "a")
	if _, err := env.r.ReadAt(context.Background(), make([]byte, 1), -1); err == nil {
		t.Error("read at a negative offset succeeded")
	}
}

func TestCloseStopsEverything(t *testing.T) {
	t.Parallel()
	opts := baseOptions(10000)
	opts.ReadAhead = 100
	env := newHarness(t, opts, nil, nil, "a", "b")
	env.src["b"].setRespond(always(errTransient))
	blockRead := env.start(context.Background(), 0, 60)
	fetch := env.src["a"].call(0)
	env.rec.awaitSettled(1) // b failed block 1 and is cooling down
	env.rec.awaitWaits(1)
	probeRead := env.start(context.Background(), 5000, 10)
	probe := env.src["a"].call(4992)

	if err := env.r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for name, res := range map[string]<-chan result{"block": blockRead, "probe": probeRead} {
		if r := <-res; !errors.Is(r.err, ErrClosed) {
			t.Errorf("%s read: %v, want ErrClosed", name, r.err)
		}
	}
	if !fetch.cancelled() || !probe.cancelled() {
		t.Error("fetches not cancelled")
	}
	if _, err := env.r.ReadAt(context.Background(), make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Errorf("read after Close: %v, want ErrClosed", err)
	}
	if err := env.r.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	t.Parallel()
	good := Options{Size: 10, BlockSize: 10, Streams: 1}
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{"negative size", func(o *Options) { o.Size = -1 }},
		{"zero block size", func(o *Options) { o.BlockSize = 0 }},
		{"zero streams", func(o *Options) { o.Streams = 0 }},
		{"negative read-ahead", func(o *Options) { o.ReadAhead = -1 }},
		{"negative probe size", func(o *Options) { o.ProbeSize = -1 }},
		{"negative switch-after", func(o *Options) { o.SwitchAfter = -1 }},
		{"negative slack", func(o *Options) { o.Slack = -1 }},
		{"negative block timeout", func(o *Options) { o.BlockTimeout = -1 }},
		{"negative hedge floor", func(o *Options) { o.HedgeFloor = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := good
			tt.mutate(&opts)
			if _, err := New(opts, nil, nil, NewMeter()); err == nil {
				t.Error("New accepted the options")
			}
		})
	}
	if _, err := New(good, nil, nil, nil); err == nil {
		t.Error("New accepted a nil meter")
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	src := newFakeSource("a")
	src.setRespond(always(nil))
	r, err := New(Options{Size: 10 << 20, BlockSize: 1 << 20, Streams: 1}, []Source{src}, nil, NewMeter())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := Options{
		Size: 10 << 20, BlockSize: 1 << 20, Streams: 1,
		ProbeSize: 256 << 10, SwitchAfter: 1 << 20, Slack: 2 << 20,
		BlockTimeout: time.Minute, HedgeFloor: 500 * time.Millisecond, Clock: SystemClock{},
	}
	if r.opts != want {
		t.Errorf("options %+v, want %+v", r.opts, want)
	}
	buf := make([]byte, 2<<20)
	if _, err := r.ReadAt(context.Background(), buf, 0); err != nil || !bytes.Equal(buf, fileBytes(0, 2<<20)) {
		t.Errorf("read with the system clock: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
