package supervise

// The bounds and the connection (connectionbounds_test.go): the rows of R5's second wait
// (TimerTurnStart), over a take awaiting its turn.

import (
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Row 1: a close disarms the second wait over a take awaiting its turn, and arms the reconnect
// deadline, whole from the close.
func TestTakeAwaitingItsTurnLosesItsSecondWaitAtAClose(t *testing.T) {
	h := boundsHarness(t)
	h.takeOf("implement the plan")
	if !h.armed(TimerTurnStart) {
		t.Fatal("the accepted take's second wait is not armed")
	}

	h.reconnectLater()

	h.wantUnarmed("after the close", TimerTurnStart)
	h.wantArmedFromNow(TimerRegistration, deadline, "after the close")
}

// Row 2: no bound of the second wait passes while no connection stands. In the second case the
// worker's report of a run of the host's own given the take, G, reaches the daemon over HTTP while
// the shim is cut off, and puts the take back; the take's own turn, T, runs meanwhile, unseen. Its
// start is replayed behind the hello, and T's report finishes the task once. The negative control
// is the commit under this change, where the put-back arms the second wait and its bound charges
// the worker and takes the task back while T runs.
func TestChargeNothingForTheSecondWaitWhileNoConnectionStands(t *testing.T) {
	t.Run("a take awaiting its turn", func(t *testing.T) {
		h := boundsHarness(t, longReconnect)
		number := h.takeOf("implement the plan")
		h.reconnectLater()

		h.advance(turnStartBound)

		h.wantCharges()
		h.wantAwaitingItsTurn(number, "a whole second bound after the close")
	})
	t.Run("a take a report put back while no connection stands", func(t *testing.T) {
		h := boundsHarness(t, longReconnect)
		number := h.takeOf("implement the plan")
		h.hostsRun()
		h.reconnectLater()
		h.reportOf(1, 0)
		h.wantAwaitingItsTurn(number, "after G's report over HTTP")

		h.advance(turnStartBound)

		h.wantCharges()
		h.wantAwaitingItsTurn(number, "a whole second bound after the put-back")
		h.helloOf(true, true)
		h.must(StreamTurnStart{Claim: h.token})
		h.answerAsk(true)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(2, number)
		h.wantRunOnce(number)
		h.wantCharges()
	})
}

// Row 5: the take's turn, T, started while no connection stood, and its start is replayed behind
// a hello that says the backlog is whole. No bound is armed at the hello; the replayed start is
// the take's turn, no bound runs over it, and T's report finishes the task once.
func TestTakeWhoseTurnStartedUnseenIsGivenItByItsReplayedStart(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.reconnectLater()

	h.helloOf(true, true)

	h.wantUnarmed("at the hello", TimerTurnStart, TimerReport, TimerTake)
	h.must(StreamTurnStart{Claim: h.token})
	h.wantTakesTurnStanding(number, "after T's replayed start")
	h.answerAsk(true)
	h.advance(2 * turnStartBound)
	h.wantCharges()
	h.wantTakesTurnStanding(number, "two bounds into T")
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantRunOnce(number)
}

// Row 5a: T's start frame sat in the socket's send buffer when the connection broke, so the shim,
// which keeps only a frame whose write failed, replays none, and its hello says its backlog is
// whole. The agent's answer that it is in a turn is the take's turn starting: no bound runs during
// T, and T's report finishes the task once. The negative control is the commit under this change:
// the second wait stays armed through the hello, and its bound charges the worker and takes the
// task back during T.
func TestTakeWhoseStartFrameWasLostIsGivenItsTurnByTheAnswer(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.reconnectLater()
	h.helloOf(true, true)
	h.wantUnarmed("at the hello", TimerTurnStart, TimerReport, TimerTake)

	h.answerAsk(true)

	h.wantState(StateWorking)
	h.wantTakesTurnStanding(number, "at the answer")
	h.advance(2 * turnStartBound)
	h.wantCharges()
	h.wantTakesTurnStanding(number, "two bounds into T")
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantRunOnce(number)
}

// Row 6: a take awaiting its turn when the connection closed, and an agent that answers the
// reconnect hello's ask that it is in a turn, whether or not the hello said its backlog is whole:
// the take's turn has started, no bound runs during it, and its report finishes the task once.
func TestTakeAwaitingItsTurnIsGivenItByAnAnswerInATurn(t *testing.T) {
	for _, whole := range []bool{true, false} {
		t.Run("backlog whole "+strconv.FormatBool(whole), func(t *testing.T) {
			h := boundsHarness(t)
			number := h.takeOf("implement the plan")
			h.reconnectLater()
			h.helloOf(whole, true)
			h.wantUnarmed("before the answer", TimerTurnStart)

			h.answerAsk(true)

			h.wantState(StateWorking)
			h.wantTakesTurnStanding(number, "at the answer")
			h.advance(2 * turnStartBound)
			h.wantCharges()
			h.must(StreamTurnEnd{Claim: h.token})
			h.wantRunOnce(number)
		})
	}
}

// Row 7: an agent that answers it is in no turn: R5's second wait is armed whole from the answer.
// A report of the take's turn, run whole while no connection stood, that lands inside that bound
// finishes the task; with no such report the second wait charges the worker at the bound counted
// from the answer, and not before. The negative control is the commit under this change, whose
// second wait, armed at the take, charges a bound after the take, during the outage's time.
func TestChargeTheSecondWaitFromTheAnswerOfNoTurnAfterAReconnect(t *testing.T) {
	answered := func(t *testing.T) (*harness, uint64) {
		t.Helper()
		h := boundsHarness(t)
		number := h.takeOf("implement the plan")
		h.reconnectLater()
		h.advance(time.Minute)
		h.helloOf(true, false)
		h.advance(5 * time.Minute)
		h.answerAsk(false)
		h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the answer")
		h.advance(turnStartBound - time.Second)
		h.wantCharges()
		return h, number
	}
	t.Run("a report of the turn run unseen inside the bound", func(t *testing.T) {
		h, number := answered(t)

		h.reportOf(1, number)

		h.wantRunOnce(number)
		h.advance(2 * turnStartBound)
		h.wantCharges()
	})
	t.Run("no report", func(t *testing.T) {
		h, number := answered(t)

		h.advance(time.Second)

		h.wantCharges(WaitTurnStart)
		if p := h.pending(); p.Number != number || p.taken() {
			t.Fatalf("the claim holds %+v, want task %d taken back at the second bound", p, number)
		}
	})
}

// Row 9: no answer, and a turn end frame on the new connection: the second wait is armed whole
// from that end, and the reconnect deadline the hello armed goes.
func TestTakeBoundsArmFromATurnEndBeforeTheAnswer(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.reconnectLater()
	h.helloOf(true, false)
	h.advance(time.Minute)

	h.mustQuiet(StreamTurnEnd{Claim: h.token})

	h.wantArmedFromNow(TimerTurnStart, turnStartBound, "at the turn's end")
	h.wantAwaitingItsTurn(number, "at the turn's end")
	h.advance(deadline)
	h.wantCalls("Resume", 0)
}

// Row 9a: no answer, and a turn start frame on the new connection: it is the take's turn, no bound
// runs during it, the reconnect deadline the hello armed goes, and the turn's report finishes the
// task once. It passes on the commit under this change too, where a start always gave the take
// its turn and no deadline was armed at the hello.
func TestTakeAwaitingItsTurnIsGivenTheTurnStartBeforeTheAnswer(t *testing.T) {
	h := boundsHarness(t)
	number := h.takeOf("implement the plan")
	h.reconnectLater()
	h.helloOf(true, true)

	h.must(StreamTurnStart{Claim: h.token})

	h.wantTakesTurnStanding(number, "at the start")
	h.advance(2 * turnStartBound)
	h.wantCharges()
	h.wantCalls("Resume", 0)
	h.must(StreamTurnEnd{Claim: h.token})
	h.wantRunOnce(number)
}

// Row 17: a take awaiting its turn while its finish is owed is under neither R5's second wait
// nor the bound of the wait for its report: whole bounds of both charge nothing and take nothing
// back, and the end-frame bound makes the finish again, by the report, once the store answers.
// The negative control is the commit under this change, whose second wait read the take awaiting
// its turn and charged the worker and took the task back under its number.
func TestTakeAwaitingItsTurnWithAnOwedFinishIsUnderNoBound(t *testing.T) {
	h, number := owedFinishAwaitingItsTurn(t, true)
	h.observe(runtime.Alive)

	h.advance(2 * turnStartBound)

	h.wantCharges()
	h.wantAwaitingItsTurn(number, "two bounds into the owed finish")
	if lines := h.logs.lines(reportMissedLine); len(lines) != 0 {
		t.Fatalf("logged %q, want nothing taken back", lines)
	}
	h.store.fail("RetireDelivery", nil)
	h.advance(testRPC)
	h.wantOwedFinishMade(number)
}
