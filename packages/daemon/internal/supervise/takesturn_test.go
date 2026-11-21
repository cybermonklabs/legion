package supervise

// Which turn is the take's (dispatch://INFRA-2811). The daemon gives the first turn that starts
// after an accepted take to that take (turnStarted); a worker answered while a run no take
// started is running holds the text until that run is over and reports the run as no task's.
// These tests pin what the daemon then does: a report naming no task gives the take its turn
// back, a take sent again after the turn it was given started is answered the same text, and the
// end-frame bound a daemon start leaves to a restored take's turn runs from the worker's first
// act, not from the start.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// currentPlugin is a harness whose worker's plugin names its task's number in every turn's
// report: the daemon's own contract is TaskTakeContract, which the harness's registration records
// for the claim (register).
func currentPlugin(d *Deps) { d.PluginContract = TaskTakeContract }

// wantFinishedOnce holds that the claim's task was finished once, by by (finishedLine's "by").
func (h *harness) wantFinishedOnce(by string) {
	h.t.Helper()
	finished := h.records.of(finishedLine)
	if len(finished) != 1 {
		h.t.Fatalf("%q was logged %d times, want once", finishedLine, len(finished))
	}
	wantAttr(h.t, finished[0], "by", by)
}

// givesARunNoTakeStartedToTheTake walks the claim to an accepted take of a task, then a run no
// take started — a background result's, which starts before the take's answer reached the worker
// — that the daemon gives to the take, and that run's end, with no report behind it yet. It
// answers the text the take was answered with.
func (h *harness) givesARunNoTakeStartedToTheTake() string {
	h.t.Helper()
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan"})
	text := h.mustTake()
	h.must(StreamTurnStart{Claim: h.token})
	if p := h.pending(); !p.turnStarted() || p.TakenText != text {
		h.t.Fatalf("the run that started after the take is %+v, want it given to the take, its text kept", p)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	return text
}

// A worker whose plugin names its task's number reports the run the daemon gave its take as no
// task's: that run was not the take's. The take awaits its turn again, with the text it was
// answered, in memory and in the store, and nothing finishes the task meanwhile, since only a
// report naming its number finishes such a claim's task; the turn that starts next is the
// take's, and its report finishes the task. A worker whose plugin is older than the take names no
// number in any report, so its claim keeps the end-frame finish, which is what finishes a
// migrated old worker's task (decision 8).
//
// The negative control is the report's give-back taken out (notTheTakesTurn): the take then
// stays on the run that was not its own, and the next turn, the held text's, is no take's.
func TestTurnReportNamingNoTaskGivesTheTakeItsTurnBack(t *testing.T) {
	t.Run("a plugin that names its task's number", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		text := h.givesARunNoTakeStartedToTheTake()
		number := h.pending().Number

		if err := h.reportTurn(0, nil); err != nil {
			t.Fatalf("report the run as no task's: %v", err)
		}

		if p := h.pending(); !p.awaitsTurn() || p.TakenText != text {
			t.Fatalf("after the report the task is %+v, want its take awaiting its turn again with the text %q", p, text)
		}
		if stored, ok := h.store.delivery(h.token); !ok || !stored.awaitsTurn() || stored.TakenText != text {
			t.Fatalf("the store holds %+v, want the take awaiting its turn again", stored)
		}
		if lines := h.logs.lines(notTheTakesTurnLine); len(lines) != 1 {
			t.Fatalf("logged %q, want the take's turn given back once", lines)
		}
		h.advance(2 * testRPC)
		if p := h.claim().Pending; p == nil || p.Number != number || !p.awaitsTurn() {
			t.Fatalf("two end-frame bounds after the report the claim holds %+v, want task %d still awaiting its turn", p, number)
		}

		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.turnStarted() {
			t.Fatalf("the turn after the report is %+v, want it the take's", p)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the take's own turn was reported, want task %d finished", p, number)
		}
		h.wantFinishedOnce(finishedByReport)
	})
	t.Run("a plugin older than the take finishes at the end frame", func(t *testing.T) {
		h := newHarness(t)
		h.givesARunNoTakeStartedToTheTake()

		if err := h.reportTurn(0, nil); err != nil {
			t.Fatalf("report the run as no task's: %v", err)
		}
		if lines := h.logs.lines(notTheTakesTurnLine); len(lines) != 0 {
			t.Fatalf("logged %q, want no turn given back for a plugin that names no number", lines)
		}
		h.advance(testRPC)

		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v an end-frame bound after the run, want the task finished by its end", p)
		}
		h.wantFinishedOnce(finishedByEndFrame)
	})
}

