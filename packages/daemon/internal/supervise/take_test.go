package supervise

// A task is a value the worker takes (take.go). These tests pin the take itself: each hold a take
// meets refused by name, each drop deleting the value, what an accepted take applies once and
// never again, the number a task keeps through its take-backs and relaunches, and what a
// registering worker is told of its value.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// adoptingIdentity is a role identity a take of a phase task adopts the working copy with.
func adoptingIdentity(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
	return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
}

// Each of the eight holds a state of the claim, its issue or its tree puts on a task refuses a
// take by its own name, leaves the task untaken and its value in the bucket, and records the
// refusal at the value's revision, which is how the end of the hold knows to write the value again
// (offerPending).
func TestTakeIsRefusedByEachHoldByName(t *testing.T) {
	for _, tc := range []struct {
		hold string
		// arrange brings a harness to a claim holding a task whose take the hold refuses, with
		// the harness's worker taking nothing on its own.
		arrange func(t *testing.T) *harness
	}{
		{hold: HoldIssueInTurn, arrange: func(t *testing.T) *harness {
			h := newHarness(t)
			h.deps.Identity = adoptingIdentity
			h.deps.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testerToken, true }
			h.restart()
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
			return h
		}},
		{hold: HoldSuspension, arrange: func(t *testing.T) *harness {
			h := newHarness(t)
			h.reach(StateWorking)
			if err := h.handle(RequestSuspend{Claim: h.token, By: SuspendedByOperator}); !suspensionHeld(err) {
				t.Fatalf("suspend mid-turn = %v, want the hold", err)
			}
			h.rt.FailSuspend(errBoom)
			_ = h.handle(StreamTurnEnd{Claim: h.token}) // the stop is refused: the hold stands over an idle claim
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "the operator's task"})
			return h
		}},
		{hold: HoldUpdateRound, arrange: func(t *testing.T) *harness {
			h := updateHarness(t, PodUpdateContract)
			h.reach(StateReady)
			h.openUpdate(testRound, testReasons)
			h.must(StreamTurnStart{Claim: h.token})
			h.signalUpdate("c0ffee")
			h.must(StreamTurnEnd{Claim: h.token})
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "review the pull request"})
			return h
		}},
		{hold: HoldLaunchWaits, arrange: func(t *testing.T) *harness {
			h := pacedHarness(t, wakeRate)
			h.reach(StateReady)
			h.observe(runtime.StaleAddress)
			h.wantWaitingWake(WakeLaunch)
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "review the pull request", ID: "outbox:42", Row: 42, Generation: 7})
			return h
		}},
		{hold: HoldStalePlugin, arrange: func(t *testing.T) *harness {
			h := newHarness(t)
			h.reach(StateReady)
			h.m.deps.PluginContract = refreshContract
			h.m.deps.PluginRelaunchable = true
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "review the pull request", ID: "outbox:42", Row: 42, Generation: 7})
			return h
		}},
		{hold: HoldRootHears, arrange: func(t *testing.T) *harness {
			h := newHarnessOf(t, rootClaim(), func(d *Deps) {
				d.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return false, nil }
			})
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: rootToken, Task: "a person decided on the tree", Generation: 2, Row: 7})
			return h
		}},
		{hold: HoldProviderError, arrange: func(t *testing.T) *harness {
			h := workingClaim(t)
			h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
			h.manualTakes()
			return h
		}},
		// A restored machine admits no name of the session yet (Machine.heldNames), so the take
		// asks the store, which holds the claim's session id for another claim.
		{hold: HoldSessionNotHeld, arrange: func(t *testing.T) *harness {
			h := newHarness(t)
			h.reach(StateReady)
			h.store.setHold(sessionID(session), claim.Token("legion-legion-LEGION-209-tester"))
			h.restart()
			h.manualTakes()
			h.must(RequestDeliver{Claim: h.token, Task: "review the pull request", ID: "outbox:42", Row: 42, Generation: 7})
			return h
		}},
	} {
		t.Run(tc.hold, func(t *testing.T) {
			h := tc.arrange(t)

			h.wantRefused(tc.hold)

			if p := h.pending(); p.taken() {
				t.Fatalf("pending %+v after the refusal, want the task untaken", p)
			}
			if got := h.claim().TaskTakeState; got != TakeRefused {
				t.Fatalf("the claim records the take %q, want it refused at the value's revision", got)
			}
			if _, held := h.values.value(h.token); !held {
				t.Fatal("the refusal deleted the value; a refused take leaves the task owed")
			}
		})
	}
}

// A take the worker sends while its claim is not ready or idle — a turn of its own running, its
// agent not yet ready — is refused by that, in-turn or not-ready, and records the refusal at the
// revision as every hold does.
func TestTakeIsRefusedInTurnAndNotReady(t *testing.T) {
	t.Run(HoldInTurn, func(t *testing.T) {
		h := newHarness(t)
		h.manualTakes()
		h.reach(StateReady)
		h.must(StreamTurnStart{Claim: h.token})
		h.must(RequestDeliver{Claim: h.token, Task: "after this turn"})
		h.wantRefused(HoldInTurn)
		if got := h.claim().TaskTakeState; got != TakeRefused {
			t.Fatalf("the claim records the take %q, want it refused at the value's revision", got)
		}
	})
	t.Run(HoldNotReady, func(t *testing.T) {
		h := newHarness(t)
		h.manualTakes()
		h.reach(StateRegistered)
		h.must(RequestDeliver{Claim: h.token, Task: "the task"})
		h.wantRefused(HoldNotReady)
		if got := h.claim().TaskTakeState; got != TakeRefused {
			t.Fatalf("the claim records the take %q, want it refused at the value's revision", got)
		}
	})
}

// A take of a task the record no longer owes is answered dropped, the task retired and its value
// deleted: a task whose issue has left the phase it was queued for, and a root architect's task
// that narrows to nothing (the third drop, the ask of a round the claim left, is
// TestTakeOfAnAskOfARoundTheClaimLeftIsDroppedAndItsValueDeleted).
func TestTakeDropsATaskTheRecordNoLongerOwesAndDeletesItsValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T) *harness
	}{
		{name: "the issue has left the task's phase", arrange: func(t *testing.T) *harness {
			holds := true
			h := newHarnessOf(t, queuedClaim(), func(d *Deps) {
				d.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return holds, nil }
			})
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "test the branch", Phase: phase.Testing, Generation: 7})
			holds = false
			return h
		}},
		{name: "a root architect's task that names nothing now", arrange: func(t *testing.T) *harness {
			h := newHarnessOf(t, rootClaim(), func(d *Deps) {
				d.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return true, nil }
				d.RootTask = func(context.Context, Claim, Delivery) (string, []byte, error) { return "", nil, nil }
			})
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: rootToken, Task: "a person decided on the tree", Generation: 2, Row: 7})
			return h
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.arrange(t)
			if _, held := h.values.value(h.token); !held {
				t.Fatal("the queue wrote no value; this test needs one to delete")
			}

			if answer := h.take(); answer.Outcome != TakeOutcomeDropped {
				t.Fatalf("take = %+v, want it dropped", answer)
			}

			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v after the drop, want none", p)
			}
			if d, held := h.store.delivery(h.token); held {
				t.Fatalf("the store still holds %+v, want the dropped task retired", d)
			}
			if _, held := h.values.value(h.token); held {
				t.Fatal("the bucket still holds the dropped task's value")
			}
			if deleted := h.values.deleted(); len(deleted) != 1 || deleted[0] != h.token {
				t.Fatalf("values deleted = %v, want the claim's one", deleted)
			}
		})
	}
}

