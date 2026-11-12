package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/config"
)

const (
	testClusterFile = "url: https://files.example.com/api\nuser: alice\npassword_file: secrets/p\n"
	testCacheFile   = "cache:\n  dir: /var/cache/splicefs\n"
	// stopTimeout bounds every wait in these tests.
	stopTimeout = 5 * time.Second
)

func mountFile(cluster, path, target string) string {
	return "cluster: " + cluster + "\npath: " + path + "\ntarget: " + target + "\n"
}

func boolPtr(v bool) *bool { return &v }

// requestRig is a running client whose configuration comes from files, the way the
// program reads it, so that requested mounts are validated against the real rules.
type requestRig struct {
	h       *harness
	dir     string
	signals chan os.Signal
}

func newRequestRig(t *testing.T) *requestRig {
	t.Helper()
	g := &requestRig{h: newHarness(t), dir: t.TempDir(), signals: make(chan os.Signal)}
	g.write("conf.d/cache.yaml", testCacheFile)
	g.write("clusters.d/example.yaml", testClusterFile)
	g.write("clusters.d/other.yaml", testClusterFile)
	g.write("mounts.d/a.yaml", mountFile("example", "/media", "/mnt/a"))
	g.h.opts.Reload = g.signals
	g.h.opts.Load = func() (*config.Config, error) { return config.LoadDir(g.dir) }
	return g
}

func (g *requestRig) write(name, content string) {
	g.h.t.Helper()
	path := filepath.Join(g.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		g.h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		g.h.t.Fatal(err)
	}
}

func (g *requestRig) remove(name string) {
	g.h.t.Helper()
	if err := os.Remove(filepath.Join(g.dir, name)); err != nil {
		g.h.t.Fatal(err)
	}
}

// begin loads the files and starts a run that is ready.
func (g *requestRig) begin() *running {
	g.h.t.Helper()
	cfg, err := config.LoadDir(g.dir)
	if err != nil {
		g.h.t.Fatal(err)
	}
	g.h.cfg = cfg
	return g.h.start()
}

// ask sends one mount request over the control socket.
func (g *requestRig) ask(m MountRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	_, err := systemHost().call(ctx, g.h.socket, Request{Op: OpMount, Mount: &m})
	return err
}

func (g *requestRig) mustAsk(m MountRequest) {
	g.h.t.Helper()
	if err := g.ask(m); err != nil {
		g.h.t.Fatalf("mount %s refused: %v", m.Target, err)
	}
}

func (g *requestRig) listPath() string { return requestedPath(g.h.socket) }

// listed returns the requested mounts the list file holds, none if it does not exist.
func (g *requestRig) listed() []MountRequest {
	g.h.t.Helper()
	data, err := os.ReadFile(g.listPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		g.h.t.Fatal(err)
	}
	var got []MountRequest
	if err := json.Unmarshal(data, &got); err != nil {
		g.h.t.Fatal(err)
	}
	return got
}

func (g *requestRig) listedTargets() []string {
	g.h.t.Helper()
	targets := []string{}
	for _, m := range g.listed() {
		targets = append(targets, m.Target)
	}
	return targets
}

// waitListed returns once the list file holds exactly these targets. The watchers of the
// mounts change it in their own goroutines.
func (g *requestRig) waitListed(want ...string) {
	g.h.t.Helper()
	waitFor(g.h.t, "the requested list to change", func() bool {
		return slices.Equal(g.listedTargets(), append([]string{}, want...))
	})
}

func (g *requestRig) finished() int {
	out := g.h.log.String()
	return strings.Count(out, "configuration applied") +
		strings.Count(out, "configuration not applied")
}

// reload asks for a reload and waits until it is done.
func (g *requestRig) reload() {
	g.h.t.Helper()
	done := g.finished()
	select {
	case g.signals <- syscall.SIGHUP:
	case <-time.After(stopTimeout):
		g.h.t.Fatal("the run did not take the reload request")
	}
	waitFor(g.h.t, "the reload to finish", func() bool { return g.finished() > done })
}

func (g *requestRig) wantLogged(parts ...string) {
	g.h.t.Helper()
	out := g.h.log.String()
	for _, part := range parts {
		if !strings.Contains(out, part) {
			g.h.t.Errorf("log lacks %q:\n%s", part, out)
		}
	}
}

func (g *requestRig) wantMountCalls(want ...string) {
	g.h.t.Helper()
	if got := g.h.mounter.targets(); !slices.Equal(got, want) {
		g.h.t.Errorf("mount calls = %v, want %v", got, want)
	}
}

