package app

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/core"
	"github.com/SpliceFS/client/mount"
)

func TestRunMountsEveryConfiguredMount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()

	if got, want := h.mounter.targets(), []string{"/mnt/a", "/mnt/b", "/mnt/c"}; !slices.Equal(got, want) {
		t.Fatalf("mounted %v, want %v", got, want)
	}
	if got, want := h.core.connectedClusters(), []string{"example", "other"}; !slices.Equal(got, want) {
		t.Errorf("connected to %v, want one connection per cluster %v", got, want)
	}
	for i, want := range []mount.Options{
		{Target: "/mnt/a", Source: "splicefs:example/media", AllowOther: true},
		{Target: "/mnt/b", Source: "splicefs:example/"},
		{Target: "/mnt/c", Source: "splicefs:other/docs"},
	} {
		got := h.mounter.opts[i]
		if got.Logger == nil {
			t.Errorf("mount %d has no logger", i)
		}
		got.Logger = nil
		if got != want {
			t.Errorf("mount %d options = %+v, want %+v", i, got, want)
		}
		if fsys, _ := h.mounter.fsys[i].(namedFS); fsys.name != want.Target {
			t.Errorf("mount %d got the view of %q", i, fsys.name)
		}
	}

	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
	for _, target := range h.mounter.targets() {
		if calls := h.mounter.get(target).unmountCalls(); !slices.Equal(calls, []bool{false}) {
			t.Errorf("%s unmount calls (force) = %v, want one polite unmount", target, calls)
		}
	}
	if h.core.closeCount() != 1 {
		t.Errorf("core closed %d times, want once", h.core.closeCount())
	}
	if _, err := os.Stat(h.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file after Run: %v, want it removed", err)
	}
}

func TestRunNotifiesReadyThenStopping(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()
	if got := h.notes.got(); !slices.Equal(got, []string{"READY=1"}) {
		t.Fatalf("notifications before stop = %v", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if got := h.notes.got(); !slices.Equal(got, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("notifications = %v", got)
	}
}

func TestRunWithoutNotifierOrLogger(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.opts.Notify, h.opts.Logger = nil, nil
	h.cfg.Mounts = h.cfg.Mounts[:1]
	mounter := h.opts.Mounter
	h.opts.Mounter = func(fsys core.FS, opts mount.Options) (mount.Mounted, error) {
		m, err := mounter(fsys, opts)
		h.mounter.get(opts.Target).end(nil)
		return m, err
	}
	if err := h.runSync(); err != nil {
		t.Fatal(err)
	}
}

func TestRunSelectsTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		targets []string
		want    []string
		cluster []string
	}{
		{"one", []string{"/mnt/c"}, []string{"/mnt/c"}, []string{"other"}},
		{"configuration order", []string{"/mnt/b", "/mnt/a"}, []string{"/mnt/a", "/mnt/b"}, []string{"example"}},
		{"cleaned", []string{"/mnt//b/"}, []string{"/mnt/b"}, []string{"example"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			r := h.start(tt.targets...)
			if got := h.mounter.targets(); !slices.Equal(got, tt.want) {
				t.Errorf("mounted %v, want %v", got, tt.want)
			}
			if got := h.core.connectedClusters(); !slices.Equal(got, tt.cluster) {
				t.Errorf("connected to %v, want %v", got, tt.cluster)
			}
			if err := r.stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunRelativeTargetIsResolved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Mounts = []config.Mount{{Cluster: "example", Path: "/", Target: "/mnt/a"}}
	sys := systemHost()
	sys.abs = func(p string) (string, error) { return "/mnt/" + p, nil }
	h.opts.sys = &sys
	r := h.start("a")
	if got := h.mounter.targets(); !slices.Equal(got, []string{"/mnt/a"}) {
		t.Errorf("mounted %v", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsBadSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mounts  []config.Mount
		targets []string
		abs     func(string) (string, error)
		want    string
	}{
		{"unknown target", testConfig().Mounts, []string{"/mnt/a", "/mnt/nope"}, nil, "/mnt/nope is not a mount in the configuration"},
		{"no mounts", nil, nil, nil, "no mounts"},
		{"unresolvable target", testConfig().Mounts, []string{"x"}, func(string) (string, error) { return "", errors.New("no cwd") }, `resolve "x": no cwd`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.cfg.Mounts = tt.mounts
			if tt.abs != nil {
				sys := systemHost()
				sys.abs = tt.abs
				h.opts.sys = &sys
			}
			err := h.runSync(tt.targets...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run = %v, want an error containing %q", err, tt.want)
			}
			if got := h.mounter.targets(); len(got) != 0 {
				t.Errorf("mounted %v before rejecting the selection", got)
			}
			if _, err := os.Stat(h.socket); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a socket was created: %v", err)
			}
		})
	}
}

func TestRunStopsOnBadCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.core.connectErr["other"] = fmt.Errorf("connect to cluster %q: %w", "other", core.ErrBadCredentials)
	err := h.runSync()
	errIs(t, err, core.ErrBadCredentials)
	if got := h.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v with bad credentials", got)
	}
	if h.core.closeCount() != 1 {
		t.Errorf("core closed %d times", h.core.closeCount())
	}
	if _, err := os.Stat(h.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestRunStopsOnOtherConnectError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("not configured")
	h.core.connectErr["example"] = boom
	errIs(t, h.runSync(), boom)
	if got := h.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v", got)
	}
}

