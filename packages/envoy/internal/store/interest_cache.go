package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// newInterestCopy is the copy of the interest bucket r keeps, which holds every interest it can
// decode (decodeStoredLocked) and keeps r's subject index in step with its rows (kvwatch.Rows).
func newInterestCopy(r *Registry) kvwatch.Copy[storedRow] {
	return kvwatch.NewCopy(kvwatch.Rows[storedRow]{Decode: r.decodeStoredLocked, Stored: r.indexInterestLocked, Removed: r.unindexInterestLocked})
}

// indexInterestLocked and unindexInterestLocked keep the subject index in step with the copy: the
// index is what Match walks, so every interest the copy stores is set in it and every
// one it removes is taken out. The copy calls each under r.mu, held for writing.
func (r *Registry) indexInterestLocked(sessionID string, item storedRow) {
	if r.index == nil {
		r.index = newSubjectIndex()
	}
	r.index.set(sessionID, item.Interest)
}

func (r *Registry) unindexInterestLocked(sessionID string, _ storedRow) {
	if r.index != nil {
		r.index.remove(sessionID)
	}
}

// interestCache is the registry as the cache its watcher feeds (kvwatch.Cache). The hooks are its
// own type's, not the registry's methods, so nothing but the watcher calls them.
type interestCache struct{ r *Registry }

// BeginRead starts a read of the interest bucket (kvwatch.Copy.BeginRead), in the copy of its
// interests and the copy of its chunks alike. Every interest and chunk stays.
func (c interestCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.BeginRead(read)
	r.chunks.BeginRead(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered): a chunk into the copy of the
// chunks, and anything else into the copy of the interests, which joins a manifest from the
// chunks delivered before it (decodeStoredLocked). It logs a value it could not decode, which
// evicted the interest. The revision fence keeps a delayed local write-through and a watcher
// update from undoing each other.
func (c interestCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	var malformed error
	if kvchunk.IsChunkKey(entry.Key) {
		malformed = r.chunks.Delivered(entry)
	} else {
		malformed = r.cache.Delivered(entry)
	}
	r.mu.Unlock()
	if malformed != nil {
		r.logger().Warn("registry watcher evicted malformed value",
			slog.String("key", entry.Key),
			slog.Uint64("revision", entry.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}

// CompleteRead removes every interest, chunk and fence entry the read found no evidence of: the
// bucket no longer holds that key.
func (c interestCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.CompleteRead()
	r.chunks.CompleteRead()
}

// WaitForCacheReady blocks until both views answer, or until ctx is done. The interest watcher is
// waited for as ready (kvwatch.Watcher.WaitReady), having ended its first scan or failed to
// start, and the role claim watcher as servable (kvwatch.Watcher.WaitComplete): the interest view
// answers from whatever its copy holds, so a partial read still serves, while the role claim view
// refuses every read until a read of its bucket has completed (ErrRoleViewNotReady), so readiness
// alone would let a caller serve a view that answers nothing. After it returns nil, Match sees
// every existing subscription in the bucket and RolesHeldBy answers. A read whose scan timed out
// releases readiness without completing, and the watcher reads the bucket again itself after it,
// so this wait ends on that re-read rather than hanging. Callers bound ctx: a watcher that fails
// to start releases it too, and so does one that can complete no read at all, through its
// terminal error or its stop; the caller then serves the write-through path while /healthz
// exposes the unavailable registry. Its error names the cache that did not answer.
func (r *Registry) WaitForCacheReady(ctx context.Context) error {
	if err := r.watcher.WaitReady(ctx); err != nil {
		return fmt.Errorf("interest cache: %w", err)
	}
	if err := r.roleWatcher.WaitComplete(ctx); err != nil {
		return fmt.Errorf("role claim cache: %w", err)
	}
	return nil
}
