package core

import (
	"context"
	"sync"
	"time"
)

// nodeTimeout bounds every single request of a fan-out to one node: a node that does
// not answer in time is treated as not answering, so it cannot hold up the others.
const nodeTimeout = 15 * time.Second

// fanOut calls call for every item at the same time, each under nodeTimeout and under
// ctx, and returns the results in item order. It returns once every call has, so no
// goroutine outlives it; the calls end early when ctx does.
func fanOut[T, R any](ctx context.Context, items []T, call func(context.Context, T) R) []R {
	results := make([]R, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
			defer cancel()
			results[i] = call(ctx, item)
		})
	}
	wg.Wait()
	return results
}
