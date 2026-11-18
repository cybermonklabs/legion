package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvchunk"
)

// ErrMalformedInterest is an interest record the bucket holds and this build cannot decode. The bus
// answered it, so it is not a read that failed, and reading it again returns the same record; the
// error wrapping it carries the decode failure.
var ErrMalformedInterest = errors.New("malformed interest record")

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache. A cache hit answers in the
// caller's goroutine; only the bucket read on a miss waits on ctx (bus.Bounded). The cache holds
// only records it decoded (interestCache.Apply evicts the rest), so only the bucket read can meet
// a record this build cannot decode: that is ErrMalformedInterest, logged at WARN with the session
// it belongs to. A record stored in chunks reads whole or not at all (decodeEntry). A read of a
// bucket since recreated, or one the revision fence drops, is answered but not cached: the new
// bucket's read delivers what it holds.
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
// here too. The read of the session's key is a direct get: bus.KeyValue.Get states that limit and
// why this build leaves it standing, and registeredTarget what it costs the repair reading this.
// The chunks of a record stored in chunks are read from the stream's leader (decodeEntry).
//
// The revision is what a write of the filter built from this row is fenced on (cmd/listener's
// fenceFilterWrite): a row the bucket answers at a higher revision afterwards moved under that
// write, so the filter it carries may be an older row's, over a newer registration's. A session
// the bucket holds no row for answers revision 0 with nats.ErrKeyNotFound, and a row this build
// cannot decode answers the revision a write of it is fenced on with ErrMalformedInterest.
func (r *Registry) RefreshAt(ctx context.Context, sessionID string) (Interest, uint64, error) {
	return r.getWithRevision(ctx, sessionID, true)
}

// getWithRevision is readStored with the row alone, for a caller that writes nothing over it.
func (r *Registry) getWithRevision(ctx context.Context, sessionID string, fromBucket bool) (Interest, uint64, error) {
	stored, revision, err := r.readStored(ctx, sessionID, fromBucket)
	return stored.Interest, revision, err
}

// readStored is Get with the bucket revision the record it answers was read at and the layout it
// is stored in, which a write over it or a delete of it reads to remove the chunks it leaves
// unnamed: the revision the copy's fence holds on a cache hit, the entry's own on a bucket read,
// and 0 for a session the bucket holds no record for. A record this build cannot decode carries
// its revision too, since the write that replaces it is conditional on it (putInterestAt), and a
// layout only when its chunks joined and its JSON did not decode (decodeEntry). A cache hit whose fence belongs
// to another epoch answers 0 as well (kvwatch.Copy.FenceFor): the write that names it then
// creates the record, the bucket refuses it for a record it holds (ErrInterestPlanConflict), and
// the plan made again over the bucket's own row carries the revision that write needs. No record
// is stored under a chunk's key (kvchunk.Prefix), so a session id under it is answered
// nats.ErrKeyNotFound with no read.
//
// fromBucket reads the bucket even when the cache holds the record, for a caller whose cache may
// be behind a write its answer must hold: one whose compare-and-swap the bucket refused, since
// the cache is then behind the row that refused it and a plan made from the cache would be
// refused again (UpsertPlanned), and one reading the topics a registration on another listener
// has just stored (RefreshAt).
func (r *Registry) readStored(ctx context.Context, sessionID string, fromBucket bool) (storedRow, uint64, error) {
	if kvchunk.IsChunkKey(sessionID) {
		return storedRow{}, 0, nats.ErrKeyNotFound
	}
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
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return storedRow{}, 0, fmt.Errorf("read %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return storedRow{}, 0, err
	}
	fresh, revision, err := r.decodeEntry(ctx, kv, sessionID, entry)
	if err != nil {
		if !errors.Is(err, ErrMalformedInterest) {
			return storedRow{}, 0, err
		}
		return fresh, revision, err
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, sessionID, fresh, revision)
	r.mu.Unlock()
	return fresh, revision, nil
}

// Cached returns sessionID's interest from the cache alone, with no bucket read on a miss, for a
// caller that must make no per-key read, such as one joining a whole list with its interests.
func (r *Registry) Cached(sessionID string) (Interest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.cache.Get(sessionID)
	return item.Interest, ok
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
			out = append(out, item.Interest)
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
		out = append(out, item.Interest)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}
