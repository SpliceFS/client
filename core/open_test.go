package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"slices"
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
	env := newHarness(t, "a", "b")
	data := pattern(1, testSize)
	env.fc.put("media/file", data, fileTime())

	opened, err := env.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFile(t, opened)
	attr := opened.Attr()
	if attr.Name != "file" || attr.Size != testSize || !attr.MTime.Equal(fileTime()) ||
		attr.Inode != inode(testCluster, "media/file", kindFile) {
		t.Errorf("Attr = %+v", attr)
	}
	if got := readAll(t, opened); !bytes.Equal(got, data) {
		t.Error("read data differs from the file")
	}
	if got := env.fc.count("head"); got != 2 {
		t.Errorf("HEAD requests = %d, want one per node", got)
	}
}

func TestOpenTrustsAFreshCacheEntry(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b", "c")
	data := pattern(1, testSize)
	env.fc.put("media/file", data, fileTime())
	fsys := env.view(0)
	readFile(t, fsys, "media/file")
	heads := env.fc.count("head")

	env.clock.Advance(testRevalid / 2)
	if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, data) {
		t.Error("read data differs from the file")
	}
	if got := env.fc.count("head"); got != heads {
		t.Errorf("an open within revalidate sent %d HEAD requests, want none", got-heads)
	}
}

func TestRestartAsksBeforeTrustingTheCache(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	readFile(t, env.view(0), "media/file")
	if err := env.client.Close(); err != nil {
		t.Fatal(err)
	}
	heads := env.fc.count("head")

	env.start()
	readFile(t, env.view(0), "media/file")
	if got := env.fc.count("head"); got != heads+2 {
		t.Errorf("the first open after a restart sent %d HEAD requests, want 2: "+
			"the nodes holding the version are not known yet", got-heads)
	}
}

func TestHeldVersionsAreBounded(t *testing.T) {
	t.Parallel()
	var versions heldVersions
	version := cache.Version{Size: 1, ETag: "x", BlockSize: 1}
	for i := range maxHeld + 1 {
		versions.remember(cacheKey(string(rune(i))), version, []string{"a"})
	}
	versions.remember(cacheKey("last"), version, []string{"a"})
	if len(versions.byFile) != maxHeld {
		t.Errorf("%d versions remembered, want at most %d", len(versions.byFile), maxHeld)
	}
	if nodes, ok := versions.recall(cacheKey("last"), version); !ok || len(nodes) != 1 {
		t.Errorf("recall of the newest = %v, %v", nodes, ok)
	}
	other := version
	other.HeadDigest = "y"
	if _, ok := versions.recall(cacheKey("last"), other); ok {
		t.Error("a different version was recalled")
	}
	other = version
	other.TailDigest = "z"
	if _, ok := versions.recall(cacheKey("last"), other); ok {
		t.Error("a version with another tail digest was recalled")
	}
}

func TestOpenRevalidatesAnOldCacheEntry(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b", "c")
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	fsys := env.view(0)
	readFile(t, fsys, "media/file")
	heads := env.fc.count("head")

	env.clock.Advance(testRevalid + time.Second)
	readFile(t, fsys, "media/file")
	if got := env.fc.count("head"); got != heads+3 {
		t.Errorf("an open after revalidate sent %d HEAD requests, want 3", got-heads)
	}
	info, ok := env.cache(0).Peek(cacheKey("media/file"))
	if !ok || !info.Validated.Equal(env.clock.Now()) {
		t.Errorf("cache entry validated at %v, want %v", info.Validated, env.clock.Now())
	}
	readFile(t, fsys, "media/file")
	if got := env.fc.count("head"); got != heads+3 {
		t.Error("the open after revalidation asked the nodes again")
	}
}

func TestChangedFileGetsANewCacheVersion(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	old := pattern(1, testSize)
	env.fc.put("media/file", old, fileTime())
	fsys := env.view(0)
	readFile(t, fsys, "media/file")
	env.fill(0, "media/file", old)

	changed := pattern(2, testSize)
	changedTime := fileTime().Add(time.Hour)
	env.fc.put("media/file", changed, changedTime)
	env.clock.Advance(testCacheTTL)

	opened, err := fsys.Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFile(t, opened)
	if !opened.Attr().MTime.Equal(changedTime) {
		t.Errorf("opened version has mtime %v, want %v", opened.Attr().MTime, changedTime)
	}
	if got := readAll(t, opened); !bytes.Equal(got, changed) {
		t.Error("the old cached data was served for the changed file")
	}
	info, _ := env.cache(0).Peek(cacheKey("media/file"))
	if info.Version.ETag != (fakeFile{changed, changedTime}).etag() || !info.Complete {
		t.Fatalf("cache holds %+v, want the new version, complete after the read", info)
	}
	if u := env.cache(0).Usage(); u.Entries != 1 || u.Bytes != testSize {
		t.Errorf("cache usage %+v, want one entry of %d bytes: the old version must be gone", u, testSize)
	}
	if !bytes.Equal(env.stored(0, "media/file"), changed) {
		t.Error("the cache entry holds blocks of the old version")
	}
}

