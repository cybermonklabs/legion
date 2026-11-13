package supervise

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// R5's two waits (dispatch://INFRA-2811). The daemon charges a worker only in a state it sees
// itself, and judges two waits, each with a bound of its own.
//
// The first wait is for a take of a value the worker reported seeing: the claim is ready or idle,
// no hold stands, its latest written revision is the one the worker reported seen, and no take
// came at it. When Timeouts.Take passes the worker is charged (chargePrompt, why naming WaitTake).
// A latest revision nobody reported seeing charges nobody, since what failed is the relay and
// not the worker: the value is written again under the same number once per first bound for as
// long as the claim's agent is up, and the claim is listed as unseen (ClaimView.UnseenTask). Both
// ride the one timer (TimerTake), since both are the first bound over the latest value.
//
// The second wait is for a turn to start after an accepted take. When Timeouts.TurnStart passes
// the worker is charged (why naming WaitTurnStart) and the task taken back under its number; the
// value is written again once it can be taken again (offerValue).
//
// A turn's start or end resets the count (resetPromptBudgets); a take does not, so a worker that
// takes after every charge and never starts a turn is still retired at the third. A hold that
// stands charges nobody: a hold that arises while a wait is armed disarms it, and its end arms
// it again from there (armWaits). The timers are memory only, as every timer is: a machine a
// daemon start built arms them again from the record at the worker's first act after that start
// (Machine.awaitsAct), so the time the daemon was not serving counts toward neither bound.

// The waits a charge names, as its why begins (chargePrompt).
const (
	// WaitTake is R5's first wait: a value the worker reported seeing that it asked no take of.
	WaitTake = "take"
	// WaitTurnStart is R5's second wait: an accepted take whose turn did not start.
	WaitTurnStart = "turn-start"
)

// firstWait is what the first bound waits on: the latest revision the daemon wrote of the claim's
// value, and whether nobody reported seeing it (unseen) or the worker saw it and asked no take.
type firstWait struct {
	unseen   bool
	epoch    string
	revision uint64
}

// UnseenTask is a claim's latest value that went a whole first bound with nobody reporting it
// seen, as the claim's published view carries it (ClaimView.UnseenTask): the number it names,
// and when the daemon last wrote it.
type UnseenTask struct {
	Task      uint64
	WrittenAt time.Time
}

// UnseenTaskValue is one claim whose value is unseen, as the state route lists it
// (api.State.UnseenTaskValues): the claim, its issue and role, and its UnseenTask.
type UnseenTaskValue struct {
	Claim     claim.Token
	Issue     string
	Role      claim.Role
	Task      uint64
	WrittenAt time.Time
}

// actOf is whether ev is an act of the claim's worker: what a daemon start's waits wait for
// before they are armed (Machine.awaitsAct). The shim's hello is one, the reconnect hello
// included, so a worker that does nothing else after a daemon start is still charged one bound
// after its reconnect; so are its registration, its ready, a seen report, a take and a turn frame.
func actOf(ev Event) bool {
	switch ev.(type) {
	case StreamHello, StreamTurnStart, StreamTurnEnd, RequestRegister, RequestReady, RequestTaskSeen, RequestTake:
		return true
	}
	return false
}

// resetPromptBudgets puts the claim's prompt budgets back to zero, the reset a turn's start or end
// makes (R5), and reports whether it changed them, for a caller whose own write would not carry
// them otherwise.
func (m *Machine) resetPromptBudgets() bool {
	b := &m.claim.Budgets
	if b.PromptFailures == 0 && b.PromptRetires == 0 {
		return false
	}
	b.PromptFailures, b.PromptRetires = 0, 0
	return true
}

