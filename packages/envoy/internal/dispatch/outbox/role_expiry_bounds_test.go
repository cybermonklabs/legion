package outbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An event whose expiries reach the bound by two copies at once - the copy kept again and a
// takeover's copy of it, whose message id names no attempt - stops at that bound: the expiry that
// records it undelivered settles the event's earlier expiry that still owed a keep, so the pass
// after it keeps nothing more, and the role's kept lane never holds a copy kept after the record.
// With endRoleDelivery's settle of the pair's owed rows taken away, the earlier row stays owed and
// the pass keeps the event a fourth time.
func TestTheExpiryThatRecordsAnEventUndeliveredSettlesItsEarlierOwedKeeps(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-bound-two-copies"
	outbox := holding(t, rig.deps())
	event := expiredOnce(t, rig, outbox, role)
	outbox.passKeptExpiries(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies, want the copy kept again", role, len(lane))
	}
	taken := envelopeOf(t, lane[1])
	taken.DedupeKey = "agent.takeover.fedcba9876543210fedcba9876543210"
	if err := rig.client.PublishStoredAs(ctx, bus.KeptRoleSubject(role), taken, "takeover-copy"); err != nil {
		t.Fatalf("store the takeover's copy: %v", err)
	}
	lane = keptLane(t, rig.client, role)
	expire(t, rig.client, role, lane[1])
	expire(t, rig.client, role, lane[2])

	outbox.passKeptExpiries(ctx, unfenced{})
	outbox.passKeptExpiries(ctx, unfenced{})

	if after := keptLane(t, rig.client, role); len(after) != 3 {
		t.Fatalf("role %s's kept lane holds %d copies, want the 3 it held when the event was recorded undelivered", role, len(after))
	}
	for _, row := range expiryRows(t, rig.database, event.ID, role) {
		if !row.settled {
			t.Fatalf("the expiry of attempt %d is still owed a keep after the event was recorded undelivered: %+v", row.attempt, row)
		}
	}
	if records := undeliveredRecords(t, rig.database, "T-1"); len(records) != 1 {
		t.Fatalf("T-1 holds %d role.undelivered events, want one", len(records))
	}
}

// A window of many events makes one message that the issue message writer's own check of a body
// takes: no single block of it is over the block bound, so the message is posted rather than
// refused on every try. It names every event. With the lists' bound taken away, the events are one
// list, one block over the bound, and the window's message is never posted.
func TestAWindowOfManyEventsMakesOneMessageTheBodyCheckTakes(t *testing.T) {
	window := undeliveredWindow{issueKey: "T-1", role: "busy-role", openedAt: time.Now()}
	for event := range 1000 {
		window.notes = append(window.notes, undeliveredNote{
			eventID: int64(100000 + event), eventType: "message.created", keeps: maxRoleKeeps, reason: model.RoleUndeliveredExpired,
			itemRef: "dispatch://T-1/message/" + uuid.NewString(),
		})
	}
	body := undeliveredMessage(window)
	if err := itemblocks.Check("body", body); err != nil {
		t.Fatalf("the message of a window of 1000 events is refused by the body check: %v", err)
	}
	for _, note := range window.notes {
		if !strings.Contains(body, fmt.Sprintf("event %d (", note.eventID)) {
			t.Fatalf("the message does not name event %d", note.eventID)
		}
	}
}

// A window whose post was made but whose mark never committed - the process died between the two -
// is posted again by the next pass, and that post finds the message the first one wrote: the issue
// holds one message for the window. With the window's own message id taken away, the second post
// writes a second message.
func TestAWindowPostedAgainAfterItsMarkWasLostHoldsOneMessage(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "undelivered-mark-lost"
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
	if _, err := rig.database.Pool.Exec(ctx, `
		update outbox_role_undelivered_notes set posted_at = null, message_id = null where role = $1
	`, role); err != nil {
		t.Fatalf("undo the window's mark: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	if posted := systemMessages(t, rig.database, "T-1"); len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages after the window was posted again, want the one", len(posted))
	}
	var unposted int
	if err := rig.database.Pool.QueryRow(ctx, `
		select count(*) from outbox_role_undelivered_notes where role = $1 and posted_at is null
	`, role).Scan(&unposted); err != nil || unposted != 0 {
		t.Fatalf("%d notes of the window are unposted after the second post (%v), want none", unposted, err)
	}
}

// An outbox whose process the publisher hold does not name records no expiry notice, moves no
// cursor, claims no expiry owed a keep and posts no window: its pass writes nothing, and the
// holder's pass after it does the work once. With the hold checks taken away, the other process
// records the notice and keeps the copy again.
func TestAnOutboxTheHoldDoesNotNameWritesNothingOfTheExpiries(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-not-the-holder"
	deps := rig.withIssueMessages(t, rig.deps())
	holder := holding(t, deps)
	other := New(deps)
	event := expiredOnce(t, rig, holder, role)

	other.passKeptExpiries(ctx, unfenced{})

	if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 0 {
		t.Fatalf("the process the hold does not name recorded %+v, want nothing", rows)
	}
	if cursor := expiryCursor(t, rig.database); cursor != 0 {
		t.Fatalf("the expiry cursor is %d after the other process's pass, want 0", cursor)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies after the other process's pass, want 1", role, len(lane))
	}

	holder.passKeptExpiries(ctx, unfenced{})
	if lane := keptLane(t, rig.client, role); len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies after the holder's pass, want the copy kept again", role, len(lane))
	}
	if _, err := rig.database.Pool.Exec(ctx, `update outbox_role_copy_expiries set next_attempt_at = null`); err != nil {
		t.Fatal(err)
	}
	expire(t, rig.client, role, keptLane(t, rig.client, role)[1])
	holder.passKeptExpiries(ctx, unfenced{})
	expire(t, rig.client, role, keptLane(t, rig.client, role)[2])
	holder.passKeptExpiries(ctx, unfenced{})
	endWindows(t, rig.database, role)
	other.passKeptExpiries(ctx, unfenced{})
	if posted := systemMessages(t, rig.database, "T-1"); len(posted) != 0 {
		t.Fatalf("the process the hold does not name posted %d messages, want none", len(posted))
	}
	holder.passKeptExpiries(ctx, unfenced{})
	if posted := systemMessages(t, rig.database, "T-1"); len(posted) != 1 {
		t.Fatalf("T-1 holds %d system messages after the holder's pass, want one", len(posted))
	}
}
