package fuse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SpliceFS/client/mount"
)

// fakeHost is a host whose answers a test scripts.
type fakeHost struct {
	statResults []statResult // consumed in order; the last one repeats
	statCalls   atomic.Int32
	unmount     func(ctx context.Context, target string, detach bool) error
	connID      uint32 // what ConnectionID answers, unless connErr is set
	connErr     error
	abortErr    error

	mu      sync.Mutex
	detachs []bool
	events  []string // "unmount" and "abort <id>", in the order they happened
}

type statResult struct {
	info fs.FileInfo
	err  error
}

func (sys *fakeHost) Stat(string) (fs.FileInfo, error) {
	i := min(int(sys.statCalls.Add(1))-1, len(sys.statResults)-1)
	return sys.statResults[i].info, sys.statResults[i].err
}

func (sys *fakeHost) Unmount(ctx context.Context, target string, detach bool) error {
	sys.mu.Lock()
	sys.detachs = append(sys.detachs, detach)
	sys.events = append(sys.events, "unmount")
	sys.mu.Unlock()
	return sys.unmount(ctx, target, detach)
}

func (sys *fakeHost) ConnectionID(string) (uint32, error) { return sys.connID, sys.connErr }

func (sys *fakeHost) AbortConnection(id uint32) error {
	sys.mu.Lock()
	defer sys.mu.Unlock()
	sys.events = append(sys.events, fmt.Sprintf("abort %d", id))
	return sys.abortErr
}

func (sys *fakeHost) eventLog() []string {
	sys.mu.Lock()
	defer sys.mu.Unlock()
	return append([]string(nil), sys.events...)
}

func (sys *fakeHost) unmountCalls() []bool {
	sys.mu.Lock()
	defer sys.mu.Unlock()
	return append([]bool(nil), sys.detachs...)
}

// abortOf calls Abort on a mount, which every mount of this package supports.
func abortOf(t *testing.T, mounted mount.Mounted) error {
	t.Helper()
	aborter, ok := mounted.(mount.Aborter)
	if !ok {
		t.Fatalf("%T does not implement mount.Aborter", mounted)
	}
	return aborter.Abort()
}

func okStat() []statResult { return []statResult{{}} }

// fakeLoop is a serve loop that runs until release is called.
type fakeLoop struct {
	released  chan struct{}
	once      sync.Once
	waitMount error
}

func newFakeLoop(t *testing.T) *fakeLoop {
	t.Helper()
	l := &fakeLoop{released: make(chan struct{})}
	t.Cleanup(l.release)
	return l
}

func (l *fakeLoop) Serve()           { <-l.released }
func (l *fakeLoop) WaitMount() error { return l.waitMount }
func (l *fakeLoop) release()         { l.once.Do(func() { close(l.released) }) }

// releasing is an unmount that succeeds and ends the loop, like the kernel does.
func releasing(l *fakeLoop) func(context.Context, string, bool) error {
	return func(context.Context, string, bool) error {
		l.release()
		return nil
	}
}

func isDone(live mount.Mounted) bool {
	select {
	case <-live.Done():
		return true
	default:
		return false
	}
}

func waitDone(t *testing.T, live mount.Mounted) {
	t.Helper()
	select {
	case <-live.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("mount did not end")
	}
}