// armWaits arms each of the two waits the claim is in as this decision left it, and disarms each
// it is not in: it runs at the end of every decision (unlock), so a wait is armed from the
// decision that began it — the seen report, the turn's end that left the worker idle, the hold's
// end, the accepted take, the worker's first act after a daemon start — and a decision that
// leaves the claim waiting on the same thing leaves the bound where it was. The claim is listed as
// unseen only while its first wait is still on a value nobody reported seeing.
func (m *Machine) armWaits() {
	first, waiting := m.firstWaitNow()
	if !waiting || !first.unseen {
		m.unseenListed = false
	}
	if _, armed := m.timers[TimerTake]; !waiting {
		m.disarm(TimerTake)
		m.firstFor = firstWait{}
	} else if !armed || m.firstFor != first {
		m.firstFor = first
		m.arm(TimerTake, m.deps.Timeouts.Take, m.claim.Pending.ID)
	}
	takenAt, waiting := m.secondWaitNow()
	if _, armed := m.timers[TimerTurnStart]; !waiting {
		m.disarm(TimerTurnStart)
		m.secondFor = time.Time{}
	} else if !armed || !m.secondFor.Equal(takenAt) {
		m.secondFor = takenAt
		m.arm(TimerTurnStart, m.deps.Timeouts.TurnStart, m.claim.Pending.ID)
	}
}

// firstWaitNow is the first wait the claim is in now, and false for none. Its agent is up — ready,
// idle or working — and it holds a task no take holds, whose value the daemon wrote. A latest
// revision nobody reported seeing is waited on in any of the three states, under a hold or not:
// what is waited for is the relay, which neither the claim's turn nor a hold stops. A value the
// worker saw is waited on only while it could take it: ready or idle, no take at that revision,
// and no hold the claim's own state or another role's turn puts on the task (claimHold,
// issueInTurn). The holds that ask the record — a root that has no architect to hear its task —
// are asked when the bound passes (takeBoundOver), as a take asks them.
func (m *Machine) firstWaitNow() (firstWait, bool) {
	c := &m.claim
	p := c.Pending
	if m.deps.Timeouts.Take <= 0 || m.awaitsAct || p == nil || p.taken() || c.TaskWrittenRevision == 0 {
		return firstWait{}, false
	}
	switch c.State {
	case StateReady, StateIdle, StateWorking:
	default:
		return firstWait{}, false
	}
	written := firstWait{epoch: c.TaskWrittenEpoch, revision: c.TaskWrittenRevision}
	if c.TaskSeenEpoch != c.TaskWrittenEpoch || c.TaskSeenRevision < c.TaskWrittenRevision {
		written.unseen = true
		return written, true
	}
	if c.State == StateWorking || c.TaskTakeState != "" || m.claimHold() != "" {
		return firstWait{}, false
	}
	if m.adopts(*p) {
		if _, inTurn := m.issueInTurn(); inTurn {
			return firstWait{}, false
		}
	}
	return written, true
}

// secondWaitNow is the accepted take the claim's second wait is on, and false for none: a take of
// the pending task accepted and its turn not started (Delivery.awaitsTurn), on a ready or idle
// claim no hold of its own state stands over (claimHold).
func (m *Machine) secondWaitNow() (time.Time, bool) {
	p := m.claim.Pending
	if m.deps.Timeouts.TurnStart <= 0 || m.awaitsAct || p == nil || !p.awaitsTurn() {
		return time.Time{}, false
	}
	switch m.claim.State {
	case StateReady, StateIdle:
	default:
		return time.Time{}, false
	}
	if m.claimHold() != "" {
		return time.Time{}, false
	}
	return p.ConfirmedAt, true
}

