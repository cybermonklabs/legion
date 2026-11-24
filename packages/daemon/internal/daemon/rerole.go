package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan" // the package's own `plan` type holds the bare name
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// prepareClaimForStep makes machine's claim, held as the start read it for startAgent, ready for
// the step that the start of a phase entry (payload, phaseEntry) is for, before it brings the
// claim back. The steps of one key are one claim, and its next step may be of another role than it holds, or of another
// agent than the one whose session it holds (agent, the start's agent, startAgent, and
// supervise.Claim.SessionAgent). Either
// way the process the claim may still run was started for the step before it, so the start asks
// that process to suspend, as the daemon and keeping its session, and is a wait (wait.ErrWaiting)
// on the row's backoff while the claim still runs: a claim in a turn holds the suspension for the
// turn's end and a launching one until its agent is ready (supervise's suspend rows), as
// stopElsewhere's suspension is held, so the step before it is not cut off mid-turn. Once the claim
// runs nothing it takes the step's role (supervise.Machine.Rerole), the one place a claim's role
// changes. The decision that brings the claim back then resumes it on its session in that role or,
// for another agent, leaves the session behind and records the start's agent
// (supervise.StartWork.Agent). A claim of the step's role and agent, and a start of no phase entry
// — a wake, a resume, a role admission brings back — are left as they stand.
//
// A start that gives the claim another role waits first for a launcher of the claim's slot that
// takes the new role's credential (reroleWaitsForCredential).
//
// ctx carries the step's standing (stepStanding). Whichever of the suspension and the re-role
// comes first judges it under the machine's lock: a step the issue left before it is
// supervise.ErrStepLeft, with nothing of the claim changed, and a step that stands commits the
// start, so the other act then judges nothing (supervise.Standing). A move after that act finds
// the start under way: a re-role after a suspension that landed at once gives the claim the role
// all the same, and the claim is not brought back for the step the issue left (supervise.StartWork,
// through Deps.PhaseHolds). A suspension the machine holds for the turn's end lands at that end on
// the judgment it passed when it was asked, and the row waits; its next run carries a standing of
// its own, judged again by the re-role. So a move back in between costs the claim that one
// suspension, and the move back's own start resumes it on its session in the role it holds.
func (r *outbox) prepareClaimForStep(ctx context.Context, machine *supervise.Machine, row record.OutboxRow, issue record.Issue, held supervise.Claim, payload record.SuperviseRequest, agent stepplan.AgentID) error {
	if !phaseEntry(payload) {
		return nil
	}
	token := held.Token
	rerole := held.Role != payload.Role
	if !rerole && !held.SessionOfAnotherAgent(agent) {
		return nil
	}
	if rerole {
		if err := r.reroleWaitsForCredential(ctx, row, issue, token, payload.Role); err != nil {
			return err
		}
	}
	if !held.RunsNothing() {
		reason := fmt.Sprintf("its claim's next step on %s is another agent's", issue.Key)
		if rerole {
			reason = fmt.Sprintf("its agent's next step on %s is the %s's", issue.Key, payload.Role)
		}
		err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon, Reason: reason})
		var refused *supervise.RefusedError
		if err != nil && !errors.Is(err, supervise.ErrSuspendHeld) && !errors.As(err, &refused) {
			return fmt.Errorf("suspend claim %s for its %s step: %w", token, payload.Role, err)
		}
		if held = machine.Claim(); !held.RunsNothing() {
			return wait.Errorf("the %s step of %s waits for its claim %s to end its turn as %s", payload.Role, issue.Key, token, held.Role)
		}
	}
	if !rerole {
		return nil
	}
	return machine.Rerole(ctx, payload.Role)
}

// phaseEntry is whether payload is the start of a phase entry: it carries an entry and names a
// phase. That is the one start held to its entry by the entry fence of supervise, which says why
// no other start is, and the one whose first act on its claim to pass under the machine's lock
// judges its step again there (stepStanding).
func phaseEntry(payload record.SuperviseRequest) bool {
	return payload.Entry > 0 && payload.Phase != ""
}

