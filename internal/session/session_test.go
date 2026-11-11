package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/BlueRival/splicefs/internal/wire"
)

func TestNewDoesNotUseTheNetwork(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	e.session()
	if got := e.dials(); len(got) != 0 {
		t.Errorf("New dialed %v", got)
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	o := New(Options{}).opts
	if o.RefreshNodes != 5*time.Minute || o.RenewBefore != 2*time.Minute {
		t.Errorf("intervals = %v, %v, want 5m, 2m", o.RefreshNodes, o.RenewBefore)
	}
	if o.Now == nil || o.Logger == nil {
		t.Error("Now and Logger must have defaults")
	}
	if o.IsUnauthorized(errDenied) || o.IsUnavailable(errDown) {
		t.Error("default predicates must report false")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	kept := New(Options{RefreshNodes: time.Second, RenewBefore: time.Minute, Logger: logger}).opts
	if kept.RefreshNodes != time.Second || kept.RenewBefore != time.Minute || kept.Logger != logger {
		t.Error("explicit options were overridden")
	}
}

func TestFirstUseConnects(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b", "c")
	s := e.session()

	checkNames(t, "Nodes", mustNodes(t, s), []string{"a", "b", "c"})

	if got := e.entry.loginCount(); got != 1 {
		t.Errorf("entry logins = %d, want 1", got)
	}
	checkNames(t, "entry list fetched with", e.entry.listingTokens(), []string{"tok-1"})
	want := []string{
		"entry https://entry.example.com/api",
		"a https://a.example.com/api",
		"b https://b.example.com/api",
		"c https://c.example.com/api",
	}
	checkNames(t, "dials", e.dials(), want)

	mustNodes(t, s)
	if got := e.entry.loginCount(); got != 1 {
		t.Errorf("entry logins after second call = %d, want 1", got)
	}
}

func TestEntryDownThenUpAfterBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	e.entry.setLogin(failWith(errDown))
	s := e.session()
	ctx := context.Background()

	_, err := s.Nodes(ctx)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, errDown) {
		t.Fatalf("first call = %v, want ErrUnavailable wrapping the cause", err)
	}

	// Within the 2s backoff the entry is not contacted again.
	e.clk.Advance(time.Second)
	if _, err := s.Nodes(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("call within backoff = %v, want ErrUnavailable", err)
	}
	if got := e.entry.loginCount(); got != 1 {
		t.Fatalf("entry logins = %d, want 1", got)
	}

	// The second failure doubles the wait to 4s.
	e.clk.Advance(time.Second)
	if _, err := s.Nodes(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second attempt = %v, want ErrUnavailable", err)
	}
	e.clk.Advance(3 * time.Second)
	if _, err := s.Nodes(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("call within doubled backoff = %v, want ErrUnavailable", err)
	}
	if got := e.entry.loginCount(); got != 2 {
		t.Fatalf("entry logins = %d, want 2", got)
	}

	e.entry.setLogin(e.grant)
	e.clk.Advance(time.Second)
	checkNames(t, "Nodes", mustNodes(t, s), []string{"a"})
	if got := e.entry.loginCount(); got != 3 {
		t.Errorf("entry logins = %d, want 3", got)
	}
}

func TestBadCredentialsAreRetriedOnlyAfterBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	e.entry.setLogin(failWith(errDenied))
	s := e.session()
	ctx := context.Background()

	_, err := s.Nodes(ctx)
	if !errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("Nodes = %v, want ErrBadCredentials only", err)
	}
	if _, err := s.Usable(ctx); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("Usable within backoff = %v, want ErrBadCredentials", err)
	}
	if got := e.entry.loginCount(); got != 1 {
		t.Fatalf("entry logins = %d, want 1", got)
	}

	e.entry.setLogin(e.grant)
	e.clk.Advance(2 * time.Second)
	checkNames(t, "Nodes", mustNodes(t, s), []string{"a"})
}

func TestConnectFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(e *env)
	}{
		{"entry cannot be dialed", func(e *env) { e.dialErr[entryName] = errDown }},
		{"node list cannot be fetched", func(e *env) { e.entry.setNodes(nodesFail(errDown)) }},
		{"node list is empty", func(e *env) { e.setList() }},
		{"no listed node can be dialed", func(e *env) { e.dialErr["a"] = errDown }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, "a")
			tt.setup(e)
			if _, err := e.session().Nodes(context.Background()); !errors.Is(err, ErrUnavailable) {
				t.Errorf("Nodes = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestNodesThatCannotBeDialedOrRepeatAreSkipped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b", "c")
	e.dialErr["b"] = errDown
	e.setList(
		wire.Node{Name: "a", URL: urlOf("a")},
		wire.Node{Name: "b", URL: urlOf("b")},
		wire.Node{Name: "a", URL: urlOf("a")},
		wire.Node{Name: "c", URL: urlOf("c")},
	)
	checkNames(t, "Nodes", mustNodes(t, e.session()), []string{"a", "c"})
}

func TestCancelledConnectDoesNotStartBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	e.entry.setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
		return wire.LoginResponse{}, ctx.Err()
	})
	s := e.session()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Nodes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Nodes = %v, want context.Canceled", err)
	}

	e.entry.setLogin(e.grant)
	checkNames(t, "Nodes", mustNodes(t, s), []string{"a"})
}

func TestClosedSessionFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")
	s.Close()
	ctx := context.Background()

	if _, err := s.Nodes(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Nodes = %v, want ErrClosed", err)
	}
	if _, err := s.Usable(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Usable = %v, want ErrClosed", err)
	}
	err := s.Do(ctx, a, func(context.Context, string) error { return nil })
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Do = %v, want ErrClosed", err)
	}
}

func TestTokenIsRenewedNearExpiry(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b")
	s := e.session()
	a := nodeOf(t, s, "a")

	if got := token(t, s, a); got != "tok-1" {
		t.Fatalf("first token = %q, want tok-1", got)
	}
	e.clk.Advance(57 * time.Minute)
	if got := token(t, s, a); got != "tok-1" {
		t.Fatalf("token with 3m left = %q, want tok-1", got)
	}
	// Expiring "within" the default 2m includes exactly 2m.
	e.clk.Advance(time.Minute)
	got := token(t, s, a)
	if got == "tok-1" {
		t.Fatalf("token with 2m left was not renewed")
	}
	if again := token(t, s, a); again != got {
		t.Errorf("token = %q after renewal, want %q again", again, got)
	}
	if n := e.entry.loginCount(); n != 1 {
		t.Errorf("entry logins = %d, want 1: renewal goes to the listed nodes", n)
	}
}

func TestRenewalFirstSuccessWinsAndCancelsTheRest(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "slow", "fast")
	s := e.session()
	slow, fast := nodeOf(t, s, "slow"), nodeOf(t, s, "fast")
	token(t, s, fast)

	cancelled := make(chan struct{})
	e.nodes["slow"].setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
		<-ctx.Done()
		close(cancelled)
		return wire.LoginResponse{}, ctx.Err()
	})
	e.nodes["fast"].setLogin(func(context.Context) (wire.LoginResponse, error) {
		return wire.LoginResponse{Token: "from-fast", Expires: e.clk.Now().Add(time.Hour)}, nil
	})
	e.clk.Advance(time.Hour)

	if got := token(t, s, slow); got != "from-fast" {
		t.Errorf("token = %q, want from-fast", got)
	}
	select {
	case <-cancelled:
	default:
		t.Error("the slow login was not cancelled before the call returned")
	}
}

func TestRenewalFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		a, b    error
		want    error
		notWant error
	}{
		{"every node down", errDown, errDown, ErrUnavailable, ErrBadCredentials},
		{"one rejects the credentials", errDown, errDenied, ErrBadCredentials, ErrUnavailable},
		{"every node rejects the credentials", errDenied, errDenied, ErrBadCredentials, ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, "a", "b")
			s := e.session()
			a := nodeOf(t, s, "a")
			token(t, s, a)
			e.nodes["a"].setLogin(failWith(tt.a))
			e.nodes["b"].setLogin(failWith(tt.b))
			e.clk.Advance(time.Hour)

			err := s.Do(context.Background(), a, func(context.Context, string) error {
				t.Error("call made without a token")
				return nil
			})
			if !errors.Is(err, tt.want) || errors.Is(err, tt.notWant) {
				t.Errorf("Do = %v, want %v and not %v", err, tt.want, tt.notWant)
			}
		})
	}
}

func TestFailedRenewalIsNotRetriedWithinBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")
	token(t, s, a)
	e.nodes["a"].setLogin(failWith(errDown))
	e.clk.Advance(time.Hour)

	for range 2 {
		err := s.Do(context.Background(), a, func(context.Context, string) error { return nil })
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Do = %v, want ErrUnavailable", err)
		}
	}
	if got := e.nodes["a"].loginCount(); got != 1 {
		t.Errorf("logins = %d, want 1 within the backoff", got)
	}

	e.nodes["a"].setLogin(e.grant)
	e.clk.Advance(2 * time.Second)
	if got := token(t, s, a); got == "tok-1" {
		t.Errorf("token = %q, want a renewed token", got)
	}
}

func TestCancelledRenewalDoesNotStartBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")
	token(t, s, a)
	e.nodes["a"].setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
		<-ctx.Done()
		return wire.LoginResponse{}, ctx.Err()
	})
	e.clk.Advance(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Do(ctx, a, func(context.Context, string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", err)
	}

	e.nodes["a"].setLogin(e.grant)
	if got := token(t, s, a); got == "tok-1" {
		t.Errorf("token = %q, want a renewed token", got)
	}
}

func TestLoginAnyWithoutNodes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.session().loginAny(context.Background(), nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("loginAny = %v, want ErrUnavailable", err)
	}
}

