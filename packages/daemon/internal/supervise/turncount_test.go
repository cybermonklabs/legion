package supervise

// The count of a process's turns a machine keeps (turnCount) after it lost it: a daemon
// restarted under the process, a turn that ended before the agent said ready, a start over a
// running turn. The report of a take's own turn makes it known again where that report places
// the turn the daemon gave the take for certain (learnTurns); everywhere else the count stays
// not known, and no report gives a take its turn back meanwhile (notTheTakesTurn). These tests
// drive each report by its own sequence, as the worker numbers its turns, because the orders
// that matter are the ones where a report and the frames of its turn arrive apart.

import "testing"

// reportOf is the worker's report of the turn its producer numbered sequence, naming task number
// (0 for none), sent where the test says rather than right behind the turn's end: a report
// travels apart from the frames of its turn, and a report sent again comes late.
func (h *harness) reportOf(sequence, number uint64) {
	h.t.Helper()
	h.worker.sequence = max(h.worker.sequence, sequence)
	if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(),
		Sequence: sequence, Task: number}); err != nil {
		h.t.Fatalf("report turn %d naming task %d: %v", sequence, number, err)
	}
	h.m.Wait()
	h.workerAsks()
}

// takeOf is task delivered and its take accepted; it answers the task's number.
func (h *harness) takeOf(task string) uint64 {
	h.t.Helper()
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.mustTake()
	return h.pending().Number
}

// hostsRun is a run of the host's own, a background result's or an Envoy notice's, that starts
// and ends with its report still to come.
func (h *harness) hostsRun() {
	h.t.Helper()
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
}

// takeRunsItsOwnTurn is task taken and its own turn the turn the daemon gives it, reported as
// turn sequence ahead of that turn's end frame or behind it; the report finishes the task.
func (h *harness) takeRunsItsOwnTurn(task string, sequence uint64, aheadOfTheEnd bool) {
	h.t.Helper()
	number := h.takeOf(task)
	h.must(StreamTurnStart{Claim: h.token})
	if aheadOfTheEnd {
		h.reportOf(sequence, number)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if !aheadOfTheEnd {
		h.reportOf(sequence, number)
	}
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("pending %+v after the report of task %d's turn, want it finished", p, number)
	}
}

// wantTheRunGivenATakeGivenBack holds that the claim's count of its process's turns is known and
// right: a take is given a run of the host's own, which the worker reports as turn sequence
// naming no task, and that report gives the take its turn back; the take's own turn then runs,
// is reported as the turn after it, and finishes the task. A count one turn off, too high or too
// low, numbers the run given the take another sequence than its report's, and the report gives
// nothing back, as a count not known gives nothing back.
func (h *harness) wantTheRunGivenATakeGivenBack(sequence uint64) {
	h.t.Helper()
	number := h.takeOf("a take a run of the host's own meets")
	h.hostsRun()
	putBacks := len(h.logs.lines(notTheTakesTurnLine))
	h.reportOf(sequence, 0)
	if p := h.pending(); !p.awaitsTurn() || len(h.logs.lines(notTheTakesTurnLine)) != putBacks+1 {
		h.t.Fatalf("after the report of the run given task %d's take, as turn %d, the claim holds %+v, want the take awaiting its turn again",
			number, sequence, p)
	}
	h.must(StreamTurnStart{Claim: h.token})
	if p := h.pending(); !p.turnStarted() {
		h.t.Fatalf("the turn after the report is %+v, want it task %d's take's", p, number)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.reportOf(sequence+1, number)
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("pending %+v after the report of task %d's own turn, want it finished", p, number)
	}
}

// wantTheCountRightFrom is a take whose own turn is the one given it, reported as turn sequence,
// every earlier turn reported before its take, and then wantTheRunGivenATakeGivenBack: a count
// not known is placed at that report, and a count known already, rightly, stays right.
func (h *harness) wantTheCountRightFrom(sequence uint64) {
	h.t.Helper()
	h.takeRunsItsOwnTurn("a take whose own turn is the one given it", sequence, false)
	h.wantTheRunGivenATakeGivenBack(sequence + 1)
}

