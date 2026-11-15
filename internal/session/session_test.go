package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SpliceFS/common/wire"
)

func TestNewDoesNotUseTheNetwork(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	cluster.session()
	if got := cluster.dials(); len(got) != 0 {
		t.Errorf("New dialed %v", got)
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	defaults := New(Options{}).opts
	if defaults.RefreshNodes != time.Minute || defaults.RenewBefore != 2*time.Minute {
		t.Errorf("intervals = %v, %v, want 1m, 2m", defaults.RefreshNodes, defaults.RenewBefore)
	}
	if defaults.Now == nil || defaults.Logger == nil {
		t.Error("Now and Logger must have defaults")
	}
	if defaults.IsUnauthorized(errDenied) || defaults.IsUnavailable(errDown) {
		t.Error("default predicates must report false")
	}
	logger := slog.New(slog.DiscardHandler)
	kept := New(Options{RefreshNodes: time.Second, RenewBefore: time.Minute, Logger: logger}).opts
	if kept.RefreshNodes != time.Second || kept.RenewBefore != time.Minute || kept.Logger != logger {
		t.Error("explicit options were overridden")
	}
}

func TestNewDefaultTreatsNoErrorAsAVersionRefusal(t *testing.T) {
	t.Parallel()
	if New(Options{}).opts.IsIncompatible(errOld) {
		t.Error("the default predicate must report false")
	}
}

func TestFirstUseConnects(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b", "c")
	session := cluster.session()

	checkNames(t, "Nodes", mustNodes(t, session), []string{"a", "b", "c"})

	if got := cluster.entry.loginCount(); got != 1 {
		t.Errorf("entry logins = %d, want 1", got)
	}
	checkNames(t, "entry list fetched with", cluster.entry.listingTokens(), []string{"tok-1"})
	want := []string{
		"entry https://entry.example.com/api",
		"a https://a.example.com/api",
		"b https://b.example.com/api",
		"c https://c.example.com/api",
	}
	checkNames(t, "dials", cluster.dials(), want)

	mustNodes(t, session)
	if got := cluster.entry.loginCount(); got != 1 {
		t.Errorf("entry logins after second call = %d, want 1", got)
	}
}

func TestEntryDownThenUpAfterBackoff(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	cluster.entry.setLogin(failWith(errDown))
	session := cluster.session()
	ctx := context.Background()

	_, err := session.Nodes(ctx)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, errDown) {
		t.Fatalf("first call = %v, want ErrUnavailable wrapping the cause", err)
	}

	// Within the 2s backoff the entry is not contacted again.
	cluster.clk.Advance(time.Second)
	if _, err := session.Nodes(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("call within backoff = %v, want ErrUnavailable", err)
	}
	if got := cluster.entry.loginCount(); got != 1 {
		t.Fatalf("entry logins = %d, want 1", got)
	}

	// The second failure doubles the wait to 4s.
	cluster.clk.Advance(time.Second)
	if _, err := session.Nodes(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second attempt = %v, want ErrUnavailable", err)
	}
	cluster.clk.Advance(3 * time.Second)
	if _, err := session.Nodes(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("call within doubled backoff = %v, want ErrUnavailable", err)
	}
	if got := cluster.entry.loginCount(); got != 2 {
		t.Fatalf("entry logins = %d, want 2", got)
	}

	cluster.entry.setLogin(cluster.grant)
	cluster.clk.Advance(time.Second)
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a"})
	if got := cluster.entry.loginCount(); got != 3 {
		t.Errorf("entry logins = %d, want 3", got)
	}
}

func TestBadCredentialsAreRetriedOnlyAfterBackoff(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	cluster.entry.setLogin(failWith(errDenied))
	session := cluster.session()
	ctx := context.Background()

	_, err := session.Nodes(ctx)
	if !errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("Nodes = %v, want ErrBadCredentials only", err)
	}
	if _, err := session.Usable(ctx); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("Usable within backoff = %v, want ErrBadCredentials", err)
	}
	if got := cluster.entry.loginCount(); got != 1 {
		t.Fatalf("entry logins = %d, want 1", got)
	}

	cluster.entry.setLogin(cluster.grant)
	cluster.clk.Advance(2 * time.Second)
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a"})
}

