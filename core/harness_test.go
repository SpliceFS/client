package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	pathpkg "path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/fetch"
	"github.com/SpliceFS/client/internal/version"
	"github.com/SpliceFS/common/api"
	"github.com/SpliceFS/common/apitest"
	"github.com/SpliceFS/common/units"
	"github.com/SpliceFS/common/wire"
)

// fakeClock fires timers only when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c     *fakeClock
	when  time.Time
	f     func()
	armed bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

//nolint:ireturn // AfterFunc implements the fetch clock, whose method returns an interface.
func (c *fakeClock) AfterFunc(d time.Duration, f func()) fetch.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), f: f, armed: true}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.armed
	t.armed = false
	return was
}

// Advance moves time forward by d, running due timers in order in the calling
// goroutine.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if t.armed && !t.when.After(target) && (next == nil || t.when.Before(next.when)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		next.armed = false
		c.now = next.when
		c.mu.Unlock()
		next.f()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

const (
	testUser     = "alice"
	testPassword = "secret"
	testCluster  = "example"
)

// fileTime is the modification time of the files the tests create, unless a test needs
// another one.
func fileTime() time.Time { return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC) }

// pattern is deterministic file content that differs with seed.
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+i>>8) ^ seed
	}
	return b
}

type fakeFile struct {
	data  []byte
	mtime time.Time
}

func (f fakeFile) etag() string {
	return api.ETag(int64(len(f.data)), f.mtime)
}

func (f fakeFile) digest() string {
	sum := sha256.Sum256(f.data[:min(len(f.data), wire.HeadDigestBytes)])
	return hex.EncodeToString(sum[:])
}

// tailDigest hashes the last wire.TailDigestBytes bytes, or all of a shorter file.
func (f fakeFile) tailDigest() string {
	sum := sha256.Sum256(f.data[max(0, len(f.data)-wire.TailDigestBytes):])
	return hex.EncodeToString(sum[:])
}

// fakeNode is a real API handler (apitest) over in-memory shares. Its switches let a
// test make it fail in the ways a real node can.
type fakeNode struct {
	name string
	node *apitest.Node
	// client trusts this node's certificate; it is kept so its connections are reused.
	client *http.Client
	group  *fakeCluster

	mu     sync.Mutex
	files  map[string]fakeFile // by "share/dir/name"
	shares map[string]*apitest.Share
	// emptyDirs are folders without files, by "share/dir", which an apitest share cannot hold.
	emptyDirs map[string]bool
	space     map[string]wire.Space
	gate      chan struct{} // when set, requests wait until it is closed
	reject    int           // this many authorized requests are answered 401
	denied    bool          // every share is refused (403)
	counts    map[string]int
	logins    int
	waits     chan struct{} // receives a value whenever a request waits at the gate
	failed    chan struct{} // receives a value whenever a request to the node fails
}

func (n *fakeNode) url() string { return n.node.URL() }

func (n *fakeNode) put(path string, data []byte, mtime time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.files[path] = fakeFile{data: data, mtime: mtime}
	n.rebuild(path)
}

// putAll stores files, by "share/dir/name", and builds each share they touch once.
func (n *fakeNode) putAll(files map[string]fakeFile) {
	n.mu.Lock()
	defer n.mu.Unlock()
	touched := map[string]bool{}
	for path, stored := range files {
		n.files[path] = stored
		touched[path] = true
	}
	built := map[string]bool{}
	for path := range touched {
		shareName, _, _ := strings.Cut(path, "/")
		if !built[shareName] {
			built[shareName] = true
			n.rebuild(path)
		}
	}
}

func (n *fakeNode) remove(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.files, path)
	n.rebuild(path)
}

// rebuild makes the share that holds path anew from the files, because an apitest share
// only ever gains files.
func (n *fakeNode) rebuild(path string) {
	shareName, _, _ := strings.Cut(path, "/")
	fresh := apitest.NewShare(fileTime())
	fresh.SetSpace(n.space[shareName])
	for filePath, stored := range n.files {
		if rest, ok := strings.CutPrefix(filePath, shareName+"/"); ok {
			if err := fresh.Replace(rest, stored.data, stored.mtime); err != nil {
				panic(err)
			}
		}
	}
	n.shares[shareName] = fresh
}

// putEmptyDir makes path a folder without files.
func (n *fakeNode) putEmptyDir(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.emptyDirs[path] = true
}

func (n *fakeNode) emptyDir(shareName, name string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.emptyDirs[shareName+"/"+name]
}

