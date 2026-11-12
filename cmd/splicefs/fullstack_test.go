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

func (c stubCore) View(config.Mount) (core.FS, error) { return c.fsys, nil }

func (stubCore) Status() core.Status {
	return core.Status{
		Clusters: []core.ClusterStatus{{Name: "example", Nodes: []core.NodeStatus{{Name: "east", BytesPerSecond: 12_000_000, Measured: true}}}},
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
func (m *stubMount) Err() error            { return nil }

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
	f := &fullStack{t: t, configDir: t.TempDir(), mount: &stubMount{done: make(chan struct{})}, ready: make(chan struct{})}
	cfg := &config.Config{Mounts: []config.Mount{{Cluster: "example", Path: "/", Target: f.configDir + "/mnt"}}}
	b := systemBackend()
	b.loadConfig = func(string) (*config.Config, error) { return cfg, nil }
	realRun := b.run
	b.run = func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error {
		opts.NewCore = func(*config.Config, core.Options) (app.Core, error) { return stubCore{fsys: fsys}, nil }
		return realRun(ctx, cfg, targets, opts)
	}
	b.mounter = mounter
	if mounter == nil {
		b.mounter = func(core.FS, mount.Options) (mount.Mounted, error) { return f.mount, nil }
	}
	b.notify = func(state string) error {
		if state == "READY=1" {
			close(f.ready)
		}
		return nil
	}
	f.backend = b
	return f
}

// startMount runs "splicefs mount" and returns a channel that receives its exit status
// and stderr when it ends.
func (f *fullStack) startMount() <-chan result {
	ended := make(chan result, 1)
	ctx, stop := context.WithCancel(context.Background())
	f.stop = stop
	f.t.Cleanup(stop)
	go func() {
		var out, errOut bytes.Buffer
		status := execute(ctx, []string{"--config", f.configDir, "mount"}, &out, &errOut, f.backend)
		ended <- result{status: status, stdout: out.String(), stderr: errOut.String()}
	}()
	return ended
}

// command runs another command of the program against the running client.
func (f *fullStack) command(args ...string) result {
	f.t.Helper()
	var out, errOut bytes.Buffer
	status := execute(context.Background(), append([]string{"--config", f.configDir}, args...), &out, &errOut, systemBackend())
	return result{status: status, stdout: out.String(), stderr: errOut.String()}
}

type result struct {
	status int
	stdout string
	stderr string
}

func TestStatusAndUmountAgainstARunningMount(t *testing.T) {
	t.Parallel()
	f := newFullStack(t, nil, nil)
	ended := f.startMount()
	<-f.ready

	status := f.command("status")
	if status.status != exitOK {
		t.Fatalf("status: %+v", status)
	}
	for _, want := range []string{f.configDir + "/mnt", "mounted", "splicefs:example/", "east", "12.0 MB/s", "/var/cache/splicefs", "4.1 kB"} {
		if !strings.Contains(status.stdout, want) {
			t.Errorf("status output lacks %q:\n%s", want, status.stdout)
		}
	}
	if j := f.command("status", "--json"); j.status != exitOK || !strings.Contains(j.stdout, `"bytes_per_second": 12000000`) {
		t.Errorf("status --json: %+v", j)
	}

	f.mount.mu.Lock()
	f.mount.busy = true
	f.mount.mu.Unlock()
	busy := f.command("umount")
	if busy.status != exitError || !strings.Contains(busy.stderr, "mount is in use") || !strings.Contains(busy.stderr, "umount --force") {
		t.Errorf("umount of a busy mount: %+v", busy)
	}
	if got := f.command("status"); !strings.Contains(got.stdout, "mounted") {
		t.Errorf("the busy mount was not left alone:\n%s", got.stdout)
	}

	if forced := f.command("umount", "--force", f.configDir+"/mnt"); forced.status != exitOK {
		t.Errorf("umount --force: %+v", forced)
	}
	// The client keeps running with no mounts, so that a reload can mount again.
	if got := f.command("status"); got.status != exitOK || strings.Contains(got.stdout, "mounted") {
		t.Errorf("status after the last mount was unmounted: %+v", got)
	}
	f.stop()
	if end := <-ended; end.status != exitOK {
		t.Errorf("mount ended with %+v after it was stopped", end)
	}
	if got := f.command("status"); got.status != exitError || !strings.Contains(got.stderr, "no client is running for this configuration") {
		t.Errorf("status after the client ended: %+v", got)
	}
	if calls := f.mount.unmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("unmount calls (force) = %v", calls)
	}
}

func TestMountRefusesASecondClient(t *testing.T) {
	t.Parallel()
	f := newFullStack(t, nil, nil)
	ended := f.startMount()
	<-f.ready

	var out, errOut bytes.Buffer
	status := execute(context.Background(), []string{"--config", f.configDir, "mount"}, &out, &errOut, f.backend)
	if status != exitError || !strings.Contains(errOut.String(), "a client is already running for this configuration") {
		t.Errorf("second mount: status %d stderr %q", status, errOut.String())
	}
	if got := f.command("umount", "--force"); got.status != exitOK {
		t.Errorf("umount: %+v", got)
	}
	f.stop()
	if end := <-ended; end.status != exitOK {
		t.Errorf("first mount ended with %+v", end)
	}
}
