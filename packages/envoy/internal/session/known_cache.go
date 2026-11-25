package session

import (
	"log/slog"

	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// knownCache is the store as the cache its watcher feeds (kvwatch.Cache). The hooks are its own
// type's, not the store's methods, so nothing but the watcher calls them.
type knownCache struct{ s *KnownSessions }

// BeginRead starts a read of the bucket (kvwatch.Copy.BeginRead), in the copy of its records and
// the copy of its chunks alike. Every record and chunk stays through it.
func (c knownCache) BeginRead(read kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.records.BeginRead(read)
	c.s.chunks.BeginRead(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered): a chunk into the copy of the
// chunks, and its marker, when the record is one, into the markers the marker pass purges
// (purgeChunkMarkers); anything else into the copy of the records, which joins a manifest from the
// chunks delivered before it (decodeKnownLocked). It logs a value it could not decode, which
// removed the session's record.
func (c knownCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if kvchunk.IsChunkKey(entry.Key) {
		_ = c.s.chunks.Delivered(entry)
		c.s.markers.Delivered(entry, c.s.now())
		return
	}
	if err := c.s.records.Delivered(entry); err != nil {
		c.s.logger().Warn("known session store evicted a malformed record",
			slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", err.Error()))
	}
}

// CompleteRead removes every record and chunk the read found no evidence of, and each record's
// topics from the index: the bucket no longer holds that key.
func (c knownCache) CompleteRead(kvwatch.Read) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	c.s.records.CompleteRead()
	c.s.chunks.CompleteRead()
}
