package nfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	gonfs "github.com/willscott/go-nfs"

	"github.com/SpliceFS/client/core"
)

// defaultCallTimeout is how long one call into the core.FS may take. It is long enough
// for a cold read from a distant node and short enough that a program is not stuck
// forever on a call nobody answers.
const defaultCallTimeout = 60 * time.Second

// serverConfig holds what a server needs besides the filesystem and the listener.
type serverConfig struct {
	owner       owner
	callTimeout time.Duration
	rootBudget  time.Duration // zero means rootAttrBudget
	logger      *slog.Logger
}

// server is a running read-only NFSv3 server for one core.FS. It serves the NFS and
// the mount protocol on the same port, so a client mounts with port and mountport set
// and needs no portmapper.
type server struct {
	view    *billyFS
	guard   *guardListener
	cancel  context.CancelFunc
	port    int
	closing atomic.Bool
	done    chan struct{}
	err     error
}

// listenLoopback listens on a free port of the IPv4 loopback address.
func listenLoopback(ctx context.Context) (net.Listener, error) {
	return listenOn(ctx, "127.0.0.1:0")
}

func listenOn(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// startServer serves fsys on listener until Close is called or the listener fails. It
// takes over listener: Close closes it.
func startServer(fsys core.FS, listener net.Listener, cfg serverConfig) (*server, error) {
	addr, err := netip.ParseAddrPort(listener.Addr().String())
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listener address %q: %w", listener.Addr(), err), listener.Close())
	}
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.DiscardHandler)
	}
	if cfg.callTimeout <= 0 {
		cfg.callTimeout = defaultCallTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	billyView := newBillyFS(ctx, newNormalizingFS(fsys), cfg.callTimeout, cfg.owner, cfg.logger)
	if cfg.rootBudget > 0 {
		billyView.rootBudget = cfg.rootBudget
	}
	srv := &server{
		view:   billyView,
		guard:  &guardListener{Listener: listener, log: cfg.logger, conns: make(map[*trackedConn]struct{})},
		cancel: cancel,
		port:   int(addr.Port()),
		done:   make(chan struct{}),
	}
	nfsServer := &gonfs.Server{Handler: newHandler(billyView, handleCacheSize, verifierCacheSize), Context: ctx}
	go srv.run(nfsServer)
	return srv, nil
}

// Port is the loopback port the server listens on.
func (s *server) Port() int { return s.port }

// Done is closed once the server has stopped and its connections are closed.
func (s *server) Done() <-chan struct{} { return s.done }

// Err is why the server stopped: nil after Close, otherwise the failure. It must only
// be called after Done is closed.
func (s *server) Err() error { return s.err }

// Close stops the server, cancels the calls in flight, closes every connection and the
// files kept open for reading. It returns when the accept loop has ended. The go-nfs goroutine of each connection ends
// as soon as its connection is closed. Close is safe to call more than once.
func (s *server) Close() {
	s.closing.Store(true)
	s.cancel()
	if err := s.guard.Close(); err != nil {
		s.guard.log.Warn("close listener", slog.Any("err", err))
	}
	<-s.done
	s.view.close()
}

func (s *server) run(nfsServer *gonfs.Server) {
	defer close(s.done)
	err := nfsServer.Serve(s.guard)
	if !s.closing.Load() {
		s.err = fmt.Errorf("serve nfs: %w", err)
	}
}

// guardListener only hands out connections that come from the loopback interface, and
// remembers them so they can be closed with the listener.
//
// The listener is bound to 127.0.0.1, so the source check can only matter if that ever
// changes: a different bind address, a mistake in a later edit, or a platform that
// delivers other traffic to the socket. It is kept because this server has no
// authentication at all, and the check is the difference between "the Mac can read
// the share" and "the network can read the share". It does not tell local users apart:
// any local process can connect.
type guardListener struct {
	net.Listener

	log *slog.Logger

	mu     sync.Mutex
	closed bool
	conns  map[*trackedConn]struct{}
}

// Accept returns the next connection from the loopback interface and closes any other.
func (g *guardListener) Accept() (net.Conn, error) {
	for {
		conn, err := g.Listener.Accept()
		if err != nil {
			//nolint:wrapcheck // go-nfs asserts net.Error on this error to retry after a timeout; a wrapper would hide it.
			return nil, err
		}
		if !isLoopback(conn.RemoteAddr()) {
			g.log.Warn("refused connection from a non-loopback address",
				slog.String("remote", conn.RemoteAddr().String()))
			discardClose(conn)
			continue
		}
		return g.track(conn)
	}
}

// Close closes the listener and every connection handed out. go-nfs calls it when its
// accept loop ends, so a failing listener cannot leave connections behind.
func (g *guardListener) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	conns := slices.Collect(maps.Keys(g.conns))
	g.mu.Unlock()

	err := g.Listener.Close()
	for _, conn := range conns {
		discardClose(conn)
	}
	if err != nil {
		return fmt.Errorf("close listener: %w", err)
	}
	return nil
}

func (g *guardListener) track(conn net.Conn) (net.Conn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		discardClose(conn)
		return nil, net.ErrClosed
	}
	tracked := &trackedConn{Conn: conn, owner: g}
	g.conns[tracked] = struct{}{}
	return tracked, nil
}

func (g *guardListener) forget(conn *trackedConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.conns, conn)
}

// trackedConn removes itself from its listener when closed.
type trackedConn struct {
	net.Conn

	owner *guardListener
}

func (c *trackedConn) Close() error {
	c.owner.forget(c)
	if err := c.Conn.Close(); err != nil {
		return fmt.Errorf("close connection: %w", err)
	}
	return nil
}

// discardClose closes a connection that is being dropped. A failure to close it cannot
// be acted on: the peer may have closed it first, and the connection is gone either way.
func discardClose(conn io.Closer) {
	_ = conn.Close() //nolint:errcheck // The reason is in the comment of this function.
}

func isLoopback(addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}
