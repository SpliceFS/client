package nfs

import (
	"context"
	"errors"
	"io"
	"net"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BlueRival/splicefs/client/core"
)

var fakeMTime = time.Unix(1_700_000_000, 123_000_000)

type fakeNode struct {
	attr core.Attr
	data []byte
}

// fakeFS is an in-memory core.FS. fail, when set, is asked before every call and may
// return an error for it. calls records every call as "op:name".
type fakeFS struct {
	mu        sync.Mutex
	nodes     map[string]*fakeNode
	nextInode uint64
	space     core.Space
	fail      func(op, name string) error
	calls     []string
}

func newFakeFS() *fakeFS {
	f := &fakeFS{nodes: map[string]*fakeNode{}, nextInode: 100, space: core.Space{Total: 1000, Free: 400}}
	f.nodes[""] = &fakeNode{attr: core.Attr{Dir: true, MTime: fakeMTime, Inode: 1}}
	return f
}

func (f *fakeFS) addDir(name string) *fakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureDir(path.Dir(name))
	f.ensureDir(name)
	return f
}

func (f *fakeFS) addFile(name string, data []byte) *fakeFS {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureDir(path.Dir(name))
	f.nextInode++
	f.nodes[name] = &fakeNode{
		attr: core.Attr{Name: path.Base(name), Size: int64(len(data)), MTime: fakeMTime, Inode: f.nextInode},
		data: data,
	}
	return f
}

func (f *fakeFS) ensureDir(name string) {
	if name == "." || name == "" {
		return
	}
	if _, ok := f.nodes[name]; ok {
		return
	}
	f.ensureDir(path.Dir(name))
	f.nextInode++
	f.nodes[name] = &fakeNode{attr: core.Attr{Name: path.Base(name), Dir: true, MTime: fakeMTime, Inode: f.nextInode}}
}

func (f *fakeFS) record(op, name string) error {
	f.mu.Lock()
	f.calls = append(f.calls, op+":"+name)
	fail := f.fail
	f.mu.Unlock()
	if fail == nil {
		return nil
	}
	return fail(op, name)
}

// listings returns the directories that were listed, in order.
func (f *fakeFS) listings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if rest, ok := strings.CutPrefix(c, "readdir:"); ok {
			out = append(out, rest)
		}
	}
	return out
}

func (f *fakeFS) node(name string) (*fakeNode, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[name]
	return n, ok
}

func (f *fakeFS) Stat(_ context.Context, name string) (core.Attr, error) {
	if err := f.record("stat", name); err != nil {
		return core.Attr{}, err
	}
	n, ok := f.node(name)
	if !ok {
		return core.Attr{}, core.ErrNotExist
	}
	return n.attr, nil
}

func (f *fakeFS) ReadDir(_ context.Context, name string) ([]core.Attr, error) {
	if err := f.record("readdir", name); err != nil {
		return nil, err
	}
	n, ok := f.node(name)
	if !ok {
		return nil, core.ErrNotExist
	}
	if !n.attr.Dir {
		return nil, core.ErrNotDir
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Attr
	for p, c := range f.nodes {
		if p != "" && path.Dir(p) == childDir(name) {
			out = append(out, c.attr)
		}
	}
	slices.SortFunc(out, func(a, b core.Attr) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// childDir is the value path.Dir returns for the children of the directory name.
func childDir(name string) string {
	if name == "" {
		return "."
	}
	return name
}

func (f *fakeFS) Open(_ context.Context, name string) (core.File, error) {
	if err := f.record("open", name); err != nil {
		return nil, err
	}
	n, ok := f.node(name)
	if !ok {
		return nil, core.ErrNotExist
	}
	if n.attr.Dir {
		return nil, core.ErrIsDir
	}
	return &fakeFile{fs: f, name: name, node: n}, nil
}

func (f *fakeFS) StatFS(context.Context) (core.Space, error) {
	if err := f.record("statfs", ""); err != nil {
		return core.Space{}, err
	}
	return f.space, nil
}

type fakeFile struct {
	fs     *fakeFS
	name   string
	node   *fakeNode
	closed bool
}

func (f *fakeFile) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	if err := f.fs.record("read", f.name); err != nil {
		return 0, err
	}
	if off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fakeFile) Attr() core.Attr { return f.node.attr }

func (f *fakeFile) Close() error {
	f.closed = true
	return f.fs.record("close", f.name)
}

// dialTCP connects to addr.
func dialTCP(t *testing.T, addr string) (net.Conn, error) {
	t.Helper()
	var d net.Dialer
	return d.DialContext(t.Context(), "tcp", addr)
}

// wantClosed checks that ln no longer accepts connections. It asks the listener itself
// rather than dialing its port, because a closed port can be handed to another test
// within microseconds.
func wantClosed(t *testing.T, ln net.Listener) {
	t.Helper()
	tcp := ln.(*net.TCPListener)
	// A deadline in the past makes a listener that is still open time out at once
	// instead of blocking in Accept.
	_ = tcp.SetDeadline(time.Unix(1, 0))
	if _, err := tcp.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("the listener on %v is still open: %v", ln.Addr(), err)
	}
}

// closeAfter closes c when the test ends.
func closeAfter(t *testing.T, c io.Closer) {
	t.Helper()
	t.Cleanup(func() { _ = c.Close() })
}
