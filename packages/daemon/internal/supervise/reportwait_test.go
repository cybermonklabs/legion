package supervise

// The wait for a take's report (reportwait.go). What the machine holds for the take's turn — a
// held suspension, a stale-plugin relaunch, a wake whose turn comes meanwhile — waits past that
// turn's end for the report that settles the task. These tests pin that wait: at a turn's end and
// a compaction's, for work asked again or asked first meanwhile, through a put-back of the take,
// in each order the reports and the take's own turn can reach the daemon, and with no report by
// the hold's deadline or the wait's bound.

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

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
		if p := h.pending(); p.Number != number || !p.startRecorded() {
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
			if p := h.pending(); p.Number != number || !p.startRecorded() {
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
	if p := h.pending(); p.Number != number || !p.startRecorded() {
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
// (suspend, holdForReport, reportHolds): the report finishes the task once, and the
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
// leaves that hold waiting (reready, heldWaits): the report finishes the task once, and
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
// compaction's end for the report of that turn (compactEnded, takePhase), as one held over the
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
// plugin that names its task's number, waits for that report (refreshPlugin, reportHolds), and a
// sweep that posts it again meanwhile changes nothing: the report finishes the task once, and the
// relaunch runs after it. A report slower than the stop timeout, held up by an attempt the daemon
// left unanswered for the producer's 5 minutes and the longest wait before the next attempt
// (pi-shared's DAEMON_ANSWER_TIMEOUT_MS, DAEMON_RESTART_MAX_DELAY_MS), still finishes the task.
// With no report by R5's second bound (TimerReport, Timeouts.TurnStart), the task is taken back
// with the note of a cut turn and the relaunch runs, and the relaunched process takes the task
// once behind that note. A claim is stale on a plugin at TaskTakeContract only once a later daemon
// raises its contract, which is the daemon here.
//
// The negative controls: refreshPlugin relaunching an idle claim at once (before INFRA-2811's
// round 2) takes the task back as a cut turn before its report, and the relaunched process runs a
// task whose turn ran whole; the wait bounded by the stop timeout (TimerReportWait at 7ade1c0c)
// takes back the task whose report is only slow, and the relaunched process runs it again.
func TestAStalePluginRelaunchWaitsForTheReportOfItsTakesTurn(t *testing.T) {
	staleOverAReportToCome := func(t *testing.T) (*harness, uint64, int) {
		t.Helper()
		h := newHarnessOf(t, queuedClaim(), withWaits, currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		number, stops := h.pending().Number, len(h.calls("Suspend"))
		h.deps.PluginContract, h.m.deps.PluginContract = testTakeContract+1, testTakeContract+1
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
	t.Run("a report slower than the stop timeout, inside the producer's schedule", func(t *testing.T) {
		h, number, stops := staleOverAReportToCome(t)
		h.advance(5*time.Minute + 30*time.Second)
		h.wantStillAwaitingItsReport(number, stops)

		h.mustReport(nil)

		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		h.wantFinishedOnce(finishedByReport)
		h.wantTakenNoMore(number, relaunchedToReady)
	})
	t.Run("no report by the bound", func(t *testing.T) {
		h, number, stops := staleOverAReportToCome(t)
		h.advance(turnStartBound - time.Second)
		h.wantStillAwaitingItsReport(number, stops)

		h.advance(time.Second)

		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
		h.wantTakenBackWithItsNote(number)
		h.wantTakenOnceMore(number, relaunchedToReady)
	})
}

// heldBehindTheTake is work held over the run the daemon gave a take: the stale-plugin relaunch
// the lane holds for that run's end (holdStaleRelaunch), or the daemon's own suspension held for
// it, which names no deadline (holdSuspension). hold asks for it while the run runs; idle holds
// that it has not run; ran holds that it has, once; next brings the claim's next process up, its
// worker taking whatever value it is given.
type heldBehindTheTake struct {
	name            string
	hold, idle, ran func(h *harness)
	next            func(h *harness)
}

// daemonSuspension is the daemon's own suspension of the claim, the workflow's linger or close,
// which names no deadline (RequestSuspend.Deadline).
func daemonSuspension(h *harness) RequestSuspend {
	return RequestSuspend{Claim: h.token, By: SuspendedByDaemon, Reason: "the issue's tree lingers"}
}

var (
	staleRelaunchHeld = heldBehindTheTake{
		name: "the stale-plugin relaunch held",
		hold: func(h *harness) {
			h.t.Helper()
			h.deps.PluginContract, h.m.deps.PluginContract = testTakeContract+1, testTakeContract+1
			h.must(StalePlugin{Claim: h.token, Trigger: "sweep"})
			if h.m.staleRelaunch == nil {
				h.t.Fatal("no relaunch is held for the run given the take")
			}
		},
		idle: func(h *harness) { h.t.Helper(); h.wantCalls("Resume", 0) },
		ran: func(h *harness) {
			h.t.Helper()
			h.wantState(StateLaunching)
			h.wantCalls("Resume", 1)
		},
		next: func(h *harness) { h.t.Helper(); h.worker.manual = false; relaunchedToReady(h) },
	}
	daemonSuspensionHeld = heldBehindTheTake{
		name: "the daemon's suspension held",
		hold: func(h *harness) {
			h.t.Helper()
			if err := h.handle(daemonSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
				h.t.Fatalf("the daemon's suspension during the run given the take answered %v, want it held", err)
			}
		},
		idle: func(h *harness) { h.t.Helper(); h.wantCalls("Suspend", 0) },
		ran: func(h *harness) {
			h.t.Helper()
			h.wantState(StateSuspended)
			h.wantCalls("Suspend", 1)
		},
		next: func(h *harness) { h.t.Helper(); h.worker.manual = false; resumedByTheOperator(h) },
	}
)

// runGivenTheTakeUnderHeldWork is a claim whose plugin names its task's number, R5's bounds set,
// whose worker took a task while a run of the host's own, G, was starting: the daemon gives G to
// the take, held asks for its work while G runs, and G ends with its report still to come. It
// answers the task's number and the sequence the producer numbers G with; the take's own turn, T,
// is the one after it.
func runGivenTheTakeUnderHeldWork(t *testing.T, held heldBehindTheTake) (h *harness, number, g uint64) {
	t.Helper()
	h = waitsHarness(t, currentPlugin)
	h.manualTakes()
	h.reach(StateReady)
	number = h.takeOf("implement the plan")
	h.must(StreamTurnStart{Claim: h.token})
	if p := h.pending(); !p.startRecorded() {
		t.Fatalf("the run that started after the take is %+v, want it given to the take", p)
	}
	held.hold(h)
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	held.idle(h)
	return h, number, h.worker.sequence + 1
}

// wantTakesTurnStanding holds that the take of task number still stands with its turn started:
// nothing took it back, as a cut turn or otherwise.
func (h *harness) wantTakesTurnStanding(number uint64, when string) {
	h.t.Helper()
	if p := h.pending(); p.Number != number || !p.startRecorded() || p.Interrupted {
		h.t.Fatalf("%s the claim holds %+v, want task %d's take standing with its turn started", when, p, number)
	}
}

// Work held over a run of the host's own that the daemon gave a take, G — the stale-plugin
// relaunch a contract raise holds for G's end, or the daemon's suspension held for it — runs once
// the take's own turn, T, is settled by its report, in each order G's report, T's start and T's
// report can reach the daemon (decision 8 and rule two of T3 of dispatch://INFRA-2811). pi-legion
// starts the held text as T at G's agent_end, before it sends G's report, so the start frame and
// the reports travel apart:
//
//   - G's report first, ahead of T's start: the take awaits its turn again, and the held work
//     waits for that turn (putBack), so T's start is the take's turn, and T's report finishes it;
//   - T's start first: G's report moves the take's turn to T, which runs, and nothing bounds the
//     wait while it runs: the bound of the wait for the report (R5's second bound) starts only at
//     T's end, and T's report finishes the take;
//   - T's report first: it finishes the take, the held work runs then, and G's report, naming no
//     task, finds no take.
//
// Each runs the task once: one finish, by T's report, and nothing of it taken by the next process.
//
// The negative control is 7ade1c0c, where the stale-plugin relaunch's wait for the report, armed
// at G's end for the stop timeout, takes back as a cut turn the take whose turn T is running, in
// the first two orders, and the relaunched worker runs the task again.
func TestHeldWorkOverARunGivenTheTakeRunsAfterTheTakesOwnTurn(t *testing.T) {
	for _, held := range []heldBehindTheTake{staleRelaunchHeld, daemonSuspensionHeld} {
		t.Run(held.name+", G's report first", func(t *testing.T) {
			h, number, g := runGivenTheTakeUnderHeldWork(t, held)
			h.reportOf(g, 0)
			if p := h.pending(); p.Number != number || !p.awaitsStart() {
				t.Fatalf("after G's report the claim holds %+v, want task %d's take awaiting its turn again", p, number)
			}
			held.idle(h)

			h.must(StreamTurnStart{Claim: h.token})
			h.wantTakesTurnStanding(number, "once T started")
			h.advance(2 * turnStartBound)
			h.wantState(StateWorking)
			h.wantTakesTurnStanding(number, "two second bounds into T")
			held.idle(h)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			held.idle(h)
			h.reportOf(g+1, number)

			held.ran(h)
			h.wantFinishedOnce(finishedByReport)
			h.wantTakenNoMore(number, held.next)
		})
		t.Run(held.name+", T's start first", func(t *testing.T) {
			h, number, g := runGivenTheTakeUnderHeldWork(t, held)
			h.must(StreamTurnStart{Claim: h.token})
			h.reportOf(g, 0)
			h.wantTakesTurnStanding(number, "after G's report, T running,")

			h.advance(2 * turnStartBound)
			h.wantState(StateWorking)
			h.wantTakesTurnStanding(number, "two second bounds into T")
			held.idle(h)
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.advance(turnStartBound - time.Second)
			h.wantState(StateIdle)
			h.wantTakesTurnStanding(number, "a second bound less a second after T's end")
			held.idle(h)
			h.reportOf(g+1, number)

			held.ran(h)
			h.wantFinishedOnce(finishedByReport)
			h.wantTakenNoMore(number, held.next)
		})
		t.Run(held.name+", T's report first", func(t *testing.T) {
			h, number, g := runGivenTheTakeUnderHeldWork(t, held)
			generation := h.generation()
			h.must(StreamTurnStart{Claim: h.token})
			h.mustQuiet(StreamTurnEnd{Claim: h.token})
			h.reportOf(g+1, number)
			held.ran(h)
			h.wantFinishedOnce(finishedByReport)

			var refused *RefusedError
			if err := h.m.Handle(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: generation, Sequence: g}); err != nil && !errors.As(err, &refused) {
				t.Fatalf("G's report after T's = %v, want it applied to nothing", err)
			}
			h.m.Wait()

			held.ran(h)
			h.wantFinishedOnce(finishedByReport)
			h.wantTakenNoMore(number, held.next)
		})
	}
}

// A report that gives a take its turn back while work is held for that take's report — the
// daemon's suspension, which names no deadline, or the stale-plugin relaunch — leaves that work
// waiting for the take's own turn or for R5's second bound, armed from the put-back, whichever
// comes first (decision 8 of dispatch://INFRA-2811): a suspension asked again meanwhile joins the
// hold, and with no turn started by the bound the worker is charged, the take is taken back as R5
// says, and the held work runs then: the suspension lands, or the relaunch runs, and the next
// process takes the task once, with no note of a cut turn, since no turn of it ran.
//
// The negative control is 7ade1c0c, where the put-back leaves the suspension waiting for a report
// no longer owed (heldForReport), and both holds keep R5's second wait from arming (claimHold): no
// bound ever ends them, and the claim stays idle and held.
func TestAPutBackLeavesHeldWorkToTheTakesTurnOrTheSecondBound(t *testing.T) {
	for _, held := range []heldBehindTheTake{daemonSuspensionHeld, staleRelaunchHeld} {
		t.Run(held.name, func(t *testing.T) {
			h, number, g := runGivenTheTakeUnderHeldWork(t, held)
			h.reportOf(g, 0)
			if p := h.pending(); p.Number != number || !p.awaitsStart() {
				t.Fatalf("after the report of the run given the take the claim holds %+v, want task %d's take awaiting its turn again", p, number)
			}
			if held.name == daemonSuspensionHeld.name {
				if err := h.handle(daemonSuspension(h)); !errors.Is(err, ErrSuspendHeld) {
					t.Fatalf("the daemon's suspension asked again after the put-back answered %v, want it joining the hold", err)
				}
			}

			h.advance(turnStartBound - time.Second)
			h.wantState(StateIdle)
			h.wantCharges()
			held.idle(h)
			if p := h.pending(); p.Number != number || !p.awaitsStart() {
				t.Fatalf("a second bound less a second after the put-back the claim holds %+v, want task %d's take still awaiting its turn", p, number)
			}

			h.advance(time.Second)

			h.wantCharges(WaitTurnStart)
			held.ran(h)
			if p := h.pending(); p.Number != number || p.taken() || p.Interrupted {
				t.Fatalf("after the second bound the claim holds %+v, want task %d taken back with no note of a cut turn", p, number)
			}
			sent := len(h.prompts())
			held.next(h)
			var again []taken
			for _, took := range h.prompts()[sent:] {
				if took.Number == number {
					again = append(again, took)
				}
			}
			if len(again) != 1 {
				t.Fatalf("the next process took task %d %d times (%+v), want once", number, len(again), again)
			}
		})
	}
}

// A wake whose turn comes while a held suspension waits on a take is spent into the hold where no
// turn runs (wakeNow, reportHolds): over a take a report put back, the hold keeps the process for
// the take's own turn, whose start may be on its way, and stops it, owing the wake's launch, only
// at R5's second bound. Where the take's own turn runs — moved to the run after the one the daemon
// gave it — the wake ends the hold as it ends every hold over a running turn (endHeld): the turn
// is cut off, and the task taken back with the note of a cut turn (rule one of T3 of
// dispatch://INFRA-2811: a wake's turn that cuts a turn mid-run takes its task back with its note).
// The wake is a replacement for a stale address, which the pacing parked over the live process
// before the take's run started (repoint, reserveWake).
//
// The negative control is 7ade1c0c: after the put-back no bound ever stops the claim, since the
// hold keeps R5's second wait from arming (claimHold); while the take's turn runs, the hold's mark
// left by the run given the take (heldForReport) spends the wake into the hold.
func TestAWakeWhileHeldWorkWaitsOnTheTakeIsSpentIntoTheHoldOutsideATurn(t *testing.T) {
	parkedUnderAHold := func(t *testing.T) (h *harness, number, g uint64, due time.Time) {
		t.Helper()
		h = pacedHarness(t, wakeRate, currentPlugin, withWaits)
		h.manualTakes()
		h.reach(StateReady)
		number = h.takeOf("implement the plan")
		h.observe(runtime.StaleAddress)
		due = h.wantWaitingWake(WakeLaunch).At
		h.must(StreamTurnStart{Claim: h.token})
		if p := h.pending(); !p.startRecorded() {
			t.Fatalf("the run that started in the wait is %+v, want it given to the take", p)
		}
		daemonSuspensionHeld.hold(h)
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		return h, number, h.worker.sequence + 1, due
	}
	t.Run("after a put-back", func(t *testing.T) {
		h, number, g, due := parkedUnderAHold(t)
		h.reportOf(g, 0)

		h.advance(due.Sub(h.clock.Now()))

		h.wantState(StateIdle)
		h.wantCalls("Suspend", 0)
		h.wantCalls("Resume", 0)
		if p := h.pending(); p.Number != number || !p.awaitsStart() {
			t.Fatalf("after the wake's turn the claim holds %+v, want task %d's take awaiting its turn", p, number)
		}
		h.advance(turnStartBound)
		h.wantCharges(WaitTurnStart)
		h.wantState(StateSuspended)
		h.wantCalls("Suspend", 1)
		h.wantCalls("Resume", 0)
	})
	t.Run("while the take's own turn runs", func(t *testing.T) {
		h, number, g, due := parkedUnderAHold(t)
		h.must(StreamTurnStart{Claim: h.token})
		h.reportOf(g, 0)
		h.wantTakesTurnStanding(number, "after the report of the run given the take, its own turn running,")

		h.advance(due.Sub(h.clock.Now()))

		h.wantState(StateSuspended)
		h.wantCalls("Suspend", 1)
		h.wantCalls("Resume", 0)
		if p := h.pending(); p.Number != number || p.taken() || !p.Interrupted {
			t.Fatalf("after the wake's turn the claim holds %+v, want task %d taken back with the note of a cut turn", p, number)
		}
	})
}

// A suspension that may not be held (RequestSuspend.NeverHold) is refused with the sentence of
// what the hold would have waited for, and the phase of the wait for the take's report decides it
// before what is held does (holdSuspension, reportWaitNow): the turn running, the compaction, the
// take a report put back, the report come and its finish refused by the store, or the report still
// to come, each with the work that waits behind it. Each refusal records nothing.
//
// The negative control is the switch that asks whether a suspension is held before it asks the
// phase (holdSuspension): a held suspension behind a put-back take is told it waits for the
// report, a held suspension behind a refused finish is told it waits for the report that came,
// and a relaunch behind a refused finish is told the report is still to come.
func TestASuspensionThatMayNotBeHeldNamesWhatTheHoldWouldWaitFor(t *testing.T) {
	reportsRefusedFinish := func(h *harness) {
		h.t.Helper()
		h.store.fail("RetireDelivery", errBoom)
		if err := h.reportTurn(h.pending().Number, nil); err == nil {
			h.t.Fatal("the report whose finish the store refused answered no failure")
		}
	}
	staleOverAReportToCome := func(t *testing.T) *harness {
		t.Helper()
		h := newHarnessOf(t, queuedClaim(), withWaits, currentPlugin)
		h.reach(StateReady)
		h.takesTurnEndsWithItsReportToCome()
		staleRelaunchHeld.hold(h)
		return h
	}
	putBack := func(held heldBehindTheTake) func(t *testing.T) *harness {
		return func(t *testing.T) *harness {
			t.Helper()
			h, number, g := runGivenTheTakeUnderHeldWork(t, held)
			h.reportOf(g, 0)
			if p := h.pending(); p.Number != number || !p.awaitsStart() {
				t.Fatalf("after the report of the run given the take the claim holds %+v, want task %d's take awaiting its turn again", p, number)
			}
			return h
		}
	}
	for _, tc := range []struct {
		name  string
		reach func(t *testing.T) *harness
		want  string
	}{
		{"the take's turn running", func(t *testing.T) *harness {
			h := newHarnessOf(t, queuedClaim(), currentPlugin)
			h.reach(StateReady)
			h.taskTheAgentTook()
			h.must(StreamTurnStart{Claim: h.token})
			return h
		}, "the claim's agent is in a turn, and this suspension is not one that is held for the turn's end"},
		{"a compaction with no turn under it", func(t *testing.T) *harness {
			h, _, _ := reportToComeOfACurrentPlugin(t)
			release, compacted := h.compactHeld()
			t.Cleanup(func() { release(); <-compacted })
			return h
		}, "the claim's session is being compacted, and this suspension is not one that is held for the compaction's end"},
		{"nothing held, the report still to come", func(t *testing.T) *harness {
			h, _, _ := reportToComeOfACurrentPlugin(t)
			return h
		}, "the turn of the claim's task ended with its report still to come, and this suspension is not one that is held for that report"},
		{"a held suspension, the report still to come", func(t *testing.T) *harness {
			h, _, _ := heldOverATakesTurn(t)
			return h
		}, "a held suspension waits for the report of the claim's last turn, and this suspension is not one that is held for it"},
		{"a held suspension behind a put-back take", putBack(daemonSuspensionHeld),
			"the claim's take awaits its turn again with a held suspension waiting behind it, and this suspension is not one that is held for that turn"},
		{"a relaunch behind a put-back take", putBack(staleRelaunchHeld),
			"the claim's take awaits its turn again with a relaunch waiting behind it, and this suspension is not one that is held for that turn"},
		{"a held suspension behind a finish the store refused", func(t *testing.T) *harness {
			h, _, _ := heldOverATakesTurn(t)
			reportsRefusedFinish(h)
			return h
		}, "a held suspension waits for the finish of the report of the claim's last turn, which the store refused, and this suspension is not one that is held for that finish"},
		{"a relaunch behind a finish the store refused", func(t *testing.T) *harness {
			h := staleOverAReportToCome(t)
			reportsRefusedFinish(h)
			return h
		}, "a relaunch waits for the finish of the report of the claim's last turn, which the store refused, and this suspension is not one that is held for that finish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.reach(t)
			before := h.claim()

			err := h.handle(RequestSuspend{Claim: h.token, Reason: "a fresh session was asked for", By: SuspendedByOperator, NeverHold: true})

			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("the suspension that may not be held answered %v, want a *RefusedError", err)
			}
			if refused.Reason != tc.want {
				t.Fatalf("the refusal says %q, want %q", refused.Reason, tc.want)
			}
			if after := h.claim(); after.State != before.State || after.SuspensionHeld != before.SuspensionHeld || after.SuspensionHeldBy != before.SuspensionHeldBy {
				t.Fatalf("the refused suspension left the claim %s holding %v by %q, want it as it was: %s holding %v by %q",
					after.State, after.SuspensionHeld, after.SuspensionHeldBy, before.State, before.SuspensionHeld, before.SuspensionHeldBy)
			}
		})
	}
}