func TestRunMountsWhenClusterIsUnreachable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(h *harness)
	}{
		{"unavailable", func(h *harness) {
			h.core.connectErr["other"] = fmt.Errorf("connect: %w", core.ErrUnavailable)
		}},
		{"connect timeout", func(h *harness) {
			h.core.blockOn["other"] = true
			h.opts.ConnectTimeout = time.Millisecond
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			tt.setup(h)
			r := h.start()
			if got := h.mounter.targets(); len(got) != 3 {
				t.Errorf("mounted %v, want all three", got)
			}
			if err := r.stop(); err != nil {
				t.Fatal(err)
			}
			if out := h.log.String(); !strings.Contains(out, "cluster unreachable") || !strings.Contains(out, "cluster=other") {
				t.Errorf("no warning about the cluster in the log:\n%s", out)
			}
		})
	}
}

func TestRunStoppedWhileConnecting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.core.blockOn["example"] = true
	r := h.startUnready()
	<-h.core.blocked
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil: a stop request always succeeds", err)
	}
	if got := h.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v", got)
	}
}

func TestRunRollsBackWhenAMountFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("fuse is not available")
	h.mounter.fail["/mnt/c"] = boom
	err := h.runSync()
	errIs(t, err, boom)
	if !strings.Contains(err.Error(), "mount /mnt/c") {
		t.Errorf("error does not name the target: %v", err)
	}
	for _, target := range []string{"/mnt/a", "/mnt/b"} {
		if calls := h.mounter.get(target).unmountCalls(); !slices.Equal(calls, []bool{true}) {
			t.Errorf("%s unmount calls (force) = %v, want one forced unmount", target, calls)
		}
	}
	if h.core.closeCount() != 1 {
		t.Errorf("core closed %d times", h.core.closeCount())
	}
	if got := h.notes.got(); len(got) != 0 {
		t.Errorf("notified %v although nothing became ready", got)
	}
}

func TestRunRollbackReportsAnUnmountFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mounter.fail["/mnt/b"] = errors.New("no fuse")
	stuck := errors.New("device is stuck")
	h.mounter.configure = func(f *fakeMounted) {
		f.behavior = func(bool) error { return stuck }
		f.endsOnOK = false
	}
	h.opts.limits = stopLimits{graceful: time.Millisecond, force: 20 * time.Millisecond}
	err := h.runSync()
	errIs(t, err, stuck)
	if !strings.Contains(err.Error(), "no fuse") || !strings.Contains(err.Error(), "did not stop") {
		t.Errorf("error = %v", err)
	}
}

