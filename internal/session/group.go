package session

import (
	"context"
	"fmt"
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

func (g *group) do(ctx context.Context, run func(ctx context.Context) error) error {
	for {
		g.mu.Lock()
		flight := g.cur
		if flight == nil {
			flight = &call{done: make(chan struct{})}
			g.cur = flight
			g.mu.Unlock()
			return g.lead(ctx, flight, run)
		}
		g.mu.Unlock()

		select {
		case <-flight.done:
		case <-ctx.Done():
			return fmt.Errorf("wait for the running call: %w", ctx.Err())
		}
		if !flight.abandoned || ctx.Err() != nil {
			return flight.err
		}
	}
}

func (g *group) lead(ctx context.Context, flight *call, run func(ctx context.Context) error) error {
	flight.err = run(ctx)
	flight.abandoned = ctx.Err() != nil
	g.mu.Lock()
	g.cur = nil
	g.mu.Unlock()
	close(flight.done)
	return flight.err
}
