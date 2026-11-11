//go:build darwin

package nfs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/mount"
)

func TestMountOptions(t *testing.T) {
	t.Parallel()
	const want = "vers=3,tcp,port=12345,mountport=12345,rdonly,noatime,nobrowse,nodev,nosuid," +
		"soft,intr,actimeo=30,locallocks,noquota,rsize=1048576"
	if got := mountOptions(12345); got != want {
		t.Errorf("mountOptions = %q\nwant %q", got, want)
	}
}

func TestMountArgs(t *testing.T) {
	t.Parallel()
	got := mountArgs(2049, "127.0.0.1:/splicefs:example/media", "/mnt/media")
	want := []string{"-o", mountOptions(2049), "127.0.0.1:/splicefs:example/media", "/mnt/media"}
	if !slices.Equal(got, want) {
		t.Errorf("mountArgs = %q, want %q", got, want)
	}
}

func TestMountFrom(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"splicefs:example/media": "127.0.0.1:/splicefs:example/media",
		"/already/rooted":        "127.0.0.1:/already/rooted",
		"splicefs:accept/":       "127.0.0.1:/splicefs:accept",
		"a//b/":                  "127.0.0.1:/a/b",
		"/":                      "127.0.0.1:/",
		"":                       "127.0.0.1:/",
	}
	for source, want := range tests {
		if got := mountFrom(source); got != want {
			t.Errorf("mountFrom(%q) = %q, want %q", source, got, want)
		}
	}
}

type command struct {
	name string
	args []string
}

// fakeSystem replaces mount_nfs, umount, the mount table and the clock. It records the
// commands, answers them through handle, and lets the test drive the polling.
type fakeSystem struct {
	mu       sync.Mutex
	commands []command
	handle   func(command) ([]byte, error)

	presence []presence // answers of isMounted in order; the last one repeats
	ticks    chan time.Time
	tickers  int
	release  chan struct{} // when set, stopping the ticker waits for it
}

type presence struct {
	mounted bool
	err     error
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{presence: []presence{{mounted: true}}, ticks: make(chan time.Time)}
}

func (f *fakeSystem) environment() environment {
	return environment{
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			c := command{name: name, args: slices.Clone(args)}
			f.mu.Lock()
			f.commands = append(f.commands, c)
			handle := f.handle
			f.mu.Unlock()
			if handle == nil {
				return nil, nil
			}
			return handle(c)
		},
		owner:  testOwner,
		listen: listenLoopback,
		isMounted: func(string, string) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			p := f.presence[0]
			if len(f.presence) > 1 {
				f.presence = f.presence[1:]
			}
			return p.mounted, p.err
		},
		newTicker: func(time.Duration) (<-chan time.Time, func()) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.tickers++
			release := f.release
			return f.ticks, func() {
				if release != nil {
					<-release
				}
			}
		},
	}
}

func (f *fakeSystem) commandsNamed(name string) []command {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []command
	for _, c := range f.commands {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// capturedLog is a logger that keeps its messages.
func capturedLog() (*slog.Logger, *bytes.Buffer, *sync.Mutex) {
	var buf bytes.Buffer
	var mu sync.Mutex
	return slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)), &buf, &mu
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// startMount mounts a fake tree on a temporary directory with the fake system and
// returns the live mount and the resolved target.
func startMount(t *testing.T, sys *fakeSystem, opts mount.Options) (*mounted, string) {
	t.Helper()
	dir := t.TempDir()
	opts.Target = dir
	m, err := mountWith(newFakeFS().addFile("a.txt", []byte("hello")), opts, sys.environment())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.(*mounted).srv.Close() })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m.(*mounted), resolved
}

func TestMountRunsMountNFS(t *testing.T) {
	t.Parallel()
	sys := newFakeSystem()
	m, target := startMount(t, sys, mount.Options{Source: "splicefs:example/media"})

	calls := sys.commandsNamed("/sbin/mount_nfs")
	if len(calls) != 1 {
		t.Fatalf("mount_nfs ran %d times", len(calls))
	}
	want := mountArgs(m.srv.Port(), "127.0.0.1:/splicefs:example/media", target)
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("mount_nfs arguments = %q\nwant %q", calls[0].args, want)
	}
	select {
	case <-m.Done():
		t.Error("Done is closed on a live mount")
	default:
	}
	if sys.tickers != 1 {
		t.Errorf("%d tickers started, want the one that watches the mount table", sys.tickers)
	}
}

