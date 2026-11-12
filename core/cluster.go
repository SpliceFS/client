package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/fetch"
	"github.com/SpliceFS/client/internal/remote"
	"github.com/SpliceFS/client/internal/session"
	"github.com/SpliceFS/client/internal/tree"
	"github.com/SpliceFS/common/wire"
)

// cluster is everything the client keeps per configured cluster: the session that
// logs in and tracks node health, the merged tree, and the measured node speeds that
// outlive the open files.
type cluster struct {
	name    string
	cfg     config.Cluster
	session *session.Session
	tree    *tree.Tree
	meter   *fetch.RateMeter
	held    heldVersions
	opts    Options

	mu     sync.Mutex
	dialed map[string]*remote.Node // by node name, the latest one dialed
	known  []string                // node names in node-list order, as last seen
}

func newCluster(name string, cfg config.Cluster, password string, ttl time.Duration, opts Options) *cluster {
	c := &cluster{
		name:   name,
		cfg:    cfg,
		meter:  fetch.NewMeter(),
		opts:   opts,
		dialed: make(map[string]*remote.Node),
	}
	c.session = session.New(session.Options{
		Cluster:        name,
		EntryURL:       cfg.URL,
		User:           cfg.User,
		Password:       password,
		Dial:           c.dial,
		Now:            opts.Now,
		IsUnauthorized: isError(remote.ErrUnauthorized),
		IsUnavailable:  isError(remote.ErrUnavailable),
		Logger:         opts.Logger,
	})
	c.tree = tree.New(lister{c}, tree.Options{
		TTL:        ttl,
		Now:        opts.Now,
		IsNotFound: isError(remote.ErrNotFound),
	})
	return c
}

func isError(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// dial makes the Node for a name and remembers it, because the session hands nodes
// back as its own small interface and the client needs the full remote API.
func (c *cluster) dial(name, baseURL string) (session.Node, error) {
	n, err := remote.New(name, baseURL, c.opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dialed[name] = n
	return n, nil
}

// node returns the node dialed under name. Every name the session or the tree hands
// out belongs to a node made by dial, and a node redialed under the same name replaces
// the old one in the session too, so the lookup finds the node they mean.
func (c *cluster) node(name string) *remote.Node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dialed[name]
}

// remotes turns the session's nodes back into the dialed ones, which have the full
// remote API.
func (c *cluster) remotes(nodes []session.Node) []*remote.Node {
	out := make([]*remote.Node, len(nodes))
	for i, n := range nodes {
		out[i] = c.node(n.Name())
	}
	return out
}

// nodes returns every node of the cluster and the ones that are usable now,
// connecting first if needed. It also remembers the node names for Status, which must
// not connect.
func (c *cluster) nodes(ctx context.Context) (all, usable []*remote.Node, err error) {
	// After a failed Nodes, Usable fails at once with the same error, because the
	// session holds a failed connect in its backoff; one check covers both.
	members, errMembers := c.session.Nodes(ctx)
	ready, errReady := c.session.Usable(ctx)
	if err := cmp.Or(errMembers, errReady); err != nil {
		return nil, nil, err
	}
	all = c.remotes(members)
	c.mu.Lock()
	c.known = names(all)
	c.mu.Unlock()
	return all, c.remotes(ready), nil
}

// knownNodes returns the node names last seen, without using the network.
func (c *cluster) knownNodes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.known)
}

func names(nodes []*remote.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name()
	}
	return out
}

// lister lets the tree list directories through the session.
type lister struct{ c *cluster }

func (l lister) Nodes(ctx context.Context) ([]string, error) {
	_, usable, err := l.c.nodes(ctx)
	return names(usable), err
}

func (l lister) List(ctx context.Context, node, dir string) ([]wire.Entry, error) {
	n := l.c.node(node)
	var entries []wire.Entry
	err := l.c.session.Do(ctx, n, func(ctx context.Context, token string) (err error) {
		entries, err = n.List(ctx, token, dir)
		return err
	})
	return entries, err
}

// source is one node's copy of one version of a file, for the block reader.
type source struct {
	c    *cluster
	node *remote.Node
	file string
	etag string
}

func (s source) ID() string { return s.node.Name() }

func (s source) Fetch(ctx context.Context, off int64, buf []byte) error {
	err := s.c.session.Do(ctx, s.node, func(ctx context.Context, token string) error {
		body, err := s.node.Range(ctx, token, s.file, s.etag, off, int64(len(buf)))
		if err != nil {
			return err
		}
		_, err = io.ReadFull(body, buf)
		return errors.Join(err, body.Close())
	})
	if errors.Is(err, remote.ErrChanged) || errors.Is(err, remote.ErrNotFound) || errors.Is(err, remote.ErrForbidden) {
		return fmt.Errorf("%w: %w", fetch.ErrGone, err)
	}
	return err
}
