package supervise

// The work hold (`gates.hold: work`) as one claim's machine keeps it. Its stop is a suspension like
// any other (RequestSuspend, By SuspendedByHold): held for the agent's turn and cut at a deadline
// the outbox stores (holdSuspension, suspendAtTimeout), and for a ready or idle claim held for the
// turn its close message starts (RequestSuspend.TurnAfter). What is the hold's own is here: the
// one check that holds every request that could start a turn of a claim the hold stopped
// (workHoldWaits, read by the fence through each event's door), and the hold's end. Each decision
// that may make the hold's stop reads whether the tree's work hold row stands (readHoldForStop,
// Store.WorkHoldStands). The read that finds the row gone drops a stop not yet made, and answers
// the hold's stop asked now ErrWorkHoldEnded. No decision holds the row after that read, so no
// decision's runtime call holds back the opening, which waits for the row under the lock every
// fact takes. Once the stop is written, the machine reads the row again (holdReadAfterStop). The
// opening takes that row for update before it reads the tree's claims. So either the opening reads
// the stop and starts the claim, or the read after the stop finds the row gone and the daemon
// starts the claim (Deps.WorkHoldEndedUnderStop). The opening's start drops a stop not yet made
// too, and starts a claim the hold stopped with one task (EndHold). The hold's stop asked again
// after the opening reads the row as every such decision does, so the stop's outbox row reads no
// gate of its own (internal/daemon's holdStop).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// workHoldWaits is the work hold's answer to ev, and nil where it has none: a claim the hold
// suspended answers every request that could start or resume a turn of its agent a wait
// (ErrHeldByWorkHold) — a start's resume by rule, a task, a wake, a CI settlement's or a hook's
// task, the boot's relaunch, a pause's resume and the resume of a launch it owed, the controller
// keeper's resume or retry, the project's controller's resume or retry, a compaction, a ready —
// except the operator's own resume and retry, which start it (turnDoor, askedDoor). Each request
// answers it before its row is reached, so no row of the table has to know the hold, and a request
// added later is held by the door it declares. The claim is left exactly as it was: the task it
// kept stays kept, and the turn-starting request is asked again by whoever holds it (the outbox's
// backoff) or is a request the hold's end makes moot, since that end starts the claim itself
// (EndHold).
//
// The project's controller's resume or retry of a phase worker is refused by the tree's hold, not
// by the claim's source alone (workHoldRefuses): a claim the controller suspended, before the
// close or after the hold's stop, is no claim the hold stopped, and the controller starts no phase
// worker of a tree whose work hold stands (decision 16 of dispatch://INFRA-4528's plan).
//
// A claim the hold's stop is only held over — in a turn, a compaction, idle waiting for the turn
// its message starts, or a launch held until its ready — is not answered here: its stop is held
// (Machine.held), so nothing is sent to it (sendPending), and while the read of the tree's work
// hold row finds the hold standing (readHoldForStop), its row runs the stop where the stop is due.
// A start of such a claim waits all the same (StartedBy).
func (m *Machine) workHoldWaits(ctx context.Context, ev Event) error {
	if ev.door() != ruleDoor {
		return nil
	}
	name, ok := requestName(ev)
	if !ok {
		name = string(kindOf(ev))
	}
	if m.claim.WorkHeld() {
		m.log.Info("supervise: the work hold holds the claim; the request waits for the hold's end", "request", name)
		return &RefusedError{State: m.claim.State, Request: name, Err: ErrHeldByWorkHold}
	}
	if by, asked := askedSource(ev); asked {
		if err := m.workHoldRefuses(ctx, m.current(), by); err != nil {
			m.log.Info("supervise: the tree's work hold refuses the controller's start of the claim", "request", name, "error", err)
			return &RefusedError{State: m.claim.State, Request: name, Err: err}
		}
	}
	return nil
}

// askedSource is who asks ev, for a request that names its asker and can start a turn: a resume
// (RequestResume.By) and a caller's retry (RequestAskedRetry.By).
func askedSource(ev Event) (SuspendSource, bool) {
	switch ev := ev.(type) {
	case RequestResume:
		return ev.By, true
	case RequestAskedRetry:
		return ev.By, true
	}
	return "", false
}

