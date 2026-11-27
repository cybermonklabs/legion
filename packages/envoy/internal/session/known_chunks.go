package session

// A known-session record is stored whole under the session's id while its encoding fits one NATS
// message, and in chunks a manifest under that id names once it does not (internal/kvchunk, which
// states the layout and why no reader gets a torn or mixed record). This file is where the
// known-session store meets that layout: the row it caches with the layout it is stored in, the
// chunks a write may delete, the collection of the chunks no record names, and the purge of the
// markers of the chunks a rewrite replaced.

import (
	"context"
	"encoding/json"
	"log/slog"
	"unique"

	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// knownRecord names the known-session record in the lines internal/kvchunk logs.
const knownRecord = "known-session record"

// knownRow is a record as the bucket stores it: the record, and the layout it is stored in
// (internal/kvchunk), so the write that replaces it deletes the chunks it leaves unnamed. The
// layout is held only for a record stored in chunks, nil for one stored whole: the store holds up
// to a hundred thousand rows, nearly all of them whole, and TestKnownSessionsMemoryForAHundredThousandRecords
// measured 1,334 bytes a record so, 1,356 with the layout held by value, and 1,324 before chunks.
// A row's principal is shared with every other row naming the same one (unique.Make), since a
// few principals register many sessions, and a delivered row's session id shares the bytes of
// the key the copy holds it under (decodeKnownLocked): with both, a record that names a principal
// measured 1,323 bytes, and one held with a principal of its own and a session id of its own 1,379.
type knownRow struct {
	KnownSession
	layout *kvchunk.Layout
}

// newKnownRow is record as stored in layout.
func newKnownRow(record KnownSession, layout kvchunk.Layout) knownRow {
	if record.Principal != "" {
		record.Principal = unique.Make(record.Principal).Value()
	}
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
// build cannot decode, which the copy stores no row for.
func (s *KnownSessions) decodeKnownLocked(entry kvwatch.Entry) (knownRow, bool, error) {
	value, layout, err := s.chunks.Join(entry.Value)
	if err != nil {
		return knownRow{}, false, err
	}
	var record KnownSession
	if err := json.Unmarshal(value, &record); err != nil {
		return knownRow{}, false, err
	}
	// The decode allocates the session id afresh, while the copy, its fence and the index already
	// hold the key's own bytes; a record whose id is its key holds those instead.
	if record.SessionID == entry.Key {
		record.SessionID = entry.Key
	}
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
// A record's own key is never purged: its marker says the session's record was deleted.
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
// nothing while the store's bucket is not open or its cache does not follow the bucket
// (kvwatch.Watcher.Following), since a copy behind the bucket could lack the manifest that names
// a chunk. Each delete waits no longer than ctx.
func (s *KnownSessions) collectChunks(ctx context.Context) (int, error) {
	watcher := s.watch()
	if watcher == nil || !watcher.Following() {
		return 0, nil
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
