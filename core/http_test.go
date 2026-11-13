package core

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestHTTPClientTransport(t *testing.T) {
	t.Parallel()
	httpClient := newHTTPClient(6, systemDialer())
	if httpClient.Timeout != 0 {
		t.Errorf("client timeout %v, want none", httpClient.Timeout)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", httpClient.Transport)
	}
	if transport.Protocols == nil || !transport.Protocols.HTTP1() || transport.Protocols.HTTP2() {
		t.Errorf("protocols %v, want HTTP/1.1 only", transport.Protocols)
	}
	if transport.MaxConnsPerHost != 6+connHeadroom || transport.MaxIdleConnsPerHost != 6+connHeadroom {
		t.Errorf("connections per host %d, idle %d; want %d",
			transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost, 6+connHeadroom)
	}
	if transport.ResponseHeaderTimeout != responseHeaderTimeout || transport.IdleConnTimeout != idleConnTimeout {
		t.Errorf("timeouts: response header %v, idle %v", transport.ResponseHeaderTimeout, transport.IdleConnTimeout)
	}
}

// fakeConn is a connection that only carries the address it was dialed with.
type fakeConn struct {
	net.Conn

	addr string
}

func TestDialTriesAddressesInShuffledOrder(t *testing.T) {
	t.Parallel()
	refused := errors.New("connection refused")
	var dialed []string
	dialer := addrDialer{
		lookup: func(_ context.Context, host string) ([]string, error) {
			if host != "files.example.com" {
				t.Errorf("looked up %q", host)
			}
			return []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, nil
		},
		shuffle: slices.Reverse[[]string],
		dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			if addr != "192.0.2.1:443" {
				return nil, refused
			}
			return fakeConn{addr: addr}, nil
		},
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "files.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if fake, ok := conn.(fakeConn); !ok || fake.addr != "192.0.2.1:443" {
		t.Errorf("connected to %v", conn)
	}
	if want := []string{"192.0.2.3:443", "192.0.2.2:443", "192.0.2.1:443"}; !slices.Equal(dialed, want) {
		t.Errorf("dialed %v, want %v", dialed, want)
	}
}

func TestDialFailures(t *testing.T) {
	t.Parallel()
	lookupFailed := errors.New("no such host")
	refused := errors.New("connection refused")
	addrs := func(list ...string) func(context.Context, string) ([]string, error) {
		return func(context.Context, string) ([]string, error) { return list, nil }
	}
	lookupError := func(context.Context, string) ([]string, error) { return nil, lookupFailed }
	for _, testCase := range []struct {
		name   string
		addr   string
		lookup func(context.Context, string) ([]string, error)
		want   string
		cause  error
	}{
		{"no port", "files.example.com", addrs("192.0.2.1"), "missing port", nil},
		{"lookup fails", "files.example.com:443", lookupError, "no such host", lookupFailed},
		{"no addresses", "files.example.com:443", addrs(), "has no addresses", nil},
		{"every address refuses", "files.example.com:443", addrs("192.0.2.1", "192.0.2.2"), "connection refused", refused},
	} {
		dialer := addrDialer{
			lookup:  testCase.lookup,
			shuffle: func([]string) {},
			dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, refused
			},
		}
		_, err := dialer.DialContext(context.Background(), "tcp", testCase.addr)
		wrongCause := testCase.cause != nil && !errors.Is(err, testCase.cause)
		if err == nil || !strings.Contains(err.Error(), testCase.want) || wrongCause {
			t.Errorf("%s: DialContext = %v, want an error containing %q", testCase.name, err, testCase.want)
		}
	}
}

func TestShuffleKeepsTheAddresses(t *testing.T) {
	t.Parallel()
	addrs := []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"}
	shuffled := slices.Clone(addrs)
	shuffle(shuffled)
	slices.Sort(shuffled)
	if !slices.Equal(shuffled, addrs) {
		t.Errorf("shuffle changed the addresses: %v", shuffled)
	}
}