func TestRefusedVersionAtLoginKeepsTheMessage(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	cluster.entry.setLogin(failWith(errOld))
	session := cluster.session()

	_, err := session.Nodes(context.Background())
	if !errors.Is(err, ErrIncompatible) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBadCredentials) {
		t.Fatalf("Nodes = %v, want ErrIncompatible only", err)
	}
	if !strings.Contains(err.Error(), errOld.Error()) {
		t.Errorf("Nodes = %q, want the refusal's message in it", err)
	}
}

func TestRefusedNodeRestsLikeANodeThatIsDown(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	err := session.Do(context.Background(), nodeA, func(context.Context, string) error { return errOld })
	if !errors.Is(err, errOld) {
		t.Fatalf("Do = %v, want the refusal", err)
	}
	checkNames(t, "Usable after a refusal", mustUsable(t, session), []string{"b"})
}

func TestConnectFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(cluster *env)
	}{
		{"entry cannot be dialed", func(cluster *env) { cluster.dialErr[entryName] = errDown }},
		{"node list cannot be fetched", func(cluster *env) { cluster.entry.setNodes(nodesFail(errDown)) }},
		{"no listed node can be dialed", func(cluster *env) { cluster.dialErr["a"] = errDown }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cluster := newEnv(t, "a")
			tt.setup(cluster)
			if _, err := cluster.session().Nodes(context.Background()); !errors.Is(err, ErrUnavailable) {
				t.Errorf("Nodes = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestNodesThatCannotBeDialedOrRepeatAreSkipped(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b", "c")
	cluster.dialErr["b"] = errDown
	cluster.setList(
		wire.Node{Name: "a", URL: urlOf("a")},
		wire.Node{Name: "b", URL: urlOf("b")},
		wire.Node{Name: "a", URL: urlOf("a")},
		wire.Node{Name: "c", URL: urlOf("c")},
	)
	checkNames(t, "Nodes", mustNodes(t, cluster.session()), []string{"a", "c"})
}

func TestCancelledConnectDoesNotStartBackoff(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	cluster.entry.setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
		return wire.LoginResponse{}, ctx.Err()
	})
	session := cluster.session()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.Nodes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Nodes = %v, want context.Canceled", err)
	}

	cluster.entry.setLogin(cluster.grant)
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a"})
}

func TestClosedSessionFails(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	session.Close()
	ctx := context.Background()

	if _, err := session.Nodes(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Nodes = %v, want ErrClosed", err)
	}
	if _, err := session.Usable(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Usable = %v, want ErrClosed", err)
	}
	err := session.Do(ctx, nodeA, func(context.Context, string) error { return nil })
	if !errors.Is(err, ErrClosed) {
		t.Errorf("Do = %v, want ErrClosed", err)
	}
}

func TestTokenIsRenewedNearExpiry(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")

	if got := token(t, session, nodeA); got != "tok-1" {
		t.Fatalf("first token = %q, want tok-1", got)
	}
	cluster.clk.Advance(57 * time.Minute)
	if got := token(t, session, nodeA); got != "tok-1" {
		t.Fatalf("token with 3m left = %q, want tok-1", got)
	}
	// Expiring "within" the default 2m includes exactly 2m.
	cluster.clk.Advance(time.Minute)
	got := token(t, session, nodeA)
	if got == "tok-1" {
		t.Fatal("token with 2m left was not renewed")
	}
	if again := token(t, session, nodeA); again != got {
		t.Errorf("token = %q after renewal, want %q again", again, got)
	}
	if n := cluster.entry.loginCount(); n != 2 {
		t.Errorf("cluster host logins = %d, want 2: the first and the renewal", n)
	}
	if n := cluster.nodeLogins(); n != 0 {
		t.Errorf("node logins = %d, want 0: renewal goes to the cluster host", n)
	}
	if n := cluster.dialCount(entryName); n != 1 {
		t.Errorf("the cluster host was dialed %d times, want once and kept", n)
	}
}

func TestRenewalFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		login   error
		want    error
		notWant error
	}{
		{"cluster host unreachable", errDown, ErrUnavailable, ErrBadCredentials},
		{"cluster host fails otherwise", errors.New("server error"), ErrUnavailable, ErrBadCredentials},
		{"cluster host rejects the credentials", errDenied, ErrBadCredentials, ErrUnavailable},
		{"cluster host refuses its version", errOld, ErrIncompatible, ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cluster := newEnv(t, "a", "b")
			session := cluster.session()
			nodeA := nodeOf(t, session, "a")
			token(t, session, nodeA)
			cluster.entry.setLogin(failWith(tt.login))
			cluster.clk.Advance(time.Hour)

			err := session.Do(context.Background(), nodeA, func(context.Context, string) error {
				t.Error("call made without a token")
				return nil
			})
			if !errors.Is(err, tt.want) || errors.Is(err, tt.notWant) {
				t.Errorf("Do = %v, want %v and not %v", err, tt.want, tt.notWant)
			}
			if got := cluster.nodeLogins() + cluster.nodeListings(); got != 0 {
				t.Errorf("node calls = %d, want 0: the nodes are not asked", got)
			}
		})
	}
}

func TestFailedRenewalIsNotRetriedWithinBackoff(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	token(t, session, nodeA)
	cluster.entryDown()
	cluster.clk.Advance(time.Hour)

	for range 2 {
		err := session.Do(context.Background(), nodeA, func(context.Context, string) error { return nil })
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Do = %v, want ErrUnavailable", err)
		}
	}
	if got := cluster.entry.loginCount(); got != 2 {
		t.Errorf("cluster host logins = %d, want 2: the first and one failed renewal", got)
	}

	cluster.entry.setLogin(cluster.grant)
	cluster.clk.Advance(2 * time.Second)
	if got := token(t, session, nodeA); got == "tok-1" {
		t.Errorf("token = %q, want a renewed token", got)
	}
}

