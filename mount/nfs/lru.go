package nfs

import (
	"container/list"
	"sync"
)

// lru is a bounded map that drops the least recently used entry when it is full. It is
// safe for concurrent use.
type lru[K comparable, V any] struct {
	mu    sync.Mutex
	limit int
	order *list.List // front is the most recently used
	items map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key   K
	value V
}

func newLRU[K comparable, V any](limit int) *lru[K, V] {
	return &lru[K, V]{limit: limit, order: list.New(), items: make(map[K]*list.Element)}
}

// get returns the value for key and marks it as recently used.
func (c *lru[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(lruEntry[K, V]).value, true
}

// add stores value under key and marks it as recently used, evicting the oldest entry
// when the limit is exceeded.
func (c *lru[K, V]) add(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = lruEntry[K, V]{key, value}
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(lruEntry[K, V]{key, value})
	if c.order.Len() > c.limit {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(lruEntry[K, V]).key)
	}
}

// remove deletes key if present.
func (c *lru[K, V]) remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}
