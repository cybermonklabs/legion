package supervise

// The held repoint (rule two of T3 of dispatch://INFRA-2811's plan, rows 39 to 39d of its state
// table): a replacement of the claim's live process — a stale-address relaunch (repoint), or a
// replacement the wake pacing parked whose turn comes — that reaches the claim after its take's
// turn ended and before that turn's report reached the daemon waits for the report, and runs at
// the task's finish, or at the wait's bound once the task is taken back with the note of a cut
// turn. The tester's row 38 is repoint_report_test.go; these are the rows around it, and the one
// case the hold is never made in: a stale address that is the daemon's own API, which no report
// can reach. Each is named so c2's Take selects it.
//
// The negative control of each is the merge b031e2e2 (b031e2e2b21043e577e4a0808657625fe36c3f28),
// where repoint replaces the process at once (repoint.go:42, replaceProcess) and a parked
// replacement's turn lets the process go at once (wake.go:633, launchWake): the task is taken back
// as a cut turn before its report, and the relaunched process runs it again.

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// overAStaleAddressAfterItsTurn is a claim on the current plugin with R5's bounds set, whose
// worker took a task and ran that take's turn to its end, its report still to come, and whose
// process the runtime then finds alive at a stale address. It answers the task's number and the
// process the repoint found.
func overAStaleAddressAfterItsTurn(t *testing.T) (h *harness, number uint64, stale runtime.Locator) {
	t.Helper()
	h, number = overATakesEndedTurn(t, queuedClaim())
	stale = h.locator()
	h.observe(runtime.StaleAddress)
	return h, number, stale
}

// wantReplacementHeld holds that the replacement of the claim's process waits for the report of
// the take's turn: the claim is idle over the process it ran, nothing was started for it and no
// wake waits, and the take of task number stands with its turn started.
func (h *harness) wantReplacementHeld(number uint64, kept runtime.Locator, starts int, when string) {
	h.t.Helper()
	if state := h.state(); state != StateIdle {
		h.t.Fatalf("%s the claim is %s, want idle over the process the replacement waits on", when, state)
	}
	if got := h.locator(); got != kept {
		h.t.Fatalf("%s the claim records %+v, want the process the replacement waits on, %+v", when, got, kept)
	}
	h.wantStarts(h.token, starts)
	if held := h.m.View().WakeWaiting; held != nil {
		h.t.Fatalf("%s the claim names wake %+v, want none: the replacement waits for the report, not for a turn of the pacing", when, held)
	}
	h.wantTakesTurnStanding(number, when)
}

// wantRelaunchedFrom holds that the claim was relaunched, uncharged, from the process kept: the
// claim is launching, the one Resume of it replaced kept, and nothing is charged.
func (h *harness) wantRelaunchedFrom(kept runtime.Locator) {
	h.t.Helper()
	h.wantState(StateLaunching)
	if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != kept {
		h.t.Fatalf("the relaunch replaced %+v, want the process the replacement waited on, %+v", resume.Previous, kept)
	}
	h.wantBudgets(Budgets{})
	h.wantNoFailure()
}

// Row 39: as row 38, a stale-address relaunch after the take's turn ended waits for that turn's
// report, and no report comes before R5's second bound (TimerReport, armed at the turn's end): at
// the bound the task is taken back with the note of a cut turn, the relaunch then runs, charged
// nothing, and the relaunched worker takes the task once, behind that note.
//
// The negative control is the merge b031e2e2: the repoint replaces the process at the stale
// observation, so the claim is launching there, which this test reads before the bound.
func TestTakeWhoseReportMissesItsBoundIsTakenBackBeforeAHeldStaleAddressRelaunchRuns(t *testing.T) {
	h, number, stale := overAStaleAddressAfterItsTurn(t)
	h.wantReplacementHeld(number, stale, 1, "at the stale observation")

	h.advance(turnStartBound - time.Second)
	h.wantReplacementHeld(number, stale, 1, "a second bound less a second after the turn's end")

	h.advance(time.Second)

	h.wantRelaunchedFrom(stale)
	h.wantTakenBackWithItsNote(number)
	h.wantTakenOnceMore(number, relaunchedAndTaken)
}

