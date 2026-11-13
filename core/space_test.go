package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// spaceHarness is three nodes with the space of two shares, and a mount of the media share
// as its second view.
func spaceHarness(t *testing.T) *harness {
	t.Helper()
	env := newHarness(t, "a", "b", "c")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	env.fc.put("docs/file", pattern(1, 10), fileTime())
	sizes := map[string][3][2]uint64{ // share: per node {total, free}
		"media": {{1000, 300}, {900, 400}, {950, 200}},
		"docs":  {{50, 40}, {60, 10}, {70, 30}},
	}
	for share, perNode := range sizes {
		for i, nodeSize := range perNode {
			env.fc.nodes[i].setSpace(share, nodeSize[0], nodeSize[1])
		}
	}
	env.addMount("/media/sub", "/mnt/media")
	return env
}

func TestStatFSCombinesTheNodes(t *testing.T) {
	t.Parallel()
	env := spaceHarness(t)
	ctx := context.Background()

	if got, err := env.view(1).StatFS(ctx); err != nil || got != (Space{Total: 900, Free: 200}) {
		t.Errorf("StatFS of a media view = %+v, %v; want the smallest total and free", got, err)
	}
	if got, err := env.view(0).StatFS(ctx); err != nil || got != (Space{Total: 950, Free: 210}) {
		t.Errorf("StatFS of the root = %+v, %v; want the sum of the shares", got, err)
	}
}

func TestStatFSRemembersTheAnswerForAMinute(t *testing.T) {
	t.Parallel()
	env := spaceHarness(t)
	media := env.view(1)
	ctx := context.Background()
	if _, err := media.StatFS(ctx); err != nil {
		t.Fatal(err)
	}

	env.fc.nodes[1].setSpace("media", 10, 5)
	if got, err := media.StatFS(ctx); err != nil || got != (Space{Total: 900, Free: 200}) {
		t.Errorf("StatFS within a minute = %+v, %v; want the remembered answer", got, err)
	}
	env.clock.Advance(time.Minute)
	if got, err := media.StatFS(ctx); err != nil || got != (Space{Total: 10, Free: 5}) {
		t.Errorf("StatFS after a minute = %+v, %v; want a new answer", got, err)
	}
}

func TestStatFSWithSilentNodes(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	env.fc.put("media/file", pattern(1, 10), fileTime())
	env.fc.nodes[0].setSpace("media", 100, 50)
	env.fc.nodes[1].setSpace("media", 10, 5)
	env.addMount("/media", "/mnt/media")
	fsys := env.view(1)
	ctx := context.Background()
	if _, err := fsys.Stat(ctx, ""); err != nil {
		t.Fatal(err)
	}

	env.fc.nodes[1].setDown(true)
	if got, err := fsys.StatFS(ctx); err != nil || got != (Space{Total: 100, Free: 50}) {
		t.Errorf("StatFS with one node down = %+v, %v; want the answering node's", got, err)
	}
	env.clock.Advance(time.Minute)
	env.fc.nodes[0].setDown(true)
	if _, err := fsys.StatFS(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("StatFS with every node down = %v, want ErrUnavailable", err)
	}
}

func TestStatFSFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	down := newHarness(t, "a")
	down.fc.setAllDown()
	if _, err := down.view(0).StatFS(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("StatFS of the root with no listing = %v, want ErrUnavailable", err)
	}

	closed := newHarness(t, "a")
	closed.addMount("/media", "/mnt/media")
	fsys := closed.view(1)
	if err := closed.client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.StatFS(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("StatFS after Close = %v, want ErrUnavailable", err)
	}

	held := newHarness(t, "a")
	held.fc.put("media/file", pattern(1, 10), fileTime())
	held.addMount("/media", "/mnt/media")
	fsys = held.view(1)
	if _, err := fsys.Stat(ctx, ""); err != nil {
		t.Fatal(err)
	}
	node := held.fc.nodes[0]
	release := node.hold()
	defer release()
	cancelled, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := fsys.StatFS(cancelled)
		done <- err
	}()
	<-node.waits
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Errorf("cancelled StatFS = %v, want only context.Canceled", err)
	}
}
