package remote

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Node is a client for one server node. The zero value is not usable; create it with New.
type Node struct {
	name   string
	base   string
	client *http.Client
}

// New returns a client for the node called name whose API lives at baseURL, for example
// "https://east.example.com/api". The URL must be absolute with an http or https scheme.
// All requests go through client, which the caller configures (TLS, timeouts, proxies).
// Leave the transport's compression enabled: listings rely on it for transparent gzip.
func New(name, baseURL string, client *http.Client) (*Node, error) {
	if client == nil {
		return nil, errors.New("remote: nil http client")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("remote: node %s: parse base URL: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("remote: node %s: base URL %q is not an absolute http or https URL", name, baseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("remote: node %s: base URL %q must not have a query or fragment", name, baseURL)
	}
	return &Node{name: name, base: strings.TrimRight(baseURL, "/"), client: client}, nil
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
			return "", fmt.Errorf("invalid path %q: empty, \".\" or \"..\" segment", p)
		}
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/"), nil
}

// route builds the request path for a call below prefix, with p escaped.
func (n *Node) route(op, prefix, p string) (string, error) {
	escaped, err := escapePath(p)
	if err != nil {
		return "", fmt.Errorf("node %s: %s: %w", n.name, op, err)
	}
	return prefix + escaped, nil
}