func TestNodeWithAnotherVersionIsNotASource(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b", "c")
	majority := pattern(1, testSize)
	env.fc.nodes[0].put("media/file", majority, fileTime())
	env.fc.nodes[1].put("media/file", pattern(9, testSize), fileTime())
	env.fc.nodes[2].put("media/file", majority, fileTime())
	fsys := env.view(0)
	// The odd node looks fastest, so it would be read first if it were a source.
	meter := env.client.clusters[testCluster].meter
	meter.Observe("a", 1<<10, time.Second)
	meter.Observe("b", 1<<30, time.Second)
	meter.Observe("c", 1<<10, time.Second)

	for range 3 {
		if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, majority) {
			t.Fatal("read data is not the version most nodes hold")
		}
	}
	if got := env.fc.nodes[1].count("range"); got != 0 {
		t.Errorf("the node with the other version served %d reads", got)
	}
	info, _ := env.cache(0).Peek(cacheKey("media/file"))
	if want := (fakeFile{majority, fileTime()}).digest(); info.Version.HeadDigest != want {
		t.Errorf("cached head digest %s, want the majority's %s", info.Version.HeadDigest, want)
	}
	if want := (fakeFile{majority, fileTime()}).tailDigest(); info.Version.TailDigest != want {
		t.Errorf("cached tail digest %s, want the majority's %s", info.Version.TailDigest, want)
	}
}

// A copy with the same size, time and first block but another end is another version: it
// must serve no block, or a read could mix the two ends.
func TestNodeWithAnotherTailIsNotASource(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b", "c")
	majority := pattern(1, testSize)
	odd := bytes.Clone(majority)
	odd[len(odd)-1] ^= 0xff
	env.fc.nodes[0].put("media/file", majority, fileTime())
	env.fc.nodes[1].put("media/file", odd, fileTime())
	env.fc.nodes[2].put("media/file", majority, fileTime())
	fsys := env.view(0)
	meter := env.client.clusters[testCluster].meter
	meter.Observe("a", 1<<10, time.Second)
	meter.Observe("b", 1<<30, time.Second)
	meter.Observe("c", 1<<10, time.Second)

	for range 3 {
		if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, majority) {
			t.Fatal("read data is not the version most nodes hold")
		}
	}
	if got := env.fc.nodes[1].count("range"); got != 0 {
		t.Errorf("the node with the other tail served %d reads", got)
	}
	info, _ := env.cache(0).Peek(cacheKey("media/file"))
	if want := (fakeFile{majority, fileTime()}).tailDigest(); info.Version.TailDigest != want {
		t.Errorf("cached tail digest %s, want the majority's %s", info.Version.TailDigest, want)
	}
}

// cachedFilesWithNodesDown caches a complete file and a partly cached one, then takes every
// node down and lets the cache entries age past the revalidation time.
func cachedFilesWithNodesDown(t *testing.T, offline bool) *harness {
	t.Helper()
	env := newHarness(t, "a", "b")
	env.cfg.Mounts[0].Offline = offline
	data := pattern(1, testSize)
	env.fc.put("media/file", data, fileTime())
	// Larger than the reader's switch to block mode, so an isolated read of it stores
	// nothing; one block is then stored by hand.
	large := pattern(3, 40*testBlockSize)
	env.fc.put("media/partial", large, fileTime())
	fsys := env.view(0)
	readFile(t, fsys, "media/file")
	env.fill(0, "media/file", data)
	env.readHead(fsys, "media/partial")
	env.fillBlocks(0, "media/partial", large, 1)
	if info, _ := env.cache(0).Peek(cacheKey("media/partial")); info.Complete || info.Bytes != testBlockSize {
		t.Fatalf("partial entry is %+v, want exactly one block", info)
	}

	env.fc.setAllDown()
	env.clock.Advance(2 * testRevalid)
	return env
}

