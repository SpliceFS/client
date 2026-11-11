package fetch

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
)

var bg = context.Background()

func TestProbeReadsFetchOnlyChunks(t *testing.T) {
	t.Parallel()
	opts := baseOptions(10000)
	opts.SwitchAfter = 200
	h := newHarness(t, opts, nil, nil, "a")
	h.src["a"].setRespond(always(nil))
	h.mustRead(0, 10)
	h.mustRead(9990, 10)
	h.mustRead(3, 6)   // chunk 0 is still cached
	h.mustRead(10, 10) // chunk 0 cached, chunk 1 fetched
	calls, _ := h.src["a"].snapshot()
	var offs []int64
	for _, c := range calls {
		if c.n != 16 {
			t.Errorf("fetch of %d bytes at %d, want only 16-byte chunks", c.n, c.off)
		}
		offs = append(offs, c.off)
	}
	if want := []int64{0, 9984, 16}; !slices.Equal(offs, want) {
		t.Errorf("fetched chunks at %v, want %v", offs, want)
	}
}

func TestProbeCacheKeepsRecentChunks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(20000), nil, nil, "a")
	h.src["a"].setRespond(always(nil))
	for i := range int64(probeChunks + 1) {
		h.mustRead(i*1000, 8)
	}
	h.mustRead(probeChunks*1000, 8)
	if calls, _ := h.src["a"].snapshot(); len(calls) != probeChunks+1 {
		t.Fatalf("%d fetches, want %d: the most recent chunk was fetched again", len(calls), probeChunks+1)
	}
	h.mustRead(0, 8)
	if calls, _ := h.src["a"].snapshot(); len(calls) != probeChunks+2 {
		t.Fatalf("%d fetches, want %d: the oldest chunk was kept", len(calls), probeChunks+2)
	}
}

func TestProbeServedFromStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(10000), newFakeStore(10000, 3), nil, "a")
	h.mustRead(310, 10)
	if calls, _ := h.src["a"].snapshot(); len(calls) != 0 {
		t.Errorf("%d fetches for a read the Store covers", len(calls))
	}
}

func TestConcurrentProbeReadsShareAChunk(t *testing.T) {
	t.Parallel()
	opts := baseOptions(10000)
	opts.SwitchAfter = 200
	h := newHarness(t, opts, nil, nil, "a")
	first := h.start(bg, 0, 4)
	c := h.src["a"].call(0)
	second := h.start(bg, 8, 4)
	h.rec.awaitWaits(1)
	c.succeed()
	h.check(<-first, 0, 4)
	h.check(<-second, 8, 4)
	if calls, _ := h.src["a"].snapshot(); len(calls) != 1 {
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
			h := newHarness(t, baseOptions(10000), nil, map[string]float64{"a": 100, "b": 50}, "a", "b")
			h.src["a"].setRespond(always(tt.err))
			h.src["b"].setRespond(always(nil))
			h.mustRead(0, 10)
			if got := h.meter.failures(); !slices.Equal(got, tt.wantFailed) {
				t.Errorf("failures %v, want %v", got, tt.wantFailed)
			}
			h.state(func(r *Reader) {
				if s := r.sources[0]; s.gone != tt.wantGone || s.cooling != tt.wantCooling {
					t.Errorf("source a gone=%v cooling=%v, want %v %v", s.gone, s.cooling, tt.wantGone, tt.wantCooling)
				}
			})
		})
	}
}

func TestProbeWaitsForCooldown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(10000), nil, nil, "a")
	res := h.start(bg, 0, 10)
	h.src["a"].call(0).fail(errTransient)
	h.rec.awaitWaits(1)
	h.clock.Advance(time.Second)
	h.src["a"].call(0).succeed()
	h.check(<-res, 0, 10)
}

