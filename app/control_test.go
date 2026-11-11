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
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, text); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
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
	h := newHarness(t)
	h.cfg.Mounts = h.cfg.Mounts[:2]
	h.core.status = core.Status{
		Clusters: []core.ClusterStatus{
			{Name: "example", Nodes: []core.NodeStatus{
				{Name: "east", BytesPerSecond: 48_200_000, Measured: true},
				{Name: "west"},
			}},
			{Name: "other"},
		},
		Caches: []core.CacheStatus{{Dir: "/var/cache/splicefs", Entries: 12, Bytes: 3_400_000_000}},
	}
	r := h.start()

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
	if got := h.status(); !reflect.DeepEqual(got, want) {
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
	if got := rawCall(t, h.socket, `{"op":"status"}`); got != wantJSON {
		t.Errorf("reply = %s\nwant    %s", got, wantJSON)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestSocketIsPrivate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()
	info, err := os.Lstat(h.socket)
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

func TestUnmountOverTheSocket(t *testing.T) {
	t.Parallel()
	stuck := errors.New("device is stuck")
	tests := []struct {
		name      string
		behavior  func(force bool) error
		targets   []string
		force     bool
		wantErr   string
		wantBusy  bool
		wantState string
		wantCalls []bool
	}{
		{name: "idle mount", targets: []string{"/mnt/a"}, wantState: StateEnded, wantCalls: []bool{false}},
		{name: "mount in use", behavior: busyUnlessForced, targets: []string{"/mnt/a"}, wantErr: "/mnt/a: mount is in use", wantBusy: true, wantState: StateMounted, wantCalls: []bool{false}},
		{name: "mount in use, forced", behavior: busyUnlessForced, targets: []string{"/mnt/a"}, force: true, wantState: StateEnded, wantCalls: []bool{true}},
		{name: "unmount fails", behavior: func(bool) error { return stuck }, targets: []string{"/mnt/a"}, wantErr: "/mnt/a: device is stuck", wantState: StateMounted, wantCalls: []bool{false}},
		{name: "busy text on a forced unmount is a failure", behavior: func(bool) error { return fmt.Errorf("device in use: %w", syscall.EBUSY) }, targets: []string{"/mnt/a"}, force: true, wantErr: "/mnt/a: device in use", wantState: StateMounted, wantCalls: []bool{true}},
		{name: "unknown target", targets: []string{"/mnt/zzz"}, wantErr: "/mnt/zzz is not a mount of the running client", wantState: StateMounted, wantCalls: nil},
		{name: "target is cleaned", targets: []string{"/mnt//a/"}, wantState: StateEnded, wantCalls: []bool{false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			r := h.start()
			a := h.mounter.get("/mnt/a")
			a.setBehavior(tt.behavior)

			err := Unmount(context.Background(), h.socket, tt.targets, tt.force)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Unmount = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Unmount = %v, want an error containing %q", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrBusy); got != tt.wantBusy {
				t.Errorf("errors.Is(ErrBusy) = %v, want %v", got, tt.wantBusy)
			}
			if got := h.status().Mounts[0].State; got != tt.wantState {
				t.Errorf("state of /mnt/a = %s, want %s", got, tt.wantState)
			}
			if calls := a.unmountCalls(); !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Errorf("unmount calls (force) = %v, want %v", calls, tt.wantCalls)
			}
			a.setBehavior(nil)
			if err := r.stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnmountEverythingEndsTheRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()
	if err := Unmount(context.Background(), h.socket, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v, want nil after the last unmount", err)
	}
	if got := h.notes.got(); len(got) != 2 || got[1] != "STOPPING=1" {
		t.Errorf("notifications = %v", got)
	}
}

func TestUnmountTheLastMountEndsTheRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Mounts = h.cfg.Mounts[:2]
	r := h.start()
	if err := Unmount(context.Background(), h.socket, []string{"/mnt/a"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.result:
		t.Fatalf("Run ended with a mount left: %v", err)
	default:
	}
	if err := Unmount(context.Background(), h.socket, []string{"/mnt/b"}, false); err != nil {
		t.Fatal(err)
	}
	if err := r.wait(); err != nil {
		t.Fatal(err)
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
		{"wrong type", `{"op":5}`, `{"error":"malformed request: json: cannot unmarshal number into Go struct field Request.op of type string"}`},
		{"unknown op", `{"op":"explode"}`, `{"error":"unknown op \"explode\""}`},
		{"no op", `{}`, `{"error":"unknown op \"\""}`},
	}
	h := newHarness(t)
	r := h.start()
	for _, tt := range tests {
		if got := strings.TrimSpace(rawCall(t, h.socket, tt.request)); got != tt.want {
			t.Errorf("%s: reply = %s, want %s", tt.name, got, tt.want)
		}
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
}

func TestStopDoesNotWaitForASilentConnection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.start()
	conn, err := dialUnix(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Connections are accepted in order, so once this one is answered the silent one
	// has been accepted and is waiting for its request.
	h.status()
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
func (l listenerFunc) Close() error              { return nil }

func TestControlLogsAnAcceptFailure(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	boom := errors.New("too many open files")
	ln := listenerFunc{accept: func() (net.Conn, error) { return nil, boom }}
	startControl(ln, &mountSet{}, newFakeCore(), logs.logger()).stop()
	if !strings.Contains(logs.String(), "control socket stopped accepting") || !strings.Contains(logs.String(), "too many open files") {
		t.Errorf("log:\n%s", logs.String())
	}
}

func TestControlIgnoresAClosedListener(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	ln := listenerFunc{accept: func() (net.Conn, error) { return nil, net.ErrClosed }}
	startControl(ln, &mountSet{}, newFakeCore(), logs.logger()).stop()
	if logs.String() != "" {
		t.Errorf("a normal stop was logged:\n%s", logs.String())
	}
}

func TestControlSurvivesAClientThatLeaves(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &control{set: &mountSet{}, status: newFakeCore(), log: logs.logger(), ctx: ctx, cancel: cancel}
	server, client := net.Pipe()
	_ = client.Close()
	c.handle(server)
	if !strings.Contains(logs.String(), "control reply not delivered") {
		t.Errorf("log:\n%s", logs.String())
	}
}
