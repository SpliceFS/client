package session

import (
	"context"
	"sync"
)

// group makes concurrent callers share one run of a function. The first caller runs
// it with its own context; the others wait for the result.
type group struct {
	mu  sync.Mutex
	cur *call
}

type call struct {
	done chan struct{}
	err  error
	// abandoned is true when the caller that ran the function gave up through its
	// context, which says nothing about what the waiters would have got.
	abandoned bool
}

func (g *group) do(ctx context.Context, fn func(ctx context.Context) error) error {
	for {
		g.mu.Lock()
		c := g.cur
		if c == nil {
			c = &call{done: make(chan struct{})}
			g.cur = c
			g.mu.Unlock()
			return g.lead(ctx, c, fn)
		}
		g.mu.Unlock()

		select {
		case <-c.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if !c.abandoned || ctx.Err() != nil {
			return c.err
		}
	}
}

func (g *group) lead(ctx context.Context, c *call, fn func(ctx context.Context) error) error {
	c.err = fn(ctx)
	c.abandoned = ctx.Err() != nil
	g.mu.Lock()
	g.cur = nil
	g.mu.Unlock()
	close(c.done)
	return c.err
}