// A report of a run reaches the take of the pending task only where the run is the turn the
// daemon gave that take, its generation and sequence that turn's (notTheTakesTurn, turnCount);
// and a report naming the take's number marks the start of its turn as still to come only where
// that start has not been seen (startedUnseen, Machine.reportedAhead).
//
// The first case is the report of the run given the take landing after the take's own run
// started. The take is accepted, and the run R1 that starts next is given it; the worker holds the
// text and runs it as R2 as soon as R1 ends, before it sends R1's report (pi-legion's agent_end
// hook starts the held text, then reports). R1's report, naming no task, lands while R2 runs: R1
// was not the take's turn, and R2, the turn after it, is, so the take's turn stays started, moved
// to R2 (notTheTakesTurn); R2's report, naming the take's number, lands after R2's end frame and
// finishes the task. R2's start was seen, so none is left to come for it, and the next task's
// turn is that task's: its start line names it, and its take's turn is started (startTakesTurn).
// The second case is the first with a suspension held over R2: R2's end leaves the hold waiting
// for R2's report (reportSettles), which finishes the task once before the stop. The third is R1's
// report landing only after R2 ended: R2 ran the take whole, so the take's turn is R2's and over,
// and a suspension then waits for R2's report. The fourth case is a run's report sent late, after
// the take's own turn started: the take was accepted after R1 ended, R2 is its turn, and R1's
// report naming no task lands while R2 runs. It is of an earlier turn than the take's and gives
// nothing back. The fifth case is the fourth with R1 run and ended before the agent said ready, an
// end the machine cannot number (turnEndedBeforeReady): its late report gives nothing back either.
//
// The negative controls: a mark set for a report whatever its turn (startedUnseen marking any
// ready or idle claim) names the finished task in the next task's start line and leaves that
// task's take awaiting its turn while the turn runs; a put-back by R1's report while a later run
// runs or has run (notTheTakesTurn before this change) leaves the take awaiting a turn it already
// had, so the suspension at R2's end, or after it, stops the claim before R2's report and gives
// the task back as never run, to run again on resume; a put-back by any report of the claim's
// generation (notTheTakesTurn with no sequence) gives the fourth case's take its turn back while
// that turn runs; and a turn's end before ready ignored with the count still known (the table's
// ignore before this change) gives R1's turn to the take in the fifth case, and R1's report then
// gives the take its turn back while that turn runs.
func TestTurnReportReachesTheTakeOnlyFromTheTurnItWasGiven(t *testing.T) {
	// givenR1RunningR2 is the take accepted, R1 given it and ended, and R2, the take's own run,
	// started; it answers the task's number.
	givenR1RunningR2 := func(t *testing.T) (*harness, uint64) {
		t.Helper()
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "the first task"})
		h.mustTake()
		number := h.pending().Number
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.must(StreamTurnStart{Claim: h.token})
		return h, number
	}
	t.Run("R1's report while the take's run runs, then its report after its end", func(t *testing.T) {
		h, number := givenR1RunningR2(t)
		if err := h.reportTurn(0, nil); err != nil {
			t.Fatalf("report R1 as no task's: %v", err)
		}
		if p := h.pending(); !p.turnStarted() {
			t.Fatalf("after R1's report the task is %+v, want its take's turn still started, moved to R2", p)
		}
		if lines := h.logs.lines(notTheTakesTurnLine); len(lines) != 0 {
			t.Fatalf("logged %q, want no turn given back while the take's own run runs", lines)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		if err := h.reportTurn(number, nil); err != nil {
			t.Fatalf("report R2 as task %d's: %v", number, err)
		}
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after R2's report, want task %d finished", p, number)
		}

		h.must(RequestDeliver{Claim: h.token, Task: "the second task"})
		h.mustTake()
		next := h.pending().Number
		starts := len(h.records.of(turnStartedLine))
		h.must(StreamTurnStart{Claim: h.token})

		wantAttr(t, h.records.of(turnStartedLine)[starts], "task", strconv.FormatUint(next, 10))
		if p := h.pending(); p.Number != next || !p.turnStarted() {
			t.Fatalf("while the next task's turn runs the claim holds %+v, want task %d's take with its turn started", p, next)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the next task's turn was reported, want task %d finished", p, next)
		}
	})
	t.Run("R1's report while the take's run runs, a suspension held over that run", func(t *testing.T) {
		h, number := givenR1RunningR2(t)
		if err := h.reportTurn(0, nil); err != nil {
			t.Fatalf("report R1 as no task's: %v", err)
		}
		holdsTheSuspension(h)
		stops := len(h.calls("Suspend"))
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantStillAwaitingItsReport(number, stops)

		if err := h.reportTurn(number, nil); err != nil {
			t.Fatalf("report R2 as task %d's: %v", number, err)
		}

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, resumedByTheOperator)
	})
	t.Run("R1's report after the take's run ended, a suspension before its report", func(t *testing.T) {
		h, number := givenR1RunningR2(t)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		if err := h.reportTurn(0, nil); err != nil {
			t.Fatalf("report R1 as no task's: %v", err)
		}
		stops := len(h.calls("Suspend"))
		h.wantStillAwaitingItsReport(number, stops)
		if err := h.handle(operatorSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the suspension after R1's late report answered %v, want it held for R2's report", err)
		}
		h.wantStillAwaitingItsReport(number, stops)

		if err := h.reportTurn(number, nil); err != nil {
			t.Fatalf("report R2 as task %d's: %v", number, err)
		}

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, resumedByTheOperator)
	})
	t.Run("a run's report sent after the take's turn started", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.manualTakes()
		h.reach(StateReady)
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.must(RequestDeliver{Claim: h.token, Task: "the first task"})
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})

		if err := h.reportTurn(0, nil); err != nil {
			t.Fatalf("report R1 as no task's: %v", err)
		}

		if p := h.pending(); !p.turnStarted() {
			t.Fatalf("after R1's late report the task is %+v, want its take's turn still started", p)
		}
		if lines := h.logs.lines(notTheTakesTurnLine); len(lines) != 0 {
			t.Fatalf("logged %q, want no turn given back by the report of an earlier turn", lines)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the take's turn was reported, want its task finished", p)
		}
		h.wantFinishedOnce(finishedByReport)
	})
	t.Run("a run that ended before ready, reported after the take's turn started", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.manualTakes()
		h.reach(StateRegistered)
		h.mustQuiet(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "the first task"})
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})

		if err := h.reportTurn(0, nil); err != nil {
			t.Fatalf("report R1 as no task's: %v", err)
		}

		if p := h.pending(); !p.turnStarted() {
			t.Fatalf("after the late report of the run before ready the task is %+v, want its take's turn still started", p)
		}
		if lines := h.logs.lines(notTheTakesTurnLine); len(lines) != 0 {
			t.Fatalf("logged %q, want no turn given back by the report of the run before ready", lines)
		}
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the take's turn was reported, want its task finished", p)
		}
		h.wantFinishedOnce(finishedByReport)
	})
}

// A take a report gave its turn back is in R5's second wait again from that report: a worker
// that then starts no turn is charged one second bound after it, and the take is taken back.
//
// The negative control is the report's give-back taken out (notTheTakesTurn): the take then
// stays on the run that was not its own, no second wait is on it, and no bound charges anyone.
func TestChargeTheSecondWaitOnATakeAReportGaveItsTurnBack(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.givesARunNoTakeStartedToTheTake()
	if err := h.reportTurn(0, nil); err != nil {
		t.Fatalf("report the run as no task's: %v", err)
	}

	h.advance(turnStartBound - time.Second)
	h.wantCharges()
	h.advance(time.Second)

	h.wantCharges(WaitTurnStart)
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v after the second bound, want the take taken back", p)
	}
}

// A daemon restarted under a live process does not know that process's count of turns, but every
// report after the restart names its turn's sequence, so a process that went on running and
// reporting tasks after the restart is one the daemon can count again. A take accepted then is in
// R5's second wait as it is in the daemon that never restarted: when the run the daemon gave the
// take is reported as no task's, the take awaits its turn again, and a worker that then starts no
// turn is charged one second bound after the report. The process ran a task before the restart
// and one after it, as a live worker does across a daemon deploy, and a run of its own (a
// background job's end, an Envoy turn) starts between the accepted take and the take's own turn.
// The seventh testing round of dispatch://INFRA-2811 saw this on the real stack (the tester's
// adversarial drive, step a10, after the daemon restart of step a9 and two tasks run and reported
// since): no charge came, and the take held a turn that had ended.
//
// The negative control is the head this test was written at (2a1a0831), where a restart builds
// the machine with its count not known for the rest of the process's launch generation, whatever
// the reports say, and notTheTakesTurn then gives no take its turn back.
func TestChargeTheSecondWaitOnATakeAReportGaveItsTurnBackAfterADaemonRestart(t *testing.T) {
	h := waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	for _, task := range []string{"the task before the restart", "the task after the restart"} {
		h.must(RequestDeliver{Claim: h.token, Task: task})
		h.mustTake()
		h.must(StreamTurnStart{Claim: h.token})
		h.must(StreamTurnEnd{Claim: h.token})
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after %s's turn was reported, want it finished", p, task)
		}
		if task == "the task before the restart" {
			h.restart()
			h.connect()
		}
	}

	h.must(RequestDeliver{Claim: h.token, Task: "the take a run of the host's own meets"})
	text := h.mustTake()
	h.must(StreamTurnStart{Claim: h.token})
	if p := h.pending(); !p.turnStarted() || p.TakenText != text {
		t.Fatalf("the run that started after the take is %+v, want it given to the take", p)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if err := h.reportTurn(0, nil); err != nil {
		t.Fatalf("report the run as no task's: %v", err)
	}
	if p := h.pending(); !p.awaitsTurn() {
		t.Errorf("after the report the task is %+v, want its take awaiting its turn again", p)
	}

	h.advance(turnStartBound)

	h.wantCharges(WaitTurnStart)
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v after the second bound, want the take taken back", p)
	}
}

