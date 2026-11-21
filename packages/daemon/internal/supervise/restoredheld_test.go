package supervise

// The producer's word that an accepted take's text was held unrun (RequestTurnOutcome.Held), read
// for a take the daemon before a restart accepted, which the restart restored from the store
// (anchorRestored, bindRestoredAnchor, heldUnrun). Turn 1 is a task run and reported before the
// take; the take is task 2's, accepted under the daemon before the restart; each later report is
// driven by its own sequence.

import (
	"strconv"
	"testing"
	"time"
)

// takeAcrossARestart is a claim whose process ran a task, reported as turn 1, then took the next
// one under the daemon before a restart and did what before adds, the clock moving a second
// around the take as a real one does, and that daemon restarted under the process, its shim not
// yet connected to the one that started. It answers the take's number.
func takeAcrossARestart(t *testing.T, before func(h *harness), set ...func(*Deps)) (*harness, uint64) {
	t.Helper()
	h := newHarnessOf(t, queuedClaim(), append([]func(*Deps){currentPlugin}, set...)...)
	h.manualTakes()
	h.reach(StateReady)
	h.takeRunsItsOwnTurn("the task before the take", 1, false)
	h.clock.Advance(time.Second)
	number := h.takeOf("the take in flight across the restart")
	h.clock.Advance(time.Second)
	before(h)
	h.restart()
	return h, number
}

// givenRunRunning is the run the daemon before the restart gave the take, a run of the host's own,
// still running at the restart: the store holds the claim working, the take's turn started.
func givenRunRunning(h *harness) { h.must(StreamTurnStart{Claim: h.token}) }

// connectInTurn is the shim's first hello after the restart, its agent in the turn the store says
// runs, so the claim stays working for that turn's own end frame (askTurnOver).
func (h *harness) connectInTurn() {
	h.t.Helper()
	h.conn.SetStreaming(true)
	h.connect()
}

// wantNextTaskStartsItsOwnTurn holds that the claim's next task, taken, is given the next turn
// that starts: the turn's line names it and its take's turn is started, which a mark a report left
// for a turn still to come would have taken instead (Machine.reportedAhead).
func (h *harness) wantNextTaskStartsItsOwnTurn() {
	h.t.Helper()
	next := h.takeOf("the next task")
	starts := len(h.records.of(turnStartedLine))
	h.must(StreamTurnStart{Claim: h.token})
	wantAttr(h.t, h.records.of(turnStartedLine)[starts], "task", strconv.FormatUint(next, 10))
	if p := h.pending(); p.Number != next || !p.turnStarted() {
		h.t.Fatalf("the next task's start left %+v, want task %d's take with its turn started", p, next)
	}
}

// wantLateReportFinishesOnce is the report of the take of task number's own turn, as turn
// sequence, landing after the restart: it finishes the task, once, by the report.
func (h *harness) wantLateReportFinishesOnce(number, sequence uint64) {
	h.t.Helper()
	finished := len(h.records.of(finishedLine))
	h.reportOf(sequence, number)
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("pending %+v after the late report of task %d's own turn, want it finished", p, number)
	}
	lines := h.records.of(finishedLine)
	if len(lines) != finished+1 {
		h.t.Fatalf("%q was logged %d times by the late report, want once", finishedLine, len(lines)-finished)
	}
	wantAttr(h.t, lines[len(lines)-1], "by", finishedByReport)
}

