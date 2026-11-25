package supervise

// The held stop (rule two of T3 of dispatch://INFRA-2811's plan v150, rows 39e to 39h of its state
// table; the architect's ruling dispatch://INFRA-2811/comment/11ddec8c-9099-4b8c-b040-acdfcfcf0638):
// the operator's stop of one claim that reaches it after its take's turn ended and before that
// turn's report reached the daemon waits for the report, as a suspension does, with the held
// repoint's bound — the report wait's own over a whole view, the reconnect deadline over a view not
// whole — and runs at the task's finish, or at the bound once the task is taken back with the note
// of a cut turn. Only the operator's stop route asks for the hold (RequestStop.WaitForReport, which
// api's stopEvent sets and operatorStop below posts as it does); the stops that end the work for
// good or must not wait stop at once as before. Each test is named so c2's Take selects it.
//
// The negative control of each is e63fe1f5 (e63fe1f57842a88c48f18bee9054ef6b6a97d8a0), where
// RequestStop has no WaitForReport, the operator's stop route posts RequestStop{Claim} (api's
// stopEvent, operator.go:581) and stop ends the claim at once (table.go:2697, end): run there,
// operatorStop is that post, the stop retires the claim before the report comes, and settle takes
// back as a cut turn a task whose turn ran whole, which a retry of the stopped claim's session then
// runs again. Row 39h's own stops end the claim at once there as here, as the row says they must;
// each of its cases fails there on its control, the operator's stop of a claim at the same point,
// which that commit does not hold.

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// operatorStop is the operator's stop of one claim, as the operator's stop route posts it (api's
// stopEvent): the one stop that asks for the hold.
func operatorStop(token claim.Token) RequestStop {
	return RequestStop{Claim: token, WaitForReport: true}
}

