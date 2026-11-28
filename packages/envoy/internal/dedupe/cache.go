package dedupe

import (
	"sync"
	"time"
)

// Cache is one process's record of the (dedupe key, session) pairs it has confirmed, kept for a
// TTL. It is not the delivery ledger: the ledger is the machine's record of what every lane
// delivered or forwarded, and the listener's publish route keeps this one beside it, because only
// a forward the stream stored through the route may answer a later send of the same key 200 with
// nothing forwarded (cmd/listener/confirmed.go). It also keeps what that answer reads and the
// ledger does not hold: the stream sequence of the stored copy (Stored), which a repeat is answered
// with, and the forward under the request that made it (Forward), which an echo of that request is
// answered with. The route reads no other process's record for either. Thread-safe via
// sync.RWMutex.
type Cache struct {
	mu      sync.RWMutex
	entries map[Key]entry
	// forwards is, for each request key its recorder names (RecordForward), the newest stored
	// copy that request's delivery made: the session it went to and the copy's sequence.
	forwards map[string]stored
	ttl      time.Duration
	clock    func() time.Time
	stop     chan struct{}
}

// entry is one recorded delivery: when it was recorded, and the stream sequence of the copy it
// stored where the recorder named one (RecordStored), 0 where it named none.
type entry struct {
	at       time.Time
	sequence uint64
}

// stored is the newest stored copy one request's delivery made: the session it went to, when it
// was recorded, and the copy's stream sequence.
type stored struct {
	sessionID string
	at        time.Time
	sequence  uint64
}

// New makes a cache with the given TTL and starts the background sweep that drops expired entries.
func New(ttl time.Duration) *Cache {
	return newCache(ttl, time.Now)
}

// newCache is New on the clock clock, which this package's tests set.
func newCache(ttl time.Duration, clock func() time.Time) *Cache {
	c := &Cache{
		entries:  make(map[Key]entry),
		forwards: make(map[string]stored),
		ttl:      ttl,
		clock:    clock,
		stop:     make(chan struct{}),
	}
	go c.cleanup()
	return c
}

// Seen reports whether the pair was recorded within the TTL window. It changes nothing:
// RecordStored is the write, made once the delivery is confirmed.
func (c *Cache) Seen(dedupeKey, sessionID string) bool {
	_, ok := c.Stored(dedupeKey, sessionID)
	return ok
}

// Stored reports whether the (dedupeKey, sessionID) pair was recorded within the TTL window, and
// the stream sequence of the copy that delivery stored where RecordStored named one, else 0.
func (c *Cache) Stored(dedupeKey, sessionID string) (uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[Key{DedupeKey: dedupeKey, SessionID: sessionID}]
	if !ok || c.clock().Sub(e.at) > c.ttl {
		return 0, false
	}
	return e.sequence, true
}

// RecordStored marks a (dedupeKey, sessionID) pair as delivered by the copy the stream holds at
// sequence, which Stored hands back to a later reader of the pair.
func (c *Cache) RecordStored(dedupeKey, sessionID string, sequence uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[Key{DedupeKey: dedupeKey, SessionID: sessionID}] = entry{at: c.clock(), sequence: sequence}
}

// RecordForward records that the request request names, a key of its recorder's own that holds
// everything that makes two requests the same request, was delivered to sessionID by the copy the
// stream holds at sequence, which Forward hands back to a later reader of the request whichever
// session it went to.
func (c *Cache) RecordForward(request, sessionID string, sequence uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forwards[request] = stored{sessionID: sessionID, at: c.clock(), sequence: sequence}
}

// Forward reports the session the newest delivery of request recorded by RecordForward went to
// within the TTL window, and the stream sequence of the copy it stored.
func (c *Cache) Forward(request string) (sessionID string, sequence uint64, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.forwards[request]
	if !ok || c.clock().Sub(s.at) > c.ttl {
		return "", 0, false
	}
	return s.sessionID, s.sequence, true
}

// Clear removes a recorded pair, so Seen and Stored answer for it as for a pair never recorded.
func (c *Cache) Clear(dedupeKey, sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, Key{DedupeKey: dedupeKey, SessionID: sessionID})
}

// Stop ends the background sweep.
func (c *Cache) Stop() {
	close(c.stop)
}

func (c *Cache) cleanup() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			now := c.clock()
			for k, e := range c.entries {
				if now.Sub(e.at) > c.ttl {
					delete(c.entries, k)
				}
			}
			for k, s := range c.forwards {
				if now.Sub(s.at) > c.ttl {
					delete(c.forwards, k)
				}
			}
			c.mu.Unlock()
		case <-c.stop:
			return
		}
	}
}
