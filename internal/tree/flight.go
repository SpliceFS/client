package tree

import (
	"context"
	"fmt"
	"sync"

	"github.com/BlueRival/splicefs/internal/wire"
)

// flight is one fetch of a directory, shared by every caller that asks for it while
// it runs. recs and err are set before done is closed.
type flight struct {
	done chan struct{}
	recs []record
	err  error
}

// wait returns the result of the flight, or the error of ctx if that ends first.
// A result that is already there wins over a context that has ended.
func (f *flight) wait(ctx context.Context, dir string) ([]record, error) {
	select {
	case <-f.done:
		return f.recs, f.err
	default:
	}
	select {
	case <-f.done:
		return f.recs, f.err
	case <-ctx.Done():
		return nil, fmt.Errorf("list %q: %w", dir, ctx.Err())
	}
}

// acquire answers from memory when it can, and otherwise joins the fetch of dir that
// is running or starts one. The fetch does not depend on the caller's context
// ending, because other callers may be waiting for it; it is bounded by NodeTimeout.
func (t *Tree) acquire(ctx context.Context, dir string) *flight {
	t.mu.Lock()
	defer t.mu.Unlock()
	if it, ok := t.items[dir]; ok && t.fresh(it) {
		t.order.MoveToFront(it.elem)
		return it.answer(dir)
	}
	if f, ok := t.flights[dir]; ok {
		return f
	}
	f := &flight{done: make(chan struct{})}
	t.flights[dir] = f
	go t.run(context.WithoutCancel(ctx), dir, f)
	return f
}

// answer is the remembered item as a flight that has already finished.
func (it *item) answer(dir string) *flight {
	f := &flight{done: make(chan struct{}), recs: it.recs}
	if it.missing {
		f.err = notFound(dir)
	}
	close(f.done)
	return f
}

func (t *Tree) run(ctx context.Context, dir string, f *flight) {
	f.recs, f.err = t.settle(dir, t.ask(ctx, dir))
	t.mu.Lock()
	delete(t.flights, dir)
	t.mu.Unlock()
	close(f.done)
}

type state int

const (
	unanswered state = iota // the node did not answer
	absent                  // the node answered that it has no such directory
	listed                  // the node answered with a listing, possibly empty
)

type reply struct {
	node    string
	state   state
	entries []wire.Entry
}

// ask lists dir on every node at the same time and returns when all have answered
// or timed out. No node is waited for beyond NodeTimeout, and no goroutine outlives
// the call.
func (t *Tree) ask(ctx context.Context, dir string) []reply {
	nodes, err := t.nodes(ctx)
	if err != nil {
		return nil
	}
	replies := make([]reply, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[i] = t.askOne(ctx, node, dir)
		}()
	}
	wg.Wait()
	return replies
}

func (t *Tree) nodes(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opts.NodeTimeout)
	defer cancel()
	return t.lister.Nodes(ctx)
}

func (t *Tree) askOne(ctx context.Context, node, dir string) reply {
	ctx, cancel := context.WithTimeout(ctx, t.opts.NodeTimeout)
	defer cancel()
	entries, err := t.lister.List(ctx, node, dir)
	switch {
	case err == nil:
		return reply{node: node, state: listed, entries: entries}
	case t.opts.IsNotFound(err):
		return reply{node: node, state: absent}
	default:
		return reply{node: node, state: unanswered}
	}
}

// settle turns the replies into the result for dir and remembers it. A directory
// that no answering node has does not exist; when nobody answered, the previous
// listing is better than none, however old.
func (t *Tree) settle(dir string, replies []reply) ([]record, error) {
	var have []reply
	var answered int
	silentNodes := make(map[string]bool)
	for _, r := range replies {
		switch r.state {
		case listed:
			have = append(have, r)
			answered++
		case absent:
			answered++
		default:
			silentNodes[r.node] = true
		}
	}
	prev, hadPrev := t.previous(dir)
	switch {
	case answered == 0 && hadPrev:
		return prev, nil
	case answered == 0:
		return nil, fmt.Errorf("list %q: %w", dir, ErrUnavailable)
	case len(have) == 0:
		t.store(dir, nil, true)
		return nil, notFound(dir)
	}
	recs := carry(prev, merge(have), silentNodes)
	t.store(dir, recs, false)
	return recs, nil
}