func TestCancelledRenewalDoesNotStartBackoff(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	token(t, session, nodeA)
	cluster.entry.setLogin(func(ctx context.Context) (wire.LoginResponse, error) {
		<-ctx.Done()
		return wire.LoginResponse{}, ctx.Err()
	})
	cluster.clk.Advance(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := session.Do(ctx, nodeA, func(context.Context, string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", err)
	}

	cluster.entry.setLogin(cluster.grant)
	if got := token(t, session, nodeA); got == "tok-1" {
		t.Errorf("token = %q, want a renewed token", got)
	}
}

func TestRenewSkipsLoginWhenTokenWasAlreadyReplaced(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	current := token(t, session, nodeA)

	got, err := session.renew(context.Background(), "an-older-token")
	if err != nil || got != current {
		t.Errorf("renew = %q, %v, want %q, nil", got, err, current)
	}
	if n := cluster.entry.loginCount(); n != 1 {
		t.Errorf("cluster host logins = %d, want 1: only the first", n)
	}
}

func TestDoRetriesOnceWhenUnauthorized(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")

	var used []string
	err := session.Do(context.Background(), nodeA, func(_ context.Context, tok string) error {
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
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")

	calls := 0
	err := session.Do(context.Background(), nodeA, func(context.Context, string) error {
		calls++
		return errDenied
	})
	if !errors.Is(err, errDenied) || calls != 2 {
		t.Errorf("Do = %v after %d calls, want errDenied after 2", err, calls)
	}
}

func TestDoReturnsTheFailureToRenewTheToken(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	cluster.entry.setLogin(failWith(errDenied))

	calls := 0
	err := session.Do(context.Background(), nodeA, func(context.Context, string) error {
		calls++
		return errDenied
	})
	if !errors.Is(err, ErrBadCredentials) || calls != 1 {
		t.Errorf("Do = %v after %d calls, want ErrBadCredentials after 1", err, calls)
	}
}

func TestDoReturnsOtherErrorsUnchanged(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	other := errors.New("no such directory")

	err := session.Do(context.Background(), nodeA, func(context.Context, string) error { return other })
	if !errors.Is(err, other) {
		t.Errorf("Do = %v, want the call's own error", err)
	}
}

func TestDoWithUnknownNodeStillRuns(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	gone := &fakeNode{name: "gone"}
	err := session.Do(context.Background(), gone, func(context.Context, string) error { return errDown })
	if !errors.Is(err, errDown) {
		t.Errorf("Do = %v, want errDown", err)
	}
	checkNames(t, "Usable", mustUsable(t, session), []string{"a"})
}

func TestNodeCooldownDoublesAndResets(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	checkNames(t, "Usable", mustUsable(t, session), []string{"a", "b"})

	fail(t, session, nodeA)
	checkNames(t, "Usable after one failure", mustUsable(t, session), []string{"b"})
	cluster.clk.Advance(2 * time.Second)
	checkNames(t, "Usable after 2s", mustUsable(t, session), []string{"a", "b"})

	fail(t, session, nodeA)
	cluster.clk.Advance(3 * time.Second)
	checkNames(t, "Usable 3s into a 4s cooldown", mustUsable(t, session), []string{"b"})
	cluster.clk.Advance(time.Second)
	checkNames(t, "Usable after 4s", mustUsable(t, session), []string{"a", "b"})

	// Failures keep doubling to 8s, 16s, 32s and stay at 60s.
	cooldowns := []time.Duration{8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for _, cooldown := range cooldowns {
		fail(t, session, nodeA)
		cluster.clk.Advance(cooldown - time.Second)
		checkNames(t, "Usable just before the cooldown ends", mustUsable(t, session), []string{"b"})
		cluster.clk.Advance(time.Second)
		checkNames(t, "Usable when the cooldown ends", mustUsable(t, session), []string{"a", "b"})
	}

	fail(t, session, nodeA)
	token(t, session, nodeA)
	checkNames(t, "Usable after a success", mustUsable(t, session), []string{"a", "b"})
	fail(t, session, nodeA)
	cluster.clk.Advance(2 * time.Second)
	checkNames(t, "Usable after a reset and one failure", mustUsable(t, session), []string{"a", "b"})
}

func TestAnsweringWithAnErrorDoesNotCoolANode(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	fail(t, session, nodeA)
	other := errors.New("no such directory")
	err := session.Do(context.Background(), nodeA, func(context.Context, string) error { return other })
	if !errors.Is(err, other) {
		t.Fatalf("Do = %v, want the call's own error", err)
	}
	// The cooldown from the earlier failure is neither extended nor cleared.
	checkNames(t, "Usable", mustUsable(t, session), []string{"b"})
	cluster.clk.Advance(2 * time.Second)
	checkNames(t, "Usable", mustUsable(t, session), []string{"a", "b"})
}

func TestUsableWhenEveryNodeIsCoolingReturnsTheSoonest(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b", "c")
	session := cluster.session()
	nodeA, nodeB, nodeC := nodeOf(t, session, "a"), nodeOf(t, session, "b"), nodeOf(t, session, "c")

	fail(t, session, nodeB)
	cluster.clk.Advance(time.Second)
	fail(t, session, nodeA)
	fail(t, session, nodeA)
	fail(t, session, nodeC)

	// b's cooldown ends at 2s, a's at 5s, c's at 3s.
	checkNames(t, "Usable", mustUsable(t, session), []string{"b"})
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a", "b", "c"})
}

func TestNodeListRefreshAddsAndDropsNodes(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b", "c")
	opts := cluster.options()
	opts.RefreshNodes = 30 * time.Second
	session := New(opts)
	ctx := context.Background()
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a", "b", "c"})
	nodeA, nodeB := nodeOf(t, session, "a"), nodeOf(t, session, "b")
	for range 6 {
		fail(t, session, nodeB)
	}

	cluster.addNode("d")
	cluster.setList(
		wire.Node{Name: "d", URL: urlOf("d")},
		wire.Node{Name: "b", URL: urlOf("b")},
		wire.Node{Name: "a", URL: urlOf("a")},
	)

	cluster.clk.Advance(29 * time.Second)
	checkNames(t, "Nodes before the interval", mustNodes(t, session), []string{"a", "b", "c"})
	cluster.clk.Advance(time.Second)
	checkNames(t, "Nodes after the interval", mustNodes(t, session), []string{"d", "b", "a"})

	if got := nodeOf(t, session, "a"); got != nodeA {
		t.Error("node a was dialed again; existing nodes must be kept")
	}
	// b was cooling for 60s when the list was fetched 30s later, and still is.
	checkNames(t, "Usable", mustUsable(t, session), []string{"d", "a"})
	if got := slices.Index(cluster.dials(), "d "+urlOf("d")); got < 0 {
		t.Errorf("dials = %v, want d dialed", cluster.dials())
	}
	if _, err := session.Nodes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := cluster.entry.listingTokens(); len(got) != 2 {
		t.Errorf("node list fetched %d times at the cluster host, want 2", len(got))
	}
	if got := cluster.nodeListings(); got != 0 {
		t.Errorf("node list fetched %d times at nodes, want 0", got)
	}
}

func TestNodeListRefreshRedialsANodeWhoseURLChanged(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	opts := cluster.options()
	opts.RefreshNodes = time.Minute
	session := New(opts)
	before := nodeOf(t, session, "a")
	fail(t, session, before)

	cluster.setList(wire.Node{Name: "a", URL: "https://moved.example.com/api"})
	cluster.clk.Advance(time.Minute)
	mustNodes(t, session)

	if got := cluster.dials(); got[len(got)-1] != "a https://moved.example.com/api" {
		t.Errorf("dials = %v, want a dialed at its new URL", got)
	}
	checkNames(t, "Usable", mustUsable(t, session), []string{"a"})
}

func TestFailedNodeListRefreshKeepsTheOldListUntilTheNextInterval(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	opts := cluster.options()
	opts.RefreshNodes = time.Minute
	session := New(opts)
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a", "b"})

	cluster.entryDown()
	cluster.setList(wire.Node{Name: "b", URL: urlOf("b")})
	cluster.clk.Advance(time.Minute)

	checkNames(t, "Nodes after a failed refresh", mustNodes(t, session), []string{"a", "b"})
	cluster.entry.setNodes(cluster.serveList)
	cluster.clk.Advance(30 * time.Second)
	checkNames(t, "Nodes within the interval", mustNodes(t, session), []string{"a", "b"})
	if got := len(cluster.entry.listingTokens()); got != 2 {
		t.Errorf("the cluster host was asked %d times before the next interval, want 2", got)
	}
	cluster.clk.Advance(30 * time.Second)
	checkNames(t, "Nodes after the next interval", mustNodes(t, session), []string{"b"})
}

func TestNodeListRefreshAsksTheClusterHostNotANode(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	session := cluster.session()
	mustNodes(t, session)
	cluster.setList(wire.Node{Name: "b", URL: urlOf("b")})
	cluster.clk.Advance(time.Minute)

	checkNames(t, "Nodes", mustNodes(t, session), []string{"b"})
	checkNames(t, "cluster host listings", cluster.entry.listingTokens(), []string{"tok-1", "tok-1"})
	if got := cluster.nodeListings(); got != 0 {
		t.Errorf("node list fetched %d times at nodes, want 0", got)
	}
}

func TestNodeListRefreshWithTheClusterHostUnreachableKeepsTheListAndWarns(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	var logged bytes.Buffer
	opts := cluster.options()
	opts.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	session := New(opts)
	mustNodes(t, session)
	cluster.entryDown()
	cluster.setList(wire.Node{Name: "b", URL: urlOf("b")})
	cluster.clk.Advance(time.Minute)

	checkNames(t, "Nodes", mustNodes(t, session), []string{"a", "b"})
	for _, want := range []string{"node list refresh failed", "cluster_host=" + opts.EntryURL} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log = %q, want it to contain %q", logged.String(), want)
		}
	}
	if got := cluster.nodeListings() + cluster.nodeLogins(); got != 0 {
		t.Errorf("nodes were used %d times, want 0", got)
	}
}

func TestNodeListRefreshWithoutADialableClusterHostFails(t *testing.T) {
	t.Parallel()
	// After a connect the cluster host is always dialed; this is the guard for the rest.
	cluster := newEnv(t, "a")
	cluster.dialErr[entryName] = errDown
	session := cluster.session()

	if _, err := session.fetchNodeList(context.Background()); !errors.Is(err, errDown) {
		t.Errorf("fetchNodeList = %v, want errDown", err)
	}
}

func TestNodeListRefreshReplacesARefusedTokenOnce(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	session := cluster.session()
	mustNodes(t, session)
	cluster.entry.setNodes(func(ctx context.Context, tok string) (wire.Nodes, error) {
		if tok == "tok-1" {
			return wire.Nodes{}, errDenied
		}
		return cluster.serveList(ctx, tok)
	})
	cluster.setList(wire.Node{Name: "b", URL: urlOf("b")})
	cluster.clk.Advance(time.Minute)

	checkNames(t, "Nodes", mustNodes(t, session), []string{"b"})
	checkNames(t, "cluster host listings", cluster.entry.listingTokens(), []string{"tok-1", "tok-1", "tok-2"})
	if got := cluster.entry.loginCount(); got != 2 {
		t.Errorf("cluster host logins = %d, want 2: the first and one replacement", got)
	}
	if got := cluster.nodeLogins() + cluster.nodeListings(); got != 0 {
		t.Errorf("nodes were used %d times, want 0", got)
	}
}

func TestNodeListRefreshIgnoresAListWithNoUsableNode(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	opts := cluster.options()
	opts.RefreshNodes = time.Minute
	session := New(opts)
	mustNodes(t, session)

	cluster.setList(wire.Node{Name: "z", URL: urlOf("z")})
	cluster.clk.Advance(time.Minute)
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a"})
}

func TestConnectWhenAlreadyConnectedDoesNothing(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	session := cluster.session()
	mustNodes(t, session)
	if err := session.connect(context.Background()); err != nil {
		t.Fatalf("connect = %v, want nil", err)
	}
	if got := cluster.entry.loginCount(); got != 1 {
		t.Errorf("entry logins = %d, want 1", got)
	}
}

func TestRejectedCredentialsAtTheClusterHostAreFinal(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a", "b")
	session := cluster.session()
	nodeA := nodeOf(t, session, "a")
	token(t, session, nodeA)
	cluster.entry.setLogin(failWith(errDenied))
	cluster.clk.Advance(time.Hour)

	err := session.Do(context.Background(), nodeA, func(context.Context, string) error {
		t.Error("call made without a token")
		return nil
	})
	if !errors.Is(err, ErrBadCredentials) || errors.Is(err, ErrUnavailable) {
		t.Errorf("Do = %v, want ErrBadCredentials only", err)
	}
	if got := cluster.nodeLogins(); got != 0 {
		t.Errorf("node logins = %d, want 0: rejected credentials are not retried at the nodes", got)
	}
}

func TestTheClusterHostIsDialedAgainOnlyAfterADialFailure(t *testing.T) {
	t.Parallel()
	cluster := newEnv(t, "a")
	cluster.dialErr[entryName] = errDown
	session := cluster.session()
	if _, err := session.Nodes(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Nodes = %v, want ErrUnavailable", err)
	}

	delete(cluster.dialErr, entryName)
	cluster.clk.Advance(2 * time.Second)
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a"})
	cluster.clk.Advance(time.Hour)
	mustNodes(t, session)
	if got := cluster.dialCount(entryName); got != 2 {
		t.Errorf("the cluster host was dialed %d times, want 2: the failure, then once and kept", got)
	}
}

func TestCallsToTheClusterHostNeverChangeANodesHealth(t *testing.T) {
	t.Parallel()
	// A node that is named like the cluster host must not take its failures.
	cluster := newEnv(t, "a", "entry")
	session := cluster.session()
	checkNames(t, "Nodes", mustNodes(t, session), []string{"a", "entry"})
	cluster.entry.setNodes(nodesFail(errDown))
	cluster.clk.Advance(time.Minute)
	checkNames(t, "Usable after a failed refresh", mustUsable(t, session), []string{"a", "entry"})
	if got := len(cluster.entry.listingTokens()); got != 2 {
		t.Errorf("cluster host listings = %d, want 2: the first and the failed refresh", got)
	}

	cluster.entry.setLogin(failWith(errDown))
	cluster.clk.Advance(time.Hour)
	checkNames(t, "Usable after a failed renewal", mustUsable(t, session), []string{"a", "entry"})
	if got := cluster.entry.loginCount(); got != 2 {
		t.Errorf("cluster host logins = %d, want 2: the first and the failed renewal", got)
	}
}