// emptyDirsIn lists the empty folders directly in the folder dir of a share.
func (n *fakeNode) emptyDirsIn(shareName, dir string) []api.Info {
	n.mu.Lock()
	defer n.mu.Unlock()
	var infos []api.Info
	for full := range n.emptyDirs {
		name, ok := strings.CutPrefix(full, shareName+"/")
		if ok && pathpkg.Dir("/"+name) == pathpkg.Clean("/"+dir) {
			infos = append(infos, api.Info{Name: pathpkg.Base(name), Dir: true, MTime: fileTime()})
		}
	}
	return infos
}

func (n *fakeNode) setSpace(shareName string, total, free uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.space[shareName] = wire.Space{Total: total, Free: free}
	n.shares[shareName].SetSpace(n.space[shareName])
}

// stop makes the node go away: it drops every connection and refuses new ones.
func (n *fakeNode) stop() {
	n.node.Stop()
}

// deny makes the node refuse every share to every token.
func (n *fakeNode) deny() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.denied = true
}

func (n *fakeNode) rejectNext(count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reject = count
}

// hold makes requests wait until the returned function is called, or the test ends.
func (n *fakeNode) hold(t *testing.T) func() {
	t.Helper()
	gate := make(chan struct{})
	n.mu.Lock()
	n.gate = gate
	n.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			n.mu.Lock()
			n.gate = nil
			n.mu.Unlock()
			close(gate)
		})
	}
	t.Cleanup(release)
	return release
}

// waitAtGate holds a request while the test holds the node.
func (n *fakeNode) waitAtGate() {
	n.mu.Lock()
	gate := n.gate
	n.mu.Unlock()
	if gate == nil {
		return
	}
	select {
	case n.waits <- struct{}{}:
	default:
	}
	<-gate
}

func (n *fakeNode) noteFailed() {
	select {
	case n.failed <- struct{}{}:
	default:
	}
}

func (n *fakeNode) count(kind string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.counts[kind]
}

func (n *fakeNode) loginCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.logins
}

// current returns the latest build of the share shareName.
func (n *fakeNode) current(shareName string) *apitest.Share {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shares[shareName]
}

// countRequest counts a request the node is sent, by its endpoint.
func (n *fakeNode) countRequest(r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.counts[requestKind(r)]++
}

// requestKind names the endpoint of a request.
func requestKind(r *http.Request) string {
	apiPath := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	switch {
	case apiPath == "login":
		return "login"
	case apiPath == "nodes":
		return "nodes"
	case strings.HasPrefix(apiPath, "tree/"):
		return "list"
	case strings.HasPrefix(apiPath, "space/"):
		return "space"
	case strings.HasPrefix(apiPath, "file/") && r.Method == http.MethodHead:
		return "head"
	case strings.HasPrefix(apiPath, "file/"):
		return "range"
	}
	return "other"
}

// nodeShare is one share of a node as the handler sees it: always the latest build of
// the share.
type nodeShare struct {
	n    *fakeNode
	name string
}

func (s nodeShare) Stat(name string) (api.Info, error) {
	if s.n.emptyDir(s.name, name) {
		return api.Info{Name: pathpkg.Base(name), Dir: true, MTime: fileTime()}, nil
	}
	return s.n.current(s.name).Stat(name)
}

func (s nodeShare) List(name string) ([]api.Info, error) {
	if s.n.emptyDir(s.name, name) {
		return nil, nil
	}
	infos, err := s.n.current(s.name).List(name)
	if err != nil {
		return nil, err
	}
	infos = append(infos, s.n.emptyDirsIn(s.name, name)...)
	slices.SortFunc(infos, func(a, b api.Info) int { return strings.Compare(a.Name, b.Name) })
	return infos, nil
}

//nolint:ireturn // api.Share.Open returns the File interface.
func (s nodeShare) Open(name string) (api.File, error) {
	return s.n.current(s.name).Open(name)
}

func (s nodeShare) Digests(name string) (api.FileDigests, api.Info, error) {
	return s.n.current(s.name).Digests(name)
}

func (s nodeShare) Space() (wire.Space, error) {
	return s.n.current(s.name).Space()
}

// nodeAuth is the cluster's users as one node sees them, with the node's switches.
type nodeAuth struct {
	n *fakeNode
}

func (a nodeAuth) Login(ctx context.Context, addr, user, password string) (string, time.Time, error) {
	a.n.waitAtGate()
	token, expires, err := a.n.group.auth.Login(ctx, addr, user, password)
	if err == nil {
		a.n.mu.Lock()
		a.n.logins++
		a.n.mu.Unlock()
	}
	return token, expires, err
}

func (a nodeAuth) Verify(token string) (api.Claims, error) {
	a.n.waitAtGate()
	a.n.mu.Lock()
	rejected, denied := a.n.reject > 0, a.n.denied
	if rejected {
		a.n.reject--
	}
	a.n.mu.Unlock()
	if rejected {
		return api.Claims{}, api.ErrInvalidToken
	}
	claims, err := a.n.group.auth.Verify(token)
	if denied {
		claims.Shares = nil
	}
	return claims, err
}

