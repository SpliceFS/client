// Package tree is the merged, cached view of the directories of a cluster. A listing
// is the union of what every node reports; a node that does not answer never makes
// a name vanish from a listing that was already known.
package tree

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BlueRival/splicefs/internal/wire"
)

// Errors that callers branch on, checked with errors.Is.
var (
	// ErrNotFound means the name does not exist, or is not a valid name.
	ErrNotFound = errors.New("no such file or directory")
	// ErrNotDir means a directory was asked for but the name is a file.
	ErrNotDir = errors.New("not a directory")
	// ErrUnavailable means no node answered and nothing is remembered.
	ErrUnavailable = errors.New("no node answered")
)

const (
	defaultNegativeTTL = 5 * time.Second
	defaultMaxDirs     = 10000
	defaultNodeTimeout = 15 * time.Second
)

// Lister asks nodes for directories.
type Lister interface {
	// Nodes returns the names of the nodes to ask now.
	Nodes(ctx context.Context) ([]string, error)
	// List returns the entries of one directory on one node. dir "" is the root of
	// the cluster, whose entries are the shares.
	List(ctx context.Context, node, dir string) ([]wire.Entry, error)
}

// Options configures a Tree. Every zero value selects the default shown, except
// TTL, where zero means that a listing is never reused.
type Options struct {
	// TTL is how long a listing is reused.
	TTL time.Duration
	// NegativeTTL is how long "does not exist" is remembered; the default is 5s.
	NegativeTTL time.Duration
	// MaxDirs is the number of listings kept in memory, least recently used out
	// first; the default is 10000.
	MaxDirs int
	// NodeTimeout is the time limit for one node to list one directory; the
	// default is 15s.
	NodeTimeout time.Duration
	// Now is the clock; the default is time.Now.
	Now func() time.Time
	// IsNotFound reports whether an error from Lister.List means that the node
	// has no such directory, as opposed to not having answered.
	IsNotFound func(error) bool
}

// Entry is one name in a directory.
type Entry struct {
	Name  string
	Dir   bool
	Size  int64
	MTime time.Time
	ETag  string
	// Nodes is, for a file, the sorted names of the nodes that reported exactly
	// this version. It is empty for a directory.
	Nodes []string
}

// Tree merges and caches directory listings. It is safe for concurrent use.
type Tree struct {
	lister Lister
	opts   Options

	mu      sync.Mutex
	items   map[string]*item
	order   *list.List // of *item, most recently used first
	flights map[string]*flight
}

// New returns a Tree that asks lister.
func New(lister Lister, opts Options) *Tree {
	if opts.NegativeTTL <= 0 {
		opts.NegativeTTL = defaultNegativeTTL
	}
	if opts.MaxDirs <= 0 {
		opts.MaxDirs = defaultMaxDirs
	}
	if opts.NodeTimeout <= 0 {
		opts.NodeTimeout = defaultNodeTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IsNotFound == nil {
		opts.IsNotFound = func(error) bool { return false }
	}
	return &Tree{
		lister:  lister,
		opts:    opts,
		items:   make(map[string]*item),
		order:   list.New(),
		flights: make(map[string]*flight),
	}
}

// List returns the merged entries of dir, sorted by name. "" is the root. Callers
// that arrive while a listing of the same directory is being fetched share it, and
// one whose context ends stops waiting without cancelling it for the others.
func (t *Tree) List(ctx context.Context, dir string) ([]Entry, error) {
	recs, err := t.records(ctx, dir)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, len(recs))
	for i, r := range recs {
		entries[i] = r.public()
	}
	return entries, nil
}

// Stat returns the entry for name, found in the listing of its parent directory.
// "" is the root, a directory, and is answered without asking anyone.
func (t *Tree) Stat(ctx context.Context, name string) (Entry, error) {
	if name == "" {
		return Entry{Dir: true}, nil
	}
	if !validName(name) {
		return Entry{}, notFound(name)
	}
	parent, base := cut(name)
	recs, err := t.records(ctx, parent)
	if err != nil {
		return Entry{}, err
	}
	r, ok := find(recs, base)
	if !ok {
		return Entry{}, notFound(name)
	}
	return r.public(), nil
}

// Forget drops the remembered listing of dir, whether it exists or not.
func (t *Tree) Forget(dir string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if it, ok := t.items[dir]; ok {
		t.drop(it)
	}
}

// records returns the listing of dir; the result is shared with the cache and must
// not be modified.
func (t *Tree) records(ctx context.Context, dir string) ([]record, error) {
	if dir != "" && !validName(dir) {
		return nil, notFound(dir)
	}
	if t.isFile(dir) {
		return nil, fmt.Errorf("%q: %w", dir, ErrNotDir)
	}
	return t.acquire(ctx, dir).wait(ctx, dir)
}

// isFile reports whether the parent's listing, if it is in memory and fresh, shows
// dir as a file. A stale listing is not trusted: a file may since have been replaced
// by a directory of the same name.
func (t *Tree) isFile(dir string) bool {
	if dir == "" {
		return false
	}
	parent, base := cut(dir)
	t.mu.Lock()
	defer t.mu.Unlock()
	it, ok := t.items[parent]
	if !ok || it.missing || !t.fresh(it) {
		return false
	}
	r, ok := find(it.recs, base)
	return ok && !r.entry.Dir
}

func notFound(name string) error {
	return fmt.Errorf("%q: %w", name, ErrNotFound)
}

// validName reports whether name is a non-empty path of non-empty segments, none of
// them "." or "..", with no leading or trailing slash.
func validName(name string) bool {
	for seg := range strings.SplitSeq(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// cut splits a valid, non-empty name into its parent directory and its last segment.
func cut(name string) (parent, base string) {
	i := strings.LastIndexByte(name, '/')
	return name[:max(i, 0)], name[i+1:]
}