// WorkHoldRefusal is the work hold's refusal of a start the project's controller (by) asks of this
// claim's agent, or nil: its resume, its retry, its fresh session, and the start of a phase its
// retry writes (api's agent door and restartWorkedPhase). It reads the claim as the machine last
// published it (View), so it never waits on the decision in flight, and the machine reads the same
// rule again where the request lands (workHoldWaits, forgetSession). So a refused request changes
// nothing of the claim: no session is forgotten and no start row is written.
func (m *Machine) WorkHoldRefusal(ctx context.Context, by SuspendSource) error {
	return m.workHoldRefuses(ctx, m.View().Claim, by)
}

// workHoldRefuses is the one statement of the controller's part of the work hold (decision 16 of
// dispatch://INFRA-4528's plan): the controller is no operator, so under `gates.hold: work`, while
// the standing design gate of the claim's tree is closed, it starts no phase worker of that tree.
// It is decided by the tree's hold (Store.WorkHoldStands, read under the claim-write deadline as
// readWorkHold reads it), not by the claim's source alone: a claim the controller suspended before
// the close, which the hold's stop does not take over (suspensionRank), or after it, taking the
// hold's stop over, is held as a claim the hold stopped is (WorkHeld, which needs no read). The
// answer wraps ErrHeldByWorkHold and names the two ways out, the gate's opening and an operator's
// own act. It is nil for every asker but the controller — the operator's own act overrides the
// hold — and for an architect or the controller's own claim, which the hold never holds. A read of
// the tree's hold that fails is an error the caller refuses with: a start the read did not clear
// may not run.
func (m *Machine) workHoldRefuses(ctx context.Context, c Claim, by SuspendSource) error {
	if by != SuspendedByController || !claim.IsPhaseWorker(c.Role) {
		return nil
	}
	if !c.WorkHeld() {
		reading, cancel := context.WithTimeout(ctx, m.deps.claimWrite())
		defer cancel()
		stands, err := m.deps.Store.WorkHoldStands(reading, c.Tree)
		if err != nil {
			return fmt.Errorf("read the work hold of %s for the controller's start of %s: %w", c.Tree, c.Token, err)
		}
		if !stands {
			return nil
		}
	}
	return fmt.Errorf("the work hold of %s holds every phase worker of the tree while its standing design gate is closed, and the controller starts none of them; the gate's opening, or an operator's own resume or retry, starts %s: %w",
		c.Tree, c.Token, ErrHeldByWorkHold)
}