// The drop test a take makes (dropUnowed) runs too when a newer task arrives behind an untaken
// task, whatever holds stand: a task the record no longer owes is dropped, its value deleted under
// the line a take's drop logs, and the newer task is queued in its place and its value written.
// Each claim here has no process, so no worker can take, and only the queue can drop the task.
// That is the daemon's TestAWorkflowStartStartsAStoppedWorkerAgain ("a production check's look
// after the issue's merge"): a tester stopped holding its testing task untaken, the issue merged
// and closed, and the production check's look that started the tester again was refused on every
// retry behind a task nobody was owed. A task the record still owes refuses the newer one, as
// before (ErrDeliveryPending). A claim whose record of its last let-go still tells gets no
// delivery of that record after this drop, where a take's drop gives it one (queueRelaunch): the
// newer task holds the slot, so the record reaches the worker in front of it, and the accepted
// take of the newer task, once the claim's process is up, carries the record's sentence in front
// of its text, once (Delivery.message).
//
// The negative control is the queue's drop removed: each of the dropped tasks refuses the newer
// task "a delivery is already pending", and the claim keeps the task nobody is owed. The record's
// case has one of its own: the record's sentence left out of the text in front of a task
// (Delivery.message), under which the newer task's take is answered the task alone.
func TestTakeDropTestRunsWhenANewerTaskArrives(t *testing.T) {
	const newer = "look at production after the merge"
	for _, tc := range []struct {
		name  string
		held  Delivery
		ended EndedWork
		// holds is the record's answer for the held task's phase (Deps.PhaseHolds).
		holds bool
		// dropped is the line the drop logs, and "" for a task the record still owes.
		dropped string
	}{
		{name: "a task whose phase the issue left",
			held:    Delivery{ID: "outbox:7", Task: "implement the plan", Phase: phase.Implementing, Generation: 7},
			dropped: "the issue has left the phase this task was queued for; it is dropped rather than taken"},
		{name: "a task whose phase the issue left, held while the relaunch's record still tells",
			held: Delivery{ID: "outbox:7", Task: "implement the plan", Phase: phase.Implementing, Generation: 7},
			ended: EndedWork{At: epoch, Cause: endDisplaced, Generation: 1, Turn: true, Background: true,
				Kinds: []string{"subagent"}, Reported: true},
			dropped: "the issue has left the phase this task was queued for; it is dropped rather than taken"},
		{name: "an older daemon's ask of an update round",
			held:    Delivery{ID: updateTaskID(testRound), Task: UpdateTask(testRound, testReasons, epoch.Add(time.Hour))},
			holds:   true,
			dropped: "this task is an ask an older daemon queued for an update round; it is dropped and never taken"},
		{name: "the delivery of a relaunch's record that no longer tells",
			held: Delivery{ID: relaunchTaskID(), Task: "Your previous process ended."},
			ended: EndedWork{At: epoch, Cause: endDisplaced, Generation: 1, Turn: true, Reported: true,
				Told: true},
			holds:   true,
			dropped: "the daemon's own delivery of a relaunch's record has nothing to say, since that record no longer tells; it is dropped rather than taken"},
		{name: "a task the record still owes",
			held:  Delivery{ID: "outbox:7", Task: "implement the plan", Phase: phase.Implementing, Generation: 7},
			holds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := queuedClaim()
			held := tc.held
			held.Number, held.QueuedAt = 1, epoch
			c.TaskWritten, c.Pending, c.EndedWork = 1, &held, tc.ended
			h := newBareHarness(t)
			h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return tc.holds, nil }
			h.restore(c)
			h.manualTakes()
			if p := h.pending(); p.ID != held.ID || p.taken() {
				t.Fatalf("the claim holds %+v, want %s untaken: this test proves nothing", p, held.ID)
			}

			err := h.handle(RequestDeliver{Claim: h.token, Task: newer, ID: "outbox:9", Row: 9})

			if tc.dropped == "" {
				if !errors.Is(err, ErrDeliveryPending) || err.Error() != "deliver refused: a delivery is already pending" {
					t.Fatalf("the newer task was answered %v, want the pending-delivery refusal behind a task the record owes", err)
				}
				if p := h.pending(); p.ID != held.ID {
					t.Fatalf("the claim holds %+v, want the owed task %s kept", p, held.ID)
				}
				if deleted := h.values.deleted(); len(deleted) != 0 {
					t.Fatalf("values deleted = %v, want none: the record owes the task", deleted)
				}
				return
			}
			if err != nil {
				t.Fatalf("the newer task was answered %v, want it queued in the place of a task nobody is owed", err)
			}
			if lines := h.logs.lines(tc.dropped, "task=1", "delivery="+held.ID); len(lines) != 1 {
				t.Fatalf("logged %v, want the take's drop line once for task 1", lines)
			}
			if deleted := h.values.deleted(); len(deleted) != 1 || deleted[0] != h.token {
				t.Fatalf("values deleted = %v, want the dropped task's value deleted", deleted)
			}
			if lines := h.logs.lines(valueDeletedLine, "task=1", "cause="+valueDeletedDropped); len(lines) != 1 {
				t.Fatalf("logged %v, want task 1's value deleted under the drop's cause", lines)
			}
			if p := h.pending(); p.ID != "outbox:9" || p.Task != newer || p.Number != 2 || p.taken() {
				t.Fatalf("the claim holds %+v, want the newer task pending untaken as task 2", p)
			}
			if d, held := h.store.delivery(h.token); !held || d.ID != "outbox:9" {
				t.Fatalf("the store holds %+v (held %v), want the newer task in the dropped one's place", d, held)
			}
			if value, held := h.values.value(h.token); !held || value.Number != 2 {
				t.Fatalf("the bucket holds %+v (held %v), want the newer task's value, task 2", value, held)
			}
			if !tc.ended.Tells() {
				return
			}
			// The newer task is the one the worker takes once the claim's process is up, behind
			// the record's sentence, and that take tells the record.
			h.reach(StateReady)
			if p := h.pending(); p.ID != "outbox:9" || p.Relaunch() {
				t.Fatalf("the ready claim holds %+v, want the newer task and no delivery of the record", p)
			}
			want := relaunchSentence(tc.ended, true) + "\n\n" + newer
			if got := h.mustTake(); got != want {
				t.Fatalf("the take of the newer task was answered %q, want the record's sentence in front of the task, once: %q", got, want)
			}
			if e := h.claim().EndedWork; !e.Told || e.ToldBy != "outbox:9" {
				t.Fatalf("after the take the record is %+v, want it told by the newer task's take", e)
			}
		})
	}
}

