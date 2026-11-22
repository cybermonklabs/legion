package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// losingUpdateKV is a session bucket whose every conditional update loses, as to writers that keep
// landing first; every other call is the bucket's own. It counts the updates it refused, and once
// it has refused stop of them it ends the writer's context, so a write loop with no bound of its
// own ends there.
type losingUpdateKV struct {
	nats.KeyValue
	updates *atomic.Int64
	stop    int64
	cancel  context.CancelFunc
}

func (kv losingUpdateKV) Update(string, []byte, uint64) (uint64, error) {
	if kv.updates.Add(1) >= kv.stop {
		kv.cancel()
	}
	return 0, errors.New("nats: wrong last sequence: 1")
}

// A write of an entry ends once it has lost entryWriteLosses writes, whatever its context allows:
// a lost write is another writer's progress, and a run of writers that keeps landing first would
// otherwise keep the write looping at the bucket's round trip, holding a registration or a
// relay's frames, for as long as its context lasts. Both loops end so, a registration's
// (PutWithin) and a change of the stored entry's (SetRepositoryGap through rewriteStoredEntry).
// Here every update loses. Control: with the loops writing until a write lands or ctx ends
// (writeEntryAt's stop rule before this row), each write loses until the bucket ends its context,
// a thousand updates in, and the row fails.
func TestAnEntryWriteStopsAfterABoundOfLostWrites(t *testing.T) {
	client := setupNATS(t)
	registry, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	const sessionID = "ses_lost_writes"
	if err := registry.Put(sessionID, SessionEntry{MachineID: "listener-a"}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var updates atomic.Int64
	useKV(t, registry, losingUpdateKV{KeyValue: registry.watcher.KV().Legacy(), updates: &updates, stop: 1000, cancel: cancel})
	for _, write := range []struct {
		what  string
		write func() error
	}{
		{"a registration (PutWithin)", func() error { return registry.PutWithin(ctx, sessionID, SessionEntry{MachineID: "listener-a"}) }},
		{"a gap mark (SetRepositoryGap)", func() error { return registry.SetRepositoryGap(ctx, sessionID, "a-mark") }},
	} {
		updates.Store(0)
		err := write.write()
		if err == nil || ctx.Err() != nil || updates.Load() != entryWriteLosses {
			t.Fatalf("%s whose every update loses answered %v after %d updates, with its context ended: %v; want an error once it has lost %d",
				write.what, err, updates.Load(), ctx.Err() != nil, entryWriteLosses)
		}
	}
}

// staleGetKV is a session bucket whose direct get of key answers stale, an entry the bucket has
// since written over, as a copy of a replicated bucket that stays a write behind does; every other
// call is the bucket's own, a read from the stream's leader (bus.KeyValue.GetFromLeader) among
// them, which does not go through this handle.
type staleGetKV struct {
	nats.KeyValue
	key   string
	stale nats.KeyValueEntry
}

func (kv staleGetKV) Get(key string) (nats.KeyValueEntry, error) {
	if key == kv.key {
		return kv.stale, nil
	}
	return kv.KeyValue.Get(key)
}

// A write of an entry that lost to another write reads the entry again from the bucket stream's
// leader, which holds every write the bucket took, so a copy that keeps answering its direct gets
// a write behind cannot spend the write's bound of lost writes (entryWriteLosses): a
// registration (PutWithin) over a stale copy of the entry and a change of the stored entry
// (SetRepositoryGap through rewriteStoredEntry) each land at their second write. Here the
// registry's copy is held behind the bucket by stopping its watch, and its direct get answers the
// entry the bucket held before. Control: with every read after a lost write a direct get
// (readEntry before this row), each write reads the stale entry again until it has lost 32 writes,
// and the row fails.
func TestAnEntryWriteReadsTheLeaderAfterALostWrite(t *testing.T) {
	client := setupNATS(t)
	registry, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	const sessionID = "ses_stale_copy"
	if err := registry.Put(sessionID, SessionEntry{MachineID: "listener-a"}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	stale, err := registry.watcher.KV().Legacy().Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's entry: %v", sessionID, err)
	}
	useKV(t, registry, staleGetKV{KeyValue: registry.watcher.KV().Legacy(), key: sessionID, stale: stale})
	registry.StopWatch()
	bucket, err := client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := bucket.Put(sessionID, []byte(`{"machine_id":"listener-b"}`)); err != nil {
		t.Fatalf("write %s's entry from another listener: %v", sessionID, err)
	}
	for _, write := range []struct {
		what  string
		write func() error
	}{
		{"a registration over the stale copy (PutWithin)", func() error {
			return registry.PutWithin(t.Context(), sessionID, SessionEntry{MachineID: "listener-a"})
		}},
		{"a gap mark (SetRepositoryGap)", func() error { return registry.SetRepositoryGap(t.Context(), sessionID, "a-mark") }},
	} {
		if err := write.write(); err != nil {
			t.Fatalf("%s while the bucket's direct get answers a write behind: %v; want it written at the revision the leader holds", write.what, err)
		}
	}
	value, err := bucket.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's entry: %v", sessionID, err)
	}
	var entry SessionEntry
	if err := json.Unmarshal(value.Value(), &entry); err != nil || entry.MachineID != "listener-a" || entry.RepositoryGap != "a-mark" {
		t.Fatalf("%s's entry is %s (%v), want listener-a's registration with the mark a-mark", sessionID, value.Value(), err)
	}
}