func dirInfo(t *testing.T) fs.FileInfo {
	t.Helper()
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func fileInfo(t *testing.T) fs.FileInfo {
	t.Helper()
	path := t.TempDir() + "/f"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestPrepareTarget(t *testing.T) {
	t.Parallel()
	unmountErr := errors.New("umount failed")
	tests := []struct {
		name       string
		stats      []statResult
		unmount    error
		wantErr    error // nil means success
		wantDetach []bool
	}{
		{name: "directory", stats: []statResult{{info: dirInfo(t)}}},
		{
			name:    "does not exist",
			stats:   []statResult{{err: &fs.PathError{Op: "stat", Path: "/x", Err: syscall.ENOENT}}},
			wantErr: syscall.ENOENT,
		},
		{name: "not a directory", stats: []statResult{{info: fileInfo(t)}}, wantErr: syscall.ENOTDIR},
		{
			name: "dead mount is detached",
			stats: []statResult{
				{err: &fs.PathError{Op: "stat", Path: "/x", Err: syscall.ENOTCONN}},
				{info: dirInfo(t)},
			},
			wantDetach: []bool{true},
		},
		{
			name:       "dead mount that cannot be detached",
			stats:      []statResult{{err: syscall.ENOTCONN}},
			unmount:    unmountErr,
			wantErr:    unmountErr,
			wantDetach: []bool{true},
		},
		{
			name: "target gone after the dead mount was detached",
			stats: []statResult{
				{err: syscall.ENOTCONN},
				{err: syscall.ENOENT},
			},
			wantErr:    syscall.ENOENT,
			wantDetach: []bool{true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sys := &fakeHost{
				statResults: tt.stats,
				unmount:     func(context.Context, string, bool) error { return tt.unmount },
			}
			err := prepareTarget(context.Background(), sys, "/x")
			if tt.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want one wrapping %v", err, tt.wantErr)
			}
			got := sys.unmountCalls()
			if fmt.Sprint(got) != fmt.Sprint(append([]bool(nil), tt.wantDetach...)) {
				t.Fatalf("unmount calls = %v, want %v", got, tt.wantDetach)
			}
		})
	}
}

func TestCleanUnmountWaitsForTheLoopAndReportsNoError(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), unmount: releasing(loop)}
	live, err := start(loop, sys, "/mnt/clean")
	if err != nil {
		t.Fatal(err)
	}
	if isDone(live) {
		t.Fatal("mount is done before unmount")
	}
	if err := live.Unmount(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !isDone(live) {
		t.Fatal("Unmount returned before the mount ended")
	}
	if live.Err() != nil {
		t.Fatalf("Err = %v, want nil after a clean unmount", live.Err())
	}
	if got := sys.unmountCalls(); len(got) != 1 || got[0] {
		t.Fatalf("unmount calls = %v, want one without detach", got)
	}
}

func TestForcedUnmountDetachesWithoutWaitingForTheLoop(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return nil }}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Unmount(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if isDone(live) {
		t.Fatal("mount ended although a user still holds it")
	}
	if got := sys.unmountCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("unmount calls = %v, want one with detach", got)
	}
	loop.release()
	waitDone(t, live)
	if live.Err() != nil {
		t.Fatalf("Err = %v", live.Err())
	}
}

func TestForcedUnmountOnlyDetaches(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), connID: 42, unmount: func(context.Context, string, bool) error { return nil }}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Unmount(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got, want := sys.eventLog(), []string{"unmount"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestPlainUnmountDoesNotAbortTheConnection(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), connID: 42, unmount: releasing(loop)}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Unmount(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got, want := sys.eventLog(), []string{"unmount"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestAbortCutsTheRememberedConnection(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), connID: 42, unmount: func(context.Context, string, bool) error { return nil }}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if got := sys.eventLog(); len(got) != 0 {
		t.Fatalf("events before the abort = %v, want none", got)
	}
	if err := abortOf(t, live); err != nil {
		t.Fatalf("Abort = %v, want nil", err)
	}
	if got, want := sys.eventLog(), []string{"abort 42"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestAbortOfAConnectionThatIsGoneIsNotAnError(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), connID: 7, abortErr: fs.ErrNotExist}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := abortOf(t, live); err != nil {
		t.Fatalf("Abort = %v, want a missing control file to be no error", err)
	}
	if got, want := sys.eventLog(), []string{"abort 7"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestAbortReturnsAFailedWrite(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), connID: 7, abortErr: syscall.EACCES}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := abortOf(t, live); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("Abort = %v, want %v", err, syscall.EACCES)
	}
}

func TestAbortWithoutARememberedConnectionDoesNothing(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), connErr: syscall.EIO}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatalf("start = %v, want a mount whose connection is unknown to still work", err)
	}
	if err := abortOf(t, live); err != nil {
		t.Fatalf("Abort = %v, want nil", err)
	}
	if got := sys.eventLog(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestUnmountOfBusyMountIsRefused(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	busy := fmt.Errorf("unmount /x: %w", syscall.EBUSY)
	sys := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return busy }}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Unmount(context.Background(), false); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("error = %v, want one wrapping EBUSY", err)
	}
	if isDone(live) {
		t.Fatal("refused unmount ended the mount")
	}
}

