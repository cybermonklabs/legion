package supervise

import (
	"context"
	"fmt"
)

// The wait for a take's report (dispatch://INFRA-2811, decision 8 and rule two of T3). For a claim
// whose plugin names its task's number, only the turn-outcome report naming that number finishes
// the task (TakesNoTask). A let-go made between the take's turn and that report would take
// with it the report that settles the task, and its take-back would mark as a cut turn a task whose
// turn ran whole, to run again in the next process. So what the machine holds for the take's turn
// waits for that report: a held suspension (Machine.held), a stale-plugin relaunch
// (Machine.staleRelaunch), a replacement of the live process — a stale-address relaunch, or a
// replacement the wake pacing parked whose turn came (Machine.heldReplacement) — the operator's
// stop of the claim (Machine.heldStop), and a wake whose
// turn comes meanwhile, which is spent into the held suspension (wakeNow, Machine.heldLaunch). And
// a take whose turn ended waits for that report with nothing held, too: no other bound reaches a
// take whose turn started. This file is that one wait:
// which of its phases the take is in (takePhase) and the claim is in (reportWaitNow), when new work
// joins it (reportHolds), its bound (TimerReport), which takes it reaches (reportReaches), and its
// one take-back (reportMissed). Nothing of it is a mark of its own: it is read off the take and the
// claim, so every decision that moves the take — its turn's start and end, the move of its turn to
// a later run, a put-back, a finish, a take-back — decides the wait again with it.
//
// The wait has three phases, by where the take's turn is (waitPhase):
//
//   - The take's turn runs (inTurn): the work waits for the turn's end, as work held for any turn
//     does (holdSuspension, holdStaleRelaunch), and no bound of this wait runs. So the wait never
//     takes back a take whose own turn is running: the turn's end begins the next phase, and the
//     bound is armed whole from there (armReportWait). That holds for the take's own turn moved to
//     the run after the one the daemon gave it (notTheTakesTurn), too. A wake whose turn comes
//     meanwhile ends a held suspension as it ends every hold over a running turn (wakeNow,
//     endHeld): rule one of T3 has a wake's turn cut a turn off and take its task back with its
//     note.
//   - The take's turn has ended and its report has not come: the work waits for the report, whose
//     finish runs it (reportedTask, settleThenAfterTurn), as does the take-back a provider error's
//     report makes. The bound is R5's second bound (Timeouts.TurnStart), armed whole at the end of
//     the take's counted turn, with work held or none, and kept by every decision that leaves the
//     take waiting the same way, work that arrives later included; over a take the bound does not
//     reach with nothing held (reportReaches, below), it is armed at the end of the decision that
//     left work waiting here. Any turn's start disarms it, a turn of the host's own included, and that
//     turn's end arms it whole again. When it passes, the task is taken back with the note of a
//     cut turn (reportMissed), as for a process that dies before its report, and the work runs;
//     with nothing held, the value is written again under the task's number for the worker to
//     take, as a retake, and nothing is charged (reportBoundOver). At the default, the bound is no
//     shorter than the producer's own schedule for the report: pi-shared sends it again after every
//     attempt the daemon leaves unanswered, each attempt bounded at 5 minutes
//     (DAEMON_ANSWER_TIMEOUT_MS) and the waits between attempts backing off from 500 ms to 30 s
//     (DAEMON_RESTART_FIRST_DELAY_MS, DAEMON_RESTART_MAX_DELAY_MS), with no limit on attempts, and
//     the Claude bridge as well (claudebridge's taskResendMax, taskAnswerTimeout). The bound's
//     default, 25 minutes (decision 13 of dispatch://INFRA-2811), holds four attempts that each
//     run out their 5 minutes, with the waits between them, so a report held up by a slow store or
//     a daemon restart lands inside it.
//   - A report gave the take its turn back (putBack): the run the daemon gave it was not its own,
//     and the take awaits its turn again. The report the work waits for is now that turn's, and the
//     worker may already have started it: pi-legion starts the held text at the run's end, before
//     it sends that run's report, so the start frame can be on its way behind the report. The work
//     goes on waiting, for the take's turn to start, which begins the first phase again, or for R5's
//     second wait, armed whole from the put-back (secondWaitNow), whichever is first. That bound
//     charges the worker and takes the task back as R5 says, and the work runs then
//     (turnStartBoundOver). Run at the put-back, a relaunch would cut off a turn whose start the
//     daemon had not seen yet, and the next process would run the task again.
//
// A held suspension's own deadline (TimerSuspend) bounds it in every phase as well, whichever is
// first (suspendAtTimeout): in the first phase its stop cuts the turn off, in the second the task
// is taken back with the note of a cut turn first (reportMissed), and in the third the stop's
// let-go takes the take back (suspended, takeBackUnstarted): with no note where the claim's
// connection showed every frame of its turns, and as a cut turn where it did not, since its turn
// may have run unseen (turnMayHaveRunUnseen). Over a report whose finish the store refused
// (waitFinish), the stop's let-go, or the settle after it, finishes the task by that report
// (takeBackUnstarted, settle).
//
// Work that reaches the claim while no turn runs joins the wait in the second phase, and in the
// third where work already waits there (reportHolds): a suspension is held (suspend,
// holdForReport), a relaunch is left waiting (refreshPlugin, runWake), a replacement of the live
// process is held (repoint, runWake, holdReplacement), the operator's stop is held (stop,
// holdStop), and a wake whose turn comes
// is spent into the held suspension (wakeNow). A take that awaits its turn with nothing waiting
// behind it is no wait: a let-go takes it back, as rule one of T3 says, with no note where the
// claim's connection showed every frame of its turns, since no turn of it ran, and as a cut turn
// where it did not, since that turn may have run unseen (turnMayHaveRunUnseen). The wait is
// memory only, as what waits in it is: a restart forgets both, and their askers ask again.
//
// The bound judges the worker by the frames its connection carries, so it runs only while that
// connection shows the machine every frame of the worker's turns (connview.go).
// A close disarms it, and no bound of it fires while no connection stands.
// After every reconnect hello of a claim holding a pending task, whatever the hello says of the
// shim's backlog, it is armed again whole, never from what was left of it, only at the agent's
// answer to whether it is in a turn or at a turn start or end frame on the new connection.
// So a take whose own turn runs while the daemon cannot see its frames — the shim cut off while the
// daemon runs on, its backlog trimmed, or that turn's start lost in the socket's send buffer — is
// never taken back during that turn, and a report the producer sends again across a daemon's outage
// or a lost connection has the whole bound once the connection is whole again.
// After a daemon start every claim reconnects with a hello, so a take or a report that reaches the
// daemon before it arms nothing either. A replacement held for the report over a view not whole is
// bounded by the reconnect deadline instead, at which the task is taken back with the note of a cut
// turn and the replacement runs, uncharged (replacementAtDeadline), and a replacement that no bound
// would reach is not held (boundedReportHolds).
//
// With nothing held the bound exists for a take whose counted turn ended and whose report naming
// its number will never come, though its process lives: its text was dropped — Claude Code never
// took it, or Oh My Pi's sendUserMessage refused it (pi-shared's task-take.ts, hostDropped), and
// the producer goes back to no take — and the turn the daemon counts as the take's reported no
// task. Three orders leave such a take holding that turn, the worker seeing no newer revision to
// take: a start of the host's own that reaches the daemon before the dropped turn's report saying
// the text was held, which makes that report moot (heldUnrun); a start of the host's own after the
// put-back that report made, which is given the take (turnStarted); and, with the count known, a
// no-task report that beats its turn's start frame, which passes over a take whose turn has not
// started (notTheTakesTurn), so that start is then given the take. A turn that ran the text sends
// its report well inside the bound, so this take-back runs a task twice only where a process dies
// before its report, which every let-go takes back alike. Turns that follow one another with every
// idle gap shorter than the bound postpone it to the first idle gap of the whole bound.
//
// A take a daemon start restored from the store is reached only where this daemon sees every turn
// start since the take's counted turn: the store shows the daemon before it handled no turn end
// after the take's accept (anchorRestored), and the first hello after the start said its shim's
// backlog is whole and its agent answered on that connection (bindRestoredAnchor), which together
// are the restored take's bound anchor. Any other restored take is left out while nothing is held:
// its own turn may have started while no daemon served it, its start frame lost, and the bound
// would fire during that turn, which would run the task twice. It runs when the worker hands it,
// and a text dropped in that state ends at the process's next let-go.
//
// The report can come and its finish fail on the store (Machine.finishOwed). It came, so no work
// begins to wait for it any more and no bound takes the task back, whichever turn the take awaits
// and whatever the claim's plugin, one whose end frame finishes its task included (takePhase,
// waitFinish); but work already waiting goes on waiting for that finish, which the end-frame bound
// makes again until it lands, and runs at it (finishAtTurnsEnd), as at the report's own finish.
// Every take-back finishes such a task by that report instead, since each reads this phase before
// it takes anything back: interrupted (every let-go that settles a take, a death, a replacement,
// the reconnect deadline and this wait's own take-back among them), takeBackUnstarted (a
// retirement and a suspension), takeBack (R5's second bound and a provider error that holds the
// issue) and takeToLadder (a provider error the ladder waits out).
// takeBackInMemory, which reads no phase, runs only behind the last two. The retirement of such a
// task for a newer one finishes it by that report too (retirePending).
//
// What waited runs where the wait ends, once the decision ending it has settled the take: the
// report's finish or its provider error's take-back (reportedTask), the owed finish
// (finishAtTurnsEnd), the wait's bound (reportBoundOver), R5's second bound over a take put back
// (turnStartBoundOver). Each settles the take through one helper (settleThenAfterTurn), which runs
// the operator's held stop where one is held, the held suspension's stop where that suspension
// waited and no stop is held, and the held replacement, the held relaunch and the offer otherwise
// (afterTurn). It reads whether the held suspension waited (heldWaits) before the settle, since
// the settled take says nothing of it any more: a suspension held for a turn that was no take's,
// whose stop the runtime refused at that turn's end, is retried at the probe interval
// (suspendHeld), not at a report. And it runs what waited only over a claim ready or idle with no
// compaction running: the end of a turn or a compaction runs it otherwise, and a settle that let
// the process go left nothing of it to run.