// wantStillItsTurn holds that the take of task number still has its turn started, no report
// having given it back since putBacks lines of it were logged.
func (h *harness) wantStillItsTurn(number uint64, putBacks int, why string) {
	h.t.Helper()
	if p := h.pending(); p.Number != number || !p.turnStarted() || len(h.logs.lines(notTheTakesTurnLine)) != putBacks {
		h.t.Fatalf("%s the claim holds %+v, want task %d's take with its turn started", why, p, number)
	}
}

// restartedUnderALiveProcess is a claim whose process ran a task, reported as turn 1, then ran
// what before adds, and a daemon restarted under that process, its shim connected again; set
// shapes the machine's deps, as newHarnessOf's do.
func restartedUnderALiveProcess(t *testing.T, before func(h *harness), set ...func(*Deps)) *harness {
	t.Helper()
	h := newHarnessOf(t, queuedClaim(), append([]func(*Deps){currentPlugin}, set...)...)
	h.manualTakes()
	h.reach(StateReady)
	h.takeRunsItsOwnTurn("the task before the restart", 1, false)
	before(h)
	h.restart()
	h.connect()
	return h
}

// nothingMore is no turn more before the restart.
func nothingMore(*harness) {}

// The report of a take's own turn makes the claim's count of its process's turns known again
// where the machine lost it, and the next take a run of the host's own meets is given its turn
// back by that run's report, as in a daemon that never lost the count. The take is one the
// machine accepted while the count was not known, a number never taken before, the turn given it
// the only start since and the one right after every report the window held at the take: that
// turn is the take's own, whose sequence the report names.
//
//   - A daemon restarted under the process, the report behind its turn's end frame and ahead of it
//     (the claim still working): the count moves to the report's sequence at the report, and the
//     end that follows it is counted on top, so a count that took the report's sequence for the
//     ends seen whatever the claim was doing is one turn high in the second case.
//   - A turn that ended before the agent said ready, which pi-legion does not number and the
//     Claude bridge does: the producer's numbering of it is read off the report.
//   - A start over a running turn (turnStartedWhileBusy), which says the end of the turn given the
//     take was missed and the turn running now may be the take's own: the given turn's late report
//     gives nothing back while the count is not known, and a later take's report places the count.
//   - The agent's answer to the ask the placement makes, delivered behind the frames of the take's
//     own turn, which shows no start after the turn given the take: the count stands.
//
// The negative controls: the head before this change, where a lost count stays lost for the rest
// of the process's generation and no take is given its turn back again; a count placed by the
// report's sequence as the ends seen, which is one turn high ahead of the end frame; and, for the
// start over a running turn, the head before this change, which gave the take back its turn by
// the given turn's report while the take's own turn ran.
func TestAReportOfATakesOwnTurnMakesTheTurnCountKnownAgain(t *testing.T) {
	for _, ahead := range []bool{false, true} {
		name := map[bool]string{false: "after a daemon restart, the report behind its turn's end frame",
			true: "after a daemon restart, the report ahead of its turn's end frame"}[ahead]
		t.Run(name, func(t *testing.T) {
			h := restartedUnderALiveProcess(t, nothingMore)
			h.takeRunsItsOwnTurn("the task after the restart", 2, ahead)
			h.wantTheRunGivenATakeGivenBack(3)
		})
	}
	t.Run("after a daemon restart, the agent's answer behind the turn's frames", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		h.takeRunsItsOwnTurn("the task after the restart", 2, false)
		h.must(StreamTurnAnswered{Claim: h.token, Ask: h.conn.LastAsk()})
		h.wantTheRunGivenATakeGivenBack(3)
	})
	for _, numbered := range []bool{false, true} {
		name := map[bool]string{false: "after a turn ended before ready that its producer did not number",
			true: "after a turn ended before ready that its producer numbered"}[numbered]
		t.Run(name, func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim(), currentPlugin)
			h.manualTakes()
			h.reach(StateRegistered)
			h.mustQuiet(StreamTurnStart{Claim: h.token})
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.reach(StateReady)
			next := uint64(1)
			if numbered {
				h.reportOf(1, 0)
				next = 2
			}
			h.takeRunsItsOwnTurn("the first task", next, false)
			h.wantTheRunGivenATakeGivenBack(next + 1)
		})
	}
	t.Run("after a start over a running turn", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		number := h.takeOf("the first task")
		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.logs.lines(notTheTakesTurnLine))
		h.reportOf(1, 0)
		h.wantStillItsTurn(number, putBacks, "after the given turn's report, while the turn started over it runs,")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(2, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		h.wantTheCountRightFrom(3)
	})
}

