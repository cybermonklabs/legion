package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// reroleForStep gives machine's claim the role of payload, the start of the step its agent is
// started for, and is a wait (wait.ErrWaiting) while the claim still runs a process. That process
// runs the role it was started with, so the start asks it to suspend, as the daemon and keeping its
// session, and the start waits on the row's backoff: a claim in a turn holds the suspension for the
// turn's end and a launching one until its agent is ready (supervise's suspend rows), as
// stopElsewhere's suspension is held, so the step before it is not cut off mid-turn. Once the claim
// runs nothing it takes the role (supervise.Machine.Rerole), and the start resumes it on its
// session with that role. This is the one place a claim's role changes.
//
// The suspension and the re-role are asked with the step's standing (supervise.Standing,
// stepStands): each is judged under the machine's lock, the first of them to pass commits the
// start, and a step the issue left before it is supervise.ErrStepLeft, with nothing of the claim
// changed. A suspension the machine holds for the turn's end lands at that end on the judgment it
// passed when it was asked: a move back in between costs the claim that one suspension, and the
// move back's own start resumes it on its session in the role it holds.
func (r *outbox) reroleForStep(ctx context.Context, machine *supervise.Machine, token claim.Token, row record.OutboxRow, payload record.SuperviseRequest) error {
	role := payload.Role
	ctx = supervise.Standing(ctx, func(ctx context.Context) (bool, error) { return r.stepStands(ctx, row, payload) })
	if held := machine.Claim(); !held.RunsNothing() {
		err := suspendUntilIdle(ctx, machine, token, fmt.Sprintf("its agent's next step on %s is the %s's", row.Issue, role), func(held supervise.Claim) error {
			return wait.Errorf("the %s step of %s waits for its claim %s to end its turn as %s", role, row.Issue, token, held.Role)
		})
		if err != nil {
			return err
		}
	}
	return machine.Rerole(ctx, role)
}

// suspendUntilIdle asks machine's claim to suspend, as the daemon and keeping its session, and is
// waiting's wait (wait.ErrWaiting) while the claim still runs a process: a claim in a turn holds the
// suspension for the turn's end and a launching one until its agent is ready (supervise's suspend
// rows), so the step before is not cut off mid-turn, and the start waits on the row's backoff. A
// suspension the machine holds (supervise.ErrSuspendHeld) or refuses for the state it is in is no
// failure: the claim's state answers. ctx carries the start's standing where it has one
// (supervise.Standing), which the suspension judges under the machine's lock. Its callers are the
// two suspensions a start asks of a claim that runs: the re-role (reroleForStep) and the leave of a
// key's session (suspendToLeave).
func suspendUntilIdle(ctx context.Context, machine *supervise.Machine, token claim.Token, reason string, waiting func(held supervise.Claim) error) error {
	err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon, Reason: reason})
	var refused *supervise.RefusedError
	if err != nil && !errors.Is(err, supervise.ErrSuspendHeld) && !errors.As(err, &refused) {
		return fmt.Errorf("suspend claim %s (%s): %w", token, reason, err)
	}
	if held := machine.Claim(); !held.RunsNothing() {
		return waiting(held)
	}
	return nil
}

// stepStands says whether the step a start of a phase entry (payload) is for is still the work its
// issue asks of the role, read from the record again (startStands). The suspension and the re-role a
// start asks of its claim judge it again under the machine's lock (supervise.Standing,
// reroleForStep), since a move or a later entry can commit between the fences' read and that lock.
func (r *outbox) stepStands(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) (bool, error) {
	stands := false
	err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := r.records.Issue(ctx, tx, row.Issue)
		if err != nil || issue == nil {
			return err
		}
		rows, err := r.records.Phases(ctx, tx, row.Issue)
		if err != nil {
			return err
		}
		stands = startStands(*issue, rows, payload)
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read the step of start row %d of %s again: %w", row.ID, row.Issue, err)
	}
	return stands, nil
}

// startStands is the start's own fence of a start of a phase entry (payload), the one the outbox's
// start judges before it acts: the issue runs the generation the start serves, holds the work of
// the start's role at the phase it serves (record.Issue.HoldsQueued, which at the phase step, where
// every step of a role the deployment defines stands, names the step's role), and its role's phases
// row names no later entry than the start's. All three, and not a part of them: a move from one
// step of the phase step to the next raises no entry on the earlier step's row and leaves the
// phase as it was, so only the step's role says that the earlier step's start is no longer the work.
func startStands(issue record.Issue, rows []record.PhaseRow, payload record.SuperviseRequest) bool {
	if issue.Generation != payload.Generation {
		return false
	}
	if payload.Phase != "" && !issue.HoldsQueued(payload.Role, payload.Phase) {
		return false
	}
	return payload.Entry == 0 || payload.Phase == "" || payload.Entry >= record.PhaseRowOf(rows, payload.Role).Entry
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
	prefix, err := r.podWaitPrefix(ctx, row, issue.Key, pod, role, "its launchers waited for a new role's credential")
	if err != nil {
		return err
	}
	return wait.Errorf("%s%s of %s: the pod %s was built before its launchers waited for a new role's credential; its update round or its release replaces it: %w",
		prefix, role, issue.Key, pod, runtime.ErrAwaitsPodUpdate)
}