func TestRenewSkipsLoginWhenTokenWasAlreadyReplaced(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")
	current := token(t, s, a)

	got, err := s.renew(context.Background(), "an-older-token")
	if err != nil || got != current {
		t.Errorf("renew = %q, %v, want %q, nil", got, err, current)
	}
	if n := e.nodes["a"].loginCount(); n != 0 {
		t.Errorf("logins = %d, want 0", n)
	}
}

func TestDoRetriesOnceWhenUnauthorized(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")

	var used []string
	err := s.Do(context.Background(), a, func(_ context.Context, tok string) error {
		used = append(used, tok)
		if len(used) == 1 {
			return errDenied
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if len(used) != 2 || used[0] != "tok-1" || used[1] == "tok-1" {
		t.Errorf("tokens used = %v, want the old token, then a new one", used)
	}
}

func TestDoRetriesOnlyOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")

	calls := 0
	err := s.Do(context.Background(), a, func(context.Context, string) error {
		calls++
		return errDenied
	})
	if !errors.Is(err, errDenied) || calls != 2 {
		t.Errorf("Do = %v after %d calls, want errDenied after 2", err, calls)
	}
}

func TestDoReturnsTheFailureToRenewTheToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")
	e.nodes["a"].setLogin(failWith(errDenied))

	calls := 0
	err := s.Do(context.Background(), a, func(context.Context, string) error {
		calls++
		return errDenied
	})
	if !errors.Is(err, ErrBadCredentials) || calls != 1 {
		t.Errorf("Do = %v after %d calls, want ErrBadCredentials after 1", err, calls)
	}
}

func TestDoReturnsOtherErrorsUnchanged(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	a := nodeOf(t, s, "a")
	other := errors.New("no such directory")

	if err := s.Do(context.Background(), a, func(context.Context, string) error { return other }); !errors.Is(err, other) {
		t.Errorf("Do = %v, want the call's own error", err)
	}
}

func TestDoWithUnknownNodeStillRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	gone := &fakeNode{name: "gone"}
	if err := s.Do(context.Background(), gone, func(context.Context, string) error { return errDown }); !errors.Is(err, errDown) {
		t.Errorf("Do = %v, want errDown", err)
	}
	checkNames(t, "Usable", mustUsable(t, s), []string{"a"})
}

func TestNodeCooldownDoublesAndResets(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b")
	s := e.session()
	a := nodeOf(t, s, "a")
	checkNames(t, "Usable", mustUsable(t, s), []string{"a", "b"})

	fail(t, s, a)
	checkNames(t, "Usable after one failure", mustUsable(t, s), []string{"b"})
	e.clk.Advance(2 * time.Second)
	checkNames(t, "Usable after 2s", mustUsable(t, s), []string{"a", "b"})

	fail(t, s, a)
	e.clk.Advance(3 * time.Second)
	checkNames(t, "Usable 3s into a 4s cooldown", mustUsable(t, s), []string{"b"})
	e.clk.Advance(time.Second)
	checkNames(t, "Usable after 4s", mustUsable(t, s), []string{"a", "b"})

	// Failures keep doubling to 8s, 16s, 32s and stay at 60s.
	for _, cooldown := range []time.Duration{8, 16, 32, 60, 60} {
		fail(t, s, a)
		e.clk.Advance(cooldown*time.Second - time.Second)
		checkNames(t, "Usable just before the cooldown ends", mustUsable(t, s), []string{"b"})
		e.clk.Advance(time.Second)
		checkNames(t, "Usable when the cooldown ends", mustUsable(t, s), []string{"a", "b"})
	}

	fail(t, s, a)
	token(t, s, a)
	checkNames(t, "Usable after a success", mustUsable(t, s), []string{"a", "b"})
	fail(t, s, a)
	e.clk.Advance(2 * time.Second)
	checkNames(t, "Usable after a reset and one failure", mustUsable(t, s), []string{"a", "b"})
}

func TestAnsweringWithAnErrorDoesNotCoolANode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b")
	s := e.session()
	a := nodeOf(t, s, "a")
	fail(t, s, a)
	other := errors.New("no such directory")
	if err := s.Do(context.Background(), a, func(context.Context, string) error { return other }); !errors.Is(err, other) {
		t.Fatalf("Do = %v, want the call's own error", err)
	}
	// The cooldown from the earlier failure is neither extended nor cleared.
	checkNames(t, "Usable", mustUsable(t, s), []string{"b"})
	e.clk.Advance(2 * time.Second)
	checkNames(t, "Usable", mustUsable(t, s), []string{"a", "b"})
}

