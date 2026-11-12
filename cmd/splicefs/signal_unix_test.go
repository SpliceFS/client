//go:build unix

package main

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
)

// The tests in this file send a real signal to the test process. They are not parallel,
// so that no other command of this test binary is running when the signal arrives.
func TestMountStopsOnSignal(t *testing.T) {
	for name, sig := range map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT} {
		t.Run(name, func(t *testing.T) {
			f := newFullStack(t, nil, nil)
			ended := f.startMount()
			<-f.ready
			if status := f.command("status"); status.status != exitOK {
				t.Fatalf("status: %+v", status)
			}
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			end := <-ended
			if end.status != exitOK {
				t.Errorf("exit status = %d, want 0\n%s", end.status, end.stderr)
			}
			if calls := f.mount.unmountCalls(); len(calls) != 1 || calls[0] {
				t.Errorf("unmount calls (force) = %v, want one polite unmount", calls)
			}
		})
	}
}

// TestMountReceivesSIGHUP sends SIGHUP while the run is active, which is the only time the
// command has asked for it. A SIGHUP outside a mount would end the test process.
func TestMountReceivesSIGHUP(t *testing.T) {
	cfg := &config.Config{}
	b, c := fakeBackend(cfg, nil, nil, nil, nil, app.Report{})
	b.run = func(_ context.Context, _ *config.Config, _ []string, opts app.Options) error {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
			return err
		}
		select {
		case sig := <-opts.Reload:
			if sig != syscall.SIGHUP {
				return errors.New("Reload received " + sig.String())
			}
		case <-time.After(5 * time.Second):
			return errors.New("SIGHUP did not reach Reload")
		}
		c.loadDir = ""
		if got, err := opts.Load(); err != nil || got != cfg {
			return errors.New("Load did not return the configuration")
		}
		return nil
	}
	status, _, stderr := executeWith(b, "--config", "/etc/example", "mount")
	if status != exitOK {
		t.Fatalf("status %d\n%s", status, stderr)
	}
	if c.loadDir != "/etc/example" {
		t.Errorf("Load read %q, want the configuration directory of the command", c.loadDir)
	}
}