// A newer task that arrives behind an untaken task the record still owes asks the record once in
// that decision: the queue's drop test reads the issue's phase (dropUnowed), and the hold's end
// its refusal makes (offer) takes that answer rather than reading the phase again. The worker
// asked at the latest revision and was refused by a hold that has ended since, so that hold's end
// writes the value again under the owed task's number, as it did when it read the phase twice.
//
// The negative control is the queue's refusal ending the hold as every other caller does
// (offerPending): the decision reads the phase twice.
func TestTakeDropTestReadsTheRecordOnceWhenANewerTaskWaitsBehindAnOwedOne(t *testing.T) {
	reads, inTurn := 0, true
	h := newHarness(t)
	h.deps.Identity = adoptingIdentity
	h.deps.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testerToken, inTurn }
	h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) {
		reads++
		return true, nil
	}
	h.restart()
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, ID: "outbox:7", Row: 7})
	h.wantRefused(HoldIssueInTurn)
	before, _ := h.values.value(h.token)
	inTurn, reads = false, 0

	err := h.handle(RequestDeliver{Claim: h.token, Task: "look at production after the merge", ID: "outbox:9", Row: 9})

	if !errors.Is(err, ErrDeliveryPending) {
		t.Fatalf("the newer task was answered %v, want the pending-delivery refusal behind a task the record owes", err)
	}
	if reads != 1 {
		t.Fatalf("the decision read the issue's phase %d times, want once: the queue's drop test asked it, and the hold's end has that answer", reads)
	}
	if after, held := h.values.value(h.token); !held || after.Number != before.Number || after.Revision <= before.Revision {
		t.Fatalf("the bucket holds %+v (held %v) after the hold's end, want the owed task's value %+v written again", after, held, before)
	}
}

// A root architect's held task names decisions that can be owed to another tree by the time its
// root hears again: the take is answered with what it still names (Deps.RootTask), and the
// delivery is written narrowed, so a take sent again is answered the same narrowed text.
func TestTakeOfARootTaskIsNarrowedToWhatItStillNames(t *testing.T) {
	h := newHarnessOf(t, rootClaim(), func(d *Deps) {
		d.RootHears = func(context.Context, Claim, Delivery) (bool, error) { return true, nil }
		d.RootTask = func(context.Context, Claim, Delivery) (string, []byte, error) {
			return "a person decided on this tree only", []byte(`{"decisions":[]}`), nil
		}
	})
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: rootToken, Task: "a person decided on two trees", Generation: 2, Row: 7,
		Kept: []byte(keptDecision)})

	if text := h.mustTake(); text != "a person decided on this tree only" {
		t.Fatalf("the take was answered %q, want the narrowed task", text)
	}
	p := h.pending()
	if p.Task != "a person decided on this tree only" || !bytes.Equal(p.Kept, []byte(`{"decisions":[]}`)) {
		t.Fatalf("pending %+v, want the delivery narrowed to what it still names", p)
	}
	if stored, _ := h.store.delivery(rootToken); stored.Task != p.Task || !bytes.Equal(stored.Kept, p.Kept) {
		t.Fatalf("stored delivery %+v, want the narrowing written", stored)
	}
	if again := h.mustTake(); again != "a person decided on this tree only" {
		t.Fatalf("the take sent again was answered %q, want the same narrowed text", again)
	}
}

// An accepted take is the write that tells the decisions the task names: the delivery is written
// with the take recorded on it (ConfirmedAt) and still naming what the row kept, in the claim's
// and the delivery's one write, before the take's turn starts — so a person's decision reaches
// the architect at its take rather than at a turn that may never start.
func TestTakeAcceptedWritesTheKeptDecisionsWithTheTake(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "a person decided: dispatch://LEGION-208/ask/ask-1",
		ID: "outbox:40", Row: 40, Phase: phase.Implementing, Generation: 7, Kept: []byte(keptDecision)})
	if stored, _ := h.store.delivery(testToken); stored.taken() {
		t.Fatalf("the stored delivery %+v is taken before any take; the queue tells nothing", stored)
	}

	h.mustTake()

	stored, ok := h.store.delivery(testToken)
	if !ok || !stored.awaitsTurn() || !bytes.Equal(stored.Kept, []byte(keptDecision)) {
		t.Fatalf("the stored delivery after the take = %+v (held %t), want it taken, its turn to come, and still naming %q",
			stored, ok, keptDecision)
	}
	if got := h.store.load(testToken).TaskTaken; got != stored.Number {
		t.Fatalf("the stored claim records task %d taken, want %d: the take's write is the claim's and the delivery's", got, stored.Number)
	}
}

// The roles of an issue share one working copy, so a role whose take was accepted is in a turn
// for every other role of the issue until that take's turn ends (Claim.InTurn): another role's
// take of a phase task is refused issue-in-turn between the take and its turn's start, and through
// the turn, and is accepted once the turn has ended.
func TestTakeAcceptedHoldsTheIssueInTurnForAnotherRoleUntilItsTurnEnds(t *testing.T) {
	implementer := newHarness(t)
	implementer.deps.Identity = adoptingIdentity
	implementer.restart()
	implementer.manualTakes()
	implementer.reach(StateReady)

	tester := newHarnessOf(t, testerClaim())
	tester.deps.Identity = adoptingIdentity
	tester.deps.IssueInTurn = func(issue string, except claim.Token) (claim.Token, bool) {
		if view := implementer.m.View(); view.Issue == issue && view.Token != except && view.InTurn() {
			return view.Token, true
		}
		return "", false
	}
	tester.restart()
	tester.manualTakes()
	tester.reach(StateReady)
	tester.must(RequestDeliver{Claim: testerToken, Task: "test the branch", Phase: phase.Testing, Generation: 7})

	implementer.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	implementer.mustTake()

	tester.wantRefused(HoldIssueInTurn)
	implementer.must(StreamTurnStart{Claim: testToken})
	tester.must(IssueTurnEnded{Claim: testerToken})
	tester.wantRefused(HoldIssueInTurn)
	tester.wantCalls("AdoptWorkingCopy", 0)

	implementer.must(StreamTurnEnd{Claim: testToken})
	tester.must(IssueTurnEnded{Claim: testerToken})

	if text := tester.mustTake(); text != "test the branch" {
		t.Fatalf("the tester's take after the implementer's turn ended = %q, want its task", text)
	}
	tester.wantCalls("AdoptWorkingCopy", 1)
}

