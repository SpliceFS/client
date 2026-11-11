package nfs

import (
	"errors"
	"io"
	"log/slog"
	"net"
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

// pipeFrom returns a server-side connection that claims to come from remote, and the
// client side of it.
func pipeFrom(remote string) (net.Conn, net.Conn) {
	server, client := net.Pipe()
	return addrConn{Conn: server, remote: fakeAddr(remote)}, client
}

type accepted struct {
	conn net.Conn
	err  error
}

// scriptedListener hands out what a test queues and fails with net.ErrClosed once closed.
type scriptedListener struct {
	addr   net.Addr
	queue  chan accepted
	closed chan struct{}
	once   sync.Once
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
	return nil
}

func (l *scriptedListener) Addr() net.Addr { return l.addr }

func newTestGuard(ln net.Listener) *guardListener {
	return &guardListener{Listener: ln, log: slog.New(slog.DiscardHandler), conns: map[*trackedConn]struct{}{}}
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
	ln := newScriptedListener("127.0.0.1:1")
	g := newTestGuard(ln)
	foreign, foreignPeer := pipeFrom("192.0.2.7:4000")
	local, localPeer := pipeFrom("127.0.0.1:4001")
	closeAfter(t, localPeer)
	ln.queue <- accepted{conn: foreign}
	ln.queue <- accepted{conn: local}

	got, err := g.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteAddr().String() != "127.0.0.1:4001" {
		t.Errorf("Accept returned the connection from %v", got.RemoteAddr())
	}
	// The refused connection was closed, which its peer sees as end of file.
	if _, err := foreignPeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read on the refused connection = %v, want EOF", err)
	}
}

func TestGuardListenerReturnsAcceptErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	ln := newScriptedListener("127.0.0.1:1")
	ln.queue <- accepted{err: boom}
	if _, err := newTestGuard(ln).Accept(); !errors.Is(err, boom) {
		t.Errorf("Accept = %v, want boom", err)
	}
}

func TestGuardListenerCloseClosesConnections(t *testing.T) {
	t.Parallel()
	ln := newScriptedListener("127.0.0.1:1")
	g := newTestGuard(ln)
	local, peer := pipeFrom("127.0.0.1:4001")
	ln.queue <- accepted{conn: local}
	if _, err := g.Accept(); err != nil {
		t.Fatal(err)
	}

	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil { // idempotent: go-nfs closes it when it stops, too
		t.Fatal(err)
	}
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read on a tracked connection after Close = %v, want EOF", err)
	}
	if _, err := g.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
	}
}

func TestGuardListenerRefusesToTrackAfterClose(t *testing.T) {
	t.Parallel()
	g := newTestGuard(newScriptedListener("127.0.0.1:1"))
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	local, peer := pipeFrom("127.0.0.1:4001")
	if _, err := g.track(local); !errors.Is(err, net.ErrClosed) {
		t.Errorf("track = %v, want net.ErrClosed", err)
	}
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the connection was not closed: %v", err)
	}
}

func TestTrackedConnForgetsItselfWhenClosed(t *testing.T) {
	t.Parallel()
	g := newTestGuard(newScriptedListener("127.0.0.1:1"))
	local, peer := pipeFrom("127.0.0.1:4001")
	closeAfter(t, peer)
	conn, err := g.track(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.conns) != 0 {
		t.Errorf("%d connections still tracked", len(g.conns))
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
	ln, err := listenLoopback(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	closeAfter(t, ln)
	if !isLoopback(ln.Addr()) || ln.Addr().(*net.TCPAddr).Port == 0 {
		t.Errorf("listening on %v", ln.Addr())
	}
}

func TestStartServerRejectsAListenerWithoutAnAddressPort(t *testing.T) {
	t.Parallel()
	ln := newScriptedListener("a pipe")
	if _, err := startServer(newFakeFS(), ln, serverConfig{}); err == nil {
		t.Fatal("no error")
	}
	select {
	case <-ln.closed:
	default:
		t.Error("the listener was left open")
	}
}

func TestServerCloseStopsEverything(t *testing.T) {
	t.Parallel()
	s := startTest(t, newFakeFS())
	conn, err := dialTCP(t, s.guard.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	closeAfter(t, conn)

	s.Close()
	s.Close() // safe to call twice
	select {
	case <-s.Done():
	default:
		t.Fatal("Done is not closed after Close")
	}
	if s.Err() != nil {
		t.Errorf("Err() = %v after a clean Close", s.Err())
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("a client connection survived Close")
	}
	wantClosed(t, s.guard.Listener)
}

func TestServerReportsAListenerFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	ln := newScriptedListener("127.0.0.1:2049")
	ln.queue <- accepted{err: boom}
	s, err := startServer(newFakeFS(), ln, serverConfig{})
	if err != nil {
		t.Fatal(err)
	}
	<-s.Done()
	if !errors.Is(s.Err(), boom) {
		t.Errorf("Err() = %v, want boom", s.Err())
	}
	s.Close()
	if s.Port() != 2049 {
		t.Errorf("Port() = %d", s.Port())
	}
}
