package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/fetch"
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
	return fmt.Sprintf("\"%x-%x\"", len(f.data), f.mtime.Unix())
}

func (f fakeFile) digest() string {
	sum := sha256.Sum256(f.data[:min(len(f.data), wire.HeadDigestBytes)])
	return hex.EncodeToString(sum[:])
}

// fakeNode serves the API from an in-memory file tree. Its switches let a test make
// it fail in the ways a real node can.
type fakeNode struct {
	name  string
	srv   *httptest.Server
	group *fakeCluster

	mu     sync.Mutex
	files  map[string]fakeFile // by "share/dir/name"
	space  map[string]wire.Space
	down   bool          // every request is answered 503
	status int           // when set, every authorized request is answered with it
	gate   chan struct{} // when set, requests wait until it is closed
	reject int           // this many authorized requests are answered 401
	counts map[string]int
	logins int
	waits  chan struct{} // receives a value whenever a request waits at the gate
}

// ServeHTTP answers one request as the switches of the node say: the request waits at
// the gate, then the node fails it, or it is served.
func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	endpoint := route(r)
	kind := endpoint.kind
	state := n.admit(kind)
	if !n.waitAtGate(r.Context(), state.gate) {
		return
	}
	switch {
	case state.down:
		writeError(w, http.StatusServiceUnavailable)
	case kind == "login":
		n.login(w, r)
	case state.rejected || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-"):
		writeError(w, http.StatusUnauthorized)
	case state.status != 0:
		writeError(w, state.status)
	default:
		n.serve(w, r, endpoint)
	}
}

// requestState is what the switches of a node say about one request.
type requestState struct {
	gate     chan struct{}
	down     bool
	status   int
	rejected bool
}

// admit counts the request and reads the switches that apply to it.
func (n *fakeNode) admit(kind string) requestState {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.counts[kind]++
	state := requestState{
		gate:     n.gate,
		down:     n.down,
		status:   n.status,
		rejected: kind != "login" && n.reject > 0,
	}
	if state.rejected {
		n.reject--
	}
	return state
}

// waitAtGate holds the request while a test holds the node. It reports false when the
// request ended first.
func (n *fakeNode) waitAtGate(ctx context.Context, gate chan struct{}) bool {
	if gate == nil {
		return true
	}
	select {
	case n.waits <- struct{}{}:
	default:
	}
	select {
	case <-gate:
		return true
	case <-ctx.Done():
		return false
	}
}

func (n *fakeNode) url() string { return n.srv.URL + "/api" }

func (n *fakeNode) put(path string, data []byte, mtime time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.files[path] = fakeFile{data: data, mtime: mtime}
}

func (n *fakeNode) remove(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.files, path)
}

func (n *fakeNode) setSpace(share string, total, free uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.space[share] = wire.Space{Total: total, Free: free}
}

func (n *fakeNode) setDown(down bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.down = down
}

func (n *fakeNode) setStatus(status int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.status = status
}

func (n *fakeNode) rejectNext(count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reject = count
}

// hold makes requests wait until the returned function is called.
func (n *fakeNode) hold() func() {
	gate := make(chan struct{})
	n.mu.Lock()
	n.gate = gate
	n.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			n.mu.Lock()
			n.gate = nil
			n.mu.Unlock()
			close(gate)
		})
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

// endpoint is the API endpoint a request addresses, with the request's path argument.
type endpoint struct {
	kind string
	arg  string
}

