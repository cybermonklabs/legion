package main

// A forward the stream stored after its resolution's fence and its own wait is no delivery, and a
// pass that was held since it read a record cannot write that older state back.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
)

// A role-lane forward checks its resolution's age before it publishes (roles.ResolutionTooOld),
// which bounds when the publish starts and not when the stream stores it. A process held between
// the two - here the forward seam holds it while the role moves and its takeover record ends -
// stored its copy on the previous holder's subject and reported it delivered, and no pass copied
// it to the new holder: the message was lost in silence.
//
// Now such a forward is reported failed (roles.ErrForwardStoredTooLate), which clears its attempt,
// and the sender's next send resolves the holder again and reaches the new holder exactly once.
//
// The control is the same rig with no hold: the forward is stored inside its wait and delivered.
func TestAForwardStoredAfterItsFenceIsNoDeliveryAndTheResendReachesTheNewHolderOnce(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_late_a", "ses_late_b", "late"
	previous := rig.relayed(a, answerDone)
	heardA := hear(t, previous)
	rig.claim(rig.mux, a, role)
	next := rig.relayed(b, answerDone)
	heardB := hear(t, next)

	held, release := rig.holdForward("the forward held past its fence")
	// The message goes on the role's lane, so its forward is the role lane forwarder's, the path
	// this row is about. The publish route's own forward is #5412's to fix and Dispatch's outbox
	// #5402's; each asks the same helper of its own acknowledgement.
	item := rig.onLane(role, "the forward held past its fence")
	crashAwait(t, held, crashWatch, "the forward to reach the hold")
	rig.claim(rig.mux, b, role)
	rig.clock.advance(roleKeepWindow + roleWakeInterval)
	// A pass that found neither the previous holder's consumer nor a resume record of its floor
	// ends and leaves the record to the next sweep (runTakeoverPass, FloorMissed), so the row
	// sweeps until the record ends, as the listener's own five-minute sweeps would.
	var ended bool
	for range 3 {
		rig.sweep(rig.cfg(), role)
		if ended = rig.onlyRecord(role, a).Finished; ended {
			break
		}
	}
	if !ended {
		t.Fatalf("the record %+v did not end in three sweeps while the forward was held", rig.onlyRecord(role, a))
	}
	release()
	answer := crashPublished{EventID: item.EventID}
	waitFor(t, crashWatch, "the held forward reported", func() bool {
		return strings.Contains(rig.harness.logs.String(), roles.ErrForwardStoredTooLate.Error())
	})
	if logs := rig.harness.logs.String(); strings.Contains(logs, `"listener role forwarded"`) &&
		strings.Contains(logs, answer.EventID) && !strings.Contains(logs, roles.ErrForwardStoredTooLate.Error()) {
		t.Fatalf("the held forward was reported delivered:\n%s", logs)
	}

	resent := rig.publish(role, "the forward held past its fence")
	if resent.status != 200 || resent.Holder != b {
		t.Fatalf("the send again answered %d %s; want 200 for %s", resent.status, resent.raw, b)
	}
	if !heardB.await(crashWatch, resent.EventID) {
		t.Fatalf("the new holder never received the message sent again; it received %v", heardB.all())
	}
	if n := heardB.count(resent.EventID); n != 1 {
		t.Fatalf("the new holder received the message sent again %d times, want exactly once", n)
	}
	if n := heardA.count(resent.EventID); n != 0 {
		t.Fatalf("the previous holder received the message sent again %d times, want none", n)
	}

	t.Run("control: a forward stored inside its wait is delivered", func(t *testing.T) {
		const c, control = "ses_late_c", "late-control"
		holder := rig.relayed(c, answerDone)
		heard := hear(t, holder)
		rig.claim(rig.mux, c, control)
		sent := rig.publish(control, "a forward with nothing holding it")
		if sent.status != 200 || sent.Holder != c {
			t.Fatalf("the control publish answered %d %s; want 200 for %s", sent.status, sent.raw, c)
		}
		if !heard.await(crashWatch, sent.EventID) {
			t.Fatalf("the control holder never received its message; it received %v", heard.all())
		}
	})
}

