package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/BlueRival/splicefs/internal/wire"
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
	return fetch[wire.Health](ctx, n, call{op: "health", method: http.MethodGet, path: "/v1/health"}, maxSmallJSON)
}

// Login exchanges a user name and password for a bearer token.
func (n *Node) Login(ctx context.Context, user, password string) (wire.LoginResponse, error) {
	const op = "login"
	c := call{op: op, method: http.MethodPost, path: "/v1/login", payload: wire.LoginRequest{User: user, Password: password}}
	resp, err := fetch[wire.LoginResponse](ctx, n, c, maxSmallJSON)
	if err == nil && resp.Token == "" {
		return wire.LoginResponse{}, n.protocolError(op, "response has no token")
	}
	return resp, err
}

// Nodes returns the cluster's node list as this node knows it.
func (n *Node) Nodes(ctx context.Context, token string) (wire.Nodes, error) {
	return fetch[wire.Nodes](ctx, n, call{op: "nodes", method: http.MethodGet, path: "/v1/nodes", token: token}, maxSmallJSON)
}

// List returns the entries of a directory. An empty dir lists the shares the user may
// read; otherwise dir is "share" or "share/a/b", unescaped.
func (n *Node) List(ctx context.Context, token, dir string) ([]wire.Entry, error) {
	const op = "list"
	path := "/v1/tree/"
	if dir != "" {
		escaped, err := n.route(op, path, dir)
		if err != nil {
			return nil, err
		}
		path = escaped + "/"
	}
	// The transport adds Accept-Encoding: gzip and decodes the answer itself, but only
	// while the header is left unset here.
	listing, err := fetch[wire.Listing](ctx, n, call{op: op, method: http.MethodGet, path: path, token: token}, maxListingJSON)
	return listing.Entries, err
}

// Space returns the capacity of the filesystem holding a share.
func (n *Node) Space(ctx context.Context, token, share string) (wire.Space, error) {
	const op = "space"
	if strings.Contains(share, "/") {
		return wire.Space{}, fmt.Errorf("node %s: %s: share name %q contains a slash", n.name, op, share)
	}
	path, err := n.route(op, "/v1/space/", share)
	if err != nil {
		return wire.Space{}, err
	}
	return fetch[wire.Space](ctx, n, call{op: op, method: http.MethodGet, path: path, token: token}, maxSmallJSON)
}

// Head returns the size, modification time, tag and head digest of a file, given as
// "share/a/b/name", unescaped.
func (n *Node) Head(ctx context.Context, token, file string) (Meta, error) {
	const op = "head"
	path, err := n.route(op, "/v1/file/", file)
	if err != nil {
		return Meta{}, err
	}
	resp, err := n.send(ctx, call{op: op, method: http.MethodHead, path: path, token: token}, http.StatusOK)
	if err != nil {
		return Meta{}, err
	}
	closeBody(resp)
	return n.metaFrom(op, resp)
}

func (n *Node) metaFrom(op string, resp *http.Response) (Meta, error) {
	if resp.ContentLength < 0 {
		return Meta{}, n.protocolError(op, "missing Content-Length")
	}
	mtime, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return Meta{}, n.protocolError(op, "bad Last-Modified %q", resp.Header.Get("Last-Modified"))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return Meta{}, n.protocolError(op, "missing ETag")
	}
	digest := resp.Header.Get(wire.HeaderHeadDigest)
	if digest == "" {
		return Meta{}, n.protocolError(op, "missing %s", wire.HeaderHeadDigest)
	}
	return Meta{Size: resp.ContentLength, MTime: mtime.UTC(), ETag: etag, HeadDigest: digest}, nil
}

// Range reads length bytes of a file starting at off, provided the node's copy still has
// the tag etag. Blocks are never compressed. The caller must close the returned reader.
// It yields exactly length bytes and reports io.ErrUnexpectedEOF if the connection ends
// early. A node whose copy has changed answers with ErrChanged.
func (n *Node) Range(ctx context.Context, token, file, etag string, off, length int64) (io.ReadCloser, error) {
	const op = "range"
	if err := checkRange(etag, off, length); err != nil {
		return nil, fmt.Errorf("node %s: %s: %w", n.name, op, err)
	}
	path, err := n.route(op, "/v1/file/", file)
	if err != nil {
		return nil, err
	}
	last := off + length - 1
	c := call{
		op:     op,
		method: http.MethodGet,
		path:   path,
		token:  token,
		header: http.Header{
			"Range":           {"bytes=" + strconv.FormatInt(off, 10) + "-" + strconv.FormatInt(last, 10)},
			"If-Match":        {etag},
			"Accept-Encoding": {"identity"},
		},
	}
	resp, err := n.send(ctx, c, http.StatusPartialContent)
	if err != nil {
		return nil, err
	}
	start, end, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || start != off || end != last {
		closeBody(resp)
		return nil, n.protocolError(op, "Content-Range %q does not match the requested bytes %d-%d", resp.Header.Get("Content-Range"), off, last)
	}
	return &exactReader{body: resp.Body, remaining: length}, nil
}

func checkRange(etag string, off, length int64) error {
	switch {
	case etag == "":
		return errors.New("empty etag: a block must be pinned to a file version")
	case off < 0:
		return fmt.Errorf("negative offset %d", off)
	case length <= 0:
		return fmt.Errorf("length %d is not positive", length)
	case off > math.MaxInt64-length:
		return fmt.Errorf("range at offset %d with length %d overflows", off, length)
	}
	return nil
}

// parseContentRange extracts the first and last byte of "bytes <first>-<last>/<total>".
func parseContentRange(h string) (first, last int64, ok bool) {
	rest, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, 0, false
	}
	span, _, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, false
	}
	first, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	last, err = strconv.ParseInt(b, 10, 64)
	return first, last, err == nil
}
