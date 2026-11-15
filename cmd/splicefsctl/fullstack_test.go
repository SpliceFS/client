package main

import (
	"bytes"
	"context"
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

// fullStack is the real program, from the command line to the control socket, against a
// running client with a stub core client and a stub mount layer behind it.
type fullStack struct {
	t         *testing.T
	configDir string
	mount     *clienttest.Mount
	// stop ends the client, the way a stop signal does.
	stop func()
	// ended receives the error of the client when it ends.
	ended <-chan error
}

// startClient runs the client of a new configuration folder with one mount, and returns when
// the mount is up.
func startClient(t *testing.T) *fullStack {
	t.Helper()
	stack := &fullStack{t: t, configDir: t.TempDir(), mount: clienttest.NewMount()}
	cfg := &config.Config{Mounts: []config.Mount{{Cluster: "example", Path: "/", Target: stack.configDir + "/mnt"}}}
	ready := make(chan struct{})
	ended := make(chan error, 1)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	opts := app.Options{
		ConfigDir: stack.configDir,
		Mounter:   func(core.FS, mount.Options) (mount.Mounted, error) { return stack.mount, nil },
		NewCore:   func(*config.Config, core.Options) (app.Core, error) { return clienttest.Core{}, nil },
		Notify: func(state string) error {
			if state == "READY=1" {
				close(ready)
			}
			return nil
		},
	}
	go func() { ended <- app.Run(ctx, cfg, nil, opts) }()
	<-ready
	stack.stop, stack.ended = stop, ended
	return stack
}

// command runs a splicefsctl command against the running client.
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
	if table.status != cli.ExitOK {
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
	if asJSON.status != cli.ExitOK || !strings.Contains(asJSON.stdout, `"bytes_per_second": 12000000`) {
		t.Errorf("status --json: %+v", asJSON)
	}
}

// requireBusyMountIsLeftAlone checks that a polite umount of a mount in use is refused, with
// the hint about --force, and that the mount is still there afterwards.
func requireBusyMountIsLeftAlone(t *testing.T, stack *fullStack) {
	t.Helper()
	stack.mount.SetBusy(true)
	busy := stack.command("umount")
	if busy.status != cli.ExitError || !strings.Contains(busy.stderr, "mount is in use") ||
		!strings.Contains(busy.stderr, "umount --force") {
		t.Errorf("umount of a busy mount: %+v", busy)
	}
	if got := stack.command("status"); !strings.Contains(got.stdout, "mounted") {
		t.Errorf("the busy mount was not left alone:\n%s", got.stdout)
	}
}

func TestStatusAndUmountAgainstARunningClient(t *testing.T) {
	t.Parallel()
	stack := startClient(t)

	requireStatusReport(t, stack)

	requireBusyMountIsLeftAlone(t, stack)

	if forced := stack.command("umount", "--force", stack.configDir+"/mnt"); forced.status != cli.ExitOK {
		t.Errorf("umount --force: %+v", forced)
	}
	// The client keeps running with no mounts, so that a reload can mount again.
	if got := stack.command("status"); got.status != cli.ExitOK || strings.Contains(got.stdout, "mounted") {
		t.Errorf("status after the last mount was unmounted: %+v", got)
	}
	stack.stop()
	if err := <-stack.ended; err != nil {
		t.Errorf("the client ended with %v after it was stopped", err)
	}
	gone := stack.command("status")
	if gone.status != cli.ExitError || !strings.Contains(gone.stderr, "no client is running for this configuration") {
		t.Errorf("status after the client ended: %+v", gone)
	}
	if calls := stack.mount.UnmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("unmount calls (force) = %v", calls)
	}
}
