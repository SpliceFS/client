package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SpliceFS/client/internal/remote"
	"github.com/SpliceFS/client/internal/session"
	"github.com/SpliceFS/common/wire"
)

// refusal is the error of a node that refuses the version of this client. Like the one of the
// remote folder, its text is the message alone and it matches remote.ErrIncompatible.
type refusal struct{ message string }

func (r refusal) Error() string { return r.message }

func (refusal) Unwrap() error { return remote.ErrIncompatible }

// dialOldServer makes every node, and so the cluster host, an old server that refuses every call.
func dialOldServer(err error) session.Dial {
	return func(string, string) (session.Node, error) { return oldServer{err}, nil }
}

// oldServer is a cluster host whose every call is refused.
type oldServer struct{ err error }

func (oldServer) Name() string { return "entry" }

func (o oldServer) Login(context.Context, string, string) (wire.LoginResponse, error) {
	return wire.LoginResponse{}, o.err
}

func (o oldServer) Nodes(context.Context, string) (wire.Nodes, error) {
	return wire.Nodes{}, o.err
}

func TestConnectReachesTheCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newHarness(t, "a", "b")
	env.start()
	if err := env.client.Connect(ctx, testCluster); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if nodes := env.client.Status().Clusters[0].Nodes; len(nodes) != 2 {
		t.Errorf("Status after Connect lists %d nodes, want 2", len(nodes))
	}
	err := env.client.Connect(ctx, "other")
	if err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBadCredentials) {
		t.Errorf("Connect to an unknown cluster = %v, want a plain error", err)
	}
}

func TestConnectWithAWrongPassword(t *testing.T) {
	t.Parallel()
	wrong := newHarness(t, "a")
	opts := wrong.options()
	opts.ReadPassword = func(string) (string, error) { return "wrong", nil }
	wrongClient, err := newClient(wrong.cfg, opts, wrong.clock)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFile(t, wrongClient)
	err = wrongClient.Connect(context.Background(), testCluster)
	if !errors.Is(err, ErrBadCredentials) || !errors.Is(err, session.ErrBadCredentials) || errors.Is(err, ErrUnavailable) {
		t.Errorf("Connect with a wrong password = %v, want ErrBadCredentials with the session's cause", err)
	}
}

func TestConnectToAnOldServer(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.start()
	message := "server files.example.com runs splicefsd 0.3.3, and this client (0.4.4) needs " +
		"splicefsd 0.4 or newer: upgrade the server, or install an older client"
	clus := env.client.clusters[testCluster]
	opts := clus.sessionOptions()
	opts.Dial = dialOldServer(refusal{message})
	clus.session = session.New(opts)

	err := env.client.Connect(context.Background(), testCluster)
	if !errors.Is(err, ErrIncompatible) || !errors.Is(err, session.ErrIncompatible) {
		t.Fatalf("Connect to an old server = %v, want ErrIncompatible with the session's cause", err)
	}
	if !errors.Is(err, remote.ErrIncompatible) {
		t.Errorf("Connect to an old server = %v, want the refusal as the cause", err)
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBadCredentials) {
		t.Errorf("Connect to an old server = %v, want neither ErrUnavailable nor ErrBadCredentials", err)
	}
	if !strings.Contains(err.Error(), message) {
		t.Errorf("Connect to an old server = %q, want the message %q in it", err, message)
	}
}

func TestClusterTreatsARefusedNodeLikeOneThatIsDown(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.start()
	opts := env.client.clusters[testCluster].sessionOptions()
	tests := []struct {
		name         string
		err          error
		isUnavail    bool
		isIncompat   bool
		isUnauthoriz bool
	}{
		{"node is down", fmt.Errorf("node a: %w", remote.ErrUnavailable), true, false, false},
		{"node refuses its version", fmt.Errorf("node a: %w", refusal{"too old"}), true, true, false},
		{"node refuses the token", fmt.Errorf("node a: %w", remote.ErrUnauthorized), false, false, true},
		{"node does not have the path", fmt.Errorf("node a: %w", remote.ErrNotFound), false, false, false},
	}
	for _, tt := range tests {
		if got := opts.IsUnavailable(tt.err); got != tt.isUnavail {
			t.Errorf("%s: IsUnavailable = %v, want %v", tt.name, got, tt.isUnavail)
		}
		if got := opts.IsIncompatible(tt.err); got != tt.isIncompat {
			t.Errorf("%s: IsIncompatible = %v, want %v", tt.name, got, tt.isIncompat)
		}
		if got := opts.IsUnauthorized(tt.err); got != tt.isUnauthoriz {
			t.Errorf("%s: IsUnauthorized = %v, want %v", tt.name, got, tt.isUnauthoriz)
		}
	}
}

func TestConnectWithEveryNodeDown(t *testing.T) {
	t.Parallel()
	down := newHarness(t, "a")
	down.fc.setAllDown()
	down.start()
	err := down.client.Connect(context.Background(), testCluster)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, session.ErrUnavailable) {
		t.Errorf("Connect with every node down = %v, want ErrUnavailable with the session's cause", err)
	}
}

func TestConnectCancelled(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a")
	env.start()
	node := env.fc.nodes[0]
	release := node.hold()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.client.Connect(ctx, testCluster) }()
	<-node.waits
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Errorf("cancelled Connect = %v, want only context.Canceled", err)
	}
}
