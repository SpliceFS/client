package main

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

// stubCore is the core client with no cluster behind it.
type stubCore struct{ fsys core.FS }

func (stubCore) Connect(context.Context, string) error { return nil }

//nolint:ireturn // implements the client's View, which returns the interface
func (c stubCore) View(config.Mount) (core.FS, error) { return c.fsys, nil }

func (stubCore) Status() core.Status {
	east := core.NodeStatus{Name: "east", BytesPerSecond: 12_000_000, Measured: true}
	return core.Status{
		Clusters: []core.ClusterStatus{{Name: "example", Nodes: []core.NodeStatus{east}}},
		Caches:   []core.CacheStatus{{Dir: "/var/cache/splicefs", Entries: 3, Bytes: 4096}},
	}
}

func (stubCore) Apply(*config.Config) error { return nil }

func (stubCore) Release(core.FS) {}

// Current is true so that a reload would leave the mounts of these tests as they are.
func (stubCore) Current(core.FS) bool { return true }

func (stubCore) Close() error { return nil }

// stubMount is a mount that is gone as soon as it is unmounted.
type stubMount struct {
	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	calls []bool
	busy  bool
}

func (m *stubMount) Done() <-chan struct{} { return m.done }
func (*stubMount) Err() error              { return nil }

//nolint:revive // flag-parameter: implements the mount layer's Unmount, which takes force
func (m *stubMount) Unmount(_ context.Context, force bool) error {
	m.mu.Lock()
	m.calls = append(m.calls, force)
	busy := m.busy
	m.mu.Unlock()
	if busy && !force {
		return fmt.Errorf("unmount: %w", syscall.EBUSY)
	}
	m.once.Do(func() { close(m.done) })
	return nil
}

// setBusy makes the mount refuse a polite unmount, as a mount with open files does.
func (m *stubMount) setBusy(busy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = busy
}

func (m *stubMount) unmountCalls() []bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.calls...)
}

// fullStack is the real program, from the command line to the control socket, with a stub
// core client and a stub mount layer.
type fullStack struct {
	t         *testing.T
	configDir string
	mount     *stubMount
	ready     chan struct{}
	backend   backend
	// stop ends the mount command that startMount started, the way a stop signal does.
	stop context.CancelFunc
}

func newFullStack(t *testing.T, fsys core.FS, mounter app.Mounter) *fullStack {
	t.Helper()
	stack := &fullStack{
		t:         t,
		configDir: t.TempDir(),
		mount:     &stubMount{done: make(chan struct{})},
		ready:     make(chan struct{}),
	}
	cfg := &config.Config{Mounts: []config.Mount{{Cluster: "example", Path: "/", Target: stack.configDir + "/mnt"}}}
	b := systemBackend()
	b.loadConfig = func(string) (*config.Config, error) { return cfg, nil }
	realRun := b.run
	b.run = func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error {
		opts.NewCore = func(*config.Config, core.Options) (app.Core, error) { return stubCore{fsys: fsys}, nil }
		return realRun(ctx, cfg, targets, opts)
	}
	b.mounter = mounter
	if mounter == nil {
		b.mounter = func(core.FS, mount.Options) (mount.Mounted, error) { return stack.mount, nil }
	}
	b.notify = func(state string) error {
		if state == "READY=1" {
			close(stack.ready)
		}
		return nil
	}
	stack.backend = b
	return stack
}

// startMount runs "splicefs mount" and returns a channel that receives its exit status
// and stderr when it ends.
func (stack *fullStack) startMount() <-chan result {
	ended := make(chan result, 1)
	ctx, stop := context.WithCancel(context.Background())
	stack.stop = stop
	stack.t.Cleanup(stop)
	go func() {
		var out, errOut bytes.Buffer
		status := execute(ctx, []string{"--config", stack.configDir, "mount"}, &out, &errOut, stack.backend)
		ended <- result{status: status, stdout: out.String(), stderr: errOut.String()}
	}()
	return ended
}

