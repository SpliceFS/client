package tree

import (
	"container/list"
	"time"
)

// item is the remembered answer for one directory: its listing, or the fact that it
// does not exist. It is never modified after it is stored.
type item struct {
	dir     string
	recs    []record
	missing bool
	at      time.Time
	elem    *list.Element
}

// fresh reports whether the item may be answered from memory. The caller holds t.mu.
func (t *Tree) fresh(cached *item) bool {
	ttl := t.opts.TTL
	if cached.missing {
		ttl = t.opts.NegativeTTL
	}
	return t.opts.Now().Sub(cached.at) < ttl
}

// previous returns the listing in memory for dir, however old.
func (t *Tree) previous(dir string) ([]record, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cached, ok := t.items[dir]
	if !ok || cached.missing {
		return nil, false
	}
	return cached.recs, true
}

// store remembers the answer for dir, evicting the least recently used directories
// beyond MaxDirs.
func (t *Tree) store(dir string, recs []record, missing bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cached, ok := t.items[dir]
	if ok {
		t.order.MoveToFront(cached.elem)
	} else {
		cached = &item{dir: dir}
		cached.elem = t.order.PushFront(cached)
		t.items[dir] = cached
	}
	cached.recs, cached.missing, cached.at = recs, missing, t.opts.Now()
	for t.order.Len() > t.opts.MaxDirs {
		t.evictOldest()
	}
}

// drop removes an item. The caller holds t.mu.
func (t *Tree) drop(cached *item) {
	t.order.Remove(cached.elem)
	delete(t.items, cached.dir)
}

// evictOldest removes the least recently used item. The caller holds t.mu. Only items
// are ever put in the order list; anything else is taken out of it all the same, so
// that the caller's loop always ends.
func (t *Tree) evictOldest() {
	oldest := t.order.Back()
	cached, ok := oldest.Value.(*item)
	if !ok {
		t.order.Remove(oldest)
		return
	}
	t.drop(cached)
}
