package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/internal/cli"
	"github.com/SpliceFS/client/internal/version"
	"github.com/SpliceFS/client/mount/nfs"
)

//nolint:paralleltest // it replaces the exit variable and os.Args, which every other test could read
func TestMainRunsTheProgram(t *testing.T) {
	oldExit, oldArgs := exit, os.Args
	t.Cleanup(func() { exit, os.Args = oldExit, oldArgs })

	status := -1
	exit = func(code int) { status = code }
	os.Args = []string{"splicefsctl", "--version"}

	main()

	if status != cli.ExitOK {
		t.Errorf("exit status = %d, want %d", status, cli.ExitOK)
	}
}

// defaultDir is what the fake backend gives as the default configuration directory.
const defaultDir = "/etc/default"

// calls records what the commands asked of the client library.
type calls struct {
	socketPath  string
	umountArgs  []string
	umountForce bool
	statusCalls int
	// clearTargets is what the clean-up of left-behind mounts was asked for, and clearCalls how
	// often it was asked.
	clearTargets []string
	clearCalls   int
	// mountRequests is every request the mount command and the helper sent.
	mountRequests []app.MountRequest
}

// fakeBackend returns a backend that records its calls in the returned value and
// answers with the given errors.
func fakeBackend(umountErr, statusErr error, report app.Report) (backend, *calls) {
	recorded := &calls{}
	return backend{
		defaultConfigDir: func() (string, error) { return defaultDir, nil },
		unmount: func(_ context.Context, socketPath string, targets []string, force bool) error {
			recorded.socketPath, recorded.umountArgs, recorded.umountForce = socketPath, targets, force
			return umountErr
		},
		clearLeftBehind: func(_ context.Context, socketPath string, targets []string) ([]nfs.Cleared, error) {
			recorded.socketPath, recorded.clearTargets = socketPath, targets
			recorded.clearCalls++
			return nil, nil
		},
		status: func(_ context.Context, socketPath string) (app.Report, error) {
			recorded.socketPath = socketPath
			recorded.statusCalls++
			return report, statusErr
		},
		requestMount: func(_ context.Context, socketPath string, req app.MountRequest) error {
			recorded.socketPath = socketPath
			recorded.mountRequests = append(recorded.mountRequests, req)
			return nil
		},
		getenv: func(string) string { return "" },
		abs:    func(path string) (string, error) { return path, nil },
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
	b, _ := fakeBackend(nil, nil, app.Report{})
	return executeWith(b, args...)
}

func TestRunUsesTheSystemBackend(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	argv := []string{"splicefsctl", "--version"}
	status := run(context.Background(), argv, strings.NewReader(""), &out, &errOut)
	want := "splicefsctl " + version.String() + "\n"
	if status != cli.ExitOK || out.String() != want || errOut.Len() != 0 {
		t.Errorf("status %d stdout %q stderr %q, want 0 %q", status, out.String(), errOut.String(), want)
	}
}

func TestSystemBackend(t *testing.T) {
	t.Parallel()
	b := systemBackend()
	set := map[string]bool{
		"defaultConfigDir": b.defaultConfigDir != nil, "unmount": b.unmount != nil,
		"clearLeftBehind": b.clearLeftBehind != nil, "status": b.status != nil,
		"requestMount": b.requestMount != nil, "getenv": b.getenv != nil, "abs": b.abs != nil,
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
		{"umount", "-h"},
		{"status", "--help"},
		{"--config", "/x", "status", "-h"},
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
		{"no command", nil, "missing command"},
		{"only global flags", []string{"--config", "/x"}, "missing command"},
		{"unknown command", []string{"frobnicate"}, `unknown command "frobnicate"`},
		{"config is the client's", []string{"config", "check"}, `unknown command "config"`},
		{"unknown global flag", []string{"--bogus", "status"}, "flag provided but not defined: -bogus"},
		{"missing value", []string{"--config"}, "flag needs an argument"},
		{"no log level", []string{"--log-level", "info", "status"}, "flag provided but not defined: -log-level"},
		{"unknown umount flag", []string{"umount", "--json"}, "flag provided but not defined: -json"},
		{"unknown status flag", []string{"status", "--force"}, "flag provided but not defined: -force"},
		{"status takes no targets", []string{"status", "/mnt/a"}, `unexpected argument "/mnt/a"`},
		{"global flag after the command", []string{"status", "--config", "/x"}, "flag provided but not defined: -config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ran := execFake(tt.args...)
			if ran.status != cli.ExitUsage || ran.stdout != "" {
				t.Errorf("status %d stdout %q", ran.status, ran.stdout)
			}
			isUsageError := strings.HasPrefix(ran.stderr, "splicefsctl: ") && strings.HasSuffix(ran.stderr, usage)
			if !isUsageError || !strings.Contains(ran.stderr, tt.want) {
				t.Errorf("stderr = %q", ran.stderr)
			}
		})
	}
}