// wantLive checks the mounts the client reports as mounted, sorted.
func (g *requestRig) wantLive(want ...string) {
	g.h.t.Helper()
	live := []string{}
	for _, m := range g.h.status().Mounts {
		if m.State == StateMounted {
			live = append(live, m.Target)
		}
	}
	slices.Sort(live)
	if !slices.Equal(live, append([]string{}, want...)) {
		g.h.t.Errorf("live mounts = %v, want %v", live, want)
	}
}

func (g *requestRig) stop(r *running) {
	g.h.t.Helper()
	if err := r.stop(); err != nil {
		g.h.t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
}

func TestRequestMountsAndListsIt(t *testing.T) {
	t.Parallel()
	g := newRequestRig(t)
	r := g.begin()

	const request = `{"op":"mount","mount":{"cluster":"example","path":"/media/tv",` +
		`"target":"/mnt/tv","allow_other":true,"offline":false}}`
	if got := rawCall(t, g.h.socket, request); got != "{}\n" {
		t.Errorf("reply = %q, want an empty reply", got)
	}

	g.wantMountCalls("/mnt/a", "/mnt/tv")
	opts := g.h.mounter.opts[1]
	if opts.Source != "splicefs:example/media/tv" || !opts.AllowOther {
		t.Errorf("mount options = %+v", opts)
	}
	g.wantLive("/mnt/a", "/mnt/tv")
	want := []MountRequest{{
		Cluster: "example", Path: "/media/tv", Target: "/mnt/tv", AllowOther: true,
		Offline: boolPtr(false),
	}}
	got := g.listed()
	if len(got) != 1 || got[0].Offline == nil || *got[0].Offline != false ||
		got[0].Cluster != want[0].Cluster || got[0].Path != want[0].Path ||
		got[0].Target != want[0].Target || got[0].AllowOther != want[0].AllowOther {
		t.Errorf("list = %+v, want %+v", got, want)
	}
	info, err := os.Stat(g.listPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("list file mode = %o, want 600", perm)
	}
	g.stop(r)
}

func TestRequestForTheSameTargetReplacesTheEntry(t *testing.T) {
	t.Parallel()
	g := newRequestRig(t)
	r := g.begin()
	g.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
	first := g.h.mounter.get("/mnt/tv")
	g.mustAsk(MountRequest{Cluster: "other", Path: "/docs", Target: "/mnt/tv/"})

	g.wantMountCalls("/mnt/a", "/mnt/tv", "/mnt/tv")
	if calls := first.unmountCalls(); !slices.Equal(calls, []bool{true}) {
		t.Errorf("the first mount was unmounted with force = %v, want one forced unmount", calls)
	}
	got := g.listed()
	if len(got) != 1 || got[0].Cluster != "other" || got[0].Path != "/docs" {
		t.Errorf("list = %+v, want the second request alone", got)
	}
	g.wantLive("/mnt/a", "/mnt/tv")
	g.stop(r)
}

func TestInvalidRequestIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request MountRequest
	}{
		{"unknown cluster", MountRequest{Cluster: "nowhere", Target: "/mnt/x"}},
		{"relative target", MountRequest{Cluster: "example", Target: "mnt/rel"}},
		{"inside a configured mount", MountRequest{Cluster: "example", Target: "/mnt/a/x"}},
		{"around a configured mount", MountRequest{Cluster: "example", Target: "/mnt"}},
		{"the target of a configured mount", MountRequest{Cluster: "example", Target: "/mnt/a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newRequestRig(t)
			r := g.begin()
			g.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/keep"})
			before, err := os.ReadFile(g.listPath())
			if err != nil {
				t.Fatal(err)
			}

			err = g.ask(tt.request)
			if err == nil || !strings.Contains(err.Error(), "the client refused the request") ||
				!strings.Contains(err.Error(), filepath.Clean(tt.request.Target)) {
				t.Errorf("error = %v, want a refusal that names %s", err, tt.request.Target)
			}
			g.wantMountCalls("/mnt/a", "/mnt/keep")
			g.wantLive("/mnt/a", "/mnt/keep")
			after, err := os.ReadFile(g.listPath())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("list changed from %s to %s", before, after)
			}
			// The answer says why. "Not mounted" alone would leave the caller guessing.
			if err != nil && strings.Contains(err.Error(), "is not mounted") {
				t.Errorf("error = %v, want the reason the request is invalid", err)
			}
			g.stop(r)
		})
	}
}

