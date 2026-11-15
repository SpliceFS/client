package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/SpliceFS/client/core"
)

const (
	// requestTimeout bounds how long a connection may take to send its request.
	requestTimeout = 5 * time.Second
	// replyTimeout bounds how long a connection may take to accept the reply.
	replyTimeout = 5 * time.Second
	// maxRequest is the largest request read. A request is a few paths.
	maxRequest = 1 << 20
	// unmountTimeout bounds an unmount asked for over the socket. A stop waits for the
	// unmounts in progress, so this is also the longest a stop can be held up by one.
	unmountTimeout = 10 * time.Second
)

// statusSource is what a Report needs from the core client.
type statusSource interface {
	Status() core.Status
}

// control serves the control socket: one request and one reply per connection.
type control struct {
	listener net.Listener
	set      *mountSet
	status   statusSource
	log      *slog.Logger
	// requests carries mount requests to the run loop, which makes every mount change.
	// Nil when this server cannot mount.
	requests chan<- mountRequest

	// cancel ends the context of the server. That context only interrupts connections that
	// have not yet sent a request.
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startControl serves listener until stop is called. The server keeps the values of parent
// but not its cancellation, because only stop ends it: the run ending must not cut off the
// reply to the unmount that ended it.
func startControl(parent context.Context, listener net.Listener, set *mountSet,
	status statusSource, log *slog.Logger, requests chan<- mountRequest,
) *control {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	server := &control{
		listener: listener, set: set, status: status, log: log, requests: requests,
		cancel: cancel,
	}
	server.wg.Go(func() { server.accept(ctx) })
	return server
}

// stop closes the listener and the connections that have not sent a request, then waits
// for the requests being served to be answered. A request being served is not cut off:
// the last unmount of a run ends the run, and its caller must still get the reply.
func (c *control) stop() {
	c.cancel()
	// The listener is the socket of Run, which reports a failed close itself.
	closeQuietly(c.listener)
	c.wg.Wait()
}

func (c *control) accept(ctx context.Context) {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				c.log.ErrorContext(ctx, "control socket stopped accepting", slog.Any("err", err))
			}
			return
		}
		c.wg.Go(func() { c.handle(ctx, conn) })
	}
}

func (c *control) handle(ctx context.Context, conn net.Conn) {
	defer closeQuietly(conn)
	reply, ok := c.serve(ctx, conn)
	if !ok {
		return
	}
	// A deadline that cannot be set means the connection is gone, and the write fails too.
	err := errors.Join(conn.SetWriteDeadline(time.Now().Add(replyTimeout)),
		json.NewEncoder(conn).Encode(reply))
	if err != nil {
		c.log.DebugContext(ctx, "control reply not delivered", slog.Any("err", err))
	}
}

// serve reads the request from conn and returns the reply. It returns false when the
// server was stopped while the request was still being read.
func (c *control) serve(ctx context.Context, conn net.Conn) (Reply, bool) {
	// Closing the connection is what wakes a read that waits for a client that says
	// nothing, when the server stops.
	stopReading := context.AfterFunc(ctx, func() { closeQuietly(conn) })
	// A deadline that cannot be set means the connection is gone, and the read fails too.
	var req Request
	err := errors.Join(conn.SetReadDeadline(time.Now().Add(requestTimeout)),
		json.NewDecoder(io.LimitReader(conn, maxRequest)).Decode(&req))
	if !stopReading() {
		return Reply{}, false
	}
	if err != nil {
		return Reply{Error: fmt.Sprintf("malformed request: %v", err)}, true
	}
	switch req.Op {
	case OpStatus:
		report := c.report()
		return Reply{Report: &report}, true
	case OpUnmount:
		return Reply{Results: c.unmount(ctx, req)}, true
	case OpMount:
		return c.mount(ctx, req.Mount), true
	}
	return Reply{Error: fmt.Sprintf("unknown op %q", req.Op)}, true
}

// mount hands the request to the run loop and waits for its answer, or for the server to be
// told to stop.
func (c *control) mount(ctx context.Context, mountReq *MountRequest) Reply {
	if mountReq == nil {
		return Reply{Error: "mount request has no mount"}
	}
	if c.requests == nil {
		return Reply{Error: "this client cannot mount on request"}
	}
	req := mountRequest{mount: mountReq.requested(), reply: make(chan error, 1)}
	select {
	case c.requests <- req:
	case <-ctx.Done():
		return Reply{Error: errStopping.Error()}
	}
	select {
	case err := <-req.reply:
		if err != nil {
			return Reply{Error: err.Error()}
		}
		return Reply{}
	case <-ctx.Done():
		return Reply{Error: errStopping.Error()}
	}
}

// unmount takes down the mounts a request names. The server being told to stop does not
// cut it short, for the reason stop gives, so it has a limit of its own.
func (c *control) unmount(ctx context.Context, req Request) []UnmountResult {
	targets := req.Targets
	if len(targets) == 0 {
		for _, live := range c.set.list() {
			targets = append(targets, live.target)
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unmountTimeout)
	defer cancel()
	results := make([]UnmountResult, 0, len(targets))
	for _, t := range targets {
		results = append(results, c.set.unmountTarget(ctx, filepath.Clean(t), req.Force))
	}
	return results
}

func (c *control) report() Report {
	status := c.status.Status()
	report := Report{
		Mounts:   c.set.report(),
		Clusters: make([]ClusterReport, 0, len(status.Clusters)),
		Caches:   make([]CacheReport, 0, len(status.Caches)),
	}
	for _, cl := range status.Clusters {
		nodes := make([]NodeReport, 0, len(cl.Nodes))
		for _, n := range cl.Nodes {
			nodes = append(nodes, NodeReport{Name: n.Name, BytesPerSecond: n.BytesPerSecond, Measured: n.Measured})
		}
		report.Clusters = append(report.Clusters, ClusterReport{Name: cl.Name, Nodes: nodes})
	}
	for _, ca := range status.Caches {
		report.Caches = append(report.Caches, CacheReport{Dir: ca.Dir, Entries: ca.Entries, Bytes: ca.Bytes})
	}
	return report
}
