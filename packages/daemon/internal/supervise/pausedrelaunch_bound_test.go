package supervise

import (
	"errors"
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

// stopOverACompaction is row 39j's case over a compaction (pausedStopOverACompaction): the
// harness, its pause flag and stop transaction, the task whose take's turn ran, the moment of the
// gate's own stop, and the compaction's release and its answer.
type stopOverACompaction struct {
	h         *harness
	flag      *pauseFlag
	stops     *stopTransaction
	number    uint64
	stopped   time.Time
	release   func()
	compacted chan error
}

// pausedStopOverACompaction is a claim whose take's turn ended with its report still to come, its
// session then being compacted with no turn under the compaction (compactHeld). A stall backstop
// over that compaction parks its relaunch behind another claim's launch (pacingBusy), and the
// relaunch's turn comes under pause 7: the pause's gate holds it, and the gate's own stop is made
// (pauseStopDue). A second past that stop the take stands for its report, the claim is still
// working for the compaction, the stop is held to R5's second bound from the stop, and the stop
// transaction was asked nothing.
//
// The stall backstop is the parked replacement raised here because a repoint after the take's
// turn ended is held for the report rather than parked (repoint, boundedReportHolds), and a
// compaction is refused once a launch waits (compactStart, launchWaits).
func pausedStopOverACompaction(t *testing.T) stopOverACompaction {
	t.Helper()
	h, flag, stops := pauseHarness(t, queuedClaim(), pacedDeps(wakeRate), currentPlugin, withWaits)
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf("implement the plan")
	h.must(StreamTurnStart{Claim: h.token})
	started := h.clock.Now()
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	release, compacted := h.compactHeld()
	h.advance(time.Second)
	pacingBusy(h)
	h.must(StallBackstop{Claim: h.token, Bound: time.Millisecond, Deadline: started.Add(time.Millisecond), Step: h.store.step(h.token)})
	waiting := h.wantWaitingWake(WakeLaunch)
	flag.set(7)

	h.advance(waiting.At.Sub(h.clock.Now()))
	stopped := h.clock.Now()
	h.advance(time.Second)

	if p := h.pending(); p.Number != number || !p.taken() || p.Interrupted {
		t.Fatalf("under the pause and the compaction, a second past the parked relaunch's turn, before the report of task %d's turn came, the claim is %s holding %+v: want the take standing for its report",
			number, h.state(), p)
	}
	h.wantState(StateWorking)
	if deadline, held := h.heldDeadline(); !held || !deadline.Equal(stopped.Add(turnStartBound)) {
		t.Fatalf("the pause's stop over the compaction is held %t with deadline %s, want it held to R5's second bound from the stop, %s",
			held, deadline, stopped.Add(turnStartBound))
	}
	if made := stops.stopsOfClaim(h.token); len(made) != 0 {
		t.Fatalf("the stop transaction was asked %+v of the claim before the report came, want nothing", made)
	}
	return stopOverACompaction{h: h, flag: flag, stops: stops, number: number, stopped: stopped, release: release, compacted: compacted}
}

// wantCompactionAnswered is the compaction let through, its answer want.
func (c stopOverACompaction) wantCompactionAnswered(want error) {
	c.h.t.Helper()
	c.release()
	if err := <-c.compacted; !errors.Is(err, want) {
		c.h.t.Fatalf("the compaction answered %v, want %v", err, want)
	}
	c.h.m.Wait()
}

// wantStoppedOwingALaunch is the claim suspended through the pause's stop transaction once, the
// stop landed, with the launch the gate held owed.
func (c stopOverACompaction) wantStoppedOwingALaunch() {
	c.h.t.Helper()
	c.h.wantState(StateSuspended)
	if made, want := c.stops.stopsOfClaim(c.h.token), []claimStop{{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true}}; !slices.Equal(made, want) {
		c.h.t.Fatalf("the stop transaction was asked %+v of the claim, want %+v: the stop met the launch the pause's gate holds", made, want)
	}
}

// resumed is the pause's resume, then the operator's resume of the stopped claim, brought up to
// ready (resumedByTheOperator), its worker taking on its own again, as pi-shared's task-take does.
func (c stopOverACompaction) resumed(h *harness) {
	h.t.Helper()
	c.flag.set(0)
	h.must(ResumePause{Claim: h.token, Pause: 7})
	h.worker.manual = false
	resumedByTheOperator(h)
}

// Row 39j over a compaction: a project pause's gate holds a launch the wake pacing parked, over a
// claim whose take's turn ended with its report still to come and whose session is then being
// compacted with no turn under the compaction (pausedStopOverACompaction). Such a compaction is
// no turn (inTurn), so the gate's own stop is held for the report, bounded by R5's second wait
// (Timeouts.TurnStart) from the stop, as over an idle claim. The report finishes the task once,
// whether it comes during the compaction or after its end, and the suspension runs once both the
// compaction and the report are over: at the compaction's end where the report came first
// (compactEnded, runHeldAtEnd), and at the report's finish where the compaction ended first
// (settleThenAfterTurn). The resumed process takes nothing of the task. With no report by the
// bound the task is taken back with the note of a cut turn, the suspension then runs, and the
// resumed process takes the task once, behind that note.
//
// The negative control is pauseStopDeadline reading busy (`m.busy() || m.takePhase() !=
// waitReport` before this change): the compaction makes the stop's deadline now, so a second past
// the relaunch's turn the task is already taken back as a cut turn and the claim suspended.
func TestTakeWhoseReportIsOwedDuringACompactionHoldsThePausesStopForIt(t *testing.T) {
	t.Run("the report during the compaction", func(t *testing.T) {
		c := pausedStopOverACompaction(t)
		h := c.h

		h.mustReport(nil)

		h.wantFinishedOnce(finishedByReport)
		h.wantState(StateWorking)
		if made := c.stops.stopsOfClaim(h.token); len(made) != 0 {
			t.Fatalf("the stop transaction was asked %+v of the claim while the compaction runs, want nothing", made)
		}
		c.wantCompactionAnswered(ErrCompactSuspended)
		c.wantStoppedOwingALaunch()
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(c.number, c.resumed)
	})
	t.Run("the report after the compaction's end", func(t *testing.T) {
		c := pausedStopOverACompaction(t)
		h := c.h

		c.wantCompactionAnswered(ErrCompactSuspended)

		h.wantState(StateIdle)
		if p := h.pending(); p.Number != c.number || !p.taken() || p.Interrupted {
			t.Fatalf("at the compaction's end, before the report of task %d's turn came, the claim holds %+v: want the take standing for its report", c.number, p)
		}
		if made := c.stops.stopsOfClaim(h.token); len(made) != 0 {
			t.Fatalf("the stop transaction was asked %+v of the claim at the compaction's end, want nothing", made)
		}
		h.mustReport(nil)
		h.wantFinishedOnce(finishedByReport)
		c.wantStoppedOwingALaunch()
		h.wantTakenNoMore(c.number, c.resumed)
	})
	t.Run("no report by the bound", func(t *testing.T) {
		c := pausedStopOverACompaction(t)
		h := c.h

		h.advance(c.stopped.Add(turnStartBound).Sub(h.clock.Now()))

		h.wantTakenBackWithItsNote(c.number)
		c.wantStoppedOwingALaunch()
		c.wantCompactionAnswered(ErrCompactProcessGone)
		h.wantTakenOnceMore(c.number, c.resumed)
	})
}
