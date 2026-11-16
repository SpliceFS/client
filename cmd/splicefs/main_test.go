package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cli"
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

	if status != cli.ExitOK {
		t.Errorf("exit status = %d, want %d", status, cli.ExitOK)
	}
}

// defaultDir is what the fake backend gives as the default configuration directory.
const defaultDir = "/etc/default"

// calls records what the commands asked of the client library.
type calls struct {
	loadDir    string
	runCfg     *config.Config
	runTargets []string
	runOpts    app.Options
}

// fakeBackend returns a backend that records its calls in the returned value and
// answers with the given errors.
func fakeBackend(cfg *config.Config, loadErr, runErr error) (backend, *calls) {
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
	b, _ := fakeBackend(&config.Config{}, nil, nil)
	return executeWith(b, args...)
}

func TestRunUsesTheSystemBackend(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	argv := []string{"splicefs", "--version"}
	status := run(context.Background(), argv, strings.NewReader(""), &out, &errOut)
	if want := "splicefs " + version.String() + "\n"; status != cli.ExitOK || out.String() != want || errOut.Len() != 0 {
		t.Errorf("status %d stdout %q stderr %q, want 0 %q", status, out.String(), errOut.String(), want)
	}
}

func TestSystemBackend(t *testing.T) {
	t.Parallel()
	b := systemBackend()
	set := map[string]bool{
		"loadConfig": b.loadConfig != nil, "defaultConfigDir": b.defaultConfigDir != nil,
		"run": b.run != nil, "mounter": b.mounter != nil, "notify": b.notify != nil,
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
		{"--log-level", "debug", "--help"},
		{"--config", "/x", "config", "-h"},
	}
	for _, args := range helpRequests {
		ran := execFake(args...)
		if ran.status != cli.ExitOK || ran.stdout != usage || ran.stderr != "" {
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
		{"unknown command", []string{"frobnicate"}, `unknown command "frobnicate"`},
		{"mount is not a command", []string{"mount"}, `unknown command "mount"`},
		{"targets are not words", []string{"/mnt/a"}, `unknown command "/mnt/a"`},
		{"umount is the control tool's", []string{"umount"}, `unknown command "umount"`},
		{"status is the control tool's", []string{"status"}, `unknown command "status"`},
		{"unknown global flag", []string{"--bogus"}, "flag provided but not defined: -bogus"},
		{"missing value", []string{"--config"}, "flag needs an argument"},
		{"missing target", []string{"--target"}, "flag needs an argument"},
		{"bad log level", []string{"--log-level", "loud"}, `invalid --log-level "loud"`},
		{"log level slog accepts but we do not", []string{"--log-level", "info+2"}, `invalid --log-level "info+2"`},
		{"umount is the control tool's", []string{"--force"}, "flag provided but not defined: -force"},
		{"config without a word", []string{"config"}, "config needs one of: check, show"},
		{"config with an unknown word", []string{"config", "fix"}, "config needs one of: check, show"},
		{"config with two words", []string{"config", "check", "show"}, "config needs one of"},
		{"unknown config flag", []string{"config", "--json", "check"}, "not defined: -json"},
		{"global flag after the command", []string{"config", "--config", "/x"}, "flag provided but not defined: -config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ran := execFake(tt.args...)
			if ran.status != cli.ExitUsage || ran.stdout != "" {
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
	ran := execFake("--version", "config", "check")
	if want := "splicefs " + version.String() + "\n"; ran.status != cli.ExitOK || ran.stdout != want {
		t.Errorf("status %d stdout %q", ran.status, ran.stdout)
	}
}

// servedWith is what the client was started with.
type servedWith struct {
	loadDir   string
	configDir string
	targets   []string
}

func TestServiceLoadsTheConfigurationAndPassesTargets(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Mounts: []config.Mount{{Target: "/mnt/a"}}}
	tests := []struct {
		name string
		args []string
		want servedWith
	}{
		{"defaults", nil, servedWith{defaultDir, defaultDir, nil}},
		{
			"targets and options",
			[]string{"--config", "/etc/example", "--log-level", "debug", "--target", "/mnt/a", "--target", "/mnt/b"},
			servedWith{"/etc/example", "/etc/example", []string{"/mnt/a", "/mnt/b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := fakeBackend(cfg, nil, nil)
			ran := executeWith(b, tt.args...)
			if ran.status != cli.ExitOK || ran.stdout != "" {
				t.Fatalf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			got := servedWith{recorded.loadDir, recorded.runOpts.ConfigDir, recorded.runTargets}
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

func TestServiceHandsTheClientItsHooks(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Mounts: []config.Mount{{Target: "/mnt/a"}}}
	b, recorded := fakeBackend(cfg, nil, nil)
	var notified []string
	b.notify = func(state string) error {
		notified = append(notified, state)
		return nil
	}
	if ran := executeWith(b); ran.status != cli.ExitOK {
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

func TestServiceLogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantDebug bool
		wantInfo  bool
	}{
		{"info is the default", nil, false, true},
		{"debug shows everything", []string{"--log-level", "debug"}, true, true},
		{"warn hides info", []string{"--log-level", "warn"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(&config.Config{}, nil, nil)
			ran := executeWith(b, tt.args...)
			if ran.status != cli.ExitOK {
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

func TestServiceFailures(t *testing.T) {
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
			b, recorded := fakeBackend(&config.Config{}, tt.loadErr, tt.runErr)
			ran := executeWith(b)
			want := tt.loadErr
			if want == nil {
				want = tt.runErr
			}
			wantStderr := "splicefs: " + want.Error() + "\n"
			if ran.status != cli.ExitError || ran.stdout != "" || !strings.HasSuffix(ran.stderr, wantStderr) {
				t.Errorf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if (tt.loadErr != nil) != (recorded.runCfg == nil) {
				t.Errorf("run was called = %v after a configuration error = %v", recorded.runCfg != nil, tt.loadErr)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

var _ io.Writer = failingWriter{}

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
