package tree

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SpliceFS/client/internal/wire"
)

var (
	errGone = errors.New("no such directory")
	errDown = errors.New("node down")
)

var start = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// at returns a time n minutes after start.
func at(n int) time.Time { return start.Add(time.Duration(n) * time.Minute) }

func wfile(name string, size int64, mtime time.Time, etag string) wire.Entry {
	return wire.Entry{Name: name, Size: size, MTime: mtime, ETag: etag}
}

func wdir(name string, mtime time.Time) wire.Entry {
	return wire.Entry{Name: name, Dir: true, MTime: mtime}
}

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

// answer is what one node says about one directory.
type answer struct {
	entries []wire.Entry
	err     error
}

// fakeLister answers from a script of node, then directory. A node or directory
// that is not in the script answers "no such directory".
type fakeLister struct {
	mu       sync.Mutex
	nodes    []string
	nodesErr error
	script   map[string]map[string]answer
	listFn   func(ctx context.Context, node, dir string) ([]wire.Entry, error)
	calls    []string // "node dir", in the order the calls started
}

func newLister(nodes ...string) *fakeLister {
	return &fakeLister{nodes: nodes, script: make(map[string]map[string]answer)}
}

// say scripts what node answers for dir.
func (f *fakeLister) say(node, dir string, entries ...wire.Entry) {
	f.set(node, dir, answer{entries: entries})
}

func (f *fakeLister) fail(node, dir string, err error) {
	f.set(node, dir, answer{err: err})
}

func (f *fakeLister) set(node, dir string, a answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.script[node] == nil {
		f.script[node] = make(map[string]answer)
	}
	f.script[node][dir] = a
}

func (f *fakeLister) Nodes(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.nodes), f.nodesErr
}

func (f *fakeLister) List(ctx context.Context, node, dir string) ([]wire.Entry, error) {
	f.mu.Lock()
	f.calls = append(f.calls, node+" "+dir)
	fn := f.listFn
	a, ok := f.script[node][dir]
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, node, dir)
	}
	if !ok {
		return nil, errGone
	}
	return slices.Clone(a.entries), a.err
}

// asked is how many times any node was asked for dir.
func (f *fakeLister) asked(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if _, d, _ := cutSpace(c); d == dir {
			n++
		}
	}
	return n
}

func (f *fakeLister) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func cutSpace(s string) (before, after string, ok bool) {
	for i := range len(s) {
		if s[i] == ' ' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func newTree(l Lister, opts Options) (*Tree, *clock) {
	clk := &clock{now: start}
	opts.Now = clk.Now
	opts.IsNotFound = func(err error) bool { return errors.Is(err, errGone) }
	return New(l, opts), clk
}

func mustList(t *testing.T, tr *Tree, dir string) []Entry {
	t.Helper()
	got, err := tr.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List(%q): %v", dir, err)
	}
	return got
}

func entryNames(entries []Entry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}

func checkNames(t *testing.T, what string, got []Entry, want ...string) {
	t.Helper()
	if names := entryNames(got); !slices.Equal(names, want) {
		t.Errorf("%s = %v, want %v", what, names, want)
	}
}

func equalEntry(a, b Entry) bool {
	return a.Name == b.Name && a.Dir == b.Dir && a.Size == b.Size && a.MTime.Equal(b.MTime) &&
		a.ETag == b.ETag && slices.Equal(a.Nodes, b.Nodes)
}

func checkEntries(t *testing.T, got, want []Entry) {
	t.Helper()
	if !slices.EqualFunc(got, want, equalEntry) {
		t.Errorf("entries =\n%+v\nwant\n%+v", got, want)
	}
}