// Row 39a: the stale address is the worker stream's, so the shim cannot dial this daemon and the
// claim's view of its turns is not whole: the report wait's bound does not run there, and the
// reconnect deadline is the bound. A repoint after the take's turn ended waits for the report, and
// at the reconnect deadline with no report the task is taken back with the note of a cut turn and
// the claim is relaunched, uncharged, as repointed: not retired by the no-hello path, which charges
// CauseNeverReconnected, since a stale observation is never charged. Both ways the view is cut are
// covered: a daemon restart under the new stream, the shim's hello never coming, and a close.
//
// The negative control is the merge b031e2e2: the repoint replaces the process at the stale
// observation, so the claim is launching there, which this test reads before the deadline.
func TestTakeUnderAHeldStaleAddressRelaunchOverAViewNotWholeIsRelaunchedUnchargedAtTheReconnectDeadline(t *testing.T) {
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
			stale := h.locator()

			h.observe(runtime.StaleAddress)

			h.wantReplacementHeld(number, stale, 1, "at the stale observation")
			h.advance(due.Sub(h.clock.Now()) - time.Second)
			h.wantReplacementHeld(number, stale, 1, "a second before the reconnect deadline")

			h.advance(time.Second)

			h.wantRelaunchedFrom(stale)
			h.wantTakenBackWithItsNote(number)
			if lines := h.logs.lines("supervise: the shim never reconnected"); len(lines) != 0 {
				t.Fatalf("logged %v, want no retirement of a shim that never reconnected", lines)
			}
			if cause := h.claim().EndedWork.Cause; cause != endRepointed {
				t.Fatalf("the let-go recorded the cause %q, want the repoint's %q", cause, endRepointed)
			}
			h.wantTakenOnceMore(number, relaunchedAndTaken)
		})
	}
}

// delayedReplacement is a replacement of the claim's live process raised during its take's turn,
// which the wake pacing parks (launchReplacing, reserveWake): raise raises it, the pacing held by
// another claim's launch reserved just before.
type delayedReplacement struct {
	name  string
	raise func(h *harness)
}

// pacingBusy is another claim's launch taking the pacing's next turn, so the next launch the
// harness's claim raises parks behind it.
func pacingBusy(h *harness) {
	h.t.Helper()
	second := h.machineFor(wakeClaim(secondToken, StateQueued))
	h.mustPostTo(second, RequestSpawn{Claim: secondToken})
}

var delayedReplacements = []delayedReplacement{
	{"a delayed repoint", func(h *harness) {
		h.t.Helper()
		pacingBusy(h)
		h.observe(runtime.StaleAddress)
	}},
	{"a delayed stall backstop", func(h *harness) {
		h.t.Helper()
		started := h.clock.Now()
		h.advance(time.Second)
		pacingBusy(h)
		h.must(StallBackstop{Claim: h.token, Bound: time.Millisecond, Deadline: started.Add(time.Millisecond), Step: h.store.step(h.token)})
	}},
	{"a delayed unanswered-hello relaunch", func(h *harness) {
		h.t.Helper()
		h.must(StreamClosed{Claim: h.token})
		h.helloOf(true, true)
		h.advance(h.m.reconnectBound() - time.Nanosecond)
		pacingBusy(h)
		h.advance(time.Nanosecond)
	}},
}

// Row 39b: a replacement the wake pacing parked during the take's turn — a repoint, a stall
// backstop, the relaunch of a hello no answer followed — whose wake's turn comes after that turn
// ended with its report still to come, waits at the wake's turn for the report, as the held repoint
// does: the report finishes the task once, the replacement runs then, and the relaunched process
// is given nothing of the task.
//
// The negative control is the merge b031e2e2: the wake's turn takes the task back as a cut turn
// and relaunches at once (launchWake), so the claim is launching at that turn, which this test
// reads before the report.
func TestTakeWhoseTurnEndedBeforeADelayedReplacementsTurnHoldsThatReplacementForItsReport(t *testing.T) {
	for _, delayed := range delayedReplacements {
		t.Run(delayed.name, func(t *testing.T) {
			h := pacedHarness(t, wakeRate, currentPlugin)
			h.manualTakes()
			h.reach(StateReady)
			number := h.takeOf("implement the plan")
			h.must(StreamTurnStart{Claim: h.token})
			kept := h.locator()
			delayed.raise(h)
			waiting := h.wantWaitingWake(WakeLaunch)
			h.wantStarts(h.token, 1)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.wantState(StateIdle)

			h.advance(waiting.At.Sub(h.clock.Now()))

			h.wantReplacementHeld(number, kept, 1, "at the wake's turn")

			h.mustReport(nil)

			h.wantFinishedOnce(finishedByReport)
			if relaunch := h.m.View().WakeWaiting; relaunch != nil {
				h.advance(relaunch.At.Sub(h.clock.Now()))
			}
			h.wantState(StateLaunching)
			h.wantStarts(h.token, 2)
			if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != kept {
				t.Fatalf("the relaunch replaced %+v, want the process the replacement waited on, %+v", resume.Previous, kept)
			}
			h.relaunched()
			h.wantRunOnce(number)
		})
	}
}