func TestMountFailures(t *testing.T) {
	t.Parallel()
	t.Run("mount_nfs fails", func(t *testing.T) {
		t.Parallel()
		sys := newFakeSystem()
		sys.handle = func(command) ([]byte, error) {
			return []byte("mount_nfs: can't mount: Operation not permitted\n"), errors.New("exit status 1")
		}
		log, _, _ := capturedLog()
		env := sys.environment()
		var listener net.Listener
		env.listen = func(ctx context.Context) (net.Listener, error) {
			var err error
			listener, err = listenLoopback(ctx)
			return listener, err
		}
		_, err := mountWith(newFakeFS(), mount.Options{Target: t.TempDir(), Logger: log}, env)
		if err == nil || !strings.Contains(err.Error(), "Operation not permitted") ||
			!strings.Contains(err.Error(), "exit status 1") {
			t.Fatalf("error = %v, want the exit status and what mount_nfs printed", err)
		}
		// The server that was started for the mount is gone.
		wantClosed(t, listener)
	})
	t.Run("mount_nfs fails silently", func(t *testing.T) {
		t.Parallel()
		sys := newFakeSystem()
		sys.handle = func(command) ([]byte, error) { return nil, errors.New("exit status 1") }
		if _, err := mountWith(newFakeFS(), mount.Options{Target: t.TempDir()}, sys.environment()); err == nil ||
			strings.HasSuffix(err.Error(), ": ") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("cannot listen", func(t *testing.T) {
		t.Parallel()
		sys := newFakeSystem()
		env := sys.environment()
		env.listen = func(context.Context) (net.Listener, error) { return nil, errors.New("no sockets") }
		if _, err := mountWith(newFakeFS(), mount.Options{Target: t.TempDir()}, env); err == nil {
			t.Error("no error")
		}
		if len(sys.commands) != 0 {
			t.Errorf("commands ran: %v", sys.commands)
		}
	})
	t.Run("listener that cannot be used", func(t *testing.T) {
		t.Parallel()
		sys := newFakeSystem()
		env := sys.environment()
		env.listen = func(context.Context) (net.Listener, error) { return newScriptedListener("a pipe"), nil }
		if _, err := mountWith(newFakeFS(), mount.Options{Target: t.TempDir()}, env); err == nil {
			t.Error("no error")
		}
		if len(sys.commands) != 0 {
			t.Errorf("commands ran: %v", sys.commands)
		}
	})
	t.Run("target does not exist", func(t *testing.T) {
		t.Parallel()
		sys := newFakeSystem()
		_, err := mountWith(newFakeFS(), mount.Options{Target: filepath.Join(t.TempDir(), "missing")}, sys.environment())
		if err == nil {
			t.Fatal("no error")
		}
		if len(sys.commands) != 0 {
			t.Errorf("commands ran: %v", sys.commands)
		}
	})
}

func TestUnmount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		force bool
		args  func(target string) []string
	}{
		{"without force", false, func(target string) []string { return []string{target} }},
		{"with force", true, func(target string) []string { return []string{"-f", target} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sys := newFakeSystem()
			m, target := startMount(t, sys, mount.Options{})

			if err := m.Unmount(t.Context(), tt.force); err != nil {
				t.Fatal(err)
			}
			calls := sys.commandsNamed("/sbin/umount")
			if len(calls) != 1 || !slices.Equal(calls[0].args, tt.args(target)) {
				t.Errorf("umount calls = %v, want one with %q", calls, tt.args(target))
			}
			select {
			case <-m.Done():
			default:
				t.Fatal("Done is not closed when Unmount returns")
			}
			if m.Err() != nil {
				t.Errorf("Err() = %v after a clean unmount", m.Err())
			}
			wantClosed(t, m.srv.guard.Listener)

			if err := m.Unmount(t.Context(), tt.force); err != nil { // already gone
				t.Errorf("second Unmount = %v", err)
			}
			if n := len(sys.commandsNamed("/sbin/umount")); n != 1 {
				t.Errorf("umount ran %d times", n)
			}
		})
	}
}

func TestUnmountOfAMountInUseIsRefusedAndCanBeRepeated(t *testing.T) {
	t.Parallel()
	sys := newFakeSystem()
	busy := true
	sys.handle = func(c command) ([]byte, error) {
		if c.name == "/sbin/umount" && busy {
			return []byte("umount: " + c.args[0] + ": Resource busy\n"), errors.New("exit status 1")
		}
		return nil, nil
	}
	m, _ := startMount(t, sys, mount.Options{})

	err := m.Unmount(t.Context(), false)
	if err == nil || !strings.Contains(err.Error(), "Resource busy") || !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Unmount of a busy mount = %v, want an error that is EBUSY", err)
	}
	select {
	case <-m.Done():
		t.Fatal("the mount ended although umount refused")
	default:
	}
	select {
	case <-m.srv.Done():
		t.Fatal("the server stopped although the mount is still there")
	default:
	}

	busy = false
	if err := m.Unmount(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	<-m.Done()
}

