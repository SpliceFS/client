package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/fetch"
	"github.com/SpliceFS/client/internal/remote"
	"github.com/SpliceFS/common/units"
)

// testSize spans several blocks and ends in a short one.
const testSize = 3*testBlockSize + 1234

func cacheKey(path string) cache.Key { return cache.Key{Cluster: testCluster, Path: path} }

func TestOpenReadsTheFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	data := pattern(1, testSize)
	h.fc.put("media/file", data, fileTime)

	f, err := h.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if a := f.Attr(); a.Name != "file" || a.Size != testSize || !a.MTime.Equal(fileTime) || a.Inode != inode(testCluster, "media/file", kindFile) {
		t.Errorf("Attr = %+v", a)
	}
	if got := readAll(t, f); !bytes.Equal(got, data) {
		t.Error("read data differs from the file")
	}
	if got := h.fc.count("head"); got != 2 {
		t.Errorf("HEAD requests = %d, want one per node", got)
	}
}

func TestOpenTrustsAFreshCacheEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b", "c")
	data := pattern(1, testSize)
	h.fc.put("media/file", data, fileTime)
	fsys := h.view(0)
	readFile(t, fsys, "media/file")
	heads := h.fc.count("head")

	h.clock.Advance(testRevalid / 2)
	if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, data) {
		t.Error("read data differs from the file")
	}
	if got := h.fc.count("head"); got != heads {
		t.Errorf("an open within revalidate sent %d HEAD requests, want none", got-heads)
	}
}

func TestRestartAsksBeforeTrustingTheCache(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	readFile(t, h.view(0), "media/file")
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	heads := h.fc.count("head")

	h.start()
	readFile(t, h.view(0), "media/file")
	if got := h.fc.count("head"); got != heads+2 {
		t.Errorf("the first open after a restart sent %d HEAD requests, want 2: the nodes holding the version are not known yet", got-heads)
	}
}

func TestHeldVersionsAreBounded(t *testing.T) {
	t.Parallel()
	var h heldVersions
	v := cache.Version{Size: 1, ETag: "x", BlockSize: 1}
	for i := range maxHeld + 1 {
		h.remember(cacheKey(string(rune(i))), v, []string{"a"})
	}
	h.remember(cacheKey("last"), v, []string{"a"})
	if len(h.byFile) != maxHeld {
		t.Errorf("%d versions remembered, want at most %d", len(h.byFile), maxHeld)
	}
	if nodes, ok := h.recall(cacheKey("last"), v); !ok || len(nodes) != 1 {
		t.Errorf("recall of the newest = %v, %v", nodes, ok)
	}
	other := v
	other.HeadDigest = "y"
	if _, ok := h.recall(cacheKey("last"), other); ok {
		t.Error("a different version was recalled")
	}
}

func TestOpenRevalidatesAnOldCacheEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b", "c")
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	fsys := h.view(0)
	readFile(t, fsys, "media/file")
	heads := h.fc.count("head")

	h.clock.Advance(testRevalid + time.Second)
	readFile(t, fsys, "media/file")
	if got := h.fc.count("head"); got != heads+3 {
		t.Errorf("an open after revalidate sent %d HEAD requests, want 3", got-heads)
	}
	info, ok := h.cache(0).Peek(cacheKey("media/file"))
	if !ok || !info.Validated.Equal(h.clock.Now()) {
		t.Errorf("cache entry validated at %v, want %v", info.Validated, h.clock.Now())
	}
	readFile(t, fsys, "media/file")
	if got := h.fc.count("head"); got != heads+3 {
		t.Error("the open after revalidation asked the nodes again")
	}
}

func TestChangedFileGetsANewCacheVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	old := pattern(1, testSize)
	h.fc.put("media/file", old, fileTime)
	fsys := h.view(0)
	readFile(t, fsys, "media/file")
	h.fill(0, "media/file", old)

	changed := pattern(2, testSize)
	changedTime := fileTime.Add(time.Hour)
	h.fc.put("media/file", changed, changedTime)
	h.clock.Advance(testCacheTTL)

	f, err := fsys.Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if !f.Attr().MTime.Equal(changedTime) {
		t.Errorf("opened version has mtime %v, want %v", f.Attr().MTime, changedTime)
	}
	if got := readAll(t, f); !bytes.Equal(got, changed) {
		t.Error("the old cached data was served for the changed file")
	}
	info, _ := h.cache(0).Peek(cacheKey("media/file"))
	if info.Version.ETag != (fakeFile{changed, changedTime}).etag() || !info.Complete {
		t.Fatalf("cache holds %+v, want the new version, complete after the read", info)
	}
	if u := h.cache(0).Usage(); u.Entries != 1 || u.Bytes != testSize {
		t.Errorf("cache usage %+v, want one entry of %d bytes: the old version must be gone", u, testSize)
	}
	if !bytes.Equal(h.stored(0, "media/file"), changed) {
		t.Error("the cache entry holds blocks of the old version")
	}
}

