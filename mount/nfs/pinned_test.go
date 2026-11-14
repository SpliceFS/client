package nfs

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs"

	"github.com/SpliceFS/client/core"
)

const pinnedName = "dir/hello.txt"

// fakeClock stands in for time in the tests of pinnedFiles: a timer runs when the test
// says so, never because time has passed.
type fakeClock struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

type fakeTimer struct {
	run     func()
	stopped bool
}

func (c *fakeClock) after(_ time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeTimer{run: f}
	c.timers = append(c.timers, timer)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		wasLive := !timer.stopped
		timer.stopped = true
		return wasLive
	}
}

// take removes the timers that match and returns what they would run.
func (c *fakeClock) take(match func(*fakeTimer) bool) []func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []func()
	c.timers = slices.DeleteFunc(c.timers, func(timer *fakeTimer) bool {
		if !match(timer) {
			return false
		}
		out = append(out, timer.run)
		return true
	})
	return out
}

// quiet lets the quiet time pass: every timer that is still set runs.
func (c *fakeClock) quiet() {
	for _, run := range c.take(func(timer *fakeTimer) bool { return !timer.stopped }) {
		run()
	}
}

// late runs the timers that were stopped. A timer that is already running when it is
// stopped runs anyway, and the code must not be fooled by it.
func (c *fakeClock) late() {
	for _, run := range c.take(func(timer *fakeTimer) bool { return timer.stopped }) {
		run()
	}
}

func newPinnedTest(t *testing.T) (*billyFS, *fakeFS, *fakeClock) {
	t.Helper()
	fsys := treeFS()
	b := newTestBilly(t, fsys)
	clock := &fakeClock{}
	b.pins.after = clock.after
	return b, fsys, clock
}

// count is how many times the core.FS was called with call, written "op:name".
func (f *fakeFS) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func opens(f *fakeFS) int  { return f.count("open:" + pinnedName) }
func closes(f *fakeFS) int { return f.count("close:" + pinnedName) }

// readOnce does what go-nfs does for a READ request: open, read, close.
func readOnce(t *testing.T, b *billyFS) string {
	t.Helper()
	opened, err := b.Open(pinnedName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	buf := make([]byte, 5)
	n, err := opened.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return string(buf[:n])
}

// waitUsers waits until want reads hold the pin of name. It is how a test knows that the
// reads it started have all arrived.
func waitUsers(t *testing.T, files *pinnedFiles, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		files.mu.Lock()
		users := 0
		if held := files.byName[name]; held != nil {
			users = held.users
		}
		files.mu.Unlock()
		if users == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d reads hold %q, want %d", users, name, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPinnedTwoReadsWithinTheQuietTimeUseOneOpen(t *testing.T) {
	t.Parallel()
	b, fsys, _ := newPinnedTest(t)
	for range 2 {
		if got := readOnce(t, b); got != "hello" {
			t.Errorf("read %q", got)
		}
	}
	if opens(fsys) != 1 || closes(fsys) != 0 {
		t.Errorf("opens = %d, closes = %d; want 1 open and the file still open", opens(fsys), closes(fsys))
	}
}

func TestPinnedReadAfterTheQuietTimeOpensAgain(t *testing.T) {
	t.Parallel()
	b, fsys, clock := newPinnedTest(t)
	readOnce(t, b)
	clock.quiet()
	if closes(fsys) != 1 {
		t.Errorf("closes = %d after the quiet time, want 1", closes(fsys))
	}
	// The file is replaced while nobody reads: the next read sees the new copy.
	fsys.addFile(pinnedName, []byte("HELLO again"))
	if got := readOnce(t, b); got != "HELLO" {
		t.Errorf("read %q after the quiet time, want the new copy", got)
	}
	if opens(fsys) != 2 {
		t.Errorf("opens = %d, want 2", opens(fsys))
	}
}

func TestPinnedReplacedFileIsNotSeenWhileReadsKeepComing(t *testing.T) {
	t.Parallel()
	b, fsys, clock := newPinnedTest(t)
	readOnce(t, b)
	fsys.addFile(pinnedName, []byte("HELLO again"))
	clock.quiet()
	// A read that came before the quiet time ran out took the file back: no timer is left
	// that could close it.
	readOnce(t, b)
	if got := readOnce(t, b); got != "HELLO" {
		t.Errorf("read %q, want the new copy after the quiet time", got)
	}
}

func TestPinnedReadKeepsTheVersionItOpened(t *testing.T) {
	t.Parallel()
	b, fsys, _ := newPinnedTest(t)
	if got := readOnce(t, b); got != "hello" {
		t.Fatalf("read %q", got)
	}
	fsys.addFile(pinnedName, []byte("HELLO again"))
	if got := readOnce(t, b); got != "hello" {
		t.Errorf("read %q, want the copy that was opened first", got)
	}
}

func TestPinnedTimerThatFiresLateDoesNothing(t *testing.T) {
	t.Parallel()
	b, fsys, clock := newPinnedTest(t)
	readOnce(t, b)
	opened, err := b.Open(pinnedName)
	if err != nil {
		t.Fatal(err)
	}
	// The timer of the first read was stopped by the second open. It runs anyway.
	clock.late()
	if closes(fsys) != 0 {
		t.Fatal("a stopped timer closed a file that is in use")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	clock.quiet()
	if closes(fsys) != 1 {
		t.Errorf("closes = %d, want 1", closes(fsys))
	}
}

func TestPinnedReadsThatArriveTogetherUseOneOpen(t *testing.T) {
	t.Parallel()
	b, fsys, _ := newPinnedTest(t)
	gate := make(chan struct{})
	fsys.fail = func(op, _ string) error {
		if op == "open" {
			<-gate
		}
		return nil
	}
	const readers = 8
	got := make([]string, readers)
	var wg sync.WaitGroup
	for i := range readers {
		wg.Go(func() { got[i] = readOnce(t, b) })
	}
	waitUsers(t, b.pins, pinnedName, readers)
	close(gate)
	wg.Wait()
	if opens(fsys) != 1 {
		t.Errorf("opens = %d, want 1", opens(fsys))
	}
	for i, g := range got {
		if g != "hello" {
			t.Errorf("reader %d read %q", i, g)
		}
	}
}

func TestPinnedFailedOpenIsSharedByTheReadsThatWaitedAndNotKept(t *testing.T) {
	t.Parallel()
	b, fsys, _ := newPinnedTest(t)
	gate := make(chan struct{})
	fsys.fail = func(op, _ string) error {
		if op == "open" {
			<-gate
			return core.ErrUnavailable
		}
		return nil
	}
	const readers = 3
	errs := make([]error, readers)
	var wg sync.WaitGroup
	for i := range readers {
		wg.Go(func() { _, errs[i] = b.Open(pinnedName) })
	}
	waitUsers(t, b.pins, pinnedName, readers)
	close(gate)
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, syscall.EIO) || !errors.Is(err, core.ErrUnavailable) {
			t.Errorf("reader %d: %v, want an I/O error", i, err)
		}
	}
	fsys.mu.Lock()
	fsys.fail = nil
	fsys.mu.Unlock()
	if got := readOnce(t, b); got != "hello" {
		t.Errorf("read %q after the failed open", got)
	}
	if opens(fsys) != 2 {
		t.Errorf("opens = %d, want the failed one and a new one", opens(fsys))
	}
}

