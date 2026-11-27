package supervise

// Work held behind the take of a claim whose plugin names its task's number (reportwait.go), under
// two bounds that come while it waits and are no bound of that wait: the reconnect deadline a
// hello arms again for the agent's word on whether it is in a turn (reconnectDeadline), and the
// project's pause, which holds the take while the work waits behind it (secondWaitNow).

import (
	"errors"
	"testing"
	"time"
)

// restoreStaleTake is a daemon start over a claim restored ready on a plugin that names its task's
// number (testTakeContract), which this daemon's later contract makes stale, holding a take the
// daemon before accepted and whose turn has not started: the boot's pass has posted the claim its
// relaunch, which waits for the agent's word on whether it is in a turn (askTurn). set changes the
// daemon's deps before the start.
func restoreStaleTake(t *testing.T, set ...func(*Deps)) *harness {
	t.Helper()
	h := newBareHarness(t)
	h.deps.PluginContract, h.deps.PluginRelaunchable = testTakeContract+1, true
	for _, change := range set {
		change(&h.deps)
	}
	h.manualTakes()
	c := fixture(StateReady)
	c.PluginContract = testTakeContract
	c.Pending.ConfirmedAt, c.Pending.TakenText, c.TaskTaken = epoch, c.Pending.Task, 1
	h.restore(c)
	h.must(StalePlugin{Claim: h.token, Trigger: "boot"})
	h.wantCalls("Resume", 0)
	return h
}

// The reconnect deadline that a hello arms again for an unanswered ask of a restored stale claim
// (askRestoredTurn) bounds the ask; it does not end a suspension held over the take's turn. The
// shim came back, so the turn's end and the take's report reach the machine: the hold waits for
// them, as reready leaves it, and lands at the report's finish, the task finished once. The
// relaunch the deadline makes waits for the turn as well (holdStaleRelaunch). A shim that never
// says hello is the deadline's own case: no turn's end will come from it, so the held suspension
// stops the process there, and the task goes back as a cut turn.
//
// The negative control is the head before this change (1d8f65eb), where the deadline runs the
// held suspension with no look at the take: it stops the process in the take's own turn and takes
// the task back as a cut turn, though that turn runs whole and its report comes, so the task runs
// again on resume.
func TestAReconnectDeadlineLeavesAHeldSuspensionToTheTakesTurnAndReport(t *testing.T) {
	t.Run("a hello, the ask unanswered, and the take's turn running at the deadline", func(t *testing.T) {
		h := restoreStaleTake(t)
		number := h.pending().Number
		h.conn.FailGetState(errBoom)
		h.connect()
		h.must(StreamTurnStart{Claim: h.token})
		h.wantTakesTurnStanding(number, "once the replayed start came")
		if err := h.handle(daemonSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's suspension during the take's turn answered %v, want it held", err)
		}

		h.advance(deadline)

		h.wantState(StateWorking)
		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		h.wantTakesTurnStanding(number, "at the reconnect deadline")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
		h.mustReport(nil)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", 1)
		h.wantCalls("Resume", 0)
		h.wantFinishedOnce(finishedByReport)
	})
	t.Run("no hello", func(t *testing.T) {
		h := newBareHarness(t)
		h.deps.PluginContract = testTakeContract
		restored := fixture(StateWorking)
		restored.PluginContract = testTakeContract
		h.restore(restored)
		if err := h.handle(daemonSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's suspension of the restored working claim answered %v, want it held", err)
		}

		h.advance(deadline)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", 1)
		if p := h.pending(); p.taken() || !p.Interrupted {
			t.Fatalf("the claim holds %+v, want its task taken back with the note of a cut turn", p)
		}
	})
}

