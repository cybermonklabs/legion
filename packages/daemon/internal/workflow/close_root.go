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
// The event of the route's own done write can reach the record before this fact. It is no
// departure while the record holds the root in the workflow (closeRootWriteAhead, which
// dispatchIssue asks before it leaves the tree on a done), so this fact alone decides the close,
// with every refusal below: one refused, a first phase started since the route's judgement among
// them, has the route write back the status Dispatch showed before its write. A close that is
// taken drops every status row queued for the root before its own (as dispatchIssue does before
// every leave): the set-back the route queued before its write (the API's queueSetBack) among
// them, which the close leaves nothing to undo.
func (e *Engine) closeRoot(ctx context.Context, tx pgx.Tx, fact intake.CloseRoot) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	if refusal, err := e.closeRootRefusal(ctx, tx, *root); err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	if err := e.store.DropStatusWrites(ctx, tx, root.Key); err != nil {
		return intake.Result{}, err
	}
	if err := e.enqueue(ctx, tx, root.Key, record.CloseStatusWrite("done", root.Status, fact.Message(), root.Generation)); err != nil {
		return intake.Result{}, err
	}
	root.Status = "done"
	return intake.Result{}, e.leave(ctx, tx, *root, "done")
}

// CloseRootRefusal is the refusal the root architect's close_root fact meets as the daemon records
// its root now, nil where the close is taken, and nil for an issue it does not record: the fences
// every architect's request meets (requestRefusal), then every refusal of closeRoot, read and
// never written, so the close_root route judges a close in a read-only transaction before it
// writes anything in Dispatch, and the fact judges it again in its own.
func (e *Engine) CloseRootRefusal(ctx context.Context, tx pgx.Tx, fact intake.CloseRoot) (*intake.Refusal, error) {
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || root == nil {
		return nil, err
	}
	if refusal, err := e.requestRefusal(ctx, tx, fact); err != nil || refusal != nil {
		return refusal, err
	}
	return e.closeRootRefusal(ctx, tx, *root)
}

// closeRootRefusal is CloseRootRefusal of root as the caller read it.
func (e *Engine) closeRootRefusal(ctx context.Context, tx pgx.Tx, root record.Issue) (*intake.Refusal, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) {
		return refused("ROOT_REQUIRED", fmt.Sprintf("%s is a child of %s. close_root ends a tree root.", root.Key, root.Tree)+" "+record.EndChildRule).Refusal, nil
	}
	if !closableAtPhase(root.Phase) || root.Lingers() || record.OutOfWorkflow(root.Status) {
		return refused("CLOSE_AFTER_PHASE_STARTED", fmt.Sprintf("%s is in phase %s with status %s; close_root ends a tree only while its root has started no phase — at admitted, whether or not it holds its slot, or waiting on a person — and a tree past that, or one already leaving, ends through its workflow or a human", root.Key, root.Phase, root.Status)).Refusal, nil
	}
	held, err := e.acceptanceHeld(ctx, tx, root)
	if err != nil {
		return nil, err
	}
	if refusal, refuses := held.refusal(actCloseRoot, root.Key, ""); refuses {
		return refusal.Refusal, nil
	}
	return nil, nil
}

// closeRootWriteAhead says whether fact is the event of a close_root's own done write that the
// daemon has not recorded as a close: the daemon wrote it (intake.DispatchIssue.DaemonActor), and
// the record still holds the issue in the workflow (record.InWorkflow). Every other done the
// daemon writes, a close status row, is queued by a fact that took the issue out of the workflow
// first. The route writes the done before the close's fact, and writes back the status it replaced
// when that fact is refused, so the event is no departure: left on it, a tree whose first phase
// started after the route's judgement would be closed with no refusal, and the write back, which
// writes nothing over an issue out of the workflow, could not undo it.
func closeRootWriteAhead(recorded *record.Issue, fact intake.DispatchIssue) bool {
	return fact.DaemonActor && fact.Status == record.DoneStatus && recorded != nil && record.InWorkflow(*recorded)
}

// closableAtPhase says whether a root at this phase has started no phase of its tree, which is
// what close_root ends. Both are states a root sits in with its design gate unanswered.
func closableAtPhase(p phase.Phase) bool {
	return p == phase.Admitted || p == phase.AwaitingPerson
}
