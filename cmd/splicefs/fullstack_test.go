package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/internal/cli"
	"github.com/SpliceFS/client/internal/clienttest"
	"github.com/SpliceFS/client/mount"
)

// fullStack is the real program, from the command line to the control socket, with a stub
// core client and a stub mount layer. The control socket is read with the same functions
// the splicefsctl program uses.
type fullStack struct {
	t         *testing.T
	configDir string
	mount     *clienttest.Mount
	ready     chan struct{}
	backend   backend
	// stop ends the service that startService started, the way a stop signal does.
	stop context.CancelFunc
}

func newFullStack(t *testing.T, fsys core.FS, mounter app.Mounter) *fullStack {
	t.Helper()
	stack := &fullStack{
		t:         t,
		configDir: t.TempDir(),
		mount:     clienttest.NewMount(),
		ready:     make(chan struct{}),
	}
	cfg := &config.Config{Mounts: []config.Mount{{Cluster: "example", Path: "/", Target: stack.configDir + "/mnt"}}}
	b := systemBackend()
	b.loadConfig = func(string) (*config.Config, error) { return cfg, nil }
	realRun := b.run
	b.run = func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error {
		opts.NewCore = func(*config.Config, core.Options) (app.Core, error) { return clienttest.Core{FS: fsys}, nil }
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

// startService runs "splicefs" and returns a channel that receives its exit status
// and stderr when it ends.
func (stack *fullStack) startService() <-chan result {
	ended := make(chan result, 1)
	ctx, stop := context.WithCancel(context.Background())
	stack.stop = stop
	stack.t.Cleanup(stop)
	go func() {
		var out, errOut bytes.Buffer
		status := execute(ctx, []string{"--config", stack.configDir}, &out, &errOut, stack.backend)
		ended <- result{status: status, stdout: out.String(), stderr: errOut.String()}
	}()
	return ended
}

// socketPath is the control socket of the running client.
func (stack *fullStack) socketPath() string { return app.SocketPath(stack.configDir) }

// report asks the running client for its status.
func (stack *fullStack) report() (app.Report, error) {
	return app.Status(context.Background(), stack.socketPath())
}

// unmount asks the running client to unmount every mount.
func (stack *fullStack) unmount(force bool) error {
	return app.Unmount(context.Background(), stack.socketPath(), nil, force)
}

// result is what one run of the program produced.
type result struct {
	status int
	stdout string
	stderr string
}

func TestRunningClientAnswersOnTheControlSocket(t *testing.T) {
	t.Parallel()
	stack := newFullStack(t, nil, nil)
	ended := stack.startService()
	<-stack.ready

	report, err := stack.report()
	if err != nil || len(report.Mounts) != 1 || report.Mounts[0].State != app.StateMounted ||
		report.Mounts[0].Target != stack.configDir+"/mnt" || report.Mounts[0].Source != "splicefs:example/" {
		t.Fatalf("status = %+v, %v", report, err)
	}
	if len(report.Clusters) != 1 || report.Clusters[0].Nodes[0].BytesPerSecond != 12_000_000 ||
		report.Caches[0].Dir != "/var/cache/splicefs" {
		t.Errorf("status lacks what the stub client measured: %+v", report)
	}

	stack.mount.SetBusy(true)
	if err := stack.unmount(false); !errors.Is(err, app.ErrBusy) {
		t.Errorf("unmount of a busy mount = %v, want a refusal because it is in use", err)
	}
	if got, _ := stack.report(); got.Mounts[0].State != app.StateMounted {
		t.Errorf("the busy mount was not left alone: %+v", got)
	}

	if err := stack.unmount(true); err != nil {
		t.Errorf("forced unmount: %v", err)
	}
	// The client keeps running with no mounts, so that a reload can mount again.
	if got, err := stack.report(); err != nil || slices.ContainsFunc(got.Mounts, isMounted) {
		t.Errorf("status after the last mount was unmounted: %+v, %v", got, err)
	}
	stack.stop()
	if end := <-ended; end.status != cli.ExitOK {
		t.Errorf("service ended with %+v after it was stopped", end)
	}
	if _, err := stack.report(); !errors.Is(err, app.ErrNotRunning) {
		t.Errorf("status after the client ended = %v, want ErrNotRunning", err)
	}
	if calls := stack.mount.UnmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("unmount calls (force) = %v", calls)
	}
}

func isMounted(m app.MountReport) bool { return m.State == app.StateMounted }

func TestSecondServiceIsRefused(t *testing.T) {
	t.Parallel()
	stack := newFullStack(t, nil, nil)
	ended := stack.startService()
	<-stack.ready

	var out, errOut bytes.Buffer
	status := execute(context.Background(), []string{"--config", stack.configDir}, &out, &errOut, stack.backend)
	if status != cli.ExitError || !strings.Contains(errOut.String(), "a client is already running for this configuration") {
		t.Errorf("second service: status %d stderr %q", status, errOut.String())
	}
	if err := stack.unmount(true); err != nil {
		t.Errorf("unmount: %v", err)
	}
	stack.stop()
	if end := <-ended; end.status != cli.ExitOK {
		t.Errorf("first service ended with %+v", end)
	}
}