// A take refused issue-in-turn records that the claim's pending task waits behind another claim's
// turn, and the machine publishes the task's number on its view (ClaimView.HeldTask), which the
// daemon names to the claim in that turn (internal/daemon's HeldBehind). The record is the
// refusal's own: the take accepted once that turn has ended leaves the view naming none, and so
// does a refusal by any other hold. A take that fails decides nothing, so it leaves the task
// named. A held task retired outside the take — a newer task in its place, or its phase ended —
// is named by nobody either, though no take was decided since.
func TestATakeRefusedIssueInTurnRecordsTheClaimItWaitsBehind(t *testing.T) {
	type arrangement struct {
		inTurn, phaseHolds bool
		phaseErr           error
	}
	// held is the arrangement of TestTakeIsRefusedByEachHoldByName's issue-in-turn case: the
	// claim's take of its phase task refused while another claim of the issue is in a turn.
	held := func(t *testing.T, a *arrangement) (*harness, uint64) {
		t.Helper()
		a.inTurn, a.phaseHolds = true, true
		h := newHarness(t)
		h.deps.Identity = adoptingIdentity
		h.deps.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testerToken, a.inTurn }
		h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return a.phaseHolds, a.phaseErr }
		h.restart()
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, ID: "outbox:7", Row: 7})
		h.wantRefused(HoldIssueInTurn)
		number := h.pending().Number
		if got := h.m.View().HeldTask; got != number {
			t.Fatalf("after the issue-in-turn refusal the view names task %d held, want the pending task %d", got, number)
		}
		return h, number
	}
	namesNone := func(t *testing.T, h *harness, after string) {
		t.Helper()
		if got := h.m.View().HeldTask; got != 0 {
			t.Fatalf("after %s the view names task %d held, want none", after, got)
		}
	}

	t.Run("the holder's turn ends and the take is accepted", func(t *testing.T) {
		var a arrangement
		h, _ := held(t, &a)
		a.inTurn = false
		h.must(IssueTurnEnded{Claim: h.token})
		if text := h.mustTake(); text != "implement the plan" {
			t.Fatalf("the take after the holder's turn ended = %q, want its task", text)
		}
		namesNone(t, h, "the accepted take")
	})

	// Another claim's turn end reaches the held claim while the holder is still in its turn, and
	// the issue-record read the take makes (phaseHolds) fails once: nothing was decided, so the
	// held take stays named to the holder, whose turn end still takes it once the store answers.
	t.Run("a record read that fails while the holder is still in its turn", func(t *testing.T) {
		var a arrangement
		h, number := held(t, &a)
		a.phaseErr = errBoom
		if err := h.handle(IssueTurnEnded{Claim: h.token}); err == nil {
			t.Fatal("the take at another claim's turn end decided with the record read failing: this case proves nothing")
		}
		if got := h.m.View().HeldTask; got != number {
			t.Fatalf("after one failed record read while the holder's turn still ran, the view names task %d held, want %d: that turn's end still takes it", got, number)
		}
		a.phaseErr = nil
		a.inTurn = false
		h.must(IssueTurnEnded{Claim: h.token})
		if text := h.mustTake(); text != "implement the plan" {
			t.Fatalf("the take at the holder's turn end = %q, want its task", text)
		}
		namesNone(t, h, "the accepted take")
	})

	t.Run("a take refused by the stale-plugin hold", func(t *testing.T) {
		var a arrangement
		h, _ := held(t, &a)
		h.m.deps.PluginContract = refreshContract
		h.m.deps.PluginRelaunchable = true
		h.wantRefused(HoldStalePlugin)
		namesNone(t, h, "the stale-plugin refusal")
	})

	t.Run("a newer task takes the held task's place", func(t *testing.T) {
		var a arrangement
		h, number := held(t, &a)
		a.phaseHolds = false
		h.must(RequestDeliver{Claim: h.token, Task: "look at production after the merge", ID: "outbox:9", Row: 9})
		if p := h.pending(); p.Number == number {
			t.Fatalf("pending %+v, want the newer task in the held task's place", p)
		}
		namesNone(t, h, "the newer task took the held task's place")
	})

	t.Run("the held task's phase ends", func(t *testing.T) {
		var a arrangement
		h, _ := held(t, &a)
		h.must(RequestSuspend{Claim: h.token, Reason: "LEGION-209 left implementing", By: SuspendedByDaemon})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the daemon's suspension, want the phase's task retired with its phase", p)
		}
		namesNone(t, h, "the phase's end retired the held task")
	})
}

// A take sent again while the first is still being answered — its adoption of the working copy
// still running — waits for that answer and is answered from it: one adoption, one acceptance,
// and one answer logged, which is no repeat from the record (answerAgain). The test runs in a
// bubble, so it reads the moment both takes are blocked rather than sleeping until they may be.
func TestTakeSentTwiceWhileTheFirstIsAnsweredWaitsForItsAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		gate := newGatedAdopt(h.rt)
		h.deps.Runtime = gate
		h.deps.Identity = adoptingIdentity
		h.restart()
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
		value := h.seen()
		request := RequestTake{Claim: testToken, Generation: h.generation(), Session: session,
			Number: value.Number, Revision: value.revision()}

		var wg sync.WaitGroup
		answers := make([]TakeAnswer, 2)
		errs := make([]error, 2)
		for i := range answers {
			wg.Go(func() { answers[i], errs[i] = h.m.Take(h.ctx, request) })
			// Every goroutine of the bubble is blocked now: the first take at the gate in its
			// adoption, and the second waiting for the first's answer — where a take that did not
			// wait would be at the gate with an adoption of its own.
			synctest.Wait()
		}
		if started := len(gate.entered); started != 1 {
			t.Fatalf("%d adoptions started while the first take was being answered, want the first's alone", started)
		}
		gate.release <- struct{}{}
		wg.Wait()
		h.m.Wait()

		for i := range answers {
			if errs[i] != nil || answers[i].Outcome != TakeOutcomeAccepted || answers[i].Text != "implement the plan" {
				t.Fatalf("take %d = %+v, %v, want it accepted with the task's text", i+1, answers[i], errs[i])
			}
		}
		h.wantCalls("AdoptWorkingCopy", 1)
		answered := h.records.of(answeredLine)
		if len(answered) != 1 {
			t.Fatalf("%q was logged %d times, want once: the second take is answered from the first's answer", answeredLine, len(answered))
		}
		wantAttr(t, answered[0], "repeat", "false")
	})
}

