package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SpliceFS/client/internal/version"
)

const (
	testClientVersion = "v0.4.4"
	testHost          = "east.example.com:8443"
)

const (
	wantOlderMinor = "server east.example.com:8443 runs splicefsd 0.3.3, and this client (0.4.4) " +
		"needs splicefsd 0.4 or newer: upgrade the server, or install an older client"
	wantOtherMain = "server east.example.com:8443 runs splicefsd 1.2.0 and this client is 0.4.4: " +
		"client and server must have the same first version number"
	wantNoHeader = "server east.example.com:8443 does not report its version, so it is older than 0.4.0, " +
		"and this client (0.4.4) needs splicefsd 0.4 or newer: upgrade the server, or install an older client"
)

func TestCheckVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		client   string
		reported []string
		// status is the status of the response, which the call wanted when it is 200.
		status int
		// wantMessage is the exact text of the refusal, empty when the answer is accepted.
		wantMessage string
	}{
		{name: "dev client checks nothing", client: "dev", status: http.StatusOK},
		{name: "dev client ignores an older server", client: "dev", reported: []string{"v0.1.0"}, status: http.StatusOK},
		{name: "commit hash client checks nothing", client: "3f9c2ab", status: http.StatusOK},
		{name: "empty client checks nothing", client: "", status: http.StatusOK},
		{name: "same version", client: testClientVersion, reported: []string{"v0.4.4"}, status: http.StatusOK},
		{name: "lower fix", client: testClientVersion, reported: []string{"v0.4.0"}, status: http.StatusOK},
		{name: "higher fix", client: testClientVersion, reported: []string{"v0.4.9"}, status: http.StatusOK},
		{name: "higher minor", client: testClientVersion, reported: []string{"v0.5.0"}, status: http.StatusOK},
		{name: "server without v", client: testClientVersion, reported: []string{"0.4.4"}, status: http.StatusOK},
		{name: "server with two numbers", client: testClientVersion, reported: []string{"v0.4"}, status: http.StatusOK},
		{
			name: "server with a git suffix", client: testClientVersion,
			reported: []string{"v0.4.4-3-gabc123-dirty"}, status: http.StatusOK,
		},
		{name: "client without v", client: "0.4.4", reported: []string{"v0.4.4"}, status: http.StatusOK},
		{name: "client with a git suffix", client: "v0.4.4-3-gabc123", reported: []string{"v0.4.4"}, status: http.StatusOK},
		{name: "development server", client: testClientVersion, reported: []string{"dev"}, status: http.StatusOK},
		{name: "empty header", client: testClientVersion, reported: []string{""}, status: http.StatusOK},
		{name: "one number only", client: testClientVersion, reported: []string{"v1"}, status: http.StatusOK},
		{name: "missing minor", client: testClientVersion, reported: []string{"v1."}, status: http.StatusOK},
		{
			name: "number too large", client: testClientVersion,
			reported: []string{"v99999999999999999999.1.0"}, status: http.StatusOK,
		},
		{
			name: "other main is refused", client: testClientVersion, reported: []string{"v1.2.0"},
			status: http.StatusOK, wantMessage: wantOtherMain,
		},
		{
			name: "other main wins over a higher minor", client: testClientVersion,
			reported: []string{"v1.9.0-extra"}, status: http.StatusOK,
			wantMessage: strings.Replace(wantOtherMain, "1.2.0", "1.9.0", 1),
		},
		{
			name: "lower main is refused", client: "v1.2.0", reported: []string{"v0.9.9"}, status: http.StatusOK,
			wantMessage: "server east.example.com:8443 runs splicefsd 0.9.9 and this client is 1.2.0: " +
				"client and server must have the same first version number",
		},
		{
			name: "lower minor is refused", client: testClientVersion, reported: []string{"v0.3.3"},
			status: http.StatusOK, wantMessage: wantOlderMinor,
		},
		{
			name: "refused also on an error status", client: testClientVersion, reported: []string{"v0.3.3"},
			status: http.StatusNotFound, wantMessage: wantOlderMinor,
		},
		{
			name: "no header on the wanted status", client: testClientVersion, status: http.StatusOK,
			wantMessage: wantNoHeader,
		},
		{name: "no header on another status", client: testClientVersion, status: http.StatusBadGateway},
		{
			name: "client of two numbers", client: "v0.4", status: http.StatusOK,
			wantMessage: strings.ReplaceAll(wantNoHeader, "0.4.4", "0.4"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkVersion(testHost, tt.client, tt.reported, tt.status, http.StatusOK)
			requireRefusal(t, err, tt.wantMessage)
		})
	}
}

// requireRefusal checks that err is a refusal with exactly the message want, or, when want
// is empty, that there is no error.
func requireRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("checkVersion() = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("checkVersion() = nil, want %q", want)
	}
	if err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, ErrIncompatible) {
		t.Errorf("error %v does not match ErrIncompatible", err)
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrProtocol) {
		t.Errorf("error %v matches ErrUnavailable or ErrProtocol", err)
	}
}