// reportWaitsLine is the line of work that begins to wait for the report of the take's turn
// (waitReport, waitsLine), at Info, with the task's number and what waits: "suspension", with the
// hold's source and its deadline (the zero time for none); "relaunch", with the pass that asked for
// it; "replacement", with what raised it ("repoint", with the process and the observation's
// detail, or "wake", with the parked wake's kind: holdReplacement); "stop", the operator's, with
// its bound and when that bound comes (holdStop); or "wake", with the wake's kind and the hold's
// source.
const reportWaitsLine = "supervise: the report of the take's turn is still to come; what is held for that turn waits for it"

// putBackWaitsLine is the line of a put-back that leaves work waiting for the take (putBack), at
// Info, with the task's number and whether a held suspension, a held stale-plugin relaunch, a held
// replacement and the operator's held stop wait ("suspension", "relaunch", "replacement", "stop"):
// the work waits for the take's own turn to start, or for R5's second bound. It is also the line
// of work that begins to wait on a take awaiting its turn again (waitTake, waitsLine), with what
// waits as reportWaitsLine names it.
const putBackWaitsLine = "supervise: the take awaits its turn again; what is held for it waits for that turn to start, or for the second bound"

// finishWaitsLine is the line of work that begins to wait on a take whose report came and whose
// finish the store refused (waitFinish, waitsLine), at Info, with the task's number and what waits
// as reportWaitsLine names it: the work waits for that finish, which the end-frame bound makes
// again until the store takes it (finishAtTurnsEnd).
const finishWaitsLine = "supervise: the report of the take's turn came and the store refused its finish; what is held for that turn waits for the finish"