// overATakesEndedTurn is claim c on the current plugin with R5's bounds set, every take of it
// driven by the test, whose worker took a task and ran that take's turn to its end, its report
// still to come. It answers the task's number.
func overATakesEndedTurn(t *testing.T, c Claim) (*harness, uint64) {
	t.Helper()
	h := newHarnessOf(t, c, withWaits, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf("implement the plan")
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	return h, number
}

// wantStopHeld holds that the operator's stop, answered err, waits for the report of the take's
// turn: it answered ErrSuspendHeld, which the stop route answers 202 with the claim as it is, the
// claim is as wantStopStillHeld holds, and the one line of the hold names task number and bound.
func (h *harness) wantStopHeld(err error, number uint64, kept runtime.Locator, bound string) {
	h.t.Helper()
	if !errors.Is(err, ErrSuspendHeld) {
		h.t.Fatalf("the operator's stop answered %v and left the claim %s, want ErrSuspendHeld: the stop held for the report of task %d's turn, which the route answers 202",
			err, h.state(), number)
	}
	h.wantStopStillHeld(number, kept, "at the operator's stop")
	held := h.heldStopRecords()
	if len(held) != 1 {
		h.t.Fatalf("%q was logged with held=stop %d times, want once", reportWaitsLine, len(held))
	}
	wantAttr(h.t, held[0], "task", strconv.FormatUint(number, 10))
	wantAttr(h.t, held[0], "bound", bound)
}

// heldStopRecords is every record of reportWaitsLine logged with held=stop: the line of a held
// stop (holdStop).
func (h *harness) heldStopRecords() []loggedRecord {
	var held []loggedRecord
	for _, r := range h.records.of(reportWaitsLine) {
		if what, ok := r.attr("held"); ok && what.String() == "stop" {
			held = append(held, r)
		}
	}
	return held
}

// wantStopStillHeld holds that the stop still waits: the claim idle over the process it ran,
// nothing released, and the take of task number standing with its turn started.
func (h *harness) wantStopStillHeld(number uint64, kept runtime.Locator, when string) {
	h.t.Helper()
	if state := h.state(); state != StateIdle {
		h.t.Fatalf("%s the claim is %s, want idle over the process the stop waits on", when, state)
	}
	if got := h.locator(); got != kept {
		h.t.Fatalf("%s the claim records %+v, want the process the stop waits on, %+v", when, got, kept)
	}
	h.wantCalls("Release", 0)
	h.wantTakesTurnStanding(number, when)
}

// wantStoppedFrom holds that the stop ran: the claim retired, the one release of it the release of
// the process kept, and nothing charged.
func (h *harness) wantStoppedFrom(kept runtime.Locator) {
	h.t.Helper()
	h.wantState(StateRetired)
	if released := h.wantCalls("Release", 1)[0].Released; released.Locator == nil || *released.Locator != kept {
		h.t.Fatalf("the stop released %+v, want the process it waited on, %+v", released.Locator, kept)
	}
	h.wantBudgets(Budgets{})
	h.wantNoFailure()
}

// wantStoppedCuttingTheTurn holds that the stop ended the claim under its own cause and took the
// task of its take's ended turn back with the note of a cut turn, its report never having come.
func (h *harness) wantStoppedCuttingTheTurn(number uint64, kept runtime.Locator) {
	h.t.Helper()
	h.wantStoppedFrom(kept)
	h.wantTakenBackWithItsNote(number)
	if cause := h.claim().EndedWork.Cause; cause != endStopped {
		h.t.Fatalf("the let-go recorded the cause %q, want the stop's %q", cause, endStopped)
	}
}

// retriedAndTaken is the operator's retry of the stopped claim, brought up to ready, and its
// worker's take of the task it is offered.
func retriedAndTaken(h *harness) {
	h.t.Helper()
	retriedByTheOperator(h)
	h.mustTake()
}

// Row 39e: a take's turn ended, its report not yet at the daemon, and the operator stops the claim:
// the stop is held for the report, the claim idle over its process; the report finishes the task
// once, and the stop runs then, releasing the process and retiring the claim with nothing of the
// task left to run again.
//
// The negative control is e63fe1f5: the stop retires the claim at once, which this test reads at
// the stop.
func TestTakeWhoseTurnEndedHoldsTheOperatorsStopForItsReport(t *testing.T) {
	h, number := overATakesEndedTurn(t, queuedClaim())
	kept := h.locator()

	h.wantStopHeld(h.handle(operatorStop(h.token)), number, kept, "the report wait's bound")
	h.advance(turnStartBound - time.Second)
	h.wantStopStillHeld(number, kept, "a second bound less a second after the turn's end")

	h.mustReport(nil)

	h.wantStoppedFrom(kept)
	h.wantRunOnce(number)
}

// Row 39f: as row 39e, and no report comes before the held repoint's bound over a whole view, R5's
// second bound (TimerReport): at the bound the task is taken back with the note of a cut turn and
// the stop then runs, uncharged; a retry of the stopped claim takes the task once, behind that
// note. The bound is the one armed at the turn's end over a take this daemon accepted; over a take
// a daemon start restored, which the bound leaves out with nothing held (reportReaches), the held
// stop counts as held work (workHeld), so the bound arms whole from the stop.
//
// The negative control is e63fe1f5: the stop retires the claim at once, which this test reads at
// the stop.
func TestTakeWhoseReportMissesItsBoundIsTakenBackBeforeAHeldStopRuns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restore func(h *harness)
	}{
		{"a take this daemon accepted", func(*harness) {}},
		{"a take a daemon start restored, its agent answering the hello's ask", func(h *harness) {
			h.t.Helper()
			h.restart()
			h.connect()
			h.answerAsk(false)
			h.wantUnarmed("over the restored take with nothing held", TimerReport)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, number := overATakesEndedTurn(t, queuedClaim())
			tc.restore(h)
			kept := h.locator()

			h.wantStopHeld(h.handle(operatorStop(h.token)), number, kept, "the report wait's bound")
			h.wantArmedFromNow(TimerReport, turnStartBound, "at the held stop")
			h.advance(turnStartBound - time.Second)
			h.wantStopStillHeld(number, kept, "a second bound less a second after the turn's end, or the stop,")

			h.advance(time.Second)

			h.wantStoppedCuttingTheTurn(number, kept)
			h.wantTakenOnceMore(number, retriedAndTaken)
		})
	}
}

// Row 39g: as row 39e, the claim's view of its turns not whole, so the report wait's own bound does
// not run and the reconnect deadline is the bound: with no report by it, the task is taken back with
// the note of a cut turn and the operator's own stop runs, uncharged — not the no-hello path's
// retirement, which charges CauseNeverReconnected — the stop route having answered 202. Both ways
// the view is cut are covered: a daemon restart, the shim's hello never coming, and a close.
//
// The negative control is e63fe1f5: the stop retires the claim at once, which this test reads at
// the stop.
func TestTakeUnderAHeldStopOverAViewNotWholeIsStoppedUnchargedAtTheReconnectDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(h *harness)
	}{
		{"a daemon restart, the shim's hello never coming", func(h *harness) { h.restart() }},
		{"a close, the shim's hello never coming", func(h *harness) { h.must(StreamClosed{Claim: h.token}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, number := overATakesEndedTurn(t, queuedClaim())
			tc.cut(h)
			due, armed := h.dueOf(TimerRegistration)
			if !armed {
				t.Fatal("no reconnect deadline is armed over the cut view: this case proves nothing")
			}
			kept := h.locator()

			h.wantStopHeld(h.handle(operatorStop(h.token)), number, kept, "the reconnect deadline")
			h.advance(due.Sub(h.clock.Now()) - time.Second)
			h.wantStopStillHeld(number, kept, "a second before the reconnect deadline")

			h.advance(time.Second)

			h.wantStoppedCuttingTheTurn(number, kept)
			if lines := h.logs.lines("supervise: the shim never reconnected"); len(lines) != 0 {
				t.Fatalf("logged %v, want no retirement of a shim that never reconnected", lines)
			}
			if cause := h.claim().PendingCause; cause != "" {
				t.Fatalf("the stopped claim carries the pending cause %q, want none: the operator's stop charges nothing", cause)
			}
			h.wantTakenOnceMore(number, retriedAndTaken)
		})
	}
}

