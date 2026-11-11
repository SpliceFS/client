package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestGroupSharesOneRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g group
		var runs atomic.Int32
		gate := make(chan struct{})
		fn := func(context.Context) error {
			runs.Add(1)
			<-gate
			return errDown
		}
		errs := make(chan error, 2)
		for range 2 {
			go func() { errs <- g.do(context.Background(), fn) }()
		}
		synctest.Wait()
		close(gate)
		for range 2 {
			if err := <-errs; !errors.Is(err, errDown) {
				t.Errorf("do = %v, want errDown", err)
			}
		}
		if got := runs.Load(); got != 1 {
			t.Errorf("function ran %d times, want 1", got)
		}
	})
}

func TestGroupWaiterRunsAgainWhenLeaderGaveUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g group
		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		leaderStarted := make(chan struct{})
		leaderErr := make(chan error, 1)
		go func() {
			leaderErr <- g.do(leaderCtx, func(ctx context.Context) error {
				close(leaderStarted)
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		<-leaderStarted

		waiterErr := make(chan error, 1)
		go func() {
			waiterErr <- g.do(context.Background(), func(context.Context) error { return errDown })
		}()
		synctest.Wait()
		cancelLeader()

		if err := <-leaderErr; !errors.Is(err, context.Canceled) {
			t.Errorf("leader = %v, want context.Canceled", err)
		}
		if err := <-waiterErr; !errors.Is(err, errDown) {
			t.Errorf("waiter = %v, want its own run to return errDown", err)
		}
	})
}

func TestGroupWaiterStopsWaitingWhenItsContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var g group
		gate := make(chan struct{})
		leaderErr := make(chan error, 1)
		go func() {
			leaderErr <- g.do(context.Background(), func(context.Context) error {
				<-gate
				return nil
			})
		}()
		synctest.Wait()

		ctx, cancel := context.WithCancel(context.Background())
		waiterErr := make(chan error, 1)
		go func() { waiterErr <- g.do(ctx, nil) }()
		synctest.Wait()
		cancel()
		if err := <-waiterErr; !errors.Is(err, context.Canceled) {
			t.Errorf("waiter = %v, want context.Canceled", err)
		}

		close(gate)
		if err := <-leaderErr; err != nil {
			t.Errorf("leader = %v, want nil", err)
		}
	})
}
