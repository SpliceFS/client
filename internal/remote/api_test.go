package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
)

func TestHealth(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		want{method: "GET", uri: "/api/v1/health", absent: []string{"Authorization"}}.check(t, r)
		writeJSON(t, w, 200, `{"status":"ok","version":"v1.0.0","node":"east"}`)
	})
	got, err := n.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if want := (wire.Health{Status: "ok", Version: "v1.0.0", Node: "east"}); got != want {
		t.Errorf("Health = %+v, want %+v", got, want)
	}
}

// loginHandler checks that a login request carries the credentials as JSON and answers
// with body.
func loginHandler(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		want{
			method: "POST",
			uri:    "/api/v1/login",
			header: map[string]string{"Content-Type": "application/json"},
			absent: []string{"Authorization"},
		}.check(t, r)
		var req wire.LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req != (wire.LoginRequest{User: "ann", Password: "p\"w"}) {
			t.Errorf("request = %+v", req)
		}
		writeJSON(t, w, 200, body)
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "token issued", body: `{"token":"abc","expires":"2030-01-02T03:04:05Z"}`},
		{name: "no token", body: `{"expires":"2030-01-02T03:04:05Z"}`, wantErr: ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := serve(t, loginHandler(t, tt.body))
			got, err := n.Login(context.Background(), "ann", "p\"w")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Login error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && (got.Token != "abc" || !got.Expires.Equal(expires)) {
				t.Errorf("Login = %+v", got)
			}
		})
	}
}

func TestSendRejectsUnencodablePayload(t *testing.T) {
	t.Parallel()
	n := serve(t, func(http.ResponseWriter, *http.Request) { t.Error("request must not be sent") })
	request := call{operation: "x", method: http.MethodPost, path: "/", payload: make(chan int)}
	resp, err := n.send(context.Background(), request, 200)
	if err == nil {
		closeBody(resp)
		t.Fatal("send with a channel payload succeeded")
	}
}

func TestNodes(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		want{method: "GET", uri: "/api/v1/nodes", header: bearerHeader()}.check(t, r)
		writeJSON(t, w, 200, `{"cluster":"c","nodes":[{"name":"east","url":"https://east.example.com/api"}]}`)
	})
	got, err := n.Nodes(context.Background(), testToken)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	want := wire.Nodes{Cluster: "c", Nodes: []wire.Node{{Name: "east", URL: "https://east.example.com/api"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Nodes = %+v, want %+v", got, want)
	}
}

func TestSpace(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		want{method: "GET", uri: "/api/v1/space/my%20share", header: bearerHeader()}.check(t, r)
		writeJSON(t, w, 200, `{"total":1000,"free":400}`)
	})
	got, err := n.Space(context.Background(), testToken, "my share")
	if err != nil {
		t.Fatalf("Space: %v", err)
	}
	if want := (wire.Space{Total: 1000, Free: 400}); got != want {
		t.Errorf("Space = %+v, want %+v", got, want)
	}
}

func TestSpaceRejectsBadShares(t *testing.T) {
	t.Parallel()
	n := serve(t, func(http.ResponseWriter, *http.Request) { t.Error("request must not be sent") })
	tests := []struct {
		share   string
		wantErr error
	}{
		{share: "", wantErr: ErrInvalidPath},
		{share: "..", wantErr: ErrInvalidPath},
		{share: "a/b", wantErr: errSlashInShare},
	}
	for _, tt := range tests {
		if _, err := n.Space(context.Background(), testToken, tt.share); !errors.Is(err, tt.wantErr) {
			t.Errorf("Space(%q) error = %v, want %v", tt.share, err, tt.wantErr)
		}
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	entry := `{"entries":[{"name":"a b","dir":false,"size":5,"mtime":"2030-01-02T03:04:05Z","etag":"\"e1\""}]}`
	wantEntries := []wire.Entry{{Name: "a b", Size: 5, MTime: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), ETag: `"e1"`}}
	tests := []struct {
		name string
		dir  string
		uri  string
	}{
		{name: "shares", dir: "", uri: "/api/v1/tree/"},
		{name: "share", dir: "media", uri: "/api/v1/tree/media/"},
		{
			name: "special characters",
			dir:  "my share/a#b/100%/caf\u00e9?",
			uri:  "/api/v1/tree/my%20share/a%23b/100%25/caf%C3%A9%3F/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := serve(t, func(w http.ResponseWriter, r *http.Request) {
				want{method: "GET", uri: tt.uri, header: bearerHeader()}.check(t, r)
				writeJSON(t, w, 200, entry)
			})
			got, err := n.List(context.Background(), testToken, tt.dir)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !reflect.DeepEqual(got, wantEntries) {
				t.Errorf("List = %+v, want %+v", got, wantEntries)
			}
		})
	}
}

