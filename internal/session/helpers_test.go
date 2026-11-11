package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/client/internal/wire"
)

var (
	errDown   = errors.New("node down")
	errDenied = errors.New("login denied")
)

var start = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// clock is a manual clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeNode answers as scripted. The functions can be replaced while a test runs.
type fakeNode struct {
	name string

	mu       sync.Mutex
	loginFn  func(ctx context.Context) (wire.LoginResponse, error)
	nodesFn  func(ctx context.Context, token string) (wire.Nodes, error)
	logins   int
	listings []string // the tokens the node list was fetched with
}

func (f *fakeNode) Name() string { return f.name }

func (f *fakeNode) Login(ctx context.Context, user, password string) (wire.LoginResponse, error) {
	f.mu.Lock()
	f.logins++
	fn := f.loginFn
	f.mu.Unlock()
	if user != "alice" || password != "secret" {
		return wire.LoginResponse{}, fmt.Errorf("unexpected credentials %q %q", user, password)
	}
	return fn(ctx)
}

func (f *fakeNode) Nodes(ctx context.Context, token string) (wire.Nodes, error) {
	f.mu.Lock()
	f.listings = append(f.listings, token)
	fn := f.nodesFn
	f.mu.Unlock()
	return fn(ctx, token)
}

func (f *fakeNode) setLogin(fn func(ctx context.Context) (wire.LoginResponse, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginFn = fn
}

func (f *fakeNode) setNodes(fn func(ctx context.Context, token string) (wire.Nodes, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodesFn = fn
}

func (f *fakeNode) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *fakeNode) listingTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.listings)
}

func failWith(err error) func(context.Context) (wire.LoginResponse, error) {
	return func(context.Context) (wire.LoginResponse, error) { return wire.LoginResponse{}, err }
}

func nodesFail(err error) func(context.Context, string) (wire.Nodes, error) {
	return func(context.Context, string) (wire.Nodes, error) { return wire.Nodes{}, err }
}

// env is a fake cluster: an entry node and named nodes that all hand out numbered
// tokens that live for ttl.
type env struct {
	t     *testing.T
	clk   *clock
	entry *fakeNode
	ttl   time.Duration

	mu      sync.Mutex
	nodes   map[string]*fakeNode
	list    wire.Nodes
	dialErr map[string]error
	dialed  []string
	issued  int
}

func newEnv(t *testing.T, names ...string) *env {
	t.Helper()
	e := &env{
		t:       t,
		clk:     &clock{now: start},
		ttl:     time.Hour,
		nodes:   make(map[string]*fakeNode),
		dialErr: make(map[string]error),
	}
	e.entry = e.newNode(entryName)
	for _, name := range names {
		e.addNode(name)
	}
	return e
}

func (e *env) newNode(name string) *fakeNode {
	return &fakeNode{name: name, loginFn: e.grant, nodesFn: e.serveList}
}

// addNode creates a node and lists it, after the nodes that are already listed.
func (e *env) addNode(name string) {
	n := e.newNode(name)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nodes[name] = n
	e.list.Nodes = append(e.list.Nodes, wire.Node{Name: name, URL: urlOf(name)})
}

func urlOf(name string) string { return "https://" + name + ".example.com/api" }

func (e *env) setList(nodes ...wire.Node) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = wire.Nodes{Cluster: "main", Nodes: nodes}
}

func (e *env) grant(context.Context) (wire.LoginResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.issued++
	return wire.LoginResponse{Token: fmt.Sprintf("tok-%d", e.issued), Expires: e.clk.Now().Add(e.ttl)}, nil
}

func (e *env) serveList(context.Context, string) (wire.Nodes, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return wire.Nodes{Cluster: e.list.Cluster, Nodes: slices.Clone(e.list.Nodes)}, nil
}

func (e *env) dial(name, baseURL string) (Node, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dialed = append(e.dialed, name+" "+baseURL)
	if err := e.dialErr[name]; err != nil {
		return nil, err
	}
	if name == entryName {
		return e.entry, nil
	}
	n, ok := e.nodes[name]
	if !ok {
		return nil, fmt.Errorf("no fake node %q", name)
	}
	return n, nil
}

func (e *env) dials() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.dialed)
}

func (e *env) options() Options {
	return Options{
		Cluster:        "main",
		EntryURL:       "https://entry.example.com/api",
		User:           "alice",
		Password:       "secret",
		Dial:           e.dial,
		Now:            e.clk.Now,
		IsUnauthorized: func(err error) bool { return errors.Is(err, errDenied) },
		IsUnavailable:  func(err error) bool { return errors.Is(err, errDown) },
	}
}

func (e *env) session() *Session { return New(e.options()) }

func names(t *testing.T, nodes []Node) []string {
	t.Helper()
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name()
	}
	return out
}

func mustNodes(t *testing.T, s *Session) []string {
	t.Helper()
	nodes, err := s.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	return names(t, nodes)
}

func mustUsable(t *testing.T, s *Session) []string {
	t.Helper()
	nodes, err := s.Usable(context.Background())
	if err != nil {
		t.Fatalf("Usable: %v", err)
	}
	return names(t, nodes)
}

func checkNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// token returns the token that Do hands to its callback.
func token(t *testing.T, s *Session, n Node) string {
	t.Helper()
	var got string
	err := s.Do(context.Background(), n, func(_ context.Context, tok string) error {
		got = tok
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return got
}

// fail makes Do record an unavailable failure for n.
func fail(t *testing.T, s *Session, n Node) {
	t.Helper()
	err := s.Do(context.Background(), n, func(context.Context, string) error { return errDown })
	if !errors.Is(err, errDown) {
		t.Fatalf("Do = %v, want errDown", err)
	}
}

func nodeOf(t *testing.T, s *Session, name string) Node {
	t.Helper()
	nodes, err := s.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	for _, n := range nodes {
		if n.Name() == name {
			return n
		}
	}
	t.Fatalf("no node %q", name)
	return nil
}
