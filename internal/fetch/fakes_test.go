package fetch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errTransient = errors.New("connection reset")

// content is the file every test reads: a deterministic function of offset.
func content(off int64) byte { return byte(off*131 + off>>9) }

func fileBytes(off, n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = content(off + int64(i))
	}
	return b
}

// call is one Fetch on a fakeSource, answered by the test.
type call struct {
	claimed bool // handed to the test
	off     int64
	n       int
	ctx     context.Context
	reply   chan error
	done    chan struct{} // closed when Fetch returns
}

func (c *call) succeed()       { c.reply <- nil }
func (c *call) fail(err error) { c.reply <- err }

// cancelled reports whether the Reader cancelled the call, once it returned.
func (c *call) cancelled() bool {
	<-c.done
	return c.ctx.Err() != nil
}

// fakeSource blocks every Fetch until the test answers it, unless respond is
// set, in which case it answers at once.
type fakeSource struct {
	id       string
	mu       sync.Mutex
	cond     *sync.Cond
	calls    []*call
	respond  func(off int64) error
	inflight int
	peak     int
}

func newFakeSource(id string) *fakeSource {
	s := &fakeSource{id: id}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *fakeSource) ID() string { return s.id }

func (s *fakeSource) Fetch(ctx context.Context, off int64, buf []byte) error {
	c := &call{off: off, n: len(buf), ctx: ctx, reply: make(chan error, 1), done: make(chan struct{})}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.inflight++
	s.peak = max(s.peak, s.inflight)
	respond := s.respond
	s.cond.Broadcast()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
		close(c.done)
	}()
	var err error
	if respond != nil {
		err = respond(off)
	} else {
		select {
		case err = <-c.reply:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil {
		copy(buf, fileBytes(off, int64(len(buf))))
	}
	return err
}

func (s *fakeSource) setRespond(f func(off int64) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = f
}

// call waits for a Fetch at off that the test has not taken yet and returns
// it. Fetches started together reach the source in any order, so tests find
// them by offset.
func (s *fakeSource) call(off int64) *call {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		for _, c := range s.calls {
			if c.off == off && !c.claimed {
				c.claimed = true
				return c
			}
		}
		s.cond.Wait()
	}
}

// manual makes the source wait for the test again, and hands it every call
// made so far, so that later lookups by offset find only new calls.
func (s *fakeSource) manual() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = nil
	for _, c := range s.calls {
		c.claimed = true
	}
}

func (s *fakeSource) snapshot() (calls []*call, peak int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*call(nil), s.calls...), s.peak
}

func always(err error) func(int64) error { return func(int64) error { return err } }

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu        sync.Mutex
	cond      *sync.Cond
	blockSize int64
	blocks    map[int64][]byte
	writeErr  error
	readErr   error
}

// newFakeStore returns a Store for a file of size bytes in 100-byte blocks,
// holding the given blocks.
func newFakeStore(size int64, present ...int64) *fakeStore {
	s := &fakeStore{blockSize: 100, blocks: make(map[int64][]byte)}
	s.cond = sync.NewCond(&s.mu)
	for _, b := range present {
		start, length := span(b, s.blockSize, size)
		s.blocks[b] = fileBytes(start, length)
	}
	return s
}

func (s *fakeStore) Has(b int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blocks[b]
	return ok
}

func (s *fakeStore) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return 0, s.readErr
	}
	for i := range p {
		pos := off + int64(i)
		p[i] = s.blocks[pos/s.blockSize][pos%s.blockSize]
	}
	return len(p), nil
}

func (s *fakeStore) WriteBlock(b int64, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.blocks[b] = append([]byte(nil), data...)
	s.cond.Broadcast()
	return nil
}

// awaitCount blocks until the Store holds n blocks.
func (s *fakeStore) awaitCount(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.blocks) < n {
		s.cond.Wait()
	}
}

// check asserts that every stored block holds the file's bytes.
func (s *fakeStore) check(t *testing.T, size int64) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for b, data := range s.blocks {
		start, length := span(b, s.blockSize, size)
		if string(data) != string(fileBytes(start, length)) {
			t.Errorf("stored block %d holds wrong bytes", b)
		}
	}
}

func (s *fakeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.blocks)
}

// fakeMeter has settable rates and records what it is told.
type fakeMeter struct {
	mu       sync.Mutex
	rates    map[string]float64
	observed []string
	failed   []string
}

func newFakeMeter(rates map[string]float64) *fakeMeter {
	if rates == nil {
		rates = map[string]float64{}
	}
	return &fakeMeter{rates: rates}
}

func (m *fakeMeter) Observe(id string, _ int, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observed = append(m.observed, id)
}

func (m *fakeMeter) Failed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, id)
}

func (m *fakeMeter) Rate(id string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rates[id]
	return r, ok
}

func (m *fakeMeter) failures() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.failed...)
}

