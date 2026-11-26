package main

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
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

// noticeStop is where a noticeSeam stops the walk of the listener it stops.
type noticeStop int

const (
	// stopBeforeNotice stops the walk after its claim write and before the notice: the notice is
	// not stored.
	stopBeforeNotice noticeStop = iota
	// stopAfterNotice stops the walk after the notice and before the cursor moves: the notice is
	// stored and the confirm is not written.
	stopAfterNotice
)

// noticeSeam is the expiry notice's publish of both of an hourRig's listeners
// (cfg.publishExpiryNotice). The first call, made by the walk of listener stopping, stops that
// walk where stop says and never returns until the test ends, as a listener stopped there would
// never write again: its walk holds the role's guard on that listener, so that listener's sweeps
// pass the lane over, and it makes no later write. Before it blocks, the call lets the other
// listener walk the lane (released). Every later call publishes as main.go does, through that
// listener's bus.Client.PublishKeptCopyExpiry. Each call records which listener made it, role's
// cursor and how many notices role's expiry subject held when it was made.
type noticeSeam struct {
	role     string
	stop     noticeStop
	stopping int
	released func()
	mu       sync.Mutex
	calls    []noticeCall
}

// noticeCall is what one call of a noticeSeam found.
type noticeCall struct {
	// listener is the index of the listener whose walk made the call.
	listener int
	// cursor is the role's drain cursor when the call was made.
	cursor bus.KeptRoleCursor
	// held is how many notices role's expiry subject held when the call was made.
	held int
	// err is a read of the lane or the subject the call could not make.
	err error
}

// install sets the seam on both of rig's listeners. A test installs it before it starts the
// sweeps, which copy the configuration they walk with. The stopped walk is let go when the test
// ends, before the sweeps are stopped.
func (s *noticeSeam) install(t *testing.T, rig *hourRig) {
	t.Helper()
	stopped := make(chan struct{})
	for i, l := range rig.listeners {
		client := l.harness.client
		l.cfg.publishExpiryNotice = func(ctx context.Context, notice bus.KeptCopyExpiry) error {
			call := noticeCall{listener: i}
			cursor, err := client.ReadKeptRoleCursor(ctx, s.role)
			call.cursor, call.err = cursor.Cursor, err
			if err == nil {
				var held []bus.StoredMessage
				held, call.err = readStored(ctx, client, bus.KeptExpiredSubject(s.role))
				call.held = len(held)
			}
			s.mu.Lock()
			s.calls = append(s.calls, call)
			first := len(s.calls) == 1
			s.mu.Unlock()
			if !first || i != s.stopping {
				return client.PublishKeptCopyExpiry(ctx, notice)
			}
			if s.stop == stopAfterNotice {
				if err := client.PublishKeptCopyExpiry(ctx, notice); err != nil {
					return err
				}
			}
			s.released()
			<-stopped
			return bus.ErrStreamFull
		}
		l.rewire(t)
	}
	rig.t.Cleanup(func() { close(stopped) })
}

// made is every call the seam has had.
func (s *noticeSeam) made() []noticeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// assertTakenOver fails unless the seam had two calls: the stopped listener's, then the other
// listener's, which took the stopped walk's claim over. Each was made with the cursor holding a
// claim of held with kept short of it, and the second was made while the stream held heldBefore
// notices of the copy: none where the walk stopped before its notice, one where it stopped after.
func (s *noticeSeam) assertTakenOver(t *testing.T, held laneCopy, heldBefore int) {
	t.Helper()
	calls := s.made()
	if len(calls) != 2 {
		t.Fatalf("the expiry notice was published %d times, want twice: the stopped walk's and the walk that took its claim over: %+v", len(calls), calls)
	}
	if calls[0].listener != s.stopping || calls[1].listener == s.stopping {
		t.Fatalf("the calls came from listeners %d and %d, want the stopped listener %d and then the other", calls[0].listener, calls[1].listener, s.stopping)
	}
	for i, call := range calls {
		if call.err != nil {
			t.Fatalf("call %d of the notice's publish could not read the lane: %v", i+1, call.err)
		}
		if call.cursor.Claiming != held.sequence || call.cursor.Kept >= held.sequence || call.cursor.DeadLetter != string(deadLetterExpired) {
			t.Fatalf("call %d of the notice's publish found the cursor %+v, want an expired claim of the copy at %d with kept short of it", i+1, call.cursor, held.sequence)
		}
	}
	if calls[1].held != heldBefore {
		t.Fatalf("the stream held %d notices of the copy when the other listener took the claim over, want %d", calls[1].held, heldBefore)
	}
}

