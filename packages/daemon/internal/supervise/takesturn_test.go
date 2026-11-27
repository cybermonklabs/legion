package supervise

// Which turn is the take's (dispatch://INFRA-2811). The daemon gives the first turn that starts
// after an accepted take to that take (turnStarted); a worker answered while a run no take
// started is running holds the text until that run is over and reports the run as no task's.
// These tests pin what the daemon then does: a report naming no task gives the take its turn
// back, a take sent again after the turn it was given started is answered the same text, and the
// end-frame bound a daemon start leaves to a restored take's turn runs from the worker's first
// act, not from the start.

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// testTakeContract is the take contract this package's tests hand a machine (Deps.TaskTakeContract):
// any number above 275, internal/api's LastNumberedContract, will do, since only api's registry
// knows the number a build gives the task-take entry, and every build gives it one above 275.
const testTakeContract = 276

// currentPlugin is a harness whose worker's plugin names its task's number in every turn's
// report: the daemon's own contract is the take contract (Deps.TaskTakeContract), which the
// harness's registration records for the claim (register).
func currentPlugin(d *Deps) { d.PluginContract = d.TaskTakeContract }

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
	if p := h.pending(); !p.startRecorded() || p.TakenText != text {
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

		if p := h.pending(); !p.awaitsStart() || p.TakenText != text {
			t.Fatalf("after the report the task is %+v, want its take awaiting its turn again with the text %q", p, text)
		}
		if stored, ok := h.store.delivery(h.token); !ok || !stored.awaitsStart() || stored.TakenText != text {
			t.Fatalf("the store holds %+v, want the take awaiting its turn again", stored)
		}
		if lines := h.logs.lines(notTheTakesTurnLine); len(lines) != 1 {
			t.Fatalf("logged %q, want the take's turn given back once", lines)
		}
		h.advance(2 * testRPC)
		if p := h.claim().Pending; p == nil || p.Number != number || !p.awaitsStart() {
			t.Fatalf("two end-frame bounds after the report the claim holds %+v, want task %d still awaiting its turn", p, number)
		}

		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.startRecorded() {
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
// for R2's report (takePhase), which finishes the task once before the stop. The third is R1's
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
		if p := h.pending(); !p.startRecorded() {
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
		if p := h.pending(); p.Number != next || !p.startRecorded() {
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

		if p := h.pending(); !p.startRecorded() {
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

		if p := h.pending(); !p.startRecorded() {
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
	if p := h.pending(); !p.startRecorded() || p.TakenText != text {
		t.Fatalf("the run that started after the take is %+v, want it given to the take", p)
	}
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if err := h.reportTurn(0, nil); err != nil {
		t.Fatalf("report the run as no task's: %v", err)
	}
	if p := h.pending(); !p.awaitsStart() {
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
			if p := h.pending(); !p.startRecorded() || p.TakenText != first {
				t.Fatalf("pending %+v after the repeated take, want the take's turn still started with its text", p)
			}
		})
	}
}

// A report the worker sends again to a daemon that restarted between the take's turn's end and
// the report reaches its task. For a claim whose plugin names no number — the one claim an
// end-frame bound watches (TakesNoTask) — the bound a restored ready or idle claim waits on
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
		if p := h.claim().Pending; p == nil || p.Number != number || !p.startRecorded() {
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
		if p := h.claim().Pending; p == nil || !p.startRecorded() {
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
			if p := h.claim().Pending; p == nil || p.Number != number || !p.startRecorded() {
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
	if second.Number == number || !second.startRecorded() {
		t.Fatalf("the claim holds %+v, want the second task's take with its turn started", second)
	}

	if err := h.m.Handle(h.ctx, first); err != nil {
		t.Fatalf("the report sent again = %v, want it answered as the first was", err)
	}
	h.m.Wait()

	if p := h.pending(); p.Number != second.Number || !p.startRecorded() {
		t.Fatalf("after the repeated report the claim holds %+v, want task %d's turn still started", p, second.Number)
	}
	if finished := h.records.of(finishedLine); len(finished) != 1 {
		t.Fatalf("%q was logged %d times, want once: the repeat finishes nothing again", finishedLine, len(finished))
	}
	if lines := h.logs.lines("a turn's report repeats one already applied"); len(lines) != 1 {
		t.Fatalf("the lines that say the report was a repeat are %q, want one", lines)
	}
}
