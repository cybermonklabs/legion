package session

import "testing"

// An entry write carries the held marker of the entry it replaces (SessionEntry.Held), whatever the
// written entry says, as it carries the gap mark (TestAnEntryWriteCarriesTheGapMarkOfTheEntryItReplaces):
// SetHeld is the marker's one writer, so a registration or a renewal written over a stale read
// neither loses a marker whose held frames a restart would lose nor raises one no hold set. The
// writing registry's watch is stopped, so its copy holds each entry as it last wrote it. Control:
// with Held left out of PutWithin's carry (the copy's, and the bucket's on a lost write), the first
// write loses the marker and the row fails.
func TestAnEntryWriteCarriesTheHeldMarkerOfTheEntryItReplaces(t *testing.T) {
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
	const known, unknown = "ses_held_known", "ses_held_unknown"
	if err := writer.Put(known, SessionEntry{MachineID: "listener-a", Title: "registered", Held: true}); err != nil {
		t.Fatalf("register %s: %v", known, err)
	}
	if got := stored(known); got.Held {
		t.Fatalf("a first write naming Held stored %+v, want no marker: SetHeld alone raises it", got)
	}
	writer.StopWatch()

	// The marker lands after the writer's last read of the entry.
	if err := marker.SetHeld(ctx, known, true); err != nil {
		t.Fatalf("raise %s's held marker: %v", known, err)
	}
	if cached, err := writer.Get(known); err != nil || cached.Held {
		t.Fatalf("UNKNOWN: the writer's copy of %s reads %+v (%v), not the entry without the marker", known, cached, err)
	}
	if err := writer.PutWithin(ctx, known, SessionEntry{MachineID: "listener-a", Title: "renewed"}); err != nil {
		t.Fatalf("renew %s over the writer's stale copy: %v", known, err)
	}
	if got := stored(known); got.Title != "renewed" || !got.Held {
		t.Fatalf("after a write over a copy that lacks the marker %s's stored entry is %+v, want the write's title and the marker another listener set", known, got)
	}

	// The marker is cleared after the writer's last read, which carried it: the writer's next
	// write raises it no more, though the entry it writes names it.
	if err := marker.SetHeld(ctx, known, false); err != nil {
		t.Fatalf("clear %s's held marker: %v", known, err)
	}
	if cached, err := writer.Get(known); err != nil || !cached.Held {
		t.Fatalf("UNKNOWN: the writer's copy of %s reads %+v (%v), not the entry with the marker", known, cached, err)
	}
	if err := writer.PutWithin(ctx, known, SessionEntry{MachineID: "listener-a", Title: "renewed again", Held: true}); err != nil {
		t.Fatalf("renew %s over the writer's stale copy: %v", known, err)
	}
	if got := stored(known); got.Title != "renewed again" || got.Held {
		t.Fatalf("after a write over a copy that holds a cleared marker %s's stored entry is %+v, want the write's title and no marker", known, got)
	}

	// Another listener registered the session and held frames for it, and the writer's copy holds
	// nothing of it, as for a session that moves to the writer's listener: the writer's first
	// write keeps the marker.
	if err := marker.Put(unknown, SessionEntry{MachineID: "listener-b"}); err != nil {
		t.Fatalf("register %s on the other listener: %v", unknown, err)
	}
	if err := marker.SetHeld(ctx, unknown, true); err != nil {
		t.Fatalf("raise %s's held marker: %v", unknown, err)
	}
	if _, err := writer.Get(unknown); err == nil {
		t.Fatalf("UNKNOWN: the writer's copy holds %s, so its write is not made over a copy that lacks the entry", unknown)
	}
	if err := writer.PutWithin(ctx, unknown, SessionEntry{MachineID: "listener-a"}); err != nil {
		t.Fatalf("register %s on the writer's listener: %v", unknown, err)
	}
	if got := stored(unknown); got.MachineID != "listener-a" || !got.Held {
		t.Fatalf("after the writer's first write of %s its stored entry is %+v, want the writer's machine and the marker the other listener set", unknown, got)
	}
}
