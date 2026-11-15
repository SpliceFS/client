package remote

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SpliceFS/client/internal/version"
)

const testToken = "tok-123"

// serve starts a test server under the base path /api and returns a Node for it.
func serve(t *testing.T, h http.HandlerFunc) *Node {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	n, err := New("east", srv.URL+"/api", srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

// want is the exact request a handler expects.
type want struct {
	method string
	uri    string
	header map[string]string
	absent []string
}

// check reports every difference between the request and w. It uses Errorf because
// handlers run on the server's goroutines.
func (w want) check(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != w.method {
		t.Errorf("method = %q, want %q", r.Method, w.method)
	}
	if r.RequestURI != w.uri {
		t.Errorf("request URI = %q, want %q", r.RequestURI, w.uri)
	}
	if got, wantUA := r.Header.Get("User-Agent"), "splicefs/"+version.String(); got != wantUA {
		t.Errorf("User-Agent = %q, want %q", got, wantUA)
	}
	for k, v := range w.header {
		if got := r.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range w.absent {
		if _, ok := r.Header[k]; ok {
			t.Errorf("header %s is set, want absent", k)
		}
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// An answer of status 204 has no body by definition, and the server refuses to send one.
	if status != http.StatusNoContent {
		writeBody(t, w, []byte(body))
	}
}

// writeBody writes a response body. It uses Errorf because handlers run on the server's
// goroutines.
func writeBody(t *testing.T, w io.Writer, body []byte) {
	t.Helper()
	if _, err := w.Write(body); err != nil {
		t.Errorf("write response body: %v", err)
	}
}

// closeLater closes c when the test ends and reports a failed close.
func closeLater(t *testing.T, c io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
}

// bearerHeader is the request header a handler expects from a logged-in client.
func bearerHeader() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken}
}