// fakeCluster is a set of nodes that share one node list and one set of users.
type fakeCluster struct {
	clock *fakeClock
	auth  *apitest.Auth
	nodes []*fakeNode
	extra []wire.Node // listed besides the nodes
}

// newFakeCluster starts the named nodes, each serving the shares named in shareNames.
func newFakeCluster(t *testing.T, clock *fakeClock, shareNames []string, names ...string) *fakeCluster {
	t.Helper()
	fakes := &fakeCluster{clock: clock, auth: apitest.NewAuth(clock.Now, time.Hour)}
	fakes.auth.AddUser(testUser, testPassword, "*")
	for _, name := range names {
		n := &fakeNode{
			name:      name,
			group:     fakes,
			files:     make(map[string]fakeFile),
			shares:    make(map[string]*apitest.Share),
			emptyDirs: make(map[string]bool),
			space:     make(map[string]wire.Space),
			counts:    make(map[string]int),
			waits:     make(chan struct{}, 64),
			failed:    make(chan struct{}, 64),
		}
		shares := make(map[string]api.Share)
		for _, shareName := range shareNames {
			n.shares[shareName] = apitest.NewShare(fileTime())
			shares[shareName] = nodeShare{n: n, name: shareName}
		}
		n.node = apitest.Start(t, api.Options{
			BasePath: "/api",
			Node:     name,
			Version:  version.String(),
			Shares:   shares,
			Auth:     nodeAuth{n: n},
			Nodes:    fakes,
			Logger:   slog.New(slog.DiscardHandler),
			Now:      clock.Now,
		})
		n.client = n.node.Client()
		fakes.nodes = append(fakes.nodes, n)
	}
	return fakes
}

// Nodes lists the nodes and the extra entries, read when each request comes.
func (fc *fakeCluster) Nodes() (wire.Nodes, error) {
	list := wire.Nodes{Cluster: testCluster}
	for _, n := range fc.nodes {
		list.Nodes = append(list.Nodes, wire.Node{Name: n.name, URL: n.url()})
	}
	list.Nodes = append(list.Nodes, fc.extra...)
	return list, nil
}

