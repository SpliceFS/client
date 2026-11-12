// Package session owns the connection of the client to one cluster: the login, the
// token, the set of nodes and the health of each node. It is the only place that
// logs in, so a token that expires or is refused is replaced once, for every caller.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/SpliceFS/common/wire"
)

// Errors that callers branch on, checked with errors.Is.
var (
	// ErrBadCredentials means the cluster rejected the user or the password.
	ErrBadCredentials = errors.New("cluster rejected the user or password")
	// ErrUnavailable means no node of the cluster could be reached.
	ErrUnavailable = errors.New("no cluster node could be reached")
	// ErrClosed means the session was closed.
	ErrClosed = errors.New("session closed")
)

const (
	defaultRefreshNodes = 5 * time.Minute
	defaultRenewBefore  = 2 * time.Minute

	// entryName is the name the entry URL is dialed with; it is a way in, not a node.
	entryName = "entry"
)

// Node is one server, as this package needs it.
type Node interface {
	// Name identifies the node within its cluster.
	Name() string
	// Login exchanges the user and password for a token.
	Login(ctx context.Context, user, password string) (wire.LoginResponse, error)
	// Nodes returns the node list of the cluster.
	Nodes(ctx context.Context, token string) (wire.Nodes, error)
}

// Dial makes a Node for a name and a base URL. It must not use the network.
type Dial func(name, baseURL string) (Node, error)

// Options configures a Session. Dial, User, Password and EntryURL are required;
// every other zero value selects a default.
type Options struct {
	Cluster  string // the config name of the cluster, for messages
	EntryURL string
	User     string
	Password string
	Dial     Dial
	// Now is the clock; the default is time.Now.
	Now func() time.Time
	// RefreshNodes is how often the node list is fetched again; the default is 5m.
	RefreshNodes time.Duration
	// RenewBefore makes the token be renewed when it expires within this time;
	// the default is 2m.
	RenewBefore time.Duration
	// IsUnauthorized reports whether an error from a node means the token or the
	// credentials were refused.
	IsUnauthorized func(error) bool
	// IsUnavailable reports whether an error from a node means the node could not
	// be reached or is failing, as opposed to having answered.
	IsUnavailable func(error) bool
	// Logger receives operational messages; the default discards them.
	Logger *slog.Logger
}

// Session is the connection to one cluster. It is safe for concurrent use.
type Session struct {
	opts       Options
	connecting group
	renewing   group

	mu           sync.Mutex
	closed       bool
	connected    bool
	token        string
	expires      time.Time
	members      []*member
	fetched      time.Time
	connectRetry backoff
	loginRetry   backoff
}

// New returns a Session. It does not use the network: the cluster is contacted by
// the first call that needs it.
func New(opts Options) *Session {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RefreshNodes <= 0 {
		opts.RefreshNodes = defaultRefreshNodes
	}
	if opts.RenewBefore <= 0 {
		opts.RenewBefore = defaultRenewBefore
	}
	if opts.IsUnauthorized == nil {
		opts.IsUnauthorized = never
	}
	if opts.IsUnavailable == nil {
		opts.IsUnavailable = never
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Session{opts: opts}
}

func never(error) bool { return false }

// Nodes returns every node of the cluster in node-list order, connecting first if
// needed.
func (s *Session) Nodes(ctx context.Context) ([]Node, error) {
	if err := s.prepare(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return nodesOf(s.members), nil
}

// Usable returns the nodes that are not cooling down after a failure, in node-list
// order. When every node is cooling it returns only the one whose cooldown ends
// first, so that there is always something to try.
func (s *Session) Usable(ctx context.Context) ([]Node, error) {
	if err := s.prepare(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return nodesOf(s.usable(s.opts.Now())), nil
}

// Do calls call with the current token. When call fails with an unauthorized error
// the token is replaced and call is made once more. The outcome is recorded as the
// health of n: an unavailable error starts or extends its cooldown, success clears
// it. The error of call is returned unchanged, except that a failed replacement of
// the token is returned instead.
func (s *Session) Do(ctx context.Context, n Node, call func(ctx context.Context, token string) error) error {
	token, err := s.currentToken(ctx)
	if err != nil {
		return err
	}
	err = call(ctx, token)
	if err != nil && s.opts.IsUnauthorized(err) {
		if token, err = s.renew(ctx, token); err != nil {
			return err
		}
		err = call(ctx, token)
	}
	s.record(n, err)
	return err
}

// Close makes every later call fail with ErrClosed.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func (s *Session) prepare(ctx context.Context) error {
	if err := s.ensureConnected(ctx); err != nil {
		return err
	}
	s.refreshNodes(ctx)
	return nil
}

func (s *Session) record(n Node, err error) {
	if err != nil && !s.opts.IsUnavailable(err) {
		// The node answered, so its health is neither proven nor disproven.
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.members {
		if m.name != n.Name() {
			continue
		}
		if err == nil {
			m.health.reset()
		} else {
			m.health.fail(s.opts.Now(), err)
		}
		return
	}
}

// refreshNodes fetches the node list again once RefreshNodes has passed. The time
// of the attempt is taken before it is made, so concurrent callers do not all fetch
// and a failure is retried only after the normal interval.
func (s *Session) refreshNodes(ctx context.Context) {
	s.mu.Lock()
	now := s.opts.Now()
	if now.Sub(s.fetched) < s.opts.RefreshNodes {
		s.mu.Unlock()
		return
	}
	s.fetched = now
	pick := s.usable(now)[0]
	known := s.members
	s.mu.Unlock()

	var list wire.Nodes
	err := s.Do(ctx, pick.node, func(ctx context.Context, token string) (err error) {
		list, err = pick.node.Nodes(ctx, token)
		return err
	})
	if err != nil {
		s.opts.Logger.Warn("node list refresh failed", "cluster", s.opts.Cluster, "node", pick.name, "error", err)
		return
	}
	next := s.dialAll(list.Nodes, known)
	if len(next) == 0 {
		s.opts.Logger.Warn("node list refresh returned no usable node", "cluster", s.opts.Cluster)
		return
	}
	s.mu.Lock()
	s.members = next
	s.mu.Unlock()
}

// usable is the members that are not cooling, or the one that stops cooling first.
// The caller holds s.mu and s.members is not empty.
func (s *Session) usable(now time.Time) []*member {
	var ready []*member
	var soonest *member
	for _, m := range s.members {
		switch {
		case !m.health.active(now):
			ready = append(ready, m)
		case soonest == nil || m.health.until.Before(soonest.health.until):
			soonest = m
		}
	}
	if len(ready) == 0 {
		return []*member{soonest}
	}
	return ready
}

func nodesOf(members []*member) []Node {
	nodes := make([]Node, len(members))
	for i, m := range members {
		nodes[i] = m.node
	}
	return nodes
}

func (s *Session) unavailable(what string, err error) error {
	return fmt.Errorf("cluster %q: %s: %w: %w", s.opts.Cluster, what, ErrUnavailable, err)
}
