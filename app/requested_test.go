package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

// requestRig is a running client whose configuration comes from files, the way the
// program reads it, so that requested mounts are validated against the real rules.
type requestRig struct {
	env     *harness
	dir     string
	signals chan os.Signal
}

func newRequestRig(t *testing.T) *requestRig {
	t.Helper()
	rig := &requestRig{env: newHarness(t), dir: t.TempDir(), signals: make(chan os.Signal)}
	rig.write("conf.d/cache.yaml", testCacheFile)
	rig.write("clusters.d/example.yaml", testClusterFile)
	rig.write("clusters.d/other.yaml", testClusterFile)
	rig.write("mounts.d/a.yaml", mountFile("example", "/media", "/mnt/a"))
	rig.env.opts.Reload = rig.signals
	rig.env.opts.Load = func() (*config.Config, error) { return config.LoadDir(rig.dir) }
	return rig
}

func (rig *requestRig) write(name, content string) {
	rig.env.t.Helper()
	path := filepath.Join(rig.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		rig.env.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		rig.env.t.Fatal(err)
	}
}

func (rig *requestRig) remove(name string) {
	rig.env.t.Helper()
	if err := os.Remove(filepath.Join(rig.dir, name)); err != nil {
		rig.env.t.Fatal(err)
	}
}

// begin loads the files and starts a run that is ready.
func (rig *requestRig) begin() *running {
	rig.env.t.Helper()
	cfg, err := config.LoadDir(rig.dir)
	if err != nil {
		rig.env.t.Fatal(err)
	}
	rig.env.cfg = cfg
	return rig.env.start()
}

// ask sends one mount request over the control socket.
func (rig *requestRig) ask(m MountRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	_, err := systemHost().call(ctx, rig.env.socket, Request{Op: OpMount, Mount: &m})
	return err
}

func (rig *requestRig) mustAsk(m MountRequest) {
	rig.env.t.Helper()
	if err := rig.ask(m); err != nil {
		rig.env.t.Fatalf("mount %s refused: %v", m.Target, err)
	}
}

func (rig *requestRig) listPath() string { return requestedPath(rig.env.socket) }

// listBytes returns what the list file holds.
func (rig *requestRig) listBytes() []byte {
	rig.env.t.Helper()
	data, err := os.ReadFile(rig.listPath())
	if err != nil {
		rig.env.t.Fatal(err)
	}
	return data
}

