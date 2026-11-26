package supervise

// The controller's actions on an agent (cybermonklabs/legion#4065: stop, retry, suspend and resume
// through its `legion` tool, recorded under the source SuspendedByController) carried onto the
// take model of dispatch://INFRA-2811: each acts through the machine's rules for a take, as the
// operator's action does. A suspension takes back a take whose turn has not started and holds its
// stop for the report of a take whose turn has ended; a stop of one agent holds for that report
// too (holdStop); a retry and a resume start a process that takes the task the claim holds once.
// Where the controller's action and the operator's meet over one held report, the operator's
// suspension outranks the controller's stop (dropOutrankedStop).

import (
	"errors"
	"testing"
	"time"
)

// controllerSuspension is the controller's suspension through its `legion` tool, as api's
// suspendAgent posts it.
func controllerSuspension(h *harness) RequestSuspend {
	return RequestSuspend{Claim: h.token, By: SuspendedByController, Reason: "the controller suspended it"}
}

// controllerStop is the controller's stop of one agent through its `legion` tool, as api's
// stopAgent posts it: a stop that waits for the report of the take's turn.
func controllerStop(h *harness) RequestStop {
	return RequestStop{Claim: h.token, By: SuspendedByController, WaitForReport: true}
}

// The controller's stop of an agent whose take's turn ended with its report still to come waits
// for that report, as the operator's stop does: it answers ErrSuspendHeld, which the agent door
// answers 202, the claim keeps its process, and the report finishes the task once before the stop
// retires the claim, charged nothing. The held stop's line names the controller.
//
// The negative control is api's stopAgent with no WaitForReport: internal/api's
// TestTakeWhoseTurnEndedHoldsTheControllersStopForItsReport fails there, the stop retiring the
// claim at once while settle takes back as a cut turn a task whose turn ran whole.
func TestTakeWhoseTurnEndedHoldsTheControllersStopForItsReport(t *testing.T) {
	h, number := overATakesEndedTurn(t, queuedClaim())
	kept := h.locator()

	h.wantStopHeld(h.handle(controllerStop(h)), number, kept, "the report wait's bound")
	wantAttr(t, h.heldStopRecords()[0], "by", string(SuspendedByController))

	h.mustReport(nil)

	h.wantStoppedFrom(kept)
	h.wantFinishedOnce(finishedByReport)
	h.wantRunOnce(number)
}

// The operator's suspension held with the controller's stop that waits for the report of the
// take's turn takes the agent over and drops that stop (dropOutrankedStop): the controller may not
// get past the operator's suspension (Claim.HeldAgainst), and its stop would retire the agent under
// it for a retry to relaunch. The report finishes the task once and the suspension runs in the
// stop's place: the claim ends suspended by the operator, its process stopped and not released,
// and the controller's resume is refused it.
//
// The negative control is holdSuspension with no dropOutrankedStop: the held stop runs first at
// the report (afterTurn), retires the claim and drops the operator's suspension with the process.
func TestTakeWhoseTurnEndedDropsTheControllersHeldStopForTheOperatorsSuspension(t *testing.T) {
	h, number := overATakesEndedTurn(t, queuedClaim())
	kept := h.locator()
	stops := len(h.calls("Suspend"))
	h.wantStopHeld(h.handle(controllerStop(h)), number, kept, "the report wait's bound")

	if err := h.handle(operatorSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the operator's suspension over the controller's held stop answered %v, want it held for the report", err)
	}

	h.mustReport(nil)

	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the claim is suspended by %q, want the operator", got)
	}
	h.wantCalls("Suspend", stops+1)
	h.wantCalls("Release", 0)
	h.wantFinishedOnce(finishedByReport)
	h.wantRunOnce(number)
	if err := h.handle(RequestResume{Claim: h.token, By: SuspendedByController}); !errors.Is(err, ErrOperatorHolds) {
		t.Fatalf("the controller's resume answered %v, want ErrOperatorHolds", err)
	}
}

