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
		cluster := newEnv(t)
		listed := []wire.Node{
			{Name: name1, URL: url1}, {Name: name2, URL: url2}, {Name: name1, URL: url2},
		}
		cluster.setList(listed...)
		opts := cluster.options()
		var unreachable []string
		if failSecond {
			unreachable = append(unreachable, name2)
		}
		opts.Dial = fuzzDial(cluster, unreachable...)
		session := New(opts)
		ctx := context.Background()
		nodes, err := session.Nodes(ctx)
		if err != nil {
			// A list with no usable node is an error, never an empty success.
			return
		}
		got := names(t, nodes)
		checkListedNames(t, got, listed)
		// Usable always offers something to try, even when every node is cooling down.
		usable, err := session.Usable(ctx)
		if err != nil || len(usable) == 0 || len(usable) > len(got) {
			t.Fatalf("Usable = %d nodes, %v, with %d nodes", len(usable), err, len(got))
		}
	})
}

// fuzzDial dials the cluster host as the cluster does, and makes a new node for every other
// name, except that the names in unreachable cannot be dialed.
func fuzzDial(cluster *env, unreachable ...string) Dial {
	return func(name, _ string) (Node, error) {
		if name == entryName {
			return cluster.entry, nil
		}
		if slices.Contains(unreachable, name) {
			return nil, errDown
		}
		return &fakeNode{name: name, loginFn: cluster.grant, nodesFn: cluster.serveList}, nil
	}
}

// checkListedNames holds what a session makes of a node list: some node, no name twice,
// and no name that was not in the list.
func checkListedNames(t *testing.T, got []string, listed []wire.Node) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("Nodes returned no node and no error for %+v", listed)
	}
	// A repeated name would make one node count twice in every decision.
	slices.Sort(got)
	if len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Fatalf("Nodes returned a name twice: %q", got)
	}
	for _, name := range got {
		if !slices.ContainsFunc(listed, func(node wire.Node) bool { return node.Name == name }) {
			t.Fatalf("Nodes returned %q, which no one listed", name)
		}
	}
}
