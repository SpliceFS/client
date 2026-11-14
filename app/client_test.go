package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SpliceFS/client/mount/nfs"
)

// serveReplies listens on a new socket and answers every connection with handler. The
// handler gets the request text.
func serveReplies(t *testing.T, handler func(conn net.Conn, request string)) string {
	t.Helper()
	path := filepath.Join(shortDir(t), "c.sock")
	listener, err := listenUnixSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			handler(conn, readRequest(conn))
			closeQuietly(conn)
		}
	}()
	t.Cleanup(func() {
		closeQuietly(listener)
		<-done
	})
	return path
}

func readRequest(conn net.Conn) string {
	var raw json.RawMessage
	if err := json.NewDecoder(conn).Decode(&raw); err != nil {
		return ""
	}
	return string(raw)
}

func reply(text string) func(net.Conn, string) {
	return func(conn net.Conn, _ string) { say(conn, text) }
}

func TestNoClientRunning(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(shortDir(t), "none.sock")
	stale := filepath.Join(shortDir(t), "stale.sock")
	staleSocket(t, stale)
	for name, path := range map[string]string{"no socket file": missing, "nothing behind the file": stale} {
		_, err := Status(context.Background(), path)
		errIs(t, err, ErrNotRunning)
		msg := err.Error()
		if !strings.Contains(msg, "no client is running for this configuration") || !strings.Contains(msg, path) {
			t.Errorf("%s: status error = %v", name, err)
		}
		errIs(t, Unmount(context.Background(), path, []string{"/mnt/a"}, false), ErrNotRunning)
	}
}

