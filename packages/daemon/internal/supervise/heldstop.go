package supervise

import (
	"context"
	"fmt"
	"time"
)

// The held stop (rule two of T3 of dispatch://INFRA-2811's plan; the architect's ruling
// dispatch://INFRA-2811/comment/11ddec8c-9099-4b8c-b040-acdfcfcf0638). The stop of one agent, the
// operator's or the controller's (stop, RequestStop.WaitForReport), that reaches it after its
// take's turn ended and before that turn's report reached the daemon waits for the report, as a
// suspension does (holdForReport): the process ran the turn whole and is alive, and its report
// travels over HTTP apart from the worker stream, so it can still land. Stopped at once, the
// process would take the report with it, settle would take back as a cut turn a task whose turn
// ran whole, and a retry of the stopped claim's session — each retry endStopped's comment names —
// would run it again.
//
// The hold is the held repoint's (holdReplacement), read in the same places and with the same
// bound (boundedReportHolds): it begins only once the take's turn is over, so a stop during a
// running turn cuts that turn off as before; its bound is the report wait's own (TimerReport) over
// a whole view and the reconnect deadline over a view not whole; it counts as held work (workHeld),
// so the report wait's bound arms over a take a daemon start restored, too; and letGo drops it with
// the process. It runs at the task's finish (afterTurn), or at the bound once the task is taken
// back with the note of a cut turn: the wait's own bound through afterTurn too (reportBoundOver),
// the reconnect deadline in stopAtDeadline. It is the asker's own stop that runs there, under the
// stop's own cause and charged nothing (end, endStopped), never the no-hello path's retirement,
// which charges CauseNeverReconnected. It is a mark of its own (Machine.heldStop) rather than one
// more held replacement because it ends the claim rather than replace its process: it runs before
// what else waits for the report and drops it with the process. The controller's held stop yields
// to the operator's suspension held with it (dropOutrankedStop).
//
// The stop route answers a held stop as it answers a held suspension, 202 with the claim as it is
// (api's claimEvent and agentEventAction, which read ErrSuspendHeld), and `legion claims stop` then
// waits for the stop as `legion claims suspend` waits for a held suspension.

// heldStopRefusedLine is the line of a held stop whose release the runtime refused where the wait
// for the report ended (runHeldStop), at Error, with the task's number and the error: the claim
// keeps its process, and nobody is left to ask the stop again but whoever asked it, whose stop
// route answered 202 long before.
const heldStopRefusedLine = "supervise: the stop held for the report of the take's turn could not release the claim's process; the claim keeps it, and the stop must be asked again"

// holdStop holds the stop of one agent that by asked for the report of the take's turn (stop,
// boundedReportHolds) and answers ErrSuspendHeld, which the stop route answers 202 with the claim
// as it is. The line is written where the stop begins to wait, by the take's phase (waitsLine),
// naming the task, the bound and when it comes (heldStopBound). A stop asked again while one is
// held is the same hold, answered the same way, and writes no line; the operator's stop asked over
// the controller's held one takes it over, as nothing outranks the operator.
func (m *Machine) holdStop(by SuspendSource) error {
	if !m.heldStop {
		m.heldStop, m.heldStopBy = true, by
		bound, due := m.heldStopBound()
		m.log.Info(m.takePhase().waitsLine(), "task", pendingNumber(m.claim.Pending), "held", "stop", "by", by, "bound", bound, "due", due)
	} else if by == SuspendedByOperator {
		m.heldStopBy = by
	}
	return fmt.Errorf("the stop of %s waits for the report of its take's turn: %w", m.claim.Token, ErrSuspendHeld)
}

// dropOutrankedStop drops the controller's stop held for the report of the take's turn once the
// operator's own suspension is held with it (holdSuspension): the controller may not get past that
// suspension (Claim.HeldAgainst), and its stop, run at the report, would end the process under it
// and retire the agent for a retry to relaunch. The held suspension runs at the report in its place
// (settleThenAfterTurn), as the operator's suspension takes over a held one of the controller's.
func (m *Machine) dropOutrankedStop() {
	if !m.heldStop || m.current().HeldAgainst(m.heldStopBy) == nil {
		return
	}
	m.heldStop, m.heldStopBy = false, ""
	m.log.Info("supervise: the operator's suspension takes the agent over from the controller's stop held for the report of the take's turn; the suspension runs in its place",
		"task", pendingNumber(m.claim.Pending))
}

// heldStopBound is the bound the held stop waits under, named as its line names it, and when it
// comes (holdStop): over a view not whole the reconnect deadline, which boundedReportHolds requires
// to stand; over a whole view the report wait's own bound, armed whole from the end of this
// decision where it is not armed yet (armReportWait), or, over a take awaiting its turn again or a
// report whose finish the store refused, which work already waiting goes on waiting on (reportHolds),
// R5's second bound or the end-frame bound.
func (m *Machine) heldStopBound() (string, time.Time) {
	kind, name := TimerReport, "the report wait's bound"
	switch {
	case m.connSeen != viewWhole:
		kind, name = TimerRegistration, "the reconnect deadline"
	case m.takePhase() == waitTake:
		kind, name = TimerTurnStart, "the second bound"
	case m.takePhase() == waitFinish:
		kind, name = TimerFinish, "the end-frame bound"
	}
	if a, armed := m.timers[kind]; armed {
		return name, a.due
	}
	return name, m.deps.Clock.Now().Add(m.deps.Timeouts.TurnStart)
}

// runHeldStop runs the held stop where the wait for the report ended, once the decision ending it
// has settled the take (afterTurn): at the report's finish, or at the wait's bound once the task is
// taken back with the note of a cut turn. It is the asker's own end of the claim (end,
// endStopped), charged nothing, and its let-go drops whatever else waited on the process. A release
// the runtime refuses changes nothing of the claim, as the stop at once leaves it; the hold is gone,
// since the wait it was held in is over, and the line says the stop must be asked again.
func (m *Machine) runHeldStop(ctx context.Context) error {
	m.heldStop, m.heldStopBy = false, ""
	number := pendingNumber(m.claim.Pending)
	err := m.end(ctx, endStopped)
	if err != nil && m.claim.State != StateRetired {
		m.log.Error(heldStopRefusedLine, "task", number, "error", err)
	}
	return err
}

// stopAtDeadline is the reconnect deadline of a claim whose stop of one agent waits for the report
// of its take's turn (reconnectDeadline): over a view not whole that deadline is the wait's bound,
// as for a held replacement (replacementAtDeadline). With no report by it the task is taken back
// with the note of a cut turn (reportMissedAtDeadline), and the held stop then runs, charged
// nothing (end, endStopped), rather than the no-hello path's retirement, which charges
// CauseNeverReconnected. A take-back the store refuses, and a release the runtime refuses, keep the
// stop held and the process, and the deadline is tried again at the probe interval, as the
// deadline's other refusals are.
func (m *Machine) stopAtDeadline(ctx context.Context) error {
	if err := m.reportMissedAtDeadline(ctx); err != nil {
		return err
	}
	m.heldStop = false
	err := m.end(ctx, endStopped)
	if err != nil && m.claim.State != StateRetired {
		m.heldStop = true
		m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
	}
	return err
}
