package outbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A window of events about a document whose reference is long - a document slug may run to
// thousands of characters - makes one message the issue message writer's own body check takes:
// its lists are bounded by their length, not only by how many events they name. At the head
// before this rule 50 such events are one list over the block bound.
func TestAWindowOfLongDocumentReferencesMakesOneMessageTheBodyCheckTakes(t *testing.T) {
	window := undeliveredWindow{issueKey: "T-1", role: "long-documents", openedAt: time.Now()}
	slug := strings.Repeat("a", 2600)
	for event := range 120 {
		window.notes = append(window.notes, undeliveredNote{
			eventID: int64(200000 + event), eventType: "artifact.version", keeps: maxRoleKeeps, reason: model.RoleUndeliveredExpired,
			itemRef: "dispatch://T-1/artifact/" + slug, document: true,
		})
	}
	body := undeliveredMessage(window)
	if err := itemblocks.Check("body", body); err != nil {
		t.Fatalf("the message of a window of 120 events about a long document reference is refused by the body check: %v", err)
	}
	for _, note := range window.notes {
		if !strings.Contains(body, fmt.Sprintf("event %d (", note.eventID)) {
			t.Fatalf("the message does not name event %d", note.eventID)
		}
	}
}

// A note that joins a window after the window's message was posted - which a writer the hold has
// left could make before the hold serialized it - is posted in a message of its own, naming it,
// rather than marked posted under the message that does not name it. At the head before this rule
// the second post answers the first message and the note is marked posted unseen.
func TestANoteThatJoinsAPostedWindowGetsAMessageOfItsOwn(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "undelivered-late-note"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.withIssueMessages(t, rig.deps()))
	outbox.scan(ctx, unfenced{})
	expireToTheBound(t, rig, outbox, role, event.ID)
	endWindows(t, rig.database, role)
	outbox.passKeptExpiries(ctx, unfenced{})
	if posted := systemMessages(t, rig.database, "T-1"); len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages once the window ended, want one", len(posted))
	}
	const late = 987654
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into outbox_role_undelivered_notes (event_id, role, issue_key, event_type, keeps, reason, window_opened_at)
		select $1, role, issue_key, 'comment.created', 3, 'expired', window_opened_at
		from outbox_role_undelivered_notes where event_id = $2 and role = $3
	`, late, event.ID, role); err != nil {
		t.Fatalf("add a late note to the posted window: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	posted := systemMessages(t, rig.database, "T-1")
	if len(posted) != 2 {
		t.Fatalf("T-1 holds %d system messages after a note joined the posted window, want a second naming it", len(posted))
	}
	if !strings.Contains(posted[1].body, fmt.Sprintf("event %d (", late)) {
		t.Fatalf("the second message does not name the late event:\n%s", posted[1].body)
	}
}

// owedExpiry writes an expiry of eventID's copy for role kept for the attempt'th time and stored
// at sequence, still owed a keep, due at next (nil: due now), as the intake records one.
func owedExpiry(t *testing.T, database *store.Store, eventID int64, role string, attempt int, sequence uint64, next *time.Time) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into outbox_role_copy_expiries (event_id, role, keep_attempt, copy_sequence, dedupe_key, kept_at, dropped_at, next_attempt_at)
		values ($1, $2, $3, $4, $5, now() - interval '2 hours', now() - interval '1 hour', $6)
	`, eventID, role, attempt, int64(sequence), fmt.Sprintf("dispatch-%d", eventID), next); err != nil {
		t.Fatalf("write an owed expiry of event %d: %v", eventID, err)
	}
}

// undeliveredCount is how many role.undelivered records name eventID.
func undeliveredCount(t *testing.T, database *store.Store, eventID int64) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = $1 and (payload ->> 'event_id')::bigint = $2
	`, model.RoleUndeliveredEventType, eventID).Scan(&count); err != nil {
		t.Fatalf("count the role.undelivered records of event %d: %v", eventID, err)
	}
	return count
}

// An event whose copy the stream no longer holds ends as an event recorded undelivered at the
// bound does: one role.undelivered record, however many of its owed keeps find their copy gone in
// one pass, and every other owed keep of it settled, so none is left owed and never claimed. At
// the head before this rule each gone copy appends a record of its own, and an owed keep that was
// not claimed in that pass stays owed for good.
func TestAnEventWhoseCopiesAreGoneIsRecordedOnceAndKeepsNothingOwed(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-copies-gone"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	alice := model.Actor{Kind: "user", ID: "alice"}
	both := rig.routedEvent(t, "T-1", role, alice)
	one := rig.laterEvent(t, "T-1", alice)
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's two", role, len(lane))
	}
	later := time.Now().Add(time.Hour)
	owedExpiry(t, rig.database, both.ID, role, 1, 999991, nil)
	owedExpiry(t, rig.database, both.ID, role, 2, 999992, nil)
	owedExpiry(t, rig.database, one.ID, role, 1, 999993, nil)
	owedExpiry(t, rig.database, one.ID, role, 2, lane[1].Sequence, &later)

	outbox.passKeptExpiries(ctx, unfenced{})

	if records := undeliveredCount(t, rig.database, both.ID); records != 1 {
		t.Fatalf("event %d, whose two owed copies are both gone, has %d role.undelivered records, want one", both.ID, records)
	}
	if records := undeliveredCount(t, rig.database, one.ID); records != 1 {
		t.Fatalf("event %d, one of whose copies is gone, has %d role.undelivered records, want one", one.ID, records)
	}
	for _, eventID := range []int64{both.ID, one.ID} {
		for _, row := range expiryRows(t, rig.database, eventID, role) {
			if !row.settled {
				t.Fatalf("event %d's expiry of attempt %d is still owed after the event was recorded undelivered: %+v", eventID, row.attempt, row)
			}
		}
	}
	if after := keptLane(t, rig.client, role); len(after) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's two and no keep after the records", role, len(after))
	}
}
