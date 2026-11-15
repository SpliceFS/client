package remote

import (
	"context"
	"errors"
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
		contentLength int64,
	) {
		headers, ok := fuzzHeader(contentType, lastModified, etag, contentRange)
		if !ok || status < 100 || status > 599 {
			return
		}
		n := answering(t, status, headers, contentLength, []byte(body))
		checkAnswers(t, n, body, contentRange)
	})
}

// fuzzHeader builds the response headers of a fuzz case. It reports false for a value with
// control characters: those cannot exist on the wire, so they would only test net/http.
func fuzzHeader(contentType, lastModified, etag, contentRange string) (http.Header, bool) {
	headers := http.Header{}
	for key, value := range map[string]string{
		"Content-Type": contentType, "Last-Modified": lastModified, "ETag": etag,
		"Content-Range": contentRange, wire.HeaderHeadDigest: "00ff", wire.HeaderTailDigest: "00ff",
	} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, false
		}
		headers.Set(key, value)
	}
	return headers, true
}

// checkClassified fails for an error that is none of the exported ones, because callers
// branch on them with errors.Is.
func checkClassified(t *testing.T, operation string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	sentinels := []error{
		ErrUnauthorized, ErrForbidden, ErrNotFound, ErrChanged, ErrBadRange, ErrRateLimited, ErrProtocol,
		ErrUnavailable,
	}
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			return
		}
	}
	t.Fatalf("%s failed with %v, which is none of the exported errors", operation, err)
}

// checkAnswers calls every operation against the fixed answer of n.
func checkAnswers(t *testing.T, n *Node, body, contentRange string) {
	t.Helper()
	ctx := context.Background()
	checkLogin(t, n, body)
	_, err := n.Health(ctx)
	checkClassified(t, "Health", err)
	_, err = n.Nodes(ctx, "t")
	checkClassified(t, "Nodes", err)
	_, err = n.List(ctx, "t", "share/dir")
	checkClassified(t, "List", err)
	_, err = n.Space(ctx, "t", "share")
	checkClassified(t, "Space", err)
	checkHead(t, n)
	checkRangeAnswer(t, n, contentRange)
}

func checkLogin(t *testing.T, n *Node, body string) {
	t.Helper()
	login, err := n.Login(context.Background(), "u", "p")
	checkClassified(t, "Login", err)
	if err == nil && login.Token == "" {
		// An empty token would be sent as no authentication at all.
		t.Fatalf("Login accepted an answer without a token: %q", body)
	}
}

func checkHead(t *testing.T, n *Node) {
	t.Helper()
	meta, err := n.Head(context.Background(), "t", "share/file")
	checkClassified(t, "Head", err)
	// A file version is pinned by these fields, so none may be missing or invalid.
	if err == nil && (meta.Size < 0 || meta.ETag == "" || meta.HeadDigest == "" || meta.TailDigest == "") {
		t.Fatalf("Head accepted %+v", meta)
	}
}

// checkRangeAnswer reads a block from a server that answered with a fixed
// Content-Range. A block that is accepted must be exactly the requested bytes long.
func checkRangeAnswer(t *testing.T, n *Node, contentRange string) {
	t.Helper()
	const off, length = 0, 10
	block, err := n.Range(context.Background(), "t", "share/file", "tag", off, length)
	checkClassified(t, "Range", err)
	if err != nil {
		return
	}
	closeLater(t, block)
	data, err := io.ReadAll(block)
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
	f.Fuzz(func(t *testing.T, input string) {
		parsed, ok := parseContentRange(input)
		if !ok {
			return
		}
		again, ok := parseContentRange(fmt.Sprintf("bytes %d-%d/*", parsed.first, parsed.last))
		if !ok || again != parsed {
			t.Fatalf("parseContentRange(%q) = %+v does not survive a round trip", input, parsed)
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
	f.Fuzz(func(t *testing.T, path string) {
		escaped, err := escapePath(path)
		if err != nil {
			if escaped != "" {
				t.Fatalf("escapePath(%q) failed but returned %q", path, escaped)
			}
			return
		}
		checkEscapedSegments(t, path, escaped)
	})
}

// checkEscapedSegments checks that each segment of escaped decodes to the segment of path it
// came from, and that none can be read as another request path.
func checkEscapedSegments(t *testing.T, path, escaped string) {
	t.Helper()
	given := strings.Split(path, "/")
	segments := strings.Split(escaped, "/")
	if len(segments) != len(given) {
		t.Fatalf("escapePath(%q) = %q changed the number of segments", path, escaped)
	}
	for i, segment := range segments {
		back, err := url.PathUnescape(segment)
		// Every segment must decode to what was given, or the server would see another name.
		if err != nil || back != given[i] || strings.ContainsAny(segment, "/?#") {
			t.Fatalf("escapePath(%q) = %q has the segment %q", path, escaped, segment)
		}
		if segment == "" || segment == "." || segment == ".." {
			t.Fatalf("escapePath(%q) = %q has a dot or empty segment", path, escaped)
		}
	}
}
