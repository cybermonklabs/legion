package supervise

import (
	"testing"
	"time"
)

// A relaunch the wake pacing parked during a take's turn (row 39b: a repoint, a stall backstop, the
// relaunch of a hello no answer followed) whose wake's turn comes under a project pause, after that
// turn ended with its report still to come. The take's turn ran whole and its live process's
// report is still coming, so the take must stand for that report, as the pause's own stop row
// holds it (rule two of T3, the first case below): the report finishes the task once, and the
// pause's stop runs then. The relaunched or resumed process must not run the task again.
//
// Test round 13 (the tester's class search for test round 12's c3 at 087ec137): the pause's gate
// holds the parked launch (wakeNow, pauseGate), and its stop (pauseStopDue) suspends the claim with
// a Deadline of now, so the held suspension's timer fires at once (holdSuspension,
// suspendAtTimeout) and the task is taken back with the note of a cut turn while its report is
// still coming; the resume then runs it again. The negative control is the head this test was
// written at, 087ec137, where the second case fails: a second past the wake's turn the claim is
// suspended with the task taken back, Interrupted set. The first case passes there, and shows the
// rule the second breaks.
func TestTakeWhoseTurnEndedWaitsForItsReportUnderAPausedDelayedRelaunch(t *testing.T) {
	t.Run("the pause's own stop row", func(t *testing.T) {
		h, flag, _ := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate), currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		number := h.takeOf("implement the plan")
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		flag.set(7)
		_ = h.handle(pauseStop(h.token, 7))
		h.advance(time.Second)
		if p := h.pending(); p.Number != number || !p.taken() || p.Interrupted {
			t.Fatalf("the pause's stop took task %d back before its report came: %+v (state %s)", number, p, h.state())
		}
		h.mustReport(nil)
		h.wantFinishedOnce(finishedByReport)
		h.wantState(StateSuspended)
	})
	for _, delayed := range delayedReplacements {
		t.Run(delayed.name+"'s turn under the pause", func(t *testing.T) {
			h, flag, _ := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate), currentPlugin)
			h.manualTakes()
			h.reach(StateReady)
			number := h.takeOf("implement the plan")
			h.must(StreamTurnStart{Claim: h.token})
			delayed.raise(h)
			waiting := h.wantWaitingWake(WakeLaunch)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.wantState(StateIdle)
			flag.set(7)

			h.advance(waiting.At.Sub(h.clock.Now()))
			h.advance(time.Second)

			if p := h.pending(); p.Number != number || !p.taken() || p.Interrupted {
				t.Fatalf("under the pause, a second past the delayed relaunch's turn, before the report of task %d's turn came, the claim is %s holding %+v: want the take standing for its report",
					number, h.state(), p)
			}
			h.mustReport(nil)
			h.wantFinishedOnce(finishedByReport)
		})
	}
}