func TestSequentialReadingKeepsWindowFilled(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 300
	h := newHarness(t, opts, nil, nil, "a", "b")
	a, b := h.src["a"], h.src["b"]
	res := h.start(bg, 0, 60)
	a0, b0 := a.call(0), b.call(100)
	a0.succeed()
	h.check(<-res, 0, 60)
	a1 := a.call(200)
	b0.succeed()
	b.call(300)
	h.mustRead(60, 100) // blocks 0 and 1 are in memory
	a1.succeed()
	a.call(400)
	want := []started{{"a", 0, false}, {"b", 1, false}, {"a", 2, false}, {"b", 3, false}, {"a", 4, false}}
	if got := h.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want %v", got, want)
	}
	for id, s := range h.src {
		if _, peak := s.snapshot(); peak > opts.Streams {
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
	h := newHarness(t, opts, nil, nil, "a", "b")
	for _, s := range h.src {
		s.setRespond(always(nil))
	}
	for off := int64(0); off < opts.Size; off += 50 {
		h.mustRead(off, 50)
	}
	perBlock := map[int64]int{}
	for id, s := range h.src {
		calls, peak := s.snapshot()
		if peak > opts.Streams {
			t.Errorf("source %s ran %d fetches at once, limit %d", id, peak, opts.Streams)
		}
		for _, c := range calls {
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
		t.Errorf("block 0 fetched whole during probe mode")
	}
}

func TestOutOfOrderReadsStaySequential(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.SwitchAfter = 200
	h := newHarness(t, opts, nil, nil, "a")
	h.src["a"].setRespond(always(nil))
	for _, off := range []int64{100, 0, 200, 50, 150} {
		h.mustRead(off, 50)
	}
	h.state(func(r *Reader) {
		if r.streak.start != 100 || r.streak.pos != 250 {
			t.Errorf("streak %+v, want one streak from 100 to 250", r.streak)
		}
	})
	h.mustRead(250, 50)
	calls, _ := h.src["a"].snapshot()
	if !slices.ContainsFunc(calls, func(c *call) bool { return c.n == 100 && c.off == 200 }) {
		t.Error("block 2 not fetched whole: still in probe mode")
	}
}

func TestSlowSourceNeverTakesNextBlocks(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 400
	opts.Streams = 2
	h := newHarness(t, opts, nil, map[string]float64{"a": 100, "b": 10}, "a", "b")
	a, b := h.src["a"], h.src["b"]
	res := h.start(bg, 0, 60)
	a.call(0).succeed()
	h.check(<-res, 0, 60)
	b.call(200).succeed()
	h.rec.awaitSettled(2)
	res = h.start(bg, 100, 100) // moves the window to blocks 2 to 6
	a.call(100).succeed()
	h.check(<-res, 100, 100)
	h.rec.awaitSettled(3)
	want := []started{{"a", 0, false}, {"a", 1, false}, {"b", 2, false}, {"b", 3, false}, {"a", 4, false}, {"a", 5, false}}
	if got := h.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want %v", got, want)
	}
}

func TestHedgeLateBlock(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 100
	h := newHarness(t, opts, nil, nil, "a", "b")
	a, b := h.src["a"], h.src["b"]
	res := h.start(bg, 0, 60)
	first, other := a.call(0), b.call(100)
	h.rec.awaitWaits(1)
	h.clock.Advance(opts.HedgeFloor)
	b.call(0).succeed() // b is busy with block 1; the hedge uses its extra slot
	h.check(<-res, 0, 60)
	if !first.cancelled() {
		t.Error("the late fetch was not cancelled")
	}
	if other.ctx.Err() != nil {
		t.Error("the fetch of block 1 was cancelled")
	}
	want := []started{{"a", 0, false}, {"b", 1, false}, {"b", 0, true}}
	if got := h.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want %v", got, want)
	}
}

func TestNoHedgeWithoutWaiter(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 100
	opts.Streams = 2
	h := newHarness(t, opts, nil, nil, "a", "b")
	a, b := h.src["a"], h.src["b"]
	res := h.start(bg, 0, 60)
	a0, a1 := a.call(0), a.call(100)
	a0.succeed()
	h.check(<-res, 0, 60)
	h.clock.Advance(time.Second)
	want := []started{{"a", 0, false}, {"a", 1, false}}
	if got := h.rec.startList(); !slices.Equal(got, want) {
		t.Fatalf("fetches %v, want %v: a block nobody waits for was hedged", got, want)
	}
	// A read now waits for block 1, which becomes the lowest waited block
	// and gets a fresh timer.
	var waits int
	h.rec.await(func() bool { waits = h.rec.waits; return true })
	res = h.start(bg, 100, 10)
	h.rec.awaitWaits(waits + 1)
	h.clock.Advance(opts.HedgeFloor)
	b.call(100).succeed()
	h.check(<-res, 100, 10)
	if !a1.cancelled() {
		t.Error("the late fetch was not cancelled")
	}
	want = append(want, started{"a", 2, false}, started{"b", 1, true})
	if got := h.rec.startList(); !slices.Equal(got, want) {
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
			h := newHarness(t, opts, nil, tt.rates, "a", "b")
			h.state(func(r *Reader) { r.armHedge(r.block(0), r.sources[0]) })
			if got := h.clock.pending(); !slices.Equal(got, []time.Duration{tt.want}) {
				t.Errorf("timers %v, want [%v]", got, tt.want)
			}
		})
	}
}

func TestTransientFailureRetriesAndCoolsDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(2000), nil, map[string]float64{"a": 100, "b": 60}, "a", "b")
	a, b := h.src["a"], h.src["b"]
	expectCooling := func(step string, want bool) {
		t.Helper()
		if got := h.cooling("a"); got != want {
			t.Fatalf("%s: a cooling = %v, want %v", step, got, want)
		}
	}

	res := h.start(bg, 0, 60)
	a.call(0).fail(errTransient)
	b.call(0).succeed()
	h.check(<-res, 0, 60)
	if got := h.meter.failures(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("failures %v, want [a]", got)
	}
	expectCooling("after the first failure", true)
	h.clock.Advance(999 * time.Millisecond)
	expectCooling("before one second", true)
	h.clock.Advance(time.Millisecond)
	expectCooling("after one second", false)

	res = h.start(bg, 100, 60)
	a.call(100).fail(errTransient)
	b.call(100).succeed()
	h.check(<-res, 100, 60)
	h.clock.Advance(time.Second)
	expectCooling("one second into the doubled cooldown", true)
	h.clock.Advance(time.Second)
	expectCooling("after two seconds", false)

	res = h.start(bg, 200, 60)
	a.call(200).succeed()
	h.check(<-res, 200, 60)
	res = h.start(bg, 300, 60)
	a.call(300).fail(errTransient)
	b.call(300).succeed()
	h.check(<-res, 300, 60)
	h.clock.Advance(time.Second)
	expectCooling("after a success reset the backoff", false)
}

func TestFailuresDuringCooldownDoNotLengthenIt(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.ReadAhead = 100
	opts.Streams = 2
	h := newHarness(t, opts, nil, nil, "a")
	a := h.src["a"]
	res := h.start(bg, 0, 60)
	a.call(0).fail(errTransient)
	a.call(100).fail(errTransient)
	h.rec.awaitSettled(2)
	h.clock.Advance(time.Second)
	if h.cooling("a") {
		t.Fatal("two failures of one outage doubled the cooldown")
	}
	a.call(0).succeed()
	a.call(100).succeed()
	h.check(<-res, 0, 60)
	if got := h.meter.failures(); len(got) != 2 {
		t.Errorf("failures %v, want both reported", got)
	}
}

func TestAllSourcesGone(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.Slack = 100
	h := newHarness(t, opts, newFakeStore(2000, 5), nil, "a", "b")
	res := h.start(bg, 0, 60)
	h.src["a"].call(0).fail(ErrGone)
	h.src["b"].call(0).fail(ErrGone)
	if r := <-res; !errors.Is(r.err, ErrUnavailable) {
		t.Fatalf("read with every source gone: %v, want ErrUnavailable", r.err)
	}
	if got := h.meter.failures(); len(got) != 0 {
		t.Errorf("ErrGone reported to the meter as %v", got)
	}
	h.mustRead(510, 50) // the Store has block 5
	if r := <-h.start(bg, 600, 60); !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("block mode read: %v, want ErrUnavailable", r.err)
	}
	if r := <-h.start(bg, 1500, 10); !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("probe read: %v, want ErrUnavailable", r.err)
	}
}

func TestNoSources(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(2000), nil, nil)
	if r := <-h.start(bg, 0, 10); !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("read: %v, want ErrUnavailable", r.err)
	}
}

func TestBlockTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(2000), nil, nil, "a")
	res := h.start(bg, 0, 60)
	h.src["a"].call(0)
	h.rec.awaitWaits(1)
	h.clock.Advance(time.Minute)
	if r := <-res; !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("read: %v, want ErrUnavailable", r.err)
	}
}

func TestTimeoutRestartsOnProgress(t *testing.T) {
	t.Parallel()
	opts := baseOptions(2000)
	opts.Streams = 2
	h := newHarness(t, opts, nil, nil, "a")
	a := h.src["a"]
	res := h.start(bg, 50, 100) // blocks 0 and 1
	first, second := a.call(0), a.call(100)
	h.rec.awaitWaits(1)
	h.clock.Advance(59 * time.Second)
	first.succeed()
	h.rec.awaitWaits(2)
	h.clock.Advance(59 * time.Second)
	second.succeed()
	h.check(<-res, 50, 100)
}

func TestProbeTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(10000), nil, nil, "a")
	res := h.start(bg, 0, 10)
	c := h.src["a"].call(0)
	h.clock.Advance(time.Minute)
	if r := <-res; !errors.Is(r.err, ErrUnavailable) {
		t.Errorf("read: %v, want ErrUnavailable", r.err)
	}
	if !c.cancelled() {
		t.Error("the probe fetch was not cancelled")
	}
	if got := h.meter.failures(); len(got) != 0 {
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
			h := newHarness(t, baseOptions(2000), nil, nil, "a")
			ctx, cancel := context.WithCancel(bg)
			res := h.start(ctx, tt.off, tt.n)
			h.src["a"].call(0)
			cancel()
			if r := <-res; !errors.Is(r.err, context.Canceled) {
				t.Errorf("read: %v, want context.Canceled", r.err)
			}
			if got := h.meter.failures(); len(got) != 0 {
				t.Errorf("the cancellation was reported as failures %v", got)
			}
		})
	}
}

func TestInterruptedReadCancelsItsFetch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(10000), nil, nil, "a")
	a := h.src["a"]
	ctx, cancel := context.WithCancel(bg)
	blockRead := h.start(ctx, 0, 60)
	fetch := a.call(0)
	probeRead := h.start(bg, 5000, 8) // a new streak in probe mode
	a.call(4992).succeed()
	h.check(<-probeRead, 5000, 8)
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
	h := newHarness(t, opts, nil, nil, "a")
	a := h.src["a"]
	res := h.start(bg, 0, 60)
	c0, c1 := a.call(0), a.call(100)
	c0.succeed()
	h.check(<-res, 0, 60)
	c2 := a.call(200)

	res = h.start(bg, 5000, 8)
	probe := a.call(4992)
	if probe.n != 16 {
		t.Errorf("fetch of %d bytes after the seek, want a 16-byte chunk", probe.n)
	}
	probe.succeed()
	h.check(<-res, 5000, 8)
	if !c1.cancelled() || !c2.cancelled() {
		t.Error("fetches outside the new window were not cancelled")
	}
	h.rec.awaitSettled(3)
	h.state(func(r *Reader) {
		if r.streak.start != 5000 || r.streak.length() >= opts.SwitchAfter {
			t.Errorf("streak %+v, want a new streak in probe mode at 5000", r.streak)
		}
	})
	if got := h.meter.failures(); len(got) != 0 {
		t.Errorf("cancelled fetches reported as failures %v", got)
	}

	h.mustRead(10, 20) // block 0 is still in memory
	if calls, _ := a.snapshot(); len(calls) != 4 {
		t.Errorf("%d fetches, want 4", len(calls))
	}
	if got := h.rec.startList(); len(got) != 3 {
		t.Errorf("block fetches %v, want none after the seek", got)
	}
}

