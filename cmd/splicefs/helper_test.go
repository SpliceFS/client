package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/client/app"
	"github.com/SpliceFS/client/internal/version"
)

// helperCalls records what the helper asked of the client library.
type helperCalls struct {
	requests   int
	socketPath string
	req        app.MountRequest
}

// helperBackend returns a backend whose client answers reply to every mount request, with
// the given environment, and the working directory /work.
func helperBackend(env map[string]string, reply error) (backend, *helperCalls) {
	b, _ := fakeBackend(nil, nil, nil, nil, nil, app.Report{})
	recorded := &helperCalls{}
	b.requestMount = func(_ context.Context, socketPath string, req app.MountRequest) error {
		recorded.requests++
		recorded.socketPath, recorded.req = socketPath, req
		return reply
	}
	b.getenv = func(key string) string { return env[key] }
	b.abs = func(path string) (string, error) {
		if filepath.IsAbs(path) {
			return path, nil
		}
		return "/work/" + path, nil
	}
	return b, recorded
}

// runAs runs the program under the name argv[0] and returns its status and output.
func runAs(b backend, argv ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	status := dispatch(ctx, argv, &out, &errOut, b)
	return result{status: status, stdout: out.String(), stderr: errOut.String()}
}

// runHelper runs the program as the mount helper with the given arguments.
func runHelper(b backend, args ...string) result {
	return runAs(b, append([]string{"/sbin/" + helperProgram}, args...)...)
}

func TestDispatchByProgramName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		argv       []string
		wantHelper bool
		wantStatus int
	}{
		{"helper name", []string{"mount.splicefs", "east:/m", "/mnt/m"}, true, exitOK},
		{"helper path", []string{"/sbin/mount.splicefs", "east:/m", "/mnt/m"}, true, exitOK},
		{"helper link in usr", []string{"/usr/sbin/mount.splicefs", "east:/m", "/mnt/m"}, true, exitOK},
		{"helper with bad usage", []string{"mount.splicefs"}, true, exitError},
		{"normal name", []string{"splicefs", "--version"}, false, exitOK},
		{"normal path", []string{"/usr/bin/splicefs", "--version"}, false, exitOK},
		{"similar name", []string{"/usr/bin/xmount.splicefs", "--version"}, false, exitOK},
		{"no program name", nil, false, exitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := helperBackend(nil, nil)
			ran := runAs(b, tt.argv...)
			if ran.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", ran.status, tt.wantStatus)
			}
			if got := recorded.requests == 1; got != (tt.wantHelper && tt.wantStatus == exitOK) {
				t.Errorf("mount requests = %d for helper = %v", recorded.requests, tt.wantHelper)
			}
			if !tt.wantHelper && tt.wantStatus == exitOK && ran.stdout != "splicefs "+version.String()+"\n" {
				t.Errorf("stdout = %q, want the version", ran.stdout)
			}
		})
	}
}

// wantMount is the request for the cluster east.
func wantMount(path, target string, allowOther bool, offline *bool) app.MountRequest {
	return app.MountRequest{
		Cluster: "east", Path: path, Target: target, AllowOther: allowOther, Offline: offline,
	}
}