func TestUnmountFailureThatIsNotBusyIsNotEBUSY(t *testing.T) {
	t.Parallel()
	sys := newFakeSystem()
	sys.handle = func(c command) ([]byte, error) {
		if c.name != "/sbin/umount" {
			return nil, nil
		}
		return []byte("umount: Operation not permitted\n"), errors.New("exit status 1")
	}
	m, _ := startMount(t, sys, mount.Options{})
	err := m.Unmount(t.Context(), false)
	if err == nil || errors.Is(err, syscall.EBUSY) || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("Unmount = %v, want the failure without EBUSY", err)
	}
}

func TestUnmountNoticesThatTheMountWentAwayMeanwhile(t *testing.T) {
	t.Parallel()
	sys := newFakeSystem()
	sys.presence = []presence{{mounted: true}, {mounted: false}}
	m, _ := startMount(t, sys, mount.Options{})
	sys.handle = func(command) ([]byte, error) {
		// Someone else unmounts first: the poll sees it while umount is failing.
		sys.ticks <- time.Time{}
		<-m.Done()
		return nil, errors.New("exit status 1")
	}
	if err := m.Unmount(t.Context(), false); err != nil {
		t.Errorf("Unmount = %v, want nil: the mount is gone", err)
	}
}

func TestUnmountGivesUpWaitingWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	sys := newFakeSystem()
	sys.release = make(chan struct{})
	m, _ := startMount(t, sys, mount.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// umount succeeds, but the supervisor is held back before it closes Done.
	err := m.Unmount(ctx, false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Unmount = %v, want the context error", err)
	}
	close(sys.release)
	<-m.Done()
}

func TestMountEndsWhenItDisappears(t *testing.T) {
	t.Parallel()
	sys := newFakeSystem()
	sys.presence = []presence{
		{mounted: true},                 // right after mount_nfs
		{err: errors.New("table gone")}, // a failed read is skipped
		{mounted: true},
		{mounted: false}, // unmounted from outside
	}
	log, buf, mu := capturedLog()
	m, _ := startMount(t, sys, mount.Options{Logger: log})

	for range 3 {
		sys.ticks <- time.Time{}
	}
	<-m.Done()
	if m.Err() != nil {
		t.Errorf("Err() = %v after an unmount from outside", m.Err())
	}
	if n := len(sys.commandsNamed("/sbin/umount")); n != 0 {
		t.Errorf("umount ran %d times for a mount that was already gone", n)
	}
	wantClosed(t, m.srv.guard.Listener)
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "table gone") {
		t.Errorf("the failed read of the mount table was not logged: %s", buf)
	}
}