// stepStanding is ctx carrying the standing of the step the start of a phase entry (payload,
// phaseEntry) is for (supervise.Standing, stepStands). It is the one standing of the start: the
// acts that make the claim ready for the step and record its agent (prepareClaimForStep,
// supervise.Machine.TakeSessionFor) each judge it under the machine's lock, and the first of them
// to pass commits the start. A start of no phase entry is answered ctx as it is, and those acts
// judge nothing.
func (r *outbox) stepStanding(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) context.Context {
	if !phaseEntry(payload) {
		return ctx
	}
	return supervise.Standing(ctx, func(ctx context.Context) (bool, error) { return r.stepStands(ctx, row, payload) })
}

// stepStands says whether the step a start of a phase entry (payload) is for is still the work its
// issue asks of the role, read from the record again: the issue runs the generation and stands in
// the phase the start serves, and the role's phases row now names no later entry than the start's
// (record.StartLeft). They are the start's own fences (supervise), which the first act the start
// asks of its claim to pass under the machine's lock — the suspension, the re-role, or the record
// of its agent on a running claim that records none — judges again there, and so commits the start
// (stepStanding), since a move back or a later entry can commit between the fences' read and that
// lock.
func (r *outbox) stepStands(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) (bool, error) {
	stands := false
	err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := r.records.Issue(ctx, tx, row.Issue)
		if err != nil || issue == nil || issue.Generation != payload.Generation || issue.Phase != payload.Phase {
			return err
		}
		rows, err := r.records.Phases(ctx, tx, row.Issue)
		if err != nil {
			return err
		}
		_, left := record.StartLeft(rows, payload.Role, payload.Entry)
		stands = !left
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read the step of start row %d of %s again: %w", row.ID, row.Issue, err)
	}
	return stands, nil
}

// reroleWaitsForCredential is the fence the start of an entry that re-roles its claim, token,
// carries where the claim's slot in the issue's live pod runs a launcher of an image before the
// credential wait (runtime.IssuePodRoles' SlotWaitsForCredential): that launcher starts a child at
// once, though the re-role has just written the new role's credential into the slot (the sandbox
// runtime's writeLaunchCredential),
// so the child would act as the App its gh files held, the earlier role's, until the next refresh
// and the kubelet's sync. It answers a wait, whose text begins with pods.StartWaitsForPodPrefix and
// which is runtime.ErrAwaitsPodUpdate too, as roleWaitsForItsPod's is, and the row tries again on
// its own backoff; the pod's update round or its release replaces the pod with launchers that
// wait. Where nothing ends the wait, the deployment's idle release off and no process recorded in
// the pod, the controller is told once, as roleWaitsForItsPod tells it (podWaitPrefix). A runtime
// with no pods answers nothing, and a start that re-roles nothing never asks. A slot with no
// launcher connected answers that it waits, which is what a daemon that has just restarted reads of
// every slot: the launch then judges the launcher it reaches, and refuses a start of another App
// into one that does not wait with the same runtime.ErrAwaitsPodUpdate, which the supervisor
// charges nothing (the sandbox runtime's relaunch). A claim whose token names no slot
// (claim.Token.Slot), and a slot the runtime refuses, is refused: no launcher serves it, so no
// answer that it waits can stand.
func (r *outbox) reroleWaitsForCredential(ctx context.Context, row record.OutboxRow, issue record.Issue, token claim.Token, role claim.Role) error {
	podRoles, ok := r.supervisor.deps.Runtime.(runtime.IssuePodRoles)
	if !ok {
		return nil
	}
	slot, ok := token.Slot()
	if !ok {
		return fmt.Errorf("the re-role of %s into %s: its claim token names no slot of a pod", token, role)
	}
	pod, waits, err := podRoles.SlotWaitsForCredential(issue.Key, slot)
	if err != nil {
		return fmt.Errorf("the re-role of %s into %s: %w", token, role, err)
	}
	if waits {
		return nil
	}
	prefix, err := r.podWaitPrefix(ctx, row, issue.Key, pod, role, token, "before its launchers waited for a new role's credential")
	if err != nil {
		return err
	}
	return wait.Errorf("%s%s of %s: the pod %s was built before its launchers waited for a new role's credential; its update round or its release replaces it: %w",
		prefix, role, issue.Key, pod, runtime.ErrAwaitsPodUpdate)
}
