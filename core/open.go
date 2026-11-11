package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BlueRival/splicefs/client/internal/cache"
	"github.com/BlueRival/splicefs/client/internal/fetch"
	"github.com/BlueRival/splicefs/client/internal/remote"
	"github.com/BlueRival/splicefs/client/internal/tree"
)

// Outcomes of a HEAD round that has no version to offer.
var (
	errMissing  = errors.New("every node that answered has no such file")
	errNoAnswer = errors.New("no node answered")
)

// choice is the version of a file picked at open and the nodes to read it from.
type choice struct {
	version   cache.Version
	mtime     time.Time
	sources   []*remote.Node
	validated bool // the nodes were asked just now
}

// Open opens the file at name. The version it reads is fixed at open: the cached one
// when it was confirmed recently enough, otherwise the one most nodes hold now.
func (v *view) Open(ctx context.Context, name string) (File, error) {
	const op = "open"
	// Deferred, so that the entry this open pins is safe from the pass.
	defer v.cache.maybeEvict()
	e, err := v.stat(ctx, op, name)
	if err != nil {
		return nil, err
	}
	if e.Dir {
		return nil, fmt.Errorf("%s %q: %w", op, name, ErrIsDir)
	}
	full := v.full(name)
	key := cache.Key{Cluster: v.c.name, Path: full}
	cacheable := e.Size >= int64(v.mount.Cache.MinFileSize)
	ch, err := v.choose(ctx, key, e, cacheable)
	if err != nil {
		return nil, v.openError(ctx, name, full, err)
	}
	entry := v.acquire(key, ch, cacheable)
	reader, err := v.client.newReader(fetch.Options{
		Size:      ch.version.Size,
		BlockSize: ch.version.BlockSize,
		Streams:   v.c.cfg.StreamsPerNode,
		ReadAhead: int64(v.c.cfg.ReadAhead),
		Clock:     v.client.clock,
	}, v.sources(full, ch), storeOf(entry), v.c.meter)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%s %q: %w", op, name, err), release(entry))
	}
	attr := v.attr(name, e)
	attr.Size, attr.MTime = ch.version.Size, ch.mtime
	return &file{attr: attr, reader: reader, entry: entry, cache: v.cache}, nil
}

// choose picks the version to open, asking the nodes unless the cache vouches for it.
func (v *view) choose(ctx context.Context, key cache.Key, e tree.Entry, cacheable bool) (choice, error) {
	all, _, err := v.c.nodes(ctx)
	if err != nil {
		return choice{}, err
	}
	listed := holding(all, e.Nodes)
	info, cached := v.cachedVersion(key, e, cacheable)
	if cached && v.client.opts.Now().Sub(info.Validated) < v.mount.Cache.Revalidate.Std() {
		// Without the nodes of the last HEAD round, as after a restart, the tree's
		// nodes could include one with the same tag and other bytes: ask instead.
		if nodes, ok := v.c.held.recall(key, info.Version); ok {
			return choice{version: info.Version, mtime: e.MTime, sources: holding(listed, nodes)}, nil
		}
	}
	ch, err := v.ask(ctx, key, listed, e.ETag)
	if errors.Is(err, errNoAnswer) && ctx.Err() == nil && v.mount.Offline && cached && info.Complete {
		// With no node reachable the tree's entry is the best knowledge of the file
		// there is, and the complete cached copy is that version.
		return choice{version: info.Version, mtime: e.MTime}, nil
	}
	return ch, err
}

// cachedVersion reports the cache entry of the file when it holds the version the
// tree lists.
func (v *view) cachedVersion(key cache.Key, e tree.Entry, cacheable bool) (cache.Info, bool) {
	if !cacheable {
		return cache.Info{}, false
	}
	info, ok := v.cache.Peek(key)
	return info, ok && info.Version.ETag == e.ETag
}