// A take of the number the claim holds as its accepted, unfinished take is a take whose answer
// was lost, whether or not the turn the daemon gave the take has started: a run that started while
// the worker waited on its answer is given to the take, and a worker refused in-turn then would
// never run the text. It is answered the text the first answer carried, and applies nothing
// again — no write, no second adoption, no second telling of the workflow — in the daemon that
// answered it and in one a restart built over the stored claim, which keeps the text through the
// turn's start (Delivery.TakenText, TurnStarted).
//
// The negative control is the repeat answered only while the take awaits its turn
// (answeredFromRecord's case before this change): the take is then refused in-turn.
func TestTakeRepeatedAfterItsTurnStartedIsAnsweredTheSameAndRecordsNothing(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the same daemon", true: "after a daemon restart"}[restart], func(t *testing.T) {
			h := newHarness(t)
			confirmed := 0
			h.deps.TaskConfirmed = func(Claim) { confirmed++ }
			h.deps.Identity = adoptingIdentity
			h.restart()
			h.manualTakes()
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
			first := h.mustTake()
			h.must(StreamTurnStart{Claim: h.token})
			if restart {
				h.restart()
			}
			puts, log := len(h.store.history()), h.store.deliveryLog()

			answer := h.take()

			if answer.Outcome != TakeOutcomeAccepted || answer.Text != first {
				t.Fatalf("the take sent again after its turn started = %+v, want it accepted with %q", answer, first)
			}
			answered := h.records.of(answeredLine)
			wantAttr(t, answered[len(answered)-1], "repeat", "true")
			if got := len(h.store.history()); got != puts {
				t.Errorf("the repeated take wrote the claim %d times, want none", got-puts)
			}
			if got := h.store.deliveryLog(); len(got) != len(log) {
				t.Errorf("the repeated take wrote the delivery: %v", got[len(log):])
			}
			if confirmed != 1 {
				t.Errorf("the workflow was told %d times, want once", confirmed)
			}
			h.wantCalls("AdoptWorkingCopy", 1)
			if p := h.pending(); !p.turnStarted() || p.TakenText != first {
				t.Fatalf("pending %+v after the repeated take, want the take's turn still started with its text", p)
			}
		})
	}
}

// A report the worker sends again to a daemon that restarted between the take's turn's end and
// the report reaches its task. For a claim whose plugin names no number — the one claim an
// end-frame bound watches (endFrameFinishes) — the bound a restored ready or idle claim waits on
// for that turn runs from the worker's first act after the start, not from the start, so the time
// no daemon served the worker is no time its report had. A report of a provider error then takes
// the task to the ladder, rather than finding it finished by a bound that ran first; and a worker
// that comes back with a hello and no report has the bound run from that hello. (The harness's
// plugin contract is below TaskTakeContract.)
//
// The negative control is the bound armed at the machine's build again (NewMachine): it passes
// while the worker is still away, and the task is finished before its report lands.
func TestTurnOutcomeSentAgainToARestartedDaemonReachesItsTask(t *testing.T) {
	t.Run("a provider error's report takes the task to the ladder", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number := h.pending().Number

		h.outage(time.Minute)
		h.advance(2 * testRPC)
		if p := h.claim().Pending; p == nil || p.Number != number || !p.turnStarted() {
			t.Fatalf("two end-frame bounds after the restart, with no act of the worker's, the claim holds %+v, want task %d's turn still waiting for its report", p, number)
		}
		if err := h.reportTurn(number, &ProviderError{Message: "overloaded_error", Status: 529}); err != nil {
			t.Fatalf("send the report of the provider error again: %v", err)
		}

		if p := h.pending(); p.Number != number || p.taken() || p.ProviderErrorRetries != 1 {
			t.Fatalf("after the report the task is %+v, want task %d taken back for the ladder", p, number)
		}
		if finished := h.records.of(finishedLine); len(finished) != 0 {
			t.Fatalf("%q was logged %d times, want the task kept for the ladder", finishedLine, len(finished))
		}
	})
	t.Run("a hello with no report runs the bound from the hello", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()

		h.outage(time.Minute)
		h.advance(2 * testRPC)
		h.connect()
		h.advance(testRPC - time.Second)
		if p := h.claim().Pending; p == nil || !p.turnStarted() {
			t.Fatalf("less than a bound after the hello the claim holds %+v, want its take's turn still waiting for its report", p)
		}
		h.advance(time.Second)

		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v a bound after the hello, want the task finished by its turn's end", p)
		}
		h.wantFinishedOnce(finishedByEndFrame)
	})
}

// A claim whose plugin names its task's number has its task finished by the report that names
// it and by nothing else (decision 8 of dispatch://INFRA-2811): the end of the take's
// turn arms no bound, so a report that comes long after the end, in the same daemon or across a
// restart, still finds the task and finishes it.
//
// The negative control is the end-frame bound armed for such a claim again (armEndFrameBound
// arming whatever the plugin): the task is then finished by the end of its turn, and the report
// finds nothing.
func TestTurnOutcomeAloneFinishesTheTaskOfAPluginThatNamesItsNumber(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the same daemon", true: "across a daemon restart"}[restart], func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim(), currentPlugin)
			h.reach(StateReady)
			h.takesTurnEndsWithItsReportToCome()
			number := h.pending().Number
			if restart {
				h.outage(time.Minute)
				h.connect()
			}

			h.advance(10 * testRPC)
			if p := h.claim().Pending; p == nil || p.Number != number || !p.turnStarted() {
				t.Fatalf("ten end-frame bounds after the turn's end the claim holds %+v, want task %d's turn still waiting for its report", p, number)
			}
			if finished := h.records.of(finishedLine); len(finished) != 0 {
				t.Fatalf("%q was logged %d times before the report, want none", finishedLine, len(finished))
			}
			h.mustReport(nil)

			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v after the report, want task %d finished", p, number)
			}
			h.wantFinishedOnce(finishedByReport)
		})
	}
}

// heldOverATakesTurn is a claim whose plugin names its task's number, in the turn of its task's
// accepted take, holding the operator's suspension for that turn (holdSuspension), whose turn then
// ends with no report behind the end frame. It answers the task's number and the stops the
// runtime was asked for until then.
func heldOverATakesTurn(t *testing.T) (h *harness, number uint64, stops int) {
	t.Helper()
	h = newHarnessOf(t, queuedClaim(), currentPlugin)
	h.reach(StateReady)
	h.taskTheAgentTook()
	number = h.pending().Number
	h.must(StreamTurnStart{Claim: h.token})
	holdsTheSuspension(h)
	stops = len(h.calls("Suspend"))
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	return h, number, stops
}

