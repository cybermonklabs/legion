package session

// The session registry's part in a bucket deleted and created again (INFRA-4094): the rows its
// copy held, written back into the new bucket from the read that finds it (beginRead, reassert),
// and the write whose own result the copy dropped because the bucket was recreated while it was in
// flight, written again into the bucket the handle reaches now (putAgainWithin). kvwatch.Reassert
// says why an ack cannot name the incarnation that took it.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// beginRead starts a read of the session bucket (kvwatch.Copy.BeginRead) with the bucket's MaxAge
// as the ttl when it has one, so a bucket recreated with another TTL expires its rows by its own.
// Every row stays. For the first read of a recreated bucket it answers the rows to write back into
// it (reassert): each row the copy holds, as its record, but for two the new bucket should not hold
// again. A row past its deadline is one the bucket's MaxAge would already have dropped, and writing
// it back would give it another MaxAge. A gone session's row (StateOf) is one the session reaper
// deletes at its next pass, so it is not brought back for that pass to delete again; every listener
// judges it on the same windows, so none brings it back. A session that ended after the recreate is
// not in the copy's way either: its delete marked the key in the new bucket (DeleteAtRead), which
// refuses the write.
func (r *SessionRegistry) beginRead(read kvwatch.Read) []kvwatch.Held[*cachedSession] {
	r.mu.Lock()
	defer r.unlockAndNotify()
	r.cache.BeginRead(read)
	if read.MaxAge > 0 {
		r.ttl = read.MaxAge
	}
	if !read.Recreated {
		return nil
	}
	now := r.now()
	var held []kvwatch.Held[*cachedSession]
	for sessionID, row := range r.cache.All() {
		if row.expired(now) || r.stateOf(row.entry, now) == Gone {
			continue
		}
		record, err := json.Marshal(row.entry)
		if err != nil {
			continue
		}
		held = append(held, kvwatch.Held[*cachedSession]{Key: sessionID, Record: record, Value: row})
	}
	return held
}

// reassert writes held, the rows beginRead answered for read, into the recreated bucket
// (kvwatch.Reassert), each only where that bucket holds nothing for its key, and stores each one
// it took in the copy at read's epoch, so the read's sweep keeps it. It runs in the read's
// BeginRead with r.mu released, and takes r.mu for each row it stores.
func (r *SessionRegistry) reassert(read kvwatch.Read, held []kvwatch.Held[*cachedSession]) {
	kvwatch.Reassert(r.watcher, read, held, func(h kvwatch.Held[*cachedSession], revision uint64) {
		r.mu.Lock()
		r.cache.Put(r.watcher, read.Epoch, h.Key, h.Value, revision)
		r.unlockAndNotify()
	})
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
		func(stored []byte) bool {
			var row SessionEntry
			return json.Unmarshal(stored, &row) == nil && row.UpdatedAt >= entry.UpdatedAt
		},
		func(epoch, revision uint64) kvwatch.Outcome {
			r.mu.Lock()
			defer r.unlockAndNotify()
			return r.cache.Put(r.watcher, epoch, sessionID, &cachedSession{entry: entry, expiresAt: r.expiryFor(time.Time{}, entry.UpdatedAt)}, revision)
		},
		func(read kvwatch.Entry, epoch uint64) {
			r.mu.Lock()
			defer r.unlockAndNotify()
			r.cache.Apply(r.watcher, epoch, read)
		})
}

// beginRead starts a read of the known-session bucket (kvwatch.Copy.BeginRead); every record stays
// through it. For the first read of a recreated bucket it answers the records to write back into
// it (reassert): each record the copy holds but one aged out of KnownSessionRetention, which the
// bucket's TTL would already have dropped and the sweep drops from the copy, and which a write
// would give another 30 days. The bucket has no delete path, so no other record ends while the
// bucket is away.
func (s *KnownSessions) beginRead(read kvwatch.Read) []kvwatch.Held[KnownSession] {
	cutoff := s.cutoff()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records.BeginRead(read)
	if !read.Recreated {
		return nil
	}
	var held []kvwatch.Held[KnownSession]
	for sessionID, record := range s.records.All() {
		if record.LastSeen < cutoff {
			continue
		}
		buf, err := json.Marshal(record)
		if err != nil {
			continue
		}
		held = append(held, kvwatch.Held[KnownSession]{Key: sessionID, Record: buf, Value: record})
	}
	return held
}

// reassert writes held, the records beginRead answered for read, into the recreated bucket
// (kvwatch.Reassert), each only where that bucket holds nothing for its key, and stores each one it
// took in the copy at read's epoch, so the read's sweep keeps it. It runs in the read's BeginRead
// with s.mu released.
func (s *KnownSessions) reassert(read kvwatch.Read, held []kvwatch.Held[KnownSession]) {
	watcher := s.watch()
	if watcher == nil {
		return
	}
	kvwatch.Reassert(watcher, read, held, func(h kvwatch.Held[KnownSession], revision uint64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.records.Put(watcher, read.Epoch, h.Key, h.Value, revision)
	})
}

// writeAgainWithin writes record, encoded as buf, again into the bucket watcher's handle reaches
// now, after the copy dropped its first write's result (kvwatch.WriteAgain). Records are ordered by
// their own stamp (LastSeen), so a record as recent as this one or later stands and is taken as
// read, and an older one - the record the copy held, written back by the read that found the new
// bucket - is written over.
func (s *KnownSessions) writeAgainWithin(ctx context.Context, watcher *kvwatch.Watcher, record KnownSession, buf []byte) error {
	return kvwatch.WriteAgain(ctx, watcher, record.SessionID, buf,
		func(stored []byte) bool {
			var held KnownSession
			return json.Unmarshal(stored, &held) == nil && held.LastSeen >= record.LastSeen
		},
		func(epoch, revision uint64) kvwatch.Outcome {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.records.Put(watcher, epoch, record.SessionID, record, revision)
		},
		func(read kvwatch.Entry, epoch uint64) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.records.Apply(watcher, epoch, read)
		})
}
