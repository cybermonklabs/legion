package session

// The session registry's and the known-session store's part in a bucket deleted and created again
// (INFRA-4094): the records each owns - the ones its own listener wrote, which name that listener
// - written back into the new bucket from the read that finds it (beginRead, reassert), and the
// write whose own result the copy dropped because the bucket was recreated while it was in flight,
// written again into the bucket the handle reaches now (putAgainWithin, writeAgainWithin).
// kvwatch.Reassert says why an ack cannot name the incarnation that took it.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// beginRead starts a read of the session bucket (kvwatch.Copy.BeginRead) with the bucket's MaxAge
// as the ttl when it has one, so a bucket recreated with another TTL expires its rows by its own.
// Every row stays. It answers the rows the read writes back (kvwatch.Reasserter.Due): for the
// first read of a recreated bucket, the rows this registry owns - registered through its own
// listener (SessionEntry.MachineID is the owner) - and still holds live; for a later read of the
// same epoch, the rows that write-back could not write. A row another listener registered is that
// listener's to write back, and a row no listener runs - one whose listener stopped - is no
// listener's: a session registers again through whichever listener it reaches next. Of its own
// rows it leaves out a gone session's (StateOf), which the session reaper deletes at its next
// pass, and expires the rest at their deadlines (kvwatch.Held.Expires), which the write-back does
// not write past. A session that ended after the recreate is not brought back either: its delete
// marked the key in the new bucket (bus.KeyValue.DeleteAtRead), which the write-back leaves
// standing.
func (r *SessionRegistry) beginRead(read kvwatch.Read) []kvwatch.Held[*cachedSession] {
	r.mu.Lock()
	defer r.unlockAndNotify()
	r.cache.BeginRead(read)
	if read.MaxAge > 0 {
		r.ttl = read.MaxAge
	}
	return r.reassertion.Due(read, func() []kvwatch.Held[*cachedSession] {
		if r.owner == "" {
			return nil
		}
		now := r.now()
		var held []kvwatch.Held[*cachedSession]
		for sessionID, row := range r.cache.All() {
			if row.entry.MachineID != r.owner || len(row.record) == 0 || r.stateOf(row.entry, now) == Gone {
				continue
			}
			held = append(held, kvwatch.Held[*cachedSession]{Key: sessionID, Record: row.record, Value: row, Expires: row.expiresAt})
		}
		return held
	})
}

// reassert writes held, the rows beginRead answered for read, into the recreated bucket
// (kvwatch.Reasserter.Write): where the bucket holds nothing for the key, and over a row registered
// before the held one (UpdatedAt), never over a delete or a later registration. It stores each
// row the bucket took in the copy at read's epoch, so the read's sweep keeps it. It runs in the
// read's BeginRead with r.mu released, and takes r.mu for each row it stores.
func (r *SessionRegistry) reassert(read kvwatch.Read, held []kvwatch.Held[*cachedSession]) {
	r.reassertion.Write(r.watcher, read, held,
		func(stored []byte, h kvwatch.Held[*cachedSession]) bool {
			return registeredSince(stored, h.Value.entry.UpdatedAt)
		},
		func(h kvwatch.Held[*cachedSession], revision uint64) {
			r.mu.Lock()
			defer r.unlockAndNotify()
			r.cache.Put(r.watcher, read.Epoch, h.Key, h.Value, revision)
		})
}

// registeredSince reports whether stored, a registration record, was made at updatedAt or later,
// by the stamp every registration carries (SessionEntry.UpdatedAt).
func registeredSince(stored []byte, updatedAt int64) bool {
	var row SessionEntry
	return json.Unmarshal(stored, &row) == nil && row.UpdatedAt >= updatedAt
}