// A take of a number already accepted whose turn has not started is a take whose answer was lost:
// it is answered the same text and records nothing — no write, no second telling, no second
// adoption.
func TestTakeRepeatedIsAnsweredTheSameAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	confirmed := 0
	h.deps.TaskConfirmed = func(Claim) { confirmed++ }
	h.deps.Identity = adoptingIdentity
	h.restart()
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	first := h.mustTake()
	puts, log := len(h.store.history()), h.store.deliveryLog()

	if again := h.mustTake(); again != first {
		t.Fatalf("the take sent again was answered %q, want %q", again, first)
	}
	if got := len(h.store.history()); got != puts {
		t.Errorf("the repeated take wrote the claim %d times, want none", got-puts)
	}
	if got := h.store.deliveryLog(); len(got) != len(log) {
		t.Errorf("the repeated take wrote the delivery: %v", got[len(log):])
	}
	if confirmed != 1 {
		t.Errorf("the workflow was told %d times, want once", confirmed)
	}
	h.wantCalls("AdoptWorkingCopy", 1)
}

// A take of a number the record does not owe the claim — a later number than any written, an
// earlier task's, or any number once nothing is pending — is refused not-owed and records
// nothing: no seen revision, no take state, no write.
func TestTaskNumberOfAnotherTaskIsRefusedAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	value := h.seen()
	before, puts := h.claim(), len(h.store.history())

	// At a revision newer than the one seen, which a take of the owed task records as seen.
	later := TaskValue{Number: value.Number + 1, Epoch: value.Epoch, Revision: value.Revision + 1}
	if answer, err := h.takeValue(later); err != nil || answer.Outcome != TakeOutcomeRefused || answer.Hold != HoldNotOwed {
		t.Fatalf("the take of task %d = %+v, %v, want it refused not-owed", later.Number, answer, err)
	}
	after := h.claim()
	if after.TaskTakeState != before.TaskTakeState || after.TaskSeenRevision != before.TaskSeenRevision || after.TaskTaken != 0 {
		t.Fatalf("the claim moved from %+v to %+v, want nothing of the refused take recorded", before, after)
	}
	if got := len(h.store.history()); got != puts {
		t.Fatalf("the refused take wrote the claim %d times, want none", got-puts)
	}
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v, want the owed task untaken", p)
	}

	// The task finished, a take of its number is not owed either: nothing is pending, and then
	// the next task is.
	h.mustTake()
	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if answer, err := h.takeValue(value); err != nil || answer.Outcome != TakeOutcomeRefused || answer.Hold != HoldNotOwed {
		t.Fatalf("the take of the finished task = %+v, %v, want it refused not-owed", answer, err)
	}
	h.must(RequestDeliver{Claim: testToken, Task: "the next task"})
	if answer, err := h.takeValue(value); err != nil || answer.Outcome != TakeOutcomeRefused || answer.Hold != HoldNotOwed {
		t.Fatalf("the take of the earlier task with the next pending = %+v, %v, want it refused not-owed", answer, err)
	}
}

// A task a provider error took back keeps its number and its id: the ladder's wait refuses its
// take, and when the wait is over the same number is written again and taken again.
func TestTakeBackByAProviderErrorKeepsTheNumber(t *testing.T) {
	h := workingClaim(t)
	first := h.wantPrompts(1)[0]

	h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})

	if p := h.pending(); p.taken() || p.Number != first.Number || p.ID != first.DeliveryID {
		t.Fatalf("pending %+v after the provider error, want task %d taken back under its id", p, first.Number)
	}
	if written := h.claim().TaskWritten; written != first.Number {
		t.Fatalf("the claim has written %d tasks, want %d: a take-back writes no new number", written, first.Number)
	}
	h.advance(promptRetryFirst)
	again := h.wantPrompts(2)[1]
	if again.Number != first.Number || again.DeliveryID != first.DeliveryID {
		t.Fatalf("the take after the wait = %+v, want task %d again", again, first.Number)
	}
}

// The note a provider error leaves is in front of the text of the next accepted take of its task,
// and only of that take's: the turn that follows ends with no error, and the next task is the
// task alone.
func TestTakeAfterAProviderErrorCarriesItsNote(t *testing.T) {
	h := workingClaim(t)
	h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
	note := h.pending().ProviderErrorNote
	if note == "" {
		t.Fatal("the provider error left no note on the task")
	}

	h.advance(promptRetryFirst)

	text := h.wantPrompts(2)[1].Message
	if !strings.HasPrefix(text, note) || !strings.HasSuffix(text, providerErrorTaskText) {
		t.Fatalf("the take after the wait was answered %q, want the note %q in front of the task", text, note)
	}

	h.must(StreamTurnStart{Claim: h.token})
	h.must(StreamTurnEnd{Claim: h.token})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the turn ended with no error, want the task finished", p)
	}
	h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
	if next := h.wantPrompts(3)[2].Message; next != "the next task" {
		t.Fatalf("the next task's take was answered %q, want the task alone", next)
	}
}

// A write of the claim's value the bucket refuses fails nothing of the decision that made it: the
// task is queued all the same, and the value is written at the claim's next chance, whatever the
// worker last asked (valueUnwritten) — here the agent's ready.
func TestTakeValueTheBucketRefusedIsWrittenAtTheNextChance(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.values.fail("Write", errBoom)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	if _, held := h.values.value(testToken); held {
		t.Fatal("the bucket holds a value it refused to write")
	}
	if lines := h.logs.lines("supervise: the task value could not be written"); len(lines) != 1 {
		t.Fatalf("logged %q, want the refused write named once", lines)
	}
	number := h.pending().Number

	h.values.fail("Write", nil)
	h.ready()

	value, held := h.values.value(testToken)
	if !held || value.Number != number {
		t.Fatalf("the bucket holds %+v (held %v) after the next chance, want task %d's value", value, held, number)
	}
	if c := h.claim(); c.TaskWrittenRevision.Revision != value.Revision {
		t.Fatalf("the claim records the write at revision %d, want the bucket's %d", c.TaskWrittenRevision.Revision, value.Revision)
	}
	if text := h.wantPrompts(1)[0].Message; text != "the task" {
		t.Fatalf("the worker took %q, want the task", text)
	}
}

// A relaunched worker takes the task its dead process was running once, under the same number:
// its registration hands it the value and it asks once per revision; nothing writes a second
// number, and a later ready or sweep gives it nothing more to take.
func TestRelaunchTakesTheSameNumberOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	first := h.wantPrompts(1)[0]

	h.observe(runtime.Gone)
	h.relaunched()

	again := h.wantPrompts(2)[1]
	if again.Number != first.Number {
		t.Fatalf("the relaunched worker took task %d, want %d", again.Number, first.Number)
	}
	h.ready()
	h.observe(runtime.Alive)
	h.wantPrompts(2)
	if written := h.claim().TaskWritten; written != first.Number {
		t.Fatalf("the claim has written %d tasks, want %d", written, first.Number)
	}
}

