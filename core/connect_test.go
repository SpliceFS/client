package core

import (
	"context"
	"errors"
	"testing"

	"github.com/SpliceFS/client/internal/session"
)

func TestConnect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, "a", "b")
	h.start()
	if err := h.client.Connect(ctx, testCluster); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if nodes := h.client.Status().Clusters[0].Nodes; len(nodes) != 2 {
		t.Errorf("Status after Connect lists %d nodes, want 2", len(nodes))
	}
	if err := h.client.Connect(ctx, "other"); err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBadCredentials) {
		t.Errorf("Connect to an unknown cluster = %v, want a plain error", err)
	}

	wrong := newHarness(t, "a")
	opts := wrong.options()
	opts.ReadPassword = func(string) (string, error) { return "wrong", nil }
	c, err := newClient(wrong.cfg, opts, wrong.clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	err = c.Connect(ctx, testCluster)
	if !errors.Is(err, ErrBadCredentials) || !errors.Is(err, session.ErrBadCredentials) || errors.Is(err, ErrUnavailable) {
		t.Errorf("Connect with a wrong password = %v, want ErrBadCredentials with the session's cause", err)
	}

	down := newHarness(t, "a")
	down.fc.setAllDown()
	down.start()
	err = down.client.Connect(ctx, testCluster)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, session.ErrUnavailable) {
		t.Errorf("Connect with every node down = %v, want ErrUnavailable with the session's cause", err)
	}
}

func TestConnectCancelled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "a")
	h.start()
	node := h.fc.nodes[0]
	release := node.hold()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.client.Connect(ctx, testCluster) }()
	<-node.waits
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Errorf("cancelled Connect = %v, want only context.Canceled", err)
	}
}
