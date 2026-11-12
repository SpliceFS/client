package tree

import (
	"context"
	"strings"
	"testing"

	"github.com/SpliceFS/common/wire"
)

// FuzzListing builds a tree from listings that two untrusted nodes made up, and from
// lookups of made up names.
func FuzzListing(f *testing.F) {
	f.Add("a", "b", "sub", false, int64(1))
	f.Add("a", "a", "a", true, int64(0))
	f.Add("", "", "", false, int64(-1))
	f.Add(".", "..", "../..", true, int64(1))
	f.Add("a/b", "/a", "a/b/c", false, int64(1<<62))
	f.Add("..", "../x", "a/../../b", false, int64(1))
	f.Add("a\x00b", "a\\b", "a//b", true, int64(7))
	f.Add(strings.Repeat("x", 1<<16), "caf\u00e9", "cafe\u0301", false, int64(3))
	f.Fuzz(func(t *testing.T, name1, name2, dir string, isDir bool, size int64) {
		l := newLister("n1", "n2")
		l.say("n1", "", wfile(name1, size, at(1), "e1"), wdir(name2, at(2)))
		l.say("n2", "", wfile(name2, size, at(1), "e2"), wire.Entry{Name: name1, Dir: isDir})
		l.say("n1", dir, wfile(name1, size, at(1), "e1"))
		l.say("n2", dir, wdir(name2, at(1)))
		tr, _ := newTree(l, Options{})
		ctx := context.Background()
		for _, d := range []string{"", dir} {
			entries, err := tr.List(ctx, d)
			if err != nil {
				continue
			}
			checkSegments(t, entries)
		}
		for _, n := range []string{name1, name2, dir, dir + "/" + name1} {
			e, err := tr.Stat(ctx, n)
			// A name that is not a clean path must not be answered, or a caller could be
			// pointed outside the directory it asked about.
			if err == nil && n != "" && !validName(n) {
				t.Fatalf("Stat(%q) = %+v, want an error", n, e)
			}
		}
	})
}

// checkSegments holds what a caller relies on when it joins an entry name to the
// directory it listed: the name is one path segment, so the entry stays inside it.
func checkSegments(t *testing.T, entries []Entry) {
	t.Helper()
	for i, e := range entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." || strings.Contains(e.Name, "/") {
			t.Fatalf("entry %d has the name %q, which is not a single path segment", i, e.Name)
		}
		// Sorted and unique names keep a name from being shown twice with two kinds.
		if i > 0 && entries[i-1].Name >= e.Name {
			t.Fatalf("entries %q and %q are not sorted and unique", entries[i-1].Name, e.Name)
		}
	}
}
