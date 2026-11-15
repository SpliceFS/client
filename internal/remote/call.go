package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"

	"github.com/SpliceFS/client/internal/version"
)

const (
	// maxListingJSON bounds a directory listing, which can legitimately be large.
	maxListingJSON = 16 << 20
	// maxSmallJSON bounds every other JSON body.
	maxSmallJSON = 64 << 10
	// drainLimit is how much of an unread body is consumed so the connection can be
	// reused; beyond that, closing the connection is cheaper than reading on.
	drainLimit = 4 << 10
)

// call describes one request. path is already escaped and relative to the base URL.
type call struct {
	operation string
	method    string
	path      string
	token     string
	header    http.Header
	// payload, when not nil, is sent as the JSON body of the request.
	payload any
}

// send performs the call and returns the response only if its status is want and the server
// is a version this client works with. Any other outcome is an error and the body is already
// consumed.
func (n *Node) send(ctx context.Context, request call, want int) (*http.Response, error) {
	httpReq, err := n.newRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	resp, err := n.client.Do(httpReq)
	if err != nil {
		return nil, n.transportError(request.operation, ctx.Err(), err)
	}
	reported := resp.Header.Values(serverVersionHeader)
	if err = checkVersion(n.host, n.clientVersion, reported, resp.StatusCode, want); err != nil {
		closeBody(resp)
		return nil, err
	}
	if resp.StatusCode != want {
		return nil, n.statusError(request.operation, resp)
	}
	return resp, nil
}

// newRequest builds the HTTP request for a call: its JSON body, the caller's headers, the
// user agent and the bearer token.
func (n *Node) newRequest(ctx context.Context, request call) (*http.Request, error) {
	// The body stays an untyped nil io.Reader for a call without a payload: a typed nil
	// would make the request claim a body it does not have.
	var body io.Reader
	if request.payload != nil {
		data, err := json.Marshal(request.payload)
		if err != nil {
			return nil, fmt.Errorf("node %s: %s: encode request: %w", n.name, request.operation, err)
		}
		body = bytes.NewReader(data)
	}
	httpReq, err := http.NewRequestWithContext(ctx, request.method, n.base+request.path, body)
	if err != nil {
		return nil, fmt.Errorf("node %s: %s: build request: %w", n.name, request.operation, err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	maps.Copy(httpReq.Header, request.header)
	httpReq.Header.Set("User-Agent", "splicefs/"+version.String())
	if request.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+request.token)
	}
	return httpReq, nil
}

// fetch performs a call that is answered with a JSON body and decodes it into dst. The
// target is passed in, not returned, so that fetch is generic only over what it decodes.
func fetch[T any](ctx context.Context, n *Node, request call, limit int64, dst *T) error {
	resp, err := n.send(ctx, request, http.StatusOK)
	if err != nil {
		return err
	}
	defer closeBody(resp)
	if !isJSON(resp.Header.Get("Content-Type")) {
		return n.protocolError(request.operation, "content type %q is not application/json",
			resp.Header.Get("Content-Type"))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n.transportError(request.operation, ctx.Err(), err)
	}
	if int64(len(data)) > limit {
		return n.protocolError(request.operation, "body is larger than %d bytes", limit)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return n.protocolError(request.operation, "undecodable JSON: %v", err)
	}
	return nil
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

// closeBody reads what is left of a small body before closing it, because the HTTP
// transport only reuses a connection whose body was read to the end.
func closeBody(resp *http.Response) {
	//nolint:errcheck // a failed drain only makes the transport close the connection instead of reusing it
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	//nolint:errcheck // the body was only read, so there is nothing a failed close could lose
	_ = resp.Body.Close()
}