func TestOfflineMountServesCompleteCacheEntries(t *testing.T) {
	t.Parallel()
	env := cachedFilesWithNodesDown(t, true)
	fsys := env.view(0)
	data := pattern(1, testSize)

	if _, err := fsys.Open(context.Background(), "media/partial"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("open of a partly cached file = %v, want ErrUnavailable", err)
	}
	opened, err := fsys.Open(context.Background(), "media/file")
	if err != nil {
		t.Fatalf("open of a complete cached file: %v", err)
	}
	defer closeFile(t, opened)
	if got := readAll(t, opened); !bytes.Equal(got, data) {
		t.Error("cached data differs from the file")
	}
}

func TestOnlineMountNeedsNodesForCachedFiles(t *testing.T) {
	t.Parallel()
	env := cachedFilesWithNodesDown(t, false)
	fsys := env.view(0)

	for _, name := range []string{"media/partial", "media/file"} {
		if _, err := fsys.Open(context.Background(), name); !errors.Is(err, ErrUnavailable) {
			t.Errorf("open of %s with every node down = %v, want ErrUnavailable", name, err)
		}
	}
}

// readHead opens name, reads a few bytes at its start and closes it.
func (h *harness) readHead(fsys FS, name string) {
	h.t.Helper()
	opened, err := fsys.Open(context.Background(), name)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := opened.ReadAt(context.Background(), make([]byte, 100), 0); err != nil {
		h.t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		h.t.Fatal(err)
	}
}

func TestMinFileSizeBypassesTheCache(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.cfg.Mounts[0].Cache.MinFileSize = units.Size(testSize + 1)
	data := pattern(1, testSize)
	env.fc.put("media/file", data, fileTime())
	fsys := env.view(0)

	for i := 1; i <= 2; i++ {
		if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, data) {
			t.Fatal("read data differs from the file")
		}
		if got := env.fc.count("head"); got != i {
			t.Errorf("open %d: %d HEAD requests in all, want %d", i, got, i)
		}
	}
	if u := env.cache(0).Usage(); u.Entries != 0 {
		t.Errorf("cache has %d entries, want none", u.Entries)
	}
}

func TestBrokenCacheStillReads(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	data := pattern(1, testSize)
	env.fc.put("media/file", data, fileTime())
	fsys := env.view(0)
	if err := env.cache(0).Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, fsys, "media/file"); !bytes.Equal(got, data) {
		t.Error("read data differs from the file")
	}
	if !strings.Contains(env.logs.String(), "cache unusable") {
		t.Errorf("the cache failure was not logged: %s", env.logs.String())
	}
	if err := env.client.Close(); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Close with a cache that failed to close = %v, want cache.ErrClosed", err)
	}
}

func TestFileVanishedFromEveryNode(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	env.fc.put("media/other", pattern(1, 10), fileTime())
	fsys := env.view(0)
	ctx := context.Background()
	if _, err := fsys.Stat(ctx, "media/file"); err != nil {
		t.Fatal(err)
	}
	for _, n := range env.fc.nodes {
		n.remove("media/file")
	}
	lists := env.fc.count("list")
	if _, err := fsys.Open(ctx, "media/file"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Open of a file every node lost = %v, want ErrNotExist", err)
	}
	if _, err := fsys.Stat(ctx, "media/file"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat after the failed open = %v, want ErrNotExist", err)
	}
	// The listing still showed the file, so the Stat above can only report it gone if the
	// nodes were asked again; the count states that directly. It is not compared with the
	// number of nodes: a node that failed earlier cools down, and the frozen test clock
	// never ends that, so how many nodes take part is not what this test is about.
	if got := env.fc.count("list"); got == lists {
		t.Error("the parent listing was not fetched again: no new list request")
	}
}

func TestOpenWithEveryNodeDown(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.cfg.Mounts[0].Cache.MinFileSize = 1 << 30
	env.fc.put("media/file", pattern(1, 10), fileTime())
	fsys := env.view(0)
	if _, err := fsys.Stat(context.Background(), "media/file"); err != nil {
		t.Fatal(err)
	}
	env.fc.setAllDown()
	_, err := fsys.Open(context.Background(), "media/file")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, remote.ErrUnavailable) {
		t.Errorf("Open with every node down = %v, want ErrUnavailable with the node's error", err)
	}
}