// A take a daemon start restored is given its turn back by a report saying its text was held,
// where the store shows the daemon before the start handled no turn start after the one it gave
// the take, and R5's second wait then bounds a worker that never runs it:
//
//   - the take awaited its turn at the restart, and a run of the host's own whose setup spanned the
//     restart sent its start only after it (the planner's case): that start is the first this
//     daemon sees, given the take, and its report puts the take back;
//   - the run the daemon before gave the take was running at the restart, its end seen by this
//     daemon, then its report;
//   - the same report landing before the shim's first hello after the restart: it is kept, and the
//     hello that gives the anchor its connection puts the take back, ahead of the frames the shim
//     replays behind it;
//   - the take's own turn's report landing ahead of that turn's start frame, after the put-back:
//     the report finishes the task as a turn whose start is still to come (startedUnseen), the mark
//     it leaves takes that start (Machine.reportedAhead), and the next task's start is its own.
//
// The negative controls: the head before this change (4f4e1107), which anchors no restored take,
// fails each where the take is to await its turn again; and a restored take read only while the
// machine has seen no turn start since it was built (the candidate condition the plan review
// dropped) fails the first, whose given run's start this machine saw: its take is never put back.
func TestAHeldReportGivesATakeRestoredFromTheStoreItsTurnBack(t *testing.T) {
	t.Run("a take awaiting its turn, a run of the host's own starting only after the restart", func(t *testing.T) {
		h, number := takeAcrossARestart(t, nothingMore, withWaits)
		h.connect()
		h.hostsRun()
		if p := h.pending(); !p.turnStarted() {
			t.Fatalf("the first start after the restart left %+v, want it given the take", p)
		}
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantPutBackBy(number, putBacks, "held")

		h.advance(turnStartBound)

		h.wantCharges(WaitTurnStart)
		if p := h.pending(); p.taken() {
			t.Fatalf("pending %+v after the second bound, want the take taken back", p)
		}
	})
	t.Run("the given run running at the restart, its end seen, then its report", func(t *testing.T) {
		h, number := takeAcrossARestart(t, givenRunRunning, withWaits)
		h.connectInTurn()
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantPutBackBy(number, putBacks, "held")

		h.advance(turnStartBound)

		h.wantCharges(WaitTurnStart)
		if p := h.pending(); p.taken() {
			t.Fatalf("pending %+v after the second bound, want the take taken back", p)
		}
	})
	t.Run("the given run's report before the first hello after the restart", func(t *testing.T) {
		h, number := takeAcrossARestart(t, givenRunRunning)
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the held report, before any hello,")
		h.connectInTurn()
		h.wantPutBackBy(number, putBacks, "held")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.turnStarted() {
			t.Fatalf("the start after the given run left %+v, want it given the take", p)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
	})
	t.Run("the take's own report ahead of its start frame, after the put-back", func(t *testing.T) {
		h, number := takeAcrossARestart(t, givenRunRunning)
		h.connectInTurn()
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantPutBackBy(number, putBacks, "held")
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		starts := len(h.records.of(turnStartedLine))
		h.must(StreamTurnStart{Claim: h.token})
		wantAttr(t, h.records.of(turnStartedLine)[starts], "task", strconv.FormatUint(number, 10))
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantNextTaskStartsItsOwnTurn()
	})
}

// A restored take whose own turn may have run under the daemon before the restart, its frames
// seen by that daemon alone, is given its turn back by no report; nor is one whose own turn this
// daemon saw start, nor one whose connection is no longer the first hello's. The cases are the
// orders the plan review asked for:
//
//   - the given run ended before the restart, its report after it: the store cannot tell whether
//     the take's own turn also started there, so the report puts nothing back, and the take's own
//     turn, run after the restart, finishes the task;
//   - the take's own turn ran and ended before the restart, its report naming the task still on
//     its way, and the given run's held report landing first: nothing is put back, so the late
//     report finishes the task as the turn that ran it, once, and the next task's start is its own;
//   - the take's own turn running at the restart and ending after it, its report still on its way,
//     then the given run's held report: the store records a turn's end after the take's accept, so
//     nothing is put back, and the late report finishes the task;
//   - the take's own turn started after the restart, then the given run's held report: a start
//     followed the given run, and the word is moot;
//   - a reconnect after the first hello: a frame may have been lost with the first connection, and
//     the word is not read.
//
// Each passes at the head before this change (4f4e1107), which anchors no restored take: they pin
// the guards. They fail with fewer: the first three with every restored take anchored and read
// while the machine has seen no start since it was built, the claim idle and its connection the
// first hello's (the candidate rule), the third alone with the store's record of a turn's end
// after the accept left out of the anchoring, the fourth with no check of the starts since the
// given one, and the fifth with no check of the connection, each where the take is to keep its
// turn. Put back, the second and third cases' late report would read the take's own turn as one
// still to start (startedUnseen), whose mark then takes the next task's start; and a second wait
// that ran out before that report landed would take the task back, its turn having run, which the
// late report then reaches as no take's and finishes nothing.
func TestAHeldReportPutsBackNoRestoredTakeWhoseOwnTurnMayHaveRun(t *testing.T) {
	t.Run("the given run ended before the restart, its report after it", func(t *testing.T) {
		h, number := takeAcrossARestart(t, func(h *harness) { h.hostsRun() })
		h.connect()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the held report of a run that ended before the restart,")
		h.ownTurnFinishes(number, 3)
	})
	t.Run("the take's own turn ran and ended before the restart, its report still on its way", func(t *testing.T) {
		h, number := takeAcrossARestart(t, func(h *harness) {
			h.hostsRun()
			h.clock.Advance(time.Second)
			h.hostsRun()
		})
		h.connect()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the given run's late held report,")
		h.wantLateReportFinishesOnce(number, 3)
		h.wantNextTaskStartsItsOwnTurn()
	})
	t.Run("the take's own turn running at the restart and ending after it, its report still on its way", func(t *testing.T) {
		h, number := takeAcrossARestart(t, func(h *harness) {
			h.hostsRun()
			h.clock.Advance(time.Second)
			h.must(StreamTurnStart{Claim: h.token})
		})
		h.connectInTurn()
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the given run's late held report,")
		h.wantLateReportFinishesOnce(number, 3)
		h.wantNextTaskStartsItsOwnTurn()
	})
	t.Run("the take's own turn started after the restart, then the given run's report", func(t *testing.T) {
		h, number := takeAcrossARestart(t, givenRunRunning)
		h.connectInTurn()
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the held report, while the take's own turn runs,")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
	})
	t.Run("a reconnect after the first hello", func(t *testing.T) {
		h, number := takeAcrossARestart(t, givenRunRunning)
		h.connectInTurn()
		h.conns.Remove(h.token)
		h.connect()
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the held report on the second connection,")
		h.ownTurnFinishes(number, 3)
	})
}