// A suspension held for the turn of a task whose plugin names the task's number waits at that
// turn's end for the report that settles the task, and stops the process only once that report is
// applied, bounded by the hold's own deadline (TimerSuspend; rule two of the plan of
// dispatch://INFRA-2811). A report inside the deadline finishes the task once, the suspension then
// stops the process, and the resumed process is given nothing of the task. With no report by the
// deadline, the stop runs on time and takes the task back with the note of a cut turn, and the
// resumed process takes it once, behind that note.
//
// The negative control is the suspension run at the end frame (turnEnded before this change): the
// claim is suspended before the report comes, the suspended claim ignores the report, and settle
// takes the finished task back as a cut turn, so it runs again on resume.
func TestRelaunchOfAHeldSuspensionWaitsForTheTaskReport(t *testing.T) {
	t.Run("a report inside the deadline", func(t *testing.T) {
		h, number, stops := heldOverATakesTurn(t)
		h.wantState(StateIdle)
		h.advance(testStop - time.Second)
		h.wantState(StateIdle)
		h.wantCalls("Suspend", stops)
		if p := h.pending(); p.Number != number || !p.turnStarted() {
			t.Fatalf("the claim holds %+v before the report, want task %d's turn waiting for it", p, number)
		}

		h.mustReport(nil)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the report, want task %d finished", p, number)
		}
		h.wantFinishedOnce(finishedByReport)
		sent := len(h.prompts())
		resumedByTheOperator(h)
		for _, took := range h.prompts()[sent:] {
			if took.Number == number {
				t.Fatalf("the resumed process took %+v, want nothing of task %d, which its report finished", took, number)
			}
		}
	})
	t.Run("no report by the deadline", func(t *testing.T) {
		h, number, stops := heldOverATakesTurn(t)
		h.advance(testStop - time.Second)
		h.wantState(StateIdle)
		h.wantCalls("Suspend", stops)

		h.advance(time.Second)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
			t.Fatalf("the claim holds %+v after the deadline, want task %d taken back with the note of a cut turn", p, number)
		}
		if finished := h.records.of(finishedLine); len(finished) != 0 {
			t.Fatalf("%q was logged %d times, want none: no report finished the task", finishedLine, len(finished))
		}
		sent := len(h.prompts())
		resumedByTheOperator(h)
		var again []taken
		for _, took := range h.prompts()[sent:] {
			if took.Number == number {
				again = append(again, took)
			}
		}
		if len(again) != 1 {
			t.Fatalf("the resumed process took task %d %d times (%+v), want once", number, len(again), again)
		}
		wantSays(t, again[0].Message, saysTaskTurn)
	})
}

// A suspension asked again while a held suspension waits past its take's turn for the report that
// settles the task joins that hold (suspend, holdSuspension): the claim reads idle meanwhile, but
// the hold still waits for the report, so the request is answered ErrSuspendHeld and stops
// nothing. The report then finishes the task once, and the hold's stop runs after it. Both askers
// that ask again are covered: the operator's second suspend, and the daemon's suspension its row
// tries again (or stopElsewhere asks again), which the operator's hold takes over.
//
// The negative control is the idle row deciding the request (suspend with no fold): the process is
// stopped at the request, the report then meets a suspended claim, which ignores it, and settle
// takes back as a cut turn the task whose turn ran whole.
func TestASuspensionAskedAgainWhileTheHoldWaitsForTheReportJoinsTheHold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		again func(h *harness) RequestSuspend
	}{
		{"the operator's second suspend", operatorSuspension},
		{"the daemon's suspension asked again", func(h *harness) RequestSuspend {
			return RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "the issue left the workflow"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, number, stops := heldOverATakesTurn(t)

			if err := h.handle(tc.again(h)); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the suspension asked again while the hold waits for the report answered %v, want it held", err)
			}
			h.wantState(StateIdle)
			h.wantCalls("Suspend", stops)
			if p := h.pending(); p.Number != number || !p.turnStarted() {
				t.Fatalf("the claim holds %+v once the suspension was asked again, want task %d's turn waiting for its report", p, number)
			}

			h.mustReport(nil)

			h.wantState(StateSuspended)
			h.wantCalls("Suspend", stops+1)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v after the report, want task %d finished", p, number)
			}
			h.wantFinishedOnce(finishedByReport)
		})
	}
}

// reportToComeOfACurrentPlugin is a claim whose plugin names its task's number, idle over the task
// whose take's turn ran and ended with its report still to come (takesTurnEndsWithItsReportToCome).
// It answers the task's number and the stops the runtime was asked for until then.
func reportToComeOfACurrentPlugin(t *testing.T) (h *harness, number uint64, stops int) {
	t.Helper()
	h = newHarnessOf(t, queuedClaim(), currentPlugin)
	h.reach(StateReady)
	h.takesTurnEndsWithItsReportToCome()
	return h, h.pending().Number, len(h.calls("Suspend"))
}

// wantStillAwaitingItsReport holds that the let-go asked of the claim waits for the report of the
// take's turn: the claim is idle over the task whose turn ran, and the runtime was asked for
// stops stops and no launch.
func (h *harness) wantStillAwaitingItsReport(number uint64, stops int) {
	h.t.Helper()
	h.wantState(StateIdle)
	h.wantCalls("Suspend", stops)
	h.wantCalls("Resume", 0)
	if p := h.pending(); p.Number != number || !p.turnStarted() {
		h.t.Fatalf("the claim holds %+v, want task %d's turn still waiting for its report", p, number)
	}
}

// wantTakenBackWithItsNote holds that the task whose take's turn ran was taken back with the note
// of a cut turn, its report never having come, and finished by nothing.
func (h *harness) wantTakenBackWithItsNote(number uint64) {
	h.t.Helper()
	if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
		h.t.Fatalf("the claim holds %+v, want task %d taken back with the note of a cut turn", p, number)
	}
	if finished := h.records.of(finishedLine); len(finished) != 0 {
		h.t.Fatalf("%q was logged %d times, want none: no report finished the task", finishedLine, len(finished))
	}
}

// wantTakenOnceMore holds that the process next taking work, brought up by next, takes the task
// once, behind the note of a cut turn.
func (h *harness) wantTakenOnceMore(number uint64, next func(*harness)) {
	h.t.Helper()
	sent := len(h.prompts())
	next(h)
	var again []taken
	for _, took := range h.prompts()[sent:] {
		if took.Number == number {
			again = append(again, took)
		}
	}
	if len(again) != 1 {
		h.t.Fatalf("the next process took task %d %d times (%+v), want once", number, len(again), again)
	}
	wantSays(h.t, again[0].Message, saysTaskTurn)
}

// wantTakenNoMore holds that the process next taking work, brought up by next, takes nothing of
// the task its report finished.
func (h *harness) wantTakenNoMore(number uint64, next func(*harness)) {
	h.t.Helper()
	sent := len(h.prompts())
	next(h)
	for _, took := range h.prompts()[sent:] {
		if took.Number == number {
			h.t.Fatalf("the next process took %+v, want nothing of task %d, which its report finished", took, number)
		}
	}
}