// waitsLine is the line of work that begins to wait on the pending take in phase w (reportHolds,
// reportToCome), chosen by what that work waits for: the report still to come (waitReport,
// reportWaitsLine), the take's own turn again (waitTake, putBackWaitsLine), or the finish of the
// report that came (waitFinish, finishWaitsLine). Its callers write it with what waits: a held
// suspension (logSuspensionWaits), a held stale-plugin relaunch (relaunchWaitsForReport,
// endNothingHeld), a held replacement (holdReplacement), the operator's held stop (holdStop) and a
// wake spent into the held suspension (wakeNow).
func (w waitPhase) waitsLine() string {
	switch w {
	case waitTake:
		return putBackWaitsLine
	case waitFinish:
		return finishWaitsLine
	}
	return reportWaitsLine
}

// reportMissedLine is the line of the wait's take-back (reportMissed), at Warn, with the task's
// number, the bound that came first, and whether work was held: the task goes back as a cut turn,
// and what waited runs, or, with nothing held, its value is written again for the worker to take.
const reportMissedLine = "supervise: the report of the take's turn did not come by its bound; the task is taken back with the note of a cut turn, and what waited for the report runs or its value is written again"

// waitPhase is a phase of the wait for the pending take's report (the three of this file's doc), or
// none. Every reader of the wait asks which the take is in (takePhase), through which the claim is
// in (reportWaitNow) where a turn may run: whether work joins the wait (reportHolds), the wait's own
// bound (reportBounded), R5's second wait (secondWaitNow), the held suspension's deadline
// (suspendAtTimeout), the refusal of a suspension that may not be held (holdSuspension), the end of
// a turn and of a compaction (runHeldAtEnd, endNothingHeld), and every take-back (interrupted,
// takeBackUnstarted, takeBack, takeToLadder).
type waitPhase uint8

