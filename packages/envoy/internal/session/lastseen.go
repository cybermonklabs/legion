// The session registry's last-seen times: when each session last wrote its registration, which
// LastSeen answers for a session that has lapsed, from the listener state bucket's shared record
// (WithLastSeenBucket) or from this process's own (SessionRegistry.seenLocal).

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dedupe"
)

// WithLastSeenBucket sets the listener state bucket (bus.ListenerStateBucket), where every
// registration write also records the session's last-seen time under `seen.<session token>`
// (lastSeenKey), the one record every listener pod answers LastSeen for a lapsed session from, a
// pod that started after the session lapsed included. Without it the registry keeps last-seen
// times in process (SessionRegistry.seenLocal): a listener whose NATS user lacks the grant for
// that bucket runs so, and a restart forgets them, until it opens the bucket later
// (SetLastSeenBucket).
func WithLastSeenBucket(kv bus.KeyValue) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.seen = kv }
}

// SetLastSeenBucket gives a registry opened without the listener state bucket that bucket from
// here on, as WithLastSeenBucket gives it at the open. The listener opens that bucket only after
// the registry, whose TTL the bucket's age follows, so it calls this at its start once it has
// opened the bucket, and again when a pod that started without the grant for the bucket opens it
// and joins its machine's delivery lease (cmd/listener's joinDeliveryLease). From then on every
// registration write records the session's last-seen time in the bucket, and LastSeen answers a
// lapsed session from the bucket's record alone, as every other pod of the fleet does: the times
// this process kept before (seenLocal), those its watcher delivered before the call included,
// answer nothing more, and each goes at its own deadline.
func (r *SessionRegistry) SetLastSeenBucket(kv bus.KeyValue) {
	r.mu.Lock()
	r.seen = kv
	r.mu.Unlock()
}