// An entry write carries the gap mark of the entry it replaces (SessionEntry.RepositoryGap),
// whatever the written entry says, and never on the strength of a copy of the bucket that trails
// the bucket: a mark another listener set after this registry last read the entry stays on the
// entry this registry writes over it, also where this registry's copy holds no entry of the
// session at all, and a mark another listener cleared since stays cleared. So a registration or a
// renewal written over a stale read neither loses a mark no frame has carried nor gives the next
// frame a mark no withheld event owes. The writing registry's watch is stopped, so its copy holds
// each entry as it last wrote it, and each write is made over an entry the bucket no longer holds.
// Control: with the entry written unconditionally as given (PutWithin before this row), the first
// write loses the mark and the row fails.
func TestAnEntryWriteCarriesTheGapMarkOfTheEntryItReplaces(t *testing.T) {
	client := setupNATS(t)
	writer, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the writing registry: %v", err)
	}
	marker, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the marking registry: %v", err)
	}
	t.Cleanup(marker.StopWatch)
	ctx := t.Context()
	stored := func(sessionID string) SessionEntry {
		t.Helper()
		entry, err := marker.Refresh(ctx, sessionID)
		if err != nil {
			t.Fatalf("read %s's entry from the bucket: %v", sessionID, err)
		}
		return entry
	}
	const known, unknown = "ses_gap_known", "ses_gap_unknown"
	if err := writer.Put(known, SessionEntry{MachineID: "listener-a", Title: "registered"}); err != nil {
		t.Fatalf("register %s: %v", known, err)
	}
	writer.StopWatch()

	// The mark lands after the writer's last read of the entry.
	if err := marker.SetRepositoryGap(ctx, known, "mark-1"); err != nil {
		t.Fatalf("mark %s: %v", known, err)
	}
	if cached, err := writer.Get(known); err != nil || cached.RepositoryGap != "" {
		t.Fatalf("UNKNOWN: the writer's copy of %s reads %+v (%v), not the entry without the mark", known, cached, err)
	}
	if err := writer.PutWithin(ctx, known, SessionEntry{MachineID: "listener-a", Title: "renewed"}); err != nil {
		t.Fatalf("renew %s over the writer's stale copy: %v", known, err)
	}
	if got := stored(known); got.Title != "renewed" || got.RepositoryGap != "mark-1" {
		t.Fatalf("after a write over a copy that lacks the mark %s's stored entry is %+v, want the write's title and the mark another listener set", known, got)
	}

	// The mark is cleared after the writer's last read, which carried it: the writer's next
	// write raises it no more, though the entry it writes says it is owed.
	if err := marker.ClearRepositoryGap(ctx, known, "mark-1"); err != nil {
		t.Fatalf("clear %s's mark: %v", known, err)
	}
	if cached, err := writer.Get(known); err != nil || cached.RepositoryGap != "mark-1" {
		t.Fatalf("UNKNOWN: the writer's copy of %s reads %+v (%v), not the entry with the mark", known, cached, err)
	}
	if err := writer.PutWithin(ctx, known, SessionEntry{MachineID: "listener-a", Title: "renewed again", RepositoryGap: "mark-1"}); err != nil {
		t.Fatalf("renew %s over the writer's stale copy: %v", known, err)
	}
	if got := stored(known); got.Title != "renewed again" || got.RepositoryGap != "" {
		t.Fatalf("after a write over a copy that holds a cleared mark %s's stored entry is %+v, want the write's title and no mark", known, got)
	}

	// Another listener registered and marked a session the writer's copy holds nothing of, as a
	// session that moves to the writer's listener is: the writer's first write keeps the mark.
	if err := marker.Put(unknown, SessionEntry{MachineID: "listener-b"}); err != nil {
		t.Fatalf("register %s on the other listener: %v", unknown, err)
	}
	if err := marker.SetRepositoryGap(ctx, unknown, "mark-2"); err != nil {
		t.Fatalf("mark %s: %v", unknown, err)
	}
	if _, err := writer.Get(unknown); err == nil {
		t.Fatalf("UNKNOWN: the writer's copy holds %s, so its write is not made over a copy that lacks the entry", unknown)
	}
	if err := writer.PutWithin(ctx, unknown, SessionEntry{MachineID: "listener-a"}); err != nil {
		t.Fatalf("register %s on the writer's listener: %v", unknown, err)
	}
	if got := stored(unknown); got.MachineID != "listener-a" || got.RepositoryGap != "mark-2" {
		t.Fatalf("after the writer's first write of %s its stored entry is %+v, want the writer's machine and the mark the other listener set", unknown, got)
	}
}