// wantListFileMode fails the test unless the list file has exactly this permission.
func (rig *requestRig) wantListFileMode(want os.FileMode) {
	rig.env.t.Helper()
	info, err := os.Stat(rig.listPath())
	if err != nil {
		rig.env.t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != want {
		rig.env.t.Errorf("list file mode = %o, want %o", perm, want)
	}
}

// listed returns the requested mounts the list file holds, none if it does not exist.
func (rig *requestRig) listed() []MountRequest {
	rig.env.t.Helper()
	data, err := os.ReadFile(rig.listPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		rig.env.t.Fatal(err)
	}
	var got []MountRequest
	if err := json.Unmarshal(data, &got); err != nil {
		rig.env.t.Fatal(err)
	}
	return got
}

func (rig *requestRig) listedTargets() []string {
	rig.env.t.Helper()
	listed := rig.listed()
	targets := make([]string, 0, len(listed))
	for _, request := range listed {
		targets = append(targets, request.Target)
	}
	return targets
}

// waitListed returns once the list file holds exactly these targets. The watchers of the
// mounts change it in their own goroutines.
func (rig *requestRig) waitListed(want ...string) {
	rig.env.t.Helper()
	waitFor(rig.env.t, "the requested list to change", func() bool {
		return slices.Equal(rig.listedTargets(), append([]string{}, want...))
	})
}

func (rig *requestRig) finished() int {
	out := rig.env.log.String()
	return strings.Count(out, "configuration applied") +
		strings.Count(out, "configuration not applied")
}

// reload asks for a reload and waits until it is done.
func (rig *requestRig) reload() {
	rig.env.t.Helper()
	done := rig.finished()
	select {
	case rig.signals <- syscall.SIGHUP:
	case <-time.After(stopTimeout):
		rig.env.t.Fatal("the run did not take the reload request")
	}
	waitFor(rig.env.t, "the reload to finish", func() bool { return rig.finished() > done })
}

func (rig *requestRig) wantLogged(parts ...string) {
	rig.env.t.Helper()
	out := rig.env.log.String()
	for _, part := range parts {
		if !strings.Contains(out, part) {
			rig.env.t.Errorf("log lacks %q:\n%s", part, out)
		}
	}
}

func (rig *requestRig) wantMountCalls(want ...string) {
	rig.env.t.Helper()
	if got := rig.env.mounter.targets(); !slices.Equal(got, want) {
		rig.env.t.Errorf("mount calls = %v, want %v", got, want)
	}
}

// wantLive checks the mounts the client reports as mounted, sorted.
func (rig *requestRig) wantLive(want ...string) {
	rig.env.t.Helper()
	live := []string{}
	for _, m := range rig.env.status().Mounts {
		if m.State == StateMounted {
			live = append(live, m.Target)
		}
	}
	slices.Sort(live)
	if !slices.Equal(live, append([]string{}, want...)) {
		rig.env.t.Errorf("live mounts = %v, want %v", live, want)
	}
}

func (rig *requestRig) stop(r *running) {
	rig.env.t.Helper()
	if err := r.stop(); err != nil {
		rig.env.t.Fatalf("Run = %v, want nil after a clean stop", err)
	}
}

func TestRequestMountsAndListsIt(t *testing.T) {
	t.Parallel()
	rig := newRequestRig(t)
	r := rig.begin()

	const request = `{"op":"mount","mount":{"cluster":"example","path":"/media/tv",` +
		`"target":"/mnt/tv","allow_other":true,"offline":false}}`
	if got := rawCall(t, rig.env.socket, request); got != "{}\n" {
		t.Errorf("reply = %q, want an empty reply", got)
	}

	rig.wantMountCalls("/mnt/a", "/mnt/tv")
	opts := rig.env.mounter.opts[1]
	if opts.Source != "splicefs:example/media/tv" || !opts.AllowOther {
		t.Errorf("mount options = %+v", opts)
	}
	rig.wantLive("/mnt/a", "/mnt/tv")
	want := []MountRequest{{
		Cluster: "example", Path: "/media/tv", Target: "/mnt/tv", AllowOther: true,
		Offline: new(false),
	}}
	if got := rig.listed(); !reflect.DeepEqual(got, want) {
		t.Errorf("list = %+v, want %+v", got, want)
	}
	rig.wantListFileMode(privateFileMode)
	// The bytes are the JSON as it was always written, without a newline at the end.
	if data := rig.listBytes(); !bytes.HasSuffix(data, []byte("}]")) {
		t.Errorf("list file = %q, want the JSON list with no newline after it", data)
	}
	rig.stop(r)
}

func TestRequestForTheSameTargetReplacesTheEntry(t *testing.T) {
	t.Parallel()
	rig := newRequestRig(t)
	r := rig.begin()
	rig.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
	first := rig.env.mounter.get("/mnt/tv")
	rig.mustAsk(MountRequest{Cluster: "other", Path: "/docs", Target: "/mnt/tv/"})

	rig.wantMountCalls("/mnt/a", "/mnt/tv", "/mnt/tv")
	if calls := first.unmountCalls(); !slices.Equal(calls, []bool{true}) {
		t.Errorf("the first mount was unmounted with force = %v, want one forced unmount", calls)
	}
	got := rig.listed()
	if len(got) != 1 || got[0].Cluster != "other" || got[0].Path != "/docs" {
		t.Errorf("list = %+v, want the second request alone", got)
	}
	rig.wantLive("/mnt/a", "/mnt/tv")
	rig.stop(r)
}

// wantRefusalNaming fails the test unless err is the refusal of a request for target. The
// answer must say why: "not mounted" alone would leave the caller guessing.
func wantRefusalNaming(t *testing.T, err error, target string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "the client refused the request") ||
		!strings.Contains(err.Error(), filepath.Clean(target)) {
		t.Errorf("error = %v, want a refusal that names %s", err, target)
		return
	}
	if strings.Contains(err.Error(), "is not mounted") {
		t.Errorf("error = %v, want the reason the request is invalid", err)
	}
	// The config package already says what is wrong with the mount, and the control socket
	// carries that text as it is.
	if strings.Contains(err.Error(), "check the requested mounts") {
		t.Errorf("error = %v, want the message of the config package without a prefix", err)
	}
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
			rig := newRequestRig(t)
			r := rig.begin()
			rig.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/keep"})
			before := rig.listBytes()

			wantRefusalNaming(t, rig.ask(tt.request), tt.request.Target)
			rig.wantMountCalls("/mnt/a", "/mnt/keep")
			rig.wantLive("/mnt/a", "/mnt/keep")
			if after := rig.listBytes(); !bytes.Equal(after, before) {
				t.Errorf("list changed from %s to %s", before, after)
			}
			rig.stop(r)
		})
	}
}

