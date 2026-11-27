package supervise

// The bounds and the connection (connectionbounds_test.go): the rows of the wait for a take's
// report (TimerReport), with nothing held as with work held.

import (
	"testing"
	"time"
)

// Row 3: a close disarms the bound of the wait for the report of a take whose turn is over, with
// nothing held, and arms the reconnect deadline.
func TestTakeWhoseReportIsOwedLosesItsReportBoundAtAClose(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantTakesTurnStanding(number, "at the take's turn's end")
	if !h.armed(TimerReport) {
		t.Fatal("the bound of the wait for the take's report is not armed at its turn's end")
	}

	h.reconnectLater()

	h.wantUnarmed("after the close", TimerReport)
	h.wantArmedFromNow(TimerRegistration, deadline, "after the close")
}

// Row 4: no bound of the wait for a take's report passes while the daemon cannot see the worker's
// frames. Two cases of the bound with nothing held: the daemon gave the take a run of the host's
// own, G, saw it start and end, and the take's own turn, T, runs while the shim is cut off. In the
// first the shim comes back with its backlog whole after a whole bound; in the second it comes
// back at once, its hello saying the backlog dropped frames (T's start among them), and the agent
// answers only after a whole bound. T's report finishes the task once in both. The negative control
// is the commit under this change, where the bound armed at G's end takes the task back as a cut
// turn while T runs, writes its value again, and T's report finishes nothing.
func TestTakeWhoseReportIsOwedIsNotTakenBackWhileNoConnectionStands(t *testing.T) {
	t.Run("the shim back with a whole backlog after a whole bound", func(t *testing.T) {
		h := boundsHarness(t, longReconnect)
		number := h.takeOf("implement the plan")
		h.hostsRun()
		h.reconnectLater()

		h.advance(turnStartBound)

		h.wantTakesTurnStanding(number, "a whole report bound after G's end, no connection standing,")
		h.helloOf(true, true)
		h.must(StreamTurnStart{Claim: h.token})
		h.answerAsk(true)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(2, number)
		h.wantRunOnce(number)
		h.wantCharges()
	})
	t.Run("a backlog cut, the agent answering after a whole bound", func(t *testing.T) {
		h := boundsHarness(t, longReconnect)
		number := h.takeOf("implement the plan")
		h.hostsRun()
		h.reconnectLater()
		h.helloOf(false, true)

		h.advance(turnStartBound)

		h.wantTakesTurnStanding(number, "a whole report bound after G's end, the hello unanswered,")
		h.answerAsk(true)
		h.wantState(StateWorking)
		h.advance(2 * turnStartBound)
		h.wantTakesTurnStanding(number, "two bounds into T")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(2, number)
		h.wantRunOnce(number)
		h.wantCharges()
	})
}

// Row 5b: the take's turn ended while no connection stood, its end frame lost. The agent's answer
// that it is in no turn ends that turn, and the bound of the wait for the take's report is armed
// whole from the answer; the report that lands inside it finishes the task.
func TestTakeBoundsArmFromTheAnswerThatEndsATurnWhoseEndFrameWasLost(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.must(StreamTurnStart{Claim: h.token})
	h.reconnectLater()
	h.advance(time.Minute)
	h.helloOf(true, false)
	h.wantUnarmed("at the hello", TimerReport)
	h.advance(time.Minute)

	h.answerAsk(false)

	h.wantState(StateIdle)
	h.wantArmedFromNow(TimerReport, turnStartBound, "at the answer")
	h.advance(turnStartBound - time.Second)
	h.wantTakesTurnStanding(number, "a bound less a second after the answer")
	h.reportOf(1, number)
	h.wantRunOnce(number)
}

// Row 15: a restored take whose first hello says the backlog is whole and whose agent answers on
// it binds its anchor, and the bound of the wait for its report with nothing held reaches it, as
// before this change: the store records the run the daemon before gave the take running at the
// restart, and that run's end, on the new connection, arms the bound whole. It passes on the
// commit under this change too; it pins that the ask a reconnect hello makes leaves such a take in
// the bound.
func TestTakeRestoredWithAWholeBacklogAndAnAnswerIsInTheNothingHeldBound(t *testing.T) {
	h, number := takeAcrossARestart(t, givenRunRunning, withWaits)
	h.connectInTurn()
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.reportOf(2, 0)

	h.wantTakeBackAtTheBound(number)
	h.wantRetakenAndRunOnce(number)
}

// Row 16: a restored take whose first hello does not say its backlog is whole is left out of the
// bound with nothing held (the stated case), and R5's second wait over a restored take awaiting
// its turn is armed as rows 6 to 9a say: not at the hello, and whole from the agent's answer of no
// turn. The first case passes on the commit under this change too, which left such a take out
// already; the second fails there, where the hello armed the second wait.
func TestTakeRestoredBehindACutBacklogIsLeftOutOfTheNothingHeldBound(t *testing.T) {
	t.Run("its given run over, nothing held", func(t *testing.T) {
		h, number := takeAcrossARestart(t, givenRunRunning, withWaits)
		h.conn.SetStreaming(true)
		h.cutHello()
		h.answerAsk(true)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(2, 0)
		putBacks := len(h.records.of(notTheTakesTurnLine))

		h.advance(2 * turnStartBound)

		h.wantCharges()
		h.wantStillItsTurn(number, putBacks, "two bounds after the given run's end")
		h.ownTurnFinishes(number, 3)
	})
	t.Run("awaiting its turn", func(t *testing.T) {
		h, number := takeAcrossARestart(t, nothingMore, withWaits)
		h.cutHello()
		h.wantUnarmed("at the hello", TimerTurnStart)

		h.answerAsk(false)

		h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
		h.wantAwaitingItsTurn(number, "at the answer")
	})
}
