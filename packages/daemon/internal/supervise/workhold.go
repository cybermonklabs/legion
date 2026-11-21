package supervise

// The work hold (`gates.hold: work`) as one claim's machine keeps it. Its stop is a suspension like
// any other (RequestSuspend, By SuspendedByHold): held for the agent's turn and cut at a deadline
// the outbox stores (holdSuspension, suspendAtTimeout), and for a ready or idle claim held for the
// turn its close message starts (RequestSuspend.TurnAfter). What is the hold's own is here: the
// one check that holds every request that could start a turn of a claim the hold stopped
// (workHoldWaits, read by the fence through each event's door), and the hold's end, which drops a
// stop not yet made and starts a claim the hold stopped with one task (EndHold, DropHoldStop).

import (
	"context"
	"fmt"
)

// workHoldWaits is the work hold's answer to ev, and nil where it has none: a claim the hold
// suspended answers every request that could start or resume a turn of its agent a wait
// (ErrHeldByWorkHold) — a start's resume by rule, a task, a wake, a CI settlement's or a hook's
// task, the boot's relaunch, a pause's resume and the resume of a launch it owed, the controller
// keeper's resume or retry, a compaction, a ready — except the operator's own resume and retry,
// which start it (turnDoor). Each request answers it before its row is reached, so no row of the
// table has to know the hold, and a request added later is held by the door it declares. The
// claim is left exactly as it was: the task it kept stays kept, and the turn-starting request is
// asked again by whoever holds it (the outbox's backoff) or is a request the hold's end makes
// moot, since that end starts the claim itself (EndHold).
//
// A claim the hold's stop is only held over — in a turn, a compaction, idle waiting for the turn
// its message starts, or a launch held until its ready — is not answered here: its stop is held
// (Machine.held), so nothing is sent to it (sendPending), and its row runs the stop where the
// stop is due. A start of such a claim waits all the same (StartedBy).
func (m *Machine) workHoldWaits(ev Event) error {
	if ev.door() != ruleDoor || !m.claim.WorkHeld() {
		return nil
	}
	name, ok := requestName(ev)
	if !ok {
		name = string(kindOf(ev))
	}
	m.log.Info("supervise: the work hold holds the claim; the request waits for the hold's end", "request", name)
	return &RefusedError{State: m.claim.State, Request: name, Err: ErrHeldByWorkHold}
}

// EndHold is the work hold's end for this claim, and the one door the hold's opening takes into
// the claim; it is no event of the table, as StartedBy is none, because no request of a rule may
// lift the hold. The outbox calls it once the claim's tree no longer holds work, before the start
// it is running against a claim the hold holds goes on as an ordinary start.
//
//   - A stop the hold asked for and has not made — held for a turn, a compaction, a launch's ready,
//     or an idle claim's next turn — is not made: it is dropped, and a task it held back is sent
//     where it may go now (dropHoldStop). EndHold reports false: the claim runs on, and nothing
//     of task is used.
//   - A claim the hold stopped is started again, paced as any start by rule is, with one task,
//     never two: task, the opening start's, whose text starts with the opening's sentences
//     (opening). Where the claim kept a task of its own that no turn has taken — a released
//     worker's pending task, a task the hold's stop held back, a task its cut turn was working —
//     and task carries no newer work than it, the one task is that kept task's work behind the
//     opening's sentences instead (openingTask), and the kept delivery is retired in its place.
//     The one task is queued under task's id and row, so the start's own delivery of task, made
//     once this returns, queues nothing more (given). EndHold reports true. With task nil the
//     claim is started with whatever it kept.
//   - Any other claim — one an operator's suspension or the daemon's own took over, one the hold
//     never reached — is left as it is, and EndHold reports false.
//
// A claim the hold stopped that records a session it does not hold is refused its start before
// anything of it changes (resumeRefusal), as a resume, a resume by rule and a delivery that
// resumes are: a RefusedError naming the forget-session to run, the claim left stopped by the hold
// with the task it kept, and nothing launched or charged, where a relaunch would be refused at
// every try until the claim failed. The outbox asks again on its backoff.
//
// A start the lifecycle refuses (Store.AdmitClaim, a reserved cleanup) leaves the claim suspended
// by the hold with the one task queued, and the outbox asks again: the task is not composed a
// second time, since its row is already the claim's (given). Nor is a task queued that the claim
// was given already, and the task it kept is then kept as it is.
func (m *Machine) EndHold(ctx context.Context, opening string, task *RequestDeliver) (bool, error) {
	m.mu.Lock()
	defer m.unlock()
	if dropped, err := m.dropHoldStop(ctx); dropped || err != nil {
		return false, err
	}
	if !m.claim.WorkHeld() {
		return false, nil
	}
	if err := m.resumeRefusal(ctx, "work hold's end"); err != nil {
		return false, err
	}
	if task != nil {
		if one, kept := m.openingTask(opening, *task); !m.given(one) {
			if p := m.claim.Pending; p != nil {
				m.log.Info("supervise: the work hold's opening replaces the task the claim kept with its one task",
					"kept", p.ID, "keptWork", kept, "delivery", one.ID)
				if err := m.dropPending(ctx); err != nil {
					return false, fmt.Errorf("retire the task %s kept before the work hold's opening: %w", m.claim.Token, err)
				}
			}
			if err := m.queue(ctx, one); err != nil {
				return false, fmt.Errorf("queue the work hold's opening task of %s: %w", m.claim.Token, err)
			}
		}
	}
	m.log.Info("supervise: the work hold ended; the claim it stopped starts again", "delivery", pendingID(m.claim.Pending))
	if err := m.revive(ctx, false, nil); err != nil {
		return false, err
	}
	return true, nil
}

