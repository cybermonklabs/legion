package outbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
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
			itemRef: "dispatch://T-1/artifact/" + slug, documentID: uuid.NewString(),
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

// A document kept from before the creation bound on names can have a reference longer than one
// block takes. Its event's item names the document by its id and the reference's length instead,
// so the window's message is still one the body check takes. With the reference cap turned off,
// the one item is a block over the bound.
func TestADocumentWithAnOversizedReferenceIsNamedByItsID(t *testing.T) {
	document := uuid.NewString()
	window := undeliveredWindow{issueKey: "T-1", role: "legacy-document", openedAt: time.Now(), notes: []undeliveredNote{{
		eventID: 300000, eventType: "artifact.version", keeps: maxRoleKeeps, reason: model.RoleUndeliveredExpired,
		itemRef: "dispatch://T-1/artifact/" + strings.Repeat("a", 40000), documentID: document,
	}}}
	body := undeliveredMessage(window)
	if err := itemblocks.Check("body", body); err != nil {
		t.Fatalf("the message about a document with a 40,000-character reference is refused by the body check: %v", err)
	}
	if !strings.Contains(body, "event 300000 (artifact.version), on the document with id "+document) {
		t.Fatalf("the message does not name the document by its id:\n%.400s", body)
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

// An event whose copies all cannot be kept again - the stream no longer holds any copy its owed
// keeps name - ends as an event recorded undelivered at the bound does: one role.undelivered
// record, however many such copies one pass finds, and none of its rows left owed. An event one of
// whose copies cannot be kept again while another is still owed a keep is not ended: that row is
// settled carried and the other stays owed. With the carried outcome taken away, each such copy
// appends a record of its own and ends an event another copy still carries.
func TestAnEventWhoseCopiesAreAllGoneIsRecordedOnce(t *testing.T) {
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
	for _, row := range expiryRows(t, rig.database, both.ID, role) {
		if !row.settled {
			t.Fatalf("event %d's expiry of attempt %d is still owed after the event was recorded undelivered: %+v", both.ID, row.attempt, row)
		}
	}
	if records := undeliveredCount(t, rig.database, one.ID); records != 0 {
		t.Fatalf("event %d, one of whose copies is still owed a keep, has %d role.undelivered records, want none", one.ID, records)
	}
	rows := expiryRows(t, rig.database, one.ID, role)
	if len(rows) != 2 || rows[0].outcome == nil || *rows[0].outcome != expiryCarried || rows[1].settled {
		t.Fatalf("event %d's expiries are %+v, want attempt 1 carried and attempt 2 still owed", one.ID, rows)
	}
	if after := keptLane(t, rig.client, role); len(after) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's two", role, len(after))
	}
}

// A copy that cannot be kept again does not end an event a sibling copy delivers in the same pass:
// with a session holding the role, the sibling is forwarded to it, and with none, the sibling is
// kept again. In both, the row is settled carried and no role.undelivered record is appended. With
// the carried outcome taken away, the row ends the event after its sibling reached the holder, or
// while its new copy still has its hour.
func TestAGoneCopyDoesNotEndAnEventItsSiblingDelivers(t *testing.T) {
	for _, held := range []bool{true, false} {
		t.Run(fmt.Sprintf("held %v", held), func(t *testing.T) {
			ctx := context.Background()
			rig := newRoleRig(t)
			role := fmt.Sprintf("expiry-sibling-%v", held)
			rig.silent(t, contracts.RoleTopicPrefix+role)
			event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
			outbox := holding(t, rig.deps())
			outbox.scan(ctx, unfenced{})
			lane := keptLane(t, rig.client, role)
			if len(lane) != 1 {
				t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
			}
			const holder = "ses_expiry_sibling_holder"
			recv := rig.receiver(t, holder)
			if held {
				rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
			}
			owedExpiry(t, rig.database, event.ID, role, 1, 999994, nil)
			owedExpiry(t, rig.database, event.ID, role, 2, lane[0].Sequence, nil)

			outbox.passKeptExpiries(ctx, unfenced{})

			if records := undeliveredCount(t, rig.database, event.ID); records != 0 {
				t.Fatalf("event %d has %d role.undelivered records though its sibling copy carried it on, want none", event.ID, records)
			}
			rows := expiryRows(t, rig.database, event.ID, role)
			want := expiryKept
			if held {
				want = expiryForwarded
			}
			if len(rows) != 2 || rows[0].outcome == nil || *rows[0].outcome != expiryCarried || rows[1].outcome == nil || *rows[1].outcome != want {
				t.Fatalf("event %d's expiries are %+v, want attempt 1 carried and attempt 2 %s", event.ID, rows, want)
			}
			if held {
				if forward := handedNow(t, recv, "the sibling's forward"); forward.EventID != fmt.Sprintf("dispatch-%d", event.ID) {
					t.Fatalf("the holder took event %q, want dispatch-%d", forward.EventID, event.ID)
				}
			} else if after := keptLane(t, rig.client, role); len(after) != 2 {
				t.Fatalf("role %s's kept lane holds %d copies, want the sibling kept again", role, len(after))
			}
		})
	}
}
