package remote

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SpliceFS/common/wire"
)

// Meta describes one file on one node, as answered to a HEAD request.
type Meta struct {
	// Size is the file size in bytes.
	Size int64
	// MTime is the file's modification time.
	MTime time.Time
	// ETag is the file's tag, derived from its size and modification time.
	ETag string
	// HeadDigest is the hex SHA-256 of the first wire.HeadDigestBytes bytes of the file.
	HeadDigest string
}

// Health asks the node whether it is up. It needs no login.
func (n *Node) Health(ctx context.Context) (wire.Health, error) {
	var health wire.Health
	request := call{operation: "health", method: http.MethodGet, path: "/v1/health"}
	err := fetch(ctx, n, request, maxSmallJSON, &health)
	return health, err
}

// Login exchanges a user name and password for a bearer token.
func (n *Node) Login(ctx context.Context, user, password string) (wire.LoginResponse, error) {
	const operation = "login"
	request := call{
		operation: operation,
		method:    http.MethodPost,
		path:      "/v1/login",
		payload:   wire.LoginRequest{User: user, Password: password},
	}
	var login wire.LoginResponse
	err := fetch(ctx, n, request, maxSmallJSON, &login)
	if err == nil && login.Token == "" {
		return wire.LoginResponse{}, n.protocolError(operation, "response has no token")
	}
	return login, err
}

// Nodes returns the cluster's node list as this node knows it.
func (n *Node) Nodes(ctx context.Context, token string) (wire.Nodes, error) {
	var nodes wire.Nodes
	request := call{operation: "nodes", method: http.MethodGet, path: "/v1/nodes", token: token}
	err := fetch(ctx, n, request, maxSmallJSON, &nodes)
	return nodes, err
}

// List returns the entries of a directory. An empty dir lists the shares the user may
// read; otherwise dir is "share" or "share/a/b", unescaped.
func (n *Node) List(ctx context.Context, token, dir string) ([]wire.Entry, error) {
	const operation = "list"
	path := "/v1/tree/"
	if dir != "" {
		escaped, err := n.route(operation, path, dir)
		if err != nil {
			return nil, err
		}
		path = escaped + "/"
	}
	// The transport adds Accept-Encoding: gzip and decodes the answer itself, but only
	// while the header is left unset here.
	var listing wire.Listing
	request := call{operation: operation, method: http.MethodGet, path: path, token: token}
	err := fetch(ctx, n, request, maxListingJSON, &listing)
	return listing.Entries, err
}

// Space returns the capacity of the filesystem holding a share.
func (n *Node) Space(ctx context.Context, token, share string) (wire.Space, error) {
	const operation = "space"
	if strings.Contains(share, "/") {
		return wire.Space{}, fmt.Errorf("node %s: %s: share name %q %w", n.name, operation, share, errSlashInShare)
	}
	path, err := n.route(operation, "/v1/space/", share)
	if err != nil {
		return wire.Space{}, err
	}
	var space wire.Space
	request := call{operation: operation, method: http.MethodGet, path: path, token: token}
	err = fetch(ctx, n, request, maxSmallJSON, &space)
	return space, err
}

// Head returns the size, modification time, tag and head digest of a file, given as
// "share/a/b/name", unescaped.
func (n *Node) Head(ctx context.Context, token, file string) (Meta, error) {
	const operation = "head"
	path, err := n.route(operation, "/v1/file/", file)
	if err != nil {
		return Meta{}, err
	}
	request := call{operation: operation, method: http.MethodHead, path: path, token: token}
	resp, err := n.send(ctx, request, http.StatusOK)
	if err != nil {
		return Meta{}, err
	}
	closeBody(resp)
	return n.metaFrom(operation, resp)
}

func (n *Node) metaFrom(operation string, resp *http.Response) (Meta, error) {
	if resp.ContentLength < 0 {
		return Meta{}, n.protocolError(operation, "missing Content-Length")
	}
	mtime, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return Meta{}, n.protocolError(operation, "bad Last-Modified %q", resp.Header.Get("Last-Modified"))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return Meta{}, n.protocolError(operation, "missing ETag")
	}
	digest := resp.Header.Get(wire.HeaderHeadDigest)
	if digest == "" {
		return Meta{}, n.protocolError(operation, "missing %s", wire.HeaderHeadDigest)
	}
	return Meta{Size: resp.ContentLength, MTime: mtime.UTC(), ETag: etag, HeadDigest: digest}, nil
}

// Range reads length bytes of a file starting at off, provided the node's copy still has
// the tag etag. Blocks are never compressed. The caller must close the returned reader.
// It yields exactly length bytes and reports io.ErrUnexpectedEOF if the connection ends
// early. A node whose copy has changed answers with ErrChanged.
func (n *Node) Range(ctx context.Context, token, file, etag string, off, length int64) (io.ReadCloser, error) {
	const operation = "range"
	if err := checkRange(etag, off, length); err != nil {
		return nil, fmt.Errorf("node %s: %s: %w", n.name, operation, err)
	}
	path, err := n.route(operation, "/v1/file/", file)
	if err != nil {
		return nil, err
	}
	want := byteRange{first: off, last: off + length - 1}
	request := call{
		operation: operation,
		method:    http.MethodGet,
		path:      path,
		token:     token,
		header: http.Header{
			"Range":           {"bytes=" + strconv.FormatInt(want.first, 10) + "-" + strconv.FormatInt(want.last, 10)},
			"If-Match":        {etag},
			"Accept-Encoding": {"identity"},
		},
	}
	resp, err := n.send(ctx, request, http.StatusPartialContent)
	if err != nil {
		return nil, err
	}
	got, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || got != want {
		closeBody(resp)
		return nil, n.protocolError(operation, "Content-Range %q does not match the requested bytes %d-%d",
			resp.Header.Get("Content-Range"), want.first, want.last)
	}
	return &exactReader{body: resp.Body, remaining: length}, nil
}

func checkRange(etag string, off, length int64) error {
	switch {
	case etag == "":
		return ErrEmptyETag
	case off < 0:
		return fmt.Errorf("%w %d", ErrNegativeOffset, off)
	case length <= 0:
		return fmt.Errorf("length %d %w", length, errNotPositive)
	case off > math.MaxInt64-length:
		return fmt.Errorf("range at offset %d with length %d %w", off, length, errRangeOverflow)
	}
	return nil
}

// byteRange is the first and last byte, both included, of a Content-Range.
type byteRange struct {
	first int64
	last  int64
}

// parseContentRange extracts the first and last byte of "bytes <first>-<last>/<total>".
func parseContentRange(header string) (byteRange, bool) {
	rest, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return byteRange{}, false
	}
	span, _, ok := strings.Cut(rest, "/")
	if !ok {
		return byteRange{}, false
	}
	firstText, lastText, ok := strings.Cut(span, "-")
	if !ok {
		return byteRange{}, false
	}
	first, err := strconv.ParseInt(firstText, 10, 64)
	if err != nil {
		return byteRange{}, false
	}
	last, err := strconv.ParseInt(lastText, 10, 64)
	if err != nil {
		return byteRange{}, false
	}
	return byteRange{first: first, last: last}, true
}