const (
	// waitNone is no turn running and no take the wait is on: none stands accepted, or the claim's
	// plugin is below TaskTakeContract, whose end frame finishes its task (TakesNoTask), and no
	// finish of the take is owed (waitFinish), so no work waits on its take past its turn's end.
	waitNone waitPhase = iota
	// waitTurn is the first phase: a turn runs (inTurn) — the take's own, one the take was passed
	// over in while it awaits its turn (heldUnrun), or one of a claim holding no take — and work
	// waits for its end, as work held for any turn does; no bound of the wait runs. A compaction
	// with no turn started under it is no turn: what meets it waits for its end (busy), and the
	// phase is the take's.
	waitTurn
	// waitReport is the second phase: the take's turn ran and is over, and the report naming its
	// number has not come.
	waitReport
	// waitFinish is the second phase's report come and its finish refused by the store
	// (Machine.finishOwed), which the end-frame bound makes again, whichever turn the take awaits
	// and whatever the claim's plugin: the report decides. No work begins to wait for it, work
	// already waiting goes on waiting for that finish, no bound of the wait nor R5's second wait
	// arms over it, and every take-back finishes the task by that report instead: retirePending,
	// interrupted, takeBackUnstarted, takeBack and takeToLadder each ask makeOwedFinish before they
	// take anything back (this file's doc).
	waitFinish
	// waitTake is the third phase: the take awaits its turn — given back the turn the daemon gave a
	// run that was not its own (putBack), or not given one yet. Work already waiting goes on
	// waiting, for the take's own turn to start or for R5's second bound; a take that awaits its
	// turn with nothing waiting behind it is no wait.
	waitTake
)

// reportWaitNow is the phase of the wait for the take's report the claim is in now: a turn running
// comes first, whatever the take, and the take's own phase otherwise (takePhase).
func (m *Machine) reportWaitNow() waitPhase {
	if m.inTurn() {
		return waitTurn
	}
	return m.takePhase()
}

// takePhase is the phase of the wait the pending take is in, read off the take alone, whether or
// not a turn runs: the one rule of where the take stands. A finish the store refused comes before
// the take's own turn and before the claim's plugin, since the report that came decides: a take
// awaiting its turn whose finish is owed is in waitFinish, not waitTake, and so is a take of a
// plugin below TaskTakeContract (TakesNoTask), which is in waitNone only with no finish owed. The
// end of a turn and of a compaction read it directly (runHeldAtEnd, endNothingHeld), since the
// claim they end is still stored working, which reportWaitNow reads as the turn.
func (m *Machine) takePhase() waitPhase {
	p := m.claim.Pending
	switch {
	case p == nil || !p.taken():
		return waitNone
	case m.finishOwed:
		return waitFinish
	case TakesNoTask(m.claim.PluginContract, m.deps.TaskTakeContract):
		return waitNone
	case p.awaitsStart():
		return waitTake
	}
	return waitReport
}

