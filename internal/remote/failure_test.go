package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// operations lists every method as a function of a Node, so that error handling is tested
// identically across all of them. okStatus is the status that is a success for it.
var operations = []struct {
	name     string
	okStatus int
	run      func(*Node) error
}{
	{"Health", 200, func(n *Node) error { _, err := n.Health(context.Background()); return err }},
	{"Login", 200, func(n *Node) error { _, err := n.Login(context.Background(), "u", "p"); return err }},
	{"Nodes", 200, func(n *Node) error { _, err := n.Nodes(context.Background(), testToken); return err }},
	{"List", 200, func(n *Node) error { _, err := n.List(context.Background(), testToken, "s"); return err }},
	{"Head", 200, func(n *Node) error { _, err := n.Head(context.Background(), testToken, "s/f"); return err }},
	{"Range", 206, func(n *Node) error {
		rc, err := n.Range(context.Background(), testToken, "s/f", "e", 0, 1)
		if rc != nil {
			_ = rc.Close()
		}
		return err
	}},
	{"Space", 200, func(n *Node) error { _, err := n.Space(context.Background(), testToken, "s"); return err }},
}

func TestStatusMapping(t *testing.T) {
	t.Parallel()
	statuses := []struct {
		status int
		want   error
	}{
		{400, ErrProtocol},
		{401, ErrUnauthorized},
		{403, ErrForbidden},
		{404, ErrNotFound},
		{412, ErrChanged},
		{416, ErrBadRange},
		{418, ErrProtocol},
		{429, ErrRateLimited},
		{500, ErrUnavailable},
		{502, ErrUnavailable},
		{503, ErrUnavailable},
		{204, ErrProtocol},
		{301, ErrProtocol},
	}
	for _, op := range operations {
		for _, st := range statuses {
			// A status that is the operation's success status is not an error case.
			if st.status == op.okStatus {
				continue
			}
			t.Run(op.name+"/"+http.StatusText(st.status), func(t *testing.T) {
				t.Parallel()
				n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, st.status, `{"error":"server says no"}`)
				})
				err := op.run(n)
				if !errors.Is(err, st.want) {
					t.Fatalf("error = %v, want %v", err, st.want)
				}
				text := err.Error()
				if !strings.Contains(text, "east") || !strings.Contains(text, strings.ToLower(op.name)) {
					t.Errorf("error %q lacks node name or operation", text)
				}
			})
		}
	}
}

func TestStatusErrorMessage(t *testing.T) {
	t.Parallel()
	long := "a" + strings.Repeat("\u00e9", 200)
	tests := []struct {
		name     string
		body     string
		contains string
		maxLen   int
	}{
		{name: "message included", body: `{"error":"share not allowed"}`, contains: `"share not allowed"`, maxLen: 1000},
		{name: "long message truncated at a character", body: `{"error":"` + long + `"}`, contains: "a" + strings.Repeat("\u00e9", 100), maxLen: 400},
		{name: "not json", body: `<html>gateway</html>`, contains: "HTTP 403", maxLen: 1000},
		{name: "empty message", body: `{"error":""}`, contains: "HTTP 403", maxLen: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 403, tt.body)
			})
			_, err := n.Nodes(context.Background(), testToken)
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("error = %v, want ErrForbidden", err)
			}
			if !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("error %q does not contain %q", err, tt.contains)
			}
			if len(err.Error()) > tt.maxLen {
				t.Errorf("error text is %d bytes, want at most %d", len(err.Error()), tt.maxLen)
			}
			if strings.Contains(err.Error(), "\ufffd") {
				t.Errorf("error %q contains a broken character", err)
			}
		})
	}
}

func TestJSONProtocolViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		body        string
		run         func(*Node) error
	}{
		{name: "wrong content type", contentType: "text/html", body: `{"status":"ok"}`, run: operations[0].run},
		{name: "missing content type", contentType: "", body: `{"status":"ok"}`, run: operations[0].run},
		{name: "malformed content type", contentType: ";;;", body: `{"status":"ok"}`, run: operations[0].run},
		{name: "undecodable", contentType: "application/json", body: `{"status":`, run: operations[0].run},
		{name: "wrong shape", contentType: "application/json", body: `[1,2]`, run: operations[0].run},
		{name: "trailing data", contentType: "application/json", body: `{"status":"ok"} {}`, run: operations[0].run},
		{name: "small body too large", contentType: "application/json", body: `{"status":"` + strings.Repeat("a", maxSmallJSON) + `"}`, run: operations[0].run},
		{name: "listing too large", contentType: "application/json", body: `{"entries":[]}` + strings.Repeat(" ", maxListingJSON), run: operations[3].run},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				_, _ = w.Write([]byte(tt.body))
			})
			if err := tt.run(n); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestListingAtTheLimitIsAccepted(t *testing.T) {
	t.Parallel()
	body := `{"entries":[]}`
	body += strings.Repeat(" ", maxListingJSON-len(body))
	n := serve(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, body) })
	if _, err := n.List(context.Background(), testToken, "s"); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestTransportFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	// The listener stays open for the whole test and drops every connection at once. A
	// closed server would free its port for a server of a parallel test, and the
	// operations would then get an HTTP answer instead of a transport failure.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	n, err := New("east", "http://"+ln.Addr().String()+"/api", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			err := op.run(n)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("error = %v, want ErrUnavailable", err)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error %v claims the caller's context ended", err)
			}
		})
	}
}

func TestBodyCutOffIsUnavailable(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"status":`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	if _, err := n.Health(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

func TestClientTimeoutIsUnavailableNotContextError(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	client := srv.Client()
	client.Timeout = time.Nanosecond
	n, err := New("east", srv.URL, client)
	if err != nil {
		t.Fatal(err)
	}
	err = operations[0].run(n)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v is a context deadline, but the caller set none", err)
	}
}

func TestCallerCancelsDuringResponse(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"Health", "List", "Range"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			n := serve(t, func(http.ResponseWriter, *http.Request) {
				cancel()
				<-release
			})
			t.Cleanup(func() { close(release) })
			var err error
			switch op {
			case "Health":
				_, err = n.Health(ctx)
			case "List":
				_, err = n.List(ctx, testToken, "s")
			case "Range":
				_, err = n.Range(ctx, testToken, "s/f", "e", 0, 1)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if errors.Is(err, ErrUnavailable) {
				t.Errorf("error %v reports the node as down", err)
			}
		})
	}
}

func TestCallerCancelsDuringBody(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"status":`))
		w.(http.Flusher).Flush()
		cancel()
		<-release
	})
	t.Cleanup(func() { close(release) })
	if _, err := n.Health(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestCallerDeadlinePassed(t *testing.T) {
	t.Parallel()
	n := serve(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	if _, err := n.Health(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestBuildRequestFailure(t *testing.T) {
	t.Parallel()
	n := &Node{name: "east", base: "://bad", client: http.DefaultClient}
	if _, err := n.Health(context.Background()); err == nil {
		t.Fatal("Health with an unusable base URL succeeded")
	}
}