func TestMountThatIsNotInTheTableIsNotPolled(t *testing.T) {
	t.Parallel()
	for name, first := range map[string]presence{
		"not listed":   {mounted: false},
		"table failed": {err: errors.New("no table")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sys := newFakeSystem()
			sys.presence = []presence{first}
			log, buf, mu := capturedLog()
			m, _ := startMount(t, sys, mount.Options{Logger: log})
			if sys.tickers != 0 {
				t.Error("polling started for a mount that could not be found")
			}
			mu.Lock()
			logged := buf.String()
			mu.Unlock()
			if !strings.Contains(logged, "mount not found in the mount table") {
				t.Errorf("no warning: %s", logged)
			}
			if err := m.Unmount(t.Context(), false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMountEndsWhenTheServerFails(t *testing.T) {
	t.Parallel()
	for name, umountErr := range map[string]error{"umount works": nil, "umount fails": errors.New("exit status 1")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sys := newFakeSystem()
			sys.handle = func(c command) ([]byte, error) {
				if c.name == "/sbin/umount" && umountErr != nil {
					return []byte("umount: not mounted\n"), umountErr
				}
				return nil, nil
			}
			m, target := startMount(t, sys, mount.Options{})

			_ = m.srv.guard.Listener.Close() // the listening socket breaks under the server
			<-m.Done()

			if err := m.Err(); err == nil || !strings.Contains(err.Error(), "serve nfs") {
				t.Fatalf("Err() = %v, want the server failure", err)
			}
			if umountErr != nil && !strings.Contains(m.Err().Error(), "not mounted") {
				t.Errorf("Err() = %v, want the umount failure as well", m.Err())
			}
			calls := sys.commandsNamed("/sbin/umount")
			if len(calls) != 1 || !slices.Equal(calls[0].args, []string{"-f", target}) {
				t.Errorf("umount calls = %v, want one forced unmount of the dead mount", calls)
			}
		})
	}
}

func statfs(fstype, on, from string) syscall.Statfs_t {
	var st syscall.Statfs_t
	setCString(st.Fstypename[:], fstype)
	setCString(st.Mntonname[:], on)
	setCString(st.Mntfromname[:], from)
	return st
}

func setCString(dst []int8, s string) {
	for i := range len(s) {
		dst[i] = int8(s[i]) //nolint:gosec // a byte is reinterpreted as a C char
	}
}

func TestMountTableHas(t *testing.T) {
	t.Parallel()
	const target, from = "/private/mnt/media", "127.0.0.1:/splicefs:example/media"
	table := []syscall.Statfs_t{
		statfs("apfs", "/", "/dev/disk3s1s1"),
		statfs("nfs", "/private/mnt/other", from),
		statfs("nfs", target, "127.0.0.1:/other"),
		statfs("smbfs", target, from),
	}
	getfsstat := func(entries []syscall.Statfs_t) func([]syscall.Statfs_t, int) (int, error) {
		return func(buf []syscall.Statfs_t, flags int) (int, error) {
			if flags != mntNoWait {
				t.Errorf("getfsstat flags = %d, want MNT_NOWAIT", flags)
			}
			if buf == nil { // like the real call, a nil buffer asks for the count
				return len(entries), nil
			}
			return copy(buf, entries), nil
		}
	}
	if got, err := mountTableHas(getfsstat(table), target, from); err != nil || got {
		t.Errorf("without our mount: %v, %v", got, err)
	}
	table = append(table, statfs("nfs", target, from))
	if got, err := mountTableHas(getfsstat(table), target, from); err != nil || !got {
		t.Errorf("with our mount: %v, %v", got, err)
	}
}

func TestMountTableHasReportsSystemErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	first := func([]syscall.Statfs_t, int) (int, error) { return 0, boom }
	calls := 0
	second := func(_ []syscall.Statfs_t, _ int) (int, error) {
		if calls++; calls == 1 {
			return 2, nil
		}
		return 0, boom
	}
	for name, getfsstat := range map[string]func([]syscall.Statfs_t, int) (int, error){"count": first, "read": second} {
		if _, err := mountTableHas(getfsstat, "/t", "f"); !errors.Is(err, boom) {
			t.Errorf("%s: error = %v, want boom", name, err)
		}
	}
}

func TestMountTableHasReadsTheRealTable(t *testing.T) {
	t.Parallel()
	if got, err := mountTableHas(syscall.Getfsstat, "/not/a/mount/point", "127.0.0.1:/none"); err != nil || got {
		t.Errorf("mountTableHas = %v, %v", got, err)
	}
}

func TestCString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		chars []int8
		want  string
	}{
		{"terminated", []int8{'n', 'f', 's', 0, 'x'}, "nfs"},
		{"full", []int8{'a', 'b'}, "ab"},
		{"empty", []int8{0}, ""},
		{"utf-8", []int8{-61, -87, 0}, "\u00e9"},
	}
	for _, tt := range tests {
		if got := cString(tt.chars); got != tt.want {
			t.Errorf("%s: cString = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestExecCommand(t *testing.T) {
	t.Parallel()
	out, err := execCommand(t.Context(), "/bin/echo", "hello", "world")
	if err != nil || string(out) != "hello world\n" {
		t.Errorf("echo = %q, %v", out, err)
	}
	if _, err := execCommand(t.Context(), "/usr/bin/false"); err == nil {
		t.Error("a failing command reported success")
	}
}

func TestSystemEnvironment(t *testing.T) {
	t.Parallel()
	env := systemEnvironment()
	if env.owner != processOwner() {
		t.Errorf("owner = %+v", env.owner)
	}
	if mounted, err := env.isMounted("/not/a/mount/point", "127.0.0.1:/none"); err != nil || mounted {
		t.Errorf("isMounted = %v, %v", mounted, err)
	}
	ticks, stop := env.newTicker(time.Hour)
	stop()
	select {
	case <-ticks:
		t.Error("a stopped ticker ticked")
	default:
	}
}

// TestMountWithTheSystemTools runs the real mount_nfs on a target that cannot be
// mounted on, which fails without needing any privilege and without mounting anything.
func TestMountWithTheSystemTools(t *testing.T) {
	t.Parallel()
	_, err := Mount(newFakeFS(), mount.Options{Target: filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("mounted on a directory that does not exist")
	}
}
