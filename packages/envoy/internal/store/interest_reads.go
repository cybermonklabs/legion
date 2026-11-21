package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/sjawhar/envoy/internal/bus"
)

// ErrMalformedInterest is an interest record the bucket holds and this build cannot decode. The bus
// answered it, so it is not a read that failed, and reading it again returns the same record; the
// error wrapping it carries the decode failure.
var ErrMalformedInterest = errors.New("malformed interest record")

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache. A cache hit answers in the
// caller's goroutine; only the bucket read on a miss waits on ctx (bus.BoundedOn), and it
// ends at once with bus.ErrRequestLostWithConnection when its connection is lost before the answer
// comes, rather than waiting out ctx for an answer nothing will send. The cache holds
// only records it decoded (interestCache.Apply evicts the rest), so only the bucket read can meet
// a record this build cannot decode: that is ErrMalformedInterest, logged at WARN with the session
// it belongs to. A read of a bucket since recreated, or one the revision fence drops, is answered
// but not cached: the new bucket's read delivers what it holds.
func (r *Registry) Get(ctx context.Context, sessionID string) (Interest, error) {
	item, _, err := r.getWithRevision(ctx, sessionID, false)
	return item, err
}

// RefreshAt is Get read from the bucket and never from the cache, with the bucket revision the
// row it answers was read at, for a caller whose answer must hold a write its own cache need not
// hold yet: the listener's repair of a session's consumer, which filters it on the row of a
// registration made on another listener (cmd/listener's registeredTarget, which reads that
// session's record beside this row through session.SessionRegistry.Refresh). The row read is
// cached as Get's own bucket read is, and a row this build cannot decode is ErrMalformedInterest
// here too. The bucket read is a direct get: bus.KeyValue.Get states that limit and why this
// build leaves it standing, and registeredTarget what it costs the repair reading this.
//
// The revision is what a write of the filter built from this row is fenced on (cmd/listener's
// fenceFilterWrite): a row the bucket answers at a higher revision afterwards moved under that
// write, so the filter it carries may be an older row's, over a newer registration's. A session
// the bucket holds no row for answers revision 0 with nats.ErrKeyNotFound, a session whose row a
// delete or purge marker ended included, since that fence reads a row gone as a row that never
// was; a row this build cannot decode answers the revision a write of it is fenced on with
// ErrMalformedInterest.
func (r *Registry) RefreshAt(ctx context.Context, sessionID string) (Interest, uint64, error) {
	return r.getWithRevision(ctx, sessionID, true)
}

// getWithRevision is Get with the bucket revision the record it answers was read at, the revision
// a write of the record is fenced on (putInterestAt): the revision the copy's fence holds on a
// cache hit, and the record's own on a bucket read. A record this build cannot decode carries its
// revision too, since the write that replaces it is conditional on it. A session the bucket holds
// no record for answers nats.ErrKeyNotFound at revision 0 from that one bucket read, a session
// whose record a delete or purge marker ended included: this is the read of a caller that writes
// only over a record it found, or writes nothing, and a caller that plans a write over a record
// it found missing reads getLatest. A cache hit whose fence belongs to another epoch answers 0 as
// well (kvwatch.Copy.FenceFor): the write that names it is refused over any record the bucket
// holds (ErrInterestPlanConflict), and the plan made again over the bucket's own row carries the
// revision that write needs.
//
// fromBucket reads the bucket even when the cache holds the record, for a caller whose cache may
// be behind a write its answer must hold: one whose compare-and-swap the bucket refused, since
// the cache is then behind the row that refused it and a plan made from the cache would be
// refused again (UpsertPlanned), and one reading the topics a registration on another listener
// has just stored (RefreshAt).
func (r *Registry) getWithRevision(ctx context.Context, sessionID string, fromBucket bool) (Interest, uint64, error) {
	return r.readInterest(ctx, sessionID, fromBucket, false)
}

