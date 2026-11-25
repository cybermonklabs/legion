package main

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The line and the counter a walk's expiry notice writes, by the text and the harness's series
// name for the listener's envoy_role_kept_expiry_notices_total, so a test reads what an operator
// reads.
const (
	expiryNoticeLine          = "listener told Dispatch a kept role message ended"
	expiryNoticesStored       = `test_role_kept_expiry_notices{result="stored"}`
	expiryNoticeDispatchLabel = "dispatch_outbox"
)

// storedNotice is one expiry notice as the stream holds it: the stored message and its body.
type storedNotice struct {
	stored bus.StoredMessage
	notice bus.KeptCopyExpiry
}

// expiryNotices is every expiry notice the stream holds for role, in stream order.
func (r *hourRig) expiryNotices(role string) []storedNotice {
	r.t.Helper()
	var found []storedNotice
	for _, stored := range r.stored(bus.KeptExpiredSubject(role)) {
		held := storedNotice{stored: stored}
		if err := json.Unmarshal(stored.Data, &held.notice); err != nil {
			r.t.Fatalf("decode the expiry notice stored at %d: %v", stored.Sequence, err)
		}
		found = append(found, held)
	}
	return found
}

// keepAttempt writes item to role's kept lane as Dispatch's outbox writes the copy it keeps for
// the attempt'th time, under bus.KeptAttemptMsgID, and returns the copy the stream holds.
func (r *hourRig) keepAttempt(role string, item contracts.Envelope, attempt int) laneCopy {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.client().PublishStoredAs(ctx, bus.KeptRoleSubject(role), item, bus.KeptAttemptMsgID(item.DedupeKey, role, attempt)); err != nil {
		r.t.Fatalf("write attempt %d of event %s to role %s's kept lane: %v", attempt, item.EventID, role, err)
	}
	return r.copyOf(role, item.EventID)
}

// assertNoticeOf fails unless notice is the expiry notice of held, kept for the attempt'th time
// on role's kept lane and dropped by a walk between droppedAfter and droppedBy.
func assertNoticeOf(t *testing.T, notice storedNotice, role string, held laneCopy, attempt int, droppedAfter, droppedBy time.Time) {
	t.Helper()
	got := notice.notice
	if got.EventID != held.item.EventID || got.SourceEventID != held.item.SourceEventID || got.Role != role ||
		got.Attempt != attempt || got.DedupeKey != held.item.DedupeKey || got.Sequence != held.sequence {
		t.Fatalf("the expiry notice is %+v, want event %s (source event %s) on role %s, attempt %d, key %s, copy %d",
			got, held.item.EventID, held.item.SourceEventID, role, attempt, held.item.DedupeKey, held.sequence)
	}
	if got.KeptAt != held.stored.UnixMilli() {
		t.Fatalf("the expiry notice says the copy was kept at %d, want the stream's store time %d", got.KeptAt, held.stored.UnixMilli())
	}
	if dropped := time.UnixMilli(got.DroppedAt); dropped.Before(droppedAfter.Truncate(time.Millisecond)) || dropped.After(droppedBy) {
		t.Fatalf("the expiry notice says the copy was dropped at %s, want between %s and %s", dropped, droppedAfter, droppedBy)
	}
	if want := "envoy.kept-expired." + role + "." + strconv.FormatUint(held.sequence, 10); notice.stored.MsgID != want {
		t.Fatalf("the expiry notice is stored under message id %q, want %q", notice.stored.MsgID, want)
	}
}

// A copy Dispatch's outbox kept on a role's kept lane, and whose hour ends with no session taking
// it, is told to Dispatch: the walk that drops it stores one expiry notice on the role's expiry
// subject, naming the event, the role, the copy's own attempt as the message id it was kept under
// says it, its key and its stream sequence, and when it was kept and dropped, under a message id
// of that copy's alone, so the two listeners that both walk the lane store one notice. It gets no
// dead letter: the person or session behind a Dispatch event sent no Envoy message. At legion main
// the walk drops the copy and tells nobody.
func TestAnOutboxCopyWhoseHourEndsIsToldToDispatch(t *testing.T) {
	t.Run("a each outbox copy dropped at the end of its hour stores one expiry notice naming its event, its role and its attempt", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-outbox-told"
		first := rig.keep(role, outboxCopy(role, "ses_outbox_told_sender", "a Dispatch event routed to the role"))
		again := rig.keepAttempt(role, outboxCopy(role, "ses_outbox_told_sender", "a Dispatch event kept again"), 2)
		listened := rig.keep(role, listenerCopy(role, "ses_listener_told_sender", "a publish the listener kept"))
		rig.startSweeps()
		rig.letSweepsRun()
		if notices := rig.expiryNotices(role); len(notices) != 0 {
			t.Fatalf("inside the copies' hour the stream holds expiry notices %+v, want none", notices)
		}

		before := rig.now()
		rig.moveClock(time.Hour + time.Minute)
		droppedAfter := before.Add(time.Hour)
		rig.waitPassed(role, listened.sequence)
		rig.waitDeadLetter(listened)
		rig.letSweepsRun()
		droppedBy := rig.now()

		notices := rig.expiryNotices(role)
		if len(notices) != 2 {
			t.Fatalf("the stream holds %d expiry notices for role %s, want one for each of the outbox's two copies and none for the listener's: %+v",
				len(notices), role, notices)
		}
		assertNoticeOf(t, notices[0], role, first, 1, droppedAfter, droppedBy)
		assertNoticeOf(t, notices[1], role, again, 2, droppedAfter, droppedBy)
		if stored := rig.count(expiryNoticesStored); stored != 2 {
			t.Fatalf("envoy_role_kept_expiry_notices_total counts %d stored, want two", stored)
		}
		for _, held := range []laneCopy{first, again} {
			lines := rig.lines(expiryNoticeLine, map[string]any{"role": role, "event_id": held.item.EventID})
			if len(lines) != 1 || lines[0]["kept_by"] != expiryNoticeDispatchLabel {
				t.Fatalf("the notice lines of event %s are %v, want one naming kept_by %s", held.item.EventID, lines, expiryNoticeDispatchLabel)
			}
			if letters := rig.deadLetters(held.item.SourceSession, held.item.EventID); len(letters) != 0 {
				t.Fatalf("event %s's sender was sent dead letters %+v, want none", held.item.EventID, letters)
			}
		}
		if drops := rig.count(hourExpiredOutbox); drops != 2 {
			t.Fatalf("%s is %d, want the two outbox copies", hourExpiredOutbox, drops)
		}
	})

	t.Run("b an outbox copy no walk reached until its hour had ended more than an hour before is told to Dispatch all the same", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-outbox-stale"
		held := rig.keep(role, outboxCopy(role, "ses_outbox_stale_sender", "a Dispatch event nobody walked"))
		before := rig.now()
		rig.moveClock(3 * time.Hour)
		rig.startSweeps()
		rig.waitPassed(role, held.sequence)
		rig.letSweepsRun()
		notices := rig.expiryNotices(role)
		if len(notices) != 1 {
			t.Fatalf("the stream holds %d expiry notices for the stale copy, want one: %+v", len(notices), notices)
		}
		assertNoticeOf(t, notices[0], role, held, 1, before.Add(3*time.Hour), rig.now())
		if drops := rig.count(`test_role_kept_expired{kept_by="dispatch_outbox",stale="true"}`); drops != 1 {
			t.Fatalf("the stale outbox copy's drop is counted %d times, want once as stale", drops)
		}
	})
}