func TestCancelledOpenReturnsContextError(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	fsys := env.view(0)
	if _, err := fsys.Stat(context.Background(), "media/file"); err != nil {
		t.Fatal(err)
	}
	node := env.fc.nodes[0]
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
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	fsys := env.view(0)
	if _, err := fsys.Stat(context.Background(), "media/file"); err != nil {
		t.Fatal(err)
	}
	if err := env.client.Close(); err != nil {
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
	env := newHarness(t, "a")
	env.cfg.Mounts[0].Cache.MaxAge = units.Duration(testMaxAge)
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	fsys := env.view(0)
	failure := errors.New("no reader")
	env.client.newReader = func(fetch.Options, []fetch.Source, fetch.Store, fetch.Meter) (*fetch.Reader, error) {
		return nil, failure
	}
	if _, err := fsys.Open(context.Background(), "media/file"); !errors.Is(err, failure) {
		t.Errorf("Open = %v, want the reader's error", err)
	}
	env.expectReleased("media/file")
}

func TestReadFailsWhenEveryNodeLostTheVersion(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	opened, err := env.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFile(t, opened)
	env.fc.put("media/file", pattern(2, testSize), fileTime().Add(time.Hour))
	_, err = opened.ReadAt(context.Background(), make([]byte, 100), 0)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, fetch.ErrUnavailable) {
		t.Errorf("ReadAt after every node changed the file = %v, want ErrUnavailable", err)
	}
}

func TestFileClose(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.cfg.Mounts[0].Cache.MaxAge = units.Duration(testMaxAge)
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	opened, err := env.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := opened.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("second Close = %v, want fs.ErrClosed", err)
	}
	if _, err := opened.ReadAt(context.Background(), make([]byte, 10), 0); !errors.Is(err, fetch.ErrClosed) {
		t.Errorf("ReadAt after Close = %v, want fetch.ErrClosed", err)
	}
	env.expectReleased("media/file")
}

func TestFileCloseReportsAFailedRelease(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.fc.put("media/file", pattern(1, testSize), fileTime())
	opened, err := env.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.client.Close(); err != nil {
		t.Fatal(err)
	}
	// The cache is closed, so the entry can no longer be released.
	if err := opened.Close(); !errors.Is(err, cache.ErrClosed) || !strings.Contains(err.Error(), "release cache entry") {
		t.Errorf("Close after the cache closed = %v, want a release error wrapping cache.ErrClosed", err)
	}
}

func TestSourceFetchErrors(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		status int
		gone   bool
	}{
		{http.StatusPreconditionFailed, true},
		{http.StatusNotFound, true},
		{http.StatusForbidden, true},
		{http.StatusServiceUnavailable, false},
		{0, false},
	} {
		env := newHarness(t, "a")
		env.fc.put("media/file", pattern(1, 100), fileTime())
		env.view(0)
		clus := env.client.clusters[testCluster]
		if _, err := clus.nodes(context.Background()); err != nil {
			t.Fatal(err)
		}
		node := env.fc.nodes[0]
		node.setStatus(testCase.status)
		src := source{c: clus, node: clus.node("a"), file: "media/file", etag: (fakeFile{pattern(1, 100), fileTime()}).etag()}
		buf := make([]byte, 50)
		err := src.Fetch(context.Background(), 10, buf)
		if testCase.status == 0 {
			if err != nil || !bytes.Equal(buf, pattern(1, 100)[10:60]) || src.ID() != "a" {
				t.Errorf("Fetch = %v, data %v", err, buf)
			}
			continue
		}
		if errors.Is(err, fetch.ErrGone) != testCase.gone || err == nil {
			t.Errorf("HTTP %d: Fetch = %v, gone %v", testCase.status, err, testCase.gone)
		}
	}
}

// answerSet builds the HEAD answers of four nodes named a to d.
type answerSet struct {
	nodes []*remote.Node
}

func newAnswerSet(t *testing.T) answerSet {
	t.Helper()
	nodes := make([]*remote.Node, 4)
	for i := range nodes {
		node, err := remote.New(string(rune('a'+i)), "http://192.0.2.1/api", &http.Client{})
		if err != nil {
			t.Fatal(err)
		}
		nodes[i] = node
	}
	return answerSet{nodes: nodes}
}

// ok is the answer of node i holding the version with the given tag, head digest and
// tail digest.
func (set answerSet) ok(i int, etag, head, tail string) headAnswer {
	return headAnswer{node: set.nodes[i], meta: remote.Meta{ETag: etag, HeadDigest: head, TailDigest: tail}}
}

// fail is the answer of node i failing with err.
func (set answerSet) fail(i int, err error) headAnswer {
	return headAnswer{node: set.nodes[i], err: err}
}

