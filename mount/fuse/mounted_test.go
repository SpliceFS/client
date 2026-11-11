package fuse

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/BlueRival/splicefs/client/mount"
)

// fakeHost is a host whose answers a test scripts.
type fakeHost struct {
	statResults []statResult // consumed in order; the last one repeats
	statCalls   atomic.Int32
	unmount     func(ctx context.Context, target string, detach bool) error

	mu      sync.Mutex
	detachs []bool
}

type statResult struct {
	info fs.FileInfo
	err  error
}

func (h *fakeHost) Stat(string) (fs.FileInfo, error) {
	i := min(int(h.statCalls.Add(1))-1, len(h.statResults)-1)
	return h.statResults[i].info, h.statResults[i].err
}

func (h *fakeHost) Unmount(ctx context.Context, target string, detach bool) error {
	h.mu.Lock()
	h.detachs = append(h.detachs, detach)
	h.mu.Unlock()
	return h.unmount(ctx, target, detach)
}

func (h *fakeHost) unmountCalls() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.detachs...)
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

func (l *fakeLoop) release()         { l.once.Do(func() { close(l.released) }) }
func (l *fakeLoop) Serve()           { <-l.released }
func (l *fakeLoop) WaitMount() error { return l.waitMount }

// releasing is an unmount that succeeds and ends the loop, like the kernel does.
func releasing(l *fakeLoop) func(context.Context, string, bool) error {
	return func(context.Context, string, bool) error {
		l.release()
		return nil
	}
}

func isDone(m mount.Mounted) bool {
	select {
	case <-m.Done():
		return true
	default:
		return false
	}
}

func waitDone(t *testing.T, m mount.Mounted) {
	t.Helper()
	select {
	case <-m.Done():
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
			h := &fakeHost{
				statResults: tt.stats,
				unmount:     func(context.Context, string, bool) error { return tt.unmount },
			}
			err := prepareTarget(context.Background(), h, "/x")
			if tt.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want one wrapping %v", err, tt.wantErr)
			}
			got := h.unmountCalls()
			if fmt.Sprint(got) != fmt.Sprint(append([]bool(nil), tt.wantDetach...)) {
				t.Fatalf("unmount calls = %v, want %v", got, tt.wantDetach)
			}
		})
	}
}

func TestCleanUnmountWaitsForTheLoopAndReportsNoError(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	h := &fakeHost{statResults: okStat(), unmount: releasing(loop)}
	m, err := start(loop, h, "/mnt/clean")
	if err != nil {
		t.Fatal(err)
	}
	if isDone(m) {
		t.Fatal("mount is done before unmount")
	}
	if err := m.Unmount(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !isDone(m) {
		t.Fatal("Unmount returned before the mount ended")
	}
	if m.Err() != nil {
		t.Fatalf("Err = %v, want nil after a clean unmount", m.Err())
	}
	if got := h.unmountCalls(); len(got) != 1 || got[0] {
		t.Fatalf("unmount calls = %v, want one without detach", got)
	}
}

func TestForcedUnmountDetachesWithoutWaitingForTheLoop(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	h := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return nil }}
	m, err := start(loop, h, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Unmount(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if isDone(m) {
		t.Fatal("mount ended although a user still holds it")
	}
	if got := h.unmountCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("unmount calls = %v, want one with detach", got)
	}
	loop.release()
	waitDone(t, m)
	if m.Err() != nil {
		t.Fatalf("Err = %v", m.Err())
	}
}

func TestUnmountOfBusyMountIsRefused(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	busy := fmt.Errorf("unmount /x: %w", syscall.EBUSY)
	h := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return busy }}
	m, err := start(loop, h, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Unmount(context.Background(), false); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("error = %v, want one wrapping EBUSY", err)
	}
	if isDone(m) {
		t.Fatal("refused unmount ended the mount")
	}
}

func TestUnmountOfEndedMountDoesNothing(t *testing.T) {
	t.Parallel()
	h := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error {
		t.Error("host asked to unmount a mount that has ended")
		return nil
	}}
	m, err := start(&fakeLoopFunc{serve: func() {}}, h, "/x")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, m)
	if err := m.Unmount(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

// fakeLoopFunc is a serve loop with a scripted Serve.
type fakeLoopFunc struct{ serve func() }

func (l *fakeLoopFunc) Serve()           { l.serve() }
func (l *fakeLoopFunc) WaitMount() error { return nil }

func TestUnmountHonoursContextWhileTheHostIsStuck(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	h := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error {
		<-stuck
		return nil
	}}
	m, err := start(loop, h, "/x")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Unmount(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one wrapping context.Canceled", err)
	}
}

// cancelsOnSecondDone is a context that reports cancellation only from the second time
// its Done channel is asked for. It lets a test cancel in the middle of a call without a
// race: Unmount asks once while it unmounts and once while it waits for the loop.
type cancelsOnSecondDone struct {
	context.Context
	calls  atomic.Int32
	open   chan struct{}
	closed chan struct{}
}

func newCancelsOnSecondDone() *cancelsOnSecondDone {
	c := &cancelsOnSecondDone{
		Context: context.Background(),
		open:    make(chan struct{}),
		closed:  make(chan struct{}),
	}
	close(c.closed)
	return c
}

func (c *cancelsOnSecondDone) Done() <-chan struct{} {
	if c.calls.Add(1) < 2 {
		return c.open
	}
	return c.closed
}

func (c *cancelsOnSecondDone) Err() error { return context.Canceled }

func TestUnmountHonoursContextWhileWaitingForTheLoop(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	h := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return nil }}
	m, err := start(loop, h, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Unmount(newCancelsOnSecondDone(), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one wrapping context.Canceled", err)
	}
}

func TestStartTearsDownAMountThatNeverBecomesReady(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	loop.waitMount = errors.New("not ready")
	h := &fakeHost{statResults: okStat(), unmount: releasing(loop)}
	m, err := start(loop, h, "/x")
	if m != nil || !errors.Is(err, loop.waitMount) {
		t.Fatalf("start = %v, %v; want nil and the wait error", m, err)
	}
	if got := h.unmountCalls(); len(got) != 1 {
		t.Fatalf("unmount calls = %v, want the mount taken down once", got)
	}
}

func TestStartReportsBothFailuresWhenTeardownFails(t *testing.T) {
	t.Parallel()
	loop := newFakeLoop(t)
	loop.waitMount = errors.New("not ready")
	teardown := errors.New("cannot unmount")
	h := &fakeHost{statResults: okStat(), unmount: func(context.Context, string, bool) error { return teardown }}
	_, err := start(loop, h, "/x")
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
			h := &fakeHost{statResults: []statResult{{err: tt.stat}}}
			m, err := start(&fakeLoopFunc{serve: func() {}}, h, "/x")
			if err != nil {
				t.Fatal(err)
			}
			waitDone(t, m)
			if tt.wantErr == nil && m.Err() != nil {
				t.Fatalf("Err = %v, want nil", m.Err())
			}
			if tt.wantErr != nil && !errors.Is(m.Err(), tt.wantErr) {
				t.Fatalf("Err = %v, want one wrapping %v", m.Err(), tt.wantErr)
			}
		})
	}
}
