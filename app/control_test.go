package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/SpliceFS/client/core"
)

// rawCall sends text to the control socket and returns everything the client answers.
func rawCall(t *testing.T, socket, text string) string {
	t.Helper()
	conn, err := dialUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(conn)
	if _, err = io.WriteString(conn, text); err != nil {
		t.Fatal(err)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("connection is a %T, want a Unix connection", conn)
	}
	// A request that is refused can be answered and closed before this side has ended its
	// half. macOS then reports that the socket is not connected, and the reply is still there.
	if err = unixConn.CloseWrite(); err != nil && !errors.Is(err, syscall.ENOTCONN) {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(reply)
}

func TestStatusReport(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	env.cfg.Mounts = env.cfg.Mounts[:2]
	env.core.status = core.Status{
		Clusters: []core.ClusterStatus{
			{Name: "example", Nodes: []core.NodeStatus{
				{Name: "east", BytesPerSecond: 48_200_000, Measured: true},
				{Name: "west"},
			}},
			{Name: "other"},
		},
		Caches: []core.CacheStatus{{Dir: "/var/cache/splicefs", Entries: 12, Bytes: 3_400_000_000}},
	}
	r := env.start()

	want := Report{
		Mounts: []MountReport{
			{Target: "/mnt/a", Source: "splicefs:example/media", State: StateMounted},
			{Target: "/mnt/b", Source: "splicefs:example/", State: StateMounted},
		},
		Clusters: []ClusterReport{
			{Name: "example", Nodes: []NodeReport{
				{Name: "east", BytesPerSecond: 48_200_000, Measured: true},
				{Name: "west"},
			}},
			{Name: "other", Nodes: []NodeReport{}},
		},
		Caches: []CacheReport{{Dir: "/var/cache/splicefs", Entries: 12, Bytes: 3_400_000_000}},
	}
	if got := env.status(); !reflect.DeepEqual(got, want) {
		t.Errorf("report = %+v\nwant     %+v", got, want)
	}

	const wantJSON = `{"report":{"mounts":[` +
		`{"target":"/mnt/a","source":"splicefs:example/media","state":"mounted"},` +
		`{"target":"/mnt/b","source":"splicefs:example/","state":"mounted"}],` +
		`"clusters":[{"name":"example","nodes":[` +
		`{"name":"east","bytes_per_second":48200000,"measured":true},` +
		`{"name":"west","bytes_per_second":0,"measured":false}]},` +
		`{"name":"other","nodes":[]}],` +
		`"caches":[{"dir":"/var/cache/splicefs","entries":12,"bytes":3400000000}]}}` + "\n"
	if got := rawCall(t, env.socket, `{"op":"status"}`); got != wantJSON {
		t.Errorf("reply = %s\nwant    %s", got, wantJSON)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestSocketIsPrivate(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()
	info, err := os.Lstat(env.socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

// stateOf is the state of a mount in a report. An unmounted mount is StateEnded until its
// watcher takes it out of the set, and absent after that, which both count as StateEnded.
func stateOf(report Report, target string) string {
	for _, m := range report.Mounts {
		if m.Target == target {
			return m.State
		}
	}
	return StateEnded
}

// unmountCase is one unmount request to a client with three idle mounts, and what must follow from it.
type unmountCase struct {
	name      string
	behavior  func(force bool) error // how /mnt/a answers an unmount
	targets   []string
	force     bool
	wantErr   string
	wantBusy  bool
	wantState string // state of /mnt/a afterwards
	wantCalls []bool // force flag of each unmount call /mnt/a got
}

func unmountCases() []unmountCase {
	stuck := errors.New("device is stuck")
	inUse := func(bool) error { return fmt.Errorf("device in use: %w", syscall.EBUSY) }
	return []unmountCase{
		{
			name: "idle mount", targets: []string{"/mnt/a"},
			wantState: StateEnded, wantCalls: []bool{false},
		},
		{
			name: "mount in use", behavior: busyUnlessForced, targets: []string{"/mnt/a"},
			wantErr: "/mnt/a: mount is in use", wantBusy: true,
			wantState: StateMounted, wantCalls: []bool{false},
		},
		{
			name: "mount in use, forced", behavior: busyUnlessForced, targets: []string{"/mnt/a"}, force: true,
			wantState: StateEnded, wantCalls: []bool{true},
		},
		{
			name: "unmount fails", behavior: func(bool) error { return stuck }, targets: []string{"/mnt/a"},
			wantErr: "/mnt/a: device is stuck", wantState: StateMounted, wantCalls: []bool{false},
		},
		{
			name: "busy text on a forced unmount is a failure", behavior: inUse, targets: []string{"/mnt/a"}, force: true,
			wantErr: "/mnt/a: device in use", wantState: StateMounted, wantCalls: []bool{true},
		},
		{
			name: "unknown target", targets: []string{"/mnt/zzz"},
			wantErr: "/mnt/zzz is not a mount of the running client", wantState: StateMounted, wantCalls: nil,
		},
		{
			name: "target is cleaned", targets: []string{"/mnt//a/"},
			wantState: StateEnded, wantCalls: []bool{false},
		},
	}
}

// wantUnmountError fails the test unless err matches the error the case expects (none, or one with that text).
func wantUnmountError(t *testing.T, err error, wantErr string) {
	t.Helper()
	switch {
	case wantErr == "" && err != nil:
		t.Fatalf("Unmount = %v", err)
	case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
		t.Fatalf("Unmount = %v, want an error containing %q", err, wantErr)
	default:
	}
}

func checkUnmount(t *testing.T, scenario *unmountCase) {
	t.Helper()
	env := newHarness(t)
	r := env.start()
	mountedA := env.mounter.get("/mnt/a")
	mountedA.setBehavior(scenario.behavior)

	err := Unmount(context.Background(), env.socket, scenario.targets, scenario.force)
	wantUnmountError(t, err, scenario.wantErr)
	if got := errors.Is(err, ErrBusy); got != scenario.wantBusy {
		t.Errorf("errors.Is(ErrBusy) = %v, want %v", got, scenario.wantBusy)
	}
	if got := stateOf(env.status(), "/mnt/a"); got != scenario.wantState {
		t.Errorf("state of /mnt/a = %s, want %s", got, scenario.wantState)
	}
	if calls := mountedA.unmountCalls(); !reflect.DeepEqual(calls, scenario.wantCalls) {
		t.Errorf("unmount calls (force) = %v, want %v", calls, scenario.wantCalls)
	}
	mountedA.setBehavior(nil)
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestUnmountOverTheSocket(t *testing.T) {
	t.Parallel()
	for _, tt := range unmountCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkUnmount(t, &tt)
		})
	}
}

func TestUnmountEverythingKeepsTheRunGoing(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()
	if err := Unmount(context.Background(), env.socket, nil, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "every view to be released", func() bool { return env.core.releasedCount() == 3 })
	select {
	case err := <-r.result:
		t.Fatalf("Run ended with %v after the last unmount, want it to idle", err)
	default:
	}
	if err := r.stop(); err != nil {
		t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
	if got := env.notes.got(); len(got) != 2 || got[1] != "STOPPING=1" {
		t.Errorf("notifications = %v", got)
	}
}

func TestMalformedRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request string
		want    string
	}{
		{"not JSON", "hello", `{"error":"malformed request: invalid character 'h' looking for beginning of value"}`},
		{"empty", "", `{"error":"malformed request: EOF"}`},
		{
			"wrong type", `{"op":5}`,
			`{"error":"malformed request: json: cannot unmarshal number into Go struct field Request.op of type string"}`,
		},
		{"unknown op", `{"op":"explode"}`, `{"error":"unknown op \"explode\""}`},
		{"no op", `{}`, `{"error":"unknown op \"\""}`},
	}
	env := newHarness(t)
	r := env.start()
	for _, tt := range tests {
		if got := strings.TrimSpace(rawCall(t, env.socket, tt.request)); got != tt.want {
			t.Errorf("%s: reply = %s, want %s", tt.name, got, tt.want)
		}
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestStopDoesNotWaitForASilentConnection(t *testing.T) {
	t.Parallel()
	env := newHarness(t)
	r := env.start()
	conn, err := dialUnix(env.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(conn)
	// Connections are accepted in order, so once this one is answered the silent one
	// has been accepted and is waiting for its request.
	env.status()
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

// listenerFunc is a net.Listener whose Accept is a function.
type listenerFunc struct {
	net.Listener

	accept func() (net.Conn, error)
}

func (l listenerFunc) Accept() (net.Conn, error) { return l.accept() }
func (listenerFunc) Close() error                { return nil }

func TestControlLogsAnAcceptFailure(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	boom := errors.New("too many open files")
	ln := listenerFunc{accept: func() (net.Conn, error) { return nil, boom }}
	startControl(t.Context(), ln, &mountSet{}, newFakeCore(), logs.logger(), nil).stop()
	out := logs.String()
	if !strings.Contains(out, "control socket stopped accepting") || !strings.Contains(out, "too many open files") {
		t.Errorf("log:\n%s", out)
	}
}

func TestControlIgnoresAClosedListener(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	ln := listenerFunc{accept: func() (net.Conn, error) { return nil, net.ErrClosed }}
	startControl(t.Context(), ln, &mountSet{}, newFakeCore(), logs.logger(), nil).stop()
	if logs.String() != "" {
		t.Errorf("a normal stop was logged:\n%s", logs.String())
	}
}

func TestControlSurvivesAClientThatLeaves(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &control{set: &mountSet{}, status: newFakeCore(), log: logs.logger(), cancel: cancel}
	server, client := net.Pipe()
	closeQuietly(client)
	c.handle(ctx, server)
	if !strings.Contains(logs.String(), "control reply not delivered") {
		t.Errorf("log:\n%s", logs.String())
	}
}
