package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/version"
)

//nolint:paralleltest // it replaces the exit variable and os.Args, which every other test could read
func TestMainRunsTheProgram(t *testing.T) {
	oldExit, oldArgs := exit, os.Args
	t.Cleanup(func() { exit, os.Args = oldExit, oldArgs })

	status := -1
	exit = func(code int) { status = code }
	os.Args = []string{"splicefs", "--version"}

	main()

	if status != exitOK {
		t.Errorf("exit status = %d, want %d", status, exitOK)
	}
}

// defaultDir is what the fake backend gives as the default configuration directory.
const defaultDir = "/etc/default"

// calls records what the commands asked of the client library.
type calls struct {
	loadDir     string
	runCfg      *config.Config
	runTargets  []string
	runOpts     app.Options
	socketPath  string
	umountArgs  []string
	umountForce bool
	statusCalls int
}

// fakeBackend returns a backend that records its calls in the returned value and
// answers with the given errors.
func fakeBackend(cfg *config.Config, loadErr, runErr, umountErr, statusErr error, report app.Report) (backend, *calls) {
	recorded := &calls{}
	return backend{
		loadConfig: func(dir string) (*config.Config, error) {
			recorded.loadDir = dir
			return cfg, loadErr
		},
		defaultConfigDir: func() (string, error) { return defaultDir, nil },
		run: func(ctx context.Context, cfg *config.Config, targets []string, opts app.Options) error {
			recorded.runCfg, recorded.runTargets, recorded.runOpts = cfg, targets, opts
			opts.Logger.DebugContext(ctx, "debug-line")
			opts.Logger.InfoContext(ctx, "info-line")
			return runErr
		},
		unmount: func(_ context.Context, socketPath string, targets []string, force bool) error {
			recorded.socketPath, recorded.umountArgs, recorded.umountForce = socketPath, targets, force
			return umountErr
		},
		status: func(_ context.Context, socketPath string) (app.Report, error) {
			recorded.socketPath = socketPath
			recorded.statusCalls++
			return report, statusErr
		},
		mounter: nil,
		notify:  func(string) error { return nil },
	}, recorded
}

// executeWith runs the program with the given backend and returns its exit status and output.
func executeWith(b backend, args ...string) result {
	var out, errOut bytes.Buffer
	status := execute(context.Background(), args, &out, &errOut, b)
	return result{status: status, stdout: out.String(), stderr: errOut.String()}
}

// execFake runs the program with a fake backend and returns what executeWith does.
func execFake(args ...string) result {
	b, _ := fakeBackend(&config.Config{}, nil, nil, nil, nil, app.Report{})
	return executeWith(b, args...)
}

func TestRunUsesTheSystemBackend(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	argv := []string{"splicefs", "--version"}
	status := run(context.Background(), argv, strings.NewReader(""), &out, &errOut)
	if want := "splicefs " + version.String() + "\n"; status != exitOK || out.String() != want || errOut.Len() != 0 {
		t.Errorf("status %d stdout %q stderr %q, want 0 %q", status, out.String(), errOut.String(), want)
	}
}