func TestVersionWinsOverACommand(t *testing.T) {
	t.Parallel()
	ran := execFake("--version", "status")
	if want := "splicefsctl " + version.String() + "\n"; ran.status != cli.ExitOK || ran.stdout != want {
		t.Errorf("status %d stdout %q", ran.status, ran.stdout)
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
			b, recorded := fakeBackend(nil, nil, app.Report{})
			ran := executeWith(b, tt.args...)
			if ran.status != cli.ExitOK || ran.stdout != "" || ran.stderr != "" {
				t.Fatalf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if want := app.SocketPath(tt.wantDir); recorded.socketPath != want {
				t.Errorf("socket = %q, want %q", recorded.socketPath, want)
			}
			if !slices.Equal(recorded.umountArgs, tt.wantTargs) || recorded.umountForce != tt.wantForce {
				t.Errorf("unmount(%v, %v), want (%v, %v)", recorded.umountArgs, recorded.umountForce, tt.wantTargs, tt.wantForce)
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
			b, _ := fakeBackend(tt.err, nil, app.Report{})
			ran := executeWith(b, "umount")
			if ran.status != cli.ExitError || ran.stdout != "" || !strings.HasPrefix(ran.stderr, "splicefsctl: ") {
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
	b, recorded := fakeBackend(nil, nil, sampleReport())
	ran := executeWith(b, "--config", "/etc/example", "status")
	if ran.status != cli.ExitOK || ran.stderr != "" {
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
	b, _ := fakeBackend(nil, nil, sampleReport())
	ran := executeWith(b, "status", "--json")
	if ran.status != cli.ExitOK || ran.stderr != "" {
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
		b, _ := fakeBackend(nil, app.ErrNotRunning, app.Report{})
		ran := executeWith(b, "status")
		wantStderr := "no client is running for this configuration"
		if ran.status != cli.ExitError || ran.stdout != "" || !strings.Contains(ran.stderr, wantStderr) {
			t.Errorf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
		}
	})
	for _, args := range [][]string{{"status"}, {"status", "--json"}} {
		t.Run("unwritable output "+strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(nil, nil, sampleReport())
			var errOut bytes.Buffer
			status := execute(context.Background(), args, failingWriter{}, &errOut, b)
			if status != cli.ExitError || !strings.Contains(errOut.String(), "disk full") {
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

// notRunningBackend is a backend whose client is not running and whose clean-up of
// left-behind mounts answers results and err.
func notRunningBackend(results []nfs.Cleared, clearErr error) (backend, *calls) {
	b, recorded := fakeBackend(app.ErrNotRunning, nil, app.Report{})
	record := b.clearLeftBehind
	b.clearLeftBehind = func(ctx context.Context, socketPath string, targets []string) ([]nfs.Cleared, error) {
		_, _ = record(ctx, socketPath, targets) //nolint:errcheck // Only records the call; it answers nothing.
		return results, clearErr
	}
	return b, recorded
}

const noClient = "splicefsctl: no client is running"

// leftBehindCase is one answer of the clean-up of left-behind mounts, and what umount must
// print and return for it.
type leftBehindCase struct {
	name       string
	results    []nfs.Cleared
	clearErr   error
	wantStatus int
	wantOut    string
	wantErr    []string
}

func leftBehindCases() []leftBehindCase {
	return []leftBehindCase{
		{
			name:       "one cleared",
			results:    []nfs.Cleared{{Target: "/mnt/a", Outcome: nfs.OutcomeCleared}},
			wantStatus: cli.ExitOK,
			wantOut:    "removed the mount /mnt/a left behind by a client that is no longer running\n",
		},
		{
			name: "cleared, already gone and served",
			results: []nfs.Cleared{
				{Target: "/mnt/a", Outcome: nfs.OutcomeCleared},
				{Target: "/mnt/b", Outcome: nfs.OutcomeGone},
				{Target: "/mnt/c", Outcome: nfs.OutcomeServed},
				{Target: "/mnt/d", Outcome: nfs.OutcomeCleared},
			},
			wantStatus: cli.ExitOK,
			wantOut: "removed the mount /mnt/a left behind by a client that is no longer running\n" +
				"removed the mount /mnt/d left behind by a client that is no longer running\n",
			wantErr: []string{"/mnt/c is served by a client of another configuration; left alone\n"},
		},
		{
			name: "cleared and failed",
			results: []nfs.Cleared{
				{Target: "/mnt/a", Outcome: nfs.OutcomeCleared},
				{Target: "/mnt/b", Outcome: nfs.OutcomeFailed, Err: errors.New("umount: Resource busy")},
			},
			wantStatus: cli.ExitError,
			wantOut:    "removed the mount /mnt/a left behind by a client that is no longer running\n",
			wantErr:    []string{"splicefsctl: /mnt/b: umount: Resource busy\n"},
		},
		{
			name:       "only failed",
			results:    []nfs.Cleared{{Target: "/mnt/b", Outcome: nfs.OutcomeFailed, Err: errors.New("stuck")}},
			wantStatus: cli.ExitError,
			wantErr:    []string{"splicefsctl: /mnt/b: stuck\n"},
		},
		{
			name:       "damaged record",
			clearErr:   nfs.ErrRecordDamaged,
			wantStatus: cli.ExitError,
			wantErr:    []string{"splicefsctl: " + nfs.ErrRecordDamaged.Error()},
		},
		{
			name:       "nothing recorded",
			wantStatus: cli.ExitError,
			wantErr:    []string{noClient},
		},
		{
			name:       "everything already gone",
			results:    []nfs.Cleared{{Target: "/mnt/a", Outcome: nfs.OutcomeGone}},
			wantStatus: cli.ExitError,
			wantErr:    []string{noClient},
		},
		{
			name:       "only served",
			results:    []nfs.Cleared{{Target: "/mnt/a", Outcome: nfs.OutcomeServed}},
			wantStatus: cli.ExitError,
			wantErr: []string{
				"/mnt/a is served by a client of another configuration; left alone\n", noClient,
			},
		},
	}
}

func TestUmountWithoutAClientClearsWhatWasLeftBehind(t *testing.T) {
	t.Parallel()
	for _, tt := range leftBehindCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := notRunningBackend(tt.results, tt.clearErr)
			ran := executeWith(b, "--config", "/etc/example", "umount", "--force", "/mnt/a")
			if ran.status != tt.wantStatus || ran.stdout != tt.wantOut {
				t.Errorf("status %d stdout %q, want %d %q\nstderr %q",
					ran.status, ran.stdout, tt.wantStatus, tt.wantOut, ran.stderr)
			}
			wantStderr(t, ran.stderr, tt.wantErr)
			if want := app.SocketPath("/etc/example"); recorded.socketPath != want ||
				!slices.Equal(recorded.clearTargets, []string{"/mnt/a"}) {
				t.Errorf("clean-up asked for %q at %q, want /mnt/a at %q", recorded.clearTargets, recorded.socketPath, want)
			}
		})
	}
}

// wantStderr fails the test unless stderr contains every text in want, or is empty when want is.
func wantStderr(t *testing.T, stderr string, want []string) {
	t.Helper()
	for _, text := range want {
		if !strings.Contains(stderr, text) {
			t.Errorf("stderr %q does not contain %q", stderr, text)
		}
	}
	if len(want) == 0 && stderr != "" {
		t.Errorf("stderr %q, want none", stderr)
	}
}

func TestUmountWithAClientDoesNotClearAnything(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{"unmounted": nil, "in use": app.ErrBusy} {
		b, recorded := fakeBackend(err, nil, app.Report{})
		execute(context.Background(), []string{"umount"}, io.Discard, io.Discard, b)
		if recorded.clearCalls != 0 {
			t.Errorf("%s: the clean-up of left-behind mounts ran while a client answered", name)
		}
	}
}

func TestMountCommandSendsTheRequestOfTheHelper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantDir string
		want    app.MountRequest
	}{
		{
			"default directory",
			[]string{"mount", "east:/media", "/mnt/media"},
			defaultDir,
			app.MountRequest{Cluster: "east", Path: "/media", Target: "/mnt/media"},
		},
		{
			"options and directory",
			[]string{"--config", "/etc/example", "mount", "east:", "/mnt/m", "-o", "allow_other,offline"},
			"/etc/example",
			app.MountRequest{Cluster: "east", Path: "/", Target: "/mnt/m", AllowOther: true, Offline: new(true)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := fakeBackend(nil, nil, app.Report{})
			ran := executeWith(b, tt.args...)
			if ran.status != cli.ExitOK || ran.stdout != "" || ran.stderr != "" {
				t.Fatalf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if want := app.SocketPath(tt.wantDir); recorded.socketPath != want {
				t.Errorf("socket = %q, want %q", recorded.socketPath, want)
			}
			if len(recorded.mountRequests) != 1 || !reflect.DeepEqual(recorded.mountRequests[0], tt.want) {
				t.Errorf("requests = %+v, want [%+v]", recorded.mountRequests, tt.want)
			}
		})
	}
}

func TestMountCommandFailures(t *testing.T) {
	t.Parallel()
	refused := errors.New(`the client refused the request: unknown cluster "east"`)
	tests := []struct {
		name    string
		args    []string
		sendErr error
		want    string
	}{
		{"no words", []string{"mount"}, nil, "need exactly a source and a target"},
		{"not a source", []string{"mount", "east", "/mnt/m"}, nil, `source "east" is not CLUSTER:PATH`},
		{"unsupported option", []string{"mount", "east:/m", "/mnt/m", "-o", "frob"}, nil, `unsupported mount option "frob"`},
		{"refused by the client", []string{"mount", "east:/m", "/mnt/m"}, refused, refused.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := fakeBackend(nil, nil, app.Report{})
			sent := b.requestMount
			b.requestMount = func(ctx context.Context, socketPath string, req app.MountRequest) error {
				_ = sent(ctx, socketPath, req) //nolint:errcheck // Only records the call; it answers nothing.
				return tt.sendErr
			}
			ran := executeWith(b, tt.args...)
			if ran.status != cli.ExitError || ran.stdout != "" || ran.stderr != "splicefsctl: "+tt.want+"\n" {
				t.Errorf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if sentRequest := len(recorded.mountRequests) == 1; sentRequest != (tt.sendErr != nil) {
				t.Errorf("request sent = %v after %q", sentRequest, tt.want)
			}
		})
	}
}

func TestUnknownDefaultConfigDir(t *testing.T) {
	t.Parallel()
	boom := errors.New("cannot find the home directory")
	tests := []struct {
		name string
		args []string
	}{
		{"umount", []string{"umount"}},
		{"status", []string{"status"}},
		{"mount", []string{"mount", "east:/m", "/mnt/m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := fakeBackend(nil, nil, app.Report{})
			b.defaultConfigDir = func() (string, error) { return "", boom }
			ran := executeWith(b, tt.args...)
			if ran.status != cli.ExitError || ran.stdout != "" || ran.stderr != "splicefsctl: "+boom.Error()+"\n" {
				t.Errorf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if recorded.socketPath != "" || recorded.statusCalls != 0 || len(recorded.mountRequests) != 0 {
				t.Errorf("a command ran without a configuration directory: %+v", recorded)
			}
		})
	}
}

// TestDefaultConfigDirIsNotLookedUp checks that the commands that do not need the default
// configuration directory do not ask for it, so they work where there is none.
func TestDefaultConfigDirIsNotLookedUp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"directory given", []string{"--config", "/etc/example", "status"}},
		{"version", []string{"--version"}},
		{"help", []string{"--help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, _ := fakeBackend(nil, nil, app.Report{})
			b.defaultConfigDir = func() (string, error) { return "", errors.New("must not be asked") }
			if ran := executeWith(b, tt.args...); ran.status != cli.ExitOK {
				t.Errorf("status %d stdout %q stderr %q, want a command that ran", ran.status, ran.stdout, ran.stderr)
			}
		})
	}
}