func TestRequestThatCannotBeMountedIsRefusedAndUnlisted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// arm makes the next mount fail, once the run is ready.
		arm  func(h *harness)
		want string
	}{
		{
			name: "the mount layer fails",
			arm: func(h *harness) {
				h.mounter.mu.Lock()
				defer h.mounter.mu.Unlock()
				h.mounter.fail["/mnt/x"] = errors.New("fuse refused")
			},
			want: "fuse refused",
		},
		{
			name: "the view cannot be made",
			arm: func(h *harness) {
				h.core.mu.Lock()
				defer h.core.mu.Unlock()
				h.core.viewErr = errors.New("no view")
			},
			want: "no view",
		},
		{
			name: "the core refuses the configuration",
			arm: func(h *harness) {
				h.core.mu.Lock()
				defer h.core.mu.Unlock()
				h.core.applyErr = errors.New("apply refused")
			},
			want: "apply refused",
		},
		{
			name: "the mount ends at once",
			arm: func(h *harness) {
				h.mounter.mu.Lock()
				defer h.mounter.mu.Unlock()
				h.mounter.configure = func(f *fakeMounted) { f.end(nil) }
			},
			want: "/mnt/x is not mounted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newRequestRig(t)
			r := g.begin()
			tt.arm(g.h)

			err := g.ask(MountRequest{Cluster: "example", Target: "/mnt/x"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one that says %q", err, tt.want)
			}
			g.waitListed()
			g.wantLive("/mnt/a")
			g.stop(r)
		})
	}
}

// controlWith serves a control socket with the given request channel and no mounts.
func controlWith(t *testing.T, requests chan<- mountRequest) (*control, string) {
	t.Helper()
	socket := filepath.Join(shortDir(t), "c.sock")
	ln, err := listenUnixSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	return startControl(ln, &mountSet{}, newFakeCore(), logs.logger(), requests), socket
}

func TestControlRefusesMountRequestsItCannotServe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		requests chan mountRequest
		request  string
		want     string
	}{
		{"no mount", make(chan mountRequest), `{"op":"mount"}`, "has no mount"},
		{
			"no run loop", nil,
			`{"op":"mount","mount":{"cluster":"example","target":"/mnt/x"}}`,
			"cannot mount on request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var requests chan<- mountRequest
			if tt.requests != nil {
				requests = tt.requests
			}
			ctl, socket := controlWith(t, requests)
			defer ctl.stop()
			if got := rawCall(t, socket, tt.request); !strings.Contains(got, tt.want) {
				t.Errorf("reply = %s, want one that says %q", got, tt.want)
			}
		})
	}
}

func TestMountRequestDuringStopIsAnswered(t *testing.T) {
	t.Parallel()
	req := &MountRequest{Cluster: "example", Target: "/mnt/x"}

	t.Run("before the run loop takes it", func(t *testing.T) {
		t.Parallel()
		ctl, _ := controlWith(t, make(chan mountRequest))
		ctl.cancel()
		reply := make(chan Reply, 1)
		go func() { reply <- ctl.mount(req) }()
		select {
		case got := <-reply:
			if got.Error != errStopping.Error() {
				t.Errorf("reply = %+v, want %q", got, errStopping)
			}
		case <-time.After(stopTimeout):
			t.Fatal("the request hung")
		}
		ctl.stop()
	})

	t.Run("while the run loop works on it", func(t *testing.T) {
		t.Parallel()
		requests := make(chan mountRequest)
		ctl, socket := controlWith(t, requests)
		answer := make(chan error, 1)
		go func() {
			conn, err := dialUnix(socket)
			if err != nil {
				answer <- err
				return
			}
			defer func() { _ = conn.Close() }()
			_, err = io.WriteString(conn,
				`{"op":"mount","mount":{"cluster":"example","target":"/mnt/x"}}`)
			if err == nil {
				var text []byte
				if text, err = io.ReadAll(conn); err == nil {
					err = errors.New(string(text))
				}
			}
			answer <- err
		}()
		select {
		case <-requests:
		case <-time.After(stopTimeout):
			t.Fatal("the request never reached the run loop")
		}
		ctl.stop()
		select {
		case err := <-answer:
			if err == nil || !strings.Contains(err.Error(), errStopping.Error()) {
				t.Errorf("answer = %v, want %q", err, errStopping)
			}
		case <-time.After(stopTimeout):
			t.Fatal("the request hung")
		}
	})
}