// Row 39c: a suspension held over the take's turn waits past its end for the report (heldWaits),
// and a repoint then comes: the repoint waits with the suspension rather than take the task back
// first and suspend at once. A report inside the suspension's deadline finishes the task once and
// the suspension then stops the process, so the resumed process is given nothing of the task; with
// no report by that deadline the stop runs then, the task taken back with the note of a cut turn,
// and the resumed process takes it once, behind that note.
//
// The negative control is the merge b031e2e2: the repoint over the held suspension takes the task
// back and suspends the claim at the stale observation (replaceNow, endHeld), which this test reads
// before the report.
func TestTakeUnderAHeldSuspensionWaitingForItsReportHoldsAStaleAddressRelaunchWithIt(t *testing.T) {
	t.Run("then the report", func(t *testing.T) {
		h, number, stops := heldOverATakesTurn(t)

		h.observe(runtime.StaleAddress)

		h.wantStillAwaitingItsReport(number, stops)
		h.mustReport(nil)
		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, resumedByTheOperator)
	})
	t.Run("no report by the suspension's deadline", func(t *testing.T) {
		h, number, stops := heldOverATakesTurn(t)

		h.observe(runtime.StaleAddress)

		h.wantStillAwaitingItsReport(number, stops)
		h.advance(testStop)
		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, resumedByTheOperator)
	})
}

// Row 39d: a take a daemon start restored from the store, its turn over and its report not come,
// which the report wait's bound leaves out with nothing held (reportReaches: the store shows the
// turn's end handled after the accept); the stale address another service's, so the view is whole
// once the agent answers the hello's ask. A repoint then: the held repoint counts as held work
// (workHeld), so the report wait's bound arms whole from it. The report finishes the task once and
// the repoint runs then; or at the bound the task is taken back with the note of a cut turn, the
// repoint runs, and the relaunched worker takes the task once, behind that note.
//
// The negative control is the merge b031e2e2: the repoint replaces the process at the stale
// observation, so the claim is launching there and no bound is armed, which this test reads at the
// repoint.
func TestTakeRestoredFromTheStoreIsBoundedByTheReportWaitUnderAHeldStaleAddressRelaunch(t *testing.T) {
	restoredUnderAStaleAddress := func(t *testing.T) (*harness, uint64, runtime.Locator) {
		t.Helper()
		h, number := overATakesEndedTurn(t, queuedClaim())
		h.restart()
		h.connect()
		h.answerAsk(false)
		h.wantUnarmed("over the restored take with nothing held", TimerReport)
		stale := h.locator()

		h.observe(runtime.StaleAddress)

		h.wantReplacementHeld(number, stale, 1, "at the stale observation")
		h.wantArmedFromNow(TimerReport, turnStartBound, "at the held repoint")
		return h, number, stale
	}
	t.Run("then the report", func(t *testing.T) {
		h, number, stale := restoredUnderAStaleAddress(t)
		h.advance(turnStartBound - time.Second)
		h.wantReplacementHeld(number, stale, 1, "a second bound less a second after the repoint")

		h.mustReport(nil)

		h.wantRelaunchedFrom(stale)
		h.relaunched()
		h.wantRunOnce(number)
	})
	t.Run("no report by the bound", func(t *testing.T) {
		h, number, stale := restoredUnderAStaleAddress(t)
		h.advance(turnStartBound - time.Second)
		h.wantReplacementHeld(number, stale, 1, "a second bound less a second after the repoint")

		h.advance(time.Second)

		h.wantRelaunchedFrom(stale)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, relaunchedAndTaken)
	})
}

// observeMovedDaemonAPI is the runtime's sweep finding the claim's process alive at a stale
// address that is the daemon's own API (runtime.Observation.DaemonAPIMoved).
func (h *harness) observeMovedDaemonAPI() {
	h.t.Helper()
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.StaleAddress,
		At: h.clock.Now(), DaemonAPIMoved: true}})
}

// Where the observation says the daemon's own API moved, no report of the process can reach the
// daemon, so a repoint after the take's turn ended is not held for one: the process is relaunched
// at once, charged nothing, the task taken back with the note of a cut turn, as before the held
// repoint; and a repoint of that kind the pacing parked during the take's turn relaunches at its
// wake's turn though that turn ended in the wait with its report still to come.
//
// The negative control is the merge b031e2e2, where runtime.Observation has no DaemonAPIMoved and
// this test does not build; behind it, the held repoint with the observation's word not read
// (repoint's held, and the replacement it parks, set for every stale address) holds the relaunch
// for a report that cannot come, which this test reads at the observation and at the wake's turn.
func TestTakeWhoseTurnEndedIsRelaunchedAtOnceWhereTheDaemonAPIsAddressMoved(t *testing.T) {
	t.Run("a repoint after the take's turn ended", func(t *testing.T) {
		h, number := overATakesEndedTurn(t, queuedClaim())
		stale := h.locator()

		h.observeMovedDaemonAPI()

		h.wantRelaunchedFrom(stale)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, relaunchedAndTaken)
	})
	t.Run("a repoint delayed during the take's turn", func(t *testing.T) {
		h := pacedHarness(t, wakeRate, currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		number := h.takeOf("implement the plan")
		h.must(StreamTurnStart{Claim: h.token})
		stale := h.locator()
		pacingBusy(h)
		h.observeMovedDaemonAPI()
		waiting := h.wantWaitingWake(WakeLaunch)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})

		h.advance(waiting.At.Sub(h.clock.Now()))

		h.wantRelaunchedFrom(stale)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, relaunchedAndTaken)
	})
}