// A released pod's role is woken by a task of its own (origin wake), which resumes the released
// claim; the process the resume starts takes that task once, and nothing gives it again.
func TestRelaunchTakesAWakeTaskOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	// The session reports at the turn's end that nothing of its own runs, as Oh My Pi's does, so the
	// release ends no work and the wake's task goes as it was given (recordEnd).
	h.reportActivity()
	h.must(RequestSuspend{Claim: testToken, By: SuspendedByReleased})
	h.wantState(StateSuspended)
	base := len(h.prompts())

	h.must(RequestDeliver{Claim: testToken, Task: "a notice woke this role", Origin: OriginWake})
	if p := h.pending(); p.Origin != OriginWake {
		t.Fatalf("pending %+v, want the wake's task", p)
	}
	h.wantState(StateLaunching)
	h.relaunched()

	taken := h.wantPrompts(base + 1)[base]
	if taken.Message != "a notice woke this role" {
		t.Fatalf("the relaunched worker took %+v, want the wake's task", taken)
	}
	h.ready()
	h.observe(runtime.Alive)
	h.wantPrompts(base + 1)
}

// A woken claim takes the task it holds first. A released pod's role is woken by a task of its
// own (origin wake), which resumes the claim; a second task that arrives while the claim is
// launching finds the wake's task untaken and still owed, and is refused behind it
// (ErrDeliveryPending) rather than put in its place (queue). The process the resume starts takes
// the wake's task under its number, and the second task's retry after the wake's turn has ended is
// queued and taken next (R5 of dispatch://INFRA-4130).
//
// The negative control is queue replacing the untaken owed task with the newer one: the first
// take is accepted on the newer task, and the wake's task is never taken.
func TestAWokenClaimTakesItsHeldTaskFirstWhileANewerTaskWaits(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	// The session reports at the turn's end that nothing of its own runs, as Oh My Pi's does, so the
	// release ends no work and the wake's task goes as it was given (recordEnd).
	h.reportActivity()
	h.must(RequestSuspend{Claim: testToken, By: SuspendedByReleased})
	h.wantState(StateSuspended)
	base := len(h.prompts())

	h.must(RequestDeliver{Claim: testToken, Task: "a notice woke this role", Origin: OriginWake})
	wake := h.pending()
	if wake.Origin != OriginWake {
		t.Fatalf("pending %+v, want the wake's task", wake)
	}
	h.wantState(StateLaunching)

	newer := RequestDeliver{Claim: testToken, Task: "review the pull request", ID: "outbox:42", Row: 42, Generation: 7}
	if err := h.handle(newer); !errors.Is(err, ErrDeliveryPending) {
		t.Errorf("the second task while the woken claim launches = %v, want %v behind the wake's untaken task", err, ErrDeliveryPending)
	}
	h.relaunched()

	first := h.wantPrompts(base + 1)[base]
	if first.Number != wake.Number || first.Message != "a notice woke this role" {
		t.Fatalf("the relaunched worker's first take = %+v, want the wake's task %d it holds", first, wake.Number)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the wake's turn ended, want the wake's task finished", p)
	}

	h.must(newer)
	next := h.wantPrompts(base + 2)[base+1]
	if next.Number == wake.Number || next.Message != "review the pull request" {
		t.Fatalf("the take after the wake's turn = %+v, want the second task's retry queued and taken next", next)
	}
}

// What a registering worker is told of its task (CurrentTask): the value the bucket holds for the
// claim, none once its task is finished, and the number of the last task the claim finished.
func TestRegisterAnswersTheCurrentValueAndTheFinishedNumber(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	if current, err := h.m.CurrentTask(h.ctx); err != nil || current.Value != nil || current.Finished != 0 {
		t.Fatalf("CurrentTask before any task = %+v, %v, want no value and nothing finished", current, err)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	number := h.pending().Number
	current, err := h.m.CurrentTask(h.ctx)
	if err != nil || current.Value == nil || current.Value.Number != number || current.Finished != 0 {
		t.Fatalf("CurrentTask with task %d pending = %+v, %v, want its value and nothing finished", number, current, err)
	}
	if want, _ := h.values.value(testToken); *current.Value != want {
		t.Fatalf("CurrentTask's value = %+v, want the bucket's %+v", *current.Value, want)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.must(StreamTurnEnd{Claim: testToken})
	if current, err := h.m.CurrentTask(h.ctx); err != nil || current.Value != nil || current.Finished != number {
		t.Fatalf("CurrentTask after task %d finished = %+v, %v, want no value and %d finished", number, current, err, number)
	}
}

// A task's value is written once when it is queued, under its number, and recorded on the claim
// at the epoch and revision the write answered; it is deleted when the record drops the task —
// here the daemon's suspension that ends the task's phase.
func TestTaskValueIsWrittenAtTheQueueAndDeletedWhenTheRecordDropsTheTask(t *testing.T) {
	h := newHarness(t)
	h.reach(StateRegistered)

	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})

	written := h.values.written()
	if len(written) != 1 || written[0].Number != 1 {
		t.Fatalf("values written = %+v, want task 1's one value", written)
	}
	if c := h.claim(); c.TaskWritten != 1 || c.TaskWrittenRevision != written[0].revision() {
		t.Fatalf("the claim records written %d at %q/%d, want 1 at the write's %q/%d",
			c.TaskWritten, c.TaskWrittenRevision.Epoch, c.TaskWrittenRevision.Revision, written[0].Epoch, written[0].Revision)
	}
	if stored := h.store.load(testToken); stored.TaskWritten != 1 || stored.Pending == nil || stored.Pending.Number != 1 {
		t.Fatalf("the stored claim records written %d with pending %+v, want task 1 durable with its number", stored.TaskWritten, stored.Pending)
	}

	h.must(RequestSuspend{Claim: testToken, Reason: "the issue left the workflow", By: SuspendedByDaemon})

	if deleted := h.values.deleted(); len(deleted) != 1 || deleted[0] != testToken {
		t.Fatalf("values deleted = %v, want the claim's value deleted with the dropped task", deleted)
	}
	if _, held := h.values.value(testToken); held {
		t.Fatal("the bucket still holds the dropped task's value")
	}
}

// A take from a claim a restart restored working, holding an owed task no take of it accepted,
// whose turn this daemon has heard nothing of, is the worker's word that its host is idle (R3):
// it is accepted, and the turn that follows is the take's own.
func TestTakeFromAClaimRestoredWorkingIsTheWorkersWordThatItIsIdle(t *testing.T) {
	restored := fixture(StateWorking)
	restored.Pending.ConfirmedAt, restored.TaskTaken = time.Time{}, 0
	h := newBareHarness(t)
	h.restore(restored)
	h.manualTakes()

	if text := h.mustTake(); text != "the task" {
		t.Fatalf("the take of the restored working claim = %q, want the task", text)
	}
	if p := h.pending(); !p.awaitsTurn() {
		t.Fatalf("pending %+v, want the take accepted and its turn to come", p)
	}

	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	if p := h.pending(); !p.turnStarted() {
		t.Fatalf("pending %+v, want the turn that followed the take's own", p)
	}
}

