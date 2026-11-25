package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// ErrMalformedInterest is an interest record the bucket holds and this build cannot decode. The bus
// answered it, so it is not a read that failed, and reading it again returns the same record; the
// error wrapping it carries the decode failure.
var ErrMalformedInterest = errors.New("malformed interest record")

// ErrInterestUnbound is an interest record that is no session's row of the key it is stored
// under: one naming another session (Interest.SessionID), no machine or no update time. The server
// itself stores such a record when a request names the key's subject as its reply subject: a copy
// of another session's row, or a JetStream answer naming none. No reader takes its topics: the
// cache keeps no such row (decodeInterest), and a bucket read answers it as ErrMalformedInterest
// wrapping this error, so every writer replaces it with the session's own row, under the
// session's own key, as it replaces a record it cannot decode.
var ErrInterestUnbound = errors.New("the interest record is no row of its key")

// boundInterest decodes value, the record stored under key, as key's interest row: a value that
// does not decode is the decode error, and one naming another session, no machine or no update
// time is ErrInterestUnbound.
func boundInterest(key string, value []byte) (Interest, error) {
	var item Interest
	if err := json.Unmarshal(value, &item); err != nil {
		return Interest{}, err
	}
	switch {
	case item.SessionID != key:
		return Interest{}, fmt.Errorf("%w: stored under %s, it names session %q", ErrInterestUnbound, key, item.SessionID)
	case item.MachineID == "":
		return Interest{}, fmt.Errorf("%w: stored under %s, it names no machine", ErrInterestUnbound, key)
	case item.UpdatedAt <= 0:
		return Interest{}, fmt.Errorf("%w: stored under %s, it names no update time", ErrInterestUnbound, key)
	}
	return item, nil
}

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache. A cache hit answers in the
// caller's goroutine; only the bucket read on a miss waits on ctx (bus.Bounded). The cache holds
// only records it decoded (interestCache.Apply evicts the rest), so only the bucket read can meet
// a record this build cannot decode: that is ErrMalformedInterest, logged at WARN with the session
// it belongs to. A read of a bucket since recreated, or one the revision fence drops, is answered
// but not cached: the new bucket's read delivers what it holds.
func (r *Registry) Get(ctx context.Context, sessionID string) (Interest, error) {
	item, _, err := r.getWithRevision(ctx, sessionID, readCache)
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
// the bucket holds no row for answers revision 0 with nats.ErrKeyNotFound, and a row this build
// cannot decode answers the revision a write of it is fenced on with ErrMalformedInterest.
func (r *Registry) RefreshAt(ctx context.Context, sessionID string) (Interest, uint64, error) {
	return r.getWithRevision(ctx, sessionID, readBucket)
}

// interestRead is where getWithRevision reads a session's row from.
type interestRead uint8

const (
	// readCache answers from the cache, and reads the bucket as readBucket does on a miss.
	readCache interestRead = iota
	// readBucket reads the bucket by a direct get, whatever the cache holds.
	readBucket
	// readLeader reads the key's newest message from the stream's leader, markers and the headers
	// the stream stored with it included (bus.KeyValue.NewestFromLeader): the read a write makes
	// again after the bucket refused it at the revision a direct get reported (UpsertWithin,
	// PlanInterestAfterConflict, removeInterestTopics).
	readLeader
)

// getWithRevision is Get with the bucket revision the record it answers was read at: the revision
// the copy's fence holds on a cache hit, the entry's own on a bucket read, and 0 for a session the
// bucket holds no record for. A record this build cannot decode carries its revision too, since
// the write that replaces it is conditional on it (putInterestAt), and so does a record naming
// another session (ErrInterestUnbound), which is answered as one it cannot decode. A cache hit
// whose fence belongs to another epoch answers 0 as well (kvwatch.Copy.FenceFor): the write that
// names it then creates the record, the bucket refuses it for a record it holds
// (ErrInterestPlanConflict), and the plan made again over the bucket's own row carries the
// revision that write needs.
//
// read says where the row is read from (interestRead). readBucket reads the bucket even when the
// cache holds the record, for a caller whose cache may be behind a write its answer must hold: one
// reading the topics a registration on another listener has just stored (RefreshAt), and the
// planned write's own read of the row it is fenced on (UpsertPlanned).
//
// A record the direct get answers that is no row of the session's is read again from the stream's
// leader (bus.KeyValue.GetFromLeader), and that read is what this answers, its revision included.
// The direct get reports the first Nats-Sequence header of the message it answers, and a record
// the server stored as its answer to a direct get of another key carries that key's header ahead
// of its own: the read reports the source record's revision, not the one the copy is stored at.
// A write replacing the copy at the revision the direct get reported is refused with wrong last
// sequence on every attempt, and the session's every registration answers 503 for as long as the
// copy stands; the leader's stream message get reports the sequence the message is stored at.
// Only such a record costs the second read, so a session's own row is read once, as before.
//
// A record that does read as the session's row can still be one the server wrote: a RePublish of
// another stream naming the key's subject stores whatever body that stream was given, with that
// stream's Nats-Sequence header, so a direct get reports it at that stream's sequence and a write
// fenced there is refused at every attempt. readLeader, the read a write makes after such a
// refusal, reads it from the leader with the headers it is stored with, and answers a record the
// server wrote (bus.LeaderServerWritten) as no row of the session's (ErrInterestUnbound, wrapped in
// ErrMalformedInterest) at the sequence it is stored at, which the write then replaces with the
// session's own row; and a delete or purge marker as no row (nats.ErrKeyNotFound) at its own
// sequence, which the write replaces the same way.
func (r *Registry) getWithRevision(ctx context.Context, sessionID string, read interestRead) (Interest, uint64, error) {
	if read == readCache {
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
	var fresh Interest
	var revision uint64
	var serverWritten bool
	var err error
	if read == readLeader {
		var leader bus.LeaderEntry
		leader, err = kv.ReadFromLeader(ctx, sessionID)
		switch {
		case err != nil:
			return Interest{}, 0, err
		case leader.Kind == bus.LeaderAbsent:
			return Interest{}, 0, nats.ErrKeyNotFound
		case leader.Kind == bus.LeaderMarker:
			return Interest{}, leader.Revision, nats.ErrKeyNotFound
		case leader.Kind == bus.LeaderServerWritten:
			revision, serverWritten = leader.Revision, true
			err = fmt.Errorf("%w: stored under %s, it is a message the server wrote", ErrInterestUnbound, sessionID)
		default:
			revision = leader.Revision
			fresh, err = boundInterest(sessionID, leader.Value)
		}
	} else {
		entry, answered, readErr := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) {
			return kv.ReadStored(ctx, sessionID, rowOf(sessionID))
		})
		if !answered {
			return Interest{}, 0, fmt.Errorf("read %s's interest record: %w", sessionID, readErr)
		}
		if readErr != nil {
			return Interest{}, 0, readErr
		}
		revision = entry.Revision()
		fresh, err = boundInterest(sessionID, entry.Value())
	}
	if err != nil {
		msg := "registry read an interest record it cannot decode"
		switch {
		case serverWritten:
			msg = "registry read an interest record the server wrote"
		case errors.Is(err, ErrInterestUnbound):
			msg = "registry read an interest record naming another session"
		}
		r.logger().Warn(msg,
			slog.String("session_id", sessionID),
			slog.Uint64("revision", revision),
			slog.String("error", err.Error()),
		)
		return Interest{}, revision, fmt.Errorf("%w: %w", ErrMalformedInterest, err)
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