// putAgainWithin writes entry, encoded as buf, for sessionID again into the bucket the handle
// reaches now, after the copy dropped its first write's result (kvwatch.WriteAgain): the read that
// found the new bucket wrote back the row the copy held then (reassert), which predates this write
// and may be the row the new bucket holds now. Registrations are ordered by their own stamp
// (UpdatedAt), the moment each was made, so a row stamped as late as this one or later stands and
// is taken as read, and an older one is written over. A session whose key the new bucket marks
// deleted stands deleted; its next heartbeat registers it again if it still runs.
func (r *SessionRegistry) putAgainWithin(ctx context.Context, sessionID string, entry SessionEntry, buf []byte) error {
	return kvwatch.WriteAgain(ctx, r.watcher, sessionID, buf,
		func(stored []byte) bool { return registeredSince(stored, entry.UpdatedAt) },
		func(epoch, revision uint64) kvwatch.Outcome {
			r.mu.Lock()
			defer r.unlockAndNotify()
			return r.cache.Put(r.watcher, epoch, sessionID, &cachedSession{entry: entry, expiresAt: r.expiryFor(time.Time{}, entry.UpdatedAt), record: buf}, revision)
		},
		func(read kvwatch.Entry, epoch uint64) {
			r.mu.Lock()
			defer r.unlockAndNotify()
			r.cache.Apply(r.watcher, epoch, read)
		})
}

// beginRead starts a read of the known-session bucket (kvwatch.Copy.BeginRead); every record stays
// through it. It answers the records the read writes back (kvwatch.Reasserter.Due): for the first
// read of a recreated bucket, the records this store owns - the ones its own listener last wrote,
// which name it (KnownSession.MachineID) - expiring one bucket TTL after their write, which the
// write-back does not write past; for a later read of the same epoch, the records that write-back
// could not write. The bucket has no delete path, so no record of its own ends while the bucket is
// away. A record an earlier build wrote names no listener, and none writes it back.
func (s *KnownSessions) beginRead(read kvwatch.Read) []kvwatch.Held[kvwatch.Kept[KnownSession]] {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records.BeginRead(read)
	return s.reassertion.Due(read, func() []kvwatch.Held[kvwatch.Kept[KnownSession]] {
		if s.owner == "" {
			return nil
		}
		cutoff := s.cutoff()
		var held []kvwatch.Held[kvwatch.Kept[KnownSession]]
		for sessionID, kept := range s.records.All() {
			// A record past the retention is one the store answers as none and its sweep
			// drops: it is not written back to live a retention again.
			if kept.Value.MachineID != s.owner || len(kept.Record) == 0 || kept.Value.LastSeen < cutoff {
				continue
			}
			held = append(held, kvwatch.HeldFrom(sessionID, kept, read.MaxAge))
		}
		return held
	})
}

// reassert writes held, the records beginRead answered for read, into the recreated bucket
// (kvwatch.Reasserter.Write): where the bucket holds nothing for the key, and over a record last
// written before the held one (LastSeen), never over a later write. It stores each record the
// bucket took in the copy at read's epoch, so the read's sweep keeps it. It runs in the read's
// BeginRead with s.mu released.
func (s *KnownSessions) reassert(read kvwatch.Read, held []kvwatch.Held[kvwatch.Kept[KnownSession]]) {
	watcher := s.watch()
	if watcher == nil {
		return
	}
	s.reassertion.Write(watcher, read, held,
		func(stored []byte, h kvwatch.Held[kvwatch.Kept[KnownSession]]) bool {
			return seenSince(stored, h.Value.Value.LastSeen)
		},
		func(h kvwatch.Held[kvwatch.Kept[KnownSession]], revision uint64) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.records.Put(watcher, read.Epoch, h.Key, h.Value, revision)
		})
}

// seenSince reports whether stored, a known-session record, was written at lastSeen or later.
func seenSince(stored []byte, lastSeen int64) bool {
	var record KnownSession
	return json.Unmarshal(stored, &record) == nil && record.LastSeen >= lastSeen
}

// writeAgainWithin writes record, encoded as buf, again into the bucket watcher's handle reaches
// now, after the copy dropped its first write's result (kvwatch.WriteAgain). Records are ordered by
// their own stamp (LastSeen), so a record as recent as this one or later stands and is taken as
// read, and an older one - the record the copy held, written back by the read that found the new
// bucket - is written over.
func (s *KnownSessions) writeAgainWithin(ctx context.Context, watcher *kvwatch.Watcher, record KnownSession, buf []byte) error {
	written := s.now()
	return kvwatch.WriteAgain(ctx, watcher, record.SessionID, buf,
		func(stored []byte) bool { return seenSince(stored, record.LastSeen) },
		func(epoch, revision uint64) kvwatch.Outcome {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.records.Put(watcher, epoch, record.SessionID, kvwatch.Kept[KnownSession]{Value: record, Record: buf, Written: written}, revision)
		},
		func(read kvwatch.Entry, epoch uint64) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.records.Apply(watcher, epoch, read)
		})
}
