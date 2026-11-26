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
// status write could end such a tree. The close writes done to Dispatch with the architect's
// reason, which the write posts on the issue first, and lingers the tree as any close does; the
// admission slot is released in the same fact.
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
// closed or in icebox, which the refusal says.
//
// A root waiting on a person (phase.AwaitingPerson) closes the same way, because it has started
// no phase either: a root reaches that phase only from admitted, and it is where the answer
// "close this" arrives (dispatch://INFRA-545, whose architect could not end its tree and whose
// controller's own Dispatch close the daemon set back, as a session's write on a waiting root).
// It keeps its slot while it waits (blockers.waitsOf), and leave frees that slot once.
//
// It is refused for a root whose work merged (CLOSE_AFTER_MERGE): such a root closes by the checks
// of its goal or by a person (D16, door 8), whether it waits in verifying or a next change of it
// waits at admitted. Its architect withdraws each defect it judges moot and asks a person, whose
// done closes it.
func (e *Engine) closeRoot(ctx context.Context, tx pgx.Tx, fact intake.CloseRoot) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	if !claim.IsTreeRoot(root.Key, root.Tree) {
		return refused("ROOT_REQUIRED", fmt.Sprintf("%s is a child of %s. close_root ends a tree root.", root.Key, root.Tree)+" "+record.EndChildRule), nil
	}
	if root.Merge.Merged() {
		return refused("CLOSE_AFTER_MERGE", fmt.Sprintf("Pull request #%d of %s merged, so %s closes by the checks of its goal. "+
			"Withdraw each defect you judge moot with its reason, then ask a person, whose done closes it. "+
			"A check you judge wrong goes into a new version of the Verification document that a person approves.",
			root.Merge.Number, root.Key, root.Key)), nil
	}
	if !closableAtPhase(root.Phase) || root.Lingers() || record.OutOfWorkflow(root.Status) {
		return refused("CLOSE_AFTER_PHASE_STARTED", fmt.Sprintf("%s is in phase %s with status %s; close_root ends a tree only while its root has started no phase — at admitted, whether or not it holds its slot, or waiting on a person — and a tree past that, or one already leaving, ends through its workflow or a human", root.Key, root.Phase, root.Status)), nil
	}
	held, err := e.acceptanceHeld(ctx, tx, *root)
	if err != nil {
		return intake.Result{}, err
	}
	if refusal, refuses := held.refusal(actCloseRoot, root.Key, ""); refuses {
		return refusal, nil
	}
	if err := e.enqueue(ctx, tx, root.Key, record.CloseStatusWrite("done", root.Status, fact.Message(), root.Generation)); err != nil {
		return intake.Result{}, err
	}
	root.Status = "done"
	return intake.Result{}, e.leave(ctx, tx, *root, "done")
}

// closableAtPhase says whether a root at this phase has started no phase of its tree, which is
// what close_root ends. Both are states a root sits in with its design gate unanswered.
func closableAtPhase(p phase.Phase) bool {
	return p == phase.Admitted || p == phase.AwaitingPerson
}
