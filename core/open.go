package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SpliceFS/client/internal/cache"
	"github.com/SpliceFS/client/internal/fetch"
	"github.com/SpliceFS/client/internal/remote"
	"github.com/SpliceFS/client/internal/tree"
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
//
//nolint:ireturn // File is the interface that FS.Open returns.
func (v *view) Open(ctx context.Context, name string) (File, error) {
	const operation = "open"
	// Deferred, so that the entry this open pins is safe from the pass.
	defer v.cache.maybeEvict()
	treeEntry, err := v.lookup(ctx, operation, name)
	if err != nil {
		return nil, err
	}
	if treeEntry.Dir {
		return nil, fmt.Errorf("%s %q: %w", operation, name, ErrIsDir)
	}
	full := v.full(name)
	key := cache.Key{Cluster: v.c.name, Path: full}
	chosen, err := v.choose(ctx, key, treeEntry)
	if err != nil {
		return nil, v.openError(ctx, name, full, err)
	}
	var entry *cache.Entry
	if v.cacheable(treeEntry) {
		entry = v.acquire(key, chosen)
	}
	reader, err := v.client.newReader(fetch.Options{
		Size:      chosen.version.Size,
		BlockSize: chosen.version.BlockSize,
		Streams:   v.c.cfg.StreamsPerNode,
		ReadAhead: int64(v.c.cfg.ReadAhead),
		Clock:     v.client.clock,
	}, v.sources(full, chosen), storeOf(entry), v.c.meter)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%s %q: %w", operation, name, err), release(entry))
	}
	attr := v.attr(name, treeEntry)
	attr.Size, attr.MTime = chosen.version.Size, chosen.mtime
	return &file{attr: attr, reader: reader, entry: entry, cache: v.cache}, nil
}

// cacheable reports whether the mount caches a file of this size: a file below the
// mount's minimum is read through without touching the cache.
func (v *view) cacheable(treeEntry tree.Entry) bool {
	return treeEntry.Size >= int64(v.mount.Cache.MinFileSize)
}

// choose picks the version to open, asking the nodes unless the cache vouches for it.
func (v *view) choose(ctx context.Context, key cache.Key, treeEntry tree.Entry) (choice, error) {
	nodes, err := v.c.nodes(ctx)
	if err != nil {
		return choice{}, err
	}
	listed := holding(nodes.all, treeEntry.Nodes)
	info, cached := v.cachedVersion(key, treeEntry)
	if cached && v.client.opts.Now().Sub(info.Validated) < v.mount.Cache.Revalidate.Std() {
		// Without the nodes of the last HEAD round, as after a restart, the tree's
		// nodes could include one with the same tag and other bytes: ask instead.
		if heldNodes, ok := v.c.held.recall(key, info.Version); ok {
			return choice{version: info.Version, mtime: treeEntry.MTime, sources: holding(listed, heldNodes)}, nil
		}
	}
	chosen, err := v.ask(ctx, key, listed, treeEntry.ETag)
	if errors.Is(err, errNoAnswer) && ctx.Err() == nil && v.mount.Offline && cached && info.Complete {
		// With no node reachable the tree's entry is the best knowledge of the file
		// there is, and the complete cached copy is that version.
		return choice{version: info.Version, mtime: treeEntry.MTime}, nil
	}
	return chosen, err
}

// cachedVersion reports the cache entry of the file when it holds the version the
// tree lists.
func (v *view) cachedVersion(key cache.Key, treeEntry tree.Entry) (cache.Info, bool) {
	if !v.cacheable(treeEntry) {
		return cache.Info{}, false
	}
	info, ok := v.cache.Peek(key)
	return info, ok && info.Version.ETag == treeEntry.ETag
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
		return fmt.Errorf("open %q: %w", name, ctx.Err())
	}
	return fmt.Errorf("open %q: %w: %w", name, ErrUnavailable, err)
}

// acquire pins the cache entry of the chosen version. The cache is an optimization: a
// broken cache disk is logged and the file is read without it.
func (v *view) acquire(key cache.Key, chosen choice) *cache.Entry {
	entry, err := v.cache.Acquire(key, chosen.version)
	if err != nil {
		v.client.opts.Logger.Warn("cache unusable, reading without it",
			slog.String("cluster", key.Cluster), slog.String("path", key.Path), slog.Any("error", err))
		return nil
	}
	if chosen.validated {
		v.cache.MarkValidated(key)
	}
	return entry
}

func (v *view) sources(full string, chosen choice) []fetch.Source {
	out := make([]fetch.Source, len(chosen.sources))
	for i, n := range chosen.sources {
		out[i] = source{c: v.c, node: n, file: full, etag: chosen.version.ETag}
	}
	return out
}

// storeOf returns the entry as a Store, or an untyped nil when there is none: a nil
// *cache.Entry in the interface would look like a cache to the reader.
//
//nolint:ireturn // The interface is the point: only an interface value can be a true nil.
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
	if err := entry.Release(); err != nil {
		return fmt.Errorf("release cache entry: %w", err)
	}
	return nil
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
		meta, err := callNode(ctx, v.c, n, n.Head, key.Path)
		return headAnswer{node: n, meta: meta, err: err}
	})
	group, err := chooseVersion(answers, treeETag)
	if err != nil {
		return choice{}, err
	}
	version := cache.Version{
		Size:       group.meta.Size,
		ETag:       group.meta.ETag,
		HeadDigest: group.meta.HeadDigest,
		BlockSize:  int64(v.c.cfg.BlockSize),
	}
	v.c.held.remember(key, version, names(group.nodes))
	return choice{version: version, mtime: group.meta.MTime, sources: group.nodes, validated: true}, nil
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
	for _, answer := range answers {
		switch {
		case answer.err == nil:
			groupKey := versionKey{answer.meta.ETag, answer.meta.HeadDigest}
			group, ok := groups[groupKey]
			if !ok {
				group = &versionGroup{meta: answer.meta}
				groups[groupKey] = group
			}
			group.nodes = append(group.nodes, answer.node)
		case errors.Is(answer.err, remote.ErrNotFound):
			missing = true
		default:
			failures = append(failures, answer.err)
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
func (group *versionGroup) matches(etag string) int {
	if group.meta.ETag == etag {
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
	//nolint:wrapcheck // File.ReadAt has io.ReaderAt semantics: callers compare the error with io.EOF.
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