func TestRequestThatCannotBeMountedIsRefusedAndUnlisted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// arm makes the next mount fail, once the run is ready.
		arm  func(env *harness)
		want string
	}{
		{
			name: "the mount layer fails",
			arm: func(env *harness) {
				env.mounter.mu.Lock()
				defer env.mounter.mu.Unlock()
				env.mounter.fail["/mnt/x"] = errors.New("fuse refused")
			},
			want: "fuse refused",
		},
		{
			name: "the view cannot be made",
			arm: func(env *harness) {
				env.core.mu.Lock()
				defer env.core.mu.Unlock()
				env.core.viewErr = errors.New("no view")
			},
			want: "no view",
		},
		{
			name: "the core refuses the configuration",
			arm: func(env *harness) {
				env.core.mu.Lock()
				defer env.core.mu.Unlock()
				env.core.applyErr = errors.New("apply refused")
			},
			want: "apply refused",
		},
		{
			name: "the mount ends at once",
			arm: func(env *harness) {
				env.mounter.mu.Lock()
				defer env.mounter.mu.Unlock()
				env.mounter.configure = func(f *fakeMounted) { f.end(nil) }
			},
			want: "/mnt/x is not mounted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newRequestRig(t)
			r := rig.begin()
			tt.arm(rig.env)

			err := rig.ask(MountRequest{Cluster: "example", Target: "/mnt/x"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one that says %q", err, tt.want)
			}
			rig.waitListed()
			rig.wantLive("/mnt/a")
			rig.stop(r)
		})
	}
}

// controlSocketPath is a socket path in a fresh folder short enough for a Unix socket.
func controlSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortDir(t), "c.sock")
}

// controlWith serves a control socket at socket with the given request channel and no mounts.
func controlWith(t *testing.T, socket string, requests chan<- mountRequest) *control {
	t.Helper()
	ln, err := listenUnixSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	return startControl(t.Context(), ln, &mountSet{}, newFakeCore(), logs.logger(), requests)
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
			socket := controlSocketPath(t)
			ctl := controlWith(t, socket, requests)
			defer ctl.stop()
			if got := rawCall(t, socket, tt.request); !strings.Contains(got, tt.want) {
				t.Errorf("reply = %s, want one that says %q", got, tt.want)
			}
		})
	}
}

func TestMountRequestBeforeTheRunLoopTakesItIsAnswered(t *testing.T) {
	t.Parallel()
	ctl := controlWith(t, controlSocketPath(t), make(chan mountRequest))
	stopped, stop := context.WithCancel(t.Context())
	stop()
	reply := make(chan Reply, 1)
	go func() { reply <- ctl.mount(stopped, &MountRequest{Cluster: "example", Target: "/mnt/x"}) }()
	select {
	case got := <-reply:
		if got.Error != errStopping.Error() {
			t.Errorf("reply = %+v, want %q", got, errStopping)
		}
	case <-time.After(stopTimeout):
		t.Fatal("the request hung")
	}
	ctl.stop()
}

