package nfs

import "sync"

// lruNode is one entry of an lru. The nodes form a ring around the sentinel in lru.head,
// so no node is ever without neighbours and no case for an empty end is needed. The list
// is written here, and not taken from container/list, because that one stores its values
// as any and every read would need a type assertion.
type lruNode[K comparable, V any] struct {
	key        K
	value      V
	prev, next *lruNode[K, V]
}

// lru is a bounded map that drops the least recently used entry when it is full. It is
// safe for concurrent use.
type lru[K comparable, V any] struct {
	mu    sync.Mutex
	limit int
	head  lruNode[K, V] // sentinel: head.next is the most recently used, head.prev the least
	items map[K]*lruNode[K, V]
}

func newLRU[K comparable, V any](limit int) *lru[K, V] {
	cache := &lru[K, V]{limit: limit, items: make(map[K]*lruNode[K, V])}
	cache.head.prev, cache.head.next = &cache.head, &cache.head
	return cache
}

// get returns the value for key and marks it as recently used.
//
//nolint:ireturn // V is the type of the cached value, which the caller chooses; it is not an interface.
func (c *lru[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	node, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.unlink(node)
	c.pushFront(node)
	return node.value, true
}

// add stores value under key and marks it as recently used, evicting the oldest entry
// when the limit is exceeded.
func (c *lru[K, V]) add(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if node, ok := c.items[key]; ok {
		node.value = value
		c.unlink(node)
		c.pushFront(node)
		return
	}
	node := &lruNode[K, V]{key: key, value: value}
	c.items[key] = node
	c.pushFront(node)
	if len(c.items) > c.limit {
		oldest := c.head.prev
		c.unlink(oldest)
		delete(c.items, oldest.key)
	}
}

// remove deletes key if present.
func (c *lru[K, V]) remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if node, ok := c.items[key]; ok {
		c.unlink(node)
		delete(c.items, key)
	}
}

func (*lru[K, V]) unlink(node *lruNode[K, V]) {
	node.prev.next = node.next
	node.next.prev = node.prev
}

func (c *lru[K, V]) pushFront(node *lruNode[K, V]) {
	node.prev = &c.head
	node.next = c.head.next
	c.head.next.prev = node
	c.head.next = node
}
