package supervise

// The producer's word that an accepted take's text was still held unrun at a turn's end
// (RequestTurnOutcome.Held), which is what gives a take its turn back while the claim's count of
// its process's turns is not known (notTheTakesTurn, heldUnrun): the first take after a daemon
// restarted under a live process, which no report of a take accepted since has made the count
// known for (learnTurns). Each report is driven by its own sequence, as the worker numbers its
// turns, after restartedUnderALiveProcess's turn 1.

import (
	"strconv"
	"testing"
)

// reportHeldOf is reportOf for a report that also says the accepted take of task held had its
// text held unrun at the end of the reported turn.
func (h *harness) reportHeldOf(sequence, number, held uint64) {
	h.t.Helper()
	h.reportRequest(RequestTurnOutcome{Sequence: sequence, Task: number, Held: held})
}

// reportRequest posts the worker's report r, its claim and generation the harness's, at the
// sequence r names.
func (h *harness) reportRequest(r RequestTurnOutcome) {
	h.t.Helper()
	r.Claim, r.Generation = h.token, h.generation()
	h.worker.sequence = max(h.worker.sequence, r.Sequence)
	if err := h.m.Handle(h.ctx, r); err != nil {
		h.t.Fatalf("report turn %d naming task %d, held %d: %v", r.Sequence, r.Task, r.Held, err)
	}
	h.m.Wait()
	h.workerAsks()
}

// wantPutBackBy holds that the take of task number awaits its turn again, put back once since
// putBacks lines of it were logged, by what by names (putBack).
func (h *harness) wantPutBackBy(number uint64, putBacks int, by string) {
	h.t.Helper()
	if p := h.pending(); p.Number != number || !p.awaitsTurn() {
		h.t.Fatalf("the claim holds %+v, want task %d's take awaiting its turn again", p, number)
	}
	lines := h.records.of(notTheTakesTurnLine)
	if len(lines) != putBacks+1 {
		h.t.Fatalf("%q was logged %d times, want %d", notTheTakesTurnLine, len(lines), putBacks+1)
	}
	wantAttr(h.t, lines[len(lines)-1], "by", by)
}

// ownTurnFinishes is the take of task number's own turn run and reported as turn sequence, which
// finishes the task.
func (h *harness) ownTurnFinishes(number, sequence uint64) {
	h.t.Helper()
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.reportOf(sequence, number)
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("pending %+v after the report of task %d's own turn, want it finished", p, number)
	}
}

// The first take after a daemon restarted under a live process, whose given run is the host's
// own, is put back by that run's report where the report says the take's text was held through
// it, and R5's second wait then charges a worker that does not start the take's turn within its
// bound. Without the word the count, not known, puts nothing back, and nothing bounds the take
// (d7383c99's re-learn rule, which only the report of a take's own turn moves). The word is read
// wherever the report lands among the frames of the turns that held the text:
//
//   - behind the given turn's end frame, and ahead of it, the claim still working that turn;
//   - ahead of the given turn's start frame: that start is given no take, and the next one is;
//   - two turns held the text, their reports landing out of order: the later turn's report is
//     moot while it is the only one, a start having followed the earlier turn as far as the machine
//     can tell, and the earlier turn's report then shows the later one held the text too, so the
//     take is put back; its own turn's report, right after the higher, then places the count
//     (learnTurns), and a later take a run of the host's own meets is given its turn back by the
//     count alone.
//
// The negative control is the head before this change (588c0d55), where no report puts a take
// back while the count is not known: every case but the one without the word fails where the take
// is to await its turn again, and the case ahead of the start frame where that start is given the
// take. The case ahead of the start frame fails too with the pass-over at the start taken out
// (turnStarted's heldUnrun case).
func TestAReportThatHeldTheTakesTextGivesTheFirstTakeAfterARestartItsTurnBack(t *testing.T) {
	t.Run("the report behind the given turn's end, then the second wait's bound", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore, withWaits)
		number := h.takeOf("the first take after the restart")
		h.hostsRun()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantPutBackBy(number, putBacks, "held")

		h.advance(turnStartBound)

		h.wantCharges(WaitTurnStart)
		if p := h.pending(); p.taken() {
			t.Fatalf("pending %+v after the second bound, want the take taken back", p)
		}
	})
	t.Run("without the word, the count not known puts nothing back and nothing bounds the take", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore, withWaits)
		number := h.takeOf("the first take after the restart")
		h.hostsRun()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportOf(2, 0)
		h.wantStillItsTurn(number, putBacks, "after the report of the run given the take, naming nothing held,")

		h.advance(turnStartBound)

		h.wantCharges()
		h.wantStillItsTurn(number, putBacks, "a second bound after that report")
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
	})
	t.Run("the report ahead of the given turn's end frame", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the first take after the restart")
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantPutBackBy(number, putBacks, "held")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		if p := h.pending(); !p.awaitsTurn() {
			t.Fatalf("after the given turn's end the claim holds %+v, want the take awaiting its turn", p)
		}
		h.ownTurnFinishes(number, 3)
	})
	t.Run("the report ahead of the given turn's start frame", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the first take after the restart")
		h.reportHeldOf(2, 0, number)
		starts := len(h.records.of(turnStartedLine))
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.awaitsTurn() || len(h.logs.lines(heldPassedOverLine)) != 1 {
			t.Fatalf("the start of the turn the report said held the text left %+v, want it given no take", p)
		}
		if _, named := h.records.of(turnStartedLine)[starts].attr("task"); named {
			t.Fatal("the start passed over names a task, want it no task's turn")
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.turnStarted() {
			t.Fatalf("the start after it left %+v, want it given the take", p)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
	})
	t.Run("two turns held the text, their reports out of order", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the first take after the restart")
		h.hostsRun()
		h.hostsRun()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(3, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the later held turn's report alone,")
		h.reportHeldOf(2, 0, number)
		h.wantPutBackBy(number, putBacks, "held")
		h.ownTurnFinishes(number, 4)
		if lines := h.logs.lines(turnsKnownAgainLine); len(lines) != 1 {
			t.Fatalf("logged %q, want the take's own report to place the count", lines)
		}
		h.wantTheRunGivenATakeGivenBack(5)
	})
}

