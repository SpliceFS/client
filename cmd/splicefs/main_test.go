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
	c := &calls{}
	return backend{
		loadConfig: func(dir string) (*config.Config, error) {
			c.loadDir = dir
			return cfg, loadErr
		},
		run: func(_ context.Context, cfg *config.Config, targets []string, opts app.Options) error {
			c.runCfg, c.runTargets, c.runOpts = cfg, targets, opts
			opts.Logger.Debug("debug-line")
			opts.Logger.Info("info-line")
			return runErr
		},
		unmount: func(_ context.Context, socketPath string, targets []string, force bool) error {
			c.socketPath, c.umountArgs, c.umountForce = socketPath, targets, force
			return umountErr
		},
		status: func(_ context.Context, socketPath string) (app.Report, error) {
			c.socketPath = socketPath
			c.statusCalls++
			return report, statusErr
		},
		mounter: nil,
		notify:  func(string) error { return nil },
	}, c
}

// execute runs the program with the given backend and returns its status and output.
func executeWith(b backend, args ...string) (status int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	status = execute(context.Background(), args, &out, &errOut, b)
	return status, out.String(), errOut.String()
}

func execFake(args ...string) (status int, stdout, stderr string) {
	b, _ := fakeBackend(&config.Config{}, nil, nil, nil, nil, app.Report{})
	return executeWith(b, args...)
}

func TestRunUsesTheSystemBackend(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	status := run(context.Background(), []string{"--version"}, strings.NewReader(""), &out, &errOut)
	if want := "splicefs " + version.String() + "\n"; status != exitOK || out.String() != want || errOut.Len() != 0 {
		t.Errorf("status %d stdout %q stderr %q, want 0 %q", status, out.String(), errOut.String(), want)
	}
}