// A suspension that reaches an idle claim whose take's turn has ended with its report still to
// come, and finds no hold, waits for that report as a suspension held over the turn does
// (suspend, holdSuspension, Machine.heldForReport): the report finishes the task once, and the
// stop runs after it. That includes the daemon's suspension its row asks again after a restart,
// which forgot the hold the first ask made. With no report by the suspension's own deadline the
// stop runs then, and takes the task back with the note of a cut turn first.
//
// The negative control is the idle row deciding the suspension at once (suspend before this
// change): the stop comes before the report, the suspended claim ignores the report, and settle
// takes back as a cut turn a task whose turn ran whole, so it runs again on resume.
func TestASuspensionOfAClaimWhoseReportIsToComeWaitsForIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
		ask     func(h *harness) RequestSuspend
	}{
		{"the operator's suspension", false, operatorSuspension},
		{"the daemon's suspension asked again after a restart", true, func(h *harness) RequestSuspend {
			return RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "the issue left the workflow"}
		}},
	} {
		t.Run(tc.name+", then the report", func(t *testing.T) {
			h, number, stops := reportToComeOfACurrentPlugin(t)
			if tc.restart {
				h.restart()
				h.connect()
			}

			if err := h.handle(tc.ask(h)); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the suspension of the claim whose report is to come answered %v, want it held for the report", err)
			}
			h.wantStillAwaitingItsReport(number, stops)
			h.mustReport(nil)

			h.wantState(StateSuspended)
			h.wantCalls("Suspend", stops+1)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("pending %+v after the report, want task %d finished", p, number)
			}
			h.wantFinishedOnce(finishedByReport)
		})
	}
	t.Run("the operator's suspension and no report by its deadline", func(t *testing.T) {
		h, number, stops := reportToComeOfACurrentPlugin(t)
		if err := h.handle(operatorSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the suspension of the claim whose report is to come answered %v, want it held for the report", err)
		}
		h.advance(testStop - time.Second)
		h.wantStillAwaitingItsReport(number, stops)

		h.advance(time.Second)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, resumedByTheOperator)
	})
}

// A ready the agent sends again while a held suspension waits past its take's turn for the report
// leaves that hold waiting (reready, Machine.heldForReport): the report finishes the task once, and
// the stop runs after it; with no report, the hold's deadline stops the claim and takes the task
// back with the note of a cut turn first. pi-legion sends its ready again from its capability
// schedule while its session is idle.
//
// The negative control is reready running every held suspension (reready before this change): the
// ready stops the process before the report, which the suspended claim ignores, and settle takes
// back as a cut turn a task whose turn ran whole.
func TestAReadySentAgainLeavesTheHoldWaitingForTheReport(t *testing.T) {
	readyAgain := func(h *harness) {
		h.t.Helper()
		h.must(RequestReady{Claim: h.token, Generation: h.generation(), Session: session})
	}
	t.Run("then the report", func(t *testing.T) {
		h, number, stops := heldOverATakesTurn(t)
		readyAgain(h)
		h.wantStillAwaitingItsReport(number, stops)

		h.mustReport(nil)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, resumedByTheOperator)
	})
	t.Run("no report by the hold's deadline", func(t *testing.T) {
		h, number, stops := heldOverATakesTurn(t)
		readyAgain(h)
		h.wantStillAwaitingItsReport(number, stops)

		h.advance(testStop)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, resumedByTheOperator)
	})
}

// A suspension held over a compaction that started after the take's turn ended waits at the
// compaction's end for the report of that turn (compactEnded, reportSettles), as one held over the
// turn waits at the turn's end: the claim is given back idle, held, and the compaction is answered
// that the hold took it (ErrCompactSuspended); the report finishes the task once and the stop runs
// after it; with no report, the hold's deadline stops the claim and takes the task back with the
// note of a cut turn first.
//
// The negative control is compactEnded running the hold at once (before this change): the stop
// comes before the report, which the suspended claim ignores, and settle takes back as a cut turn
// a task whose turn ran whole.
func TestACompactionsEndLeavesTheHoldWaitingForTheReport(t *testing.T) {
	compactedUnderAHold := func(t *testing.T) (*harness, uint64, int) {
		t.Helper()
		h, number, stops := reportToComeOfACurrentPlugin(t)
		release, compacted := h.compactHeld()
		holdsTheSuspension(h)
		release()
		if err := <-compacted; !errors.Is(err, ErrCompactSuspended) {
			t.Fatalf("the compaction a held suspension met at its end answered %v, want ErrCompactSuspended", err)
		}
		h.m.Wait()
		return h, number, stops
	}
	t.Run("then the report", func(t *testing.T) {
		h, number, stops := compactedUnderAHold(t)
		h.wantStillAwaitingItsReport(number, stops)

		h.mustReport(nil)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, resumedByTheOperator)
	})
	t.Run("no report by the hold's deadline", func(t *testing.T) {
		h, number, stops := compactedUnderAHold(t)
		h.wantStillAwaitingItsReport(number, stops)

		h.advance(testStop)

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", stops+1)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, resumedByTheOperator)
	})
}

// The stale-plugin relaunch of a claim whose take's turn ended with its report still to come, on a
// plugin that names its task's number, waits for that report (refreshPlugin, reportSettles), and a
// sweep that posts it again meanwhile changes nothing: the report finishes the task once, and the
// relaunch runs after it. With no report by the stop timeout (TimerReportWait, the bound the
// operator's suspension has), the task is taken back with the note of a cut turn and the relaunch
// runs, and the relaunched process takes the task once behind that note. A claim is stale on a
// plugin at TaskTakeContract only once a later daemon raises its contract, which is the daemon
// here.
//
// The negative control is refreshPlugin relaunching an idle claim at once (before this change):
// the relaunch takes the task back as a cut turn before its report, and the relaunched process
// runs a task whose turn ran whole.
func TestAStalePluginRelaunchWaitsForTheReportOfItsTakesTurn(t *testing.T) {
	staleOverAReportToCome := func(t *testing.T) (*harness, uint64, int) {
		t.Helper()
		h, number, stops := reportToComeOfACurrentPlugin(t)
		h.deps.PluginContract, h.m.deps.PluginContract = TaskTakeContract+1, TaskTakeContract+1
		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
		h.wantStillAwaitingItsReport(number, stops)
		return h, number, stops
	}
	t.Run("then the report", func(t *testing.T) {
		h, number, stops := staleOverAReportToCome(t)
		h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
		h.wantStillAwaitingItsReport(number, stops)

		h.mustReport(nil)

		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, relaunchedToReady)
	})
	t.Run("no report by the bound", func(t *testing.T) {
		h, number, stops := staleOverAReportToCome(t)
		h.advance(testStop - time.Second)
		h.wantStillAwaitingItsReport(number, stops)

		h.advance(time.Second)

		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, relaunchedToReady)
	})
}