// askToMount sends a mount request over the socket and delivers what came back, as an error if the
// client answered with any text.
func askToMount(socket string, answer chan<- error) {
	conn, err := dialUnix(socket)
	if err != nil {
		answer <- err
		return
	}
	defer closeQuietly(conn)
	_, err = io.WriteString(conn, `{"op":"mount","mount":{"cluster":"example","target":"/mnt/x"}}`)
	if err == nil {
		var text []byte
		if text, err = io.ReadAll(conn); err == nil {
			err = errors.New(string(text))
		}
	}
	answer <- err
}

func TestMountRequestWhileTheRunLoopWorksOnItIsAnswered(t *testing.T) {
	t.Parallel()
	requests := make(chan mountRequest)
	socket := controlSocketPath(t)
	ctl := controlWith(t, socket, requests)
	answer := make(chan error, 1)
	go askToMount(socket, answer)
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
}

func TestUnmountTakesARequestedMountOffTheList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		end  func(t *testing.T, rig *requestRig)
	}{
		{"the administrator unmounts it", func(_ *testing.T, rig *requestRig) {
			rig.env.mounter.get("/mnt/tv").end(nil)
		}},
		{"it is unmounted over the socket", func(t *testing.T, rig *requestRig) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			if err := Unmount(ctx, rig.env.socket, []string{"/mnt/tv"}, false); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newRequestRig(t)
			r := rig.begin()
			rig.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
			rig.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/film"})

			tt.end(t, rig)
			rig.waitListed("/mnt/film")
			rig.wantLive("/mnt/a", "/mnt/film")
			rig.stop(r)
			// The mount that stayed is still listed after the stop.
			rig.waitListed("/mnt/film")
		})
	}
}

func TestStopKeepsRequestedMountsAndTheNextRunMountsThemBeforeReady(t *testing.T) {
	t.Parallel()
	rig := newRequestRig(t)
	var mountedAtReady []string
	rig.env.opts.Notify = func(state string) error {
		if state == "READY=1" {
			mountedAtReady = rig.env.mounter.targets()
		}
		return rig.env.notes.notify(state)
	}
	first := rig.begin()
	rig.mustAsk(MountRequest{Cluster: "example", Path: "/media", Target: "/mnt/tv", AllowOther: true})
	rig.stop(first)
	rig.waitListed("/mnt/tv")

	second := rig.begin()
	want := []string{"/mnt/a", "/mnt/tv", "/mnt/a", "/mnt/tv"}
	if !slices.Equal(mountedAtReady, want) {
		t.Errorf("mounted when ready = %v, want %v", mountedAtReady, want)
	}
	if last := rig.env.mounter.opts[3]; last.Source != "splicefs:example/media" || !last.AllowOther {
		t.Errorf("remounted with %+v", last)
	}
	rig.wantLive("/mnt/a", "/mnt/tv")
	rig.waitListed("/mnt/tv")
	rig.stop(second)
}

func TestStartDropsAListedMountThatFails(t *testing.T) {
	t.Parallel()
	rig := newRequestRig(t)
	rig.env.mounter.fail["/mnt/bad"] = errors.New("fuse refused")
	list := `[{"cluster":"example","target":"/mnt/bad"},{"cluster":"example","target":"/mnt/ok"}]`
	if err := os.WriteFile(rig.listPath(), []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	r := rig.begin()

	rig.wantLive("/mnt/a", "/mnt/ok")
	rig.waitListed("/mnt/ok")
	rig.wantLogged("requested mount dropped", "/mnt/bad", "fuse refused")
	rig.stop(r)
}

func TestListFileThatCannotBeUsed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"malformed", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("{not a list"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a folder instead of a file", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newRequestRig(t)
			tt.setup(t, rig.listPath())
			r := rig.begin()

			rig.wantLogged("requested mounts ignored")
			rig.wantMountCalls("/mnt/a")
			rig.wantLive("/mnt/a")
			rig.stop(r)
		})
	}
}