func TestSystemBackend(t *testing.T) {
	t.Parallel()
	b := systemBackend()
	set := map[string]bool{
		"loadConfig": b.loadConfig != nil, "defaultConfigDir": b.defaultConfigDir != nil,
		"run": b.run != nil, "unmount": b.unmount != nil, "status": b.status != nil,
		"mounter": b.mounter != nil, "notify": b.notify != nil, "requestMount": b.requestMount != nil,
		"getenv": b.getenv != nil, "abs": b.abs != nil,
	}
	for name, isSet := range set {
		if !isSet {
			t.Errorf("the system backend has no %s", name)
		}
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()
	helpRequests := [][]string{
		{"-h"},
		{"--help"},
		{"mount", "--help"},
		{"umount", "-h"},
		{"status", "--help"},
		{"--config", "/x", "status", "-h"},
	}
	for _, args := range helpRequests {
		ran := execFake(args...)
		if ran.status != exitOK || ran.stdout != usage || ran.stderr != "" {
			t.Errorf("%v: status %d stdout %q stderr %q", args, ran.status, ran.stdout, ran.stderr)
		}
	}
}

func TestBadUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "missing command"},
		{"only global flags", []string{"--config", "/x"}, "missing command"},
		{"unknown command", []string{"frobnicate"}, `unknown command "frobnicate"`},
		{"unknown global flag", []string{"--bogus", "mount"}, "flag provided but not defined: -bogus"},
		{"missing value", []string{"--config"}, "flag needs an argument"},
		{"bad log level", []string{"--log-level", "loud", "mount"}, `invalid --log-level "loud"`},
		{"log level slog accepts but we do not", []string{"--log-level", "info+2", "mount"}, `invalid --log-level "info+2"`},
		{"unknown mount flag", []string{"mount", "--force"}, "flag provided but not defined: -force"},
		{"unknown umount flag", []string{"umount", "--json"}, "flag provided but not defined: -json"},
		{"unknown status flag", []string{"status", "--force"}, "flag provided but not defined: -force"},
		{"status takes no targets", []string{"status", "/mnt/a"}, `unexpected argument "/mnt/a"`},
		{"config without a word", []string{"config"}, "config needs one of: check, show"},
		{"config with an unknown word", []string{"config", "fix"}, "config needs one of: check, show"},
		{"config with two words", []string{"config", "check", "show"}, "config needs one of"},
		{"unknown config flag", []string{"config", "--json", "check"}, "not defined: -json"},
		{"global flag after the command", []string{"mount", "--config", "/x"}, "flag provided but not defined: -config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ran := execFake(tt.args...)
			if ran.status != exitUsage || ran.stdout != "" {
				t.Errorf("status %d stdout %q", ran.status, ran.stdout)
			}
			isUsageError := strings.HasPrefix(ran.stderr, "splicefs: ") && strings.HasSuffix(ran.stderr, usage)
			if !isUsageError || !strings.Contains(ran.stderr, tt.want) {
				t.Errorf("stderr = %q", ran.stderr)
			}
		})
	}
}

func TestVersionWinsOverACommand(t *testing.T) {
	t.Parallel()
	ran := execFake("--version", "mount")
	if want := "splicefs " + version.String() + "\n"; ran.status != exitOK || ran.stdout != want {
		t.Errorf("status %d stdout %q", ran.status, ran.stdout)
	}
}

// mountedWith is what a mount command handed to the client library.
type mountedWith struct {
	loadDir   string
	configDir string
	targets   []string
}

func TestMountLoadsTheConfigurationAndPassesTargets(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Mounts: []config.Mount{{Target: "/mnt/a"}}}
	tests := []struct {
		name string
		args []string
		want mountedWith
	}{
		{"defaults", []string{"mount"}, mountedWith{defaultDir, defaultDir, nil}},
		{
			"targets and options",
			[]string{"--config", "/etc/example", "--log-level", "debug", "mount", "/mnt/a", "/mnt/b"},
			mountedWith{"/etc/example", "/etc/example", []string{"/mnt/a", "/mnt/b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := fakeBackend(cfg, nil, nil, nil, nil, app.Report{})
			ran := executeWith(b, tt.args...)
			if ran.status != exitOK || ran.stdout != "" {
				t.Fatalf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			got := mountedWith{recorded.loadDir, recorded.runOpts.ConfigDir, recorded.runTargets}
			if got.loadDir != tt.want.loadDir || got.configDir != tt.want.configDir ||
				!slices.Equal(got.targets, tt.want.targets) {
				t.Errorf("mounted with %+v, want %+v", got, tt.want)
			}
			if recorded.runCfg != cfg {
				t.Error("run did not get the loaded configuration")
			}
		})
	}
}

func TestMountHandsTheClientItsHooks(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Mounts: []config.Mount{{Target: "/mnt/a"}}}
	b, recorded := fakeBackend(cfg, nil, nil, nil, nil, app.Report{})
	var notified []string
	b.notify = func(state string) error {
		notified = append(notified, state)
		return nil
	}
	if ran := executeWith(b, "mount"); ran.status != exitOK {
		t.Fatalf("status %d stderr %q", ran.status, ran.stderr)
	}
	if recorded.runOpts.Reload == nil {
		t.Error("run got no Reload channel")
	}
	recorded.loadDir = ""
	if got, err := recorded.runOpts.Load(); err != nil || got != cfg || recorded.loadDir != defaultDir {
		t.Errorf("Load = %v, %v from %q, want the configuration of %q", got, err, recorded.loadDir, defaultDir)
	}
	if err := recorded.runOpts.Notify("READY=1"); err != nil || len(notified) != 1 || notified[0] != "READY=1" {
		t.Errorf("Notify is not the backend's: %v %v", err, notified)
	}
}

func TestMountLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantDebug bool
		wantInfo  bool
	}{
		{"info is the default", []string{"mount"}, false, true},
		{"debug shows everything", []string{"--log-level", "debug", "mount"}, true, true},
		{"warn hides info", []string{"--log-level", "warn", "mount"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(&config.Config{}, nil, nil, nil, nil, app.Report{})
			ran := executeWith(b, tt.args...)
			if ran.status != exitOK {
				t.Fatalf("status %d stderr %q", ran.status, ran.stderr)
			}
			if got := strings.Contains(ran.stderr, "debug-line"); got != tt.wantDebug {
				t.Errorf("debug line logged = %v, want %v\n%s", got, tt.wantDebug, ran.stderr)
			}
			if got := strings.Contains(ran.stderr, "info-line"); got != tt.wantInfo {
				t.Errorf("info line logged = %v, want %v\n%s", got, tt.wantInfo, ran.stderr)
			}
			if tt.wantDebug && !strings.Contains(ran.stderr, "level=DEBUG") {
				t.Errorf("log is not text:\n%s", ran.stderr)
			}
		})
	}
}

func TestMountFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		loadErr error
		runErr  error
	}{
		{"configuration", errors.New("invalid client config /x: no mounts"), nil},
		{"run", nil, errors.New("cluster rejected the user or password")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := fakeBackend(&config.Config{}, tt.loadErr, tt.runErr, nil, nil, app.Report{})
			ran := executeWith(b, "mount")
			want := tt.loadErr
			if want == nil {
				want = tt.runErr
			}
			wantStderr := "splicefs: " + want.Error() + "\n"
			if ran.status != exitError || ran.stdout != "" || !strings.HasSuffix(ran.stderr, wantStderr) {
				t.Errorf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if (tt.loadErr != nil) != (recorded.runCfg == nil) {
				t.Errorf("run was called = %v after a configuration error = %v", recorded.runCfg != nil, tt.loadErr)
			}
		})
	}
}

func TestUmount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantDir   string
		wantTargs []string
		wantForce bool
	}{
		{"everything", []string{"umount"}, defaultDir, nil, false},
		{
			"targets",
			[]string{"--config", "/etc/example", "umount", "/mnt/a", "/mnt/b"},
			"/etc/example",
			[]string{"/mnt/a", "/mnt/b"},
			false,
		},
		{"force", []string{"umount", "--force", "/mnt/a"}, defaultDir, []string{"/mnt/a"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := fakeBackend(nil, nil, nil, nil, nil, app.Report{})
			ran := executeWith(b, tt.args...)
			if ran.status != exitOK || ran.stdout != "" || ran.stderr != "" {
				t.Fatalf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if want := app.SocketPath(tt.wantDir); recorded.socketPath != want {
				t.Errorf("socket = %q, want %q", recorded.socketPath, want)
			}
			if !slices.Equal(recorded.umountArgs, tt.wantTargs) || recorded.umountForce != tt.wantForce {
				t.Errorf("unmount(%v, %v), want (%v, %v)", recorded.umountArgs, recorded.umountForce, tt.wantTargs, tt.wantForce)
			}
			if recorded.loadDir != "" {
				t.Error("umount loaded the configuration; it only needs the directory")
			}
		})
	}
}

func TestUmountFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantHint bool
	}{
		{"not running", app.ErrNotRunning, false},
		{"in use", errors.Join(errors.New("/mnt/a: mount is in use"), app.ErrBusy), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(nil, nil, nil, tt.err, nil, app.Report{})
			ran := executeWith(b, "umount")
			if ran.status != exitError || ran.stdout != "" || !strings.HasPrefix(ran.stderr, "splicefs: ") {
				t.Errorf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if got := strings.Contains(ran.stderr, `use "umount --force"`); got != tt.wantHint {
				t.Errorf("force hint shown = %v, want %v\n%s", got, tt.wantHint, ran.stderr)
			}
		})
	}
}

// sampleReport is a report with every kind of row: mounts, clusters with and without nodes, caches.
func sampleReport() app.Report {
	return app.Report{
		Mounts: []app.MountReport{
			{Target: "/mnt/files", Source: "splicefs:example/", State: app.StateMounted},
			{Target: "/srv/movies", Source: "splicefs:example/media/movies", State: app.StateEnded},
		},
		Clusters: []app.ClusterReport{
			{Name: "example", Nodes: []app.NodeReport{
				{Name: "east.example.com", BytesPerSecond: 48_200_000, Measured: true},
				{Name: "west.example.com"},
			}},
			{Name: "standby", Nodes: []app.NodeReport{}},
		},
		Caches: []app.CacheReport{
			{Dir: "/var/cache/splicefs", Entries: 120, Bytes: 3_400_000_000},
			{Dir: "/data/cache/movies", Entries: 0, Bytes: 0},
		},
	}
}