// A clear of the gap mark removes only the mark it names, the one the clearing relay's frame
// carried: while two relays of one session run at once, as while it moves from one listener to
// another, a mark the other relay set since for an event it withheld later stays on the entry
// for that relay's own frame to clear, and so for the next listener to send after a move. A mark
// is set over another one, and an entry the bucket does not hold takes none: the relay then gives
// its event back. Control: with the clear writing no mark whatever mark the entry holds
// (ClearRepositoryGap before this row), the first clear removes the other relay's mark and the
// row fails.
func TestAGapMarkClearLeavesAMarkAnotherRelaySetSince(t *testing.T) {
	client := setupNATS(t)
	ctx := t.Context()
	relays := make([]*SessionRegistry, 2)
	for i := range relays {
		registry, err := OpenSessionRegistry(ctx, client.Conn, WithSessionReplicas(1))
		if err != nil {
			t.Fatalf("open relay %d's registry: %v", i, err)
		}
		t.Cleanup(registry.StopWatch)
		relays[i] = registry
	}
	first, second := relays[0], relays[1]
	stored := func() string {
		t.Helper()
		entry, err := second.Refresh(ctx, "ses_gap_handover")
		if err != nil {
			t.Fatalf("read ses_gap_handover's entry from the bucket: %v", err)
		}
		return entry.RepositoryGap
	}
	if err := first.Put("ses_gap_handover", SessionEntry{MachineID: "listener-a"}); err != nil {
		t.Fatalf("register ses_gap_handover: %v", err)
	}
	if err := first.SetRepositoryGap(ctx, "ses_gap_handover", "first-relays-mark"); err != nil {
		t.Fatalf("the first relay's mark: %v", err)
	}
	if err := second.SetRepositoryGap(ctx, "ses_gap_handover", "second-relays-mark"); err != nil {
		t.Fatalf("the second relay's mark: %v", err)
	}
	if got := stored(); got != "second-relays-mark" {
		t.Fatalf("UNKNOWN: the entry carries mark %q after the second relay's, want second-relays-mark", got)
	}
	if err := first.ClearRepositoryGap(ctx, "ses_gap_handover", "first-relays-mark"); err != nil {
		t.Fatalf("the first relay's clear: %v", err)
	}
	if got := stored(); got != "second-relays-mark" {
		t.Fatalf("after the first relay cleared the mark its frame carried the entry carries mark %q: the second relay's mark, which no frame has carried, is lost", got)
	}
	if err := second.ClearRepositoryGap(ctx, "ses_gap_handover", "second-relays-mark"); err != nil {
		t.Fatalf("the second relay's clear: %v", err)
	}
	if got := stored(); got != "" {
		t.Fatalf("after the second relay cleared its own mark the entry carries mark %q, want none", got)
	}
	err := first.SetRepositoryGap(ctx, "ses_gap_gone", "first-relays-mark")
	if !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("a mark of a session the bucket holds no entry of answered %v, want an error naming the missing entry: the relay would acknowledge the event while no entry carries its mark", err)
	}
}

