package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/SpliceFS/common/wire"
)

// Sentinel errors for the ways a node can answer. They are wrapped with the node name and
// the operation and are meant to be checked with errors.Is.
var (
	// ErrUnauthorized means the token is missing, invalid or expired (HTTP 401).
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden means the user may not read the share (HTTP 403).
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound means the node does not have the path (HTTP 404).
	ErrNotFound = errors.New("not found")
	// ErrChanged means the node's copy of the file no longer has the requested tag
	// (HTTP 412).
	ErrChanged = errors.New("file changed on node")
	// ErrBadRange means the node rejected the byte range (HTTP 416).
	ErrBadRange = errors.New("range not satisfiable")
	// ErrRateLimited means the node is limiting requests from this client (HTTP 429).
	ErrRateLimited = errors.New("rate limited")
	// ErrProtocol means the node sent a response that violates the API: an unexpected
	// status below 500, a wrong content type, undecodable JSON or a missing header.
	ErrProtocol = errors.New("protocol violation")
	// ErrUnavailable means the node could not be reached or failed: a transport error,
	// a timeout that is not the caller's own context ending, or any 5xx status.
	ErrUnavailable = errors.New("node unavailable")
)

// Errors for a request this package refuses to send. They are wrapped with the node name and
// the operation, and mean the caller passed an argument that no node could answer.
var (
	// ErrNilClient means New was given no HTTP client.
	ErrNilClient = errors.New("remote: nil http client")
	// ErrInvalidPath means a path has an empty, "." or ".." segment.
	ErrInvalidPath = errors.New("invalid path")
	// ErrEmptyETag means a block was requested without the tag of the file version it belongs to.
	ErrEmptyETag = errors.New("empty etag: a block must be pinned to a file version")
	// ErrNegativeOffset means a block was requested at a negative offset.
	ErrNegativeOffset = errors.New("negative offset")
)

// The errors below end a message that names the offending value first, such as
// `length 0 is not positive`, so their own text is only the tail of that sentence.
var (
	errSlashInShare   = errors.New("contains a slash")
	errNotPositive    = errors.New("is not positive")
	errRangeOverflow  = errors.New("overflows")
	errNotAbsoluteURL = errors.New("is not an absolute http or https URL")
	errQueryOrFragURL = errors.New("must not have a query or fragment")
)

// maxServerMessage bounds how much of the server's error message goes into an error text.
const maxServerMessage = 256

func sentinelFor(status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusForbidden:
		return ErrForbidden
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusPreconditionFailed:
		return ErrChanged
	case status == http.StatusRequestedRangeNotSatisfiable:
		return ErrBadRange
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= http.StatusInternalServerError:
		return ErrUnavailable
	default:
		return ErrProtocol
	}
}

// statusError turns an unexpected response into an error and consumes the response.
func (n *Node) statusError(operation string, resp *http.Response) error {
	defer closeBody(resp)
	err := fmt.Errorf("node %s: %s: %w (HTTP %d)", n.name, operation, sentinelFor(resp.StatusCode), resp.StatusCode)
	if msg := serverMessage(resp.Body); msg != "" {
		return fmt.Errorf("%w: %q", err, msg)
	}
	return err
}

// serverMessage returns the message of a wire.Error body, or "" when the body has none.
// It is best effort: an error response must never turn into a second failure.
func serverMessage(body io.Reader) string {
	var e wire.Error
	if err := json.NewDecoder(io.LimitReader(body, maxSmallJSON)).Decode(&e); err != nil {
		return ""
	}
	msg := e.Error
	if len(msg) > maxServerMessage {
		msg = msg[:maxServerMessage]
		// Cutting at a byte count can split a multi-byte character.
		for !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	return strings.TrimSpace(msg)
}

// transportError classifies a failure to complete an exchange. If the caller's context
// has ended it wins, whatever the transport reported.
func (n *Node) transportError(operation string, cerr, err error) error {
	if cerr != nil {
		return fmt.Errorf("node %s: %s: %w", n.name, operation, cerr)
	}
	// The cause is formatted with %v on purpose: wrapping it could make a client-side
	// timeout satisfy errors.Is(err, context.DeadlineExceeded), which is reserved for the
	// caller's own deadline.
	return fmt.Errorf("node %s: %s: %w: %v", n.name, operation, ErrUnavailable, err.Error())
}

func (n *Node) protocolError(operation, format string, args ...any) error {
	return fmt.Errorf("node %s: %s: %w: %s", n.name, operation, ErrProtocol, fmt.Sprintf(format, args...))
}
