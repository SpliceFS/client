package core

import (
	"slices"
	"sync"

	"github.com/SpliceFS/client/internal/cache"
)

// maxHeld bounds the remembered versions. Forgetting one costs a single HEAD round at
// the next open of that file, so the oldest entry need not be tracked exactly.
const maxHeld = 10000

// heldVersions remembers, for each file, the version that a HEAD round chose last and
// the nodes that held it. A recent cache entry lets an open skip HEAD, but the tree
// only knows which nodes report the same tag; a node whose copy has that tag and other
// bytes is excluded only by the head digest, which this record keeps.
type heldVersions struct {
	mu     sync.Mutex
	byFile map[cache.Key]held
}

type held struct {
	version cache.Version
	nodes   []string
}

func (h *heldVersions) remember(key cache.Key, v cache.Version, nodes []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byFile == nil {
		h.byFile = make(map[cache.Key]held)
	}
	if _, ok := h.byFile[key]; !ok && len(h.byFile) >= maxHeld {
		for k := range h.byFile {
			delete(h.byFile, k)
			break
		}
	}
	h.byFile[key] = held{version: v, nodes: slices.Clone(nodes)}
}

// recall returns the nodes that held version v of the file, if that is the version
// remembered.
func (h *heldVersions) recall(key cache.Key, v cache.Version) ([]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.byFile[key]
	if !ok || r.version != v {
		return nil, false
	}
	return slices.Clone(r.nodes), true
}