// takesTurnRan is whether the pending take's turn ran, as the one rule of where the take stands
// reads it (takePhase): its turn started, or a report naming its number came and the store refused
// its finish (waitFinish), whichever turn the take awaits, since that report decides. Every reader
// of whether the take's turn ran asks it: the serving credit a finish earns (retireTask), the
// settle of a claim no turn runs in (settle), and the end-frame bound armed again at the end of a
// turn or a compaction (runHeldAtEnd, endNothingHeld).
func (m *Machine) takesTurnRan() bool {
	p := m.claim.Pending
	return p != nil && (p.startRecorded() || m.takePhase() == waitFinish)
}

// takeAwaitsTurn is whether the pending take awaits its turn, as the one rule of where the take
// stands reads it: accepted, and its turn not run (takesTurnRan). The turn start given to a take
// (turnStarted), a newer task that waits behind a take (queue, runParkedLaunchNow) and what a
// let-go gives back of what the take told (untellUnstarted) ask it.
func (m *Machine) takeAwaitsTurn() bool {
	p := m.claim.Pending
	return p != nil && p.taken() && !m.takesTurnRan()
}

// reportToCome is whether the report that settles the take is still to come in phase w: its turn
// over (waitReport), or the take awaiting its turn, whose own turn's report is (waitTake).
func (w waitPhase) reportToCome() bool { return w == waitReport || w == waitTake }

// workHeld is whether anything waits on the machine for the claim's turn or its take's report: a
// held suspension, a held stale-plugin relaunch, a replacement held for the report
// (heldReplacement: the held repoint, or a parked replacement whose wake's turn came), or the
// operator's stop held for it (heldStop).
func (m *Machine) workHeld() bool {
	return m.held != nil || m.staleRelaunch != nil || m.heldReplacement != nil || m.heldStop
}

// reportHolds is whether work that reaches the claim now joins the wait for the take's report
// rather than run as the claim stands, and whether work already waiting on the take goes on
// waiting (reportWaitNow). In the second phase, the report still to come, all work waits for it;
// with that report come and its finish refused, or over a take that awaits its turn, only work
// already waiting does, and what arrives joins it. In the first phase work waits for the turn's end
// anyway (holdSuspension, holdStaleRelaunch), and with no take the wait is on, nothing waits.
func (m *Machine) reportHolds() bool {
	switch m.reportWaitNow() {
	case waitReport:
		return true
	case waitFinish, waitTake:
		return m.workHeld()
	}
	return false
}

// heldWaits is whether the held suspension waits behind the wait now (reportHolds): held, no turn
// running, and the take's report or that report's owed finish still to land. A decision that ends
// the wait reads it before it settles the take, and runs that suspension's stop after the settle
// (settleThenAfterTurn).
func (m *Machine) heldWaits() bool { return m.held != nil && m.reportHolds() }

