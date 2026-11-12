package session

import (
	"context"
	"slices"
	"testing"

	"github.com/SpliceFS/common/wire"
)

// FuzzNodeList builds a session from a node list that the entry node made up, which is
// what a compromised or confused server can send.
func FuzzNodeList(f *testing.F) {
	f.Add("a", "https://a.example.com/api", "b", "https://b.example.com/api", true)
	f.Add("a", "u", "a", "v", true)
	f.Add("", "", "", "", false)
	f.Add("../x", "file:///etc/passwd", "a/b", "http://[::1", true)
	f.Add("n\x00", "\xff", "dup", "dup", false)
	f.Fuzz(func(t *testing.T, name1, url1, name2, url2 string, failSecond bool) {
		e := newEnv(t)
		listed := []wire.Node{
			{Name: name1, URL: url1}, {Name: name2, URL: url2}, {Name: name1, URL: url2},
		}
		e.setList(listed...)
		opts := e.options()
		opts.Dial = func(name, _ string) (Node, error) {
			if name == entryName {
				return e.entry, nil
			}
			if failSecond && name == name2 {
				return nil, errDown
			}
			return &fakeNode{name: name, loginFn: e.grant, nodesFn: e.serveList}, nil
		}
		s := New(opts)
		ctx := context.Background()
		nodes, err := s.Nodes(ctx)
		if err != nil {
			// A list with no usable node is an error, never an empty success.
			return
		}
		got := names(t, nodes)
		if len(got) == 0 {
			t.Fatalf("Nodes returned no node and no error for %+v", listed)
		}
		// A repeated name would make one node count twice in every decision.
		slices.Sort(got)
		if len(slices.Compact(slices.Clone(got))) != len(got) {
			t.Fatalf("Nodes returned a name twice: %q", got)
		}
		for _, n := range got {
			if n != name1 && n != name2 {
				t.Fatalf("Nodes returned %q, which no one listed", n)
			}
		}
		// Usable always offers something to try, even when every node is cooling down.
		usable, err := s.Usable(ctx)
		if err != nil || len(usable) == 0 || len(usable) > len(got) {
			t.Fatalf("Usable = %d nodes, %v, with %d nodes", len(usable), err, len(got))
		}
	})
}