// The mount works without the list file: only a restart of the client would not bring it
// back. The failure is logged and the requester still gets its mount.
func TestListFileThatCannotBeWrittenIsLogged(t *testing.T) {
	t.Parallel()
	rig := newRequestRig(t)
	// A folder where the file belongs makes the rename of the new list fail.
	if err := os.Mkdir(rig.listPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	r := rig.begin()

	rig.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/tv"})
	rig.wantLogged("requested mounts not saved")
	rig.wantLive("/mnt/a", "/mnt/tv")
	rig.stop(r)
}

func TestRequestedStoreWithoutAUsableFolder(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	store := newRequestedStore(filepath.Join(t.TempDir(), "missing", "c.mounts.json"), logs.logger())
	store.add(config.Requested{Cluster: "example", Target: "/mnt/tv"})
	if !strings.Contains(logs.String(), "requested mounts not saved") {
		t.Errorf("log:\n%s", logs.String())
	}
	if got := store.list(); len(got) != 1 {
		t.Errorf("list = %v, want the mount kept in memory", got)
	}
	if store.remove("/mnt/other") {
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
	rig := newRequestRig(t)
	r := rig.begin()
	rig.mustAsk(MountRequest{Cluster: "example", Path: "/media", Target: "/mnt/tv"})
	rig.write("mounts.d/b.yaml", mountFile("other", "/", "/mnt/b"))

	rig.reload()

	rig.wantMountCalls("/mnt/a", "/mnt/tv", "/mnt/b")
	if calls := rig.env.mounter.get("/mnt/tv").unmountCalls(); len(calls) != 0 {
		t.Errorf("the requested mount was unmounted: %v", calls)
	}
	rig.wantLive("/mnt/a", "/mnt/b", "/mnt/tv")
	rig.waitListed("/mnt/tv")
	rig.env.core.mu.Lock()
	applied := slices.Clone(rig.env.core.applied)
	rig.env.core.mu.Unlock()
	last := applied[len(applied)-1]
	if got := last.Mounts[len(last.Mounts)-1]; got.Name != config.RequestedName("/mnt/tv") {
		t.Errorf("applied mounts end with %+v, want the requested mount", got)
	}
	rig.stop(r)
}

func TestReloadDropsARequestedMountThatIsNoLongerValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(rig *requestRig)
	}{
		{"its cluster was removed", func(rig *requestRig) { rig.remove("clusters.d/other.yaml") }},
		{"its target overlaps a new file mount", func(rig *requestRig) {
			rig.write("mounts.d/z.yaml", mountFile("example", "/", "/mnt/gone/inner"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rig := newRequestRig(t)
			r := rig.begin()
			rig.mustAsk(MountRequest{Cluster: "example", Target: "/mnt/kept"})
			rig.mustAsk(MountRequest{Cluster: "other", Target: "/mnt/gone"})
			gone := rig.env.mounter.get("/mnt/gone")
			rig.write("mounts.d/y.yaml", mountFile("example", "/", "/mnt/y"))

			tt.change(rig)
			rig.reload()

			rig.wantLogged("requested mount dropped", "/mnt/gone", "configuration applied")
			if calls := gone.unmountCalls(); !slices.Equal(calls, []bool{true}) {
				t.Errorf("the dropped mount was unmounted with force = %v, want one", calls)
			}
			rig.waitListed("/mnt/kept")
			live := []string{"/mnt/a", "/mnt/kept", "/mnt/y"}
			if tt.name != "its cluster was removed" {
				live = append(live, "/mnt/gone/inner")
			}
			slices.Sort(live)
			rig.wantLive(live...)
			rig.stop(r)
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
