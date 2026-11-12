package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/SpliceFS/common/wire"
)

// roundTripFunc is a transport that answers from memory, so no network is involved.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// answering returns a Node whose server always sends the given response.
func answering(t *testing.T, status int, header http.Header, length int64, body []byte) *Node {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := &http.Response{
			StatusCode:    status,
			Status:        strconv.Itoa(status) + " " + http.StatusText(status),
			Header:        header.Clone(),
			Body:          io.NopCloser(strings.NewReader(string(body))),
			ContentLength: length,
			Request:       r,
		}
		if r.Method == http.MethodHead {
			resp.Body = http.NoBody
		}
		return resp, nil
	})}
	n, err := New("east", "http://node.example/api", client)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// FuzzResponses lets a server answer every call with arbitrary status, headers and body.
// The server is not trusted, so every accepted answer must be one the caller can use.
func FuzzResponses(f *testing.F) {
	const jsonType, stamp = "application/json", "Mon, 02 Jan 2006 15:04:05 GMT"
	for _, seed := range []struct {
		status                     int
		contentType, body          string
		lastModified, etag, crange string
		length                     int64
	}{
		{200, jsonType, `{"token":"t","expires":"2030-01-01T00:00:00Z"}`, "", "", "", 5},
		{200, jsonType, `{"entries":[{"name":"a","size":3,"etag":"e"}]}`, "", "", "", 3},
		{200, jsonType, `{"cluster":"c","nodes":[{"name":"n","url":"http://x"}]}`, "", "", "", 0},
		{200, jsonType, `{"total":10,"free":5}`, stamp, "tag", "00ff", 10},
		{206, "application/octet-stream", "0123456789", "", "tag", "bytes 0-9/10", 10},
		{206, "text/plain", "x", "", "tag", "bytes 5-4/10", 1},
		{200, jsonType, `{"token":""}`, "garbage", "", "", -1},
		{200, jsonType, `[1,2`, "", "", "bytes 0-", 1 << 62},
		{500, "text/html", "<html>", "", "", "", 6},
		{200, jsonType + "; charset=utf-8", `{"entries":null}`, "", "", "bytes -1--5/*", 0},
	} {
		f.Add(seed.status, seed.contentType, seed.body, seed.lastModified, seed.etag,
			seed.crange, seed.length)
	}
	f.Fuzz(func(t *testing.T, status int, contentType, body, lastModified, etag, contentRange string,
		contentLength int64) {
		h := http.Header{}
		// Header values with control characters cannot exist on the wire, so they would
		// only test net/http.
		for k, v := range map[string]string{
			"Content-Type": contentType, "Last-Modified": lastModified, "ETag": etag,
			"Content-Range": contentRange, wire.HeaderHeadDigest: "00ff",
		} {
			if strings.ContainsAny(v, "\r\n\x00") {
				return
			}
			h.Set(k, v)
		}
		if status < 100 || status > 599 {
			return
		}
		n := answering(t, status, h, contentLength, []byte(body))
		ctx := context.Background()

		if r, err := n.Login(ctx, "u", "p"); err == nil && r.Token == "" {
			// An empty token would be sent as no authentication at all.
			t.Fatalf("Login accepted an answer without a token: %q", body)
		}
		_, _ = n.Health(ctx)
		_, _ = n.Nodes(ctx, "t")
		_, _ = n.List(ctx, "t", "share/dir")
		_, _ = n.Space(ctx, "t", "share")
		if m, err := n.Head(ctx, "t", "share/file"); err == nil {
			// A file version is pinned by these fields, so none may be missing or invalid.
			if m.Size < 0 || m.ETag == "" || m.HeadDigest == "" {
				t.Fatalf("Head accepted %+v", m)
			}
		}
		checkRangeAnswer(t, n, contentRange)
	})
}

// checkRangeAnswer reads a block from a server that answered with a fixed
// Content-Range. A block that is accepted must be exactly the requested bytes long.
func checkRangeAnswer(t *testing.T, n *Node, contentRange string) {
	t.Helper()
	const off, length = 0, 10
	rc, err := n.Range(context.Background(), "t", "share/file", "tag", off, length)
	if err != nil {
		return
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	// The cache stores whatever it is given as a whole block, so a short body without an
	// error would be saved as if it were complete.
	if err == nil && len(data) != length {
		t.Fatalf("Range with Content-Range %q gave %d bytes and no error", contentRange, len(data))
	}
	if len(data) > length {
		t.Fatalf("Range gave %d bytes, more than the %d requested", len(data), length)
	}
}

// FuzzParseContentRange checks the header parser against its own encoding and for no
// panic on any text.
func FuzzParseContentRange(f *testing.F) {
	for _, s := range []string{
		"bytes 0-9/10", "bytes 5-6/*", "", "bytes ", "bytes -", "bytes 0-9", "bytes a-9/10",
		"bytes 9223372036854775807-9223372036854775807/*", "bytes 99999999999999999999-1/2",
		"bytes -1--2/3", "bytes 1-2-3/4", "bytes +1-+2/3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		first, last, ok := parseContentRange(in)
		if !ok {
			return
		}
		a, b, ok := parseContentRange(fmt.Sprintf("bytes %d-%d/*", first, last))
		if !ok || a != first || b != last {
			t.Fatalf("parseContentRange(%q) = %d, %d does not survive a round trip", in, first, last)
		}
	})
}

// FuzzEscapePath checks that no path chosen by a server or a user can become a request
// path other than the one it names.
func FuzzEscapePath(f *testing.F) {
	for _, s := range []string{
		"share/a/b", "a", "", "/", "a//b", ".", "..", "a/../b", "a/./b", "a b/c%2Fd", "a?b#c",
		"caf\u00e9/\x00", "a\\b", "%2e%2e", strings.Repeat("a/", 1000),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		escaped, err := escapePath(p)
		if err != nil {
			if escaped != "" {
				t.Fatalf("escapePath(%q) failed but returned %q", p, escaped)
			}
			return
		}
		segments := strings.Split(escaped, "/")
		if len(segments) != len(strings.Split(p, "/")) {
			t.Fatalf("escapePath(%q) = %q changed the number of segments", p, escaped)
		}
		for i, s := range segments {
			back, err := url.PathUnescape(s)
			// Every segment must decode to what was given, or the server would see another name.
			if err != nil || back != strings.Split(p, "/")[i] || strings.ContainsAny(s, "/?#") {
				t.Fatalf("escapePath(%q) = %q has the segment %q", p, escaped, s)
			}
			if s == "" || s == "." || s == ".." {
				t.Fatalf("escapePath(%q) = %q has a dot or empty segment", p, escaped)
			}
		}
	})
}