func TestPinnedFailedReadDropsTheFileAndTheNextReadOpensAgain(t *testing.T) {
	t.Parallel()
	b, fsys, _ := newPinnedTest(t)
	reads := openTwo(t, b)
	failing, other := reads[0], reads[1]
	fsys.mu.Lock()
	fsys.fail = func(op, _ string) error {
		if op == "read" {
			return core.ErrUnavailable
		}
		return nil
	}
	fsys.mu.Unlock()
	if _, err := failing.ReadAt(make([]byte, 5), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("ReadAt = %v, want an I/O error", err)
	}
	fsys.mu.Lock()
	fsys.fail = nil
	fsys.mu.Unlock()

	// The other read still holds the dropped file and finishes with it; the last one to
	// finish closes it.
	if err := failing.Close(); err != nil {
		t.Fatal(err)
	}
	if closes(fsys) != 0 {
		t.Fatal("the file was closed while a read still holds it")
	}
	if n, err := other.ReadAt(make([]byte, 5), 0); n != 5 || err != nil {
		t.Errorf("the other read: %d, %v", n, err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if closes(fsys) != 1 {
		t.Errorf("closes = %d, want 1", closes(fsys))
	}
	readOnce(t, b)
	if opens(fsys) != 2 {
		t.Errorf("opens = %d, want a new open after the failed read", opens(fsys))
	}
}

// openTwo opens the file twice, which gives two reads that hold one open.
func openTwo(t *testing.T, b *billyFS) [2]billy.File {
	t.Helper()
	first, err := b.Open(pinnedName)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Open(pinnedName)
	if err != nil {
		t.Fatal(err)
	}
	return [2]billy.File{first, second}
}

func TestPinnedSecondCloseOfAFileIsIgnored(t *testing.T) {
	t.Parallel()
	b, _, _ := newPinnedTest(t)
	opened, err := b.Open(pinnedName)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	b.pins.mu.Lock()
	defer b.pins.mu.Unlock()
	if users := b.pins.byName[pinnedName].users; users != 0 {
		t.Errorf("%d reads hold the file after it was closed twice, want 0", users)
	}
}

func TestPinnedFilesAreClosedWhenTheMountEnds(t *testing.T) {
	t.Parallel()
	b, fsys, clock := newPinnedTest(t)
	fsys.addFile("other.txt", []byte("other"))
	touch(t, b, "other.txt")
	inUse, err := b.Open(pinnedName)
	if err != nil {
		t.Fatal(err)
	}

	b.close()
	b.close()
	if fsys.count("close:other.txt") != 1 {
		t.Errorf("the idle file was not closed at the end of the mount: %v", fsys.calls)
	}
	if closes(fsys) != 0 {
		t.Error("the file that is in use was closed under its read")
	}
	if err := inUse.Close(); err != nil {
		t.Fatal(err)
	}
	if closes(fsys) != 1 {
		t.Errorf("closes = %d, want the file closed when its read ended", closes(fsys))
	}
	clock.quiet()
	if closes(fsys) != 1 || fsys.count("close:other.txt") != 1 {
		t.Errorf("a timer closed a file again: %v", fsys.calls)
	}
	if _, err := b.Open(pinnedName); !errors.Is(err, syscall.EIO) || !errors.Is(err, errMountEnded) {
		t.Errorf("Open after the mount ended = %v, want an I/O error", err)
	}
}

// touch opens a file and closes it again without reading, which leaves it kept open.
func touch(t *testing.T, b *billyFS, name string) {
	t.Helper()
	opened, err := b.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedMountEndingDuringAnOpenClosesTheFileAfterItsRead(t *testing.T) {
	t.Parallel()
	b, fsys, _ := newPinnedTest(t)
	gate := make(chan struct{})
	fsys.fail = func(op, _ string) error {
		if op == "open" {
			<-gate
		}
		return nil
	}
	var opened billy.File
	var openErr error
	var wg sync.WaitGroup
	wg.Go(func() { opened, openErr = b.Open(pinnedName) })
	waitUsers(t, b.pins, pinnedName, 1)
	b.close()
	close(gate)
	wg.Wait()
	if openErr != nil {
		t.Fatal(openErr)
	}
	if closes(fsys) != 0 {
		t.Fatal("the file was closed before its read ended")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if closes(fsys) != 1 {
		t.Errorf("closes = %d, want 1", closes(fsys))
	}
}

func TestPinnedFailedCloseIsLogged(t *testing.T) {
	t.Parallel()
	fsys := treeFS()
	var logged bytes.Buffer
	b := newBillyFS(t.Context(), fsys, time.Minute, testOwner(), slog.New(slog.NewTextHandler(&logged, nil)))
	clock := &fakeClock{}
	b.pins.after = clock.after
	readOnce(t, b)
	fsys.mu.Lock()
	fsys.fail = func(op, _ string) error {
		if op == "close" {
			return errors.New("disk on fire")
		}
		return nil
	}
	fsys.mu.Unlock()
	clock.quiet()
	if out := logged.String(); !strings.Contains(out, "close of a file kept open failed") ||
		!strings.Contains(out, pinnedName) || !strings.Contains(out, "disk on fire") {
		t.Errorf("the failed close was not logged: %q", out)
	}
}

// The tests below go through the NFS protocol, with the clock of the test in place of
// time.

func startPinnedProtocolTest(t *testing.T, fsys *fakeFS) (*server, *nfs.Target) {
	t.Helper()
	srv := startTest(t, fsys)
	clock := &fakeClock{}
	// The server already runs, so the clock is set under the lock the server reads it under.
	srv.view.pins.mu.Lock()
	srv.view.pins.after = clock.after
	srv.view.pins.mu.Unlock()
	return srv, dial(t, srv)
}

func TestProtocolReadsOfOneFileUseOneOpenUntilTheMountEnds(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile(pinnedName, []byte("hello world"))
	srv, target := startPinnedProtocolTest(t, fsys)
	opened, err := target.Open("/" + pinnedName)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	for _, off := range []int64{0, 1, 0} {
		if n, err := opened.ReadAt(buf, off); n != 5 || err != nil {
			t.Fatalf("ReadAt(%d) = %d, %v", off, n, err)
		}
	}
	if opens(fsys) != 1 || closes(fsys) != 0 {
		t.Fatalf("opens = %d, closes = %d; want one open, still open", opens(fsys), closes(fsys))
	}
	srv.Close()
	if closes(fsys) != 1 {
		t.Errorf("closes = %d after the mount ended, want 1", closes(fsys))
	}
}

// This test uses core.ErrUnavailable, which is what the core returns for a failed read
// today; the error for a file that changed on the servers is not on main yet.
func TestProtocolFailedReadIsAnIOErrorAndTheNextReadOpensAgain(t *testing.T) {
	t.Parallel()
	fsys := newFakeFS().addFile(pinnedName, []byte("hello world"))
	_, target := startPinnedProtocolTest(t, fsys)
	opened, err := target.Open("/" + pinnedName)
	if err != nil {
		t.Fatal(err)
	}
	fsys.mu.Lock()
	fsys.fail = func(op, _ string) error {
		if op == "read" {
			return core.ErrUnavailable
		}
		return nil
	}
	fsys.mu.Unlock()
	_, err = opened.ReadAt(make([]byte, 5), 0)
	wantStatus(t, "read", err, nfs.NFS3ErrIO)
	fsys.mu.Lock()
	fsys.fail = nil
	fsys.mu.Unlock()
	if n, err := opened.ReadAt(make([]byte, 5), 0); n != 5 || err != nil {
		t.Errorf("read after the failure = %d, %v", n, err)
	}
	if opens(fsys) != 2 {
		t.Errorf("opens = %d, want 2", opens(fsys))
	}
}