// openError translates the failure to choose a version. A file that every answering
// node has lost is gone: the parent listing that still shows it is dropped, so the
// next look-up does not offer it again.
func (v *view) openError(ctx context.Context, name, full string, err error) error {
	switch {
	case errors.Is(err, errMissing):
		v.c.tree.Forget(full[:max(strings.LastIndexByte(full, '/'), 0)])
		return notExist("open", name)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("open %q: %w: %w", name, ErrUnavailable, err)
}

// acquire pins the cache entry of the chosen version. The cache is an optimization: a
// broken cache disk is logged and the file is read without it.
func (v *view) acquire(key cache.Key, ch choice, cacheable bool) *cache.Entry {
	if !cacheable {
		return nil
	}
	entry, err := v.cache.Acquire(key, ch.version)
	if err != nil {
		v.client.opts.Logger.Warn("cache unusable, reading without it",
			"cluster", key.Cluster, "path", key.Path, "error", err)
		return nil
	}
	if ch.validated {
		v.cache.MarkValidated(key)
	}
	return entry
}

func (v *view) sources(full string, ch choice) []fetch.Source {
	out := make([]fetch.Source, len(ch.sources))
	for i, n := range ch.sources {
		out[i] = source{c: v.c, node: n, file: full, etag: ch.version.ETag}
	}
	return out
}

// storeOf returns the entry as a Store, or an untyped nil when there is none: a nil
// *cache.Entry in the interface would look like a cache to the reader.
func storeOf(entry *cache.Entry) fetch.Store {
	if entry == nil {
		return nil
	}
	return entry
}

func release(entry *cache.Entry) error {
	if entry == nil {
		return nil
	}
	return entry.Release()
}

// holding returns the nodes among all whose names are in names.
func holding(all []*remote.Node, names []string) []*remote.Node {
	var out []*remote.Node
	for _, n := range all {
		if slices.Contains(names, n.Name()) {
			out = append(out, n)
		}
	}
	return out
}

// headAnswer is one node's answer to HEAD.
type headAnswer struct {
	node *remote.Node
	meta remote.Meta
	err  error
}

// ask sends HEAD to the nodes at once, chooses the version from their answers and
// remembers which nodes hold it.
func (v *view) ask(ctx context.Context, key cache.Key, nodes []*remote.Node, treeETag string) (choice, error) {
	answers := fanOut(ctx, nodes, func(ctx context.Context, n *remote.Node) headAnswer {
		var meta remote.Meta
		err := v.c.session.Do(ctx, n, func(ctx context.Context, token string) (err error) {
			meta, err = n.Head(ctx, token, key.Path)
			return err
		})
		return headAnswer{node: n, meta: meta, err: err}
	})
	g, err := chooseVersion(answers, treeETag)
	if err != nil {
		return choice{}, err
	}
	version := cache.Version{
		Size:       g.meta.Size,
		ETag:       g.meta.ETag,
		HeadDigest: g.meta.HeadDigest,
		BlockSize:  int64(v.c.cfg.BlockSize),
	}
	v.c.held.remember(key, version, names(g.nodes))
	return choice{version: version, mtime: g.meta.MTime, sources: g.nodes, validated: true}, nil
}

// versionGroup is the nodes that hold one version of a file.
type versionGroup struct {
	meta  remote.Meta
	nodes []*remote.Node
}

type versionKey struct{ etag, digest string }

// chooseVersion groups the nodes by tag and head digest and returns the largest group.
// A tie goes to the tree's tag, which is what the listing showed, then to the greatest
// tag and digest, so that the choice does not depend on the order of the answers.
func chooseVersion(answers []headAnswer, treeETag string) (versionGroup, error) {
	groups := make(map[versionKey]*versionGroup)
	var missing bool
	var failures []error
	for _, a := range answers {
		switch {
		case a.err == nil:
			k := versionKey{a.meta.ETag, a.meta.HeadDigest}
			g, ok := groups[k]
			if !ok {
				g = &versionGroup{meta: a.meta}
				groups[k] = g
			}
			g.nodes = append(g.nodes, a.node)
		case errors.Is(a.err, remote.ErrNotFound):
			missing = true
		default:
			failures = append(failures, a.err)
		}
	}
	switch {
	case len(groups) > 0:
		return *slices.MaxFunc(slices.Collect(maps.Values(groups)), func(a, b *versionGroup) int {
			return cmp.Or(
				cmp.Compare(len(a.nodes), len(b.nodes)),
				cmp.Compare(a.matches(treeETag), b.matches(treeETag)),
				strings.Compare(a.meta.ETag, b.meta.ETag),
				strings.Compare(a.meta.HeadDigest, b.meta.HeadDigest),
			)
		}), nil
	case missing:
		return versionGroup{}, errMissing
	}
	return versionGroup{}, errors.Join(append([]error{errNoAnswer}, failures...)...)
}

// matches is 1 when the group holds the version with the given tag, else 0.
func (g *versionGroup) matches(etag string) int {
	if g.meta.ETag == etag {
		return 1
	}
	return 0
}

// file is an open file.
type file struct {
	attr   Attr
	reader *fetch.Reader
	entry  *cache.Entry
	cache  *cacheDir
	closed atomic.Bool
}

func (f *file) Attr() Attr { return f.attr }

func (f *file) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	n, err := f.reader.ReadAt(ctx, p, off)
	if errors.Is(err, fetch.ErrUnavailable) {
		return n, fmt.Errorf("read %q: %w: %w", f.attr.Name, ErrUnavailable, err)
	}
	return n, err
}

// Close stops the reader, which waits for its fetches to end, before the cache entry
// is released: the reader writes blocks into the entry until then. A due eviction
// pass runs afterwards, when the entry may be evicted too.
func (f *file) Close() error {
	if !f.closed.CompareAndSwap(false, true) {
		return fs.ErrClosed
	}
	err := errors.Join(f.reader.Close(), release(f.entry))
	f.cache.maybeEvict()
	return err
}
