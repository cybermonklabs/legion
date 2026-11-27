package kvchunk

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// MarkerGrace is how long after a store's watcher was delivered a chunk key's delete marker the
// store may purge that marker (Markers, PurgeMarkers): long enough that every running listener's
// watcher has been delivered the delete too and dropped the chunk from its copy, so the purge takes
// away nothing a copy still holds. A listener whose watcher misses the delete - a reconnect across
// it - drops the chunk at its next read of the bucket, which finds no message on the key.
const MarkerGrace = 5 * time.Minute

// Markers is the chunk keys a store's watcher was delivered as removed - a delete or purge marker,
// or the server's removal of a chunk for its age - with when it was first delivered on the store's
// clock, for a store whose bucket keeps a marker until its MaxAge (the known-session bucket) to
// purge them. A chunk key is never written again, since each write names a new generation, so once
// no copy holds its chunk its marker means nothing, and a bucket of such markers would hold one
// subject for every chunk each rewrite replaced. A record's own key is never one: its marker says
// the record was deleted.
//
// It has no lock of its own: the store calls every method under the mutex that guards its copies.
type Markers struct {
	seen map[string]time.Time
}

// NewMarkers is an empty set of a bucket's chunk markers.
func NewMarkers() *Markers { return &Markers{seen: map[string]time.Time{}} }

// Delivered takes one record of a chunk's key the watcher delivered at now: a marker is kept with
// the first moment it was delivered, so a read of the bucket again does not restart its grace, and
// a put - a key nothing writes again, delivered again only by a forged write - drops it. A key not
// in the shape this build writes (wellFormed) is never kept, so no pass ever offers it to
// PurgeMarkers.
func (m *Markers) Delivered(entry kvwatch.Entry, now time.Time) {
	if entry.Op == kvwatch.OpPut || !wellFormed(entry.Key) {
		delete(m.seen, entry.Key)
		return
	}
	if _, ok := m.seen[entry.Key]; !ok {
		m.seen[entry.Key] = now
	}
}

// Due is every marker first delivered at cutoff or before, sorted: the ones PurgeMarkers purges.
func (m *Markers) Due(cutoff time.Time) []string {
	var due []string
	for key, seen := range m.seen {
		if !seen.After(cutoff) {
			due = append(due, key)
		}
	}
	slices.Sort(due)
	return due
}

// Forget drops keys, markers the store purged or found gone.
func (m *Markers) Forget(keys []string) {
	for _, key := range keys {
		delete(m.seen, key)
	}
}

// PurgeMarkers purges from kv's stream the one subject of each of keys, chunk keys whose newest
// message is a marker (Markers.Due), with a purge filtered to that subject, which leaves no message
// on it - no marker either - and reports how many it purged. A purge of a subject another listener
// purged first purges nothing and counts too. A purge that failed ends it with that error: a server
// that refuses it - a NATS user without STREAM.PURGE on the bucket's stream - refuses every one.
// Each request waits no longer than ctx, and no longer than bus.RequestTimeout for a ctx with no
// deadline of its own. A request nats.go refuses while it reconnects the connection fails as
// bus.ErrReconnecting (bus.Reconnecting).
//
// Each key becomes a purge filter, so each must be in the shape this build writes (wellFormed):
// one that is not - a key holding a wildcard would purge every chunk subject of the bucket - is
// refused before any purge is sent, and nothing is purged.
func PurgeMarkers(ctx context.Context, kv bus.KeyValue, keys []string) (int, error) {
	for _, key := range keys {
		if !wellFormed(key) {
			return 0, fmt.Errorf("purge the marker of %q: not a key in the shape of a chunk's, %s<generation>.<index>", key, Prefix)
		}
	}
	if len(keys) == 0 {
		return 0, nil
	}
	within := func() (context.Context, context.CancelFunc) {
		if _, ok := ctx.Deadline(); ok {
			return context.WithCancel(ctx)
		}
		return context.WithTimeout(ctx, bus.RequestTimeout)
	}
	opened, cancel := within()
	stream, err := kv.Streams().Stream(opened, "KV_"+kv.Bucket())
	cancel()
	if err != nil {
		return 0, fmt.Errorf("open the stream of bucket %s to purge its chunk markers: %w", kv.Bucket(), bus.Reconnecting(err))
	}
	for purged, key := range keys {
		request, cancel := within()
		err := stream.Purge(request, jetstream.WithPurgeSubject("$KV."+kv.Bucket()+"."+key))
		cancel()
		if err != nil {
			return purged, fmt.Errorf("purge the marker of chunk %s: %w", key, bus.Reconnecting(err))
		}
	}
	return len(keys), nil
}
