package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// closeRoot ends a tree its root architect closes before the tree's first phase starts: a human
// decided no change at the design gate, or the issue is moot. sign_off
// needs the production check and a backward move a phase worker, so without this only a human's
// status write could end such a tree. The close_root route writes done in Dispatch before it
// applies this fact, so Dispatch refuses the close while a child of another project is open; the
// fact queues the daemon's own close row behind it, whose reason the outbox posts on the issue
// after the status (Dispatch takes a message on a closed issue), and lingers the tree as any
// close does; the admission slot is released in the same fact.
//
// It is refused, as the sign-off is and with the same code, while any issue beneath the root is
// still in the workflow (OpenBeneath): the close lingers the whole tree, which would suspend
// those issues mid-turn and lose their work, and the order a tree keeps is that its children
// finish first. A root at admitted with children running is exactly the shape children-first
// creates, so this is the common case rather than a corner of it. Past that, it is refused as the
// sign-off is, in the same order (acceptanceHeld, dispatch://INFRA-3320 R8), while a root whose
// step rests on the root's approval is still in the workflow (SIGNOFF_RESTING_ROOT_IN_WORKFLOW),
// and then while a Dispatch child of the root is open (SIGNOFF_CHILD_OPEN). A child filed and never
// released is open, so a root at admitted with only such children closes once each of them is
// closed or in icebox, which the refusal says. The judgement is CloseRootRefusal's, which the
// route asks first in a read-only transaction of its own, before it writes done in Dispatch.
//
// A root waiting on a person (phase.AwaitingPerson) closes the same way, because it has started
// no phase either: a root reaches that phase only from admitted, and it is where the answer
// "close this" arrives (dispatch://INFRA-545, whose architect could not end its tree and whose
// controller's own Dispatch close the daemon set back, as a session's write on a waiting root).
// It keeps its slot while it waits (blockers.waitsOf), and leave frees that slot once.
//
// The event of the route's own done write can reach the record before this fact, and leave the
// tree as any close Dispatch shows does (closedByItsOwnWrite): the close is then made already, so
// the fact posts the reason that leave did not, and is taken.
func (e *Engine) closeRoot(ctx context.Context, tx pgx.Tx, fact intake.CloseRoot) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	if closedByItsOwnWrite(*root, fact) {
		e.logOnCommit(ctx, "workflow: the close_root route's own done write reached the record before its fact; the fact posts the reason", "issue", root.Key,
			"written", fact.Written, "recorded", root.LastDispatchSeq)
		return intake.Result{}, e.enqueue(ctx, tx, root.Key, record.MessagePost{Body: fact.Message()})
	}
	if refusal, err := e.closeRootRefusal(ctx, tx, *root); err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	if err := e.enqueue(ctx, tx, root.Key, record.CloseStatusWrite("done", root.Status, fact.Message(), root.Generation)); err != nil {
		return intake.Result{}, err
	}
	root.Status = "done"
	return intake.Result{}, e.leave(ctx, tx, *root, "done")
}

// CloseRootRefusal is the refusal the root architect's close_root of key meets as the daemon
// records key now, nil where the close is taken, and nil for an issue it does not record: every
// refusal of closeRoot, read and never written, so the close_root route judges a close in a
// read-only transaction before it writes done in Dispatch, and the fact judges it again in its own.
func (e *Engine) CloseRootRefusal(ctx context.Context, tx pgx.Tx, key string) (*intake.Refusal, error) {
	root, err := e.store.Issue(ctx, tx, key)
	if err != nil || root == nil {
		return nil, err
	}
	return e.closeRootRefusal(ctx, tx, *root)
}

// closeRootRefusal is CloseRootRefusal of root as the caller read it.
func (e *Engine) closeRootRefusal(ctx context.Context, tx pgx.Tx, root record.Issue) (*intake.Refusal, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) {
		return refused("ROOT_REQUIRED", fmt.Sprintf("%s is a child of %s; close_root ends a tree root, and a child leaves with park_child "+parkRule, root.Key, root.Tree)).Refusal, nil
	}
	if !closableAtPhase(root.Phase) || root.Lingers() || record.OutOfWorkflow(root.Status) {
		return refused("CLOSE_AFTER_PHASE_STARTED", fmt.Sprintf("%s is in phase %s with status %s; close_root ends a tree only while its root has started no phase — at admitted, whether or not it holds its slot, or waiting on a person — and a tree past that, or one already leaving, ends through its workflow or a human", root.Key, root.Phase, root.Status)).Refusal, nil
	}
	held, err := e.acceptanceHeld(ctx, tx, root)
	if err != nil {
		return nil, err
	}
	if refusal, refuses := held.refusal(actCloseRoot, root.Key); refuses {
		return refusal.Refusal, nil
	}
	return nil, nil
}

// closedByItsOwnWrite says whether root left the workflow on the event of the close_root route's
// own done write (intake.CloseRoot.Written) before the fact arrived: the record has applied that
// event or a later one, Dispatch showed done there, and the record no longer holds root in the
// workflow (record.InWorkflow), as the leave on that event left it.
func closedByItsOwnWrite(root record.Issue, fact intake.CloseRoot) bool {
	return fact.Written > 0 && root.LastDispatchSeq >= fact.Written && root.DispatchStatus == record.DoneStatus && !record.InWorkflow(root)
}

// closableAtPhase says whether a root at this phase has started no phase of its tree, which is
// what close_root ends. Both are states a root sits in with its design gate unanswered.
func closableAtPhase(p phase.Phase) bool {
	return p == phase.Admitted || p == phase.AwaitingPerson
}