func TestBlocksGoToStore(t *testing.T) {
	t.Parallel()
	store := newFakeStore(2000)
	h := newHarness(t, baseOptions(2000), store, nil, "a")
	h.src["a"].setRespond(always(nil))
	h.mustRead(0, 60)
	h.mustRead(60, 90)
	if store.count() != 2 {
		t.Errorf("Store holds %d blocks, want 2", store.count())
	}
	h.state(func(r *Reader) {
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
	h := newHarness(t, opts, store, map[string]float64{"a": 100, "b": 10}, "a", "b")
	for _, s := range h.src {
		s.setRespond(always(nil))
	}
	for off := int64(0); off < opts.Size; off += 50 {
		h.mustRead(off, 50)
	}
	store.awaitCount(20) // blocks 0 and 1 were read through probe chunks
	store.check(t, opts.Size)
}

func TestSmallFileSkipsProbeMode(t *testing.T) {
	t.Parallel()
	store := newFakeStore(40)
	h := newHarness(t, baseOptions(40), store, nil, "a")
	h.src["a"].setRespond(always(nil))
	h.mustRead(0, 10)
	if !store.Has(0) {
		t.Error("the small file is not in the Store after a read")
	}
	calls, _ := h.src["a"].snapshot()
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
	h := newHarness(t, opts, newFakeStore(10000), rates, ids...)
	for _, s := range h.src {
		s.setRespond(always(nil))
	}
	h.mustRead(0, 100)
	for _, s := range h.src {
		s.manual()
	}
	return h
}

func TestBackfillYieldsToFastSource(t *testing.T) {
	t.Parallel()
	h := backfillHarness(t, map[string]float64{"a": 100, "b": 10}, "a", "b")
	res := h.start(bg, 100, 60)
	h.src["a"].call(100).succeed()
	h.check(<-res, 100, 60)
	want := []started{{"a", 1, false}, {"b", 0, false}}
	if got := h.rec.startList(); !slices.Equal(got, want) {
		t.Errorf("fetches %v, want the read's block on the fast source and backfill on the slow one", got)
	}
}

func TestStuckBackfillDoesNotDelayReads(t *testing.T) {
	t.Parallel()
	h := backfillHarness(t, nil, "a")
	a := h.src["a"]
	res := h.start(bg, 100, 60)
	a.call(100).succeed()
	h.check(<-res, 100, 60)
	backfill := a.call(0) // the only slot is now busy with block 0

	res = h.start(bg, 200, 60)
	if !backfill.cancelled() {
		t.Fatal("the backfill fetch kept the slot a read needs")
	}
	a.call(200).succeed()
	h.check(<-res, 200, 60)
	a.call(0).succeed() // backfill resumes once the read is served
	h.store.awaitCount(3)
	h.store.check(t, 10000)
}

func TestSeekCancelsBackfill(t *testing.T) {
	t.Parallel()
	h := backfillHarness(t, nil, "a")
	a := h.src["a"]
	res := h.start(bg, 100, 60)
	a.call(100).succeed()
	h.check(<-res, 100, 60)
	backfill := a.call(0)

	res = h.start(bg, 5000, 8)
	a.call(4992).succeed()
	h.check(<-res, 5000, 8)
	if !backfill.cancelled() {
		t.Error("the seek left the old streak's backfill running")
	}
}

func TestFailingStoreWriteStillDelivers(t *testing.T) {
	t.Parallel()
	store := newFakeStore(2000)
	store.writeErr = errors.New("no space left on device")
	h := newHarness(t, baseOptions(2000), store, nil, "a")
	h.src["a"].setRespond(always(nil))
	h.mustRead(0, 60)
	h.mustRead(60, 90)
	if store.count() != 0 {
		t.Errorf("Store holds %d blocks after failed writes", store.count())
	}
}

func TestStoreReadError(t *testing.T) {
	t.Parallel()
	store := newFakeStore(2000, 0)
	store.readErr = errors.New("input/output error")
	h := newHarness(t, baseOptions(2000), store, nil, "a")
	if r := <-h.start(bg, 0, 10); !errors.Is(r.err, store.readErr) {
		t.Errorf("read: %v, want the Store's error", r.err)
	}
}

func TestEvictKeepsWaitedBlocks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(2000), nil, nil, "a")
	h.state(func(r *Reader) {
		for i := range int64(5) {
			b := r.block(i)
			b.waiters = 1
			b.buf = r.bufs.Get().(*[]byte)
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

func TestVictimOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(2000), nil, nil, "a")
	h.state(func(r *Reader) {
		r.streak = streak{started: true, start: 0, pos: 200} // window is block 2
		for i, used := range map[int64]uint64{1: 5, 2: 1, 3: 3} {
			b := r.block(i)
			b.used = used
			b.buf = r.bufs.Get().(*[]byte)
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
			h := newHarness(t, baseOptions(tt.size), nil, nil, "a")
			h.src["a"].setRespond(always(nil))
			r := <-h.start(bg, tt.off, tt.n)
			if r.n != tt.wantN || !errors.Is(r.err, tt.wantErr) || (tt.wantErr == nil && r.err != nil) {
				t.Fatalf("ReadAt = %d, %v; want %d, %v", r.n, r.err, tt.wantN, tt.wantErr)
			}
			if string(r.data) != string(fileBytes(tt.off, int64(tt.wantN))) {
				t.Error("wrong bytes")
			}
		})
	}
}

func TestReadAtNegativeOffset(t *testing.T) {
	t.Parallel()
	h := newHarness(t, baseOptions(100), nil, nil, "a")
	if _, err := h.r.ReadAt(bg, make([]byte, 1), -1); err == nil {
		t.Error("read at a negative offset succeeded")
	}
}

func TestCloseStopsEverything(t *testing.T) {
	t.Parallel()
	opts := baseOptions(10000)
	opts.ReadAhead = 100
	h := newHarness(t, opts, nil, nil, "a", "b")
	h.src["b"].setRespond(always(errTransient))
	blockRead := h.start(bg, 0, 60)
	fetch := h.src["a"].call(0)
	h.rec.awaitSettled(1) // b failed block 1 and is cooling down
	h.rec.awaitWaits(1)
	probeRead := h.start(bg, 5000, 10)
	probe := h.src["a"].call(4992)

	if err := h.r.Close(); err != nil {
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
	if _, err := h.r.ReadAt(bg, make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Errorf("read after Close: %v, want ErrClosed", err)
	}
	if err := h.r.Close(); err != nil {
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
	if _, err := r.ReadAt(bg, buf, 0); err != nil || string(buf) != string(fileBytes(0, 2<<20)) {
		t.Errorf("read with the system clock: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