func TestNodeWithAnotherVersionIsNotASource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b", "c")
	majority := pattern(1, testSize)
	h.fc.nodes[0].put("media/file", majority, fileTime)
	h.fc.nodes[1].put("media/file", pattern(9, testSize), fileTime)
	h.fc.nodes[2].put("media/file", majority, fileTime)
	fsys := h.view(0)
	// The odd node looks fastest, so it would be read first if it were a source.
	meter := h.client.clusters[testCluster].meter
	meter.Observe("a", 1<<10, time.Second)
	meter.Observe("b", 1<<30, time.Second)
	meter.Observe("c", 1<<10, time.Second)

	for range 3 {
		if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, majority) {
			t.Fatal("read data is not the version most nodes hold")
		}
	}
	if got := h.fc.nodes[1].count("range"); got != 0 {
		t.Errorf("the node with the other version served %d reads", got)
	}
	info, _ := h.cache(0).Peek(cacheKey("media/file"))
	if want := (fakeFile{majority, fileTime}).digest(); info.Version.HeadDigest != want {
		t.Errorf("cached head digest %s, want the majority's %s", info.Version.HeadDigest, want)
	}
}

func TestOfflineServesCompleteCacheEntries(t *testing.T) {
	t.Parallel()
	for _, offline := range []bool{true, false} {
		h := newHarness(t, "a", "b")
		h.cfg.Mounts[0].Offline = offline
		data := pattern(1, testSize)
		h.fc.put("media/file", data, fileTime)
		// Larger than the reader's switch to block mode, so an isolated read of it stores
		// nothing; one block is then stored by hand.
		large := pattern(3, 40*testBlockSize)
		h.fc.put("media/partial", large, fileTime)
		fsys := h.view(0)
		readFile(t, fsys, "media/file")
		h.fill(0, "media/file", data)
		h.readHead(fsys, "media/partial")
		h.fillBlocks(0, "media/partial", large, 1)
		if info, _ := h.cache(0).Peek(cacheKey("media/partial")); info.Complete || info.Bytes != testBlockSize {
			t.Fatalf("partial entry is %+v, want exactly one block", info)
		}

		h.fc.setAllDown()
		h.clock.Advance(2 * testRevalid)
		if _, err := fsys.Open(context.Background(), "media/partial"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("offline %v: open of a partly cached file = %v, want ErrUnavailable", offline, err)
		}
		f, err := fsys.Open(context.Background(), "media/file")
		if !offline {
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("offline false: open with every node down = %v, want ErrUnavailable", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("offline true: open of a complete cached file: %v", err)
		}
		if got := readAll(t, f); !bytes.Equal(got, data) {
			t.Error("offline true: cached data differs from the file")
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}
}

// readHead opens name, reads a few bytes at its start and closes it.
func (h *harness) readHead(fsys FS, name string) {
	h.t.Helper()
	f, err := fsys.Open(context.Background(), name)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := f.ReadAt(context.Background(), make([]byte, 100), 0); err != nil {
		h.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		h.t.Fatal(err)
	}
}

func TestMinFileSizeBypassesTheCache(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.cfg.Mounts[0].Cache.MinFileSize = units.Size(testSize + 1)
	data := pattern(1, testSize)
	h.fc.put("media/file", data, fileTime)
	fsys := h.view(0)

	for i := 1; i <= 2; i++ {
		if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, data) {
			t.Fatal("read data differs from the file")
		}
		if got := h.fc.count("head"); got != i {
			t.Errorf("open %d: %d HEAD requests in all, want %d", i, got, i)
		}
	}
	if u := h.cache(0).Usage(); u.Entries != 0 {
		t.Errorf("cache has %d entries, want none", u.Entries)
	}
}

func TestBrokenCacheStillReads(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	data := pattern(1, testSize)
	h.fc.put("media/file", data, fileTime)
	fsys := h.view(0)
	if err := h.cache(0).Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, data) {
		t.Error("read data differs from the file")
	}
	if !strings.Contains(h.logs.String(), "cache unusable") {
		t.Errorf("the cache failure was not logged: %s", h.logs.String())
	}
	if err := h.client.Close(); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Close with a cache that failed to close = %v, want cache.ErrClosed", err)
	}
}

func TestFileVanishedFromEveryNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	h.fc.put("media/file", pattern(1, 10), fileTime)
	h.fc.put("media/other", pattern(1, 10), fileTime)
	fsys := h.view(0)
	ctx := context.Background()
	if _, err := fsys.Stat(ctx, "media/file"); err != nil {
		t.Fatal(err)
	}
	for _, n := range h.fc.nodes {
		n.remove("media/file")
	}
	lists := h.fc.count("list")
	if _, err := fsys.Open(ctx, "media/file"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Open of a file every node lost = %v, want ErrNotExist", err)
	}
	if _, err := fsys.Stat(ctx, "media/file"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat after the failed open = %v, want ErrNotExist", err)
	}
	if got := h.fc.count("list"); got != lists+2 {
		t.Errorf("the parent listing was not fetched again: %d new list requests", got-lists)
	}
}

func TestOpenWithEveryNodeDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.cfg.Mounts[0].Cache.MinFileSize = 1 << 30
	h.fc.put("media/file", pattern(1, 10), fileTime)
	fsys := h.view(0)
	if _, err := fsys.Stat(context.Background(), "media/file"); err != nil {
		t.Fatal(err)
	}
	h.fc.setAllDown()
	_, err := fsys.Open(context.Background(), "media/file")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, remote.ErrUnavailable) {
		t.Errorf("Open with every node down = %v, want ErrUnavailable with the node's error", err)
	}
}

func TestCancelledOpenReturnsContextError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, 10), fileTime)
	fsys := h.view(0)
	if _, err := fsys.Stat(context.Background(), "media/file"); err != nil {
		t.Fatal(err)
	}
	node := h.fc.nodes[0]
	release := node.hold()
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fsys.Open(ctx, "media/file")
		done <- err
	}()
	<-node.waits
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Open = %v, want context.Canceled", err)
	}
}

func TestOpenAfterCloseIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, 10), fileTime)
	fsys := h.view(0)
	if _, err := fsys.Stat(context.Background(), "media/file"); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Open(context.Background(), "media/file"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Open after Close = %v, want ErrUnavailable", err)
	}
}

// expectReleased checks that nothing pins the cache entry of path: an entry that is
// not pinned is evicted once it is older than max_age.
func (h *harness) expectReleased(path string) {
	h.t.Helper()
	if _, ok := h.cache(0).Peek(cacheKey(path)); !ok {
		h.t.Fatalf("nothing cached for %s", path)
	}
	h.clock.Advance(2 * testMaxAge)
	if err := h.cache(0).Evict(); err != nil {
		h.t.Fatal(err)
	}
	if _, ok := h.cache(0).Peek(cacheKey(path)); ok {
		h.t.Errorf("cache entry of %s is still pinned", path)
	}
}

const testMaxAge = time.Minute

func TestReaderFailureReleasesTheEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.cfg.Mounts[0].Cache.MaxAge = units.Duration(testMaxAge)
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	fsys := h.view(0)
	failure := errors.New("no reader")
	h.client.newReader = func(fetch.Options, []fetch.Source, fetch.Store, fetch.Meter) (*fetch.Reader, error) {
		return nil, failure
	}
	if _, err := fsys.Open(context.Background(), "media/file"); !errors.Is(err, failure) {
		t.Errorf("Open = %v, want the reader's error", err)
	}
	h.expectReleased("media/file")
}

func TestReadFailsWhenEveryNodeLostTheVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a", "b")
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	f, err := h.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h.fc.put("media/file", pattern(2, testSize), fileTime.Add(time.Hour))
	_, err = f.ReadAt(context.Background(), make([]byte, 100), 0)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, fetch.ErrUnavailable) {
		t.Errorf("ReadAt after every node changed the file = %v, want ErrUnavailable", err)
	}
}

func TestFileClose(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.cfg.Mounts[0].Cache.MaxAge = units.Duration(testMaxAge)
	h.fc.put("media/file", pattern(1, testSize), fileTime)
	f, err := h.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := f.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("second Close = %v, want fs.ErrClosed", err)
	}
	if _, err := f.ReadAt(context.Background(), make([]byte, 10), 0); !errors.Is(err, fetch.ErrClosed) {
		t.Errorf("ReadAt after Close = %v, want fetch.ErrClosed", err)
	}
	h.expectReleased("media/file")
}

func TestSourceFetchErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		gone   bool
	}{
		{http.StatusPreconditionFailed, true},
		{http.StatusNotFound, true},
		{http.StatusForbidden, true},
		{http.StatusServiceUnavailable, false},
		{0, false},
	} {
		h := newHarness(t, "a")
		h.fc.put("media/file", pattern(1, 100), fileTime)
		h.view(0)
		cl := h.client.clusters[testCluster]
		if _, _, err := cl.nodes(context.Background()); err != nil {
			t.Fatal(err)
		}
		node := h.fc.nodes[0]
		node.setStatus(tc.status)
		src := source{c: cl, node: cl.node("a"), file: "media/file", etag: (fakeFile{pattern(1, 100), fileTime}).etag()}
		buf := make([]byte, 50)
		err := src.Fetch(context.Background(), 10, buf)
		switch {
		case tc.status == 0:
			if err != nil || !bytes.Equal(buf, pattern(1, 100)[10:60]) || src.ID() != "a" {
				t.Errorf("Fetch = %v, data %v", err, buf)
			}
		case errors.Is(err, fetch.ErrGone) != tc.gone || err == nil:
			t.Errorf("HTTP %d: Fetch = %v, gone %v", tc.status, err, tc.gone)
		}
	}
}

func TestChooseVersion(t *testing.T) {
	t.Parallel()
	nodes := make([]*remote.Node, 4)
	for i := range nodes {
		n, err := remote.New(string(rune('a'+i)), "http://192.0.2.1/api", &http.Client{})
		if err != nil {
			t.Fatal(err)
		}
		nodes[i] = n
	}
	meta := func(etag, digest string) remote.Meta { return remote.Meta{ETag: etag, HeadDigest: digest} }
	ok := func(i int, m remote.Meta) headAnswer { return headAnswer{node: nodes[i], meta: m} }
	fail := func(i int, err error) headAnswer { return headAnswer{node: nodes[i], err: err} }
	down := errors.New("connection refused")
	notFound := remote.ErrNotFound

	for _, tc := range []struct {
		name    string
		answers []headAnswer
		tree    string
		want    remote.Meta
		nodes   []int
		err     error
	}{
		{"largest group", []headAnswer{ok(0, meta("x", "1")), ok(1, meta("y", "1")), ok(2, meta("y", "1"))}, "x", meta("y", "1"), []int{1, 2}, nil},
		{"same tag, other digest", []headAnswer{ok(0, meta("x", "1")), ok(1, meta("x", "2")), ok(2, meta("x", "1"))}, "x", meta("x", "1"), []int{0, 2}, nil},
		{"tie goes to the tree's tag", []headAnswer{ok(0, meta("y", "1")), ok(1, meta("x", "1"))}, "x", meta("x", "1"), []int{1}, nil},
		{"tie goes to the greatest tag", []headAnswer{ok(0, meta("y", "1")), ok(1, meta("z", "1"))}, "x", meta("z", "1"), []int{1}, nil},
		{"tie goes to the greatest digest", []headAnswer{ok(0, meta("x", "2")), ok(1, meta("x", "1"))}, "x", meta("x", "2"), []int{0}, nil},
		{"answers beat silence", []headAnswer{fail(0, down), fail(1, notFound), ok(2, meta("x", "1"))}, "x", meta("x", "1"), []int{2}, nil},
		{"every answer is not found", []headAnswer{fail(0, notFound), fail(1, down)}, "x", remote.Meta{}, nil, errMissing},
		{"no answer", []headAnswer{fail(0, down)}, "x", remote.Meta{}, nil, down},
		{"nobody asked", nil, "x", remote.Meta{}, nil, errNoAnswer},
	} {
		g, err := chooseVersion(tc.answers, tc.tree)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: error %v, want %v", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || g.meta != tc.want || len(g.nodes) != len(tc.nodes) {
			t.Errorf("%s: chose %+v from %d nodes, %v; want %+v from %v", tc.name, g.meta, len(g.nodes), err, tc.want, tc.nodes)
			continue
		}
		for i, n := range tc.nodes {
			if g.nodes[i] != nodes[n] {
				t.Errorf("%s: node %d is %s, want %s", tc.name, i, g.nodes[i].Name(), nodes[n].Name())
			}
		}
	}
}

func TestReadAtEndOfFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, 10), fileTime)
	f, err := h.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 20)
	n, err := f.ReadAt(context.Background(), buf, 4)
	if n != 6 || !errors.Is(err, io.EOF) || err != io.EOF { //nolint:errorlint // io.ReaderAt callers compare io.EOF directly
		t.Errorf("ReadAt across the end = %d, %v; want 6, io.EOF unwrapped", n, err)
	}
}
