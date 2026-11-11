package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// serveReplies listens on a new socket and answers every connection with handler. The
// handler gets the request text.
func serveReplies(t *testing.T, handler func(conn net.Conn, request string)) string {
	t.Helper()
	path := filepath.Join(shortDir(t), "c.sock")
	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			handler(conn, readRequest(conn))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
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
	return func(conn net.Conn, _ string) { _, _ = io.WriteString(conn, text) }
}

func TestNoClientRunning(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(shortDir(t), "none.sock")
	stale := filepath.Join(shortDir(t), "stale.sock")
	staleSocket(t, stale)
	for name, path := range map[string]string{"no socket file": missing, "nothing behind the file": stale} {
		_, err := Status(context.Background(), path)
		errIs(t, err, ErrNotRunning)
		if !strings.Contains(err.Error(), "no client is running for this configuration") || !strings.Contains(err.Error(), path) {
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
		_, _ = io.WriteString(conn, `{"results":[{"target":"/mnt/a","result":"unmounted"}],"report":{}}`)
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
		_, _ = io.WriteString(conn, `{"results":[]}`)
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
	if err := sys.unmount(context.Background(), path, []string{"mnt/a"}, false); err == nil || !strings.Contains(err.Error(), "no cwd") {
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
	for _, want := range []string{"/mnt/b: mount is in use", "/mnt/c is not a mount of the running client", "/mnt/d: device is stuck"} {
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
		if err == nil || !strings.Contains(err.Error(), "connect to the client: permission denied") || errors.Is(err, ErrNotRunning) {
			t.Errorf("call = %v", err)
		}
	})
	t.Run("request cannot be sent", func(t *testing.T) {
		t.Parallel()
		sys := systemHost()
		sys.dial = func(context.Context, string) (net.Conn, error) {
			server, client := net.Pipe()
			_ = server.Close()
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
			_, _ = io.Copy(io.Discard, conn) // never answers; ends when the caller hangs up
		})
		_, err := Status(ctx, path)
		errIs(t, err, context.Canceled)
	})
}