func TestSystemBackend(t *testing.T) {
	t.Parallel()
	b := systemBackend()
	if b.loadConfig == nil || b.run == nil || b.unmount == nil || b.status == nil || b.mounter == nil || b.notify == nil {
		t.Errorf("incomplete backend: %+v", b)
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-h"}, {"--help"}, {"mount", "--help"}, {"umount", "-h"}, {"status", "--help"}, {"--config", "/x", "status", "-h"}} {
		status, stdout, stderr := execFake(args...)
		if status != exitOK || stdout != usage || stderr != "" {
			t.Errorf("%v: status %d stdout %q stderr %q", args, status, stdout, stderr)
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
		{"global flag after the command", []string{"mount", "--config", "/x"}, "flag provided but not defined: -config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status, stdout, stderr := execFake(tt.args...)
			if status != exitUsage || stdout != "" {
				t.Errorf("status %d stdout %q", status, stdout)
			}
			if !strings.HasPrefix(stderr, "splicefs: ") || !strings.Contains(stderr, tt.want) || !strings.HasSuffix(stderr, usage) {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

func TestVersionWinsOverACommand(t *testing.T) {
	t.Parallel()
	status, stdout, _ := execFake("--version", "mount")
	if want := "splicefs " + version.String() + "\n"; status != exitOK || stdout != want {
		t.Errorf("status %d stdout %q", status, stdout)
	}
}

func TestMount(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Mounts: []config.Mount{{Target: "/mnt/a"}}}
	tests := []struct {
		name      string
		args      []string
		wantDir   string
		wantTargs []string
		wantDebug bool
	}{
		{"defaults", []string{"mount"}, config.DefaultDir, nil, false},
		{"targets and options", []string{"--config", "/etc/example", "--log-level", "debug", "mount", "/mnt/a", "/mnt/b"}, "/etc/example", []string{"/mnt/a", "/mnt/b"}, true},
		{"warn hides info", []string{"--log-level", "warn", "mount"}, config.DefaultDir, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, c := fakeBackend(cfg, nil, nil, nil, nil, app.Report{})
			var notified []string
			b.notify = func(state string) error {
				notified = append(notified, state)
				return nil
			}
			status, stdout, stderr := executeWith(b, tt.args...)
			if status != exitOK || stdout != "" {
				t.Fatalf("status %d stdout %q stderr %q", status, stdout, stderr)
			}
			if c.loadDir != tt.wantDir || c.runOpts.ConfigDir != tt.wantDir {
				t.Errorf("configuration directory = %q and %q, want %q", c.loadDir, c.runOpts.ConfigDir, tt.wantDir)
			}
			if c.runCfg != cfg {
				t.Error("run did not get the loaded configuration")
			}
			if !slices.Equal(c.runTargets, tt.wantTargs) {
				t.Errorf("targets = %v, want %v", c.runTargets, tt.wantTargs)
			}
			if err := c.runOpts.Notify("READY=1"); err != nil || len(notified) != 1 || notified[0] != "READY=1" {
				t.Errorf("Notify is not the backend's: %v %v", err, notified)
			}
			if got := strings.Contains(stderr, "debug-line"); got != tt.wantDebug {
				t.Errorf("debug line logged = %v, want %v\n%s", got, tt.wantDebug, stderr)
			}
			if got, want := strings.Contains(stderr, "info-line"), !strings.Contains(strings.Join(tt.args, " "), "warn"); got != want {
				t.Errorf("info line logged = %v, want %v\n%s", got, want, stderr)
			}
			if tt.wantDebug && !strings.Contains(stderr, "level=DEBUG") {
				t.Errorf("log is not text:\n%s", stderr)
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
			b, c := fakeBackend(&config.Config{}, tt.loadErr, tt.runErr, nil, nil, app.Report{})
			status, stdout, stderr := executeWith(b, "mount")
			want := tt.loadErr
			if want == nil {
				want = tt.runErr
			}
			if status != exitError || stdout != "" || !strings.HasSuffix(stderr, "splicefs: "+want.Error()+"\n") {
				t.Errorf("status %d stdout %q stderr %q", status, stdout, stderr)
			}
			if (tt.loadErr != nil) != (c.runCfg == nil) {
				t.Errorf("run was called = %v after a configuration error = %v", c.runCfg != nil, tt.loadErr)
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
		{"everything", []string{"umount"}, config.DefaultDir, nil, false},
		{"targets", []string{"--config", "/etc/example", "umount", "/mnt/a", "/mnt/b"}, "/etc/example", []string{"/mnt/a", "/mnt/b"}, false},
		{"force", []string{"umount", "--force", "/mnt/a"}, config.DefaultDir, []string{"/mnt/a"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, c := fakeBackend(nil, nil, nil, nil, nil, app.Report{})
			status, stdout, stderr := executeWith(b, tt.args...)
			if status != exitOK || stdout != "" || stderr != "" {
				t.Fatalf("status %d stdout %q stderr %q", status, stdout, stderr)
			}
			if want := app.SocketPath(tt.wantDir); c.socketPath != want {
				t.Errorf("socket = %q, want %q", c.socketPath, want)
			}
			if !slices.Equal(c.umountArgs, tt.wantTargs) || c.umountForce != tt.wantForce {
				t.Errorf("unmount(%v, %v), want (%v, %v)", c.umountArgs, c.umountForce, tt.wantTargs, tt.wantForce)
			}
			if c.loadDir != "" {
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
			status, stdout, stderr := executeWith(b, "umount")
			if status != exitError || stdout != "" || !strings.HasPrefix(stderr, "splicefs: ") {
				t.Errorf("status %d stdout %q stderr %q", status, stdout, stderr)
			}
			if got := strings.Contains(stderr, `use "umount --force"`); got != tt.wantHint {
				t.Errorf("force hint shown = %v, want %v\n%s", got, tt.wantHint, stderr)
			}
		})
	}
}

var sampleReport = app.Report{
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
	b, c := fakeBackend(nil, nil, nil, nil, nil, sampleReport)
	status, stdout, stderr := executeWith(b, "--config", "/etc/example", "status")
	if status != exitOK || stderr != "" {
		t.Fatalf("status %d stderr %q", status, stderr)
	}
	if stdout != sampleText {
		t.Errorf("output:\n%s\nwant:\n%s", stdout, sampleText)
	}
	if want := app.SocketPath("/etc/example"); c.socketPath != want || c.statusCalls != 1 {
		t.Errorf("socket %q after %d calls, want %q once", c.socketPath, c.statusCalls, want)
	}
}

func TestStatusJSON(t *testing.T) {
	t.Parallel()
	b, _ := fakeBackend(nil, nil, nil, nil, nil, sampleReport)
	status, stdout, stderr := executeWith(b, "status", "--json")
	if status != exitOK || stderr != "" {
		t.Fatalf("status %d stderr %q", status, stderr)
	}
	var got app.Report
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("%v in %q", err, stdout)
	}
	if !reflect.DeepEqual(got, sampleReport) {
		t.Errorf("report = %+v, want %+v", got, sampleReport)
	}
	if !strings.HasPrefix(stdout, "{\n  \"mounts\": [\n") {
		t.Errorf("JSON is not indented:\n%s", stdout)
	}
}

func TestStatusWithoutCaches(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := writeReport(&out, app.Report{Mounts: []app.MountReport{{Target: "/mnt/a", Source: "splicefs:x/", State: app.StateMounted}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "MOUNT   STATE    SOURCE\n/mnt/a  mounted  splicefs:x/\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestStatusFailures(t *testing.T) {
	t.Parallel()
	t.Run("no client", func(t *testing.T) {
		t.Parallel()
		b, _ := fakeBackend(nil, nil, nil, nil, app.ErrNotRunning, app.Report{})
		status, stdout, stderr := executeWith(b, "status")
		if status != exitError || stdout != "" || !strings.Contains(stderr, "no client is running for this configuration") {
			t.Errorf("status %d stdout %q stderr %q", status, stdout, stderr)
		}
	})
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		t.Run("unwritable output "+strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(nil, nil, nil, nil, nil, sampleReport)
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
	for name, want := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, ok := parseLevel(name); !ok || got != want {
			t.Errorf("parseLevel(%q) = %v, %v", name, got, ok)
		}
	}
}
