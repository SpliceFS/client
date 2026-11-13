package nfs

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
)

type fakeAddr string

func (fakeAddr) Network() string  { return "tcp" }
func (a fakeAddr) String() string { return string(a) }

// addrConn is a connection that claims to come from a given address.
type addrConn struct {
	net.Conn

	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// pipeEnds are the two ends of a connection made by pipeFrom.
type pipeEnds struct {
	server net.Conn // claims to come from the address given to pipeFrom
	client net.Conn
}

// pipeFrom returns a connection whose server side claims to come from remote.
func pipeFrom(remote string) pipeEnds {
	server, client := net.Pipe()
	return pipeEnds{server: addrConn{Conn: server, remote: fakeAddr(remote)}, client: client}
}

type accepted struct {
	conn net.Conn
	err  error
}

// scriptedListener hands out what a test queues and fails with net.ErrClosed once closed.
type scriptedListener struct {
	addr     net.Addr
	queue    chan accepted
	closed   chan struct{}
	once     sync.Once
	closeErr error // what Close returns
}

func newScriptedListener(addr string) *scriptedListener {
	return &scriptedListener{addr: fakeAddr(addr), queue: make(chan accepted, 8), closed: make(chan struct{})}
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	select {
	case a := <-l.queue:
		return a.conn, a.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *scriptedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.closeErr
}

func (l *scriptedListener) Addr() net.Addr { return l.addr }

func newTestGuard(listener net.Listener) *guardListener {
	return &guardListener{Listener: listener, log: slog.New(slog.DiscardHandler), conns: map[*trackedConn]struct{}{}}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:2049", true},
		{"127.255.0.9:1", true},
		{"[::1]:2049", true},
		{"[::ffff:127.0.0.1]:2049", true},
		{"192.0.2.1:2049", false},
		{"[2001:db8::1]:2049", false},
		{"0.0.0.0:2049", false},
		{"not an address", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isLoopback(fakeAddr(tt.addr)); got != tt.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestGuardListenerRefusesConnectionsFromOtherAddresses(t *testing.T) {
	t.Parallel()
	listener := newScriptedListener("127.0.0.1:1")
	guard := newTestGuard(listener)
	foreign := pipeFrom("192.0.2.7:4000")
	local := pipeFrom("127.0.0.1:4001")
	closeAfter(t, local.client)
	listener.queue <- accepted{conn: foreign.server}
	listener.queue <- accepted{conn: local.server}

	got, err := guard.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteAddr().String() != "127.0.0.1:4001" {
		t.Errorf("Accept returned the connection from %v", got.RemoteAddr())
	}
	// The refused connection was closed, which its peer sees as end of file.
	if _, err := foreign.client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read on the refused connection = %v, want EOF", err)
	}
}

func TestGuardListenerReturnsAcceptErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	listener := newScriptedListener("127.0.0.1:1")
	listener.queue <- accepted{err: boom}
	if _, err := newTestGuard(listener).Accept(); !errors.Is(err, boom) {
		t.Errorf("Accept = %v, want boom", err)
	}
}

func TestGuardListenerCloseClosesConnections(t *testing.T) {
	t.Parallel()
	listener := newScriptedListener("127.0.0.1:1")
	guard := newTestGuard(listener)
	local := pipeFrom("127.0.0.1:4001")
	listener.queue <- accepted{conn: local.server}
	if _, err := guard.Accept(); err != nil {
		t.Fatal(err)
	}

	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil { // idempotent: go-nfs closes it when it stops, too
		t.Fatal(err)
	}
	if _, err := local.client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read on a tracked connection after Close = %v, want EOF", err)
	}
	if _, err := guard.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
	}
}

func TestGuardListenerRefusesToTrackAfterClose(t *testing.T) {
	t.Parallel()
	guard := newTestGuard(newScriptedListener("127.0.0.1:1"))
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	local := pipeFrom("127.0.0.1:4001")
	if _, err := guard.track(local.server); !errors.Is(err, net.ErrClosed) {
		t.Errorf("track = %v, want net.ErrClosed", err)
	}
	if _, err := local.client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the connection was not closed: %v", err)
	}
}

func TestTrackedConnForgetsItselfWhenClosed(t *testing.T) {
	t.Parallel()
	guard := newTestGuard(newScriptedListener("127.0.0.1:1"))
	local := pipeFrom("127.0.0.1:4001")
	closeAfter(t, local.client)
	conn, err := guard.track(local.server)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if len(guard.conns) != 0 {
		t.Errorf("%d connections still tracked", len(guard.conns))
	}
}

func TestListenFailure(t *testing.T) {
	t.Parallel()
	if _, err := listenOn(t.Context(), "127.0.0.1:99999"); err == nil {
		t.Error("an invalid port was accepted")
	}
}

func TestListenLoopbackUsesAFreePortOnTheLoopbackInterface(t *testing.T) {
	t.Parallel()
	listener, err := listenLoopback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	closeAfter(t, listener)
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !isLoopback(tcpAddr) || tcpAddr.Port == 0 {
		t.Errorf("listening on %v", listener.Addr())
	}
}

func TestStartServerRejectsAListenerWithoutAnAddressPort(t *testing.T) {
	t.Parallel()
	listener := newScriptedListener("a pipe")
	if _, err := startServer(newFakeFS(), listener, serverConfig{}); err == nil {
		t.Fatal("no error")
	}
	select {
	case <-listener.closed:
	default:
		t.Error("the listener was left open")
	}
}

func TestServerCloseStopsEverything(t *testing.T) {
	t.Parallel()
	srv := startTest(t, newFakeFS())
	conn, err := dialTCP(t, srv.guard.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	closeAfter(t, conn)

	srv.Close()
	srv.Close() // safe to call twice
	select {
	case <-srv.Done():
	default:
		t.Fatal("Done is not closed after Close")
	}
	if srv.Err() != nil {
		t.Errorf("Err() = %v after a clean Close", srv.Err())
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("a client connection survived Close")
	}
	wantClosed(t, srv.guard.Listener)
}

func TestServerCloseLogsAListenerThatFailsToClose(t *testing.T) {
	t.Parallel()
	listener := newScriptedListener("127.0.0.1:2049")
	listener.closeErr = errors.New("boom")
	var logged bytes.Buffer
	srv, err := startServer(newFakeFS(), listener, serverConfig{logger: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if !strings.Contains(logged.String(), "close listener") || !strings.Contains(logged.String(), "boom") {
		t.Errorf("the failed close of the listener was not logged: %q", logged.String())
	}
}

func TestServerReportsAListenerFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	listener := newScriptedListener("127.0.0.1:2049")
	listener.queue <- accepted{err: boom}
	srv, err := startServer(newFakeFS(), listener, serverConfig{})
	if err != nil {
		t.Fatal(err)
	}
	<-srv.Done()
	if !errors.Is(srv.Err(), boom) {
		t.Errorf("Err() = %v, want boom", srv.Err())
	}
	srv.Close()
	if srv.Port() != 2049 {
		t.Errorf("Port() = %d", srv.Port())
	}
}
