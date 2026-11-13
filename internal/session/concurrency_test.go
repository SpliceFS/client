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
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cluster := newEnv(t, "a", "b")
		session := cluster.session()
		nodeA := nodeOf(t, session, "a")
		token(t, session, nodeA)

		gate := make(chan struct{})
		cluster.entry.setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
			<-gate
			return cluster.grant(ctx)
		})
		cluster.clk.Advance(time.Hour)

		tokens := callConcurrently(t, session, nodeA, 5, gate)

		for i, tok := range tokens {
			if tok != tokens[0] || tok == "tok-1" {
				t.Errorf("caller %d got token %q, want one shared new token (first was %q)", i, tok, tokens[0])
			}
		}
		if got := cluster.entry.loginCount(); got != 2 {
			t.Errorf("cluster host logins = %d, want 2: the first and one shared renewal", got)
		}
		if got := cluster.nodeLogins(); got != 0 {
			t.Errorf("node logins = %d, want 0", got)
		}
	})
}

// callConcurrently makes callers calls of Do on the node, waits until every one of them is
// blocked on the gate, opens the gate and returns the token each caller was given.
func callConcurrently(t *testing.T, session *Session, node Node, callers int, gate chan struct{}) []string {
	t.Helper()
	tokens := make([]string, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			err := session.Do(context.Background(), node, func(_ context.Context, tok string) error {
				tokens[i] = tok
				return nil
			})
			if err != nil {
				t.Errorf("caller %d: Do = %v", i, err)
			}
		})
	}
	synctest.Wait()
	close(gate)
	wg.Wait()
	return tokens
}

func TestConcurrentFirstCallsConnectOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cluster := newEnv(t, "a")
		gate := make(chan struct{})
		cluster.entry.setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
			<-gate
			return cluster.grant(ctx)
		})
		session := cluster.session()

		var wg sync.WaitGroup
		for range 3 {
			wg.Go(func() { mustNodes(t, session) })
		}
		synctest.Wait()
		close(gate)
		wg.Wait()

		if got := cluster.entry.loginCount(); got != 1 {
			t.Errorf("entry logins = %d, want 1", got)
		}
	})
}