// Row 39h: as row 39e, and the claim is ended by a stop that must not wait — the operator's close
// of the claim, the workflow's close of the tree, the controller's stop at a boot that leaves the
// controller to its operator (internal/daemon's stopLaunchedController, which posts RequestStop
// with no WaitForReport) — and each ends it at once as before, with no hold: the process released,
// the claim retired, and the task taken back with the note of a cut turn. The operator's close of
// the tree, whose stops of the tree's other claims ask no hold either, is
// TestTakeWhoseTurnEndedIsStoppedAtOnceByTheOperatorsCloseOfItsTree in internal/api, where those
// stops are posted (closeTree). Each case's control is the operator's stop of a claim at the same
// point, which is held: the case's own stop is not held there because it is not the operator's
// stop, not because no hold could be made.
//
// The negative control is e63fe1f5: every case's control fails there, the operator's stop retiring
// its claim at once.
func TestTakeWhoseTurnEndedIsStoppedAtOnceByEveryOtherStop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claim   Claim
		restart bool
		stop    func(token claim.Token) Event
	}{
		{"the operator's close of the claim", rootClaim(), false, func(token claim.Token) Event { return RequestOperatorClose{Claim: token} }},
		{"the workflow's close of the tree", queuedClaim(), false, func(token claim.Token) Event { return RequestTreeClose{Claim: token} }},
		{"the controller's stop at a boot", controllerClaim(), true, func(token claim.Token) Event { return RequestStop{Claim: token} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control, controlNumber := overATakesEndedTurn(t, queuedClaim())
			if tc.restart {
				control.restart()
			}
			controlKept := control.locator()
			bound := "the report wait's bound"
			if tc.restart {
				bound = "the reconnect deadline"
			}
			control.wantStopHeld(control.handle(operatorStop(control.token)), controlNumber, controlKept, bound)

			h, number := overATakesEndedTurn(t, tc.claim)
			if tc.restart {
				h.restart()
			}
			kept := h.locator()

			if err := h.handle(tc.stop(h.token)); err != nil {
				t.Fatalf("%s answered %v, want the claim ended at once", tc.name, err)
			}

			h.wantStoppedFrom(kept)
			h.wantTakenBackWithItsNote(number)
			if held := h.heldStopRecords(); len(held) != 0 {
				t.Fatalf("%s logged the held stop's line %v, want no hold", tc.name, held[0].attrs)
			}
		})
	}
}

// The held stop's order beside a held suspension (rule two of T3; settleThenAfterTurn, afterTurn):
// a take's turn ended with its report still to come, the operator's suspension is held for that
// report, and the operator's stop is then held with it, the stop route answering 202. The report
// finishes the task once, and the stop runs first: the claim retired, its process released and
// never suspended, nothing charged. The stop ends the claim for good, and its let-go drops the
// suspension with the process.
//
// The negative control is settleThenAfterTurn with its !m.heldStop removed (M-79): the
// suspension's stop runs at the report, its let-go drops the stop the route answered 202 for, and
// the claim ends suspended rather than retired.
func TestTakeWhoseTurnEndedRunsTheHeldStopBeforeTheSuspensionHeldWithIt(t *testing.T) {
	h, number := overATakesEndedTurn(t, queuedClaim())
	kept := h.locator()
	stops := len(h.calls("Suspend"))

	if err := h.handle(operatorSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the operator's suspension of a claim whose report is to come answered %v, want it held for the report", err)
	}
	h.wantStopHeld(h.handle(operatorStop(h.token)), number, kept, "the report wait's bound")

	h.mustReport(nil)

	h.wantStoppedFrom(kept)
	h.wantCalls("Suspend", stops)
	h.wantFinishedOnce(finishedByReport)
	h.wantRunOnce(number)
}
