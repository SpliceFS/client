package tree

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	o := New(newLister(), Options{}).opts
	if o.NegativeTTL != 5*time.Second || o.MaxDirs != 10000 || o.NodeTimeout != 15*time.Second {
		t.Errorf("defaults = %v, %d, %v, want 5s, 10000, 15s", o.NegativeTTL, o.MaxDirs, o.NodeTimeout)
	}
	if o.Now == nil || o.IsNotFound(errGone) {
		t.Error("Now must be set and IsNotFound must report false")
	}
	kept := New(newLister(), Options{NegativeTTL: time.Second, MaxDirs: 3, NodeTimeout: time.Minute}).opts
	if kept.NegativeTTL != time.Second || kept.MaxDirs != 3 || kept.NodeTimeout != time.Minute {
		t.Error("explicit options were overridden")
	}
}

func TestAnErrorIsASilentNodeUnlessItIsNotFound(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	l.fail("n1", "", errGone)
	tr := New(l, Options{})
	if _, err := tr.List(context.Background(), ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("List = %v, want ErrUnavailable when IsNotFound is not set", err)
	}
}

func TestNotFoundRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(l *fakeLister)
		want  error
	}{
		{"every node says not found", func(*fakeLister) {}, ErrNotFound},
		{"one says not found and one is silent", func(l *fakeLister) { l.fail("n2", "d", errDown) }, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := newLister("n1", "n2")
			tt.setup(l)
			tr, _ := newTree(l, Options{})
			if _, err := tr.List(context.Background(), "d"); !errors.Is(err, tt.want) {
				t.Errorf("List = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestOneNodeHavingTheDirectoryIsEnough(t *testing.T) {
	t.Parallel()
	l := newLister("n1", "n2")
	l.say("n2", "d", wfile("f", 1, at(1), "e"))
	tr, _ := newTree(l, Options{})
	checkNames(t, "List", mustList(t, tr, "d"), "f")
}

func TestNegativeCaching(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	tr, clk := newTree(l, Options{TTL: time.Hour, NegativeTTL: 10 * time.Second})
	ctx := context.Background()

	for range 2 {
		if _, err := tr.List(ctx, "gone"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("List = %v, want ErrNotFound", err)
		}
	}
	if got := l.asked("gone"); got != 1 {
		t.Fatalf("asked %d times, want 1 within the negative TTL", got)
	}
	if _, err := tr.Stat(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat = %v, want ErrNotFound", err)
	}

	clk.Advance(10 * time.Second)
	l.say("n1", "gone", wfile("f", 1, at(1), "e"))
	checkNames(t, "List after the negative TTL", mustList(t, tr, "gone"), "f")
	if got := l.asked("gone"); got != 2 {
		t.Errorf("asked %d times, want 2", got)
	}
}

func TestTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ttl  time.Duration
		wait time.Duration
		want int
	}{
		{"zero is never reused", 0, 0, 2},
		{"reused within the TTL", time.Minute, time.Minute - time.Nanosecond, 1},
		{"asked again when the TTL has passed", time.Minute, time.Minute, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := newLister("n1")
			l.say("n1", "", wfile("f", 1, at(1), "e"))
			tr, clk := newTree(l, Options{TTL: tt.ttl})
			mustList(t, tr, "")
			clk.Advance(tt.wait)
			mustList(t, tr, "")
			if got := l.asked(""); got != tt.want {
				t.Errorf("asked %d times, want %d", got, tt.want)
			}
		})
	}
}

func TestMaxDirsEvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	for _, d := range []string{"x", "y", "z"} {
		l.say("n1", d, wfile("f", 1, at(1), "e"))
	}
	tr, _ := newTree(l, Options{TTL: time.Hour, MaxDirs: 2})

	mustList(t, tr, "x")
	mustList(t, tr, "y")
	mustList(t, tr, "x") // x is now more recent than y
	mustList(t, tr, "z") // evicts y
	mustList(t, tr, "x")
	mustList(t, tr, "z")
	if got := l.total(); got != 3 {
		t.Fatalf("lister calls = %d, want 3: x and z must still be in memory", got)
	}
	mustList(t, tr, "y")
	if got := l.asked("y"); got != 2 {
		t.Errorf("y asked %d times, want 2 after eviction", got)
	}
}

