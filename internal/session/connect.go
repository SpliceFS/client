package session

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/SpliceFS/common/wire"
)

// ensureConnected connects on first use, and again after a failed attempt once its
// backoff has passed. Within the backoff it fails at once with the last error, so
// a wrong password file or a dead entry is not hammered.
func (s *Session) ensureConnected(ctx context.Context) error {
	s.mu.Lock()
	closed, connected := s.closed, s.connected
	s.mu.Unlock()
	switch {
	case closed:
		return ErrClosed
	case connected:
		return nil
	}
	return s.connecting.do(ctx, s.connect)
}

func (s *Session) connect(ctx context.Context) error {
	s.mu.Lock()
	if s.connected {
		s.mu.Unlock()
		return nil
	}
	if err := s.connectRetry.blocked(s.opts.Now()); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	login, members, err := s.enter(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	if err != nil {
		// A caller that gave up says nothing about the cluster.
		if ctx.Err() == nil {
			s.connectRetry.fail(now, err)
			s.opts.Logger.Warn("cluster connect failed", "cluster", s.opts.Cluster,
				"retry_in", delay(s.connectRetry.failures), "error", err)
		}
		return err
	}
	s.connectRetry.reset()
	s.connected = true
	s.token, s.expires = login.Token, login.Expires
	s.members = members
	s.fetched = now
	s.opts.Logger.Info("cluster connected", "cluster", s.opts.Cluster, "nodes", len(members))
	return nil
}

// entryNode returns the cluster host. It is dialed on first use and kept; if dialing fails, the
// next attempt dials again.
func (s *Session) entryNode() (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry == nil {
		entry, err := s.opts.Dial(entryName, s.opts.EntryURL)
		if err != nil {
			return nil, err
		}
		s.entry = entry
	}
	return s.entry, nil
}

func (s *Session) badCredentials(what string, err error) error {
	return fmt.Errorf("cluster %q: %s: %w: %w", s.opts.Cluster, what, ErrBadCredentials, err)
}

// enter logs in at the cluster host and fetches the node list with that token.
func (s *Session) enter(ctx context.Context) (wire.LoginResponse, []*member, error) {
	entry, err := s.entryNode()
	if err != nil {
		return wire.LoginResponse{}, nil, s.unavailable("dial entry", err)
	}
	login, err := entry.Login(ctx, s.opts.User, s.opts.Password)
	if err != nil {
		if s.opts.IsUnauthorized(err) {
			return wire.LoginResponse{}, nil, s.badCredentials("log in at entry", err)
		}
		return wire.LoginResponse{}, nil, s.unavailable("log in at entry", err)
	}
	list, err := entry.Nodes(ctx, login.Token)
	if err != nil {
		return wire.LoginResponse{}, nil, s.unavailable("fetch node list", err)
	}
	members := s.dialAll(list.Nodes, nil)
	if len(members) == 0 {
		return wire.LoginResponse{}, nil, s.unavailable("fetch node list",
			fmt.Errorf("%d nodes listed, none usable", len(list.Nodes)))
	}
	return login, members, nil
}

// dialAll returns a member for every listed node, in list order. A member in known
// with the same name and URL is kept, so its health survives; a node that cannot be
// dialed is skipped so that one bad entry does not hide the others.
func (s *Session) dialAll(listed []wire.Node, known []*member) []*member {
	byName := make(map[string]*member, len(known))
	for _, m := range known {
		byName[m.name] = m
	}
	seen := make(map[string]bool, len(listed))
	members := make([]*member, 0, len(listed))
	for _, n := range listed {
		if seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		if m, ok := byName[n.Name]; ok && m.url == n.URL {
			members = append(members, m)
			continue
		}
		node, err := s.opts.Dial(n.Name, n.URL)
		if err != nil {
			s.opts.Logger.Warn("cannot dial node", "cluster", s.opts.Cluster, "node", n.Name, "error", err)
			continue
		}
		members = append(members, &member{name: n.Name, url: n.URL, node: node})
	}
	return members
}

// currentToken returns a token that is valid for at least RenewBefore, logging in
// again if it is not.
func (s *Session) currentToken(ctx context.Context) (string, error) {
	if err := s.ensureConnected(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	token, expires := s.token, s.expires
	s.mu.Unlock()
	if expires.Sub(s.opts.Now()) > s.opts.RenewBefore {
		return token, nil
	}
	return s.renew(ctx, token)
}

// renew replaces the token stale. Concurrent callers share one login, and a caller
// holding a token that was already replaced gets the new one without another login.
func (s *Session) renew(ctx context.Context, stale string) (string, error) {
	err := s.renewing.do(ctx, func(ctx context.Context) error { return s.login(ctx, stale) })
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, nil
}

func (s *Session) login(ctx context.Context, stale string) error {
	s.mu.Lock()
	if s.token != stale {
		s.mu.Unlock()
		return nil
	}
	if err := s.loginRetry.blocked(s.opts.Now()); err != nil {
		s.mu.Unlock()
		return err
	}
	nodes := nodesOf(s.members)
	s.mu.Unlock()

	login, err := s.loginEntryFirst(ctx, nodes)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			s.loginRetry.fail(s.opts.Now(), err)
		}
		return err
	}
	s.loginRetry.reset()
	s.token, s.expires = login.Token, login.Expires
	return nil
}

// loginEntryFirst logs in at the cluster host. Only when it cannot be reached does it log in at
// the known nodes, so that a client that knows the nodes keeps working while the cluster host
// is down. Rejected credentials are final: the nodes would share the user file.
func (s *Session) loginEntryFirst(ctx context.Context, nodes []Node) (wire.LoginResponse, error) {
	entry, err := s.entryNode()
	if err != nil {
		return s.loginAny(ctx, nodes)
	}
	login, err := entry.Login(ctx, s.opts.User, s.opts.Password)
	switch {
	case err == nil:
		return login, nil
	case s.opts.IsUnauthorized(err):
		return wire.LoginResponse{}, s.badCredentials("log in at entry", err)
	case s.opts.IsUnavailable(err):
		return s.loginAny(ctx, nodes)
	}
	return wire.LoginResponse{}, s.unavailable("log in at entry", err)
}

// loginAny logs in at all nodes at once and returns the first success, so that a
// user file that has not reached every node yet still works. The slower attempts are
// cancelled and have ended when loginAny returns.
func (s *Session) loginAny(parent context.Context, nodes []Node) (wire.LoginResponse, error) {
	if len(nodes) == 0 {
		return wire.LoginResponse{}, s.unavailable("log in", errors.New("no nodes known"))
	}
	ctx, cancel := context.WithCancel(parent)
	type result struct {
		login wire.LoginResponse
		err   error
	}
	results := make(chan result, len(nodes))
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			login, err := n.Login(ctx, s.opts.User, s.opts.Password)
			results <- result{login, err}
		}()
	}

	var denied, last error
	for range nodes {
		r := <-results
		switch {
		case r.err == nil:
			return r.login, nil
		case s.opts.IsUnauthorized(r.err):
			denied = r.err
		default:
			last = r.err
		}
	}
	switch {
	case parent.Err() != nil:
		return wire.LoginResponse{}, parent.Err()
	case denied != nil:
		return wire.LoginResponse{}, s.badCredentials("log in", denied)
	}
	return wire.LoginResponse{}, s.unavailable("log in", last)
}
