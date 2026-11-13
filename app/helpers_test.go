package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

// shortDir returns a temporary directory with a short path: the path of a Unix socket
// is limited to about 100 bytes, which t.TempDir often exceeds.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sfs") //nolint:usetesting // t.TempDir paths are too long for a socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})
	return dir
}

// say writes text to conn for a fake peer. A failed write closes the connection, which the
// test sees as the failure of the call under test.
func say(conn net.Conn, text string) {
	if _, err := io.WriteString(conn, text); err != nil {
		closeQuietly(conn)
	}
}

func dialUnix(path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(context.Background(), "unix", path)
}

func listenUnixSocket(path string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(context.Background(), "unix", path)
}

// fakeMounted is a mount whose end the test controls.
type fakeMounted struct {
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	err      error
	calls    []bool // the force argument of every Unmount call
	behavior func(force bool) error
	endsOnOK bool
	abortErr error // what Abort answers, for a mount made abortable
	aborts   []int // how many Unmount calls had been made at each Abort call
}

func newFakeMounted() *fakeMounted {
	return &fakeMounted{done: make(chan struct{}), endsOnOK: true}
}

func (f *fakeMounted) Done() <-chan struct{} { return f.done }

func (f *fakeMounted) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeMounted) Unmount(_ context.Context, force bool) error {
	f.mu.Lock()
	f.calls = append(f.calls, force)
	behavior, endsOnOK := f.behavior, f.endsOnOK
	f.mu.Unlock()
	if behavior != nil {
		if err := behavior(force); err != nil {
			return err
		}
	}
	if endsOnOK {
		f.end(nil)
	}
	return nil
}

// end ends the mount the way an unmount from outside the program does.
func (f *fakeMounted) end(err error) {
	f.once.Do(func() {
		f.mu.Lock()
		f.err = err
		f.mu.Unlock()
		close(f.done)
	})
}

func (f *fakeMounted) setBehavior(behavior func(force bool) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behavior = behavior
}

func (f *fakeMounted) unmountCalls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

// abortableMounted is a fakeMounted that also implements mount.Aborter, like the FUSE mount.
type abortableMounted struct{ *fakeMounted }

func (a abortableMounted) Abort() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.aborts = append(a.aborts, len(a.calls))
	return a.abortErr
}

// abortCalls returns, for every Abort call, how many Unmount calls came before it.
func (f *fakeMounted) abortCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.aborts...)
}

// busyUnlessForced refuses a polite unmount the way a mount in use does.
//
//nolint:revive // it is a behavior of fakeMounted, whose type takes the flag
func busyUnlessForced(force bool) error {
	if force {
		return nil
	}
	return fmt.Errorf("unmount: %w", syscall.EBUSY)
}

// fakeMounter hands out fakeMounted values and records what it was asked to mount.
type fakeMounter struct {
	mu      sync.Mutex
	opts    []mount.Options
	fsys    []core.FS
	mounted map[string]*fakeMounted
	fail    map[string]error
	// configure, if set, is called on every new mount before it is returned.
	configure func(*fakeMounted)
	// abortable makes every new mount implement mount.Aborter.
	abortable bool
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{mounted: make(map[string]*fakeMounted), fail: make(map[string]error)}
}

//nolint:ireturn // it implements the Mounter function type, which returns the interface
func (m *fakeMounter) Mount(fsys core.FS, opts mount.Options) (mount.Mounted, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts = append(m.opts, opts)
	m.fsys = append(m.fsys, fsys)
	if err := m.fail[opts.Target]; err != nil {
		return nil, err
	}
	mounted := newFakeMounted()
	if m.configure != nil {
		m.configure(mounted)
	}
	m.mounted[opts.Target] = mounted
	if m.abortable {
		return abortableMounted{mounted}, nil
	}
	return mounted, nil
}

func (m *fakeMounter) get(target string) *fakeMounted {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted[target]
}

func (m *fakeMounter) targets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.opts))
	for _, o := range m.opts {
		out = append(out, o.Target)
	}
	return out
}

// namedFS lets a test tell the filesystems of different mounts apart.
type namedFS struct {
	core.FS

	name string
}

// fakeCore is a Core with scripted behavior.
type fakeCore struct {
	mu          sync.Mutex
	connectErr  map[string]error
	blockOn     map[string]bool // Connect waits for its context to end
	blocked     chan struct{}   // receives one value per Connect that starts waiting
	connected   []string
	viewErr     error
	closeErr    error
	closed      int
	status      core.Status
	viewedMount []config.Mount
	applyErr    error
	applied     []*config.Config
	released    []core.FS
	// stale names the targets whose views are no longer current.
	stale map[string]bool
}

func newFakeCore() *fakeCore {
	return &fakeCore{connectErr: make(map[string]error), blockOn: make(map[string]bool), blocked: make(chan struct{}, 16)}
}

