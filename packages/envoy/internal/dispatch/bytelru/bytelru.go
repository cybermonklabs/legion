// Package bytelru holds the byte-bounded least-recently-used cache Dispatch keeps immutable
// encodings in: the display trees a read is shown as (pmdoc) and the dashboard assets another
// build's tabs ask for (routes). Both key by content - a hash of the markdown, a hashed build
// file name - so an entry never goes stale and only ever leaves to make room for another.
//
// The bound is on what each entry is charged, and the caller says what that is: Put charges a
// value its own bytes, which is the whole of an entry whose key is a hash and whose value is the
// slice it was made as, and PutSize charges whatever the caller counts - the spare capacity of a
// buffer handed over, the bytes of a key a request spelled, a fixed cost for the entry itself
// (api's list answers). That charge is what each package's own bound promises its reader.
package bytelru

import (
	"container/list"
	"sync"
)

// entry is one kept value with the key that finds it, so eviction can drop the map's entry from
// the list's own element, and what the cache charged for it, so eviction gives back exactly what
// the entry added.
type entry[K comparable, V ~[]byte] struct {
	key   K
	value V
	size  int64
}

// Cache is the values kept by their key, least recently used first out, holding at most the
// limit it was made with. Every method takes the cache's lock, so one cache serves every
// goroutine of a process.
type Cache[K comparable, V ~[]byte] struct {
	limit int64

	mu    sync.Mutex
	held  int64
	items map[K]*list.Element
	order *list.List // of *entry[K, V], the most recently used at the front
}

// New is an empty cache charging at most limit bytes of entries.
func New[K comparable, V ~[]byte](limit int64) *Cache[K, V] {
	return &Cache[K, V]{limit: limit, items: make(map[K]*list.Element), order: list.New()}
}

// Get is the value kept under key, marked the most recently used. The value is shared, never
// copied: a caller must not write to it.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, held := c.items[key]
	if !held {
		var missing V
		return missing, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*entry[K, V]).value, true
}

// Put keeps value under key, charged the value's own bytes. A caller whose entry holds more than
// that - a key of its own bytes, a buffer with capacity past its length - charges it with
// PutSize.
func (c *Cache[K, V]) Put(key K, value V) {
	c.PutSize(key, value, int64(len(value)))
}

// PutSize keeps value under key charged size bytes, and drops the least recently used entries
// until what is charged fits the limit. size is what the entry really costs this process, which
// is never less than the value's own bytes and is often more. An entry charged more than the
// whole limit is never kept, so one enormous one cannot empty the cache for everybody else; that
// same bound leaves the list holding at least two entries whenever the eviction loop runs, so its
// back is always an entry. A key already held is only marked used again, since a caller keys by
// content and the value under one key never changes.
func (c *Cache[K, V]) PutSize(key K, value V, size int64) {
	// The limit bounds the bytes of the values whatever a caller passes, so a charge under the
	// value's own length is read as that length.
	size = max(size, int64(len(value)))
	if size > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, held := c.items[key]; held {
		c.order.MoveToFront(element)
		return
	}
	c.items[key] = c.order.PushFront(&entry[K, V]{key: key, value: value, size: size})
	c.held += size
	for c.held > c.limit {
		evicted := c.order.Remove(c.order.Back()).(*entry[K, V])
		delete(c.items, evicted.key)
		c.held -= evicted.size
	}
}

// Held is the bytes the cache's entries are charged now, which is what its limit bounds.
func (c *Cache[K, V]) Held() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}
