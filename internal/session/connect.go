package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

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
			s.opts.Logger.WarnContext(ctx, "cluster connect failed", slog.String("cluster", s.opts.Cluster),
				slog.Duration("retry_in", delay(s.connectRetry.failures)), slog.Any("error", err))
		}
		return err
	}
	s.connectRetry.reset()
	s.connected = true
	s.token, s.expires = login.Token, login.Expires
	s.members = members
	s.fetched = now
	s.opts.Logger.InfoContext(ctx, "cluster connected", slog.String("cluster", s.opts.Cluster),
		slog.Int("nodes", len(members)))
	return nil
}

// entryNode returns the cluster host. It is dialed on first use and kept; if dialing fails, the
// next attempt dials again.
//
//nolint:ireturn // Node is this package's own interface, and Options.Dial returns it.
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

// errNoneUsable ends the text of the error for a node list in which no node could be dialed.
var errNoneUsable = errors.New("none usable")

// enter logs in at the cluster host and fetches the node list with that token.
func (s *Session) enter(ctx context.Context) (wire.LoginResponse, []*member, error) {
	entry, login, err := s.loginAtEntry(ctx)
	if err != nil {
		return wire.LoginResponse{}, nil, err
	}
	list, err := entry.Nodes(ctx, login.Token)
	if err != nil {
		return wire.LoginResponse{}, nil, s.unavailable("fetch node list", err)
	}
	members := s.dialAll(s.listed(list.Nodes), nil)
	if len(members) == 0 {
		return wire.LoginResponse{}, nil, s.unavailable("fetch node list",
			fmt.Errorf("%d nodes listed, %w", len(list.Nodes), errNoneUsable))
	}
	return login, members, nil
}

// listed is the nodes to use for a node list the cluster host returned. A server lists
// itself only when its administrator has set its public address, so a single server
// that was just installed returns nothing although it is answering this very client:
// the URL of the cluster host is then the only node. A list with nodes is never added
// to, because the administrator chose it, and none of them being usable stays a failure.
func (s *Session) listed(nodes []wire.Node) []wire.Node {
	if len(nodes) > 0 {
		return nodes
	}
	s.fallbackNote.Do(func() {
		s.opts.Logger.Info("cluster lists no nodes, using its address as the only node",
			slog.String("cluster", s.opts.Cluster), slog.String("url", s.opts.EntryURL))
	})
	return []wire.Node{s.entryAsNode()}
}

// entryAsNode is the URL of the cluster host as a node. It is named after the host and
// port as written in the URL, so that status shows an address the user recognises. A URL
// without a host cannot be named that way and keeps the name it was dialed with. It is
// not called entryNode, which is the dialed cluster host that logins go to.
func (s *Session) entryAsNode() wire.Node {
	name := entryName
	if u, err := url.Parse(s.opts.EntryURL); err == nil && u.Host != "" {
		name = u.Host
	}
	return wire.Node{Name: name, URL: s.opts.EntryURL}
}

// dialAll returns a member for every listed node, in list order. A member in known
// with the same name and URL is kept, so its health survives; a node that cannot be
// dialed is skipped so that one bad entry does not hide the others.
func (s *Session) dialAll(listed []wire.Node, known []*member) []*member {
	byName := make(map[string]*member, len(known))
	for _, peer := range known {
		byName[peer.name] = peer
	}
	seen := make(map[string]bool, len(listed))
	members := make([]*member, 0, len(listed))
	for _, n := range listed {
		if seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		if peer, ok := byName[n.Name]; ok && peer.url == n.URL {
			members = append(members, peer)
			continue
		}
		node, err := s.opts.Dial(n.Name, n.URL)
		if err != nil {
			s.opts.Logger.Warn("cannot dial node", slog.String("cluster", s.opts.Cluster),
				slog.String("node", n.Name), slog.Any("error", err))
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
	s.mu.Unlock()

	_, login, err := s.loginAtEntry(ctx)

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

// loginAtEntry logs in at the cluster host and returns it with the login. Every login goes
// there and nowhere else: when the cluster host cannot be reached, no node is assumed to be
// reachable either, so the failure is final until the next attempt.
//
//nolint:ireturn // Node is this package's own interface, and Options.Dial returns it.
func (s *Session) loginAtEntry(ctx context.Context) (Node, wire.LoginResponse, error) {
	entry, err := s.entryNode()
	if err != nil {
		return nil, wire.LoginResponse{}, s.unavailable("dial entry", err)
	}
	login, err := entry.Login(ctx, s.opts.User, s.opts.Password)
	switch {
	case err == nil:
		return entry, login, nil
	case s.opts.IsUnauthorized(err):
		return nil, wire.LoginResponse{}, s.badCredentials("log in at entry", err)
	}
	return nil, wire.LoginResponse{}, s.unavailable("log in at entry", err)
}