func (f *fakeCore) Connect(ctx context.Context, cluster string) error {
	f.mu.Lock()
	f.connected = append(f.connected, cluster)
	err, block := f.connectErr[cluster], f.blockOn[cluster]
	f.mu.Unlock()
	if block {
		f.blocked <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

//nolint:ireturn // it implements the View method of the Core interface
func (f *fakeCore) View(mountCfg config.Mount) (core.FS, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.viewedMount = append(f.viewedMount, mountCfg)
	if f.viewErr != nil {
		return nil, f.viewErr
	}
	return namedFS{name: mountCfg.Target}, nil
}

func (f *fakeCore) Status() core.Status { return f.status }

func (f *fakeCore) Apply(cfg *config.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, cfg)
	return nil
}

func (f *fakeCore) Release(fsys core.FS) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, fsys)
}

func (f *fakeCore) Current(fsys core.FS) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	named, ok := fsys.(namedFS)
	if !ok {
		// Only a view the fake made can be stale.
		return true
	}
	return !f.stale[named.name]
}

func (f *fakeCore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func (f *fakeCore) releasedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.released)
}

func (f *fakeCore) connectedClusters() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(slices.Values(f.connected))
}

func (f *fakeCore) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// waitFor returns once cond holds. The watchers and the run loop work in their own
// goroutines, so a test that needs their result has to wait for it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// notifications records the states passed to Notify, and signals each one.
type notifications struct {
	mu     sync.Mutex
	states []string
	fail   map[string]error
	ready  chan struct{}
}

func newNotifications() *notifications {
	return &notifications{fail: make(map[string]error), ready: make(chan struct{}, 1)}
}

func (n *notifications) notify(state string) error {
	n.mu.Lock()
	n.states = append(n.states, state)
	err := n.fail[state]
	n.mu.Unlock()
	if state == "READY=1" {
		n.ready <- struct{}{}
	}
	return err
}

func (n *notifications) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.states...)
}

// logBuffer is a logger that a test can read while goroutines write to it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// harness is one Run with fakes around it.
type harness struct {
	t       *testing.T
	cfg     *config.Config
	core    *fakeCore
	mounter *fakeMounter
	notes   *notifications
	log     *logBuffer
	opts    Options
	socket  string
}

func testConfig() *config.Config {
	return &config.Config{Mounts: []config.Mount{
		{Cluster: "example", Path: "/media", Target: "/mnt/a", AllowOther: true},
		{Cluster: "example", Path: "/", Target: "/mnt/b"},
		{Cluster: "other", Path: "/docs", Target: "/mnt/c"},
	}}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	env := &harness{
		t:       t,
		cfg:     testConfig(),
		core:    newFakeCore(),
		mounter: newFakeMounter(),
		notes:   newNotifications(),
		log:     &logBuffer{},
		socket:  filepath.Join(shortDir(t), "c.sock"),
	}
	env.opts = Options{
		Logger:     env.log.logger(),
		Mounter:    env.mounter.Mount,
		Notify:     env.notes.notify,
		SocketPath: env.socket,
		NewCore:    func(*config.Config, core.Options) (Core, error) { return env.core, nil },
		limits:     stopLimits{graceful: time.Minute, force: time.Minute},
	}
	return env
}

// running is a Run in progress.
type running struct {
	env    *harness
	cancel context.CancelFunc
	result chan error
}

// start runs the client in the background and returns once it has reported ready.
func (env *harness) start(targets ...string) *running {
	env.t.Helper()
	r := env.startUnready(targets...)
	select {
	case <-env.notes.ready:
	case err := <-r.result:
		env.t.Fatalf("Run ended before it was ready: %v", err)
	}
	return r
}

// startUnready runs the client in the background without waiting for it.
func (env *harness) startUnready(targets ...string) *running {
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{env: env, cancel: cancel, result: make(chan error, 1)}
	env.t.Cleanup(func() {
		cancel()
		if err := r.wait(); err != nil {
			env.t.Logf("Run ended with %v", err)
		}
	})
	go func() { r.result <- Run(ctx, env.cfg, targets, env.opts) }()
	return r
}

// stop cancels the run and returns its result.
func (r *running) stop() error {
	r.cancel()
	return r.wait()
}

// wait returns the result of Run. It may be called once.
func (r *running) wait() error {
	err := <-r.result
	r.result <- err
	return err
}

// status asks the running client for its report over the socket.
func (env *harness) status() Report {
	env.t.Helper()
	report, err := Status(context.Background(), env.socket)
	if err != nil {
		env.t.Fatal(err)
	}
	return report
}

// runSync runs the client to its end.
func (env *harness) runSync(targets ...string) error {
	return Run(context.Background(), env.cfg, targets, env.opts)
}

func errIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want one wrapping %v", err, target)
	}
}