// An end report sent again after its first attempt finished its task — the worker heard no
// answer — is answered as the first was, with no failure for the worker, and applies nothing
// again (decision 8 of dispatch://INFRA-2811): the task it named stays finished once,
// and the claim's next task, whose take's turn has started since, is not touched, though the
// report names a number no take of the claim holds now.
//
// The negative control is the repeat applied as a first report (turnOutcome with no stop at a
// repeat): it then reads as the report of the next task's turn naming another number, and gives
// that take its turn back.
func TestTurnOutcomeRepeatedAfterItsTaskFinishedAnswersTheSameAndAppliesNothing(t *testing.T) {
	h := newHarnessOf(t, queuedClaim(), currentPlugin)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "the first task"})
	number := h.pending().Number
	h.must(StreamTurnStart{Claim: h.token})
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.worker.sequence++
	first := RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence, Task: number}
	if err := h.m.Handle(h.ctx, first); err != nil {
		t.Fatalf("report the first task's turn: %v", err)
	}
	h.m.Wait()
	h.must(RequestDeliver{Claim: h.token, Task: "the second task"})
	h.must(StreamTurnStart{Claim: h.token})
	second := h.pending()
	if second.Number == number || !second.turnStarted() {
		t.Fatalf("the claim holds %+v, want the second task's take with its turn started", second)
	}

	if err := h.m.Handle(h.ctx, first); err != nil {
		t.Fatalf("the report sent again = %v, want it answered as the first was", err)
	}
	h.m.Wait()

	if p := h.pending(); p.Number != second.Number || !p.turnStarted() {
		t.Fatalf("after the repeated report the claim holds %+v, want task %d's turn still started", p, second.Number)
	}
	if finished := h.records.of(finishedLine); len(finished) != 1 {
		t.Fatalf("%q was logged %d times, want once: the repeat finishes nothing again", finishedLine, len(finished))
	}
	if lines := h.logs.lines("a turn's report repeats one already applied"); len(lines) != 1 {
		t.Fatalf("the lines that say the report was a repeat are %q, want one", lines)
	}
}

// A report's finish the store refused is made again by the end-frame bound, armed wherever the
// report arrived, and armed again after every write the store refuses, until one lands
// (decision 8 of dispatch://INFRA-2811): across a store outage of several bounds the
// task is finished once the store answers, once, by the report. The report can come after its
// turn's end frame, ahead of it — while the claim is still working, where the bound is armed at
// the turn's end — or, for a plugin that names no number, after the bound already fired and its
// own write was refused.
//
// The negative control is the bound armed once and not again after a refused write (finishTask
// with no re-arm): the finish is then lost with the first refusal, and the task stands unfinished
// after the store answers.
func TestTurnOutcomeFinishTheStoreRefusedIsMadeAgainAcrossAnOutage(t *testing.T) {
	outage := func(t *testing.T, h *harness, number uint64) {
		t.Helper()
		for bound := 1; bound <= 3; bound++ {
			h.advance(testRPC)
			if p := h.claim().Pending; p == nil || p.Number != number {
				t.Fatalf("bound %d of the outage left %+v, want task %d still pending", bound, p, number)
			}
		}
		h.store.fail("RetireDelivery", nil)
		h.advance(testRPC)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v a bound after the store answered, want task %d finished", p, number)
		}
		h.wantFinishedOnce(finishedByReport)
	}
	t.Run("a report after its turn's end", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number := h.pending().Number
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		outage(t, h, number)
	})
	t.Run("a report ahead of its turn's end frame", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.taskTheAgentTook()
		number := h.pending().Number
		h.must(StreamTurnStart{Claim: h.token})
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		h.advance(3 * testRPC)
		// The claim is still working, so no bound is armed until the turn's end frame, and the
		// three bounds that pass make nothing again.
		if _, armed := h.m.timers[TimerFinish]; armed {
			t.Fatal("the end-frame bound is armed while the claim is still working, want it armed at the turn's end")
		}
		if p := h.claim().Pending; p == nil || p.Number != number {
			t.Fatalf("three bounds into the turn the claim holds %+v, want task %d still pending", p, number)
		}
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		outage(t, h, number)
	})
	t.Run("a report after the bound fired", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number := h.pending().Number
		h.store.fail("RetireDelivery", errBoom)
		h.advance(testRPC)
		if err := h.reportTurn(number, nil); err == nil {
			t.Fatal("the report whose finish the store refused answered no failure")
		}
		outage(t, h, number)
	})
}

// A report's finish the store refused is owed still after a daemon restart (decision 8 of
// dispatch://INFRA-2811). The report is applied to the claim's window before its finish is
// written, so the worker's report sent again is a repeat that finishes nothing, and the daemon
// that saw the refusal owed the finish in memory alone. The restarted daemon reads it from the
// window — the newest applied report of the claim's generation that names the pending task's
// number, over a take that stands accepted (Claim.owedReport) — and the end-frame bound it arms
// at its start makes it, with no act of the worker's awaited: the task is finished once, by the
// report. A report that did nothing to the task leaves nothing owed, live or after a restart:
// one of an earlier generation, which the process the claim relaunched from sent after its
// death took the take of that number back and after the next process took it again, finishes
// nothing in the daemon it reaches (reachesLadder); and one that carried a provider error, whose
// take-back and ladder step the store took, is not of the take that stands now, the next one.
// The report of the take's own turn is what finishes the task then.
//
// The negative controls: the restored machine without the derivation (NewMachine with no
// owedReport) leaves the task whose finish the store refused pending, its turn over, with nothing
// to finish it; a report of any generation reaching the take (reachesLadder with no generation)
// finishes the next process's take by the earlier one's report; and a provider error's report
// owed whatever its ladder step (owedReport with no step) takes back the take after the ladder.
func TestTurnOutcomeFinishTheStoreRefusedIsOwedAgainAfterADaemonRestart(t *testing.T) {
	// owesNothing restarts the daemon over a claim idle after the turn of task number's take,
	// with that turn's report to come, and holds that no bound finishes the task before the
	// report does.
	owesNothing := func(t *testing.T, h *harness, number uint64) {
		t.Helper()
		if p := h.pending(); p.Number != number || !p.turnStarted() || h.state() != StateIdle {
			t.Fatalf("the claim is %s holding %+v, want it idle over task %d's turn, its report to come", h.state(), p, number)
		}
		h.restart()
		h.connect()
		h.advance(3 * testRPC)
		if p := h.claim().Pending; p == nil || p.Number != number || !p.turnStarted() {
			t.Fatalf("three bounds after the restart the claim holds %+v, want task %d's turn over and its report to come", p, number)
		}
		if finished := h.records.of(finishedLine); len(finished) != 0 {
			t.Fatalf("%q was logged %d times before the turn's report, want none", finishedLine, len(finished))
		}
		h.mustReport(nil)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v after the turn's report, want task %d finished", p, number)
		}
	}
	t.Run("a report naming its number, its finish refused", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number := h.pending().Number
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(number, nil); !errors.Is(err, errBoom) {
			t.Fatalf("the report whose finish the store refused = %v, want %v", err, errBoom)
		}
		kept := h.store.load(h.token).TurnOutcomes
		if len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number || kept[0].ProviderError {
			t.Fatalf("the store keeps %+v, want the report applied, naming task %d and no provider error", kept, number)
		}
		h.store.fail("RetireDelivery", nil)

		h.restart()
		h.advance(testRPC - time.Second)
		if p := h.claim().Pending; p == nil || p.Number != number || !p.turnStarted() {
			t.Fatalf("less than a bound after the restart the claim holds %+v, want task %d's turn over and its finish to come", p, number)
		}
		h.advance(time.Second)

		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending %+v a bound after the restart, want task %d finished", p, number)
		}
		h.advance(3 * testRPC)
		h.wantFinishedOnce(finishedByReport)
	})
	t.Run("a report of an earlier generation", func(t *testing.T) {
		h := newHarnessOf(t, queuedClaim(), currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number, left := h.pending().Number, h.generation()
		h.observe(runtime.Gone)
		h.relaunched()
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		// The report the dead process sent as it ended its turn reaches the daemon after its death
		// took the take back, and after the relaunched process took the task again and ran it.
		h.worker.sequence++
		h.must(RequestTurnOutcome{Claim: h.token, Generation: left, Sequence: h.worker.sequence, Task: number})
		if p := h.claim().Pending; p == nil || p.Number != number || !p.turnStarted() {
			t.Fatalf("after generation %d's report the claim holds %+v, want generation %d's take of task %d standing, its turn over",
				left, p, h.generation(), number)
		}
		kept := h.store.load(h.token).TurnOutcomes
		if h.generation() == left || len(kept) != 1 || kept[0].Generation != left || kept[0].Unapplied || kept[0].Task != number {
			t.Fatalf("the claim is at generation %d keeping %+v, want a later one keeping generation %d's report of task %d applied",
				h.generation(), kept, left, number)
		}
		owesNothing(t, h, number)
	})
	t.Run("a report that carried a provider error", func(t *testing.T) {
		h := workingClaim(t, currentPlugin)
		number := h.pending().Number
		h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
		h.climbs(1, promptRetryDelay(1))
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		kept := h.store.load(h.token).TurnOutcomes
		if len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number || !kept[0].ProviderError ||
			kept[0].ProviderErrorStatus != 529 || kept[0].ProviderErrorRetries != 1 {
			t.Fatalf("the store keeps %+v, want the report applied, naming task %d, its provider error and the ladder's first step", kept, number)
		}
		owesNothing(t, h, number)
	})
}

