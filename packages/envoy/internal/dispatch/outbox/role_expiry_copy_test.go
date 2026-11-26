package outbox

import (
	"context"
	"encoding/json"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A notice that does not name its event's copy is passed over, and only the cursor moves past it:
// one whose sequence is where the role's kept lane holds another event's copy, one whose sequence
// is where the stream holds a message of another subject - the issue topic the scan published the
// event to - and one whose sequence is not below its own, which no listener stores. The real
// notice of the other event's copy, read in the same pass, is recorded and kept again, so the
// check takes a copy that is the notice's. With any one of the three checks taken away
// (noticedCopy), its notice is recorded for the first event.
func TestANoticeThatNamesNoCopyOfItsEventIsPassedOver(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-notice-copy"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	first := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	second := rig.routedEvent(t, "T-2", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	firstCopy, secondCopy := newestCopyOf(t, rig, role, first.ID), newestCopyOf(t, rig, role, second.ID)
	published, err := rig.client.NextStoredMessage(ctx, "notifications.dispatch.>", 1)
	if err != nil {
		t.Fatalf("read the issue topic message the scan published: %v", err)
	}

	// Each forged notice is stored under an id of its own: one under the id of the copy it names
	// (bus.KeptExpiryMsgID) would take the place of that copy's real notice in the stream's
	// duplicate window.
	for _, forged := range []struct {
		what     string
		sequence uint64
	}{
		{"another event's copy", secondCopy.Sequence},
		{"a message of another subject", published.Sequence},
		{"a sequence not below its own", 1 << 40},
	} {
		notice := expiryNoticeOf(t, role, firstCopy)
		notice.Sequence = forged.sequence
		data, err := json.Marshal(notice)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rig.client.JS().Publish(bus.KeptExpiredSubject(role), data, natsgo.MsgId("forged: "+forged.what)); err != nil {
			t.Fatalf("store the notice naming %s: %v", forged.what, err)
		}
	}
	expire(t, rig.client, role, secondCopy)
	last, err := rig.client.LastStoredMessage(ctx, bus.KeptExpiredSubject(role))
	if err != nil {
		t.Fatalf("read the last notice back: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	if rows := expiryRows(t, rig.database, first.ID, role); len(rows) != 0 {
		t.Fatalf("the outbox recorded %+v for event %d, whose copy no notice named, want nothing", rows, first.ID)
	}
	if rows := expiryRows(t, rig.database, second.ID, role); len(rows) != 1 || rows[0].outcome == nil || *rows[0].outcome != expiryKept {
		t.Fatalf("the outbox recorded %+v for event %d, want its one real notice kept again", rows, second.ID)
	}
	if cursor := expiryCursor(t, rig.database); cursor != int64(last.Sequence) {
		t.Fatalf("the expiry cursor is %d, want it at the last notice's sequence %d", cursor, last.Sequence)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 3 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's two and the second event's keep again", role, len(lane))
	}
}

// An expired copy whose sequence holds another copy when the expiry pass reads it back - a stream
// created again between the notice's record and the read, whose copy stands at the old sequence -
// is gone: the pair ends recorded undelivered with the reason copy_gone, and the other copy is
// neither kept again nor forwarded under this event. The row is planted as the first copy's notice
// recorded it, with the second copy's sequence in place of its own. With the read's check taken
// away (readExpiredCopy), the second event's copy is kept again under the first one's row.
func TestAnExpiredCopyWhoseSequenceHoldsAnotherCopyIsGone(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-copy-replaced"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	first := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	second := rig.routedEvent(t, "T-2", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	firstCopy, secondCopy := newestCopyOf(t, rig, role, first.ID), newestCopyOf(t, rig, role, second.ID)
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into outbox_role_copy_expiries (event_id, role, keep_attempt, copy_sequence, dedupe_key, kept_at, dropped_at)
		values ($1, $2, 1, $3, $4, now() - interval '1 hour', now())
	`, first.ID, role, int64(secondCopy.Sequence), envelopeOf(t, firstCopy).DedupeKey); err != nil {
		t.Fatalf("record event %d's expiry at the second copy's sequence: %v", first.ID, err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	if rows := expiryRows(t, rig.database, first.ID, role); len(rows) != 1 || rows[0].outcome == nil || *rows[0].outcome != expiryUndelivered {
		t.Fatalf("event %d's expiries are %+v, want its one row settled undelivered", first.ID, rows)
	}
	records := undeliveredRecords(t, rig.database, "T-1")
	if len(records) != 1 || records[0].Payload.(model.RoleUndeliveredEventPayload).Reason != model.RoleUndeliveredCopyGone {
		t.Fatalf("T-1 holds role.undelivered records %+v, want one with the reason %s", records, model.RoleUndeliveredCopyGone)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies (last id %q), want the scan's two and no keep again", role, len(lane), lastMsgID(lane))
	}
	if rows := expiryRows(t, rig.database, second.ID, role); len(rows) != 0 {
		t.Fatalf("the outbox recorded %+v for event %d, want nothing", rows, second.ID)
	}
}

// A cursor that names another stream than the one the notices are read from - one created again
// since, which numbers its notices from its own first message - is set back to this stream's
// start: the notice this stream holds below the old cursor is recorded and kept again, and the
// cursor then stands at that notice and names this stream. With the identity check taken away
// (readKeptExpiries), the read starts past the old cursor and the notice is never recorded.
func TestACursorOfAnotherStreamIsSetBackToThisStreamsStart(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-stream-again"
	outbox := holding(t, rig.deps())
	event := expiredOnce(t, rig, outbox, role)
	notice, err := rig.client.LastStoredMessage(ctx, bus.KeptExpiredSubject(role))
	if err != nil {
		t.Fatalf("read the notice back: %v", err)
	}
	streamID, err := rig.client.StreamID(ctx)
	if err != nil || streamID == "" {
		t.Fatalf("the stream's identity is %q (%v), want one", streamID, err)
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into outbox_kept_expiry_cursor (id, sequence, stream_id) values (true, $1, 'a-stream-before-this-one')
	`, int64(notice.Sequence)+1000); err != nil {
		t.Fatalf("plant the cursor of another stream: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	if rows := expiryRows(t, rig.database, event.ID, role); len(rows) != 1 || rows[0].outcome == nil || *rows[0].outcome != expiryKept {
		t.Fatalf("event %d's expiries are %+v, want the notice below the old cursor recorded and kept again", event.ID, rows)
	}
	var cursor int64
	var cursorStream string
	if err := rig.database.Pool.QueryRow(ctx, `select sequence, stream_id from outbox_kept_expiry_cursor`).Scan(&cursor, &cursorStream); err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	if cursor != int64(notice.Sequence) || cursorStream != streamID {
		t.Fatalf("the cursor is %d in stream %q, want %d in stream %q", cursor, cursorStream, notice.Sequence, streamID)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 2 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one and the keep again", role, len(lane))
	}
}