// EndHold is the work hold's end for this claim, and the one door the hold's opening takes into
// the claim; it is no event of the table, as StartedBy is none, because no request of a rule may
// lift the hold. The outbox calls it once the claim's tree no longer holds work, before the start
// it is running against a claim the hold holds goes on as an ordinary start.
//
//   - A stop the hold asked for and has not made — held for a turn, a compaction, a launch's ready,
//     or an idle claim's next turn — is not made: it is dropped, and a task it held back is sent
//     where it may go now (dropHoldStop). EndHold reports false: the claim runs on, and nothing
//     of task is used. A decision of the claim whose read of the tree's work hold row finds the
//     row gone drops such a stop too (readHoldForStop), so EndHold finds one only where no
//     decision came between the opening and the start.
//   - A claim the hold stopped is started again, paced as any start by rule is, with one task,
//     never two: task, the opening start's, whose text starts with the opening's sentences, which
//     it carries apart too (RequestDeliver.Opening). Where the claim kept a task of a run that
//     no turn has taken — a released worker's pending task, a task the hold's stop held back, a
//     task its cut turn was working — and task carries no newer work than it (OpeningKeeps), the
//     one task is that kept task's work behind task's opening sentences instead (openingTask),
//     with the news task carries, which the start rendered over that work, and the kept delivery
//     is retired in its place. The one task is queued under task's id and row, so the start's own
//     delivery of task, made once this returns, queues nothing more (given). EndHold reports
//     true. With task nil the claim is started with whatever it kept.
//   - A claim the hold stopped that kept an operator's task that no turn has taken
//     (KeptOperatorTask) is started again with that task first. The operator's deliver route
//     queues such a task with no run, no phase and no entry (api's deliverEvent). An operator's
//     act overrides the hold, and the daemon drops no instruction without saying so. So EndHold
//     does not retire that task, does not put its work behind the opening's sentences
//     (OpeningKeeps), and queues nothing of task. The start's own delivery of task, made once
//     this returns, waits behind the operator's task by queue's rule for a task that no rule
//     replaces: while no turn has taken the operator's task, queue refuses task
//     ErrDeliveryPending (refuseBehindPending), and the start's row asks again on its backoff.
//     Once a turn has taken the operator's task, queue puts task in its place, and the claim
//     sends task when that turn ends. EndHold reports true. The outbox has a start with no
//     opening's sentences wait while the claim's own start of the opening is queued, so that
//     start ends the hold and its task, which builds the opening's sentences again at its later
//     attempt, goes second (the outbox's openingGoesFirst, waitForOpeningInFlight). Limit: where
//     no such start of the opening is queued (the stop was made after the opening read the claim,
//     or a newer stop or a newer run undid that start), a start with no opening's sentences ends
//     the hold here, and its worker is never told that the hold stopped its turn, and is told the
//     gate's open message only where its issue's holder row still owes the opening then (the
//     outbox's startNewsOf). Under `merge`, where the boot ended the hold, a start waits for the
//     start of the opening only while the claim is stopped: a later start that records itself
//     first finishes that start of the opening, and the worker is not told the boot's sentence.
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
//
// EndHold takes the machine's lock outside Handle, so it first makes the claim write the machine
// owes, as Handle does at the head of every decision (repairBeforeTheHold). Where that write is
// still owed, the store does not hold the claim the machine holds, so EndHold answers an error
// and changes nothing, and the outbox asks again on its backoff.
func (m *Machine) EndHold(ctx context.Context, task *RequestDeliver) (bool, error) {
	m.mu.Lock()
	defer m.unlock()
	if err := m.repairBeforeTheHold(ctx); err != nil {
		return false, err
	}
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
		if one, kept := m.openingTask(*task); !m.given(one) {
			switch p := m.claim.Pending; {
			case KeptOperatorTask(p):
				m.log.Info("supervise: the work hold's opening keeps the operator's task the claim kept; the claim takes it first, and the opening's task waits behind it",
					"kept", p.ID, "delivery", one.ID)
			default:
				if p != nil {
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
	}
	m.log.Info("supervise: the work hold ended; the claim it stopped starts again", "delivery", pendingID(m.claim.Pending))
	if err := m.revive(ctx, false, nil, nil); err != nil {
		return false, err
	}
	return true, nil
}

// errClaimWriteOwed is the hold's end met by a claim write the database has not taken, which the
// repair could not make either (repairBeforeTheHold).
var errClaimWriteOwed = errors.New("a claim write the database has not taken is still owed")

// repairBeforeTheHold makes the claim write the machine owes (repair) before EndHold reads the
// claim, as Handle does at the head of every decision: EndHold takes the machine's lock outside
// Handle. A write the database refused for good drops the mark, as the repair's rule does, and
// the caller goes on. A write still owed (Machine.owedWrite) means the store does not hold the
// claim the machine holds, and a fact the caller applies would read the claim the store holds. So
// it answers errClaimWriteOwed, and the caller changes nothing.
func (m *Machine) repairBeforeTheHold(ctx context.Context) error {
	m.repair(ctx)
	if m.owedWrite == nil {
		return nil
	}
	return fmt.Errorf("supervise: the work hold's end of %s waits: %w", m.claim.Token, errClaimWriteOwed)
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
	m.dropUnmadeHoldStop()
	return true, m.sendPending(ctx)
}

// readHoldForStop reads whether the tree's work hold row stands (readWorkHold,
// Store.WorkHoldStands) for a decision that may make the work hold's stop: a decision of a
// claim the hold's stop is held over (Machine.held, By SuspendedByHold), or one of the hold's stop
// asked now (RequestSuspend, By SuspendedByHold). Handle calls it after the claim's lock, before
// the fence. The opening takes the same row for update before it reads the tree's claims, and
// deletes the row in that same transaction (workflow's tellAndEndWorkHold). So a read asked while
// an opening holds the row waits for the opening's commit. Where the opening has committed, the
// read finds no row:
//
//   - A stop held over the claim is not made: it is dropped (dropUnmadeHoldStop), and dropped is
//     true, so the decision sends a task the stop held back once its row has run. A turn's end
//     leaves the claim idle, a compaction's end gives the claim back, the hold's own deadline cuts
//     nothing, and a launch's ready goes ahead.
//   - The hold's stop asked now is not made: err is ErrWorkHoldEnded. Where the stop's row posts
//     that stop again while it is held over the claim, the held stop is dropped as above, and the
//     task it held back is sent. Where the claim already stands stopped by the hold, an earlier
//     attempt of the row made the stop, and the read of the row after the stop did not run or
//     failed (holdReadAfterStop). The opening may have read that claim live, so the daemon is told
//     that the hold has ended under the stop (holdEndedUnderStop), and the daemon starts the
//     claim. That fact reads the claims from the store, so where the stop's own claim write is
//     still owed — Handle's repair failed again — the daemon is not told, and err wraps
//     errClaimWriteOwed instead, not ErrWorkHoldEnded: the row asks again, and the next decision
//     makes the owed write first (Handle's repair) and then tells the daemon.
//
// The decision holds no lock on the row after this read. The opening waits for the row under the
// lock every fact takes (intake.SerializeFacts), so a decision that held the row across a call of
// the runtime — a process stop, a tree's or an issue's close — would hold every fact of the daemon
// for as long as that call ran, whatever source asked for it. So the opening can commit after this
// read and before the decision writes the stop. That is the one order the read leaves: a stop
// decided in the instant before an opening commits is made, and the claim is started at once with
// the opening's task. Each write of the hold's stop is followed by a read of the row in a
// transaction of its own (holdReadAfterStop). That read waits for an opening in flight, and where
// it finds the row gone, the daemon starts the claim (Deps.WorkHoldEndedUnderStop).
//
// The wait for the read ends at the claim-write deadline (Deps.claimWrite), as a claim write's
// wait does. A read that fails — the store failed, or the deadline passed — keeps a stop held over
// the claim held, with a warning, and the decision goes on: a stop it makes is read after as every
// stop is, and the stop's outbox row asks again on its backoff and posts the stop again, whose
// decision reads the row. The hold's stop asked now answers that error instead, so its outbox row
// asks again.
//
// A tool step and a model's answer read nothing, whatever the claim holds. Each kind's row in
// every state records what the agent did and nothing else: toolStepped writes the claim's steps
// and answered keeps the model in memory, in every live state, and a claim with no process ignores
// both. Neither moves the claim, sends a task or fires a held suspension, so neither decision can
// make the hold's stop, and a read for each step of a turn would cost the store a transaction for
// nothing. Any other decision reads nothing.
func (m *Machine) readHoldForStop(ctx context.Context, ev Event) (dropped bool, err error) {
	switch ev.(type) {
	case StreamToolStep, StreamAnswered:
		return false, nil
	}
	request, stopping := ev.(RequestSuspend)
	stopping = stopping && request.By == SuspendedByHold
	held := m.held != nil && m.held.By == SuspendedByHold
	if !stopping && !held {
		return false, nil
	}
	stands, err := m.readWorkHold(ctx)
	if err != nil {
		if held {
			m.log.Warn("supervise: the work hold's record did not answer; the hold's stop stays held, and the stop's outbox row drops it if the hold has ended",
				"tree", m.claim.Tree, "reason", m.held.Reason, "error", err)
		}
		if stopping {
			return false, fmt.Errorf("read the work hold of %s for the stop of %s: %w", m.claim.Tree, m.claim.Token, err)
		}
		return false, nil
	}
	if stands {
		return false, nil
	}
	if held {
		m.dropUnmadeHoldStop()
		dropped = true
	}
	if stopping {
		if m.claim.WorkHeld() {
			if m.owedWrite != nil {
				return dropped, fmt.Errorf("supervise: the work hold of %s has ended under the stop of %s, whose claim write is still owed: %w",
					m.claim.Tree, m.claim.Token, errClaimWriteOwed)
			}
			m.holdEndedUnderStop(request.HoldVersion)
		}
		err = ErrWorkHoldEnded
	}
	return dropped, err
}

// readWorkHold is whether the work hold of the claim's tree stands (Store.WorkHoldStands), read
// under the claim-write deadline (Deps.claimWrite), as a claim write's wait is bounded. Both reads
// of the row go through it: the decision's (readHoldForStop) and the read after the hold's stop
// (holdReadAfterStop).
func (m *Machine) readWorkHold(ctx context.Context) (bool, error) {
	reading, cancel := context.WithTimeout(ctx, m.deps.claimWrite())
	defer cancel()
	return m.deps.Store.WorkHoldStands(reading, m.claim.Tree)
}

// holdReadAfterStop reads the tree's work hold row again once the work hold's stop (request, By
// SuspendedByHold) is written: a read for share in a transaction of its own, under the claim-write
// deadline (readWorkHold). The opening takes that row for update before it reads the tree's
// claims, and deletes it in that same transaction. So this read waits for an opening in flight, or
// it reads the row while no opening holds it:
//
//   - The row stands: nothing more. An opening after this read reads the claims after the stop,
//     which is committed, and starts the claim itself.
//   - The row is gone: the opening has committed, and it may have read the claim before the
//     stop's write and started nothing of it. The daemon is told (holdEndedUnderStop), and it
//     starts each claim the hold stopped that no opening start covers.
//   - The read fails or misses its deadline: a warning, and the error is the decision's. The
//     stop's outbox row asks again: a held stop's row is asked again until the claim answers nil,
//     and the stop asked now answers this error. The row's next attempt posts the stop again: a
//     row that still stands changes nothing (suspendedAgain), and a row the opening deleted tells
//     the daemon (readHoldForStop).
//
// A stop whose own claim write failed is not read: the decision answers the write's error, the
// write is owed (repair), and the row's next attempt makes it before it reads the claim (Handle).
// A suspension by any other source reads nothing.
func (m *Machine) holdReadAfterStop(ctx context.Context, request RequestSuspend) error {
	if request.By != SuspendedByHold {
		return nil
	}
	stands, err := m.readWorkHold(ctx)
	if err != nil {
		m.log.Warn("supervise: the work hold's record did not answer after the hold's stop was written; the stop's outbox row starts the claim if the hold has ended",
			"tree", m.claim.Tree, "reason", request.Reason, "error", err)
		return fmt.Errorf("read the work hold of %s after the stop of %s: %w", m.claim.Tree, m.claim.Token, err)
	}
	if !stands {
		m.holdEndedUnderStop(request.HoldVersion)
	}
	return nil
}

// holdEndedUnderStop tells the daemon that the work hold of the claim's tree has ended while the
// claim stands stopped by the hold (Deps.WorkHoldEndedUnderStop), with version, the version that
// closed the gate as the hold's stop named it. The daemon applies a fact at once, and that fact
// waits on the lock every fact takes. The decision holds no lock on the row then
// (readHoldForStop).
func (m *Machine) holdEndedUnderStop(version int) {
	m.log.Info("supervise: the work hold has ended and the claim stands stopped by it; the daemon starts the claim",
		"tree", m.claim.Tree, "version", version)
	if m.deps.WorkHoldEndedUnderStop != nil {
		m.deps.WorkHoldEndedUnderStop(m.current(), version)
	}
}

// dropUnmadeHoldStop drops the work hold's held stop, which is not made, and logs it.
func (m *Machine) dropUnmadeHoldStop() {
	m.log.Info("supervise: the work hold ended before its stop was made; the stop is not made",
		"reason", m.held.Reason, "deadline", m.held.Deadline, "launch", m.heldLaunch)
	m.dropHeld()
}

// OpeningTask is the one task the work hold's opening gives a claim it stopped: opening, the
// opening's sentences, a blank line, and work. A start that carries the opening's sentences apart
// (record.SuperviseRequest.Opening) carries a task that begins with OpeningTask(opening, ""), and
// the record refuses any other.
func OpeningTask(opening, work string) string { return opening + "\n\n" + work }

// OpeningWork is task's work: task with the opening's sentences it begins with (OpeningTask(opening,
// "")) cut off its front. A task with no opening, or a task that does not begin with the opening's
// sentences, is its whole text.
func OpeningWork(task, opening string) string {
	if opening == "" {
		return task
	}
	return strings.TrimPrefix(task, OpeningTask(opening, ""))
}

// OpeningKeeps is whether the work hold's opening keeps pending, the task of its own that a claim
// the hold stopped holds, in place of the task of the opening's start, a start of the run
// generation at entry (resend: RequestDeliver.ResendEntry). The opening keeps a task that no turn
// has taken, unless the start carries newer work than it. It never keeps the daemon's own delivery
// of a relaunch's record, which is no work: its sentence goes in front of whatever task follows it
// (Delivery.message). Nor does it keep a task of news alone (Delivery.NewsOnly): it holds no work,
// and the opening's start carries the news it still owes, which no turn confirmed. Nor does it keep
// an operator's task that no turn has taken (KeptOperatorTask): that task is no work of the run,
// and the opening neither retires it nor puts its work behind the opening's sentences. The claim
// takes it first, and the opening's task waits behind it (EndHold).
//
// Newer work is a task of a later run than the kept one's, or of a later entry of the same run,
// that is no resend: a transition or an owed entry written for the role while the gate was closed,
// whose start the opening's folded into, so the role is given the newest entry's task. A start of
// no entry, the phase's resume task the boot builds, carries none.
//
// It is the one statement of the rule, and two readers ask it: openingTask, which builds the one
// task, and the daemon's startNewsOf, which renders the news the opening's start carries over the
// kept work, so the news that start carries is news of the work the claim is given.
func OpeningKeeps(pending *Delivery, generation uint64, entry int64, resend bool) bool {
	if pending == nil || !pending.ConfirmedAt.IsZero() || pending.Relaunch() || pending.NewsOnly || KeptOperatorTask(pending) {
		return false
	}
	newer := entry > 0 && !resend && (generation > pending.Generation || (generation == pending.Generation && entry > pending.Entry))
	return !newer
}

// KeptOperatorTask is whether pending, the delivery a claim holds, is an operator's own task that
// no turn has taken: a task (HoldsTask) of no run (Delivery.Generation 0) that no turn confirmed.
// The operator's deliver route queues such a task with no run, no phase and no entry (api's
// deliverEvent). The work hold's opening never retires it, and never puts its work behind the
// opening's sentences: the claim takes it first, and the start that ends the hold waits behind it
// (EndHold, OpeningKeeps).
//
// It is the one statement of the rule, and four readers ask it: EndHold, OpeningKeeps, and the
// daemon's openingGoesFirst and startNewsOf. openingGoesFirst has a start that carries no
// opening's sentences wait while the claim's own start of the opening is queued, so that start
// ends the hold, and the daemon's waitForOpeningInFlight keeps that wait until that start's task
// is queued, so it goes second. Where no such start is queued, the start that carries no
// opening's sentences ends the hold itself, and startNewsOf settles no opening as told for it: its
// task follows the operator's task with no opening's sentences, so the opening stays owed to its
// news.
func KeptOperatorTask(pending *Delivery) bool {
	return HoldsTask(pending) && pending.ConfirmedAt.IsZero() && pending.Generation == 0
}

// openingTask is the one task the hold's opening gives a claim it stopped (EndHold): task, the
// opening start's own, unless the opening keeps the task the claim kept (OpeningKeeps). Then it is
// the kept task's work behind task's opening sentences (RequestDeliver.Opening): its phase, run,
// entry, kept decisions, check look and review round, under task's id and row, a resend of its
// entry (RequestDeliver.ResendEntry), so the row decides it and the entry the claim was given
// stands. kept says which of the two it is.
//
// The kept task's work is its text with the sentences of an earlier opening taken off its front
// (Delivery.Work): a claim an earlier opening started and a later close stopped before any turn
// took that opening's task keeps the earlier opening's task. The one task carries the newest
// opening's sentences once, then that work, and it is marked with those sentences
// (RequestDeliver.Opening), so the next opening reads it the same way.
//
// The news of the one task is the news task carries, where task carries news (RequestDeliver.News).
// The opening's start reads the kept task by the same rule, and renders the news the claim is owed
// now over the kept work (the daemon's startNewsOf). So task's text is the opening's sentences,
// the kept work and that news, and the one task takes that text with that news, which a send renders
// again (Deps.DesignNews). Where task carries no news, the kept task's own news goes with its work
// (Delivery.News), whose own text is the work without the opening's sentences, so each send renders
// that news again behind the newest sentences.
func (m *Machine) openingTask(task RequestDeliver) (one RequestDeliver, kept bool) {
	p := m.claim.Pending
	if !OpeningKeeps(p, task.Generation, task.Entry, task.ResendEntry) {
		return task, false
	}
	one = task
	if len(task.News) == 0 {
		opening := task.Opening
		one.Task, one.Opening, one.News = p.Work(), "", p.News
		if opening != "" {
			one.Task, one.Opening = OpeningTask(opening, one.Task), opening
		}
	}
	one.Phase, one.Generation, one.Entry, one.ResendEntry = p.Phase, p.Generation, p.Entry, true
	one.Kept, one.CheckLook, one.RoundNamed = p.Kept, p.CheckLook, p.RoundNamed
	return one, true
}
