package session

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// knownCache is the store as the cache its watcher feeds (kvwatch.Cache). The hooks are its own
// type's, not the store's methods, so nothing but the watcher calls them.
type knownCache struct{ s *KnownSessions }

// BeginRead starts a read of the bucket (kvwatch.Copy.BeginRead). Every record stays through it.
func (c knownCache) BeginRead(read kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.records.BeginRead(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a value it could
// not decode, which removed the session's record.
func (c knownCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if err := c.s.records.Delivered(entry); err != nil {
		c.s.logger().Warn("known session store evicted a malformed record",
			slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", err.Error()))
	}
}

// CompleteRead removes every record the read found no evidence of, and its topics from the index:
// the bucket no longer holds that session's record.
func (c knownCache) CompleteRead(kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.records.CompleteRead()
}

// Unmarked names the records the read has no evidence of, held or only in stored, the stream's
// keys, for the watcher to read from the stream's leader before the read completes
// (kvwatch.Copy.Unmarked).
func (c knownCache) Unmarked(stored []string) []string {
	c.s.mu.RLock()
	defer c.s.mu.RUnlock()
	return c.s.records.Unmarked(stored)
}
