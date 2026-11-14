package app

import (
	"context"
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

// wantMountOptions checks the options and the view each mount was started with, in mount order.
func wantMountOptions(t *testing.T, env *harness, wants []mount.Options) {
	t.Helper()
	for i, want := range wants {
		got := env.mounter.opts[i]
		if got.Logger == nil {
			t.Errorf("mount %d has no logger", i)
		}
		got.Logger = nil
		if got != want {
			t.Errorf("mount %d options = %+v, want %+v", i, got, want)
		}
		if fsys, ok := env.mounter.fsys[i].(namedFS); !ok || fsys.name != want.Target {
			t.Errorf("mount %d got the view %+v, want one of %q", i, fsys, want.Target)
		}
	}
}

func TestRunMountsEveryConfiguredMount(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()

	if got, want := env.mounter.targets(), []string{"/mnt/a", "/mnt/b", "/mnt/c"}; !slices.Equal(got, want) {
		t.Fatalf("mounted %v, want %v", got, want)
	}
	if got, want := env.core.connectedClusters(), []string{"example", "other"}; !slices.Equal(got, want) {
		t.Errorf("connected to %v, want one connection per cluster %v", got, want)
	}
	wantMountOptions(t, env, []mount.Options{
		{Target: "/mnt/a", Source: "splicefs:example/media", AllowOther: true},
		{Target: "/mnt/b", Source: "splicefs:example/"},
		{Target: "/mnt/c", Source: "splicefs:other/docs"},
	})

	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
	for _, target := range env.mounter.targets() {
		if calls := env.mounter.get(target).unmountCalls(); !slices.Equal(calls, []bool{false}) {
			t.Errorf("%s unmount calls (force) = %v, want one polite unmount", target, calls)
		}
	}
	if env.core.closeCount() != 1 {
		t.Errorf("core closed %d times, want once", env.core.closeCount())
	}
	if _, err := os.Stat(env.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file after Run: %v, want it removed", err)
	}
}

func TestRunNotifiesReadyThenStopping(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()
	if got := env.notes.got(); !slices.Equal(got, []string{"READY=1"}) {
		t.Fatalf("notifications before stop = %v", got)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if got := env.notes.got(); !slices.Equal(got, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("notifications = %v", got)
	}
}

func TestRunWithoutNotifierOrLogger(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.opts.Notify, env.opts.Logger = nil, nil
	env.cfg.Mounts = env.cfg.Mounts[:1]
	mounter := env.opts.Mounter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env.opts.Mounter = func(fsys core.FS, opts mount.Options) (mount.Mounted, error) {
		m, err := mounter(fsys, opts)
		cancel()
		return m, err
	}
	if err := Run(ctx, env.cfg, nil, env.opts); err != nil {
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
			env := newHarness(t)
			r := env.start(tt.targets...)
			if got := env.mounter.targets(); !slices.Equal(got, tt.want) {
				t.Errorf("mounted %v, want %v", got, tt.want)
			}
			if got := env.core.connectedClusters(); !slices.Equal(got, tt.cluster) {
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
	env := newHarness(t)
	env.cfg.Mounts = []config.Mount{{Cluster: "example", Path: "/", Target: "/mnt/a"}}
	sys := systemHost()
	sys.abs = func(p string) (string, error) { return "/mnt/" + p, nil }
	env.opts.sys = &sys
	r := env.start("a")
	if got := env.mounter.targets(); !slices.Equal(got, []string{"/mnt/a"}) {
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
		{
			name:    "unknown target",
			mounts:  testConfig().Mounts,
			targets: []string{"/mnt/a", "/mnt/nope"},
			want:    "/mnt/nope is not a mount in the configuration",
		},
		{
			name:    "unresolvable target",
			mounts:  testConfig().Mounts,
			targets: []string{"x"},
			abs:     func(string) (string, error) { return "", errors.New("no cwd") },
			want:    `resolve "x": no cwd`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newHarness(t)
			env.cfg.Mounts = tt.mounts
			if tt.abs != nil {
				sys := systemHost()
				sys.abs = tt.abs
				env.opts.sys = &sys
			}
			err := env.runSync(tt.targets...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run = %v, want an error containing %q", err, tt.want)
			}
			if got := env.mounter.targets(); len(got) != 0 {
				t.Errorf("mounted %v before rejecting the selection", got)
			}
			if _, err := os.Stat(env.socket); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a socket was created: %v", err)
			}
		})
	}
}

func TestRunStopsOnBadCredentials(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.core.connectErr["other"] = fmt.Errorf("connect to cluster %q: %w", "other", core.ErrBadCredentials)
	err := env.runSync()
	errIs(t, err, core.ErrBadCredentials)
	// The core package already says which cluster, so the message starts with it once.
	if want := "connect to cluster \"other\": " + core.ErrBadCredentials.Error(); err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if got := env.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v with bad credentials", got)
	}
	if env.core.closeCount() != 1 {
		t.Errorf("core closed %d times", env.core.closeCount())
	}
	if _, err := os.Stat(env.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestRunStopsOnAnIncompatibleServer(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	message := "server files.example.com runs splicefsd 0.3.3, and this client (0.4.4) needs " +
		"splicefsd 0.4 or newer: upgrade the server, or install an older client"
	env.core.connectErr["other"] = fmt.Errorf("connect to cluster %q: %w: %s", "other", core.ErrIncompatible, message)
	err := env.runSync()
	errIs(t, err, core.ErrIncompatible)
	if !strings.Contains(err.Error(), message) {
		t.Errorf("error = %q, want the message %q in it", err, message)
	}
	if got := env.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v with an incompatible server", got)
	}
	if _, err := os.Stat(env.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestRunStopsOnOtherConnectError(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	boom := errors.New("not configured")
	env.core.connectErr["example"] = boom
	errIs(t, env.runSync(), boom)
	if got := env.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v", got)
	}
}

func TestRunMountsWhenClusterIsUnreachable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(env *harness)
	}{
		{"unavailable", func(env *harness) {
			env.core.connectErr["other"] = fmt.Errorf("connect: %w", core.ErrUnavailable)
		}},
		{"connect timeout", func(env *harness) {
			env.core.blockOn["other"] = true
			env.opts.ConnectTimeout = time.Millisecond
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newHarness(t)
			tt.setup(env)
			r := env.start()
			if got := env.mounter.targets(); len(got) != 3 {
				t.Errorf("mounted %v, want all three", got)
			}
			if err := r.stop(); err != nil {
				t.Fatal(err)
			}
			out := env.log.String()
			if !strings.Contains(out, "cluster unreachable") || !strings.Contains(out, "cluster=other") {
				t.Errorf("no warning about the cluster in the log:\n%s", out)
			}
		})
	}
}

func TestRunStoppedWhileConnecting(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.core.blockOn["example"] = true
	r := env.startUnready()
	<-env.core.blocked
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil: a stop request always succeeds", err)
	}
	if got := env.mounter.targets(); len(got) != 0 {
		t.Errorf("mounted %v", got)
	}
}