// A report that cannot place the turn given a take for certain leaves the count of the process's
// turns not known, and no report gives a take its turn back meanwhile; the report of a later
// take's own turn that can places it, rightly (wantTheCountRightFrom). Each case is an order of a
// report and the frames where a rule that placed the count on less would place it one turn off:
//
//   - A report of a turn that ended before the restart, sent again after it, the claim idle, and
//     another such report while the next take's own turn runs. A count taken from a report's
//     sequence whenever the claim is not working (the seventh testing round's probe) is one turn
//     low after the first, and the second then gives back the take whose own turn runs.
//   - The report of a take's own turn landing after the next turn's end frame, that run's own
//     report landing while the next take's own turn runs. The same probe is one turn low, and the
//     late report gives back a take whose own turn runs.
//   - The run given a take is the host's own while the count is not known: its report gives
//     nothing back, and the take's own turn, the one after it, places no count. A count placed by
//     any report naming the take's number, from the turn the take was given, is one turn high.
//   - The report of a take's own turn ahead of its start frame, the run given the take the host's
//     own: the report names the sequence after the given run's, not the one right after the
//     window's newest, and places nothing. Placed from the turn given, the count is one turn high.
//   - The start of a turn that ended before the take, reported before it, seen after the take, as
//     a shim replays behind its hello what it held while no daemon heard it: the take's own turn
//     starts after it, and its report places nothing. Placed from the turn given, the count is one
//     turn high.
//   - The start of the take's own turn dropped from the backlog of a shim that reconnected, its
//     end seen as a turn never seen to begin, and a run of the host's own given the take: the
//     connection the take was accepted on is gone, and the report places nothing. Placed from the
//     turn given, the count is one turn low.
//   - A late report of an earlier take of the number, the second wait having taken that take back
//     and the worker having run its text after all, landing while the number's next take runs the
//     turn given it. The report names the pending take's number and the sequence right after the
//     window's newest, but the number was taken before, and it places nothing. A take of a number
//     taken before, kept like a first one's, places the count one turn low.
//   - The start of a turn that ended before the take, reported before it, seen after the take, and
//     the report of the take's own turn ahead of that turn's start frame: nothing the machine has
//     seen tells it from the turn given the take being the take's own, so the report places the
//     count, and asks the agent whether it is in a turn. The answer comes behind the frames the
//     shim sent before it, the take's own turn's start among them, and that start after the turn
//     given the take takes the placement back; an answer to another ask, delivered before those
//     frames, checks nothing. Kept, or taken as checked by that other answer, the count is one
//     turn high.
//   - An agent that does not answer that ask: nothing then says the placement held, so it is taken
//     back at once, and the report of the run given the next take gives nothing back. A placement
//     kept unconfirmed gives that take back.
//
// Every case passes at the head before this change (8a51a149) but for its last step, since a
// count lost there stays lost and no take is given its turn back again; each count placement
// named above fails its case at the step it names. At the head that placed the count without
// asking the agent (d7383c99), the case of the report ahead of its start frame fails at its last
// step, and the case of the agent that does not answer where the next take is given back.
func TestAReportPlacesNoTurnCountItCannotPlaceForCertain(t *testing.T) {
	t.Run("a report of a turn that ended before the restart, sent again after it", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, func(h *harness) {
			h.hostsRun()
			h.hostsRun()
		})
		h.reportOf(2, 0)
		number := h.takeOf("the task after the restart")
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.logs.lines(notTheTakesTurnLine))
		h.reportOf(3, 0)
		h.wantStillItsTurn(number, putBacks, "after a report sent again of a turn before the restart, while the take's own turn runs,")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(4, number)
		h.wantTheCountRightFrom(5)
	})
	t.Run("the report of a take's own turn after the next turn's end frame", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the task after the restart")
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.hostsRun()
		h.reportOf(2, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		next := h.takeOf("the next task")
		h.must(StreamTurnStart{Claim: h.token})
		putBacks := len(h.logs.lines(notTheTakesTurnLine))
		h.reportOf(3, 0)
		h.wantStillItsTurn(next, putBacks, "after the late report of the run before the take, while the take's own turn runs,")
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(4, next)
		h.wantTheCountRightFrom(5)
	})
	t.Run("the run given a take while the count is not known is the host's own", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the task after the restart")
		h.hostsRun()
		putBacks := len(h.logs.lines(notTheTakesTurnLine))
		h.reportOf(2, 0)
		h.wantStillItsTurn(number, putBacks, "after the report of the run given the take, the count not known,")
		h.hostsRun()
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		h.wantTheCountRightFrom(4)
	})
	t.Run("the report of a take's own turn ahead of its start frame, the run given it the host's own", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the task after the restart")
		h.hostsRun()
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		h.hostsRun()
		h.reportOf(2, 0)
		h.wantTheCountRightFrom(4)
	})
	t.Run("the start of a turn that ended before the take, seen after it", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		h.reportOf(2, 0)
		number := h.takeOf("the task after the restart")
		h.hostsRun()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		h.wantTheCountRightFrom(4)
	})
	t.Run("the start of the take's own turn dropped from the backlog of a shim that reconnected", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		number := h.takeOf("the task after the restart")
		h.conns.Remove(h.token)
		h.connect()
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		h.reportOf(2, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, 0)
		h.wantTheCountRightFrom(4)
	})
	t.Run("a late report of an earlier take of a number taken again", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore, withWaits)
		number := h.takeOf("a take the second wait gives back")
		h.advance(turnStartBound)
		h.wantCharges(WaitTurnStart)
		if p := h.pending(); p.Number != number || p.taken() {
			t.Fatalf("pending %+v after the second bound, want task %d taken back", p, number)
		}
		h.hostsRun()
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})
		h.reportOf(2, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the late report naming task %d, want it finished", p, number)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reportOf(3, number)
		h.wantTheCountRightFrom(4)
	})
	t.Run("the start of a turn that ended before the take, seen after it, the take's own report ahead of its start frame", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		h.reportOf(2, 0)
		number := h.takeOf("the task after the restart")
		h.hostsRun()
		h.reportOf(3, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		h.must(StreamTurnAnswered{Claim: h.token, Ask: "an earlier ask"})
		h.hostsRun()
		h.must(StreamTurnAnswered{Claim: h.token, Ask: h.conn.LastAsk()})
		h.wantTheCountRightFrom(4)
	})
	t.Run("an agent that does not answer the ask a placement makes", func(t *testing.T) {
		h := restartedUnderALiveProcess(t, nothingMore)
		h.conn.FailGetState(errBoom)
		h.takeRunsItsOwnTurn("the task after the restart", 2, false)
		h.conn.FailGetState(nil)
		number := h.takeOf("the next task")
		h.hostsRun()
		putBacks := len(h.logs.lines(notTheTakesTurnLine))
		h.reportOf(3, 0)
		h.wantStillItsTurn(number, putBacks, "after the report of the run given the take, the count unconfirmed,")
		h.hostsRun()
		h.reportOf(4, number)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report of the take's own turn, want it finished", p)
		}
		h.wantTheCountRightFrom(5)
	})
}