// A pass that read a record and was then held cannot write that older state over a later pass's
// write, the one that finished the record among them: the write names the sequence the pass read
// (bus.Client.WriteTakeoverRecordAt), and the stream refuses it (bus.ErrTakeoverRecordMoved). The
// record a reader sees after a finish is finished, which is what lets a client read a finished
// record once (bus.Client.TakeoverRecords).
//
// The control is a write at the subject's newest message, which lands.
func TestAPassCannotWriteOverARecordAnotherPassMoved(t *testing.T) {
	rig := newCrashRig(t)
	const a, b, role = "ses_moved_a", "ses_moved_b", "moved"
	rig.gone(a)
	rig.claim(rig.away, a, role)
	rig.sendTo(a, role, 1, "before the takeover")
	next := rig.relayed(b, answerDone)
	_ = next
	rig.claim(rig.mux, b, role)
	rig.idle(rig.cfg(), role)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	records, err := rig.harness.client.TakeoverRecords(ctx, role)
	if err != nil || len(records) != 1 {
		t.Fatalf("the takeover records of %s: %d, %v; want one", role, len(records), err)
	}
	stale := records[0]

	// A later pass moves the record on: the control, a write at the subject's newest message,
	// lands.
	moved := stale.Record
	moved.Next = stale.Record.Next + 1
	at, err := rig.harness.client.WriteTakeoverRecordAt(ctx, stale, moved)
	if err != nil {
		t.Fatalf("control: a write at the subject's newest message: %v", err)
	}
	// The pass held since its read cannot write its older state over that.
	if _, err := rig.harness.client.WriteTakeoverRecordAt(ctx, stale, stale.Record); !errors.Is(err, bus.ErrTakeoverRecordMoved) {
		t.Fatalf("the held pass's write answered %v; want bus.ErrTakeoverRecordMoved", err)
	}
	// The pass that read the moved record finishes it, and no write lands over the finish.
	current := bus.StoredTakeoverRecord{Subject: stale.Subject, Sequence: at, Record: moved}
	finished := moved
	finished.Finished = true
	end, err := rig.harness.client.WriteTakeoverRecordAt(ctx, current, finished)
	if err != nil {
		t.Fatalf("the pass that finishes the record: %v", err)
	}
	if _, err := rig.harness.client.WriteTakeoverRecordAt(ctx, current, moved); !errors.Is(err, bus.ErrTakeoverRecordMoved) {
		t.Fatalf("a write over the finish by a pass that read the record before it answered %v; want bus.ErrTakeoverRecordMoved", err)
	}
	ended := bus.StoredTakeoverRecord{Subject: stale.Subject, Sequence: end, Record: finished}
	if _, err := rig.harness.client.WriteTakeoverRecordAt(ctx, ended, moved); !errors.Is(err, bus.ErrTakeoverRecordFinished) {
		t.Fatalf("a write over a record read finished answered %v; want bus.ErrTakeoverRecordFinished", err)
	}
	if read := rig.onlyRecord(role, a); !read.Finished {
		t.Fatalf("the record on the stream is %+v; want the finish to stand", read)
	}
}

// crashRig.publish names the role's lane, so the publish route forwards through the role lane's
// forwarder, whose seam the hold is on (crashRig.holdForward). This keeps that true: a change that
// moved the route off the forwarder would leave the rows above holding nothing.
func TestTheHoldSeamIsOnThePathARolePublishTakes(t *testing.T) {
	rig := newCrashRig(t)
	const holder, role = "ses_seam", "seam"
	standIn := rig.relayed(holder, answerDone)
	heard := hear(t, standIn)
	rig.claim(rig.mux, holder, role)
	held, release := rig.holdForward("the seam's own message")
	go rig.publish(role, "the seam's own message")
	crashAwait(t, held, crashWatch, "the publish route's forward to reach the hold")
	release()
	waitFor(t, crashWatch, "the forward delivered", func() bool { return len(heard.all()) > 0 })
	if subject := contracts.LegacyAgentSubject(holder); len(rig.stored(subject)) == 0 {
		t.Fatalf("nothing is stored on %s after the forward", subject)
	}
}