func TestParseRelease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		wantText string
		wantOK   bool
	}{
		{in: "v0.4.4", wantText: "0.4.4", wantOK: true},
		{in: "0.4.4", wantText: "0.4.4", wantOK: true},
		{in: "v10.20.30", wantText: "10.20.30", wantOK: true},
		{in: "v0.4", wantText: "0.4", wantOK: true},
		{in: "v0.4.", wantText: "0.4", wantOK: true},
		{in: "v0.4.x", wantText: "0.4", wantOK: true},
		{in: "v0.4.4-rc1", wantText: "0.4.4", wantOK: true},
		{in: "v0.4.4.9", wantText: "0.4.4", wantOK: true},
		{in: "dev"},
		{in: ""},
		{in: "v"},
		{in: "v1"},
		{in: "v1-2"},
		{in: "v1."},
		{in: "v.1"},
		{in: "-1.2.3"},
		{in: "vv0.4.4"},
		{in: "v99999999999999999999.1"},
		{in: "v1.99999999999999999999"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, ok := parseRelease(tt.in)
			if ok != tt.wantOK || got.text != tt.wantText {
				t.Errorf("parseRelease(%q) = %q, %v, want %q, %v", tt.in, got.text, ok, tt.wantText, tt.wantOK)
			}
		})
	}
}

func TestNewTakesHostAndOwnVersion(t *testing.T) {
	t.Parallel()
	n, err := New("east", "https://east.example.com:8443/api", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if n.host != testHost {
		t.Errorf("host = %q, want %q", n.host, testHost)
	}
	if n.clientVersion != version.String() {
		t.Errorf("clientVersion = %q, want %q", n.clientVersion, version.String())
	}
}

// answerWith serves every request with status and, when reported is not empty, the version
// header. The body is JSON with a message, like the real server's errors.
func answerWith(t *testing.T, status int, reported string) *Node {
	t.Helper()
	n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if reported != "" {
			w.Header().Set(serverVersionHeader, reported)
		}
		writeJSON(t, w, status, `{"error":"from the server"}`)
	})
	n.clientVersion = testClientVersion
	return n
}

// hostOf names a test node the way an incompatibility message does.
func hostOf(n *Node) string {
	return strings.TrimSuffix(strings.TrimPrefix(n.base, "http://"), "/api")
}

func TestEveryOperationRefusesAServerWithoutHeader(t *testing.T) {
	t.Parallel()
	for _, operation := range operations() {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			n := answerWith(t, operation.okStatus, "")
			err := operation.run(n)
			if !errors.Is(err, ErrIncompatible) {
				t.Fatalf("error = %v, want ErrIncompatible", err)
			}
			want := strings.Replace(wantNoHeader, testHost, hostOf(n), 1)
			if err.Error() != want {
				t.Errorf("message = %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestEveryOperationRefusesAnIncompatibleServer(t *testing.T) {
	t.Parallel()
	for _, operation := range operations() {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			n := answerWith(t, operation.okStatus, "v0.3.3")
			err := operation.run(n)
			if !errors.Is(err, ErrIncompatible) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("error = %v, want ErrIncompatible and not ErrUnavailable", err)
			}
			want := strings.Replace(wantOlderMinor, testHost, hostOf(n), 1)
			if err.Error() != want {
				t.Errorf("message = %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestCompatibleServerIsUsed(t *testing.T) {
	t.Parallel()
	for _, reported := range []string{"v0.4.4", "v0.4.0", "v0.9.1", "dev"} {
		t.Run(reported, func(t *testing.T) {
			t.Parallel()
			n := answerWith(t, http.StatusOK, reported)
			if _, err := n.Health(context.Background()); err != nil {
				t.Errorf("Health() error = %v, want nil", err)
			}
		})
	}
}

func TestDevClientAcceptsServerWithoutHeader(t *testing.T) {
	t.Parallel()
	n := answerWith(t, http.StatusOK, "")
	n.clientVersion = "dev"
	if _, err := n.Health(context.Background()); err != nil {
		t.Errorf("Health() error = %v, want nil", err)
	}
}

func TestErrorStatusWithoutHeaderKeepsItsOwnError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusBadGateway, ErrUnavailable},
		{http.StatusServiceUnavailable, ErrUnavailable},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusTeapot, ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			t.Parallel()
			n := answerWith(t, tt.status, "")
			err := runHealth(n)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if errors.Is(err, ErrIncompatible) {
				t.Errorf("error %v blames the server version for a status without the header", err)
			}
		})
	}
}

func TestErrorStatusWithOldVersionIsRefused(t *testing.T) {
	t.Parallel()
	n := answerWith(t, http.StatusNotFound, "v0.3.3")
	err := runHealth(n)
	if !errors.Is(err, ErrIncompatible) || errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrIncompatible and not ErrNotFound", err)
	}
}

// spyBody counts how often it is closed.
type spyBody struct {
	io.Reader

	closed *atomic.Int32
}

func (b spyBody) Close() error {
	b.closed.Add(1)
	return nil
}

func TestRefusalClosesTheBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		reported string
	}{
		{name: "old minor", reported: "v0.3.3"},
		{name: "other main", reported: "v1.0.0"},
		{name: "no header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var closed atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				header := http.Header{}
				if tt.reported != "" {
					header.Set(serverVersionHeader, tt.reported)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     header,
					Body:       spyBody{Reader: strings.NewReader(`{}`), closed: &closed},
					Request:    r,
				}, nil
			})}
			n, err := New("east", "http://"+testHost+"/api", client)
			if err != nil {
				t.Fatal(err)
			}
			n.clientVersion = testClientVersion
			if err := runHealth(n); !errors.Is(err, ErrIncompatible) {
				t.Fatalf("error = %v, want ErrIncompatible", err)
			}
			if got := closed.Load(); got != 1 {
				t.Errorf("body closed %d times, want 1", got)
			}
		})
	}
}