// settleThenAfterTurn is a decision that settles the pending take and then runs what waited on it:
// the report's finish or its provider error's take-back (reportedTask), the end-frame bound's
// finish (finishAtTurnsEnd), the wait's bound (reportBoundOver), and R5's second bound over a take
// put back (turnStartBoundOver). settle settles the take; whether the held suspension waited
// (heldWaits) is read before it, since the settled take says nothing of it any more. A settle that
// fails runs nothing and answers its error. What waited runs only over a claim ready or idle with
// no compaction running: a turn or a compaction that runs has its end to run it (turnEnded,
// compactEnded), and a settle that let the process go — a charge that retired it, a held stop that
// landed, a provider error that failed the claim — let the held suspension and the relaunch go with
// it (letGo), and the offer's holds refuse a claim not ready or idle (claimHold).
//
// A held suspension that waited for the take this decision settled runs first and instead of the
// rest: its stop ends the process the held relaunch was for, which letGo drops with it, and leaves
// no take to offer. A stop the runtime refuses keeps it held and tries it again at the probe
// interval (suspendHeld), the claim idle meanwhile, as a refused stop at a turn's end leaves it.
// Where none waited — none is held, or one whose stop was refused before waits for no take and is
// left to that retry — what the turn held runs as at any turn's end (afterTurn).
//
// The operator's stop held for the report (heldStop) runs before a held suspension that waited with
// it, through afterTurn: it ends the claim for good, and its let-go drops the suspension with the
// process (letGo). Run the other way, the suspension's let-go would drop the stop the route
// answered 202 for, and the claim would stand suspended rather than stopped.
func (m *Machine) settleThenAfterTurn(ctx context.Context, settle func() error) error {
	heldWaited := m.heldWaits()
	if err := settle(); err != nil {
		return err
	}
	if (m.claim.State != StateReady && m.claim.State != StateIdle) || m.compacting != nil {
		return nil
	}
	if heldWaited && m.held != nil && !m.heldStop {
		return m.suspendHeld(ctx)
	}
	return m.afterTurn(ctx)
}

// heldRunsNow is whether the held suspension waits for nothing now and its stop may run: held, no
// turn or compaction running (busy), whose end it waits for, and not waiting behind the wait for
// the report of its take's turn (heldWaits). reready and reconnectDeadline run it then.
func (m *Machine) heldRunsNow() bool { return m.held != nil && !m.busy() && !m.heldWaits() }

// logSuspensionWaits writes the line of the held suspension that begins to wait on the take, with
// the hold's source and its deadline (holdForReport, and runHeldAtEnd for turnEnded and
// compactEnded), chosen by the take's phase (waitsLine): read off the take, since the end of a turn
// or a compaction runs it over a claim still stored working. It logs nothing with no suspension
// held.
func (m *Machine) logSuspensionWaits() {
	if m.held == nil {
		return
	}
	m.log.Info(m.takePhase().waitsLine(), "task", pendingNumber(m.claim.Pending), "held", "suspension", "by", m.held.By, "deadline", m.held.Deadline)
}

// reportBounded is the pending take whose report the wait's own bound runs on, and false for none:
// the wait's second phase, the take's turn over and its report not come (waitReport: a report
// whose finish the store refused came, and is no report missed); work waits for that report, or
// nothing does and the bound reaches the take all the same (reportReaches); and the bound is
// judged — R5's second bound set, and the claim's connection showing every frame of its turns
// (connview.go), as R5's own waits are (secondWaitNow). A take awaiting its turn after a put-back
// is bounded by R5's second wait instead, so no take is under the two bounds at once.
func (m *Machine) reportBounded() (*Delivery, bool) {
	if m.deps.Timeouts.TurnStart <= 0 || m.awaitsAct || m.connSeen != viewWhole || !SaidReady(m.claim.State) ||
		m.reportWaitNow() != waitReport {
		return nil, false
	}
	if !m.workHeld() && !m.reportReaches() {
		return nil, false
	}
	return m.claim.Pending, true
}

// reportReaches is whether the wait's bound reaches the pending take with nothing held: every take
// this daemon accepted, and a take a daemon start restored from the store only where this daemon
// sees every turn start since the take's counted turn — the restored take's anchor stands
// (anchorRestored: the store holds no turn end that daemon handled after the take's accept) and
// is bound to the connection of the first hello after the start (bindRestoredAnchor: that hello
// said its shim's backlog is whole, and the agent answered on it). An anchor that went since — a
// start over a running turn lost the count (turnCount.lose), or the process changed — leaves the
// take out, as one never anchored is: a turn of its own may have started unseen, and the bound
// would fire during it (reportwait.go's doc). With no pending task it reaches nothing.
func (m *Machine) reportReaches() bool {
	p := m.claim.Pending
	if p == nil {
		return false
	}
	if !m.restoredTake {
		return true
	}
	a := m.turns.anchor
	return a.restored && a.conn != 0 && a.number == p.Number && m.turns.generation == m.claim.Generation
}