func TestHelperSendsTheRequest(t *testing.T) {
	t.Parallel()
	ignored := []string{
		"rw", "async", "ro", "defaults", "auto", "noauto", "nofail", "_netdev", "user", "nouser", "users",
		"owner", "exec", "noexec", "suid", "nosuid", "dev", "nodev", "atime", "noatime",
		"relatime", "x-systemd.requires=splicefs.service", "x-foo", "x-", "comment=a",
	}
	const std = "east:/m /mnt/m " // the arguments that most cases add options to
	tests := []struct {
		name string
		args string
		want app.MountRequest
	}{
		{"with a path", "east:/media /mnt/media", wantMount("/media", "/mnt/media", false, nil)},
		{"whole cluster", "east:/ /mnt/m", wantMount("/", "/mnt/m", false, nil)},
		{"empty path", "east: /mnt/m", wantMount("/", "/mnt/m", false, nil)},
		{"several colons", "east:/a:b:c /mnt/m", wantMount("/a:b:c", "/mnt/m", false, nil)},
		{"colon right after the cluster", "east:: /mnt/m", wantMount(":", "/mnt/m", false, nil)},
		{"relative target", "east:/m rel/dir", wantMount("/m", "/work/rel/dir", false, nil)},
		{"target is cleaned", "east:/m /mnt/x/../y/", wantMount("/m", "/mnt/y", false, nil)},
		{"allow_other", std + "-o allow_other", wantMount("/m", "/mnt/m", true, nil)},
		{"offline", std + "-o offline", wantMount("/m", "/mnt/m", false, new(true))},
		{"nooffline", std + "-o nooffline", wantMount("/m", "/mnt/m", false, new(false))},
		{
			"offline then nooffline", std + "-o offline,nooffline",
			wantMount("/m", "/mnt/m", false, new(false)),
		},
		{
			"nooffline then offline", std + "-o nooffline,offline",
			wantMount("/m", "/mnt/m", false, new(true)),
		},
		{
			"several -o", std + "-o ro -o allow_other,offline -o _netdev",
			wantMount("/m", "/mnt/m", true, new(true)),
		},
		{"flags before", "-s -f -n -v " + std + "-o ro", wantMount("/m", "/mnt/m", false, nil)},
		{"flags after", std + "-sfnv -o allow_other", wantMount("/m", "/mnt/m", true, nil)},
		{"combined flags first", "-sfnv " + std, wantMount("/m", "/mnt/m", false, nil)},
		{"-o between the words", "east:/m -o allow_other /mnt/m", wantMount("/m", "/mnt/m", true, nil)},
		{"empty list items", std + "-o ,ro,,allow_other,", wantMount("/m", "/mnt/m", true, nil)},
		{
			"all ignored options", std + "-o " + strings.Join(ignored, ","),
			wantMount("/m", "/mnt/m", false, nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := helperBackend(nil, nil)
			ran := runHelper(b, strings.Fields(tt.args)...)
			if ran.status != exitOK || ran.stdout != "" || ran.stderr != "" {
				t.Fatalf("status %d stdout %q stderr %q", ran.status, ran.stdout, ran.stderr)
			}
			if recorded.requests != 1 || !reflect.DeepEqual(recorded.req, tt.want) {
				t.Errorf("request = %+v (%d calls), want %+v", recorded.req, recorded.requests, tt.want)
			}
		})
	}
}

func TestHelperEmptyOptionValue(t *testing.T) {
	t.Parallel()
	b, recorded := helperBackend(nil, nil)
	ran := runHelper(b, "east:/m", "/mnt/m", "-o", "")
	if ran.status != exitOK || ran.stderr != "" || recorded.req.AllowOther || recorded.req.Offline != nil {
		t.Errorf("status %d stderr %q request %+v", ran.status, ran.stderr, recorded.req)
	}
}

// TestIgnoredOptionsAreListed keeps the accepted set equal to the one in the design.
func TestIgnoredOptionsAreListed(t *testing.T) {
	t.Parallel()
	want := "async atime auto defaults dev exec noatime noauto nodev noexec nofail nosuid " +
		"nouser owner relatime ro rw suid user users _netdev"
	for opt := range strings.FieldsSeq(want) {
		if !slices.Contains(ignoredOptions(), opt) {
			t.Errorf("option %q is not ignored", opt)
		}
	}
	if got := len(ignoredOptions()); got != len(strings.Fields(want)) {
		t.Errorf("%d ignored options, want %d", got, len(strings.Fields(want)))
	}
}

func TestHelperBadUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", nil, "need exactly a source and a target"},
		{"only a source", []string{"east:/m"}, "need exactly a source and a target"},
		{"three words", []string{"east:/m", "/mnt/m", "/mnt/n"}, "need exactly a source and a target"},
		{"no colon", []string{"east", "/mnt/m"}, `source "east" is not CLUSTER:PATH`},
		{"empty cluster", []string{":/m", "/mnt/m"}, `source ":/m" is not CLUSTER:PATH`},
		{"empty source", []string{"", "/mnt/m"}, `source "" is not CLUSTER:PATH`},
		{
			"unknown option",
			[]string{"east:/m", "/mnt/m", "-o", "ro,frob=1"},
			`unsupported mount option "frob=1"`,
		},
		{
			"option after a good list",
			[]string{"east:/m", "/mnt/m", "-o", "ro", "-o", "sync"},
			`unsupported mount option "sync"`,
		},
		{"-o without a value", []string{"east:/m", "/mnt/m", "-o"}, "-o needs a value"},
		{"unknown flag", []string{"-x", "east:/m", "/mnt/m"}, `unknown flag "-x"`},
		{"unknown flag in a group", []string{"east:/m", "/mnt/m", "-sx"}, `unknown flag "-sx"`},
		{"long flag", []string{"east:/m", "/mnt/m", "--verbose"}, `unknown flag "--verbose"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := helperBackend(nil, nil)
			ran := runHelper(b, tt.args...)
			if ran.status != exitError || ran.stdout != "" || recorded.requests != 0 {
				t.Errorf("status %d stdout %q requests %d", ran.status, ran.stdout, recorded.requests)
			}
			if !strings.HasPrefix(ran.stderr, "mount.splicefs: ") || !strings.Contains(ran.stderr, tt.want) ||
				!strings.HasSuffix(ran.stderr, "\n"+helperUsage) || strings.Count(ran.stderr, "\n") != 2 {
				t.Errorf("stderr = %q, want %q and one usage line", ran.stderr, tt.want)
			}
		})
	}
}

func TestHelperTargetCannotBeResolved(t *testing.T) {
	t.Parallel()
	b, recorded := helperBackend(nil, nil)
	b.abs = func(string) (string, error) { return "", errors.New("no working directory") }
	ran := runHelper(b, "east:/m", "rel")
	if ran.status != exitError || recorded.requests != 0 ||
		!strings.Contains(ran.stderr, `mount.splicefs: resolve target "rel": no working directory`) {
		t.Errorf("status %d requests %d stderr %q", ran.status, recorded.requests, ran.stderr)
	}
}

func TestHelperConfigDirectory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"default", nil, defaultDir},
		{"empty variable", map[string]string{configEnv: ""}, defaultDir},
		{"variable", map[string]string{configEnv: "/custom/conf"}, "/custom/conf"},
		{"other variables are ignored", map[string]string{"SPLICEFS_OTHER": "/x"}, defaultDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := helperBackend(tt.env, nil)
			if ran := runHelper(b, "east:/m", "/mnt/m"); ran.status != exitOK {
				t.Fatalf("status %d stderr %q", ran.status, ran.stderr)
			}
			if want := app.SocketPath(tt.want); recorded.socketPath != want {
				t.Errorf("socket = %s, want %s", recorded.socketPath, want)
			}
		})
	}
}

func TestHelperNoDefaultConfigDirectory(t *testing.T) {
	t.Parallel()
	b, recorded := helperBackend(nil, nil)
	b.defaultConfigDir = func() (string, error) { return "", errors.New("find the home directory") }
	ran := runHelper(b, "east:/m", "/mnt/m")
	want := "mount.splicefs: find the home directory\n"
	if ran.status != exitMountFailed || recorded.requests != 0 || ran.stderr != want {
		t.Errorf("status %d requests %d stderr %q", ran.status, recorded.requests, ran.stderr)
	}
	// A directory in the environment makes the default unnecessary.
	b, recorded = helperBackend(map[string]string{configEnv: "/custom"}, nil)
	b.defaultConfigDir = func() (string, error) { return "", errors.New("must not be asked") }
	ran = runHelper(b, "east:/m", "/mnt/m")
	if ran.status != exitOK || recorded.requests != 1 {
		t.Errorf("status %d requests %d stderr %q", ran.status, recorded.requests, ran.stderr)
	}
}

func TestHelperClientFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{"refused", errors.New(`the client refused the request: unknown cluster "east"`)},
		{"not running", fmt.Errorf("%w (socket /run/x.sock): start the service", app.ErrNotRunning)},
		{"cannot connect", errors.New("connect to the client: permission denied")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, recorded := helperBackend(nil, tt.err)
			ran := runHelper(b, "east:/m", "/mnt/m")
			if ran.status != exitMountFailed || ran.stdout != "" || recorded.requests != 1 {
				t.Errorf("status %d stdout %q requests %d", ran.status, ran.stdout, recorded.requests)
			}
			if want := "mount.splicefs: " + tt.err.Error() + "\n"; ran.stderr != want {
				t.Errorf("stderr = %q, want %q", ran.stderr, want)
			}
		})
	}
}

// TestHelperWithNoClientRunning goes through the real client function: a configuration
// directory nobody serves has no socket.
func TestHelperWithNoClientRunning(t *testing.T) {
	t.Parallel()
	b, _ := helperBackend(map[string]string{configEnv: t.TempDir()}, nil)
	b.requestMount = app.RequestMount
	ran := runHelper(b, "east:/m", "/mnt/m")
	if ran.status != exitMountFailed || ran.stdout != "" {
		t.Errorf("status %d stdout %q", ran.status, ran.stdout)
	}
	wants := []string{"mount.splicefs: ", "no client is running", "start the splicefs service"}
	for _, want := range wants {
		if !strings.Contains(ran.stderr, want) {
			t.Errorf("stderr %q lacks %q", ran.stderr, want)
		}
	}
}

func TestUsageNamesTheHelper(t *testing.T) {
	t.Parallel()
	if !strings.Contains(usage, "mount.splicefs") || !strings.Contains(usage, "mount(8)") {
		t.Errorf("usage does not mention the mount helper:\n%s", usage)
	}
}

func TestRunDispatchesOnTheProgramName(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	argv := []string{"/sbin/mount.splicefs"}
	status := run(context.Background(), argv, strings.NewReader(""), &out, &errOut)
	if status != exitError || out.Len() != 0 || !strings.HasSuffix(errOut.String(), helperUsage) {
		t.Errorf("status %d stdout %q stderr %q", status, out.String(), errOut.String())
	}
}
