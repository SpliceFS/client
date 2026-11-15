package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

const fallbackLine = "cluster lists no nodes, using its address as the only node"

// logged returns a logger and the text it has written so far.
func logged() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestEmptyListUsesTheEntryURLAsTheOnlyNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		want string // the name of the node
	}{
		{"host and path", "https://entry.example.com/api", "entry.example.com"},
		{"port is kept", "https://entry.example.com:8443/api", "entry.example.com:8443"},
		{"address with a port", "http://10.0.0.5:8080", "10.0.0.5:8080"},
		{"user name is not part of the name", "https://bob@files.example.com/x", "files.example.com"},
		{"no host", "no-host", entryName},
		{"not a URL", "://bad", entryName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cluster := newEnv(t)
			if tt.want != entryName && tt.want != entryHost {
				cluster.nodes[tt.want] = cluster.newNode(tt.want)
			}
			opts := cluster.options()
			opts.EntryURL = tt.url
			logger, buf := logged()
			opts.Logger = logger
			session := New(opts)

			checkNames(t, "Nodes", mustNodes(t, session), []string{tt.want})

			checkNames(t, "dials", cluster.dials(), []string{"entry " + tt.url, tt.want + " " + tt.url})
			if got := strings.Count(buf.String(), fallbackLine); got != 1 {
				t.Errorf("fallback line logged %d times, want 1; log:\n%s", got, buf)
			}
			for _, part := range []string{"level=INFO", "cluster=main", "url=" + tt.url} {
				if !strings.Contains(buf.String(), part) {
					t.Errorf("log lacks %q:\n%s", part, buf)
				}
			}
		})
	}
}

func TestEmptyListReadsThroughTheEntryURL(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t)
	opts := cluster.options()
	logger, buf := logged()
	opts.Logger = logger
	session := New(opts)

	checkNames(t, "Usable", mustUsable(t, session), []string{entryHost})
	n := nodeOf(t, session, entryHost)
	if got := token(t, session, n); got != "tok-1" {
		t.Errorf("token = %q, want tok-1", got)
	}
	// The only node stays in use while it is cooling, as every cluster's last node does.
	fail(t, session, n)
	checkNames(t, "Usable after a failure", mustUsable(t, session), []string{entryHost})
	if got := strings.Count(buf.String(), fallbackLine); got != 1 {
		t.Errorf("fallback line logged %d times, want 1", got)
	}
	if got := cluster.entry.loginCount(); got != 1 {
		t.Errorf("entry logins = %d, want 1", got)
	}
}

func TestListWithNodesNeverAddsTheEntryURL(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	opts := cluster.options()
	logger, buf := logged()
	opts.Logger = logger
	opts.RefreshNodes = time.Minute
	session := New(opts)

	checkNames(t, "Nodes", mustNodes(t, session), []string{"a", "b"})
	cluster.clk.Advance(time.Minute)
	checkNames(t, "Nodes after a refresh", mustNodes(t, session), []string{"a", "b"})

	for _, d := range cluster.dials() {
		if strings.HasPrefix(d, entryHost) {
			t.Errorf("dials = %v, want the entry URL dialed only as the entry", cluster.dials())
		}
	}
	if strings.Contains(buf.String(), fallbackLine) {
		t.Errorf("fallback line logged for a cluster that lists nodes:\n%s", buf)
	}
}

func TestListWithNodesOfWhichNoneIsUsableIsStillAnError(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	cluster.dialErr["a"] = errDown
	opts := cluster.options()
	logger, buf := logged()
	opts.Logger = logger
	session := New(opts)

	const want = "1 nodes listed, none usable"
	_, err := session.Nodes(context.Background())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), want) {
		t.Errorf("Nodes = %v, want ErrUnavailable with %q", err, want)
	}
	if slices.ContainsFunc(cluster.dials(), func(d string) bool { return strings.HasPrefix(d, entryHost) }) {
		t.Errorf("dials = %v, want no fallback dial", cluster.dials())
	}
	if strings.Contains(buf.String(), fallbackLine) {
		t.Errorf("fallback line logged for a cluster that lists nodes:\n%s", buf)
	}
}

func TestListChangesBetweenEmptyAndNodes(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t)
	opts := cluster.options()
	logger, buf := logged()
	opts.Logger = logger
	opts.RefreshNodes = time.Minute
	session := New(opts)
	refresh := func() { cluster.clk.Advance(time.Minute) }

	checkNames(t, "empty list", mustNodes(t, session), []string{entryHost})
	fallback := nodeOf(t, session, entryHost)

	refresh()
	if got := nodeOf(t, session, entryHost); got != fallback {
		t.Error("the fallback was dialed again while the list stayed empty")
	}

	cluster.addNode("a")
	refresh()
	checkNames(t, "list with a node replaces the fallback", mustNodes(t, session), []string{"a"})

	cluster.setList()
	refresh()
	checkNames(t, "empty list again", mustNodes(t, session), []string{entryHost})

	cluster.addNode("b")
	refresh()
	checkNames(t, "list with a node again", mustNodes(t, session), []string{"b"})

	if got := strings.Count(buf.String(), fallbackLine); got != 1 {
		t.Errorf("fallback line logged %d times over the session, want 1", got)
	}
}

func TestListWithNodesBecomingEmptyFallsBack(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	opts := cluster.options()
	logger, buf := logged()
	opts.Logger = logger
	opts.RefreshNodes = time.Minute
	session := New(opts)
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a"})
	if strings.Contains(buf.String(), fallbackLine) {
		t.Fatalf("fallback line logged while nodes were listed:\n%s", buf)
	}

	cluster.setList()
	cluster.clk.Advance(time.Minute)

	checkNames(t, "Nodes", mustNodes(t, session), []string{entryHost})
	if got := strings.Count(buf.String(), fallbackLine); got != 1 {
		t.Errorf("fallback line logged %d times, want 1", got)
	}
}
