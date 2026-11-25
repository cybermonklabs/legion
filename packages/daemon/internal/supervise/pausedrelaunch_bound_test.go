package supervise

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// heldDeadline is the deadline of the claim's held suspension, and whether one is held.
func (h *harness) heldDeadline() (time.Time, bool) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if h.m.held == nil {
		return time.Time{}, false
	}
	return h.m.held.Deadline, true
}

// claimStop is one stop the stop transaction was asked of one claim, and whether it landed.
type claimStop struct {
	stop   PauseStop
	landed bool
}

// stopsOfClaim is the stops the stop transaction was asked of token's claim, in the order asked:
// the pause stops every claim of the harness whose launch its gate holds, the other claim whose
// launch took the pacing's turn (pacingBusy) among them.
func (s *stopTransaction) stopsOfClaim(token claim.Token) []claimStop {
	var of []claimStop
	for _, call := range s.made() {
		if call.claim.Token == token {
			of = append(of, claimStop{stop: call.stop, landed: call.landed})
		}
	}
	return of
}

// Row 39j's bound (rule two of T3 of dispatch://INFRA-2811's plan): a relaunch the wake pacing
// parked during a take's turn, whose wake's turn comes under a project pause after that turn ended
// with its report still to come. The pause's stop of the claim (pauseStopDue) is held for the
// report, bounded by R5's second wait (Timeouts.TurnStart) from the stop rather than by a deadline
// of now. With no report by that bound the task is taken back with the note of a cut turn, and the
// suspension then runs, through the pause's stop transaction with the launch the held task owes.
// The bound that comes first is the report wait's own (TimerReport, armed at the turn's end) where
// the claim's view is whole, and the held suspension's deadline where it is not, as after the
// hello no answer followed.
//
// The negative control is the head this test was written at, where the pause's stop names a
// deadline of now: a second past the wake's turn the take is already taken back and the claim
// suspended.
func TestTakeWhoseReportMissesItsBoundUnderAPausedParkedRelaunchIsTakenBackBeforeTheSuspension(t *testing.T) {
	for _, parked := range parkedReplacements {
		t.Run(parked.name+"'s turn under the pause", func(t *testing.T) {
			h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate), currentPlugin, withWaits)
			h.manualTakes()
			h.reach(StateReady)
			number := h.takeOf("implement the plan")
			h.must(StreamTurnStart{Claim: h.token})
			parked.raise(h)
			waiting := h.wantWaitingWake(WakeLaunch)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.wantState(StateIdle)
			flag.set(7)

			h.advance(waiting.At.Sub(h.clock.Now()))
			stopped := h.clock.Now()
			h.advance(time.Second)

			if p := h.pending(); p.Number != number || !p.taken() || p.Interrupted {
				t.Fatalf("under the pause, a second past the parked relaunch's turn, before the report of task %d's turn came, the claim is %s holding %+v: want the take standing for its report",
					number, h.state(), p)
			}
			h.wantState(StateIdle)
			if deadline, held := h.heldDeadline(); !held || !deadline.Equal(stopped.Add(turnStartBound)) {
				t.Fatalf("the pause's stop is held %t with deadline %s, want it held to R5's second bound from the stop, %s",
					held, deadline, stopped.Add(turnStartBound))
			}
			if made := stops.stopsOfClaim(h.token); len(made) != 0 {
				t.Fatalf("the stop transaction was asked %+v of the claim before the report's bound, want nothing", made)
			}

			h.advance(stopped.Add(turnStartBound).Sub(h.clock.Now()))

			h.wantTakenBackWithItsNote(number)
			h.wantState(StateSuspended)
			if made, want := stops.stopsOfClaim(h.token), []claimStop{{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true}}; !slices.Equal(made, want) {
				t.Fatalf("the stop transaction was asked %+v of the claim at the report's bound, want %+v: the stop of a role whose task was taken back owes it a launch", made, want)
			}
		})
	}
}
