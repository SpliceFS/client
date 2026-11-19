package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The cluster scenarios in which the nodes change while the client runs: files that come
// and go between listings, and nodes that stop, come back or cut a reply in the middle of
// a read.

// cutAfter makes the node drop its reply for the file path, by "share/dir/name", once the
// bytes before offset after are sent.
func (n *fakeNode) cutAfter(path string, after int64) {
	shareName, name, _ := strings.Cut(path, "/")
	n.current(shareName).Cut(name, after)
}

// firstHeld returns the index of the node whose gate a request reached first.
func (fc *fakeCluster) firstHeld() int {
	cases := make([]reflect.SelectCase, len(fc.nodes))
	for i, n := range fc.nodes {
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(n.waits)}
	}
	chosen, _, _ := reflect.Select(cases)
	return chosen
}

// stopHeld stops a node whose requests wait at its gate. Stop waits for the requests in
// flight, so the gate opens once the client has seen its request fail, which is after
// the node dropped the connection.
func stopHeld(n *fakeNode, release func()) {
	stopped := make(chan struct{})
	go func() {
		n.stop()
		close(stopped)
	}()
	<-n.failed
	release()
	<-stopped
}

// readFrom reads f from offset to the end.
func readFrom(f File, offset int64) ([]byte, error) {
	var out []byte
	buf := make([]byte, 20000)
	for {
		n, err := f.ReadAt(context.Background(), buf, offset+int64(len(out)))
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

type readResult struct {
	data []byte
	err  error
}

// readWithChange reads name: its first block, then the rest while every node holds its
// requests. change alters the node that a request of the rest reached first, while that
// request waits, so the change always meets a read in flight; release opens that node's
// gate, the other gates open after change.
func (h *harness) readWithChange(name string, change func(n *fakeNode, release func())) []byte {
	h.t.Helper()
	opened, err := h.view(0).Open(context.Background(), name)
	if err != nil {
		h.t.Fatalf("Open %q: %v", name, err)
	}
	defer closeFile(h.t, opened)
	first := make([]byte, testBlockSize)
	if n, err := opened.ReadAt(context.Background(), first, 0); n != len(first) {
		h.t.Fatalf("ReadAt 0: %d bytes, %v", n, err)
	}
	releases := make([]func(), len(h.fc.nodes))
	for i, n := range h.fc.nodes {
		releases[i] = n.hold(h.t)
	}
	done := make(chan readResult, 1)
	go func() {
		rest, err := readFrom(opened, testBlockSize)
		done <- readResult{data: rest, err: err}
	}()
	held := h.fc.firstHeld()
	change(h.fc.nodes[held], releases[held])
	for _, release := range releases {
		release()
	}
	got := <-done
	if got.err != nil {
		h.t.Fatalf("ReadAt after the change: %v", got.err)
	}
	return slices.Concat(first, got.data)
}

func TestReadInFlightScenarios(t *testing.T) {
	t.Parallel()
	original := copyOf(1, 16*testBlockSize, timeMid())
	other := copyOf(2, 16*testBlockSize, timeNew())
	for _, tt := range []struct {
		name   string
		change func(t *testing.T, n *fakeNode, release func())
	}{
		{"a server stops in the middle of a read", func(_ *testing.T, n *fakeNode, release func()) {
			stopHeld(n, release)
		}},
		// The other nodes still hold their requests, so the read is not done before the
		// node is back.
		{"a server stops and comes back in the middle of a read", func(t *testing.T, n *fakeNode, release func()) {
			t.Helper()
			stopHeld(n, release)
			if err := n.node.Restart(); err != nil {
				t.Fatal(err)
			}
		}},
		{"a reply is cut in the middle of a read", func(_ *testing.T, n *fakeNode, _ func()) {
			n.cutAfter("media/big.bin", testBlockSize+testBlockSize/2)
		}},
		{"a file changes on one server in the middle of a read", func(_ *testing.T, n *fakeNode, _ func()) {
			n.put("media/big.bin", other.data, other.mtime)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newHarness(t, "a", "b", "c")
			env.fc.put("media/big.bin", original.data, original.mtime)
			got := env.readWithChange("media/big.bin", func(n *fakeNode, release func()) {
				tt.change(t, n, release)
			})
			if !bytes.Equal(got, original.data) {
				t.Error("the read is not the copy it started with")
			}
		})
	}
}

// relist lets the listings age past their time to live, so the next request lists again.
func (h *harness) relist() { h.clock.Advance(testCacheTTL + time.Second) }

func (h *harness) expectNames(fsys FS, want ...string) {
	h.t.Helper()
	if files := walkShare(h.t, fsys).files; !slices.Equal(files, want) {
		h.t.Errorf("files listed = %q, want %q", files, want)
	}
}

func (h *harness) expectContent(fsys FS, name string, want fakeFile) {
	h.t.Helper()
	if got := readFile(h.t, fsys, scenarioShare+"/"+name); !bytes.Equal(got, want.data) {
		h.t.Errorf("%s reads as other bytes (%d read, want %d)", name, len(got), len(want.data))
	}
}

func TestFileAddedWhileRunning(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	base, added := copyOf(1, 40, timeMid()), copyOf(2, 50, timeMid())
	env.fc.nodes[0].put("media/base.txt", base.data, base.mtime)
	fsys := env.view(0)
	env.expectNames(fsys, "base.txt")
	env.fc.nodes[1].put("media/new.txt", added.data, added.mtime)
	env.relist()
	env.expectNames(fsys, "base.txt", "new.txt")
	env.expectContent(fsys, "new.txt", added)
}

// A file one node loses is still listed and read from the other: read.bin was read before
// the loss, fresh.bin never was.
func TestFileRemovedFromOneWhileRunning(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	read, fresh := copyOf(1, 100<<10, timeOld()), copyOf(2, 100<<10, timeOld())
	env.fc.put("media/read.bin", read.data, read.mtime)
	env.fc.put("media/fresh.bin", fresh.data, fresh.mtime)
	fsys := env.view(0)
	env.expectContent(fsys, "read.bin", read)
	env.fc.nodes[0].remove("media/read.bin")
	env.fc.nodes[0].remove("media/fresh.bin")
	env.relist()
	env.expectNames(fsys, "fresh.bin", "read.bin")
	env.expectContent(fsys, "read.bin", read)
	env.expectContent(fsys, "fresh.bin", fresh)
}

// A file no node has any more is gone, although the cache holds its bytes.
func TestFileRemovedFromAllWhileRunning(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	gone, keep := copyOf(1, 100<<10, timeOld()), copyOf(2, 30, timeMid())
	env.fc.put("media/gone.bin", gone.data, gone.mtime)
	env.fc.put("media/keep.txt", keep.data, keep.mtime)
	fsys := env.view(0)
	env.expectContent(fsys, "gone.bin", gone)
	for _, n := range env.fc.nodes {
		n.remove("media/gone.bin")
	}
	env.relist()
	if _, err := fsys.Stat(context.Background(), "media/gone.bin"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat of the removed file = %v, want ErrNotExist", err)
	}
	if _, err := fsys.Open(context.Background(), "media/gone.bin"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Open of the removed file = %v, want ErrNotExist", err)
	}
	env.expectNames(fsys, "keep.txt")
}

// A file replaced on every node by a copy of another size and a newer time shows the new
// size and the new bytes, not the cached old ones.
func TestFileReplacedWhileRunning(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	old, replaced := copyOf(1, 100<<10, timeOld()), copyOf(2, 150<<10, timeNew())
	env.fc.put("media/doc.bin", old.data, old.mtime)
	fsys := env.view(0)
	env.expectContent(fsys, "doc.bin", old)
	env.fc.put("media/doc.bin", replaced.data, replaced.mtime)
	env.relist()
	attr, err := fsys.Stat(context.Background(), "media/doc.bin")
	if err != nil || attr.Size != int64(len(replaced.data)) {
		t.Errorf("Stat = %+v, %v; want the size of the new copy", attr, err)
	}
	env.expectContent(fsys, "doc.bin", replaced)
}

// With the entry host down at the start the client comes up, listings fail, and they work
// once the host is back.
func TestEntryHostDownAtStart(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	one, two := copyOf(1, 65536, timeMid()), copyOf(2, 65536, timeMid())
	env.fc.nodes[0].put("media/one.bin", one.data, one.mtime)
	env.fc.nodes[1].put("media/two.bin", two.data, two.mtime)
	env.fc.nodes[0].stop()
	fsys := env.view(0)
	if _, err := fsys.ReadDir(context.Background(), scenarioShare); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ReadDir with the entry host down = %v, want ErrUnavailable", err)
	}
	if err := env.fc.nodes[0].node.Restart(); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(time.Minute)
	env.expectNames(fsys, "one.bin", "two.bin")
	env.expectContent(fsys, "one.bin", one)
	env.expectContent(fsys, "two.bin", two)
}

// The entry host is needed to log in, not to go on: after the start the other node serves,
// also a file never read before.
func TestEntryHostStopsAfterStart(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	first, second := copyOf(1, 256<<10, timeMid()), copyOf(2, 256<<10, timeMid())
	env.fc.put("media/first.bin", first.data, first.mtime)
	env.fc.put("media/second.bin", second.data, second.mtime)
	fsys := env.view(0)
	env.expectContent(fsys, "first.bin", first)
	env.fc.nodes[0].stop()
	env.expectContent(fsys, "second.bin", second)
}

// With every node down a file wholly in the cache is still read, and a file that is not
// fails as unavailable instead of hanging.
func TestEveryServerDownWithCache(t *testing.T) {
	t.Parallel()
	env := newHarness(t, "a", "b")
	cached, never := copyOf(1, 3*testBlockSize, timeMid()), copyOf(2, 3*testBlockSize, timeMid())
	env.fc.put("media/cached.bin", cached.data, cached.mtime)
	env.fc.put("media/never.bin", never.data, never.mtime)
	fsys := env.view(0)
	env.expectContent(fsys, "cached.bin", cached)
	if _, err := fsys.Stat(context.Background(), "media/never.bin"); err != nil {
		t.Fatal(err)
	}
	env.fc.setAllDown()
	env.expectContent(fsys, "cached.bin", cached)
	opened, err := fsys.Open(context.Background(), "media/never.bin")
	if err == nil {
		_, err = readFrom(opened, 0)
		closeFile(t, opened)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("reading an uncached file with every node down = %v, want ErrUnavailable", err)
	}
}