// decisionFailsHarness is a ready claim queued a phase task whose take's decision fails: the
// record's phase read fails (errBoom) until the case clears the answer it returns, and then
// answers that the phase holds.
func decisionFailsHarness(t *testing.T) (*harness, *bool) {
	t.Helper()
	failing := true
	h := waitsHarness(t, func(d *Deps) {
		d.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) {
			if failing {
				return false, errBoom
			}
			return true, nil
		}
	})
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	return h, &failing
}

// A take whose decision fails on a read of the store before it is answered — the connection
// dropped under the read of the issue's phase — is answered with no outcome, which the take route
// answers 500 CLAIM_WRITE_FAILED. The plugin sends no take again after an answer that carries a
// code, and asks again only at a newer revision of the value (pi-shared's task-take.ts,
// unanswered), so the record, which still owes the task with no hold standing, writes the value
// again under its number (R2: whenever the record owes a claim a task the claim is free to take);
// otherwise the task never reaches the worker and no bound charges anyone for it.
func TestTakeWhoseDecisionFailedIsWrittenAgainForTheWorkerToAsk(t *testing.T) {
	h, failing := decisionFailsHarness(t)
	asked := h.seen()
	if answer, err := h.takeValue(asked); answer.Outcome != "" || err == nil {
		t.Fatalf("take while the phase read fails = %+v, %v; want no answer and the read's failure", answer, err)
	}
	*failing = false

	h.advance(takeBound)
	h.observe(runtime.Alive)

	value, held := h.values.value(h.token)
	if !held || value.Number != asked.Number || value == asked {
		t.Fatalf("a first bound after the failed take the bucket holds %+v (held %t), want task %d written again at a newer revision than %d, the plugin's only signal to ask again",
			value, held, asked.Number, asked.Revision)
	}
	if answer, err := h.takeValue(value); answer.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take at the newer revision = %+v, %v; want it accepted", answer, err)
	}
	h.wantCharges()
}

// A take of a relaunched process that meets its predecessor's take still in flight — in the
// adoption of the working copy it made with the machine's lock let go — is not that take sent
// again, though it names the same number at the same revision (nothing is written at a
// relaunch): it comes from another launch. It waits for the old take and is decided afresh, so
// what the new process asked is recorded and answered, rather than handed the old take's answer,
// which is the refusal of a launch the claim has left.
//
// The negative control is the in-flight take joined on number, epoch and revision alone
// (sameTake before this change): the new process is answered the old take's answer, and no
// adoption is made for it.
func TestTakeOfARelaunchedProcessIsDecidedAfreshWhileItsPredecessorsTakeAdopts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		gate := newGatedAdopt(h.rt)
		h.deps.Runtime = gate
		h.deps.Identity = adoptingIdentity
		h.restart()
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
		value := h.seen()
		old := RequestTake{Claim: testToken, Generation: h.generation(), Session: session,
			Number: value.Number, Revision: value.revision()}

		var wg sync.WaitGroup
		var oldAnswer, newAnswer TakeAnswer
		var oldErr, newErr error
		wg.Go(func() { oldAnswer, oldErr = h.m.Take(h.ctx, old) })
		synctest.Wait()
		if started := len(gate.entered); started != 1 {
			t.Fatalf("%d adoptions started, want the old process's take in its adoption", started)
		}

		h.observe(runtime.Gone)
		h.relaunched()
		if now, _ := h.values.value(testToken); now != value {
			t.Fatalf("the bucket holds %+v after the relaunch, want the same value %+v: nothing is written at a relaunch", now, value)
		}
		next := old
		next.Generation = h.generation()
		wg.Go(func() { newAnswer, newErr = h.m.Take(h.ctx, next) })
		synctest.Wait()
		gate.release <- struct{}{}
		synctest.Wait()
		gate.release <- struct{}{}
		wg.Wait()
		h.m.Wait()

		if oldErr == nil && oldAnswer.Outcome == TakeOutcomeAccepted {
			t.Fatalf("the old process's take was answered %+v, want it not accepted: its process is gone, and the take the claim holds is the relaunched process's", oldAnswer)
		}
		if newErr != nil || newAnswer.Outcome != TakeOutcomeAccepted || newAnswer.Text != "implement the plan" {
			t.Fatalf("the relaunched process's take = %+v, %v, want it decided afresh and accepted with the task's text", newAnswer, newErr)
		}
		if p := h.pending(); !p.awaitsTurn() {
			t.Fatalf("pending %+v, want the new process's take accepted and its turn to come", p)
		}
		h.wantCalls("AdoptWorkingCopy", 2)
	})
}

// A turn that starts while a take's adoption of the working copy runs, with the machine's lock let
// go, is not that take's turn: no take was accepted when it started, so it is the agent's own and
// its start line names no task. The adoption's outcome then reaches a working claim, which
// refuses the take in-turn (adoptedTooLate), so the workflow is told nothing. The refusal is
// recorded at the value's revision, so the turn's end writes the value again at a newer one
// (offerPending), and the worker's take at that revision is accepted (R3 of
// dispatch://INFRA-4130).
//
// The negative control is the adoption's outcome accepted over a working claim (adopted asked
// there, passing over the in-turn hold): the take is accepted into the running turn and the
// workflow told.
func TestATurnThatStartsDuringTheAdoptionIsNotTheTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		gate := newGatedAdopt(h.rt)
		h.deps.Runtime = gate
		h.deps.Identity = adoptingIdentity
		confirmed := 0
		h.deps.TaskConfirmed = func(Claim) { confirmed++ }
		h.restart()
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
		value := h.seen()
		request := RequestTake{Claim: testToken, Generation: h.generation(), Session: session,
			Number: value.Number, Revision: value.revision()}

		var wg sync.WaitGroup
		var answer TakeAnswer
		var err error
		wg.Go(func() { answer, err = h.m.Take(h.ctx, request) })
		synctest.Wait()
		if started := len(gate.entered); started != 1 {
			t.Fatalf("%d adoptions started, want the take in its adoption", started)
		}
		h.must(StreamTurnStart{Claim: testToken})
		gate.release <- struct{}{}
		wg.Wait()
		h.m.Wait()

		if err != nil || answer.Outcome != TakeOutcomeRefused || answer.Hold != HoldInTurn {
			t.Fatalf("the take whose adoption a turn's start overtook = %+v, %v, want it refused %q", answer, err, HoldInTurn)
		}
		wantTurnLine(t, h, startedLine, h.generation(), 0)
		if p := h.pending(); p.taken() {
			t.Fatalf("pending %+v after the refusal, want the task untaken: the turn is the agent's own", p)
		}
		if confirmed != 0 {
			t.Fatalf("the workflow was told %d times of a take that was refused, want never", confirmed)
		}

		h.must(StreamTurnEnd{Claim: testToken})

		again, held := h.values.value(testToken)
		if !held || again.Number != value.Number || !again.revision().after(value.revision()) {
			t.Fatalf("after the turn's end the bucket holds %+v (held %t), want task %d written again at a revision newer than %d",
				again, held, value.Number, value.Revision)
		}
		gate.release <- struct{}{}
		if answer, err := h.takeValue(again); err != nil || answer.Outcome != TakeOutcomeAccepted || answer.Text != "implement the plan" {
			t.Fatalf("the take at the newer revision = %+v, %v, want it accepted with the task's text", answer, err)
		}
		if confirmed != 1 {
			t.Fatalf("the workflow was told %d times of the accepted take, want once", confirmed)
		}
	})
}