// A report naming the accepted take's number that comes ahead of the take's turn start, while the
// store refuses both the start's write and the finish's, leaves the finish owed (decision 8 of
// dispatch://INFRA-2811): the report is applied, so the worker's report sent again is a repeat,
// and nothing else would finish the task. In the daemon that received it the start stands in
// memory, the decision goes on to the finish, and the finish's refusal arms the end-frame bound;
// in a daemon restarted over what the store holds — the report applied, the take still awaiting
// its turn — the take's turn is started from the report and the finish is owed again. Either way
// the bound finishes the task once, by the report, with the serving credit a finished take's
// turn earns (retireTask): its phase entry served, not handed back as owed.
//
// The negative controls: the decision stopped at the start's refused write (reportedTask
// returning it) leaves no finish owed in the same daemon; a finish owed after a restart only over
// a take whose turn started (owedReport requiring it) leaves none; and the restored take's turn
// left unstarted (NewMachine without the start) finishes the task with its entry handed back.
func TestTurnOutcomeAheadOfItsTurnStartIsFinishedWhenTheStoreRefusedItsStartAndFinish(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "in the same daemon", true: "across a daemon restart"}[restart], func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim(), currentPlugin)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", Phase: phase.Implementing, Generation: 7, Entry: 1})
			number := h.pending().Number
			if p := h.pending(); !p.awaitsTurn() {
				t.Fatalf("the task is %+v, want its take accepted and its turn to come", p)
			}
			h.store.fail("PutDelivery", errBoom)
			h.store.fail("RetireDelivery", errBoom)
			if err := h.reportTurn(number, nil); !errors.Is(err, errBoom) {
				t.Fatalf("the report whose start and finish the store refused = %v, want %v", err, errBoom)
			}
			h.store.fail("PutDelivery", nil)
			h.store.fail("RetireDelivery", nil)
			if stored, ok := h.store.delivery(h.token); !ok || !stored.awaitsTurn() {
				t.Fatalf("the store holds %+v (held %v), want the take still awaiting its turn", stored, ok)
			}
			if restart {
				h.restart()
			}

			h.advance(testRPC - time.Second)
			if p := h.claim().Pending; p == nil || p.Number != number {
				t.Fatalf("less than a bound on the claim holds %+v, want task %d's finish to come", p, number)
			}
			h.advance(time.Second)

			c := h.claim()
			if c.Pending != nil || c.TaskFinished != number {
				t.Fatalf("a bound on the claim holds %+v and finished task %d, want task %d finished", c.Pending, c.TaskFinished, number)
			}
			if c.ServedEntry != 1 || c.GivenEntry != 1 {
				t.Fatalf("the finished claim served entry %d and was given entry %d, want entry 1 served", c.ServedEntry, c.GivenEntry)
			}
			h.wantFinishedOnce(finishedByReport)
		})
	}
}

// cutOffStore is the store going away part way through a decision: it takes the next takes
// writes of the claim and its delivery, whatever their kind, and refuses each one after them with
// errBoom. A negative takes is a store that takes every write.
type cutOffStore struct {
	Store
	mu    sync.Mutex
	takes int
}

func (s *cutOffStore) set(takes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.takes = takes
}

func (s *cutOffStore) admit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.takes < 0:
		return nil
	case s.takes == 0:
		return errBoom
	}
	s.takes--
	return nil
}

func (s *cutOffStore) PutClaim(ctx context.Context, c Claim) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.PutClaim(ctx, c)
}

func (s *cutOffStore) PutDelivery(ctx context.Context, token claim.Token, d Delivery) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.PutDelivery(ctx, token, d)
}

func (s *cutOffStore) PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.PutClaimAndDelivery(ctx, c, d)
}

func (s *cutOffStore) RetireDelivery(ctx context.Context, c Claim, deliveryID string) error {
	if err := s.admit(); err != nil {
		return err
	}
	return s.Store.RetireDelivery(ctx, c, deliveryID)
}

// providerErrorAsTheStoreGoes is a claim whose take's turn ended in a provider error of status,
// reported as the store went away: from the report on, the store takes takes writes — the
// report's own the first of them — and refuses each one after them, and it answers again once
// the report is decided. ahead is a report that comes ahead of its turn's start frame, so the
// report starts that turn itself (startedUnseen); otherwise the report follows the turn's end.
// It answers the task's number and the report's error.
func providerErrorAsTheStoreGoes(t *testing.T, status, takes int, ahead bool) (*harness, uint64, error) {
	t.Helper()
	cut := &cutOffStore{takes: -1}
	through := func(d *Deps) { cut.Store, d.Store = d.Store, cut }
	var h *harness
	if ahead {
		h = newHarnessOf(t, queuedClaim(), currentPlugin, through)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText, Phase: phase.Implementing, Generation: 7, Entry: 1})
	} else {
		h = workingClaim(t, currentPlugin, through)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
	}
	number := h.pending().Number
	cut.set(takes)
	err := h.reportTurn(number, &ProviderError{Message: "the provider answered an error", Status: status})
	cut.set(-1)
	return h, number, err
}