// LastSeen is sessionID's last registration write, in Unix milliseconds, or 0 when there is none to
// report. While the session is live it is the live registration's UpdatedAt. Once the session lapsed
// (its registration expired or was deleted) it is the time its last registration write recorded
// (recordSeen), for the registry's retention (WithLastSeenRetention, LastSeenMemory by default,
// and never less than one TTL) after that registration's expiry, so callers can report why an
// otherwise durable reference stopped resolving.
//
// With the listener state bucket (WithLastSeenBucket) that time is the session's `seen.` record
// alone, the server's timestamp of the write, which every pod reads alike, a pod started after the
// session lapsed included: every pod of the fleet answers one time for one session through the
// whole window, and a record the bucket has not got (it was never written, a bucket MaxAge shorter
// than the window removed it, or it could not be read) is no answer on any of them. The listener
// keeps a record for this registry's LastSeenWindow and the interest reaper's allowance past its
// last write, and longer when the bucket already keeps it longer (cmd/listener's
// listenerStateAge), so the window, not the bucket, ends the answer, and every pod ends it at the
// same moment. Without the bucket the time is this
// process's own record (seenLocal), the UpdatedAt of every registration this process wrote or its
// watcher saw, which a restart forgets. The window is measured on this process's clock against
// the server's timestamp; a skew of seconds against a TTL of minutes moves its end by that skew.
//
// Its read of the bucket waits no longer than ctx (bus.BoundedOn), so a caller answering a request
// keeps its own budget: a bucket that does not answer costs that budget once, and every further
// row of the same request reads an ended ctx and answers at once. A read ctx ended is a WARN and
// no time, as an unreadable record is.
//
// It takes only the shared lock and changes nothing: an expired entry the expiry timer has not
// removed yet answers the lapsed session's record, which is what its removal leaves in place, so
// the answer is the same before and after the removal.
func (r *SessionRegistry) LastSeen(ctx context.Context, sessionID string) int64 {
	now := r.now()
	r.mu.RLock()
	cs, cached := r.cache.Get(sessionID)
	at := r.seenLocal[sessionID]
	ttl, retention, seen := r.ttl, r.retention(), r.seen
	r.mu.RUnlock()
	if cached && !cs.expired(now) {
		return cs.entry.UpdatedAt
	}
	if ttl <= 0 {
		return 0
	}
	if seen.Opened() {
		stored, err := readSeen(ctx, seen, sessionID)
		if err != nil {
			r.logger().Warn("session last-seen unavailable: its record in the listener state bucket could not be read",
				slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
		}
		at = stored
	}
	if at <= 0 || !now.Before(seenUntil(at, ttl, retention)) {
		return 0
	}
	return at
}

// seenUntil is when a lapsed session's last-seen time at (Unix milliseconds) stops being reported:
// the retention after the expiry of the registration written at at, at plus the registry's
// LastSeenWindow when ttl and retention are the registry's own.
func seenUntil(at int64, ttl, retention time.Duration) time.Time {
	return time.UnixMilli(at).Add(ttl + retention)
}

// lastSeenUntilLocked is when sessionID's per-process last-seen record may be dropped after its
// entry left the cache at left: the retention past that, and never before the record's own end
// (seenUntil), so nothing LastSeen still reports is dropped. Callers must hold r.mu.
func (r *SessionRegistry) lastSeenUntilLocked(sessionID string, left time.Time) time.Time {
	until := left.Add(r.retention())
	if at, ok := r.seenLocal[sessionID]; ok && seenUntil(at, r.ttl, r.retention()).After(until) {
		return seenUntil(at, r.ttl, r.retention())
	}
	return until
}

// seenRecord is the value of a session's `seen.` record in the listener state bucket: at is the
// server's timestamp, in Unix milliseconds, of the session's last registration write.
type seenRecord struct {
	At int64 `json:"at"`
}

// lastSeenKey is sessionID's last-seen key in the listener state bucket, `seen.<session token>`
// (bus.LastSeenPrefix), with the session token every key of that bucket names a session by
// (dedupe.SessionToken).
func lastSeenKey(sessionID string) string {
	return bus.LastSeenPrefix + dedupe.SessionToken(sessionID)
}

// recordSeen records the registration write at revision written as sessionID's last-seen time in
// the listener state bucket, when the registry has it: it reads the write back from the session
// bucket for the server's timestamp, the clock the registration's expiry runs on, and puts it under
// lastSeenKey, each bucket call waiting no longer than ctx (bus.BoundedOn). A failure of either
// logs a WARN and leaves the registration, which is written, as it is: the record keeps the
// previous write's time, except after a put that ctx's end or the loss of its connection
// abandoned, which may still land and record this write's time. The read is by key, a direct get
// on the key's own subject, which the listener's grant allows; a get by revision is a direct get
// on the stream's bare subject, which it does not.
// A read that finds another revision, or no record, means a newer write replaced this one. A
// registration's newer write records its own time; a rewrite of the stored entry
// (rewriteStoredEntry: a gap mark, its clear, or a renewal) records none, so the record keeps the
// time an earlier registration recorded until the next registration write records its own.
func (r *SessionRegistry) recordSeen(ctx context.Context, kv bus.KeyValue, sessionID string, written uint64) {
	r.mu.RLock()
	seen := r.seen
	r.mu.RUnlock()
	if !seen.Opened() {
		return
	}
	stored, _, err := bus.BoundedOn(ctx, kv, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if errors.Is(err, nats.ErrKeyNotFound) || err == nil && stored.Revision() != written {
		return
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded: the registration write could not be read back",
			slog.String("session_id", sessionID), slog.Uint64("revision", written), slog.String("error", err.Error()))
		return
	}
	value, err := json.Marshal(seenRecord{At: stored.Created().UnixMilli()})
	if err == nil {
		_, _, err = bus.BoundedOn(ctx, seen, func() (uint64, error) { return seen.Put(lastSeenKey(sessionID), value) })
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded in the listener state bucket",
			slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
	}
}

// readSeen reads sessionID's last-seen time, in Unix milliseconds, from its record in seen, the
// listener state bucket: 0 when there is none. The read waits no longer than ctx (bus.BoundedOn),
// so a route that answers a caller is held no longer than its own budget by a bucket that does not
// answer; a read ctx ended is an error naming the key, which the caller reports as no time.
func readSeen(ctx context.Context, seen bus.KeyValue, sessionID string) (int64, error) {
	key := lastSeenKey(sessionID)
	entry, answered, err := bus.BoundedOn(ctx, seen, func() (nats.KeyValueEntry, error) { return seen.Get(key) })
	if !answered {
		return 0, fmt.Errorf("read %s from the listener state bucket: %w", key, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var record seenRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return 0, fmt.Errorf("decode %s: %w", entry.Key(), err)
	}
	return record.At, nil
}
