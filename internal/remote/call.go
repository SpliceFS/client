package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	op     string
	method string
	path   string
	token  string
	header http.Header
	// payload, when not nil, is sent as the JSON body of the request.
	payload any
}

// send performs the call and returns the response only if its status is want. Any other
// outcome is an error and the body is already consumed.
func (n *Node) send(ctx context.Context, c call, want int) (*http.Response, error) {
	body, err := encodePayload(c.payload)
	if err != nil {
		return nil, fmt.Errorf("node %s: %s: encode request: %w", n.name, c.op, err)
	}
	req, err := http.NewRequestWithContext(ctx, c.method, n.base+c.path, body)
	if err != nil {
		return nil, fmt.Errorf("node %s: %s: build request: %w", n.name, c.op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "splicefs/"+version.String())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, n.transportError(c.op, ctx.Err(), err)
	}
	if resp.StatusCode != want {
		return nil, n.statusError(c.op, resp)
	}
	return resp, nil
}

// fetch performs a call that is answered with a JSON body and decodes it into a T.
func fetch[T any](ctx context.Context, n *Node, c call, limit int64) (T, error) {
	var v T
	resp, err := n.send(ctx, c, http.StatusOK)
	if err != nil {
		return v, err
	}
	defer closeBody(resp)
	if !isJSON(resp.Header.Get("Content-Type")) {
		return v, n.protocolError(c.op, "content type %q is not application/json", resp.Header.Get("Content-Type"))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return v, n.transportError(c.op, ctx.Err(), err)
	}
	if int64(len(data)) > limit {
		return v, n.protocolError(c.op, "body is larger than %d bytes", limit)
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, n.protocolError(c.op, "undecodable JSON: %v", err)
	}
	return v, nil
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

// closeBody reads what is left of a small body before closing it, because the HTTP
// transport only reuses a connection whose body was read to the end.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}

// encodePayload returns nil for a call without a body. It must return an untyped nil
// io.Reader in that case: a typed nil would make the request claim a body it does not have.
func encodePayload(payload any) (io.Reader, error) {
	if payload == nil {
		return nil, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}
