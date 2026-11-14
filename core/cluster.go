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
	opts    Options // with this cluster's own HTTPClient

	// password and ttl are what the cluster was built with, to tell on a reload whether
	// it can be kept.
	password string
	ttl      time.Duration
	ownHTTP  bool // the HTTP client was made for this cluster and is closed with it

	// holds counts the views that use the cluster and retired says that a reload replaced
	// or removed it. Both are guarded by the Client's mu.
	holds   int
	retired bool

	mu     sync.Mutex
	dialed map[string]*remote.Node // by node name, the latest one dialed
	known  []string                // node names in node-list order, as last seen
}

func newCluster(name string, cfg config.Cluster, password string, ttl time.Duration, opts Options) *cluster {
	clus := &cluster{
		name:     name,
		cfg:      cfg,
		meter:    fetch.NewMeter(),
		opts:     opts,
		password: password,
		ttl:      ttl,
		dialed:   make(map[string]*remote.Node),
	}
	if opts.HTTPClient == nil {
		clus.opts.HTTPClient = newHTTPClient(cfg.StreamsPerNode, systemDialer())
		clus.ownHTTP = true
	}
	clus.session = session.New(clus.sessionOptions())
	clus.tree = tree.New(lister{clus}, tree.Options{
		TTL:        ttl,
		Now:        opts.Now,
		IsNotFound: isError(remote.ErrNotFound),
	})
	return clus
}

// sessionOptions says how the session of the cluster reaches it and judges the errors of its nodes.
func (c *cluster) sessionOptions() session.Options {
	return session.Options{
		Cluster:        c.name,
		EntryURL:       c.cfg.URL,
		User:           c.cfg.User,
		Password:       c.password,
		Dial:           c.dial,
		Now:            c.opts.Now,
		IsUnauthorized: isError(remote.ErrUnauthorized),
		IsUnavailable:  unreachable,
		IsIncompatible: isError(remote.ErrIncompatible),
		Logger:         c.opts.Logger,
	}
}

// matches reports whether a cluster built with cfg, password and ttl would behave as this
// one does.
func (c *cluster) matches(cfg config.Cluster, password string, ttl time.Duration) bool {
	return c.cfg == cfg && c.password == password && c.ttl == ttl
}

// close ends the session and drops the idle connections of the cluster's own HTTP client.
// A client that was passed in with Options is shared and left alone.
func (c *cluster) close() {
	c.session.Close()
	if c.ownHTTP {
		c.opts.HTTPClient.CloseIdleConnections()
	}
}

// unreachable reports whether a node cannot serve this client now: it is down, or it runs a
// splicefsd the client refuses. Both rest the node, so that one old server never fails a read
// that newer servers can deliver.
func unreachable(err error) bool {
	return errors.Is(err, remote.ErrUnavailable) || errors.Is(err, remote.ErrIncompatible)
}

func isError(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// dial makes the Node for a name and remembers it, because the session hands nodes
// back as its own small interface and the client needs the full remote API.
//
//nolint:ireturn // session.Options.Dial fixes this signature.
func (c *cluster) dial(name, baseURL string) (session.Node, error) {
	node, err := remote.New(name, baseURL, c.opts.HTTPClient)
	if err != nil {
		return nil, err //nolint:wrapcheck // remote.New names the node and says what is wrong with its URL.
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dialed[name] = node
	return node, nil
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

// nodeSet is the nodes of a cluster at one moment.
type nodeSet struct {
	all    []*remote.Node // every node of the cluster
	usable []*remote.Node // the ones that can be used now
}

// nodes returns every node of the cluster and the ones that are usable now,
// connecting first if needed. It also remembers the node names for Status, which must
// not connect.
func (c *cluster) nodes(ctx context.Context) (nodeSet, error) {
	// After a failed Nodes, Usable fails at once with the same error, because the
	// session holds a failed connect in its backoff; one check covers both.
	members, errMembers := c.session.Nodes(ctx)
	ready, errReady := c.session.Usable(ctx)
	if err := cmp.Or(errMembers, errReady); err != nil {
		// Connect adds the cluster to the message, and the session error says the rest.
		return nodeSet{}, err //nolint:wrapcheck // See above, a prefix would change the message.
	}
	set := nodeSet{all: c.remotes(members), usable: c.remotes(ready)}
	c.mu.Lock()
	c.known = names(set.all)
	c.mu.Unlock()
	return set, nil
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
	set, err := l.c.nodes(ctx)
	return names(set.usable), err
}

func (l lister) List(ctx context.Context, node, dir string) ([]wire.Entry, error) {
	remoteNode := l.c.node(node)
	return callNode(ctx, l.c, remoteNode, remoteNode.List, dir)
}

// callNode runs one request of a node that takes a name and returns a value, such as
// List, Head or Space, through the cluster's session.
//
//nolint:ireturn // T is the result type of the request the caller passes in, not an interface chosen here.
func callNode[T any](
	ctx context.Context, clus *cluster, node *remote.Node,
	method func(ctx context.Context, token, name string) (T, error), name string,
) (T, error) {
	var result T
	err := clus.do(ctx, node, func(ctx context.Context, token string) error {
		var err error
		result, err = method(ctx, token, name)
		return err
	})
	return result, err
}

// do runs call on node through the session, which logs in and retries as needed, and
// names the node in the error.
func (c *cluster) do(ctx context.Context, node *remote.Node, call func(context.Context, string) error) error {
	if err := c.session.Do(ctx, node, call); err != nil {
		return fmt.Errorf("node %s: %w", node.Name(), err)
	}
	return nil
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
	err := s.c.do(ctx, s.node, func(ctx context.Context, token string) error {
		body, err := s.node.Range(ctx, token, s.file, s.etag, off, int64(len(buf)))
		if err != nil {
			return fmt.Errorf("read range of %q: %w", s.file, err)
		}
		_, err = io.ReadFull(body, buf)
		return errors.Join(err, body.Close())
	})
	if errors.Is(err, remote.ErrChanged) || errors.Is(err, remote.ErrNotFound) || errors.Is(err, remote.ErrForbidden) {
		return fmt.Errorf("%w: %w", fetch.ErrGone, err)
	}
	return err
}