func TestListDecodesGzip(t *testing.T) {
	t.Parallel()
	body := `{"entries":[{"name":"x","dir":true,"size":0,"mtime":"2030-01-02T03:04:05Z"}]}`
	n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if ae := r.Header.Get("Accept-Encoding"); !strings.Contains(ae, "gzip") {
			t.Errorf("Accept-Encoding = %q, want gzip", ae)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		writeBody(t, w, gzipBytes(t, body))
	})
	got, err := n.List(context.Background(), testToken, "media")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "x" || !got[0].Dir {
		t.Errorf("List = %+v", got)
	}
}

func TestListRejectsBadDir(t *testing.T) {
	t.Parallel()
	n := serve(t, func(http.ResponseWriter, *http.Request) { t.Error("request must not be sent") })
	for _, dir := range []string{"a//b", "a/../b", "a/", "/a", "."} {
		if _, err := n.List(context.Background(), testToken, dir); err == nil {
			t.Errorf("List(%q) succeeded", dir)
		}
	}
}

func TestHead(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		want{method: "HEAD", uri: "/api/v1/file/my%20share/a%23b/100%25/caf%C3%A9%3F.bin", header: bearerHeader()}.check(t, r)
		w.Header().Set("Content-Length", "12345")
		w.Header().Set("Last-Modified", "Wed, 02 Jan 2030 03:04:05 GMT")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set(wire.HeaderHeadDigest, "deadbeef")
		w.Header().Set(wire.HeaderTailDigest, "cafe")
	})
	got, err := n.Head(context.Background(), testToken, "my share/a#b/100%/caf\u00e9?.bin")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	want := Meta{
		Size: 12345, MTime: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), ETag: `"abc"`,
		HeadDigest: "deadbeef", TailDigest: "cafe",
	}
	if got != want {
		t.Errorf("Head = %+v, want %+v", got, want)
	}
}

func TestHeadProtocolViolations(t *testing.T) {
	t.Parallel()
	full := map[string]string{
		"Content-Length":      "10",
		"Last-Modified":       "Wed, 02 Jan 2030 03:04:05 GMT",
		"ETag":                `"abc"`,
		wire.HeaderHeadDigest: "deadbeef",
		wire.HeaderTailDigest: "cafe",
	}
	tests := []struct {
		name   string
		header string
		// value replaces the header; an empty value removes it.
		value string
	}{
		{name: "no content length", header: "Content-Length"},
		{name: "no last modified", header: "Last-Modified"},
		{name: "bad last modified", header: "Last-Modified", value: "yesterday"},
		{name: "no etag", header: "ETag"},
		{name: "no head digest", header: wire.HeaderHeadDigest},
		{name: "no tail digest", header: wire.HeaderTailDigest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := maps.Clone(full)
			if tt.value == "" {
				delete(headers, tt.header)
			} else {
				headers[tt.header] = tt.value
			}
			n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range headers {
					w.Header().Set(k, v)
				}
			})
			_, err := n.Head(context.Background(), testToken, "s/f")
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Head error = %v, want ErrProtocol", err)
			}
			if !strings.Contains(err.Error(), tt.header) {
				t.Errorf("error %q does not mention %q", err, tt.header)
			}
		})
	}
}

func TestHeadRejectsBadPath(t *testing.T) {
	t.Parallel()
	n := serve(t, func(http.ResponseWriter, *http.Request) { t.Error("request must not be sent") })
	if _, err := n.Head(context.Background(), testToken, "s/../f"); err == nil {
		t.Fatal("Head with .. succeeded")
	}
}

func rangeHeaders(off, last, total string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Range", "bytes "+off+"-"+last+"/"+total)
	}
}

