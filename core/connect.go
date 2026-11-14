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

// ErrIncompatible means a cluster runs a splicefsd that this client refuses because of its
// version. Like ErrBadCredentials it does not go away by waiting, so a program reports it
// and stops. The text of the error says which versions are involved.
var ErrIncompatible = errors.New("cluster server version does not fit this client")

// Connect logs in to the named cluster and fetches its node list now, instead of at
// first use. It fails with ErrBadCredentials when the cluster rejects the login and
// with ErrIncompatible when its splicefsd is too old or of another main version, and
// with ErrUnavailable when no node can be reached; the session's error is kept as the
// cause. A cluster that is not configured is a plain error.
func (c *Client) Connect(ctx context.Context, cluster string) error {
	c.mu.Lock()
	clus, ok := c.clusters[cluster]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("connect: cluster %q %w", cluster, errNotConfigured)
	}
	_, err := clus.nodes(ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrBadCredentials):
		return fmt.Errorf("connect to cluster %q: %w: %w", cluster, ErrBadCredentials, err)
	case errors.Is(err, session.ErrIncompatible):
		return fmt.Errorf("connect to cluster %q: %w: %w", cluster, ErrIncompatible, err)
	case ctx.Err() != nil:
		// The caller's own context ended. It is passed on as it is, so that an interrupted call is
		// not mistaken for an outage, and the caller says what it was doing.
		return ctx.Err() //nolint:wrapcheck // See above, a prefix would change the message.
	}
	return fmt.Errorf("connect to cluster %q: %w: %w", cluster, ErrUnavailable, err)
}