// route names the endpoint of a request.
func route(r *http.Request) endpoint {
	apiPath := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	switch {
	case apiPath == "login":
		return endpoint{kind: "login"}
	case apiPath == "nodes":
		return endpoint{kind: "nodes"}
	case strings.HasPrefix(apiPath, "tree/"):
		return endpoint{kind: "list", arg: strings.TrimSuffix(strings.TrimPrefix(apiPath, "tree/"), "/")}
	case strings.HasPrefix(apiPath, "space/"):
		return endpoint{kind: "space", arg: strings.TrimPrefix(apiPath, "space/")}
	case strings.HasPrefix(apiPath, "file/") && r.Method == http.MethodHead:
		return endpoint{kind: "head", arg: strings.TrimPrefix(apiPath, "file/")}
	case strings.HasPrefix(apiPath, "file/"):
		return endpoint{kind: "range", arg: strings.TrimPrefix(apiPath, "file/")}
	}
	return endpoint{kind: "other", arg: apiPath}
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request, target endpoint) {
	n.mu.Lock()
	defer n.mu.Unlock()
	arg := target.arg
	switch target.kind {
	case "nodes":
		writeJSON(w, n.group.nodeList())
	case "list":
		n.list(w, arg)
	case "space":
		s, ok := n.space[arg]
		if !ok {
			writeError(w, http.StatusNotFound)
			return
		}
		writeJSON(w, s)
	case "head", "range":
		stored, ok := n.files[arg]
		if !ok {
			writeError(w, http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", stored.etag())
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set(wire.HeaderHeadDigest, stored.digest())
		http.ServeContent(w, r, "", stored.mtime, bytes.NewReader(stored.data))
	default:
		writeError(w, http.StatusNotFound)
	}
}

// list answers with the entries of dir, derived from the paths of the files.
func (n *fakeNode) list(w http.ResponseWriter, dir string) {
	prefix := dir + "/"
	if dir == "" {
		prefix = ""
	}
	entries := make(map[string]wire.Entry)
	for filePath, stored := range n.files {
		rest, ok := strings.CutPrefix(filePath, prefix)
		if !ok {
			continue
		}
		name, _, isDir := strings.Cut(rest, "/")
		if isDir {
			entries[name] = wire.Entry{Name: name, Dir: true, MTime: fileTime()}
		} else {
			entries[name] = wire.Entry{Name: name, Size: int64(len(stored.data)), MTime: stored.mtime, ETag: stored.etag()}
		}
	}
	if len(entries) == 0 && dir != "" {
		writeError(w, http.StatusNotFound)
		return
	}
	listing := wire.Listing{Entries: []wire.Entry{}}
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		listing.Entries = append(listing.Entries, entries[name])
	}
	writeJSON(w, listing)
}

func (n *fakeNode) login(w http.ResponseWriter, r *http.Request) {
	var req wire.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.User != testUser || req.Password != testPassword {
		writeError(w, http.StatusUnauthorized)
		return
	}
	n.mu.Lock()
	n.logins++
	token := fmt.Sprintf("tok-%s-%d", n.name, n.logins)
	n.mu.Unlock()
	writeJSON(w, wire.LoginResponse{Token: token, Expires: n.group.clock.Now().Add(time.Hour)})
}

func writeJSON(w http.ResponseWriter, value any) {
	writeJSONStatus(w, http.StatusOK, value)
}

func writeError(w http.ResponseWriter, status int) {
	writeJSONStatus(w, status, wire.Error{Error: http.StatusText(status)})
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		// The client is gone. Aborting the handler is the way net/http ends it without a log line.
		panic(http.ErrAbortHandler)
	}
}

// fakeCluster is a set of fake nodes that share one node list.
type fakeCluster struct {
	clock *fakeClock
	nodes []*fakeNode
	extra []wire.Node // listed besides the fake nodes
}

func newFakeCluster(t *testing.T, clock *fakeClock, names ...string) *fakeCluster {
	t.Helper()
	fakes := &fakeCluster{clock: clock}
	for _, name := range names {
		n := &fakeNode{
			name:   name,
			group:  fakes,
			files:  make(map[string]fakeFile),
			space:  make(map[string]wire.Space),
			counts: make(map[string]int),
			waits:  make(chan struct{}, 64),
		}
		n.srv = httptest.NewServer(n)
		t.Cleanup(n.srv.Close)
		fakes.nodes = append(fakes.nodes, n)
	}
	return fakes
}

func (fc *fakeCluster) nodeList() wire.Nodes {
	list := wire.Nodes{Cluster: testCluster}
	for _, n := range fc.nodes {
		list.Nodes = append(list.Nodes, wire.Node{Name: n.name, URL: n.url()})
	}
	list.Nodes = append(list.Nodes, fc.extra...)
	return list
}

// put stores a file on every node.
func (fc *fakeCluster) put(path string, data []byte, mtime time.Time) {
	for _, n := range fc.nodes {
		n.put(path, data, mtime)
	}
}

func (fc *fakeCluster) setAllDown() {
	for _, n := range fc.nodes {
		n.setDown(true)
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

// newHarness makes a fake cluster with the named nodes and a configuration with one
// mount of the cluster root; edit may change the configuration before the Client is
// built by start.
func newHarness(t *testing.T, names ...string) *harness {
	t.Helper()
	clock := newFakeClock()
	fakes := newFakeCluster(t, clock, names...)
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
		HTTPClient:   &http.Client{},
		Now:          h.clock.Now,
		ReadPassword: func(string) (string, error) { return testPassword, nil },
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
