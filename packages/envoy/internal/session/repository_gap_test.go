package session

import "testing"

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
	if err := marker.SetRepositoryGap(ctx, known, true); err != nil {
		t.Fatalf("mark %s: %v", known, err)
	}
	if cached, err := writer.Get(known); err != nil || cached.RepositoryGap {
		t.Fatalf("UNKNOWN: the writer's copy of %s reads %+v (%v), not the entry without the mark", known, cached, err)
	}
	if err := writer.PutWithin(ctx, known, SessionEntry{MachineID: "listener-a", Title: "renewed"}); err != nil {
		t.Fatalf("renew %s over the writer's stale copy: %v", known, err)
	}
	if got := stored(known); got.Title != "renewed" || !got.RepositoryGap {
		t.Fatalf("after a write over a copy that lacks the mark %s's stored entry is %+v, want the write's title and the mark another listener set", known, got)
	}

	// The mark is cleared after the writer's last read, which carried it: the writer's next
	// write raises it no more, though the entry it writes says it is owed.
	if err := marker.SetRepositoryGap(ctx, known, false); err != nil {
		t.Fatalf("clear %s's mark: %v", known, err)
	}
	if cached, err := writer.Get(known); err != nil || !cached.RepositoryGap {
		t.Fatalf("UNKNOWN: the writer's copy of %s reads %+v (%v), not the entry with the mark", known, cached, err)
	}
	if err := writer.PutWithin(ctx, known, SessionEntry{MachineID: "listener-a", Title: "renewed again", RepositoryGap: true}); err != nil {
		t.Fatalf("renew %s over the writer's stale copy: %v", known, err)
	}
	if got := stored(known); got.Title != "renewed again" || got.RepositoryGap {
		t.Fatalf("after a write over a copy that holds a cleared mark %s's stored entry is %+v, want the write's title and no mark", known, got)
	}

	// Another listener registered and marked a session the writer's copy holds nothing of, as a
	// session that moves to the writer's listener is: the writer's first write keeps the mark.
	if err := marker.Put(unknown, SessionEntry{MachineID: "listener-b"}); err != nil {
		t.Fatalf("register %s on the other listener: %v", unknown, err)
	}
	if err := marker.SetRepositoryGap(ctx, unknown, true); err != nil {
		t.Fatalf("mark %s: %v", unknown, err)
	}
	if _, err := writer.Get(unknown); err == nil {
		t.Fatalf("UNKNOWN: the writer's copy holds %s, so its write is not made over a copy that lacks the entry", unknown)
	}
	if err := writer.PutWithin(ctx, unknown, SessionEntry{MachineID: "listener-a"}); err != nil {
		t.Fatalf("register %s on the writer's listener: %v", unknown, err)
	}
	if got := stored(unknown); got.MachineID != "listener-a" || !got.RepositoryGap {
		t.Fatalf("after the writer's first write of %s its stored entry is %+v, want the writer's machine and the mark the other listener set", unknown, got)
	}
}