func TestRunRollsBackWhenAMountFails(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	boom := errors.New("fuse is not available")
	env.mounter.fail["/mnt/c"] = boom
	err := env.runSync()
	errIs(t, err, boom)
	if !strings.Contains(err.Error(), "mount /mnt/c") {
		t.Errorf("error does not name the target: %v", err)
	}
	for _, target := range []string{"/mnt/a", "/mnt/b"} {
		if calls := env.mounter.get(target).unmountCalls(); !slices.Equal(calls, []bool{true}) {
			t.Errorf("%s unmount calls (force) = %v, want one forced unmount", target, calls)
		}
	}
	if env.core.closeCount() != 1 {
		t.Errorf("core closed %d times", env.core.closeCount())
	}
	if got := env.notes.got(); len(got) != 0 {
		t.Errorf("notified %v although nothing became ready", got)
	}
}

func TestRunRollbackReportsAnUnmountFailure(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.mounter.fail["/mnt/b"] = errors.New("no fuse")
	stuck := errors.New("device is stuck")
	env.mounter.configure = func(f *fakeMounted) {
		f.behavior = func(bool) error { return stuck }
		f.endsOnOK = false
	}
	env.opts.limits = stopLimits{graceful: time.Millisecond, force: 20 * time.Millisecond}
	err := env.runSync()
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
	env := newHarness(t)
	boom := errors.New("cluster \"example\" is not configured")
	env.core.viewErr = boom
	err := env.runSync()
	errIs(t, err, boom)
	if err.Error() != boom.Error() {
		t.Errorf("error = %q, want the message of the core package as it is", err)
	}
}