// RoundTrip sends each request with the client of the node it goes to, which trusts
// that node's certificate; a request to any other address goes out as usual.
func (fc *fakeCluster) RoundTrip(r *http.Request) (*http.Response, error) {
	for _, n := range fc.nodes {
		if strings.HasPrefix(n.url(), "https://"+r.URL.Host+"/") {
			n.countRequest(r)
			resp, err := n.client.Transport.RoundTrip(r)
			if err != nil {
				n.noteFailed()
			}
			return resp, err
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// put stores a file on every node.
func (fc *fakeCluster) put(path string, data []byte, mtime time.Time) {
	for _, n := range fc.nodes {
		n.put(path, data, mtime)
	}
}

func (fc *fakeCluster) setAllDown() {
	for _, n := range fc.nodes {
		n.stop()
	}
}

func (fc *fakeCluster) count(kind string) int {
	total := 0
	for _, n := range fc.nodes {
		total += n.count(kind)
	}
	return total
}

// syncBuffer collects log output from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is a Client over a fake cluster, with a fake clock and a real cache
// directory.
type harness struct {
	t      *testing.T
	clock  *fakeClock
	fc     *fakeCluster
	cfg    *config.Config
	client *Client
	logs   *syncBuffer
}

const (
	testBlockSize = 64 << 10
	testCacheTTL  = 30 * time.Second
	testRevalid   = time.Hour
)

func testCache(dir string) config.Cache {
	return config.Cache{
		Dir:        dir,
		MaxSize:    units.UnlimitedSize,
		MaxAge:     units.UnlimitedDuration,
		Revalidate: units.Duration(testRevalid),
		ListingTTL: units.Duration(testCacheTTL),
	}
}

// newHarness makes a fake cluster with the named nodes, which serve the share media, and
// a configuration with one mount of the cluster root; a test may change the
// configuration before the Client is built by start.
func newHarness(t *testing.T, names ...string) *harness {
	t.Helper()
	return newHarnessOf(t, []string{"media"}, names...)
}

// newHarnessOf is newHarness with nodes that serve the shares named in shareNames.
func newHarnessOf(t *testing.T, shareNames []string, names ...string) *harness {
	t.Helper()
	clock := newFakeClock()
	fakes := newFakeCluster(t, clock, shareNames, names...)
	cacheCfg := testCache(t.TempDir())
	cfg := &config.Config{
		Cache: cacheCfg,
		Clusters: map[string]config.Cluster{testCluster: {
			URL:            fakes.nodes[0].url(),
			User:           testUser,
			PasswordFile:   "password",
			BlockSize:      testBlockSize,
			StreamsPerNode: 2,
			ReadAhead:      4 * testBlockSize,
		}},
		Mounts: []config.Mount{{Cluster: testCluster, Path: "/", Target: "/mnt/files", Offline: true, Cache: cacheCfg}},
	}
	return &harness{t: t, clock: clock, fc: fakes, cfg: cfg, logs: &syncBuffer{}}
}

// addMount adds a mount of path with the top-level cache settings.
func (h *harness) addMount(path, target string) {
	h.cfg.Mounts = append(h.cfg.Mounts, config.Mount{
		Cluster: testCluster, Path: path, Target: target, Offline: true, Cache: h.cfg.Cache,
	})
}

func (h *harness) options() Options {
	return Options{
		Logger:       slog.New(slog.NewTextHandler(h.logs, nil)),
		HTTPClient:   &http.Client{Transport: h.fc},
		Now:          h.clock.Now,
		ReadPassword: func(string) (string, error) { return testPassword, nil },
		openCache:    cache.OpenUnsynced,
	}
}

// start builds the Client, which is closed when the test ends.
func (h *harness) start() {
	h.t.Helper()
	started, err := newClient(h.cfg, h.options(), h.clock)
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	h.t.Cleanup(func() {
		// A test may close the client itself; the second Close then finds its caches closed.
		if err := started.Close(); err != nil && !errors.Is(err, cache.ErrClosed) {
			h.t.Errorf("Close: %v", err)
		}
	})
	h.client = started
}

// view returns the FS of mount i, starting the Client if needed.
//
//nolint:ireturn // FS is the interface that Client.View returns and the tests exercise.
func (h *harness) view(i int) FS {
	h.t.Helper()
	if h.client == nil {
		h.start()
	}
	v, err := h.client.View(h.cfg.Mounts[i])
	if err != nil {
		h.t.Fatalf("View: %v", err)
	}
	return v
}

func (h *harness) cache(i int) *cache.Cache {
	return h.client.caches[h.cfg.Mounts[i].Cache.Dir].Cache
}

// fill stores every block of data in the cache entry for the version that is cached
// for path, so that the entry is complete.
func (h *harness) fill(i int, path string, data []byte) {
	h.t.Helper()
	h.fillBlocks(i, path, data, (int64(len(data))+testBlockSize-1)/testBlockSize)
}

// fillBlocks stores the first count blocks of data in the cache entry for the version
// that is cached for path.
func (h *harness) fillBlocks(i int, path string, data []byte, count int64) {
	h.t.Helper()
	entry := h.pin(i, path)
	for b := range count {
		block := data[b*testBlockSize : min(int64(len(data)), (b+1)*testBlockSize)]
		if err := entry.WriteBlock(b, block); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := entry.Release(); err != nil {
		h.t.Fatal(err)
	}
}

// stored returns the bytes of the complete cache entry for path.
func (h *harness) stored(i int, path string) []byte {
	h.t.Helper()
	entry := h.pin(i, path)
	defer func() {
		if err := entry.Release(); err != nil {
			h.t.Errorf("release the cache entry of %s: %v", path, err)
		}
	}()
	data := make([]byte, entry.Version().Size)
	if n, err := entry.ReadAt(data, 0); n != len(data) {
		h.t.Fatalf("read cache entry of %s: %d bytes, %v", path, n, err)
	}
	return data
}

// pin acquires the cache entry for the version that is cached for path.
func (h *harness) pin(i int, path string) *cache.Entry {
	h.t.Helper()
	key := cache.Key{Cluster: testCluster, Path: path}
	info, ok := h.cache(i).Peek(key)
	if !ok {
		h.t.Fatalf("nothing cached for %s", path)
	}
	entry, err := h.cache(i).Acquire(key, info.Version)
	if err != nil {
		h.t.Fatal(err)
	}
	return entry
}

// closeAfter closes closer when the test ends and fails the test if that fails.
func closeAfter(t *testing.T, closer io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// readFile opens name and reads it to the end in small pieces.
// closeFile closes closer and fails the test when that fails.
func closeFile(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func readFile(t *testing.T, fsys FS, name string) []byte {
	t.Helper()
	opened, err := fsys.Open(context.Background(), name)
	if err != nil {
		t.Fatalf("Open %q: %v", name, err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Errorf("Close %q: %v", name, err)
		}
	}()
	return readAll(t, opened)
}

func readAll(t *testing.T, f File) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 20000)
	for {
		n, err := f.ReadAt(context.Background(), buf, int64(len(out)))
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadAt %d: %v", len(out), err)
		}
	}
}