// The controller's suspension of an idle agent whose take's turn ended with its report still to
// come waits for that report, as the operator's does (holdForReport): the report finishes the
// task once and the stop runs after it, the claim suspended by the controller. With no report by
// the suspension's deadline, the stop timeout from its arrival as the operator's has, the stop
// runs then and takes the task back with the note of a cut turn first, and the controller's resume
// starts a process that takes it once.
//
// The negative control is holdSuspension's deadline read for the operator alone (By ==
// SuspendedByOperator, before #4065): the controller's suspension then has no deadline, and the
// second case's stop never runs at the stop timeout.
func TestTakeWhoseTurnEndedHoldsTheControllersSuspensionForItsReport(t *testing.T) {
	t.Run("then the report", func(t *testing.T) {
		h, number, stops := reportToComeOfACurrentPlugin(t)
		if err := h.handle(controllerSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the controller's suspension answered %v, want it held for the report", err)
		}
		h.wantStillAwaitingItsReport(number, stops)

		h.mustReport(nil)

		h.wantState(StateSuspended)
		if got := h.claim().SuspendedBy; got != SuspendedByController {
			t.Fatalf("the claim is suspended by %q, want the controller", got)
		}
		h.wantCalls("Suspend", stops+1)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report, want task %d finished", p, number)
		}
		h.wantFinishedOnce(finishedByReport)
	})
	t.Run("and no report by its deadline", func(t *testing.T) {
		h, number, stops := reportToComeOfACurrentPlugin(t)
		if err := h.handle(controllerSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the controller's suspension answered %v, want it held for the report", err)
		}
		h.advance(testStop - time.Second)
		h.wantStillAwaitingItsReport(number, stops)

		h.advance(time.Second)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, func(h *harness) {
			h.must(RequestResume{Claim: h.token, By: SuspendedByController})
			h.relaunched()
		})
	})
}

// The controller's suspension of an agent whose accepted take has not started its turn takes that
// take back, unstarted, under the suspension's cause (suspended's takeBackUnstarted), as the
// operator's does: the task waits untaken through the suspension, which keeps it, since the
// controller's source keeps the phase's task (SuspendSource.RetiresPhaseTask). The controller's
// resume then starts a process that takes it once.
//
// The negative control is the controller's source retiring the phase's task (RetiresPhaseTask true
// for it): the suspended claim then holds no task, and the resumed process takes nothing.
func TestTakeAwaitingItsTurnIsTakenBackByTheControllersSuspensionAndTakenOnceAtItsResume(t *testing.T) {
	h := newHarnessOf(t, queuedClaim(), currentPlugin)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	first := h.wantPrompts(1)[0]

	h.must(controllerSuspension(h))

	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByController {
		t.Fatalf("the claim is suspended by %q, want the controller", got)
	}
	if p := h.pending(); p.Number != first.Number || p.taken() {
		t.Fatalf("the suspended claim holds %+v, want task %d taken back", p, first.Number)
	}
	if back := h.records.of(takenBackLiteral); len(back) != 1 {
		t.Fatalf("logged %d take-backs, want one", len(back))
	} else if cause, _ := back[0].attr("cause"); cause.String() != takenBackSuspended {
		t.Fatalf("the take-back names cause %q, want %q", cause, takenBackSuspended)
	}

	h.must(RequestResume{Claim: h.token, By: SuspendedByController})
	h.relaunched()

	again := h.wantPrompts(2)[1]
	if again.Number != first.Number || again.DeliveryID != first.DeliveryID {
		t.Fatalf("the resumed process took %+v, want task %d again", again, first.Number)
	}
}

// The controller's retry of an agent whose take's turn failed it (a provider error that cannot
// clear, which takes the take back first: takeBackAndFail) starts a process that takes the task
// the failed claim holds, once, as the operator's retry does (askedRetry, retry).
//
// The negative control is askedBy refusing the controller's source: the retry is then refused and
// the task waits on the failed claim.
func TestTakeOfAFailedClaimIsTakenOnceAfterTheControllersRetry(t *testing.T) {
	h := workingClaim(t)
	number := h.pending().Number
	h.errorTurn(ProviderError{Message: "invalid_request_error", Status: 400})
	h.wantState(StateFailed)
	if p := h.claim().Pending; p == nil || p.Number != number || p.taken() {
		t.Fatalf("the failed claim holds %+v, want task %d taken back", p, number)
	}

	h.must(RequestAskedRetry{Claim: h.token, By: SuspendedByController})
	h.relaunched()

	var again []taken
	for _, took := range h.prompts() {
		if took.Number == number {
			again = append(again, took)
		}
	}
	if len(again) != 2 {
		t.Fatalf("task %d was taken %d times (%+v), want twice: once before the failure and once after the controller's retry",
			number, len(again), again)
	}
}
