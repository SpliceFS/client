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

	// ctx ends when the server is told to stop; it only interrupts connections that
	// have not yet sent a request.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startControl serves ln until stop is called.
func startControl(ln net.Listener, set *mountSet, status statusSource, log *slog.Logger) *control {
	ctx, cancel := context.WithCancel(context.Background())
	c := &control{listener: ln, set: set, status: status, log: log, ctx: ctx, cancel: cancel}
	c.wg.Go(c.accept)
	return c
}

// stop closes the listener and the connections that have not sent a request, then waits
// for the requests being served to be answered. A request being served is not cut off:
// the last unmount of a run ends the run, and its caller must still get the reply.
func (c *control) stop() {
	c.cancel()
	_ = c.listener.Close()
	c.wg.Wait()
}

func (c *control) accept() {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				c.log.Error("control socket stopped accepting", "err", err)
			}
			return
		}
		c.wg.Go(func() { c.handle(conn) })
	}
}

func (c *control) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reply, ok := c.serve(conn)
	if !ok {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(replyTimeout))
	if err := json.NewEncoder(conn).Encode(reply); err != nil {
		c.log.Debug("control reply not delivered", "err", err)
	}
}

// serve reads the request from conn and returns the reply. It returns false when the
// server was stopped while the request was still being read.
func (c *control) serve(conn net.Conn) (Reply, bool) {
	// Closing the connection is what wakes a read that waits for a client that says
	// nothing, when the server stops.
	stopReading := context.AfterFunc(c.ctx, func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	var req Request
	err := json.NewDecoder(io.LimitReader(conn, maxRequest)).Decode(&req)
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
		return Reply{Results: c.unmount(req)}, true
	}
	return Reply{Error: fmt.Sprintf("unknown op %q", req.Op)}, true
}

func (c *control) unmount(req Request) []UnmountResult {
	targets := req.Targets
	if len(targets) == 0 {
		for _, m := range c.set.list() {
			targets = append(targets, m.target)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), unmountTimeout)
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