// fakeClock fires timers only when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c     *fakeClock
	when  time.Time
	f     func()
	armed bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), f: f, armed: true}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.armed
	t.armed = false
	return was
}

// Advance moves time forward by d, running due timers in order in the
// calling goroutine.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		t := c.due(target)
		if t == nil {
			break
		}
		t.armed = false
		c.now = t.when
		c.mu.Unlock()
		t.f()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

func (c *fakeClock) due(target time.Time) *fakeTimer {
	var next *fakeTimer
	for _, t := range c.timers {
		if t.armed && !t.when.After(target) && (next == nil || t.when.Before(next.when)) {
			next = t
		}
	}
	return next
}

// pending lists the due times of armed timers, relative to now.
func (c *fakeClock) pending() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, t := range c.timers {
		if t.armed {
			out = append(out, t.when.Sub(c.now))
		}
	}
	return out
}

// started is one fetch the Reader started.
type started struct {
	id    string
	block int64
	hedge bool
}

// recorder is the tracer of tests.
type recorder struct {
	mu      sync.Mutex
	cond    *sync.Cond
	starts  []started
	settled int
	waits   int
}

func newRecorder() *recorder {
	r := &recorder{}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *recorder) fetchStarted(id string, block int64, hedge bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, started{id, block, hedge})
	r.cond.Broadcast()
}

func (r *recorder) fetchSettled(string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settled++
	r.cond.Broadcast()
}

func (r *recorder) waiting() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits++
	r.cond.Broadcast()
}

// await blocks until pred, called with r.mu held, is true.
func (r *recorder) await(pred func() bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for !pred() {
		r.cond.Wait()
	}
}

func (r *recorder) awaitWaits(n int)   { r.await(func() bool { return r.waits >= n }) }
func (r *recorder) awaitSettled(n int) { r.await(func() bool { return r.settled >= n }) }

func (r *recorder) startList() []started {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]started(nil), r.starts...)
}

// harness is a Reader wired to fakes. Closing it at the end of the test
// checks that nothing the Reader started is left running.
type harness struct {
	t     *testing.T
	r     *Reader
	clock *fakeClock
	meter *fakeMeter
	rec   *recorder
	src   map[string]*fakeSource
	store *fakeStore
}

func baseOptions(size int64) Options {
	return Options{
		Size:         size,
		BlockSize:    100,
		Streams:      1,
		ProbeSize:    16,
		SwitchAfter:  50,
		Slack:        300,
		BlockTimeout: time.Minute,
		HedgeFloor:   500 * time.Millisecond,
	}
}

func newHarness(t *testing.T, opts Options, store *fakeStore, rates map[string]float64, ids ...string) *harness {
	t.Helper()
	h := &harness{t: t, clock: newFakeClock(), meter: newFakeMeter(rates), rec: newRecorder(), src: map[string]*fakeSource{}, store: store}
	opts.Clock = h.clock
	var sources []Source
	for _, id := range ids {
		h.src[id] = newFakeSource(id)
		sources = append(sources, h.src[id])
	}
	var st Store
	if store != nil {
		st = store
	}
	r, err := New(opts, sources, st, h.meter)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.trace = h.rec
	h.r = r
	t.Cleanup(h.close)
	return h
}

func (h *harness) close() {
	if err := h.r.Close(); err != nil {
		h.t.Errorf("Close: %v", err)
	}
	for id, s := range h.src {
		s.mu.Lock()
		if s.inflight != 0 {
			h.t.Errorf("source %s: %d fetches still running after Close", id, s.inflight)
		}
		s.mu.Unlock()
	}
	if p := h.clock.pending(); len(p) != 0 {
		h.t.Errorf("timers still armed after Close: %v", p)
	}
}

type result struct {
	data []byte
	n    int
	err  error
}

// start runs ReadAt in its own goroutine.
func (h *harness) start(ctx context.Context, off int64, n int) <-chan result {
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		got, err := h.r.ReadAt(ctx, buf, off)
		ch <- result{buf[:got], got, err}
	}()
	return ch
}

// mustRead reads and checks that the bytes are the file's.
func (h *harness) mustRead(off int64, n int) {
	h.t.Helper()
	h.check(<-h.start(context.Background(), off, n), off, n)
}

// check asserts a complete, correct read of n bytes at off.
func (h *harness) check(res result, off int64, n int) {
	h.t.Helper()
	if res.err != nil {
		h.t.Fatalf("read %d at %d: %v", n, off, res.err)
	}
	if string(res.data) != string(fileBytes(off, int64(n))) {
		h.t.Fatalf("read %d at %d: wrong bytes", n, off)
	}
}

// state runs f with the Reader's lock held.
func (h *harness) state(f func(r *Reader)) {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	f(h.r)
}

func (h *harness) cooling(id string) bool {
	var c bool
	h.state(func(r *Reader) {
		for _, s := range r.sources {
			if s.id == id {
				c = s.cooling
			}
		}
	})
	return c
}
