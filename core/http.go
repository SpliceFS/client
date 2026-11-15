package core

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// errNoAddresses ends the message "dial <address>: <host> has no addresses".
var errNoAddresses = errors.New("has no addresses")

const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
	idleConnTimeout       = 90 * time.Second
	// connHeadroom is the connections per node beyond the block streams: one for the
	// hedge fetch a reader may add, the rest for listings, HEAD and space requests,
	// which must not queue behind long block reads.
	connHeadroom = 4
)

// newHTTPClient returns the client shared by every node of every cluster. It has no
// overall timeout on purpose: a block read may take long on a slow link, and every
// request is bounded by its context instead.
func newHTTPClient(streamsPerNode int, dialer addrDialer) *http.Client {
	// HTTP/1.1 only: block requests in flight to one node then travel over separate
	// connections, so one lost packet stalls one block. Over HTTP/2 they would share
	// one stream-multiplexed connection, and a loss would stall all of them.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	perNode := streamsPerNode + connHeadroom
	return &http.Client{Transport: &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: dialer.DialContext,
		Protocols:   &protocols,
		// Every stream of every open file wants its own connection; idle ones are kept
		// so that the next block does not pay for a new TCP and TLS handshake. Requests
		// beyond the limit wait for a connection rather than opening ever more.
		MaxIdleConnsPerHost: perNode,
		MaxConnsPerHost:     perNode,
		TLSHandshakeTimeout: tlsHandshakeTimeout,
		// A node that accepts a request but does not start answering is as good as
		// down; this frees the stream for another node long before a block timeout.
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleConnTimeout,
	}}
}

// addrDialer connects to a host name by trying its addresses in random order, so that
// an entry name with several addresses spreads clients over the nodes behind it and a
// dead address does not stop the client from reaching the others.
type addrDialer struct {
	lookup  func(ctx context.Context, host string) ([]string, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	shuffle func(addrs []string)
}

func systemDialer() addrDialer {
	d := &net.Dialer{Timeout: dialTimeout}
	return addrDialer{lookup: net.DefaultResolver.LookupHost, dial: d.DialContext, shuffle: shuffle}
}

//nolint:gosec // The order spreads load over addresses; predicting it gains an attacker nothing.
func shuffle(addrs []string) {
	rand.Shuffle(len(addrs), func(i, j int) {
		addrs[i], addrs[j] = addrs[j], addrs[i]
	})
}

// DialContext has the signature of http.Transport.DialContext.
func (d addrDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	addrs, err := d.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("dial %s: %s %w", addr, host, errNoAddresses)
	}
	d.shuffle(addrs)
	errs := make([]error, 0, len(addrs))
	for _, a := range addrs {
		conn, err := d.dial(ctx, network, net.JoinHostPort(a, port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("dial %s: %w", addr, errors.Join(errs...))
}