func TestRunReportsACoreError(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	boom := errors.New("cache directory is not writable")
	env.opts.NewCore = func(*config.Config, core.Options) (Core, error) { return nil, boom }
	err := env.runSync()
	errIs(t, err, boom)
	if !strings.HasPrefix(err.Error(), "start client: ") {
		t.Errorf("error = %v", err)
	}
	if _, err := os.Stat(env.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestRunReportsACloseError(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	boom := errors.New("cache close failed")
	env.core.closeErr = boom
	r := env.start()
	errIs(t, r.stop(), boom)
}

func TestRunFailsWhenReadyCannotBeSignalled(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	boom := errors.New("notify socket gone")
	env.notes.fail["READY=1"] = boom
	err := env.runSync()
	errIs(t, err, boom)
	for _, target := range env.mounter.targets() {
		if calls := env.mounter.get(target).unmountCalls(); !slices.Equal(calls, []bool{true}) {
			t.Errorf("%s unmount calls (force) = %v, want a forced unmount", target, calls)
		}
	}
	if _, err := os.Stat(env.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestRunOnlyWarnsWhenStoppingCannotBeSignalled(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.notes.fail["STOPPING=1"] = errors.New("notify socket gone")
	r := env.start()
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if !strings.Contains(env.log.String(), "could not tell the service manager") {
		t.Errorf("no warning in the log:\n%s", env.log.String())
	}
}

func TestRunForcesAMountThatIsInUse(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()
	busy := env.mounter.get("/mnt/b")
	busy.setBehavior(busyUnlessForced)
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil: a stop must always succeed", err)
	}
	if calls := busy.unmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("busy mount unmount calls (force) = %v, want polite then forced", calls)
	}
	if calls := env.mounter.get("/mnt/a").unmountCalls(); !slices.Equal(calls, []bool{false}) {
		t.Errorf("idle mount unmount calls (force) = %v, want only the polite one", calls)
	}
	if !strings.Contains(env.log.String(), "unmount refused, forcing it") {
		t.Errorf("refusal not logged:\n%s", env.log.String())
	}
}

func TestRunReportsAMountThatCannotBeRemoved(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.opts.limits = stopLimits{graceful: time.Millisecond, force: 20 * time.Millisecond}
	r := env.start("/mnt/a")
	stuck := errors.New("device is stuck")
	mounted := env.mounter.get("/mnt/a")
	mounted.setBehavior(func(bool) error { return stuck })
	err := r.stop()
	errIs(t, err, stuck)
	if !strings.Contains(err.Error(), "mount /mnt/a did not stop") {
		t.Errorf("error = %v", err)
	}
	if calls := mounted.unmountCalls(); !slices.Equal(calls, []bool{false, true}) {
		t.Errorf("unmount calls (force) = %v", calls)
	}
}

func TestRunKeepsRunningWhenEveryMountEnds(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()
	for _, target := range env.mounter.targets() {
		env.mounter.get(target).end(nil)
	}
	waitFor(t, "every view to be released", func() bool { return env.core.releasedCount() == 3 })
	select {
	case err := <-r.result:
		t.Fatalf("Run ended with %v after its mounts ended, want it to idle", err)
	default:
	}
	if got := len(env.status().Mounts); got != 0 {
		t.Errorf("report lists %d mounts, want none", got)
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
	if got := env.notes.got(); !slices.Equal(got, []string{"READY=1", "STOPPING=1"}) {
		t.Errorf("notifications = %v", got)
	}
	for _, target := range env.mounter.targets() {
		if calls := env.mounter.get(target).unmountCalls(); len(calls) != 0 {
			t.Errorf("%s was unmounted again: %v", target, calls)
		}
	}
}

func TestRunKeepsRunningWhileOneMountRemains(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()
	env.mounter.get("/mnt/a").end(nil)
	env.mounter.get("/mnt/b").end(nil)
	waitFor(t, "two views to be released", func() bool { return env.core.releasedCount() == 2 })
	report := env.status()
	if len(report.Mounts) != 1 || report.Mounts[0].Target != "/mnt/c" || report.Mounts[0].State != StateMounted {
		t.Errorf("mounts = %+v, want only /mnt/c, mounted", report.Mounts)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if calls := env.mounter.get("/mnt/a").unmountCalls(); len(calls) != 0 {
		t.Errorf("an ended mount was unmounted: %v", calls)
	}
}

func TestRunLogsAMountThatEndsWithAnError(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.cfg.Mounts = env.cfg.Mounts[:2]
	r := env.start()
	lost := errors.New("connection to the kernel was lost")
	env.mounter.get("/mnt/a").end(lost)
	waitFor(t, "the view to be released", func() bool { return env.core.releasedCount() == 1 })
	waitFor(t, "the log line", func() bool { return strings.Contains(env.log.String(), "mount ended with an error") })
	if !strings.Contains(env.log.String(), lost.Error()) {
		t.Errorf("the error is not in the log:\n%s", env.log.String())
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil: the failure was logged when it happened", err)
	}
}

func TestOptionsDefaults(t *testing.T) {
	t.Parallel()
	opts := Options{ConfigDir: "/etc/example"}.withDefaults()
	if opts.Logger == nil || opts.Notify == nil || opts.NewCore == nil || opts.sys == nil {
		t.Errorf("defaults missing: %+v", opts)
	}
	if err := opts.Notify("READY=1"); err != nil {
		t.Errorf("default Notify = %v", err)
	}
	if opts.ConnectTimeout != 15*time.Second {
		t.Errorf("ConnectTimeout = %v", opts.ConnectTimeout)
	}
	if opts.limits.graceful != gracefulStopTimeout || opts.limits.force != forceStopTimeout {
		t.Errorf("limits = %+v", opts.limits)
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
		{
			"configuration directory",
			Options{ConfigDir: "/etc/other", sys: &failing},
			sys.socketPath("/etc/other"), nil,
		},
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
	env := newHarness(t)
	boom := errors.New("home directory unknown")
	sys := fakeSystem(nil, 1000)
	sys.configDir = func() (string, error) { return "", boom }
	env.opts.SocketPath, env.opts.sys = "", &sys
	errIs(t, env.runSync(), boom)
	if got := env.mounter.targets(); len(got) != 0 {
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
		Clusters: map[string]config.Cluster{"example": {
			URL: "https://files.example.com/api", User: "alice", PasswordFile: password, StreamsPerNode: 1,
		}},
		Mounts: []config.Mount{{
			Cluster: "example", Path: "/", Target: dir + "/mnt", Cache: config.Cache{Dir: dir + "/cache"},
		}},
	}
	coreClient, err := newCoreClient(cfg, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := coreClient.Status().Clusters; len(got) != 1 || got[0].Name != "example" {
		t.Errorf("status = %+v", coreClient.Status())
	}
	if err = coreClient.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.Clusters["example"] = config.Cluster{PasswordFile: dir + "/missing"}
	coreClient, err = newCoreClient(cfg, core.Options{})
	if err == nil || coreClient != nil {
		t.Errorf("newCoreClient with a missing password file = %v, %v", coreClient, err)
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
