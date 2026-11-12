package tree

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
)

func TestMergeRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		nodes map[string][]wire.Entry
		want  []Entry
	}{
		{
			name: "union of names sorted bytewise",
			nodes: map[string][]wire.Entry{
				"n1": {wfile("b", 1, at(1), "e"), wfile("B", 1, at(1), "e")},
				"n2": {wfile("a", 2, at(2), "f")},
			},
			want: []Entry{
				{Name: "B", Size: 1, MTime: at(1), ETag: "e", Nodes: []string{"n1"}},
				{Name: "a", Size: 2, MTime: at(2), ETag: "f", Nodes: []string{"n2"}},
				{Name: "b", Size: 1, MTime: at(1), ETag: "e", Nodes: []string{"n1"}},
			},
		},
		{
			name: "kind is what most nodes say: file",
			nodes: map[string][]wire.Entry{
				"n1": {wfile("x", 5, at(1), "e")},
				"n2": {wfile("x", 5, at(1), "e")},
				"n3": {wdir("x", at(9))},
			},
			want: []Entry{{Name: "x", Size: 5, MTime: at(1), ETag: "e", Nodes: []string{"n1", "n2"}}},
		},
		{
			name: "kind is what most nodes say: directory",
			nodes: map[string][]wire.Entry{
				"n1": {wdir("x", at(1))},
				"n2": {wdir("x", at(2))},
				"n3": {wfile("x", 5, at(9), "e")},
			},
			want: []Entry{{Name: "x", Dir: true, MTime: at(2)}},
		},
		{
			name: "a tie in kind is a directory",
			nodes: map[string][]wire.Entry{
				"n1": {wdir("x", at(1))},
				"n2": {wfile("x", 5, at(9), "e")},
			},
			want: []Entry{{Name: "x", Dir: true, MTime: at(1)}},
		},
		{
			name: "the largest group of identical reports wins",
			nodes: map[string][]wire.Entry{
				"n1": {wfile("x", 5, at(1), "old")},
				"n2": {wfile("x", 6, at(9), "new")},
				"n3": {wfile("x", 5, at(1), "old")},
			},
			want: []Entry{{Name: "x", Size: 5, MTime: at(1), ETag: "old", Nodes: []string{"n1", "n3"}}},
		},
		{
			name: "a tie between groups goes to the newest mtime",
			nodes: map[string][]wire.Entry{
				"n1": {wfile("x", 5, at(1), "z")},
				"n2": {wfile("x", 6, at(2), "a")},
			},
			want: []Entry{{Name: "x", Size: 6, MTime: at(2), ETag: "a", Nodes: []string{"n2"}}},
		},
		{
			name: "then to the greatest etag",
			nodes: map[string][]wire.Entry{
				"n1": {wfile("x", 5, at(1), "b")},
				"n2": {wfile("x", 6, at(1), "c")},
				"n3": {wfile("x", 7, at(1), "a")},
			},
			want: []Entry{{Name: "x", Size: 6, MTime: at(1), ETag: "c", Nodes: []string{"n2"}}},
		},
		{
			name: "then to the largest size",
			nodes: map[string][]wire.Entry{
				"n1": {wfile("x", 5, at(1), "e")},
				"n2": {wfile("x", 7, at(1), "e")},
			},
			want: []Entry{{Name: "x", Size: 7, MTime: at(1), ETag: "e", Nodes: []string{"n2"}}},
		},
		{
			name: "a directory takes the newest mtime of the nodes that say directory",
			nodes: map[string][]wire.Entry{
				"n1": {wdir("d", at(3))},
				"n2": {wdir("d", at(5))},
				"n3": {wdir("d", at(4)), wfile("f", 1, at(1), "e")},
				"n4": {wfile("d", 1, at(99), "e")},
			},
			want: []Entry{
				{Name: "d", Dir: true, MTime: at(5)},
				{Name: "f", Size: 1, MTime: at(1), ETag: "e", Nodes: []string{"n3"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := newLister("n3", "n1", "n2", "n4")
			for node, entries := range tt.nodes {
				l.say(node, "", entries...)
			}
			tr, _ := newTree(l, Options{})
			checkEntries(t, mustList(t, tr, ""), tt.want)
		})
	}
}

func TestAnEmptyDirectoryIsNotMissing(t *testing.T) {
	t.Parallel()
	l := newLister("n1", "n2")
	l.say("n1", "empty")
	tr, _ := newTree(l, Options{})
	got := mustList(t, tr, "empty")
	if len(got) != 0 {
		t.Errorf("List = %+v, want no entries", got)
	}
}

func TestKeepEntriesOfSilentNodes(t *testing.T) {
	t.Parallel()
	l := newLister("n1", "n2")
	l.say("n1", "", wfile("a", 1, at(1), "e"), wdir("sub", at(1)), wfile("b", 1, at(1), "e"))
	l.say("n2", "", wfile("b", 1, at(1), "e"), wfile("c", 1, at(1), "e"))
	tr, _ := newTree(l, Options{})
	checkNames(t, "first listing", mustList(t, tr, ""), "a", "b", "c", "sub")

	// n1 stops answering: what only n1 reported stays, what only n2 reported goes.
	l.fail("n1", "", errDown)
	l.say("n2", "", wfile("b", 1, at(1), "e"))
	checkNames(t, "n1 silent", mustList(t, tr, ""), "a", "b", "sub")

	// n1 answering that it has no such directory is an answer: its names go.
	l.fail("n1", "", errGone)
	checkNames(t, "n1 answers not found", mustList(t, tr, ""), "b")
}

func TestKeptEntryIsUnchanged(t *testing.T) {
	t.Parallel()
	l := newLister("n1", "n2")
	l.say("n1", "", wfile("a", 7, at(3), "tag"))
	l.say("n2", "", wfile("a", 7, at(3), "tag"))
	tr, _ := newTree(l, Options{})
	first := mustList(t, tr, "")

	l.fail("n1", "", errDown)
	l.say("n2", "")
	checkEntries(t, mustList(t, tr, ""), first)
}

func TestSilentReportersOfAnEntryThatIsStillListedAreRemembered(t *testing.T) {
	t.Parallel()
	l := newLister("n1", "n2")
	l.say("n1", "", wfile("a", 1, at(1), "e"))
	l.say("n2", "", wfile("a", 1, at(1), "e"))
	tr, _ := newTree(l, Options{})
	mustList(t, tr, "")

	// n2 alone still lists a while n1 is silent.
	l.fail("n1", "", errDown)
	checkNames(t, "n1 silent", mustList(t, tr, ""), "a")
	// n2 drops it, and n1 may still have it.
	l.say("n2", "")
	checkNames(t, "n2 dropped it", mustList(t, tr, ""), "a")
	// n1 answers and it is really gone.
	l.say("n1", "")
	checkNames(t, "n1 answered", mustList(t, tr, ""))
}

func TestNoNodeAnsweringReturnsThePreviousListing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		degrade func(l *fakeLister)
	}{
		{"every node silent", func(l *fakeLister) { l.fail("n1", "", errDown) }},
		{"the node list cannot be fetched", func(l *fakeLister) { l.nodesErr = errDown }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := newLister("n1")
			l.say("n1", "", wfile("a", 1, at(1), "e"))
			tr, clk := newTree(l, Options{TTL: time.Minute})
			first := mustList(t, tr, "")

			clk.Advance(24 * time.Hour)
			tt.degrade(l)
			checkEntries(t, mustList(t, tr, ""), first)
		})
	}
}

func TestNoNodeAnsweringWithNothingRememberedIsUnavailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(l *fakeLister)
	}{
		{"every node silent", func(l *fakeLister) { l.fail("n1", "", errDown) }},
		{"the node list cannot be fetched", func(l *fakeLister) { l.nodesErr = errDown }},
		{"there are no nodes", func(l *fakeLister) { l.nodes = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := newLister("n1")
			tt.setup(l)
			tr, _ := newTree(l, Options{})
			if _, err := tr.List(context.Background(), ""); !errors.Is(err, ErrUnavailable) {
				t.Errorf("List = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestMissingDirectoryIsNotAListingToFallBackOn(t *testing.T) {
	t.Parallel()
	l := newLister("n1")
	tr, clk := newTree(l, Options{})
	if _, err := tr.List(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("List = %v, want ErrNotFound", err)
	}
	clk.Advance(time.Hour)
	l.fail("n1", "gone", errDown)
	if _, err := tr.List(context.Background(), "gone"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("List = %v, want ErrUnavailable", err)
	}
}
