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
func (t *Tree) fresh(it *item) bool {
	ttl := t.opts.TTL
	if it.missing {
		ttl = t.opts.NegativeTTL
	}
	return t.opts.Now().Sub(it.at) < ttl
}

// previous returns the listing in memory for dir, however old.
func (t *Tree) previous(dir string) ([]record, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	it, ok := t.items[dir]
	if !ok || it.missing {
		return nil, false
	}
	return it.recs, true
}

// store remembers the answer for dir, evicting the least recently used directories
// beyond MaxDirs.
func (t *Tree) store(dir string, recs []record, missing bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	it, ok := t.items[dir]
	if ok {
		t.order.MoveToFront(it.elem)
	} else {
		it = &item{dir: dir}
		it.elem = t.order.PushFront(it)
		t.items[dir] = it
	}
	it.recs, it.missing, it.at = recs, missing, t.opts.Now()
	for t.order.Len() > t.opts.MaxDirs {
		t.drop(t.order.Back().Value.(*item))
	}
}

// drop removes an item. The caller holds t.mu.
func (t *Tree) drop(it *item) {
	t.order.Remove(it.elem)
	delete(t.items, it.dir)
}