// command runs another command of the program against the running client.
func (stack *fullStack) command(args ...string) result {
	stack.t.Helper()
	var out, errOut bytes.Buffer
	argv := append([]string{"--config", stack.configDir}, args...)
	status := execute(context.Background(), argv, &out, &errOut, systemBackend())
	return result{status: status, stdout: out.String(), stderr: errOut.String()}
}

// result is what one run of the program produced.
type result struct {
	status int
	stdout string
	stderr string
}

// requireStatusReport checks that status, as a table and as JSON, reports the running mount
// and what the stub client measured.
func requireStatusReport(t *testing.T, stack *fullStack) {
	t.Helper()
	table := stack.command("status")
	if table.status != exitOK {
		t.Fatalf("status: %+v", table)
	}
	wanted := []string{
		stack.configDir + "/mnt", "mounted", "splicefs:example/", "east", "12.0 MB/s",
		"/var/cache/splicefs", "4.1 kB",
	}
	for _, want := range wanted {
		if !strings.Contains(table.stdout, want) {
			t.Errorf("status output lacks %q:\n%s", want, table.stdout)
		}
	}
	asJSON := stack.command("status", "--json")
	if asJSON.status != exitOK || !strings.Contains(asJSON.stdout, `"bytes_per_second": 12000000`) {
		t.Errorf("status --json: %+v", asJSON)
	}
}

// requireBusyMountIsLeftAlone checks that a polite umount of a mount in use is refused, with
// the hint about --force, and that the mount is still there afterwards.
func requireBusyMountIsLeftAlone(t *testing.T, stack *fullStack) {
	t.Helper()
	stack.mount.setBusy(true)
	busy := stack.command("umount")
	if busy.status != exitError || !strings.Contains(busy.stderr, "mount is in use") ||
		!strings.Contains(busy.stderr, "umount --force") {
		t.Errorf("umount of a busy mount: %+v", busy)
	}
	if got := stack.command("status"); !strings.Contains(got.stdout, "mounted") {
		t.Errorf("the busy mount was not left alone:\n%s", got.stdout)
	}
}

func TestStatusAndUmountAgainstARunningMount(t *testing.T) {
	t.Parallel()
	stack := newFullStack(t, nil, nil)
	ended := stack.startMount()
	<-stack.ready

	requireStatusReport(t, stack)

	requireBusyMountIsLeftAlone(t, stack)

	if forced := stack.command("umount", "--force", stack.configDir+"/mnt"); forced.status != exitOK {
		t.Errorf("umount --force: %+v", forced)
	}
	// The client keeps running with no mounts, so that a reload can mount again.
	if got := stack.command("status"); got.status != exitOK || strings.Contains(got.stdout, "mounted") {
		t.Errorf("status after the last mount was unmounted: %+v", got)
	}
	stack.stop()
	if end := <-ended; end.status != exitOK {
		t.Errorf("mount ended with %+v after it was stopped", end)
	}
	gone := stack.command("status")
	if gone.status != exitError || !strings.Contains(gone.stderr, "no client is running for this configuration") {
		t.Errorf("status after the client ended: %+v", gone)
	}
	if calls := stack.mount.unmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("unmount calls (force) = %v", calls)
	}
}

func TestMountRefusesASecondClient(t *testing.T) {
	t.Parallel()
	stack := newFullStack(t, nil, nil)
	ended := stack.startMount()
	<-stack.ready

	var out, errOut bytes.Buffer
	status := execute(context.Background(), []string{"--config", stack.configDir, "mount"}, &out, &errOut, stack.backend)
	if status != exitError || !strings.Contains(errOut.String(), "a client is already running for this configuration") {
		t.Errorf("second mount: status %d stderr %q", status, errOut.String())
	}
	if got := stack.command("umount", "--force"); got.status != exitOK {
		t.Errorf("umount: %+v", got)
	}
	stack.stop()
	if end := <-ended; end.status != exitOK {
		t.Errorf("first mount ended with %+v", end)
	}
}