// The outage renewal writes the entry back only as the bucket held it when its check read it: a
// registration another listener wrote between the renewal's read and its write is never
// overwritten with what the renewal read, and the renewal reads the entry again and asks its
// check again, which the other listener's entry fails. Here the check itself writes the other
// listener's registration the first time it is asked, which makes that interleaving certain. A
// renewal no write races renews the entry, stamped now, with its mark. Control: with the renewal
// written as the outage renewal was before this row (a read, then PutWithin over it), the other
// listener's registration is overwritten and the row fails.
func TestARenewalLeavesAnEntryWrittenSinceItsRead(t *testing.T) {
	client := setupNATS(t)
	ctx := t.Context()
	edge, err := OpenSessionRegistry(ctx, client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the edge's registry: %v", err)
	}
	t.Cleanup(edge.StopWatch)
	other, err := OpenSessionRegistry(ctx, client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the other listener's registry: %v", err)
	}
	t.Cleanup(other.StopWatch)
	const sessionID = "ses_renewal_raced"
	read := func() (SessionEntry, uint64) {
		t.Helper()
		bucket, err := client.JS().KeyValue(bus.SessionBucket)
		if err != nil {
			t.Fatalf("open the session bucket: %v", err)
		}
		value, err := bucket.Get(sessionID)
		if err != nil {
			t.Fatalf("read %s's entry: %v", sessionID, err)
		}
		var entry SessionEntry
		if err := json.Unmarshal(value.Value(), &entry); err != nil {
			t.Fatalf("decode %s's entry: %v", sessionID, err)
		}
		return entry, value.Revision()
	}
	ours := func(entry SessionEntry) bool { return entry.MachineID == "edge-a" }
	if err := edge.Put(sessionID, SessionEntry{MachineID: "edge-a", Title: "edge-a's", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register %s at the edge: %v", sessionID, err)
	}
	if err := other.SetRepositoryGap(ctx, sessionID, "a-mark"); err != nil {
		t.Fatalf("mark %s: %v", sessionID, err)
	}
	before, beforeRevision := read()
	if err := edge.RenewWithin(ctx, sessionID, ours); err != nil {
		t.Fatalf("renew %s: %v", sessionID, err)
	}
	if renewed, revision := read(); revision <= beforeRevision || renewed.Title != "edge-a's" || renewed.RepositoryGap != "a-mark" || renewed.UpdatedAt < before.UpdatedAt {
		t.Fatalf("after a renewal no write raced %s's entry is %+v at revision %d, want edge-a's, its mark, past revision %d", sessionID, renewed, revision, beforeRevision)
	}

	var asked []SessionEntry
	err = edge.RenewWithin(ctx, sessionID, func(entry SessionEntry) bool {
		asked = append(asked, entry)
		if len(asked) == 1 {
			if err := other.PutWithin(ctx, sessionID, SessionEntry{MachineID: "edge-b", Title: "edge-b's", Projects: []string{"PRIV"}}); err != nil {
				t.Errorf("register %s at the other listener: %v", sessionID, err)
			}
		}
		return ours(entry)
	})
	if err != nil {
		t.Fatalf("renew %s while the other listener registers it: %v", sessionID, err)
	}
	if got, _ := read(); got.MachineID != "edge-b" || got.Title != "edge-b's" {
		t.Fatalf("after the renewal %s's entry is %+v: the other listener's registration, written after the renewal's read, is overwritten with the entry it read", sessionID, got)
	}
	if len(asked) != 2 || asked[1].MachineID != "edge-b" {
		t.Fatalf("the renewal asked its check of %d entries (%+v), want two: the entry it first read, then the other listener's, read again after its write lost", len(asked), asked)
	}
}
