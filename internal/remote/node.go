package remote

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SpliceFS/client/internal/version"
)

// Node is a client for one server node. The zero value is not usable; create it with New.
type Node struct {
	name string
	base string
	// host is the host name and port of base, which is how an incompatible server is named
	// to a person.
	host   string
	client *http.Client
	// clientVersion is the version this client is compared with the server's. It is a field
	// so that a test can choose it without a build flag.
	clientVersion string
}

// New returns a client for the node called name whose API lives at baseURL, for example
// "https://east.example.com/api". The URL must be absolute with an http or https scheme.
// All requests go through client, which the caller configures (TLS, timeouts, proxies).
// Leave the transport's compression enabled: listings rely on it for transparent gzip.
func New(name, baseURL string, client *http.Client) (*Node, error) {
	if client == nil {
		return nil, ErrNilClient
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("remote: node %s: parse base URL: %w", name, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("remote: node %s: base URL %q %w", name, baseURL, errNotAbsoluteURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("remote: node %s: base URL %q %w", name, baseURL, errQueryOrFragURL)
	}
	return &Node{
		name:          name,
		base:          strings.TrimRight(baseURL, "/"),
		host:          parsed.Host,
		client:        client,
		clientVersion: version.String(),
	}, nil
}

// Name returns the node's name as given to New.
func (n *Node) Name() string {
	return n.name
}

// escapePath validates a slash-separated, unescaped path and escapes each segment for use
// in a URL. Empty, "." and ".." segments are rejected so that a name chosen by a remote
// party can never be turned into a different request path by the server's cleaning.
func escapePath(p string) (string, error) {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("%w %q: empty, \".\" or \"..\" segment", ErrInvalidPath, p)
		}
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/"), nil
}

// route builds the request path for a call below prefix, with p escaped.
func (n *Node) route(operation, prefix, p string) (string, error) {
	escaped, err := escapePath(p)
	if err != nil {
		return "", fmt.Errorf("node %s: %s: %w", n.name, operation, err)
	}
	return prefix + escaped, nil
}
