//go:build linux

package fuse

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"path"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SpliceFS/client/core"
)

const (
	bigSize     = 8<<20 + 123 // not a multiple of any block size
	fakeTotal   = 1000 * blockSize
	fakeFree    = 250 * blockSize
	fakeContent = "hello, world\n"
)

var fakeMTime = time.Date(2025, 3, 4, 5, 6, 7, 891_000_000, time.UTC)

// fakeFS is a core.FS over an in-memory tree whose behavior a test can steer.
type fakeFS struct {
	files map[string][]byte
	dirs  map[string]bool

	statCalls atomic.Int64
	readCalls atomic.Int64
	open      atomic.Int64 // files opened and not yet closed

	mu         sync.Mutex
	failAll    error
	failPaths  map[string]error
	space      core.Space
	spaceErr   error
	blocked    string        // reads of this path wait for cancellation
	reading    chan struct{} // receives once per blocked read that has started
	cancelled  chan error    // receives the context error of each cancelled blocked read
	readFault  *readFault
	closeError error
}

// readFault makes reads return n bytes together with err.
type readFault struct {
	n   int
	err error
}

func newFakeFS() *fakeFS {
	big := make([]byte, bigSize)
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i+8 <= len(big); i += 8 {
		binary.LittleEndian.PutUint64(big[i:], r.Uint64())
	}
	return &fakeFS{
		files: map[string][]byte{
			"hello.txt":         []byte(fakeContent),
			"big.bin":           big,
			"empty.txt":         {},
			"docs/readme.md":    []byte("# docs\n"),
			"docs/sub/deep.txt": []byte("deep\n"),
		},
		dirs:      map[string]bool{"": true, "docs": true, "docs/sub": true, "vacant": true},
		failPaths: map[string]error{},
		space:     core.Space{Total: fakeTotal, Free: fakeFree},
		reading:   make(chan struct{}, 16),
		cancelled: make(chan error, 16),
	}
}

// fakeInode is stable across instances, as the core's inode numbers are across restarts.
func fakeInode(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return h.Sum64() | 1<<40
}

func (f *fakeFS) setFailAll(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAll = err
}

func (f *fakeFS) setFailPath(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPaths[name] = err
}

func (f *fakeFS) setSpace(s core.Space, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.space, f.spaceErr = s, err
}

func (f *fakeFS) blockReadsOf(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = name
}

func (f *fakeFS) setReadFault(rf *readFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFault = rf
}

func (f *fakeFS) setCloseError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeError = err
}

// failure returns the error scripted for name, if any.
func (f *fakeFS) failure(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return f.failAll
	}
	return f.failPaths[name]
}

func (f *fakeFS) attr(name string) (core.Attr, error) {
	a := core.Attr{Name: path.Base(name), MTime: fakeMTime, Inode: fakeInode(name)}
	switch data, isFile := f.files[name]; {
	case isFile:
		a.Size = int64(len(data))
	case f.dirs[name]:
		a.Dir = true
	default:
		return core.Attr{}, fmt.Errorf("fake: %q: %w", name, core.ErrNotExist)
	}
	if name == "" {
		a.Name = ""
	}
	return a, nil
}

func (f *fakeFS) Stat(_ context.Context, name string) (core.Attr, error) {
	f.statCalls.Add(1)
	if err := f.failure(name); err != nil {
		return core.Attr{}, err
	}
	return f.attr(name)
}

func (f *fakeFS) ReadDir(_ context.Context, name string) ([]core.Attr, error) {
	if err := f.failure(name); err != nil {
		return nil, err
	}
	if _, isFile := f.files[name]; isFile {
		return nil, fmt.Errorf("fake: %q: %w", name, core.ErrNotDir)
	}
	if !f.dirs[name] {
		return nil, fmt.Errorf("fake: %q: %w", name, core.ErrNotExist)
	}
	var out []core.Attr
	for _, child := range f.children(name) {
		a, err := f.attr(child)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// children lists the full paths directly below dir, sorted by name.
func (f *fakeFS) children(dir string) []string {
	var out []string
	for p := range f.files {
		if parentOf(p) == dir {
			out = append(out, p)
		}
	}
	for p := range f.dirs {
		if p != "" && parentOf(p) == dir {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func parentOf(p string) string {
	if dir := path.Dir(p); dir != "." {
		return dir
	}
	return ""
}

func (f *fakeFS) Open(_ context.Context, name string) (core.File, error) {
	if err := f.failure(name); err != nil {
		return nil, err
	}
	if f.dirs[name] {
		return nil, fmt.Errorf("fake: %q: %w", name, core.ErrIsDir)
	}
	data, ok := f.files[name]
	if !ok {
		return nil, fmt.Errorf("fake: %q: %w", name, core.ErrNotExist)
	}
	f.open.Add(1)
	a, err := f.attr(name)
	if err != nil {
		return nil, err
	}
	return &fakeFile{fs: f, name: name, data: data, attr: a}, nil
}

func (f *fakeFS) StatFS(context.Context) (core.Space, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return core.Space{}, f.failAll
	}
	return f.space, f.spaceErr
}

type fakeFile struct {
	fs   *fakeFS
	name string
	data []byte
	attr core.Attr
}

func (ff *fakeFile) Attr() core.Attr { return ff.attr }

func (ff *fakeFile) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	ff.fs.readCalls.Add(1)
	ff.fs.mu.Lock()
	blocked, fault := ff.fs.blocked, ff.fs.readFault
	ff.fs.mu.Unlock()
	if blocked == ff.name {
		ff.fs.reading <- struct{}{}
		<-ctx.Done()
		ff.fs.cancelled <- ctx.Err()
		return 0, ctx.Err()
	}
	if fault != nil {
		return copy(p[:min(fault.n, len(p))], ff.data[off:]), fault.err
	}
	if off >= int64(len(ff.data)) {
		return 0, io.EOF
	}
	n := copy(p, ff.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (ff *fakeFile) Close() error {
	ff.fs.open.Add(-1)
	ff.fs.mu.Lock()
	defer ff.fs.mu.Unlock()
	return ff.fs.closeError
}