func TestMissingDirectoriesCountTowardsMaxDirs(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	l.say("n1", "x", wfile("f", 1, at(1), "e"))
	tr, _ := newTree(l, Options{TTL: time.Hour, NegativeTTL: time.Hour, MaxDirs: 1})
	mustList(t, tr, "x")
	if _, err := tr.List(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	mustList(t, tr, "x")
	if got := l.asked("x"); got != 2 {
		t.Errorf("x asked %d times, want 2 after the negative entry pushed it out", got)
	}
}

func TestForget(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	l.say("n1", "x", wfile("f", 1, at(1), "e"))
	tr, _ := newTree(l, Options{TTL: time.Hour})
	mustList(t, tr, "x")
	tr.Forget("unknown")
	mustList(t, tr, "x")
	if got := l.asked("x"); got != 1 {
		t.Fatalf("asked %d times, want 1", got)
	}
	tr.Forget("x")
	mustList(t, tr, "x")
	if got := l.asked("x"); got != 2 {
		t.Errorf("asked %d times, want 2 after Forget", got)
	}
}

func TestInvalidNamesAreNotFoundWithoutAskingAnyone(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	tr, _ := newTree(l, Options{})
	ctx := context.Background()
	for _, name := range []string{"/", "/a", "a/", "a//b", ".", "..", "a/./b", "a/../b", "a/.."} {
		if _, err := tr.List(ctx, name); !errors.Is(err, ErrNotFound) {
			t.Errorf("List(%q) = %v, want ErrNotFound", name, err)
		}
		if _, err := tr.Stat(ctx, name); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", name, err)
		}
	}
	if got := l.total(); got != 0 {
		t.Errorf("lister calls = %d, want 0", got)
	}
}

func TestNamesAreComparedByteForByte(t *testing.T) {
	t.Parallel()
	composed, decomposed := "caf\u00e9", "cafe\u0301"
	l := newLister("n1")
	l.say("n1", "", wfile(composed, 1, at(1), "e"), wfile(decomposed, 2, at(1), "e"))
	tr, _ := newTree(l, Options{})
	got := mustList(t, tr, "")
	if len(got) != 2 {
		t.Fatalf("List = %+v, want both spellings", got)
	}
	st, err := tr.Stat(context.Background(), decomposed)
	if err != nil || st.Size != 2 {
		t.Errorf("Stat = %+v, %v, want the decomposed spelling", st, err)
	}
}

func TestStat(t *testing.T) {
	t.Parallel()
	l := newLister("n1", "n2")
	l.say("n1", "", wdir("movies", at(1)))
	l.say("n1", "movies", wfile("a.mkv", 9, at(2), "e"), wdir("extras", at(3)))
	l.say("n2", "movies", wfile("a.mkv", 9, at(2), "e"))
	tr, _ := newTree(l, Options{TTL: time.Hour})
	ctx := context.Background()

	root, err := tr.Stat(ctx, "")
	if err != nil || !root.Dir || root.Name != "" {
		t.Errorf("Stat(root) = %+v, %v, want a directory", root, err)
	}
	if got := l.total(); got != 0 {
		t.Fatalf("Stat of the root asked %d nodes, want 0", got)
	}

	got, err := tr.Stat(ctx, "movies/a.mkv")
	want := Entry{Name: "a.mkv", Size: 9, MTime: at(2), ETag: "e", Nodes: []string{"n1", "n2"}}
	if err != nil || !equalEntry(got, want) {
		t.Errorf("Stat(file) = %+v, %v, want %+v", got, err, want)
	}
	sub, err := tr.Stat(ctx, "movies/extras")
	if err != nil || !sub.Dir {
		t.Errorf("Stat(dir) = %+v, %v, want a directory", sub, err)
	}
	if got := l.asked("movies"); got != 2 {
		t.Errorf("movies asked %d times, want once per node", got)
	}

	if _, err := tr.Stat(ctx, "movies/none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(absent) = %v, want ErrNotFound", err)
	}
	if _, err := tr.Stat(ctx, "nowhere/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(under a missing directory) = %v, want ErrNotFound", err)
	}
	if _, err := tr.Stat(ctx, "movies/a.mkv/x"); !errors.Is(err, ErrNotDir) {
		t.Errorf("Stat(under a file) = %v, want ErrNotDir", err)
	}
	if _, err := tr.Stat(ctx, "movies"); err != nil {
		t.Errorf("Stat(movies) = %v", err)
	}
}

