package tree

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SpliceFS/client/internal/wire"
)

// gated makes every List wait for the gate, and records whether its context was
// still alive when it was let go.
type gated struct {
	gate      chan struct{}
	mu        sync.Mutex
	cancelled bool
}

func (g *gated) list(ctx context.Context, _, _ string) ([]wire.Entry, error) {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cancelled = g.cancelled || ctx.Err() != nil
	return []wire.Entry{wfile("f", 1, at(1), "e")}, nil
}

func TestConcurrentListsShareOneFanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLister("n1", "n2")
		g := &gated{gate: make(chan struct{})}
		l.listFn = g.list
		tr, _ := newTree(l, Options{})

		results := make(chan []Entry, 2)
		for range 2 {
			go func() {
				got, err := tr.List(context.Background(), "d")
				if err != nil {
					t.Error(err)
				}
				results <- got
			}()
		}
		synctest.Wait()
		close(g.gate)
		checkNames(t, "first caller", <-results, "f")
		checkNames(t, "second caller", <-results, "f")

		if got := l.asked("d"); got != 2 {
			t.Errorf("nodes asked %d times, want once each (2)", got)
		}
	})
}

func TestACallerThatGivesUpDoesNotCancelTheFanOutForOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLister("n1")
		g := &gated{gate: make(chan struct{})}
		l.listFn = g.list
		tr, _ := newTree(l, Options{TTL: time.Hour})

		ctx, cancel := context.WithCancel(context.Background())
		quitter := make(chan error, 1)
		go func() {
			_, err := tr.List(ctx, "d")
			quitter <- err
		}()
		synctest.Wait()

		stayer := make(chan []Entry, 1)
		go func() {
			got, err := tr.List(context.Background(), "d")
			if err != nil {
				t.Error(err)
			}
			stayer <- got
		}()
		synctest.Wait()

		cancel()
		if err := <-quitter; !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled caller = %v, want context.Canceled", err)
		}
		close(g.gate)
		checkNames(t, "remaining caller", <-stayer, "f")

		if g.cancelled {
			t.Error("the node's context was cancelled when one caller gave up")
		}
		checkNames(t, "cached result", mustList(t, tr, "d"), "f")
		if got := l.asked("d"); got != 1 {
			t.Errorf("asked %d times, want 1", got)
		}
	})
}

func TestACancelledCallerWithNothingCachedGetsItsContextError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLister("n1")
		l.say("n1", "", wfile("f", 1, at(1), "e"))
		tr, _ := newTree(l, Options{TTL: time.Hour})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := tr.List(ctx, ""); !errors.Is(err, context.Canceled) {
			t.Errorf("List = %v, want context.Canceled", err)
		}
		// The fan-out that was started still finishes and is remembered.
		synctest.Wait()
		checkNames(t, "List", mustList(t, tr, ""), "f")
		if got := l.asked(""); got != 1 {
			t.Errorf("asked %d times, want 1", got)
		}
	})
}

func TestAHungNodeIsGivenUpOnAfterNodeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLister("fast", "hung")
		var mu sync.Mutex
		var hungErr error
		l.listFn = func(ctx context.Context, node, _ string) ([]wire.Entry, error) {
			if node == "fast" {
				return []wire.Entry{wfile("f", 1, at(1), "e")}, nil
			}
			<-ctx.Done()
			mu.Lock()
			defer mu.Unlock()
			hungErr = ctx.Err()
			return nil, ctx.Err()
		}
		tr, _ := newTree(l, Options{NodeTimeout: 10 * time.Second})

		before := time.Now()
		checkNames(t, "List", mustList(t, tr, ""), "f")
		if waited := time.Since(before); waited != 10*time.Second {
			t.Errorf("List took %v, want NodeTimeout (10s)", waited)
		}
		mu.Lock()
		defer mu.Unlock()
		if !errors.Is(hungErr, context.DeadlineExceeded) {
			t.Errorf("hung node's context ended with %v, want DeadlineExceeded", hungErr)
		}
	})
}

func TestAHungNodeListDoesNotBlockForever(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &hungNodes{}
		tr, _ := newTree(l, Options{NodeTimeout: 10 * time.Second})
		if _, err := tr.List(context.Background(), ""); !errors.Is(err, ErrUnavailable) {
			t.Errorf("List = %v, want ErrUnavailable", err)
		}
	})
}

// hungNodes is a Lister whose Nodes call never returns until its context ends.
type hungNodes struct{}

func (hungNodes) Nodes(ctx context.Context) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hungNodes) List(context.Context, string, string) ([]wire.Entry, error) {
	return nil, errDown
}