// The word is read only where it is certain, and otherwise as a report without it, so it never
// puts back a take whose own turn is running or has run, which is review round 2's R2-3 class
// (notTheTakesTurn):
//
//   - a report saying the text was held, landing after the take's own turn started (the plan
//     review's case: that turn's start reached the daemon before the report), or after it ended
//     with its report still to come: a start has followed the reported turn's end, so the word is
//     moot, and the take's own report finishes the task;
//   - a report sent before the take's answer reached the worker, which names nothing held, landing
//     after the take's own turn started: it puts nothing back (shape A's hazard, which shape B has
//     no field for);
//   - a report of an earlier take's hold, sent late, after a provider error's ladder gave the
//     number again and the new take's own turn started: a take of a number taken before keeps no
//     anchor, so the word reaches it not;
//   - a report naming as held another number than the pending take's;
//   - a report landing after the shim reconnected, whose backlog dropped the take's own turn: on
//     a connection the take was not accepted on the starts seen say nothing, and put back, the take
//     would be finished by its own report as a turn still to start, whose mark would name it in the
//     next task's start.
//
// Each passes at the head before this change (588c0d55), which reads no word: they pin the guards.
// They fail where the take is to keep its turn under a word applied with fewer guards: the first
// two with no check of the starts since the given one (heldUnrun's unrun), which fails the case of
// two held turns above at its moot step too, the ladder's with a take of a number taken before
// anchored as a first one (anchorTake's first), the other number's with no check that the word
// names the pending take, and the reconnect's with no check of the connection.
func TestAReportThatHeldTheTakesTextPutsBackNoTakeWhoseOwnTurnStarted(t *testing.T) {
	t.Run("after the take's own turn started", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the first take after the restart")
		h.hostsRun()
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
	t.Run("after the take's own turn ended, its report still to come", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the first take after the restart")
		h.hostsRun()
		h.hostsRun()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the held report, the take's own turn over,")
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
	})
	t.Run("a report sent before the take's answer, after the take's own turn started", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		h.hostsRun()
		number := h.takeOf("the first take after the restart")
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportOf(2, 0)
		h.wantStillItsTurn(number, putBacks, "after the late report of a run before the take, while the take's own turn runs,")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
	})
	t.Run("an earlier take's hold reported after a provider error's ladder gave the number again", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("a take whose own turn the provider fails")
		h.hostsRun()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportRequest(RequestTurnOutcome{Sequence: 3, Task: number, Refused: true, Message: "overloaded",
			ProviderError: &ProviderError{Message: "overloaded_error", Status: 529}})
		if p := h.pending(); p.Number != number || p.taken() {
			t.Fatalf("after the provider error the claim holds %+v, want task %d taken back to its ladder", p, number)
		}
		h.advance(promptRetryDelay(1))
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the earlier take's held report, while the new take's own turn runs,")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(4, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the new take's own turn, want it finished", p)
		}
	})
	t.Run("a report naming another number held", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the first take after the restart")
		h.hostsRun()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number+1)
		h.wantStillItsTurn(number, putBacks, "after a report naming another number held,")
		h.ownTurnFinishes(number, 3)
	})
	t.Run("after the shim reconnected, its backlog having dropped the take's own turn", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the first take after the restart")
		h.hostsRun()
		h.conns.Remove(h.token)
		h.connect()
		putBacks := len(h.records.of(notTheTakesTurnLine))
		h.reportHeldOf(2, 0, number)
		h.wantStillItsTurn(number, putBacks, "after the held report on the new connection,")
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		next := h.takeOf("the next task")
		starts := len(h.records.of(turnStartedLine))
		h.must(StreamTurnStart{Claim: h.token})
		wantAttr(t, h.records.of(turnStartedLine)[starts], "task", strconv.FormatUint(next, 10))
		if p := h.pending(); p.Number != next || !p.turnStarted() {
			t.Fatalf("the next task's start left %+v, want task %d's take with its turn started", p, next)
		}
	})
}
