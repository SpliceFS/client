package session

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SpliceFS/common/wire"
)

func TestConcurrentCallersShareOneLoginWhenTheTokenExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, "a", "b")
		s := e.session()
		a := nodeOf(t, s, "a")
		token(t, s, a)

		gate := make(chan struct{})
		e.entry.setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
			<-gate
			return e.grant(ctx)
		})
		e.clk.Advance(time.Hour)

		const callers = 5
		tokens := make([]string, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				_ = s.Do(context.Background(), a, func(_ context.Context, tok string) error {
					tokens[i] = tok
					return nil
				})
			})
		}
		synctest.Wait()
		close(gate)
		wg.Wait()

		for i, tok := range tokens {
			if tok != tokens[0] || tok == "tok-1" {
				t.Errorf("caller %d got token %q, want one shared new token (first was %q)", i, tok, tokens[0])
			}
		}
		if got := e.entry.loginCount(); got != 2 {
			t.Errorf("cluster host logins = %d, want 2: the first and one shared renewal", got)
		}
		if got := e.nodeLogins(); got != 0 {
			t.Errorf("node logins = %d, want 0", got)
		}
	})
}

func TestConcurrentFirstCallsConnectOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, "a")
		gate := make(chan struct{})
		e.entry.setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
			<-gate
			return e.grant(ctx)
		})
		s := e.session()

		var wg sync.WaitGroup
		for range 3 {
			wg.Go(func() { mustNodes(t, s) })
		}
		synctest.Wait()
		close(gate)
		wg.Wait()

		if got := e.entry.loginCount(); got != 1 {
			t.Errorf("entry logins = %d, want 1", got)
		}
	})
}