func TestRange(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		want{
			method: "GET",
			uri:    "/api/v1/file/my%20share/a%23b/100%25/caf%C3%A9%3F.bin",
			header: map[string]string{
				"Authorization":   "Bearer " + testToken,
				"Range":           "bytes=100-109",
				"If-Match":        `"abc"`,
				"Accept-Encoding": "identity",
			},
		}.check(t, r)
		rangeHeaders("100", "109", "5000")(w)
		w.WriteHeader(http.StatusPartialContent)
		writeBody(t, w, []byte("0123456789"))
	})
	rc, err := n.Range(context.Background(), testToken, "my share/a#b/100%/caf\u00e9?.bin", `"abc"`, 100, 10)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	closeLater(t, rc)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "0123456789" {
		t.Errorf("body = %q", got)
	}
}

func TestRangeIgnoresBytesBeyondLength(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		rangeHeaders("0", "3", "*")(w)
		w.WriteHeader(http.StatusPartialContent)
		writeBody(t, w, []byte("abcdefgh"))
	})
	rc, err := n.Range(context.Background(), testToken, "s/f", "e", 0, 4)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	closeLater(t, rc)
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != "abcd" {
		t.Errorf("ReadAll = %q, %v; want abcd, nil", got, err)
	}
}

func TestRangeShortBody(t *testing.T) {
	t.Parallel()
	n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		rangeHeaders("0", "9", "10")(w)
		w.WriteHeader(http.StatusPartialContent)
		writeBody(t, w, []byte("0123"))
	})
	rc, err := n.Range(context.Background(), testToken, "s/f", "e", 0, 10)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	closeLater(t, rc)
	got, err := io.ReadAll(rc)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("read error = %v, want io.ErrUnexpectedEOF", err)
	}
	if string(got) != "0123" {
		t.Errorf("got %q before the error, want %q", got, "0123")
	}
}

func TestRangeContentRangeMismatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		header string
	}{
		{name: "wrong start", status: 206, header: "bytes 101-110/5000"},
		{name: "end too early", status: 206, header: "bytes 100-108/5000"},
		{name: "end too late", status: 206, header: "bytes 100-110/5000"},
		{name: "missing", status: 206, header: ""},
		{name: "other unit", status: 206, header: "items 100-109/5000"},
		{name: "full content answer", status: 200, header: "bytes 100-109/5000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Content-Range", tt.header)
				}
				w.WriteHeader(tt.status)
				writeBody(t, w, []byte("0123456789"))
			})
			rc, err := n.Range(context.Background(), testToken, "s/f", "e", 100, 10)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Range error = %v, want ErrProtocol", err)
			}
			if rc != nil {
				t.Error("Range returned a reader with an error")
			}
		})
	}
}

func TestRangeRejectsBadArguments(t *testing.T) {
	t.Parallel()
	n := serve(t, func(http.ResponseWriter, *http.Request) { t.Error("request must not be sent") })
	tests := []struct {
		name    string
		file    string
		etag    string
		off     int64
		length  int64
		wantErr error
	}{
		{name: "empty etag", file: "s/f", etag: "", off: 0, length: 1, wantErr: ErrEmptyETag},
		{name: "negative offset", file: "s/f", etag: "e", off: -1, length: 1, wantErr: ErrNegativeOffset},
		{name: "zero length", file: "s/f", etag: "e", off: 0, length: 0, wantErr: errNotPositive},
		{name: "negative length", file: "s/f", etag: "e", off: 0, length: -5, wantErr: errNotPositive},
		{name: "overflow", file: "s/f", etag: "e", off: 1<<63 - 1, length: 2, wantErr: errRangeOverflow},
		{name: "bad path", file: "s/../f", etag: "e", off: 0, length: 1, wantErr: ErrInvalidPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := n.Range(context.Background(), testToken, tt.file, tt.etag, tt.off, tt.length)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Range error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseContentRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in          string
		first, last int64
		ok          bool
	}{
		{in: "bytes 0-9/10", first: 0, last: 9, ok: true},
		{in: "bytes 5-6/*", first: 5, last: 6, ok: true},
		{in: "", ok: false},
		{in: "0-9/10", ok: false},
		{in: "bytes 0-9", ok: false},
		{in: "bytes 0/10", ok: false},
		{in: "bytes a-9/10", ok: false},
		{in: "bytes 0-b/10", ok: false},
		{in: "bytes */10", ok: false},
	}
	for _, tt := range tests {
		got, ok := parseContentRange(tt.in)
		want := byteRange{first: tt.first, last: tt.last}
		if ok != tt.ok || (ok && got != want) {
			t.Errorf("parseContentRange(%q) = %+v, %v; want %+v, %v", tt.in, got, ok, want, tt.ok)
		}
	}
}