// R18's two stop rows: a listener whose walk stops between two of its writes for an outbox copy
// whose hour ended leaves a claim, the other listener's walk takes that claim over once it is past
// bus.KeptRoleClaimWindow, and the stream then holds one notice for the copy, under the copy's own
// message id (bus.KeptExpiryMsgID), which only the walk whose confirm landed reports. The rows
// move the listeners' clock past the copy's hour, so every claim reads older than the window the
// moment it is written.
//
//   - c: the walk stops after its claim write and before the notice. The other listener's walk
//     finds no notice stored and stores it.
//   - d: the walk stops after the notice and before the cursor moves. The other listener's walk
//     stores the notice again under the same id, and the stream keeps the one it holds.
//
// Each row fails when its stop is planted one step later: c sees the notice already stored when
// the other listener takes over, and d, whose walk then finishes, sees one call where it wants
// two. Each fails too when its recovery is removed: with a walk that takes an expired claim of an
// outbox copy over ending the copy with no notice (judgeKeptRoleCopy), each row sees one call
// where it wants two, and c's notice is never stored.
//
// Dispatch's outbox reading that one notice and reopening the event's delivery once is the outbox's
// own row, which stores one notice twice through bus.Client.PublishKeptCopyExpiry as these two
// walks do (internal/dispatch/outbox,
// TestANoticeStoredAgainByTheWalkThatTookTheClaimOverReopensTheRowOnce).
func TestAnExpiryNoticeReachesDispatchWhenAListenerStopsBetweenItsWrites(t *testing.T) {
	for _, row := range []struct {
		name       string
		stop       noticeStop
		heldBefore int
	}{
		{"c a listener stopped after its claim write and before the notice", stopBeforeNotice, 0},
		{"d a listener stopped after the notice and before the cursor moves", stopAfterNotice, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newHourRig(t)
			role := "hour-outbox-stopped-" + strconv.Itoa(int(row.stop))
			held := rig.keep(role, outboxCopy(role, "ses_outbox_stopped_sender", "a Dispatch event whose notice a listener stops in"))
			// The second listener walks first and stops; the first takes its claim over. The first
			// listener's guard is held until the stop, so its sweeps pass the lane over until then.
			release, taken := rig.listeners[0].cfg.roleKeepDrains.hold(context.Background(), role)
			if !taken {
				t.Fatalf("take role %s's handover guard on the first listener", role)
			}
			var once sync.Once
			seam := &noticeSeam{role: role, stop: row.stop, stopping: 1, released: func() { once.Do(release) }}
			seam.install(t, rig)
			before := rig.now()
			rig.moveClock(time.Hour + time.Minute)
			rig.startSweeps()
			rig.waitPassed(role, held.sequence)
			rig.letSweepsRun()

			seam.assertTakenOver(t, held, row.heldBefore)
			notices := rig.expiryNotices(role)
			if len(notices) != 1 {
				t.Fatalf("the stream holds %d expiry notices for the copy, want one: %+v", len(notices), notices)
			}
			assertNoticeOf(t, notices[0], role, held, 1, before.Add(time.Hour), rig.now())
			if stored := rig.count(expiryNoticesStored); stored != 1 {
				t.Fatalf("envoy_role_kept_expiry_notices_total counts %d stored, want one: the walk whose confirm landed", stored)
			}
			if lines := rig.lines(expiryNoticeLine, map[string]any{"role": role}); len(lines) != 1 {
				t.Fatalf("the notice is logged %d times, want once: %v", len(lines), lines)
			}
		})
	}
}