// armReportWait arms the wait's bound (TimerReport) where the claim is in it (reportBounded) and
// disarms it where it is not. It runs at the end of every decision (armWaits, unlock), so the bound
// is armed whole from the decision that left the take waiting for the report of a turn that is
// over — that turn's end, or, over a take the bound reaches only with work held, the work's
// arrival — and a decision that leaves it waiting on the same take keeps the bound where it was.
// Any turn's start disarms it, the take's own turn moved to a later run and a turn of the host's
// own included, and that turn's end arms it whole again; a put-back disarms it, and R5's second
// wait bounds the take from there.
func (m *Machine) armReportWait() {
	p, waiting := m.reportBounded()
	if !waiting {
		m.disarm(TimerReport)
		return
	}
	if a, armed := m.timers[TimerReport]; !armed || a.deliveryID != p.ID {
		m.arm(TimerReport, m.deps.Timeouts.TurnStart, p.ID)
	}
}

// reportBoundOver is the wait's own bound passing (TimerReport): the report of the take's turn did
// not come within R5's second bound of the decision that left the take waiting for it. The task is
// taken back with the note of a cut turn (reportMissed), and what waited runs as the claim stands
// (settleThenAfterTurn, afterTurn): the operator's held stop, the held suspension's stop, the held
// replacement, or the stale-plugin relaunch, which then take nothing back; with nothing held, the
// offer of the task (offerPending) writes its value again under its number, as the retake it is,
// and the worker takes it again. Nothing is charged: a worker whose text was dropped did nothing
// wrong. A bound that finds a turn running, the report come or the take out of its reach does
// nothing (reportBounded), so it never takes back a take whose own turn is running; that turn's
// end arms it again. A claim being compacted runs the work at the compaction's end
// (compactEnded). A take-back the store refused is tried again at the probe interval.
func reportBoundOver(m *Machine, ctx context.Context, _ Event) error {
	p, waiting := m.reportBounded()
	if !waiting {
		return nil
	}
	return m.settleThenAfterTurn(ctx, func() error {
		if err := m.reportMissed(ctx, "the wait's bound"); err != nil {
			m.arm(TimerReport, m.deps.Timeouts.Probe, p.ID)
			return err
		}
		return nil
	})
}

// reportMissed is the wait's one take-back: the report of the take's turn did not come by the
// bound by names — the wait's own (reportBoundOver), the held suspension's deadline
// (suspendAtTimeout), or the reconnect deadline over a view not whole (reportMissedAtDeadline). So
// the task goes back under its number with the note of a cut turn, as for a
// process that dies before its report (interrupted), before what waited runs: a take-back the
// store refused stops nothing, and the caller tries it again at the probe interval. Where the
// process stays, the next write of the value is the retake it is (Machine.rewrite); a let-go
// clears that with the process. With no pending task it takes nothing back.
func (m *Machine) reportMissed(ctx context.Context, by string) error {
	p := m.claim.Pending
	if p == nil {
		return nil
	}
	m.log.Warn(reportMissedLine, "task", p.Number, "by", by, "held", m.workHeld())
	if err := m.interrupted(ctx, "the report of the task's turn did not come by "+by, ordinaryEnd); err != nil {
		return fmt.Errorf("take back the task of %s, whose report did not come: %w", m.claim.Token, err)
	}
	m.rewrite = valueRewrite{number: p.Number, cause: TaskValueRetake}
	return nil
}

// reportMissedAtDeadline is the wait's take-back at the reconnect deadline, which is the bound of
// work held for the report over a view not whole, where the wait's own bound does not run
// (reportBounded): the held replacement's (replacementAtDeadline) and the held stop's
// (stopAtDeadline). With the report still to come the task goes back with the note of a cut turn
// (reportMissed) before what waited runs, as at the wait's own bound. A take-back the store refuses
// keeps what waited held, and the deadline is tried again at the probe interval, as the deadline's
// other refusals are.
func (m *Machine) reportMissedAtDeadline(ctx context.Context) error {
	if !m.reportWaitNow().reportToCome() {
		return nil
	}
	if err := m.reportMissed(ctx, "the reconnect deadline"); err != nil {
		m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
		return err
	}
	return nil
}
