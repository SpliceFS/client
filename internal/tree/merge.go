package tree

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/SpliceFS/common/wire"
)

// record is a merged entry together with the nodes that reported the name, in any
// kind, which is what decides whether it may be kept when some of them are silent.
type record struct {
	entry     Entry
	reporters []string // sorted, no duplicates
}

func (r record) public() Entry {
	e := r.entry
	e.Nodes = slices.Clone(e.Nodes)
	return e
}

func compareName(a, b record) int { return strings.Compare(a.entry.Name, b.entry.Name) }

// find looks a name up in records sorted by name.
func find(recs []record, name string) (record, bool) {
	i, ok := slices.BinarySearchFunc(recs, name, func(r record, name string) int {
		return strings.Compare(r.entry.Name, name)
	})
	if !ok {
		return record{}, false
	}
	return recs[i], true
}

type report struct {
	node  string
	entry wire.Entry
}

// merge combines the listings of the nodes that have the directory into one listing
// sorted by name.
func merge(have []reply) []record {
	byName := make(map[string][]report)
	for _, r := range have {
		for _, e := range r.entries {
			if !validSegment(e.Name) {
				continue
			}
			byName[e.Name] = append(byName[e.Name], report{node: r.node, entry: e})
		}
	}
	recs := make([]record, 0, len(byName))
	for name, reports := range byName {
		recs = append(recs, resolve(name, reports))
	}
	slices.SortFunc(recs, compareName)
	return recs
}

// resolve decides the kind of a name by majority, with a tie going to the directory
// because that keeps its subtree reachable, and then the version.
func resolve(name string, reports []report) record {
	var dirs, files []report
	nodes := make([]string, len(reports))
	for i, r := range reports {
		nodes[i] = r.node
		if r.entry.Dir {
			dirs = append(dirs, r)
		} else {
			files = append(files, r)
		}
	}
	rec := record{reporters: sortedUnique(nodes)}
	if len(dirs) >= len(files) {
		rec.entry = directory(name, dirs)
	} else {
		rec.entry = file(name, files)
	}
	return rec
}

func directory(name string, reports []report) Entry {
	newest := slices.MaxFunc(reports, func(a, b report) int { return a.entry.MTime.Compare(b.entry.MTime) })
	return Entry{Name: name, Dir: true, MTime: newest.entry.MTime}
}

type version struct {
	size  int64
	mtime int64
	etag  string
}

type candidate struct {
	entry wire.Entry
	nodes []string
}

// file picks the version that most nodes report. On a tie the newest modification
// time wins, then the greatest tag, then the largest size, so that the choice does
// not depend on the order in which nodes answered.
func file(name string, reports []report) Entry {
	groups := make(map[version]*candidate)
	for _, r := range reports {
		key := version{r.entry.Size, r.entry.MTime.UnixNano(), r.entry.ETag}
		c, ok := groups[key]
		if !ok {
			c = &candidate{entry: r.entry}
			groups[key] = c
		}
		c.nodes = append(c.nodes, r.node)
	}
	best := slices.MaxFunc(slices.Collect(maps.Values(groups)), func(a, b *candidate) int {
		return cmp.Or(
			cmp.Compare(len(a.nodes), len(b.nodes)),
			a.entry.MTime.Compare(b.entry.MTime),
			strings.Compare(a.entry.ETag, b.entry.ETag),
			cmp.Compare(a.entry.Size, b.entry.Size),
		)
	})
	return Entry{
		Name:  name,
		Size:  best.entry.Size,
		MTime: best.entry.MTime,
		ETag:  best.entry.ETag,
		Nodes: sortedUnique(best.nodes),
	}
}

// carry keeps what the silent nodes may still have. A name in prev that the fresh
// merge lacks stays while a node that reported it did not answer; a name that is in
// both remembers those silent reporters too, so that it is not lost when the nodes
// that answered drop it later.
func carry(prev, fresh []record, silentNodes map[string]bool) []record {
	var kept []record
	for _, earlier := range prev {
		held := heldBy(earlier.reporters, silentNodes)
		if len(held) == 0 {
			continue
		}
		i, ok := slices.BinarySearchFunc(fresh, earlier, compareName)
		if ok {
			fresh[i].reporters = sortedUnique(append(fresh[i].reporters, held...))
			continue
		}
		earlier.reporters = held
		kept = append(kept, earlier)
	}
	fresh = append(fresh, kept...)
	slices.SortFunc(fresh, compareName)
	return fresh
}

// heldBy returns the nodes among reporters that are silent.
func heldBy(reporters []string, silentNodes map[string]bool) []string {
	var held []string
	for _, n := range reporters {
		if silentNodes[n] {
			held = append(held, n)
		}
	}
	return held
}

func sortedUnique(nodes []string) []string {
	nodes = slices.Clone(nodes)
	slices.Sort(nodes)
	return slices.Compact(nodes)
}