func TestRunRefusesToStartTwice(t *testing.T) {
	t.Parallel()
	first := newHarness(t)
	r := first.start()
	second := newHarness(t)
	second.opts.SocketPath = first.socket
	err := second.runSync()
	errIs(t, err, ErrAlreadyRunning)
	if got := second.mounter.targets(); len(got) != 0 {
		t.Errorf("the second client mounted %v", got)
	}
	if got := first.status().Mounts; len(got) != 3 {
		t.Errorf("the first client was disturbed: %+v", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestRunReportsAViewError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("cluster \"example\" is not configured")
	h.core.viewErr = boom
	errIs(t, h.runSync(), boom)
}

func TestRunReportsACoreError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("cache directory is not writable")
	h.opts.NewCore = func(*config.Config, core.Options) (Core, error) { return nil, boom }
	err := h.runSync()
	errIs(t, err, boom)
	if !strings.HasPrefix(err.Error(), "start client: ") {
		t.Errorf("error = %v", err)
	}
	if _, err := os.Stat(h.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestRunReportsACloseError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("cache close failed")
	h.core.closeErr = boom
	r := h.start()
	errIs(t, r.stop(), boom)
}

func TestRunFailsWhenReadyCannotBeSignalled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("notify socket gone")
	h.notes.fail["READY=1"] = boom
	err := h.runSync()
	errIs(t, err, boom)
	for _, target := range h.mounter.targets() {
		if calls := h.mounter.get(target).unmountCalls(); !slices.Equal(calls, []bool{true}) {
			t.Errorf("%s unmount calls (force) = %v, want a forced unmount", target, calls)
		}
	}
	if _, err := os.Stat(h.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestRunOnlyWarnsWhenStoppingCannotBeSignalled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.notes.fail["STOPPING=1"] = errors.New("notify socket gone")
	r := h.start()
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if !strings.Contains(h.log.String(), "could not tell the service manager") {
		t.Errorf("no warning in the log:\n%s", h.log.String())
	}
}

func TestRunForcesAMountThatIsInUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()
	busy := h.mounter.get("/mnt/b")
	busy.setBehavior(busyUnlessForced)
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil: a stop must always succeed", err)
	}
	if calls := busy.unmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("busy mount unmount calls (force) = %v, want polite then forced", calls)
	}
	if calls := h.mounter.get("/mnt/a").unmountCalls(); !slices.Equal(calls, []bool{false}) {
		t.Errorf("idle mount unmount calls (force) = %v, want only the polite one", calls)
	}
	if !strings.Contains(h.log.String(), "unmount refused, forcing it") {
		t.Errorf("refusal not logged:\n%s", h.log.String())
	}
}

func TestRunReportsAMountThatCannotBeRemoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.opts.limits = stopLimits{graceful: time.Millisecond, force: 20 * time.Millisecond}
	r := h.start("/mnt/a")
	stuck := errors.New("device is stuck")
	m := h.mounter.get("/mnt/a")
	m.setBehavior(func(bool) error { return stuck })
	err := r.stop()
	errIs(t, err, stuck)
	if !strings.Contains(err.Error(), "mount /mnt/a did not stop") {
		t.Errorf("error = %v", err)
	}
	if calls := m.unmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("unmount calls (force) = %v", calls)
	}
}

func TestRunEndsWhenEveryMountEndsByItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()
	for _, target := range h.mounter.targets() {
		h.mounter.get(target).end(nil)
	}
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v, want nil after outside unmounts", err)
	}
	if got := h.notes.got(); !slices.Equal(got, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("notifications = %v", got)
	}
	for _, target := range h.mounter.targets() {
		if calls := h.mounter.get(target).unmountCalls(); len(calls) != 0 {
			t.Errorf("%s was unmounted again: %v", target, calls)
		}
	}
}

func TestRunKeepsRunningWhileOneMountRemains(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()
	h.mounter.get("/mnt/a").end(nil)
	h.mounter.get("/mnt/b").end(nil)
	report := h.status()
	states := []string{report.Mounts[0].State, report.Mounts[1].State, report.Mounts[2].State}
	if !slices.Equal(states, []string{StateEnded, StateEnded, StateMounted}) {
		t.Errorf("states = %v", states)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if calls := h.mounter.get("/mnt/a").unmountCalls(); len(calls) != 0 {
		t.Errorf("an ended mount was unmounted: %v", calls)
	}
}

func TestRunReturnsMountErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Mounts = h.cfg.Mounts[:2]
	r := h.start()
	lost, gone := errors.New("connection to the kernel was lost"), errors.New("server failed")
	h.mounter.get("/mnt/a").end(lost)
	h.mounter.get("/mnt/b").end(gone)
	err := r.wait()
	errIs(t, err, lost)
	errIs(t, err, gone)
	if !strings.Contains(err.Error(), "mount /mnt/a: ") {
		t.Errorf("error does not name the mount: %v", err)
	}
}

