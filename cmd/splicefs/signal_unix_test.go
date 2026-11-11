//go:build unix

package main

import (
	"syscall"
	"testing"
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