// DropHoldStop is the work hold's end for a claim whose stop row meets a tree that no longer
// holds work (the outbox's holdStopUnmade), decided under the claim's lock in one call. A stop the
// hold asked for and has not made is dropped, as EndHold drops it, and dropped is true. Nothing is
// started: a claim the hold has already stopped is left stopped, and stopped is true, so the
// caller has the hold's opening start it with the opening's sentences. Any other claim is left as
// it is, and both are false.
func (m *Machine) DropHoldStop(ctx context.Context) (dropped, stopped bool, err error) {
	m.mu.Lock()
	defer m.unlock()
	if dropped, err := m.dropHoldStop(ctx); dropped || err != nil {
		return dropped, false, err
	}
	return false, m.claim.WorkHeld(), nil
}

// dropHoldStop drops a stop the work hold asked for and has not made — held for a turn, a
// compaction, a launch's ready, or an idle claim's next turn — and sends a task it held back where
// it may go now. It reports whether there was one. A project pause's hold whose pause has ended is
// dropped first, as at the start of every decision (dropEndedPauseHold).
func (m *Machine) dropHoldStop(ctx context.Context) (bool, error) {
	m.dropEndedPauseHold()
	if m.held == nil || m.held.By != SuspendedByHold {
		return false, nil
	}
	m.log.Info("supervise: the work hold ended before its stop was made; the stop is not made",
		"reason", m.held.Reason, "deadline", m.held.Deadline, "launch", m.heldLaunch)
	m.dropHeld()
	return true, m.sendPending(ctx)
}

// OpeningTask is the one task the work hold's opening gives a claim it stopped: opening, the
// opening's sentences, a blank line, and work. A start that carries the opening's sentences apart
// (record.SuperviseRequest.Opening) carries a task that begins with OpeningTask(opening, ""), and
// the record refuses any other.
func OpeningTask(opening, work string) string { return opening + "\n\n" + work }

// openingTask is the one task the hold's opening gives a claim it stopped (EndHold): task, the
// opening start's own, unless the claim kept a task of its own that no turn has taken and task
// carries no newer work than it. Then it is the kept task's work behind the opening's sentences:
// its phase, run, entry, kept decisions, check look and review round, under task's id and row, a
// resend of its entry (RequestDeliver.ResendEntry), so the row decides it and the entry the claim
// was given stands. kept says which of the two it is. The daemon's own delivery of a relaunch's
// record is no work, and is never kept: its sentence goes in front of whatever task follows it
// (Delivery.message).
//
// Newer work is a task of a later run than the kept one's, or of a later entry of the same run,
// that is no resend: a transition or an owed entry written for the role while the gate was closed,
// whose start the opening's folded into, so the role is given the newest entry's task. A start of
// no entry, the phase's resume task the boot builds, carries none.
func (m *Machine) openingTask(opening string, task RequestDeliver) (one RequestDeliver, kept bool) {
	p := m.claim.Pending
	if p == nil || !p.ConfirmedAt.IsZero() || p.Relaunch() {
		return task, false
	}
	if task.Entry > 0 && !task.ResendEntry && (task.Generation > p.Generation || (task.Generation == p.Generation && task.Entry > p.Entry)) {
		return task, false
	}
	one = task
	one.Task = p.Task
	if opening != "" {
		one.Task = OpeningTask(opening, p.Task)
	}
	one.Phase, one.Generation, one.Entry, one.ResendEntry = p.Phase, p.Generation, p.Entry, true
	one.Kept, one.CheckLook, one.RoundNamed = p.Kept, p.CheckLook, p.RoundNamed
	return one, true
}