func TestOptionsDefaults(t *testing.T) {
	t.Parallel()
	o := Options{ConfigDir: "/etc/example"}.withDefaults()
	if o.Logger == nil || o.Notify == nil || o.NewCore == nil || o.sys == nil {
		t.Errorf("defaults missing: %+v", o)
	}
	if err := o.Notify("READY=1"); err != nil {
		t.Errorf("default Notify = %v", err)
	}
	if o.ConnectTimeout != 15*time.Second {
		t.Errorf("ConnectTimeout = %v", o.ConnectTimeout)
	}
	if o.limits.graceful != gracefulStopTimeout || o.limits.force != forceStopTimeout {
		t.Errorf("limits = %+v", o.limits)
	}
	kept := Options{ConnectTimeout: time.Second}.withDefaults()
	if kept.ConnectTimeout != time.Second {
		t.Errorf("explicit settings were replaced: %+v", kept)
	}
}

func TestControlSocket(t *testing.T) {
	t.Parallel()
	sys := fakeSystem(nil, 1000)
	sys.configDir = func() (string, error) { return "/etc/example", nil }
	failing := fakeSystem(nil, 1000)
	boom := errors.New("home directory unknown")
	failing.configDir = func() (string, error) { return "", boom }
	tests := []struct {
		name    string
		opts    Options
		want    string
		wantErr error
	}{
		{"explicit socket", Options{SocketPath: "/x.sock", sys: &failing}, "/x.sock", nil},
		{"configuration directory", Options{ConfigDir: "/etc/other", sys: &failing},
			sys.socketPath("/etc/other"), nil},
		{"default directory", Options{sys: &sys}, sys.socketPath("/etc/example"), nil},
		{"default directory unknown", Options{sys: &failing}, "", boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.opts.controlSocket()
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Errorf("controlSocket() = %q, %v, want %q, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestSystemHostFindsTheDefaultConfigDir(t *testing.T) {
	t.Parallel()
	want, wantErr := config.DefaultConfigDir()
	if got, err := systemHost().configDir(); got != want || (err == nil) != (wantErr == nil) {
		t.Errorf("configDir() = %q, %v, want %q, %v", got, err, want, wantErr)
	}
}

func TestRunReportsAnUnknownDefaultConfigDir(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("home directory unknown")
	sys := fakeSystem(nil, 1000)
	sys.configDir = func() (string, error) { return "", boom }
	h.opts.SocketPath, h.opts.sys = "", &sys
	errIs(t, h.runSync(), boom)
	if got := h.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v without a control socket", got)
	}
}

func TestDefaultCore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	password := dir + "/password"
	if err := os.WriteFile(password, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Clusters: map[string]config.Cluster{"example": {URL: "https://files.example.com/api", User: "alice", PasswordFile: password, StreamsPerNode: 1}},
		Mounts:   []config.Mount{{Cluster: "example", Path: "/", Target: dir + "/mnt", Cache: config.Cache{Dir: dir + "/cache"}}},
	}
	c, err := newCoreClient(cfg, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Status().Clusters; len(got) != 1 || got[0].Name != "example" {
		t.Errorf("status = %+v", c.Status())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.Clusters["example"] = config.Cluster{PasswordFile: dir + "/missing"}
	c, err = newCoreClient(cfg, core.Options{})
	if err == nil || c != nil {
		t.Errorf("newCoreClient with a missing password file = %v, %v", c, err)
	}
}

func TestIsBusy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("unmount: %w", syscall.EBUSY), true},
		{errors.New("umount: exit status 1: umount(/mnt/a): Resource busy"), true},
		{errors.New("permission denied"), false},
	}
	for _, tt := range tests {
		if got := isBusy(tt.err); got != tt.want {
			t.Errorf("isBusy(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
