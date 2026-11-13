package core

import (
	"context"
	"errors"
	"testing"

	"github.com/SpliceFS/client/internal/session"
)

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