// takeBoundOver is the first bound passing (TimerTake). A value nobody reported seeing is written
// again under its number, which gives the relay a new revision to carry, and the claim is listed
// as unseen; nobody is charged. A value the worker saw and asked no take of charges the worker,
// unless what a take would meet now refuses or drops it (takeable): a hold that stands charges
// nobody, and is waited out (armWaits arms the bound again from this decision), and a task the
// record no longer owes is dropped here, as a take would drop it.
func takeBoundOver(m *Machine, ctx context.Context, _ Event) error {
	armedFor := m.firstFor
	m.firstFor = firstWait{}
	now, waiting := m.firstWaitNow()
	if !waiting || now != armedFor {
		return nil
	}
	p := m.claim.Pending
	if now.unseen {
		m.unseenListed = true
		m.log.Info("supervise: nobody reported seeing the claim's latest value within the first bound; it is written again under its number, and nobody is charged",
			"task", p.Number, "epoch", now.epoch, "revision", now.revision, "bound", m.deps.Timeouts.Take)
		return m.writeValue(ctx, "unseen")
	}
	hold, dropped, err := m.takeable(ctx)
	switch {
	case err != nil || dropped:
		return err
	case hold != "":
		m.log.Info("supervise: the first bound passed while a hold stands over the task; the worker is not charged",
			"task", p.Number, "hold", hold)
		return nil
	}
	return m.chargePrompt(ctx, fmt.Sprintf("%s: the worker reported seeing task %d at revision %d and asked no take of it within %s",
		WaitTake, p.Number, now.revision, m.deps.Timeouts.Take))
}

// turnStartBoundOver is the second bound passing (TimerTurnStart): the take of the pending task
// was accepted and no turn started. The worker is charged, and the task is taken back under its
// number, so the next take of it is decided afresh; where the charge left the process in place,
// the value is written again (offerValue), since the worker asked at the revision it holds. A
// charge that retired the process leaves the value to the relaunched worker, whose registration
// reads it.
func turnStartBoundOver(m *Machine, ctx context.Context, _ Event) error {
	armedFor := m.secondFor
	m.secondFor = time.Time{}
	takenAt, waiting := m.secondWaitNow()
	if !waiting || !takenAt.Equal(armedFor) {
		return nil
	}
	number := m.claim.Pending.Number
	m.log.Warn("supervise: no turn started within the second bound after the accepted take; the worker is charged and the task taken back",
		"task", number, "takenAt", takenAt, "bound", m.deps.Timeouts.TurnStart)
	if err := m.chargePrompt(ctx, fmt.Sprintf("%s: the take of task %d was accepted and no turn started within %s",
		WaitTurnStart, number, m.deps.Timeouts.TurnStart)); err != nil {
		return err
	}
	if p := m.claim.Pending; p == nil || p.Number != number || !p.awaitsTurn() {
		return nil
	}
	if err := m.takeBack(ctx, WaitTurnStart); err != nil {
		return err
	}
	if m.claim.State != StateReady && m.claim.State != StateIdle {
		return nil
	}
	return m.offerValue(ctx, "retake")
}

// reconnectDeadline is the reconnect deadline of a claim a daemon start restored with its agent
// ready, idle or working (NewMachine): its shim has not said hello since. It probes as the
// registration deadline does (registrationDeadline). A process found gone takes the death path
// (judge, died). A process found alive is retired and relaunched with a charged launch failure,
// CauseNeverReconnected, since a live process whose shim never comes back is held by nothing
// else: no frame of it reaches the daemon, so none of R5's waits can be judged of it. The task
// its turn was running is taken back first, as a replacement takes it (interrupted), so the
// relaunched worker takes it again under its number.
//
// A suspension held for the claim's turn ends here instead: no turn's end will reach it from a
// shim that never came back. A relaunch the tree lifecycle refuses, a take-back the store would
// not write and a stop the runtime refuses each keep the process, and the deadline is tried again
// at the probe interval, as the registration deadline's are.
func reconnectDeadline(m *Machine, ctx context.Context, _ Event) error {
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	return m.probe(ctx, func(ctx context.Context) error {
		if err := m.checkLaunch(ctx); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose shim never reconnected: %w", m.claim.Token, err)
		}
		if err := m.interrupted(ctx, "the shim never reconnected after the daemon started", ordinaryEnd); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose shim never reconnected: take the task back: %w", m.claim.Token, err)
		}
		incarnation := m.claim.Locator.Incarnation
		if err := m.suspendProcess(ctx); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose shim never reconnected: suspend: %w", m.claim.Token, err)
		}
		m.log.Warn("supervise: the shim never reconnected after the daemon started; retired its process", "incarnation", incarnation)
		return m.relaunchAfterFailure(ctx, CauseNeverReconnected)
	}, TimerRegistration, m.deps.Timeouts.Probe)
}