// refusedTakeBack is providerErrorAsTheStoreGoes with a store that takes the report's own write
// and refuses every write after it, so the report is applied and the take-back, the ladder's step
// and any failure of the claim are written nowhere. Across a restart, the daemon restarts over
// what the store holds and the worker's shim says hello: the claim's first decision.
func refusedTakeBack(t *testing.T, status int, restart, ahead bool) *harness {
	t.Helper()
	h, number, err := providerErrorAsTheStoreGoes(t, status, 1, ahead)
	if !errors.Is(err, errBoom) {
		t.Fatalf("the report whose take-back the store refused = %v, want %v", err, errBoom)
	}
	stored := h.store.load(h.token)
	if p := stored.Pending; p == nil || !p.taken() || p.turnStarted() == ahead || p.ProviderErrorRetries != 0 {
		t.Fatalf("the store holds %+v, want its take standing, its turn started %v, with no ladder step", p, !ahead)
	}
	if kept := stored.TurnOutcomes; len(kept) != 1 || kept[0].Unapplied || kept[0].Task != number {
		t.Fatalf("the store keeps %+v, want the report of task %d applied", kept, number)
	}
	if restart {
		h.restart()
		h.connect()
	}
	return h
}

// wantLadderOwedLine holds that a daemon restarted over a provider error's report whose take-back
// the store refused logged that take-back's making once (ladderOwedLine), and one that did not
// restart logged none.
func (h *harness) wantLadderOwedLine(restart bool) {
	h.t.Helper()
	want := map[bool]int{false: 0, true: 1}[restart]
	if lines := h.logs.lines(ladderOwedLine); len(lines) != want {
		h.t.Fatalf("logged %q, want the owed take-back's line %d times", lines, want)
	}
}

// A provider error's report whose take-back the store refused still takes the task to the ladder
// (dispatch://INFRA-956 R2, decision 8 of dispatch://INFRA-2811). The report is applied, so the
// worker's report sent again is a repeat, and what it does to the task is done by the decision
// that applied it or by nothing. In the daemon that received it the take-back stands in memory,
// and the ladder's step and its wait follow it, or, for a status that holds the issue, the
// claim's failure; the next decision writes them (settle, repair). In a daemon restarted over what
// the store holds — the report applied, the take still accepted, its ladder step not recorded —
// the claim's first decision makes the take-back and the step from the report
// (Machine.ladderOwed). Either way the task is counted one provider error and one step, and is
// given again once its wait is over; a status that holds the issue fails the claim. A report that
// came ahead of its turn's start frame started that turn first (startedUnseen), so the take-back
// keeps the handshake's confirmation of the take's entry, as any provider error's does: the turn
// started on the take's text (giveBackTake).
//
// The take-back, the ladder's step and the claim's run are one write (takeToLadder), so they land
// together or not at all: a store that goes away right after that write leaves a restarted
// daemon a task taken back with its step, owing nothing, whose next take carries the provider
// error's note; one that refuses it leaves the take standing with no step, which the restart owes
// the ladder (above).
//
// The negative controls: the ladder stopped at the take-back's refused write (takeToLadder
// returning it) leaves the task in the same daemon with no step and no wait, and the claim a 400
// ended running; the restored machine with no ladder owed (NewMachine without ladderOwed) leaves
// the take standing, its turn over, with nothing to take it back; a restored take awaiting its
// turn taken back with that turn unstarted gives back the entry's confirmation; and the take-back
// written apart from the step (takeBack's own write, then providerErrorClears'), with the second
// write refused, leaves the store a take-back with no step, which a restart gives again with no
// note.
func TestTurnOutcomeOfAProviderErrorWhoseTakeBackTheStoreRefusedTakesTheTaskToTheLadder(t *testing.T) {
	clears := func(t *testing.T, h *harness, restart bool) {
		t.Helper()
		p := h.pending()
		if p.taken() || p.ProviderErrorRetries != 1 || p.ProviderErrorNote == "" || !h.m.waitsForProviderError(p.ID) {
			t.Fatalf("the task is %+v (waiting %v), want it taken back, one ladder step counted and its wait armed",
				p, h.m.waitsForProviderError(p.ID))
		}
		if run := h.claim().ProviderErrorRun; run != 1 {
			t.Fatalf("the run of provider errors is %d, want 1", run)
		}
		h.advance(promptRetryDelay(1) - time.Second)
		h.wantPrompts(1)
		h.advance(time.Second)
		h.wantPrompts(2)
		if stored, ok := h.store.delivery(h.token); !ok || stored.ProviderErrorRetries != 1 || stored.ProviderErrorNote == "" {
			t.Fatalf("the store holds %+v (held %v), want the ladder's step written", stored, ok)
		}
		h.wantLadderOwedLine(restart)
	}
	for _, restart := range []bool{false, true} {
		name := map[bool]string{false: "in the same daemon", true: "across a daemon restart"}[restart]
		t.Run(name+", a status that clears", func(t *testing.T) {
			clears(t, refusedTakeBack(t, 529, restart, false), restart)
		})
		t.Run(name+", a report ahead of its turn's start", func(t *testing.T) {
			h := refusedTakeBack(t, 529, restart, true)
			if confirmed := h.claim().Handshake.ConfirmedAt; confirmed.IsZero() {
				t.Fatalf("the claim's handshake is %+v, want its confirmation of the take's entry kept", h.claim().Handshake)
			}
			clears(t, h, restart)
		})
		t.Run(name+", a status that holds the issue", func(t *testing.T) {
			h := refusedTakeBack(t, 400, restart, false)
			h.wantState(StateFailed)
			if failure := h.claim().Failure; !strings.Contains(failure, "HTTP 400") {
				t.Fatalf("failure %q, want the provider error's status named", failure)
			}
			h.wantLadderOwedLine(restart)
		})
	}
	t.Run("across a daemon restart, the take-back and the ladder's step written together", func(t *testing.T) {
		h, _, _ := providerErrorAsTheStoreGoes(t, 529, 2, false)
		stored := h.store.load(h.token)
		if p := stored.Pending; p == nil || p.taken() || p.ProviderErrorRetries != 1 || p.ProviderErrorNote == "" || stored.ProviderErrorRun != 1 {
			t.Fatalf("the store holds a run of %d and %+v, want the take-back, the ladder's step and the run written together",
				stored.ProviderErrorRun, p)
		}
		h.restart()
		h.connect()
		if taken := h.wantPrompts(2)[1]; !strings.Contains(taken.Message, providerErrorCause(529)) {
			t.Fatalf("the take after the restart was answered %q, want the provider error's note in front of the task", taken.Message)
		}
		if p := h.pending(); p.ProviderErrorRetries != 1 || h.claim().ProviderErrorRun != 1 {
			t.Fatalf("after the restart the task is %+v with a run of %d, want one step and a run of one", p, h.claim().ProviderErrorRun)
		}
		h.wantLadderOwedLine(false)
	})
}