func TestChooseVersionPicksTheLargestGroup(t *testing.T) {
	t.Parallel()
	set := newAnswerSet(t)
	ok, fail := set.ok, set.fail
	down := errors.New("connection refused")

	for _, testCase := range []struct {
		name    string
		answers []headAnswer
		tree    string
		want    remote.Meta
		nodes   []string
	}{
		{
			"largest group",
			[]headAnswer{ok(0, "x", "1", "1"), ok(1, "y", "1", "1"), ok(2, "y", "1", "1")},
			"x",
			remote.Meta{ETag: "y", HeadDigest: "1", TailDigest: "1"},
			[]string{"b", "c"},
		},
		{
			"same tag, other head digest",
			[]headAnswer{ok(0, "x", "1", "1"), ok(1, "x", "2", "1"), ok(2, "x", "1", "1")},
			"x",
			remote.Meta{ETag: "x", HeadDigest: "1", TailDigest: "1"},
			[]string{"a", "c"},
		},
		{
			"tie goes to the tree's tag",
			[]headAnswer{ok(0, "y", "1", "1"), ok(1, "x", "1", "1")},
			"x",
			remote.Meta{ETag: "x", HeadDigest: "1", TailDigest: "1"},
			[]string{"b"},
		},
		{
			"tie goes to the greatest tag",
			[]headAnswer{ok(0, "y", "1", "1"), ok(1, "z", "1", "1")},
			"x",
			remote.Meta{ETag: "z", HeadDigest: "1", TailDigest: "1"},
			[]string{"b"},
		},
		{
			"tie goes to the greatest head digest",
			[]headAnswer{ok(0, "x", "2", "1"), ok(1, "x", "1", "1")},
			"x",
			remote.Meta{ETag: "x", HeadDigest: "2", TailDigest: "1"},
			[]string{"a"},
		},
		{
			"same tag and head, other tail",
			[]headAnswer{ok(0, "x", "1", "1"), ok(1, "x", "1", "2"), ok(2, "x", "1", "1")},
			"x",
			remote.Meta{ETag: "x", HeadDigest: "1", TailDigest: "1"},
			[]string{"a", "c"},
		},
		{
			"tie goes to the greatest head digest before the tail digest",
			[]headAnswer{ok(0, "x", "1", "9"), ok(1, "x", "2", "1")},
			"x",
			remote.Meta{ETag: "x", HeadDigest: "2", TailDigest: "1"},
			[]string{"b"},
		},
		{
			"tie goes to the greatest tail digest",
			[]headAnswer{ok(0, "x", "1", "1"), ok(1, "x", "1", "2")},
			"x",
			remote.Meta{ETag: "x", HeadDigest: "1", TailDigest: "2"},
			[]string{"b"},
		},
		{
			"answers beat silence",
			[]headAnswer{fail(0, down), fail(1, remote.ErrNotFound), ok(2, "x", "1", "1")},
			"x",
			remote.Meta{ETag: "x", HeadDigest: "1", TailDigest: "1"},
			[]string{"c"},
		},
	} {
		group, err := chooseVersion(testCase.answers, testCase.tree)
		if err != nil || group.meta != testCase.want || !slices.Equal(names(group.nodes), testCase.nodes) {
			t.Errorf("%s: chose %+v from %v, %v; want %+v from %v",
				testCase.name, group.meta, names(group.nodes), err, testCase.want, testCase.nodes)
		}
	}
}

func TestChooseVersionFailures(t *testing.T) {
	t.Parallel()
	set := newAnswerSet(t)
	fail := set.fail
	down := errors.New("connection refused")

	for _, testCase := range []struct {
		name    string
		answers []headAnswer
		want    error
	}{
		{"every answer is not found", []headAnswer{fail(0, remote.ErrNotFound), fail(1, down)}, errMissing},
		{"no answer", []headAnswer{fail(0, down)}, down},
		{"nobody asked", nil, errNoAnswer},
	} {
		if _, err := chooseVersion(testCase.answers, "x"); !errors.Is(err, testCase.want) {
			t.Errorf("%s: error %v, want %v", testCase.name, err, testCase.want)
		}
	}
}

func TestReadAtEndOfFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.fc.put("media/file", pattern(1, 10), fileTime())
	opened, err := h.view(0).Open(context.Background(), "media/file")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFile(t, opened)
	buf := make([]byte, 20)
	n, err := opened.ReadAt(context.Background(), buf, 4)
	if n != 6 || err != io.EOF { //nolint:errorlint // io.ReaderAt callers compare io.EOF directly
		t.Errorf("ReadAt across the end = %d, %v; want 6, io.EOF unwrapped", n, err)
	}
}
