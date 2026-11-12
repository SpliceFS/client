package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/SpliceFS/client/internal/session"
)

// ErrBadCredentials means a cluster rejected the configured user or password. Unlike
// ErrUnavailable it does not go away by waiting, so a program reports it and stops.
var ErrBadCredentials = errors.New("cluster rejected the user or password")

// Connect logs in to the named cluster and fetches its node list now, instead of at
// first use. It fails with ErrBadCredentials when the cluster rejects the login and
// with ErrUnavailable when no node can be reached; the session's error is kept as the
// cause. A cluster that is not configured is a plain error.
func (c *Client) Connect(ctx context.Context, cluster string) error {
	c.mu.Lock()
	cl, ok := c.clusters[cluster]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("connect: cluster %q is not configured", cluster)
	}
	_, _, err := cl.nodes(ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrBadCredentials):
		return fmt.Errorf("connect to cluster %q: %w: %w", cluster, ErrBadCredentials, err)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("connect to cluster %q: %w: %w", cluster, ErrUnavailable, err)
}
