package supervise

// The bound of the wait for a take's report with nothing held (reportwait.go, reportBoundOver,
// reportReaches): a take whose counted turn ended and whose report naming its number never comes
// is taken back at R5's second bound, and its value written again. These tests pin when that bound
// runs, from when after a daemon's start, and which restored takes it leaves out.

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// wantTakeBackAtTheBound holds that the take of task number, its counted turn ended now and nothing
// held, keeps that turn for R5's second bound less a second, and at the bound is taken back with
// the note of a cut turn and no charge, its value written again once, as a retake
// (reportwait.go, reportBoundOver).
func (h *harness) wantTakeBackAtTheBound(number uint64) {
	h.t.Helper()
	writes := len(h.records.of(writtenLine))
	h.advance(turnStartBound - time.Second)
	h.wantTakesTurnStanding(number, "a second bound less a second after the counted turn's end")
	h.wantCharges()

	h.advance(time.Second)

	h.wantCharges()
	if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
		h.t.Fatalf("a second bound after the counted turn's end the claim holds %+v, want task %d taken back with the note of a cut turn", p, number)
	}
	written := h.records.of(writtenLine)[writes:]
	if len(written) != 1 {
		h.t.Fatalf("logged %d value writes at the bound, want the take-back's one", len(written))
	}
	wantAttr(h.t, written[0], "task", strconv.FormatUint(number, 10))
	wantAttr(h.t, written[0], "cause", TaskValueRetake)
}

// wantRetakenAndRunOnce holds that the worker takes task number again at the value the bound
// wrote, behind the note of a cut turn, and that this take's own turn runs it and its report
// finishes it, once.
func (h *harness) wantRetakenAndRunOnce(number uint64) {
	h.t.Helper()
	finished, sent := len(h.records.of(finishedLine)), len(h.prompts())
	h.mustTake()
	again := h.prompts()[sent:]
	if len(again) != 1 || again[0].Number != number {
		h.t.Fatalf("the worker took %+v at the value written again, want task %d once", again, number)
	}
	wantSays(h.t, again[0].Message, saysTaskTurn)
	h.must(StreamTurnStart{Claim: h.token})
	if p := h.pending(); p.Number != number || !p.startRecorded() {
		h.t.Fatalf("the turn after the retake left %+v, want it task %d's", p, number)
	}
	h.must(StreamTurnEnd{Claim: h.token})
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("pending %+v after the retaken take's own turn was reported, want task %d finished", p, number)
	}
	lines := h.records.of(finishedLine)[finished:]
	if len(lines) != 1 {
		h.t.Fatalf("%q was logged %d times after the retake, want once", finishedLine, len(lines))
	}
	wantAttr(h.t, lines[0], "by", finishedByReport)
}

// A take whose counted turn has ended, and whose report naming its number never comes, is taken
// back at R5's second bound from that turn's end with nothing held (decision 8 of
// dispatch://INFRA-2811; reportwait.go): its text was dropped — Claude Code never took it, or Oh
// My Pi's sendUserMessage refused it — and the producer reports the turn as no task's: Oh My Pi,
// holding no take, names nothing held, and the Claude bridge names the number held. Without the
// bound such a take keeps a turn that ran nothing of it, the worker sees no newer revision, and
// the task never runs. The orders the bridge's worker found:
//
//   - A, the count not known after a restart: a run of the host's own (an Envoy card's) starts
//     right after the dropped turn, and its start frame beats the dropped turn's report that the
//     text was held, which is then moot (heldUnrun); the bound runs from the card's turn's end;
//   - B, the count not known: the dropped turn's report puts the take back, and a run of the host's
//     own that starts next is given it, then reports no task;
//   - C, the count known: a no-task report that beats its turn's start frame passes over a take
//     whose turn has not started (notTheTakesTurn), and that start is then given the take; a run of
//     the host's own half a bound later disarms the bound, and its end arms it whole again.
//
// Each ends with the take taken back at the bound, its value written again once, and the task run
// once by the worker's next take. The negative control is the head before this change (dd71399b),
// whose bound runs only while work is held: the take keeps its turn, and no value is written.
func TestTakeBoundedWithNothingHeldAfterADroppedText(t *testing.T) {
	t.Run("A: a host's start beats the dropped turn's held report", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore, withWaits)
		number := h.takeOf("the take whose text the host dropped")
		h.hostsRun()
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the dropped turn's held report, a card's turn running,")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, 0)
		h.wantStillItsTurn(number, putBacks, "after the card's turn's report,")

		h.wantTakeBackAtTheBound(number)
		h.wantRetakenAndRunOnce(number)
	})
	t.Run("B: a host's start after the put-back is given the take", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore, withWaits)
		number := h.takeOf("the take whose text the host dropped")
		h.hostsRun()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantPutBackBy(number, putBacks, "held")
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.startRecorded() {
			t.Fatalf("the card's start after the put-back left %+v, want it given the take", p)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, 0)

		h.wantTakeBackAtTheBound(number)
		h.wantRetakenAndRunOnce(number)
	})
	t.Run("C: the count known, a no-task report beats its start frame", func(t *testing.T) {
		h := waitsHarness(t, currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		number := h.takeOf("the take whose text the host dropped")
		h.reportOf(1, 0)
		if p := h.pending(); !p.awaitsStart() || len(h.logs.lines(notTheTakesTurnLine)) != 0 {
			t.Fatalf("after the no-task report ahead of its start the claim holds %+v, want the take awaiting its turn, nothing put back", p)
		}
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.startRecorded() {
			t.Fatalf("the reported turn's start left %+v, want it given the take", p)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.advance(turnStartBound / 2)
		h.hostsRun()
		h.reportOf(2, 0)

		h.wantTakeBackAtTheBound(number)
		h.wantRetakenAndRunOnce(number)
	})
}

// After a daemon restart the bound of the wait for a take's report starts no earlier than the
// worker's first act after the boot (Machine.awaitsAct): a take whose counted turn ended while no
// daemon served it, three bounds ago, is not taken back at the boot, nor before the worker's hello,
// and its end, replayed behind that hello, starts the bound whole. The take is one the daemon
// before the restart gave its turn, which was running when that daemon stopped: the store records
// no turn's end after the accept, and the hello says the shim's backlog is whole and its agent
// answers, so this daemon sees every start since (reportReaches).
//
// The negative control is the head before this change (dd71399b), whose bound runs only while
// work is held: the take keeps its turn, and no value is written.
func TestTakeBoundedWithNothingHeldFromTheWorkersFirstActAfterABoot(t *testing.T) {
	h := newHarnessOf(t, queuedClaim(), currentPlugin, withWaits)
	h.manualTakes()
	h.reach(StateReady)
	h.takeRunsItsOwnTurn("the task before the take", 1, false)
	h.clock.Advance(time.Second)
	number := h.takeOf("the take whose turn ends in the outage")
	h.clock.Advance(time.Second)
	givenRunRunning(h)
	h.outage(3 * turnStartBound)

	h.advance(deadline - time.Second)
	h.wantTakesTurnStanding(number, "before the worker's first act after the boot")
	h.connectInTurn()
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.reportOf(2, 0)

	h.wantTakeBackAtTheBound(number)
	h.wantRetakenAndRunOnce(number)
}

// A report naming the take's number that lands inside the bound finishes the task, and nothing is
// taken back: the bound goes with the take (reportwait.go).
func TestTakeBoundedWithNothingHeldIsFinishedByAReportInsideTheBound(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	number := h.takeOf("the take whose report is slow")
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.advance(turnStartBound - time.Second)

	h.reportOf(1, number)

	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the report inside the bound, want task %d finished", p, number)
	}
	h.advance(2 * turnStartBound)
	h.wantFinishedOnce(finishedByReport)
	h.wantCharges()
	if lines := h.logs.lines(reportMissedLine); len(lines) != 0 {
		t.Fatalf("logged %q, want nothing taken back", lines)
	}
}

