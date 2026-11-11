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

	"github.com/BlueRival/splicefs/client/config"
	"github.com/BlueRival/splicefs/client/internal/cache"
	"github.com/BlueRival/splicefs/client/internal/fetch"
	"github.com/BlueRival/splicefs/internal/units"
	"github.com/BlueRival/splicefs/internal/wire"
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

var fileTime = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

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
func (n *fakeNode) hold() (release func()) {
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

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind, arg := route(r)
	n.mu.Lock()
	n.counts[kind]++
	gate, down, status := n.gate, n.down, n.status
	rejected := kind != "login" && n.reject > 0
	if rejected {
		n.reject--
	}
	n.mu.Unlock()
	if gate != nil {
		select {
		case n.waits <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case down:
		writeError(w, http.StatusServiceUnavailable)
	case kind == "login":
		n.login(w, r)
	case rejected || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-"):
		writeError(w, http.StatusUnauthorized)
	case status != 0:
		writeError(w, status)
	default:
		n.serve(w, r, kind, arg)
	}
}

// route names the endpoint of a request and returns its path argument.
func route(r *http.Request) (kind, arg string) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	switch {
	case p == "login":
		return "login", ""
	case p == "nodes":
		return "nodes", ""
	case strings.HasPrefix(p, "tree/"):
		return "list", strings.TrimSuffix(strings.TrimPrefix(p, "tree/"), "/")
	case strings.HasPrefix(p, "space/"):
		return "space", strings.TrimPrefix(p, "space/")
	case strings.HasPrefix(p, "file/") && r.Method == http.MethodHead:
		return "head", strings.TrimPrefix(p, "file/")
	case strings.HasPrefix(p, "file/"):
		return "range", strings.TrimPrefix(p, "file/")
	}
	return "other", p
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request, kind, arg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch kind {
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
		f, ok := n.files[arg]
		if !ok {
			writeError(w, http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", f.etag())
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set(wire.HeaderHeadDigest, f.digest())
		http.ServeContent(w, r, "", f.mtime, bytes.NewReader(f.data))
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
	for p, f := range n.files {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		name, _, isDir := strings.Cut(rest, "/")
		if isDir {
			entries[name] = wire.Entry{Name: name, Dir: true, MTime: fileTime}
		} else {
			entries[name] = wire.Entry{Name: name, Size: int64(len(f.data)), MTime: f.mtime, ETag: f.etag()}
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wire.Error{Error: http.StatusText(status)})
}

// fakeCluster is a set of fake nodes that share one node list.
type fakeCluster struct {
	clock *fakeClock
	nodes []*fakeNode
	extra []wire.Node // listed besides the fake nodes
}

func newFakeCluster(t *testing.T, clock *fakeClock, names ...string) *fakeCluster {
	t.Helper()
	fc := &fakeCluster{clock: clock}
	for _, name := range names {
		n := &fakeNode{
			name:   name,
			group:  fc,
			files:  make(map[string]fakeFile),
			space:  make(map[string]wire.Space),
			counts: make(map[string]int),
			waits:  make(chan struct{}, 64),
		}
		n.srv = httptest.NewServer(n)
		t.Cleanup(n.srv.Close)
		fc.nodes = append(fc.nodes, n)
	}
	return fc
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
	fc := newFakeCluster(t, clock, names...)
	c := testCache(t.TempDir())
	cfg := &config.Config{
		Cache: c,
		Clusters: map[string]config.Cluster{testCluster: {
			URL:            fc.nodes[0].url(),
			User:           testUser,
			PasswordFile:   "password",
			BlockSize:      testBlockSize,
			StreamsPerNode: 2,
			ReadAhead:      4 * testBlockSize,
		}},
		Mounts: []config.Mount{{Cluster: testCluster, Path: "/", Target: "/mnt/files", Offline: true, Cache: c}},
	}
	return &harness{t: t, clock: clock, fc: fc, cfg: cfg, logs: &syncBuffer{}}
}

// addMount adds a mount of path with the top-level cache settings.
func (h *harness) addMount(path, target string) {
	h.cfg.Mounts = append(h.cfg.Mounts, config.Mount{Cluster: testCluster, Path: path, Target: target, Offline: true, Cache: h.cfg.Cache})
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
	c, err := newClient(h.cfg, h.options(), h.clock)
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	h.client = c
}

// view returns the FS of mount i, starting the Client if needed.
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
	key := cache.Key{Cluster: testCluster, Path: path}
	info, ok := h.cache(i).Peek(key)
	if !ok {
		h.t.Fatalf("nothing cached for %s", path)
	}
	e, err := h.cache(i).Acquire(key, info.Version)
	if err != nil {
		h.t.Fatal(err)
	}
	for b := int64(0); b*testBlockSize < int64(len(data)); b++ {
		block := data[b*testBlockSize : min(int64(len(data)), (b+1)*testBlockSize)]
		if err := e.WriteBlock(b, block); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := e.Release(); err != nil {
		h.t.Fatal(err)
	}
}

// readFile opens name and reads it to the end in small pieces.
func readFile(t *testing.T, fsys FS, name string) []byte {
	t.Helper()
	f, err := fsys.Open(context.Background(), name)
	if err != nil {
		t.Fatalf("Open %q: %v", name, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close %q: %v", name, err)
		}
	}()
	return readAll(t, f)
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