func TestStatFailsWhenNoNodeAnswers(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	l.fail("n1", "", errDown)
	tr, _ := newTree(l, Options{})
	if _, err := tr.Stat(context.Background(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Stat = %v, want ErrUnavailable", err)
	}
}

func TestListOfAFileIsNotADirectory(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	l.say("n1", "", wfile("f", 1, at(1), "e"), wdir("d", at(1)))
	l.say("n1", "d", wfile("g", 1, at(1), "e"), wdir("sub", at(1)))
	l.say("n1", "d/sub")
	tr, clk := newTree(l, Options{TTL: time.Minute})
	ctx := context.Background()
	mustList(t, tr, "")
	mustList(t, tr, "d")
	calls := l.total()

	if _, err := tr.List(ctx, "f"); !errors.Is(err, ErrNotDir) {
		t.Errorf("List(f) = %v, want ErrNotDir", err)
	}
	if _, err := tr.List(ctx, "d/g"); !errors.Is(err, ErrNotDir) {
		t.Errorf("List(d/g) = %v, want ErrNotDir", err)
	}
	if got := l.total(); got != calls {
		t.Errorf("ErrNotDir asked %d nodes, want 0", got-calls)
	}

	// A directory, and a name the parent does not list, are asked for.
	mustList(t, tr, "d/sub")
	if _, err := tr.List(ctx, "d/none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List(d/none) = %v, want ErrNotFound", err)
	}

	// A listing that has expired is not trusted to say what is a file.
	clk.Advance(time.Minute)
	if _, err := tr.List(ctx, "f"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List(f) after the parent expired = %v, want the nodes' answer, ErrNotFound", err)
	}
}

func TestListOfAChildOfAMissingParentIsAsked(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	tr, _ := newTree(l, Options{TTL: time.Hour, NegativeTTL: time.Hour})
	ctx := context.Background()
	if _, err := tr.List(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := tr.List(ctx, "a/b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List(a/b) = %v, want ErrNotFound", err)
	}
	if got := l.asked("a/b"); got != 1 {
		t.Errorf("a/b asked %d times, want 1", got)
	}
}

func TestReturnedSlicesDoNotAliasTheCache(t *testing.T) {
	t.Parallel()
	l := newLister("n1", "n2")
	l.say("n1", "", wfile("a", 1, at(1), "e"))
	l.say("n2", "", wfile("a", 1, at(1), "e"))
	tr, _ := newTree(l, Options{TTL: time.Hour})
	ctx := context.Background()

	first := mustList(t, tr, "")
	first[0].Name = "changed"
	first[0].Nodes[0] = "changed"
	_ = slices.Delete(first, 0, 1)

	again := mustList(t, tr, "")
	want := Entry{Name: "a", Size: 1, MTime: at(1), ETag: "e", Nodes: []string{"n1", "n2"}}
	checkEntries(t, again, []Entry{want})

	st, err := tr.Stat(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	st.Nodes[1] = "changed"
	checkEntries(t, mustList(t, tr, ""), []Entry{want})
}

func TestAFreshListingIsReturnedEvenToACancelledCaller(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	l.say("n1", "", wfile("a", 1, at(1), "e"))
	tr, _ := newTree(l, Options{TTL: time.Hour})
	mustList(t, tr, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.List(ctx, ""); err != nil {
		t.Errorf("List = %v, want the cached listing", err)
	}
}