func TestRequestsOnTheWire(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []string
	path := serveReplies(t, func(conn net.Conn, request string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, request)
		say(conn, `{"results":[{"target":"/mnt/a","result":"unmounted"}],"report":{}}`)
	})
	if err := Unmount(context.Background(), path, []string{"/mnt/a"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	want := []string{`{"op":"unmount","targets":["/mnt/a"],"force":true}`, `{"op":"status"}`}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

func TestUnmountResolvesRelativeTargets(t *testing.T) {
	t.Parallel()
	requests := make(chan string, 1)
	path := serveReplies(t, func(conn net.Conn, request string) {
		requests <- request
		say(conn, `{"results":[]}`)
	})
	sys := fakeSystem(nil, 1)
	sys.dial = systemHost().dial
	if err := sys.unmount(context.Background(), path, []string{"mnt/a"}, false); err != nil {
		t.Fatal(err)
	}
	if got, want := <-requests, `{"op":"unmount","targets":["/work/mnt/a"]}`; got != want {
		t.Errorf("request = %s, want %s", got, want)
	}

	sys.abs = func(string) (string, error) { return "", errors.New("no cwd") }
	err := sys.unmount(context.Background(), path, []string{"mnt/a"}, false)
	if err == nil || !strings.Contains(err.Error(), "no cwd") {
		t.Errorf("unmount with an unresolvable target = %v", err)
	}
}

func TestUnmountNamesEveryFailedTarget(t *testing.T) {
	t.Parallel()
	path := serveReplies(t, reply(`{"results":[`+
		`{"target":"/mnt/a","result":"unmounted"},`+
		`{"target":"/mnt/b","result":"busy"},`+
		`{"target":"/mnt/c","result":"unknown"},`+
		`{"target":"/mnt/d","result":"failed","error":"device is stuck"}]}`))
	err := Unmount(context.Background(), path, nil, false)
	errIs(t, err, ErrBusy)
	for _, want := range []string{
		"/mnt/b: mount is in use",
		"/mnt/c is not a mount of the running client",
		"/mnt/d: device is stuck",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "/mnt/a") {
		t.Errorf("error names the target that was unmounted: %v", err)
	}
}

func TestBadReplies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		reply string
		want  string
	}{
		{"error reply", `{"error":"unknown op \"x\""}`, `the client refused the request: unknown op "x"`},
		{"no report", `{}`, "the client sent no report"},
		{"garbage", `<html>`, "read reply: invalid character '<'"},
		{"nothing", ``, "read reply: EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := serveReplies(t, reply(tt.reply))
			_, err := Status(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Status = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestCallFailures(t *testing.T) {
	t.Parallel()
	t.Run("connect error", func(t *testing.T) {
		t.Parallel()
		sys := systemHost()
		sys.dial = func(context.Context, string) (net.Conn, error) { return nil, errors.New("permission denied") }
		_, err := sys.call(context.Background(), "/x.sock", Request{Op: OpStatus})
		if err == nil || !strings.Contains(err.Error(), "connect to the client: permission denied") ||
			errors.Is(err, ErrNotRunning) {
			t.Errorf("call = %v", err)
		}
	})
	t.Run("request cannot be sent", func(t *testing.T) {
		t.Parallel()
		sys := systemHost()
		sys.dial = func(context.Context, string) (net.Conn, error) {
			server, client := net.Pipe()
			closeQuietly(server)
			return client, nil
		}
		_, err := sys.call(context.Background(), "/x.sock", Request{Op: OpStatus})
		if err == nil || !strings.Contains(err.Error(), "send request") {
			t.Errorf("call = %v", err)
		}
	})
	t.Run("caller gives up", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		path := serveReplies(t, func(conn net.Conn, _ string) {
			cancel()
			// It never answers and ends when the caller hangs up, with an error nobody reads.
			io.Copy(io.Discard, conn) //nolint:errcheck // see above
		})
		_, err := Status(ctx, path)
		errIs(t, err, context.Canceled)
	})
}

func TestRequestMount(t *testing.T) {
	t.Parallel()
	yes := true
	const minimal = `{"op":"mount","mount":{"cluster":"x","target":"/mnt/a"}}`
	const full = `{"op":"mount","mount":{"cluster":"east","path":"/m","target":"/mnt/a",` +
		`"allow_other":true,"offline":true}}`
	tests := []struct {
		name    string
		req     MountRequest
		reply   string
		want    string
		wantErr string
	}{
		{"minimal", MountRequest{Cluster: "x", Target: "/mnt/a"}, `{}`, minimal, ""},
		{"every field", MountRequest{
			Cluster: "east", Path: "/m", Target: "/mnt/a", AllowOther: true, Offline: &yes,
		}, `{}`, full, ""},
		{
			"refused",
			MountRequest{Cluster: "x", Target: "/mnt/a"},
			`{"error":"unknown cluster \"x\""}`,
			minimal, `the client refused the request: unknown cluster "x"`,
		},
		{
			"malformed reply",
			MountRequest{Cluster: "x", Target: "/mnt/a"},
			`<html>`,
			minimal, "read reply: invalid character '<'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requests := make(chan string, 1)
			path := serveReplies(t, func(conn net.Conn, request string) {
				requests <- request
				say(conn, tt.reply)
			})
			err := RequestMount(context.Background(), path, tt.req)
			if (err == nil) != (tt.wantErr == "") ||
				err != nil && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("RequestMount = %v, want %q", err, tt.wantErr)
			}
			if got := <-requests; got != tt.want {
				t.Errorf("request = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRequestMountNoClientRunning(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(shortDir(t), "none.sock")
	err := RequestMount(context.Background(), missing, MountRequest{Cluster: "east", Target: "/mnt/a"})
	errIs(t, err, ErrNotRunning)
	if !strings.Contains(err.Error(), "start the splicefs service") ||
		!strings.Contains(err.Error(), missing) {
		t.Errorf("RequestMount = %v", err)
	}
}

func TestClearLeftBehindUsesTheRecordNextToTheSocket(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	folder := filepath.Join(dir, "real")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(folder, link); err != nil {
		t.Fatal(err)
	}
	// What the system resolves for the folder of a target is what the mount layer recorded.
	resolvedDir, err := filepath.EvalSymlinks(folder)
	if err != nil {
		t.Fatal(err)
	}
	// The target itself is a link to nothing, which would fail if anything resolved it.
	if err = os.Symlink(filepath.Join(dir, "nothing"), filepath.Join(folder, "dead")); err != nil {
		t.Fatal(err)
	}

	var gotPath string
	var gotTargets []string
	want := []nfs.Cleared{{Target: filepath.Join(resolvedDir, "dead"), Outcome: nfs.OutcomeCleared}}
	sys := fakeSystem(nil, 1)
	sys.clearMounts = func(_ context.Context, recordPath string, targets []string) ([]nfs.Cleared, error) {
		gotPath, gotTargets = recordPath, targets
		return want, nil
	}
	got, err := sys.clearLeftBehind(context.Background(), "/run/x/c.sock", []string{filepath.Join(link, "dead")})
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("clearLeftBehind = %v, %v, want %v", got, err, want)
	}
	if gotPath != "/run/x/c.nfs.json" {
		t.Errorf("record path = %q, want the one next to the socket", gotPath)
	}
	if wantTargets := []string{filepath.Join(resolvedDir, "dead")}; !slices.Equal(gotTargets, wantTargets) {
		t.Errorf("targets = %q, want %q", gotTargets, wantTargets)
	}
}

func TestClearLeftBehindWithoutTargetsAsksForAll(t *testing.T) {
	t.Parallel()
	sys := fakeSystem(nil, 1)
	asked := false
	sys.clearMounts = func(_ context.Context, _ string, targets []string) ([]nfs.Cleared, error) {
		asked = len(targets) == 0
		return nil, nfs.ErrRecordDamaged
	}
	_, err := sys.clearLeftBehind(context.Background(), "/run/x/c.sock", nil)
	errIs(t, err, nfs.ErrRecordDamaged)
	if !asked {
		t.Error("the clean-up was not asked for every mount")
	}
}

func TestClearLeftBehindReportsATargetThatCannotBeMadeAbsolute(t *testing.T) {
	t.Parallel()
	sys := fakeSystem(nil, 1)
	sys.abs = func(string) (string, error) { return "", errors.New("no cwd") }
	sys.clearMounts = func(context.Context, string, []string) ([]nfs.Cleared, error) {
		t.Error("the clean-up ran without a target")
		return nil, nil
	}
	if _, err := sys.clearLeftBehind(context.Background(), "/run/x/c.sock", []string{"mnt"}); err == nil ||
		!strings.Contains(err.Error(), "no cwd") {
		t.Errorf("clearLeftBehind = %v", err)
	}
}

func TestClearLeftBehindOfTheSystem(t *testing.T) {
	t.Parallel()
	// A record that does not exist is an empty one, and on the systems without a record the
	// answer is empty as well.
	results, err := ClearLeftBehind(context.Background(), filepath.Join(t.TempDir(), "c.sock"), nil)
	if len(results) != 0 || err != nil {
		t.Errorf("ClearLeftBehind = %v, %v, want nothing", results, err)
	}
}