func TestUnmountTakesARequestedMountOffTheList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		end  func(t *testing.T, g *requestRig)
	}{
		{"the administrator unmounts it", func(_ *testing.T, g *requestRig) {
			g.h.mounter.get("/mnt/tv").end(nil)
		}},
		{"it is unmounted over the socket", func(t *testing.T, g *requestRig) {
			ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			if err := Unmount(ctx, g.h.socket, []string{"/mnt/tv"}, false); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newRequestRig(t)
			r := g.begin()
			g.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
			g.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/film"})

			tt.end(t, g)
			g.waitListed("/mnt/film")
			g.wantLive("/mnt/a", "/mnt/film")
			g.stop(r)
			// The mount that stayed is still listed after the stop.
			g.waitListed("/mnt/film")
		})
	}
}

func TestStopKeepsRequestedMountsAndTheNextRunMountsThemBeforeReady(t *testing.T) {
	t.Parallel()
	g := newRequestRig(t)
	var mountedAtReady []string
	g.h.opts.Notify = func(state string) error {
		if state == "READY=1" {
			mountedAtReady = g.h.mounter.targets()
		}
		return g.h.notes.notify(state)
	}
	first := g.begin()
	g.mustAsk(MountRequest{Cluster: "example", Path: "/media", Target: "/mnt/tv", AllowOther: true})
	g.stop(first)
	g.waitListed("/mnt/tv")

	second := g.begin()
	want := []string{"/mnt/a", "/mnt/tv", "/mnt/a", "/mnt/tv"}
	if !slices.Equal(mountedAtReady, want) {
		t.Errorf("mounted when ready = %v, want %v", mountedAtReady, want)
	}
	if last := g.h.mounter.opts[3]; last.Source != "splicefs:example/media" || !last.AllowOther {
		t.Errorf("remounted with %+v", last)
	}
	g.wantLive("/mnt/a", "/mnt/tv")
	g.waitListed("/mnt/tv")
	g.stop(second)
}

func TestStartDropsAListedMountThatFails(t *testing.T) {
	t.Parallel()
	g := newRequestRig(t)
	g.h.mounter.fail["/mnt/bad"] = errors.New("fuse refused")
	list := `[{"cluster":"example","target":"/mnt/bad"},{"cluster":"example","target":"/mnt/ok"}]`
	if err := os.WriteFile(g.listPath(), []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	r := g.begin()

	g.wantLive("/mnt/a", "/mnt/ok")
	g.waitListed("/mnt/ok")
	g.wantLogged("requested mount dropped", "/mnt/bad", "fuse refused")
	g.stop(r)
}

func TestListFileThatCannotBeUsed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"malformed", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not a list"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a folder instead of a file", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newRequestRig(t)
			tt.setup(t, g.listPath())
			r := g.begin()

			g.wantLogged("requested mounts ignored")
			g.wantMountCalls("/mnt/a")
			g.wantLive("/mnt/a")
			g.stop(r)
		})
	}
}

