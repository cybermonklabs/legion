package session

// A known-session record is stored whole under the session's id while its encoding fits one NATS
// message, and in chunks a manifest under that id names once it does not (internal/kvchunk, which
// states the layout and why no reader gets a torn or mixed record). This file is where the
// known-session store meets that layout: the row it caches with the layout it is stored in, the
// chunks a write may delete, the collection of the chunks no record names, and the purge of the
// markers of the chunks a rewrite replaced.

import (
	"context"
	"log/slog"

	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/store"
)

// knownRecord names the known-session record in the lines internal/kvchunk logs.
const knownRecord = "known-session record"

// knownRow is a record as the bucket stores it: the record, and the layout it is stored in
// (internal/kvchunk), so the write that replaces it deletes the chunks it leaves unnamed. The
// layout is held only for a record stored in chunks, nil for one stored whole: the store holds up
// to a hundred thousand rows, nearly all of them whole, and TestKnownSessionsMemoryForAHundredThousandRecords
// measured 1,334 bytes a record so, 1,356 with the layout held by value, and 1,324 before chunks.
type knownRow struct {
	KnownSession
	layout *kvchunk.Layout
}

// newKnownRow is record as stored in layout.
func newKnownRow(record KnownSession, layout kvchunk.Layout) knownRow {
	row := knownRow{KnownSession: record}
	if layout.Chunked() {
		row.layout = &layout
	}
	return row
}

// stored is the layout the row's record is stored in, the zero layout for one stored whole.
func (r knownRow) stored() kvchunk.Layout {
	if r.layout == nil {
		return kvchunk.Layout{}
	}
	return *r.layout
}

// decodeKnownLocked is the record a put of the bucket leaves in the copy, called under s.mu: a
// record stored whole is decoded as it is, and a manifest is joined from the chunks the watcher
// delivered before it (kvchunk.Chunks.Join). A manifest the copy cannot join is a record this
// build cannot decode, which the copy stores no row for. The record keeps the fields KnownSession
// names, and every other key goes into unknown (store.DecodeKeeping): the copy stores each record
// this answers at once (kvwatch.Copy), under mu as this runs, so the keys kept are that record's.
func (s *KnownSessions) decodeKnownLocked(entry kvwatch.Entry) (knownRow, bool, error) {
	value, layout, err := s.chunks.Join(entry.Value)
	if err != nil {
		return knownRow{}, false, err
	}
	var record KnownSession
	unknown, err := store.DecodeKeeping(value, &record, knownSessionKeys)
	if err != nil {
		return knownRow{}, false, err
	}
	s.keepUnknown(entry.Key, unknown)
	return newKnownRow(record, layout), true, nil
}

// ownLayout is layout when no record the copy holds for another session names its generation,
// and the zero layout otherwise. A generation is new for each write, so only a manifest copied under
// another key - which any writer of the bucket can store - shares one, and the chunks it names are
// then not this record's alone: the write that replaces this record leaves them to the sweep's
// collection, which takes only chunks no record names. It reads every record only for a chunked
// layout. Callers hold no lock.
func (s *KnownSessions) ownLayout(sessionID string, layout kvchunk.Layout) kvchunk.Layout {
	if !layout.Chunked() {
		return layout
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, row := range s.records.All() {
		if key != sessionID && row.stored().Generation == layout.Generation {
			return kvchunk.Layout{}
		}
	}
	return layout
}

// purgeChunkMarkers purges from the bucket each chunk key's marker that the watcher was first
// delivered more than kvchunk.MarkerGrace ago (kvchunk.Markers.Due, kvchunk.PurgeMarkers), and
// reports how many it purged. Every replaced chunk leaves one marker on a key nothing writes
// again, and the bucket keeps a marker for its 30-day MaxAge, so without this pass a session whose
// record is two chunks, rewritten every KnownSessionRefreshAge, leaves 8,640 marker subjects in
// the bucket in 30 days; with it, the chunks of one rewrite and their markers for one grace or two.
// A record's own key is never purged: its marker says the session's record was deleted. The
// markers the watcher delivered decide each purge, not the bucket stream's leader:
// internal/bus/kv_leader_read.go names it among the deletes a copy decides, and why it is safe.
//
// A pass that failed - a NATS user without STREAM.PURGE on the bucket's stream refuses every
// purge - is logged at WARN once until a pass purges again, and keeps every marker it did not
// purge for the next pass, except one delivered before KnownSessionRetention, which the bucket's
// MaxAge has removed. It does nothing while the store's bucket is not open. Each purge waits no
// longer than ctx.
func (s *KnownSessions) purgeChunkMarkers(ctx context.Context) (int, error) {
	watcher := s.watch()
	if watcher == nil {
		return 0, nil
	}
	now := s.now()
	s.mu.RLock()
	due := s.markers.Due(now.Add(-kvchunk.MarkerGrace))
	s.mu.RUnlock()
	kv, _ := watcher.Handle()
	purged, err := kvchunk.PurgeMarkers(ctx, kv, due)
	s.mu.Lock()
	s.markers.Forget(due[:purged])
	if err != nil {
		s.markers.Forget(s.markers.Due(now.Add(-KnownSessionRetention)))
	}
	s.mu.Unlock()
	if err != nil {
		if !s.markerPurgeFailed.Swap(true) {
			s.logger().Warn("the known-session store could not purge the markers of its replaced chunks",
				slog.Int("markers_purged", purged), slog.Int("markers_due", len(due)), slog.String("error", err.Error()))
		}
		return purged, err
	}
	if purged > 0 {
		s.markerPurgeFailed.Store(false)
	}
	return purged, nil
}

// collectChunks deletes every chunk of the bucket that no record of the copy names and that the
// chunk copy took longer ago than kvchunk.Grace (kvchunk.Chunks.Unnamed), and reports how many it
// deleted: the chunks a write that stopped part way left behind, which no write deletes. It judges
// nothing while the store's bucket is not open, nor in a pass kvchunk.CollectionReady skips (a
// cache that does not follow the bucket, or that stays behind the bucket stream's last sequence).
// Each delete waits no longer than ctx.
// The cache, not the bucket stream's leader, decides these deletes, which
// internal/bus/kv_leader_read.go names among the deletes a copy decides, with why each is safe.
func (s *KnownSessions) collectChunks(ctx context.Context) (int, error) {
	watcher := s.watch()
	if watcher == nil {
		return 0, nil
	}
	if ready, err := kvchunk.CollectionReady(ctx, watcher, s.chunkBarrierWait, s.logger()); !ready {
		return 0, err
	}
	cutoff := s.now().Add(-kvchunk.Grace)
	s.mu.RLock()
	named := map[string]bool{}
	for _, row := range s.records.All() {
		if row.stored().Chunked() {
			named[row.stored().Generation] = true
		}
	}
	unnamed := s.chunks.Unnamed(func(generation string) bool { return named[generation] }, cutoff)
	s.mu.RUnlock()
	kv, _ := watcher.Handle()
	return kvchunk.Collect(ctx, kv, unnamed)
}