const sampleText = `MOUNT        STATE    SOURCE
/mnt/files   mounted  splicefs:example/
/srv/movies  ended    splicefs:example/media/movies

CLUSTER example   RATE
east.example.com  48.2 MB/s
west.example.com  not measured

CLUSTER standby  RATE
(no nodes known yet)

CACHE                FILES  SIZE
/var/cache/splicefs  120    3.4 GB
/data/cache/movies   0      0 B
`

func TestStatus(t *testing.T) {
	t.Parallel()
	b, recorded := fakeBackend(nil, nil, nil, nil, nil, sampleReport())
	ran := executeWith(b, "--config", "/etc/example", "status")
	if ran.status != exitOK || ran.stderr != "" {
		t.Fatalf("status %d stderr %q", ran.status, ran.stderr)
	}
	if ran.stdout != sampleText {
		t.Errorf("output:\n%s\nwant:\n%s", ran.stdout, sampleText)
	}
	if want := app.SocketPath("/etc/example"); recorded.socketPath != want || recorded.statusCalls != 1 {
		t.Errorf("socket %q after %d calls, want %q once", recorded.socketPath, recorded.statusCalls, want)
	}
}

func TestStatusJSON(t *testing.T) {
	t.Parallel()
	b, _ := fakeBackend(nil, nil, nil, nil, nil, sampleReport())
	ran := executeWith(b, "status", "--json")
	if ran.status != exitOK || ran.stderr != "" {
		t.Fatalf("status %d stderr %q", ran.status, ran.stderr)
	}
	var got app.Report
	if err := json.Unmarshal([]byte(ran.stdout), &got); err != nil {
		t.Fatalf("%v in %q", err, ran.stdout)
	}
	if !reflect.DeepEqual(got, sampleReport()) {
		t.Errorf("report = %+v, want %+v", got, sampleReport())
	}
	if !strings.HasPrefix(ran.stdout, "{\n  \"mounts\": [\n") {
		t.Errorf("JSON is not indented:\n%s", ran.stdout)
	}
}

func TestStatusWithoutCaches(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	report := app.Report{Mounts: []app.MountReport{{Target: "/mnt/a", Source: "splicefs:x/", State: app.StateMounted}}}
	err := writeReport(&out, report)
	if err != nil {
		t.Fatal(err)
	}
	if want := "MOUNT   STATE    SOURCE\n/mnt/a  mounted  splicefs:x/\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// TestStatusTableWriteFailsAtTheEnd covers a report of one table: the table writer holds
// its rows back until the end, so that is where the failure shows.
func TestStatusTableWriteFailsAtTheEnd(t *testing.T) {
	t.Parallel()
	err := writeReport(failingWriter{}, app.Report{Mounts: []app.MountReport{{Target: "/mnt/a"}}})
	if err == nil || err.Error() != "disk full" {
		t.Errorf("error = %v", err)
	}
}

func TestStatusFailures(t *testing.T) {
	t.Parallel()
	t.Run("no client", func(t *testing.T) {
		t.Parallel()
		b, _ := fakeBackend(nil, nil, nil, nil, app.ErrNotRunning, app.Report{})
		ran := executeWith(b, "status")
		wantStderr := "no client is running for this configuration"
		if ran.status != exitError || ran.stdout != "" || !strings.Contains(ran.stderr, wantStderr) {
			t.Errorf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
		}
	})
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		t.Run("unwritable output "+strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(nil, nil, nil, nil, nil, sampleReport())
			var errOut bytes.Buffer
			status := execute(context.Background(), args, failingWriter{}, &errOut, b)
			if status != exitError || !strings.Contains(errOut.String(), "disk full") {
				t.Errorf("status %d stderr %q", status, errOut.String())
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

var _ io.Writer = failingWriter{}

func TestFormatBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{48_200_000, "48.2 MB"},
		{3_400_000_000, "3.4 GB"},
		{5e12, "5.0 TB"},
		{5e15, "5000.0 TB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.in); got != tt.want {
			t.Errorf("formatBytes(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()
	levels := map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
	}
	for name, want := range levels {
		if got, ok := parseLevel(name); !ok || got != want {
			t.Errorf("parseLevel(%q) = %v, %v", name, got, ok)
		}
	}
}