// The mount works without the list file: only a restart of the client would not bring it
// back. The failure is logged and the requester still gets its mount.
func TestListFileThatCannotBeWrittenIsLogged(t *testing.T) {
	t.Parallel()
	g := newRequestRig(t)
	// A folder where the file belongs makes the rename of the new list fail.
	if err := os.Mkdir(g.listPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	r := g.begin()

	g.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
	g.wantLogged("requested mounts not saved")
	g.wantLive("/mnt/a", "/mnt/tv")
	g.stop(r)
}

func TestRequestedStoreWithoutAUsableFolder(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	s := newRequestedStore(filepath.Join(t.TempDir(), "missing", "c.mounts.json"), logs.logger())
	s.add(config.Requested{Cluster: "example", Target: "/mnt/tv"})
	if !strings.Contains(logs.String(), "requested mounts not saved") {
		t.Errorf("log:\n%s", logs.String())
	}
	if got := s.list(); len(got) != 1 {
		t.Errorf("list = %v, want the mount kept in memory", got)
	}
	if s.remove("/mnt/other") {
		t.Error("removed a mount that was never listed")
	}
}

func TestNilRequestedStoreIsEmpty(t *testing.T) {
	t.Parallel()
	var s *requestedStore
	s.add(config.Requested{Target: "/mnt/tv"})
	if s.remove("/mnt/tv") || s.list() != nil {
		t.Error("a nil store must stay empty")
	}
}

func TestReloadKeepsAMountedRequestedMount(t *testing.T) {
	t.Parallel()
	g := newRequestRig(t)
	r := g.begin()
	g.mustAsk(MountRequest{Cluster: "example", Path: "/media", Target: "/mnt/tv"})
	g.write("mounts.d/b.yaml", mountFile("other", "/", "/mnt/b"))

	g.reload()

	g.wantMountCalls("/mnt/a", "/mnt/tv", "/mnt/b")
	if calls := g.h.mounter.get("/mnt/tv").unmountCalls(); len(calls) != 0 {
		t.Errorf("the requested mount was unmounted: %v", calls)
	}
	g.wantLive("/mnt/a", "/mnt/b", "/mnt/tv")
	g.waitListed("/mnt/tv")
	g.h.core.mu.Lock()
	applied := slices.Clone(g.h.core.applied)
	g.h.core.mu.Unlock()
	last := applied[len(applied)-1]
	if got := last.Mounts[len(last.Mounts)-1]; got.Name != config.RequestedName("/mnt/tv") {
		t.Errorf("applied mounts end with %+v, want the requested mount", got)
	}
	g.stop(r)
}

func TestReloadDropsARequestedMountThatIsNoLongerValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(g *requestRig)
	}{
		{"its cluster was removed", func(g *requestRig) { g.remove("clusters.d/other.yaml") }},
		{"its target overlaps a new file mount", func(g *requestRig) {
			g.write("mounts.d/z.yaml", mountFile("example", "/", "/mnt/gone/inner"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newRequestRig(t)
			r := g.begin()
			g.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/kept"})
			g.mustAsk(MountRequest{Cluster: "other", Target: "/mnt/gone"})
			gone := g.h.mounter.get("/mnt/gone")
			g.write("mounts.d/y.yaml", mountFile("example", "/", "/mnt/y"))

			tt.change(g)
			g.reload()

			g.wantLogged("requested mount dropped", "/mnt/gone", "configuration applied")
			if calls := gone.unmountCalls(); !slices.Equal(calls, []bool{true}) {
				t.Errorf("the dropped mount was unmounted with force = %v, want one", calls)
			}
			g.waitListed("/mnt/kept")
			live := []string{"/mnt/a", "/mnt/kept", "/mnt/y"}
			if tt.name != "its cluster was removed" {
				live = append(live, "/mnt/gone/inner")
			}
			slices.Sort(live)
			g.wantLive(live...)
			g.stop(r)
		})
	}
}

// A request is added to the list before the effective configuration is checked, and its
// watcher may be late, so the reload must cope with a listed mount that is not live.
func TestMountedRequestsIgnoresMountsThatEnded(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	endedMount := func(target string) *liveMount {
		m := newFakeMounted()
		m.end(nil)
		return &liveMount{target: target, mounted: m}
	}
	tests := []struct {
		name string
		live []*liveMount
	}{
		{"not in the set", nil},
		{"in the set but ended", []*liveMount{endedMount("/mnt/tv")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRequestedStore(filepath.Join(t.TempDir(), "c.mounts.json"), logs.logger())
			store.add(config.Requested{Cluster: "example", Target: "/mnt/tv"})
			set := &mountSet{mounts: tt.live}
			r := &reloader{set: set, store: store}

			if got := r.mountedRequests(); len(got) != 0 {
				t.Errorf("mounted requests = %v, want none", got)
			}
			if got := store.list(); len(got) != 0 {
				t.Errorf("list = %v, want it emptied", got)
			}
		})
	}
}

// dropUntilValid is only called with files that are valid alone, so it always ends with a
// valid result. Its answer for files that are not is the error, with nothing left listed.
func TestDropUntilValidGivesUpWhenTheFilesAreInvalidAlone(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	cfg := &config.Config{Mounts: []config.Mount{{Cluster: "nowhere", Target: "/mnt/a"}}}
	store := newRequestedStore(filepath.Join(t.TempDir(), "c.mounts.json"), logs.logger())
	reqs := []config.Requested{
		{Cluster: "example", Target: "/mnt/tv"}, {Cluster: "example", Target: "/mnt/film"},
	}
	for _, req := range reqs {
		store.add(req)
	}
	r := &reloader{store: store, opts: Options{Logger: slog.New(logs.logger().Handler())}}

	eff, err := r.dropUntilValid(cfg, reqs, errors.New("invalid"))
	if eff != nil || err == nil {
		t.Errorf("dropUntilValid = %v, %v, want the error of the files", eff, err)
	}
	if got := store.list(); len(got) != 0 {
		t.Errorf("list = %v, want none", got)
	}
}