// A delete of the claim's value the bucket refuses is made again at the claim's next chance
// while the record owes the claim no task, the sweep's observation among them, so a finished
// task's value does not outlive it until the claim's next task or the daemon's next boot (R1).
// A task queued meanwhile has its own value in the bucket, which that chance must not delete.
//
// The negative controls are the delete made again nowhere (offerPending with no deleteAgain):
// the finished task's value stands after the next chance; and the delete made again whatever
// the record owes (deleteAgain before offerPending's check of the pending task): the new task's
// value is deleted.
func TestTaskValueDeleteTheBucketRefusedIsMadeAgainAtTheNextChance(t *testing.T) {
	t.Run("the delete is made again at the next chance", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		number := h.pending().Number
		h.values.fail("Delete", errBoom)
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the turn was reported, want task %d finished", p, number)
		}
		if _, held := h.values.value(h.token); !held {
			t.Fatal("the bucket holds no value after a delete it refused")
		}

		h.values.fail("Delete", nil)
		h.observe(runtime.Alive)

		if value, held := h.values.value(h.token); held {
			t.Fatalf("the bucket still holds %+v after the next chance, want the finished task's value deleted", value)
		}
		deleted := h.records.of(deletedLine)
		if len(deleted) != 1 {
			t.Fatalf("%q was logged %d times, want once", deletedLine, len(deleted))
		}
		wantAttr(t, deleted[0], "cause", valueDeletedFinished)
	})
	t.Run("a task written meanwhile is not deleted", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		h.values.fail("Delete", errBoom)
		h.must(StreamTurnEnd{Claim: h.token})
		h.values.fail("Delete", nil)
		h.must(RequestDeliver{Claim: h.token, Task: "the next task"})
		next := h.pending().Number

		h.observe(runtime.Alive)

		if value, held := h.values.value(h.token); !held || value.Number != next {
			t.Fatalf("the bucket holds %+v (held %v), want task %d's value", value, held, next)
		}
		if deleted := h.values.deleted(); len(deleted) != 0 {
			t.Fatalf("values deleted %v, want none: the record owes task %d", deleted, next)
		}
	})
}

// A take whose accept the store refused is refused at its revision and its value written again
// once the task is free to take (undecided), but not while the claim still owes the write the
// store did not take: the store is away, a take at the new revision would fail in turn, and a
// task whose take reads nothing of the store — one of no phase — would go round a write, a take
// and a failed answer at every sweep for as long as the store is away. The value is written once
// the owed write lands, and the take at that revision is accepted.
//
// The negative control is the rewrite made whatever the claim owes (offerPending with no check
// of the owed write): the first sweep of the outage writes the value again.
func TestTakeFailedOnTheStoreIsWrittenAgainOnlyOnceTheOwedWriteLands(t *testing.T) {
	h := newHarness(t)
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "the operator's task"})
	asked := h.seen()
	h.store.fail("PutClaim", errBoom)
	h.store.fail("PutDelivery", errBoom)
	if answer, err := h.takeValue(asked); answer.Outcome != "" || err == nil {
		t.Fatalf("the take while the store refuses writes = %+v, %v; want no answer and the write's failure", answer, err)
	}
	written := len(h.values.written())

	for sweep := 1; sweep <= 3; sweep++ {
		_ = h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Alive, At: h.clock.Now()}})
		if got := h.values.written(); len(got) != written {
			t.Fatalf("sweep %d of the outage wrote the value again: %+v", sweep, got[written:])
		}
	}

	h.store.fail("PutClaim", nil)
	h.store.fail("PutDelivery", nil)
	h.observe(runtime.Alive)

	rewritten := h.values.written()
	if len(rewritten) != written+1 {
		t.Fatalf("the values written once the store answered are %+v, want one more than the %d before", rewritten, written)
	}
	value := rewritten[len(rewritten)-1]
	if value.Number != asked.Number || value.Revision <= asked.Revision {
		t.Fatalf("the store's return wrote %+v, want task %d again at a revision past %d", value, asked.Number, asked.Revision)
	}
	lines := h.records.of(writtenLine)
	wantAttr(t, lines[len(lines)-1], "cause", TaskValueTakeFailed)
	if answer, err := h.takeValue(value); answer.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take at the newer revision = %+v, %v; want it accepted", answer, err)
	}
}

// TestAHeldTakeIsNamedNoLongerOnceItsClaimCannotTakeIt holds ClaimView.HeldTask to its own word: it
// names a task a take of which waits for another claim's turn, "and the task is taken once that
// turn ends". A claim that is suspended or retired while its take stands refused issue-in-turn
// keeps its pending task untaken, and no takeable runs for it, so the end of the other claim's turn
// takes nothing; its view must name no held task then, or the daemon's HeldBehind names that task
// to every claim of the issue that starts a turn, each of whose workers is told a task waits for
// its turn's end (dispatch://INFRA-4130 R2) that never starts then.
func TestAHeldTakeIsNamedNoLongerOnceItsClaimCannotTakeIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(h *harness)
	}{
		{"an operator suspends the held claim", func(h *harness) {
			h.must(RequestSuspend{Claim: h.token, Reason: "drain", By: SuspendedByOperator})
			h.wantState(StateSuspended)
		}},
		{"the held claim's pod is released", func(h *harness) {
			h.must(RequestSuspend{Claim: h.token, By: SuspendedByReleased})
			h.wantState(StateSuspended)
		}},
		{"an operator stops the held claim", func(h *harness) {
			h.must(RequestStop{Claim: h.token})
			h.wantState(StateRetired)
		}},
		{"the held claim's issue closes", func(h *harness) {
			h.must(RequestIssueClose{Claim: h.token})
			h.wantState(StateRetired)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.deps.Identity = adoptingIdentity
			h.deps.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return testerToken, true }
			h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return true, nil }
			h.restart()
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, ID: "outbox:7", Row: 7})
			h.wantRefused(HoldIssueInTurn)
			number := h.pending().Number
			if got := h.m.View().HeldTask; got != number {
				t.Fatalf("after the issue-in-turn refusal the view names task %d held, want the pending task %d", got, number)
			}
			tc.end(h)
			if got := h.m.View().HeldTask; got != 0 {
				t.Fatalf("the claim is %s with its pending task %d untaken, and its view still names task %d held behind another claim's turn; want none, since that turn's end does not have it taken",
					h.claim().State, number, got)
			}
		})
	}
}