func TestUsableWhenEveryNodeIsCoolingReturnsTheSoonest(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b", "c")
	s := e.session()
	a, b, c := nodeOf(t, s, "a"), nodeOf(t, s, "b"), nodeOf(t, s, "c")

	fail(t, s, b)
	e.clk.Advance(time.Second)
	fail(t, s, a)
	fail(t, s, a)
	fail(t, s, c)

	// b's cooldown ends at 2s, a's at 5s, c's at 3s.
	checkNames(t, "Usable", mustUsable(t, s), []string{"b"})
	checkNames(t, "Nodes", mustNodes(t, s), []string{"a", "b", "c"})
}

func TestNodeListRefreshAddsAndDropsNodes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b", "c")
	opts := e.options()
	opts.RefreshNodes = 30 * time.Second
	s := New(opts)
	ctx := context.Background()
	checkNames(t, "Nodes", mustNodes(t, s), []string{"a", "b", "c"})
	a, b := nodeOf(t, s, "a"), nodeOf(t, s, "b")
	for range 6 {
		fail(t, s, b)
	}

	e.addNode("d")
	e.setList(
		wire.Node{Name: "d", URL: urlOf("d")},
		wire.Node{Name: "b", URL: urlOf("b")},
		wire.Node{Name: "a", URL: urlOf("a")},
	)

	e.clk.Advance(29 * time.Second)
	checkNames(t, "Nodes before the interval", mustNodes(t, s), []string{"a", "b", "c"})
	e.clk.Advance(time.Second)
	checkNames(t, "Nodes after the interval", mustNodes(t, s), []string{"d", "b", "a"})

	if got := nodeOf(t, s, "a"); got != a {
		t.Error("node a was dialed again; existing nodes must be kept")
	}
	// b was cooling for 60s when the list was fetched 30s later, and still is.
	checkNames(t, "Usable", mustUsable(t, s), []string{"d", "a"})
	if got := slices.Index(e.dials(), "d "+urlOf("d")); got < 0 {
		t.Errorf("dials = %v, want d dialed", e.dials())
	}
	if _, err := s.Nodes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.nodes["a"].listingTokens(); len(got) != 1 {
		t.Errorf("node list fetched %d times from a, want once", len(got))
	}
}

func TestNodeListRefreshRedialsANodeWhoseURLChanged(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	opts := e.options()
	opts.RefreshNodes = time.Minute
	s := New(opts)
	before := nodeOf(t, s, "a")
	fail(t, s, before)

	e.setList(wire.Node{Name: "a", URL: "https://moved.example.com/api"})
	e.clk.Advance(time.Minute)
	mustNodes(t, s)

	if got := e.dials(); got[len(got)-1] != "a https://moved.example.com/api" {
		t.Errorf("dials = %v, want a dialed at its new URL", got)
	}
	checkNames(t, "Usable", mustUsable(t, s), []string{"a"})
}

func TestFailedNodeListRefreshKeepsTheOldListUntilTheNextInterval(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a", "b")
	opts := e.options()
	opts.RefreshNodes = time.Minute
	s := New(opts)
	checkNames(t, "Nodes", mustNodes(t, s), []string{"a", "b"})

	e.nodes["a"].setNodes(nodesFail(errDown))
	e.setList(wire.Node{Name: "b", URL: urlOf("b")})
	e.clk.Advance(time.Minute)

	checkNames(t, "Nodes after a failed refresh", mustNodes(t, s), []string{"a", "b"})
	e.nodes["a"].setNodes(e.serveList)
	e.clk.Advance(30 * time.Second)
	checkNames(t, "Nodes within the interval", mustNodes(t, s), []string{"a", "b"})
	if got := len(e.nodes["a"].listingTokens()); got != 1 {
		t.Errorf("a was asked %d times before the next interval, want 1", got)
	}
	e.clk.Advance(30 * time.Second)
	checkNames(t, "Nodes after the next interval", mustNodes(t, s), []string{"b"})
}

func TestNodeListRefreshIgnoresAListWithNoUsableNode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	opts := e.options()
	opts.RefreshNodes = time.Minute
	s := New(opts)
	mustNodes(t, s)

	e.setList(wire.Node{Name: "z", URL: urlOf("z")})
	e.clk.Advance(time.Minute)
	checkNames(t, "Nodes", mustNodes(t, s), []string{"a"})
}

func TestConnectWhenAlreadyConnectedDoesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "a")
	s := e.session()
	mustNodes(t, s)
	if err := s.connect(context.Background()); err != nil {
		t.Fatalf("connect = %v, want nil", err)
	}
	if got := e.entry.loginCount(); got != 1 {
		t.Errorf("entry logins = %d, want 1", got)
	}
}