// getLatest is getWithRevision for a caller that plans a write over what it read, a record it
// found missing included (UpsertWithin, PlanInterest, UpsertPlanned): a bucket read that finds no
// record answers the revision of the key's newest entry beside nats.ErrKeyNotFound
// (bus.KeyValue.GetLatest). That is a delete or purge marker's for a record one ended, and 0 only
// for a session whose key holds no entry at all. A write planned on a deleted record therefore
// lands only over that very delete, and one planned on no entry only where none was stored since:
// a write at revision 0 is refused over any entry, a marker included (putInterestAt), so a plan
// that read a marker as revision 0 would be refused for good. The marker costs a second read of
// the bucket on every miss, and a failure of that read fails the whole read, which is why a
// caller that writes nothing over a missing record reads getWithRevision instead.
func (r *Registry) getLatest(ctx context.Context, sessionID string, fromBucket bool) (Interest, uint64, error) {
	return r.readInterest(ctx, sessionID, fromBucket, true)
}

// readInterest is getWithRevision, and getLatest where latest is set.
func (r *Registry) readInterest(ctx context.Context, sessionID string, fromBucket, latest bool) (Interest, uint64, error) {
	if !fromBucket {
		r.mu.RLock()
		item, ok := r.cache.Get(sessionID)
		_, cached := r.watcher.Handle()
		revision := r.cache.FenceFor(cached, sessionID)
		r.mu.RUnlock()
		if ok {
			return item, revision, nil
		}
	}
	kv, epoch := r.watcher.Handle()
	read, answered, err := bus.BoundedOn(ctx, kv, func() (bus.LatestEntry, error) {
		if latest {
			return kv.GetLatest(sessionID)
		}
		entry, err := kv.Get(sessionID)
		return bus.LatestEntry{Entry: entry}, err
	})
	if !answered {
		return Interest{}, 0, fmt.Errorf("read %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return Interest{}, read.Revision, err
	}
	entry := read.Entry
	fresh, err := decodeInterestValue(entry.Value())
	if err != nil {
		r.logger().Warn("registry read an interest record it cannot decode",
			slog.String("session_id", sessionID),
			slog.Uint64("revision", entry.Revision()),
			slog.String("error", err.Error()),
		)
		return Interest{}, entry.Revision(), fmt.Errorf("%w: %w", ErrMalformedInterest, err)
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, sessionID, fresh, entry.Revision())
	r.mu.Unlock()
	return fresh, entry.Revision(), nil
}

// Cached returns sessionID's interest from the cache alone, with no bucket read on a miss, for a
// caller that must make no per-key read, such as one joining a whole list with its interests.
func (r *Registry) Cached(sessionID string) (Interest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cache.Get(sessionID)
}

// Match returns the cached interests registered on machineID whose topics match topic, looked up
// in the subject index: a listener delivers live only to the sessions of its own machine.
func (r *Registry) Match(machineID string, topic string) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.matchLocked(func(visit func(string)) { r.index.match(machineID, topic, visit) })
}

// MatchAllMachines returns the cached interests whose topics match topic on every machine, each
// session once however many of its topics match: the sessions a publish to topic is for, wherever
// they registered, which a publish's recipient count reads (cmd/listener). Delivery reads Match,
// a machine's own sessions; nothing delivers by this walk.
func (r *Registry) MatchAllMachines(topic string) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.matchLocked(func(visit func(string)) { r.index.matchAllMachines(topic, visit) })
}

// matchLocked collects the cached interests walk visits, once each (oncePerKey): a session's
// interest can hold several patterns that match one subject, and the index visits its key once
// per matching pattern. A registry with no index yet, which a test can build as a literal,
// matches nothing. Callers hold r.mu.
func (r *Registry) matchLocked(walk func(visit func(key string))) []Interest {
	out := []Interest{}
	if r.index == nil {
		return out
	}
	walk(oncePerKey(func(key string) {
		if item, ok := r.cache.Get(key); ok {
			out = append(out, item)
		}
	}))
	return out
}

// List returns all cached interests sorted by SessionID for deterministic output, or
// kvwatch.ErrRefilling while the copy cannot be served (Servable): before its first complete read
// of the bucket, and once it has not followed the bucket for longer than the stale bound.
func (r *Registry) List() ([]Interest, error) {
	if err := r.Servable(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Interest, 0, r.cache.Len())
	for _, item := range r.cache.All() {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}