func TestUnmountOfEndedMountDoesNothing(t *testing.T) {
	t.Parallel()
	sys := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error {
		t.Error("host asked to unmount a mount that has ended")
		return nil
	}}
	live, err := start(&fakeLoopFunc{serve: func() {}}, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, live)
	if err := live.Unmount(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

// fakeLoopFunc is a serve loop with a scripted Serve.
type fakeLoopFunc struct{ serve func() }

func (l *fakeLoopFunc) Serve()         { l.serve() }
func (*fakeLoopFunc) WaitMount() error { return nil }

func TestUnmountHonoursContextWhileTheHostIsStuck(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	sys := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error {
		<-stuck
		return nil
	}}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := live.Unmount(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one wrapping context.Canceled", err)
	}
}

// cancelsOnSecondDone is a context that reports cancellation only from the second time
// its Done channel is asked for. It lets a test cancel in the middle of a call without a
// race: Unmount asks once while it unmounts and once while it waits for the loop.
type cancelsOnSecondDone struct {
	calls  atomic.Int32
	open   chan struct{}
	closed chan struct{}
}

var _ context.Context = (*cancelsOnSecondDone)(nil)

func newCancelsOnSecondDone() *cancelsOnSecondDone {
	cancelCtx := &cancelsOnSecondDone{
		open:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	close(cancelCtx.closed)
	return cancelCtx
}

func (*cancelsOnSecondDone) Deadline() (time.Time, bool) { return time.Time{}, false }

func (*cancelsOnSecondDone) Value(any) any { return nil }

func (cancelCtx *cancelsOnSecondDone) Done() <-chan struct{} {
	if cancelCtx.calls.Add(1) < 2 {
		return cancelCtx.open
	}
	return cancelCtx.closed
}

func (*cancelsOnSecondDone) Err() error { return context.Canceled }

func TestUnmountHonoursContextWhileWaitingForTheLoop(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	sys := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return nil }}
	live, err := start(loop, sys, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Unmount(newCancelsOnSecondDone(), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one wrapping context.Canceled", err)
	}
}

func TestStartTearsDownAMountThatNeverBecomesReady(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	loop.waitMount = errors.New("not ready")
	sys := &fakeHost{statResults: okStat(), unmount: releasing(loop)}
	live, err := start(loop, sys, "/x")
	if live != nil || !errors.Is(err, loop.waitMount) {
		t.Fatalf("start = %v, %v; want nil and the wait error", live, err)
	}
	if got := sys.unmountCalls(); len(got) != 1 {
		t.Fatalf("unmount calls = %v, want the mount taken down once", got)
	}
}

func TestStartReportsBothFailuresWhenTeardownFails(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	loop.waitMount = errors.New("not ready")
	teardown := errors.New("cannot unmount")
	sys := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return teardown }}
	_, err := start(loop, sys, "/x")
	if !errors.Is(err, loop.waitMount) || !errors.Is(err, teardown) {
		t.Fatalf("error = %v, want both failures", err)
	}
}

func TestErrReportsALostConnection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stat    error
		wantErr error
	}{
		{"ordinary directory after the end", nil, nil},
		{"dead mount after the end", &fs.PathError{Op: "stat", Path: "/x", Err: syscall.ENOTCONN}, syscall.ENOTCONN},
		{"other stat failure is not a lost connection", syscall.EACCES, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sys := &fakeHost{statResults: []statResult{{err: tt.stat}}}
			live, err := start(&fakeLoopFunc{serve: func() {}}, sys, "/x")
			if err != nil {
				t.Fatal(err)
			}
			waitDone(t, live)
			if tt.wantErr == nil && live.Err() != nil {
				t.Fatalf("Err = %v, want nil", live.Err())
			}
			if tt.wantErr != nil && !errors.Is(live.Err(), tt.wantErr) {
				t.Fatalf("Err = %v, want one wrapping %v", live.Err(), tt.wantErr)
			}
		})
	}
}