// A take a daemon start restored from the store is bounded with nothing held where this daemon sees
// every turn start since the take's counted turn (reportReaches): the store records no turn's end
// after the take's accept, the run the daemon before gave it running at the restart, and the first
// hello says the shim's backlog is whole and its agent answers on it. The producer here names
// nothing held in its reports (an older one, without the field), so no report gives the take its
// turn back, and the bound takes it back with no charge a whole bound after the given run's end.
//
// The negative control is the head before this change (dd71399b), whose bound runs only while
// work is held: the take keeps its turn until the process's next let-go.
func TestTakeBoundedWithNothingHeldRestoredWhereTheDaemonSeesEveryStart(t *testing.T) {
	h, number := takeAcrossARestart(t, givenRunRunning, withWaits)
	h.connectInTurn()
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.reportOf(2, 0)

	h.wantTakeBackAtTheBound(number)
	h.wantRetakenAndRunOnce(number)
}

// A take a daemon start restored from the store is left out of the bound with nothing held where
// this daemon may not see every turn start since the take's counted turn (reportReaches): its own
// turn may have started while no daemon served it, and the bound would fire during it and run the
// task twice. It keeps its turn for as long as it waits, charged nothing, and the report of its own
// turn, run after the bounds, finishes it once:
//
//   - the store records the given run's end after the take's accept (anchorRestored refuses it);
//   - the first hello does not say its shim's backlog is whole (bindRestoredAnchor refuses it);
//   - the hello says it is, and the agent does not answer on the connection (the same).
//
// They pass before this change too, since no bound ran with nothing held: their control is
// reportReaches answering true of every take, which takes each back at the bound.
func TestTakeBoundedWithNothingHeldLeavesARestoredTakeItCannotSeeWhole(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, hello func(h *harness)
	}{
		{"the store records the given run's end after the accept", func(h *harness) { h.hostsRun() }, func(h *harness) {
			h.connect()
			h.answerAsk(false)
		}},
		{"the hello does not say the backlog is whole", givenRunRunning, func(h *harness) {
			h.conn.SetStreaming(true)
			h.cutHello()
		}},
		{"the agent does not answer on the hello's connection", givenRunRunning, func(h *harness) {
			h.conn.FailGetState(errors.New("the shim closed the connection behind its ack"))
			h.connectInTurn()
			h.conn.FailGetState(nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, number := takeAcrossARestart(t, tc.before, withWaits)
			tc.hello(h)
			if h.state() == StateWorking {
				h.mustQuiet(StreamTurnEnd{Claim: h.token})
			}
			h.reportOf(2, 0)
			putBacks := len(h.records.of(notTheTakesTurnLine))

			h.advance(2 * turnStartBound)

			h.wantCharges()
			h.wantStillItsTurn(number, putBacks, "two bounds after the given run's end")
			if lines := h.logs.lines(reportMissedLine); len(lines) != 0 {
				t.Fatalf("logged %q, want the restored take left out of the bound", lines)
			}
			h.ownTurnFinishes(number, 3)
		})
	}
}