// A claim a daemon start restored ready on a stale plugin, holding a take, whose relaunch waits for
// its agent's word on whether it is in a turn (askTurn): the first hello's ask goes unanswered, the
// replay shows the take's turn start, and the connection breaks with that turn's end frame lost, so
// the claim is working on a turn whose end will not come. Every reconnect hello of a claim holding
// a pending task asks once, bounded by the reconnect deadline (connview.go), so the relaunch is
// decided by the next hello's answer or by that deadline, and never left to the stall backstop:
//
//   - the next hello asks again (askView), and an answer of no turn ends the turn the claim
//     is read in (answerView): the relaunch waits for the take's report, which finishes the
//     task once before the relaunch runs;
//   - with no answer and no turn frame on that connection by the reconnect deadline, the process is
//     relaunched, charged nothing, and the task taken back as a cut turn, as at the deadline of
//     every hello no answer followed (reconnectDeadline);
//   - with no hello again by that deadline, the process is retired and relaunched as one whose shim
//     never reconnected, the task taken back as a cut turn.
//
// The negative control is the commit under this change: a hello over the working claim asked
// nothing for the relaunch and disarmed the reconnect deadline (reconnectedMidTurn), and with no
// hello the deadline held the relaunch for the turn's end (refreshPlugin, holdStaleRelaunch), so
// the claim stayed working until the stall backstop.
func TestTakeOfARestoredStaleClaimInATurnWhoseEndWasLostIsDecidedByTheNextAnswerOrTheDeadline(t *testing.T) {
	inATurnWithItsEndLost := func(t *testing.T) (*harness, uint64) {
		t.Helper()
		h := restoreStaleTake(t)
		number := h.pending().Number
		h.conn.FailGetState(errBoom)
		h.connect()
		h.must(StreamTurnStart{Claim: h.token})
		h.wantTakesTurnStanding(number, "once the replayed start came")
		h.reconnectLater()
		return h, number
	}
	t.Run("the next hello's ask answered no turn", func(t *testing.T) {
		h, number := inATurnWithItsEndLost(t)
		h.connect()
		if h.conn.LastAsk() == "" {
			t.Fatal("the hello over the working claim asked its agent nothing")
		}

		h.answerAsk(false)

		h.wantState(StateIdle)
		h.wantCalls("Resume", 0)
		h.wantTakesTurnStanding(number, "after the answer")
		h.mustReport(nil)
		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, relaunchedToReady)
	})
	t.Run("the next hello's ask unanswered by the reconnect deadline", func(t *testing.T) {
		h, number := inATurnWithItsEndLost(t)
		h.conn.FailGetState(errBoom)
		h.connect()

		h.advance(deadline)

		h.wantCalls("Resume", 1)
		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{})
		h.wantCharges()
		if lines := h.logs.lines(viewUnansweredRelaunchLine); len(lines) != 1 {
			t.Fatalf("logged %q, want the relaunch of a hello no answer followed once", lines)
		}
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, relaunchedAndTaken)
	})
	t.Run("no hello again by the reconnect deadline", func(t *testing.T) {
		h, number := inATurnWithItsEndLost(t)

		h.advance(deadline)

		h.wantCalls("Resume", 1)
		if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
			t.Fatalf("the relaunch is pending cause %q, want %q", cause, CauseNeverReconnected)
		}
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, relaunchedAndTaken)
	})
}

// A report that gives a take its turn back while the project is paused, the pause's own stop held
// behind the take (reportwait.go), leaves that stop to the take's own turn or to R5's second bound
// armed from the put-back, as for any work held there (decision 8 of dispatch://INFRA-2811): the
// pause holds the take, not the work that waits on it (secondWaitNow). With no turn started by the
// bound, the worker is charged, the take is taken back with no note, since no turn of it ran, and
// the pause's stop lands, owing the role a launch for the task it keeps.
//
// The negative control is the head before this change (1d8f65eb), whose second wait reads the
// pause's hold first and arms nothing (claimHold): a stop with no deadline waits for a turn the
// worker may never start, and the claim stays idle and held while the pause stands.
func TestChargeOnceAtTheSecondBoundForAPutBackWhilePausedThenThePausesStopLands(t *testing.T) {
	h, flag, stops := pauseHarness(t, queuedClaim(), withWaits, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf("implement the plan")
	h.must(StreamTurnStart{Claim: h.token})
	if p := h.pending(); !p.startRecorded() {
		t.Fatalf("the run that started after the take is %+v, want it given to the take", p)
	}
	flag.set(7)
	if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the pause's stop during the run given the take answered %v, want it held", err)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	h.reportOf(h.worker.sequence+1, 0)
	if p := h.pending(); p.Number != number || !p.awaitsStart() {
		t.Fatalf("after the report of the run given the take the claim holds %+v, want task %d's take awaiting its turn again", p, number)
	}

	h.advance(turnStartBound - time.Second)
	h.wantState(StateIdle)
	h.wantCharges()
	stops.wantStops(t)

	h.advance(time.Second)

	h.wantCharges(WaitTurnStart)
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
	if p := h.pending(); p.Number != number || p.taken() || p.Interrupted {
		t.Fatalf("after the second bound the claim holds %+v, want task %d taken back with no note of a cut turn", p, number)
	}
}
