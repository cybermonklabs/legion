package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A merge forward is an act as the implementer, so while the record holds one asked of the owner
// that has not ended (record.OwnersMergeForward) the owner's slot holds the implement App (the
// daemon's ActsAs, which reads that predicate at each write of the slot). One predicate decides it,
// over the issue and its pull request as the record holds them, and each write of either record
// asks it of the record before the write and after it (ownersMergeForwardWritten):
//   - a write that ends the ask queues the write of the owner's slot back to its own App, as the end
//     of each step the owner does as a role of another App does (ownerSlotCredential). That write
//     is made where the record then answers the owner's own App, and an act of another App that
//     runs by then keeps its App (internal/daemon's slotCredential), so no path leaves the slot on
//     the implement App once no act holds it there;
//   - a write that opens the ask again asks the owner again (askOwnerAgain), since that ask's
//     delivery is what writes the slot for the implement App before it tells the owner.
//
// The writes are the pull request's (putPullRequest: a newer ask, the landing, the clear of the
// ask on a move into the implementation step or a leave, the close or the merge of the pull
// request), the issue's (writeIssueOn: a hold, a stopped task's among them, its release, and the
// issue's leave of the workflow) and a new generation's (clearGeneration: a child set back to
// todo, an adoption into a tree), which drops the ask with the run. The one other writer that ends
// an ask, the daemon's rename pass deleting a pull request record whose name another issue's
// holds, writes no issue whose pod may still be live (internal/daemon's renamePass keep), so no
// slot of the owner's runs then, and the owner's next launch writes the App the record gives it.
//
// A newer ask that replaces the one the record holds (mergeForward: the head moved, or the move
// out of the implementation step asks again) is a write of the pull request as any other is. Where
// it is asked of the owner too, the predicate stays true, and the slot stays on the implement App
// until the new ask ends. Where it reaches another claim, the agent of another step of the type
// implementation, the predicate turns false: the write ends the owner's ask, and the owner's slot
// is written back to its own App. A tree whose root lingers has every role suspended
// (record.TreeLingers), and the daemon's ActsAs answers the owner's own App there, which the
// owner's next launch writes; no fact of the linger is a write of the slot, and the predicate
// reads no linger.

// ownersMergeForwardWritten acts on what a write of issue's records does to a merge forward asked
// of its owner, asking the one predicate (record.OwnersMergeForward) of the records before the
// write, before and beforePR, and after it, after and afterPR (nil for no pull request): where the
// write ends the ask, the owner's slot is written back to its own App (mergeForwardWriteBack), and
// where it opens the ask again, the owner is asked again (askOwnerAgain). Each writer hands it the
// record it writes as the store holds it and as the write leaves it, and the other record as the
// store holds it now, so of two writes of one fact that each end the ask, the second reads the
// first's record and acts on nothing.
func (e *Engine) ownersMergeForwardWritten(ctx context.Context, tx pgx.Tx, before, after record.Issue, beforePR, afterPR *record.PullRequest) error {
	was, is := record.OwnersMergeForward(before, beforePR), record.OwnersMergeForward(after, afterPR)
	switch {
	case was && !is:
		// Planted fault, a control of a test, never merged: an end of the ask writes nothing back.
		return nil
	case is && !was:
		return e.askOwnerAgain(ctx, tx, after, *afterPR)
	}
	return nil
}

// pullRequestWritten is ownersMergeForwardWritten for a write of a pull request over before, the
// row the record held (nil for none), with after, read against the issue the record holds. Where
// neither row holds a merge forward, the predicate is false on both sides, and nothing more is read.
func (e *Engine) pullRequestWritten(ctx context.Context, tx pgx.Tx, before *record.PullRequest, after record.PullRequest) error {
	if (before == nil || before.MergeForward == nil) && after.MergeForward == nil {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, after.Issue)
	if err != nil || issue == nil {
		return err
	}
	return e.ownersMergeForwardWritten(ctx, tx, *issue, *issue, before, &after)
}

// issueWritten is ownersMergeForwardWritten for a write of after over the issue the record holds,
// with the pull request the record holds. A write that leaves the issue's half of the predicate as
// it found it (record.Issue.ActiveInWorkflow) reads nothing more.
func (e *Engine) issueWritten(ctx context.Context, tx pgx.Tx, after record.Issue) error {
	before, err := e.store.Issue(ctx, tx, after.Key)
	if err != nil || before == nil || before.ActiveInWorkflow() == after.ActiveInWorkflow() {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, after.Key)
	if err != nil || pr == nil {
		return err
	}
	return e.ownersMergeForwardWritten(ctx, tx, *before, after, pr, pr)
}

// clearGeneration clears the facts of issue's run (record.Store.ClearGeneration) before the issue
// takes its next generation: a re-entry of a child set back to todo (ReenterChild) and an adoption
// into a tree (moveIntoTree). The clear drops the merge forward of the issue's pull request with
// the run, so it is a write of that pull request (ownersMergeForwardWritten), which ends a merge
// forward asked of the owner while the owner's architect may still run.
func (e *Engine) clearGeneration(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	before, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if err := e.store.ClearGeneration(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	if before == nil {
		return nil
	}
	after, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	return e.ownersMergeForwardWritten(ctx, tx, issue, issue, before, after)
}

// askOwnerAgain asks the owner of issue again for the merge forward pr holds, where a write opens
// it again (ownersMergeForwardWritten): a hold's retry or a stopped task's restart after the hold
// ended it and wrote the slot back to the owner's own App. The owner's slot acts as the implementer
// once more, and the owner was told of the ask before. Nothing else writes the slot then: the ask
// is asked once for each head (mergeForward), and the slot's next refresh or launch comes after the
// owner was told to merge. The ask told again is that write: its delivery has the runtime write the
// owner's slot for the implement App and tells the owner only once the slot's gh files hold it (the
// daemon's ownerSlotActs). It is the ask the record holds, built by the one builder mergeForward's
// ask uses too (record.MergeForward.Ask): to the claim it reached, as the role it was made as, and
// queued alone, its hold sent with the first ask. It covers the hold as the first ask did where the
// owner is the worker of the step the task stands at (mergeForwardHold): there the hold's worker is
// named by the owner's own claim (record.Issue.StepWorker), and everywhere else by its role's phases
// row, which names another agent, so the claim test the first ask's addressing made
// (addressMergeForward) is made here on the hold's recipient alone, with nothing addressed. An ask
// of the owner at that head still queued does both already (record.Store.MergeForwardAskPending),
// so none is queued behind it; mergeForward relies on that, since it queues its ask before it
// writes the record that opens the merge forward. A tree that lingers has every role suspended and
// is asked nothing, as mergeForward asks it nothing.
func (e *Engine) askOwnerAgain(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest) error {
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	asked := pr.MergeForward
	if pending, err := e.store.MergeForwardAskPending(ctx, tx, issue.Key, asked.Agent, asked.Head); err != nil || pending {
		return err
	}
	hold, held, err := mergeForwardHold(issue, *asked)
	if err != nil {
		return err
	}
	var covers record.MergeForwardStep
	if held && hold.claim == asked.Agent {
		covers = record.MergeForwardHold
	}
	e.logOnCommit(ctx, "workflow: a merge forward asked of the owner opens again, and the owner is asked again", "issue", issue.Key,
		"tree", issue.Tree, "owner", asked.Agent, "head", asked.Head, "base", asked.Base)
	return e.enqueue(ctx, tx, issue.Key, asked.Ask(pr.Number, covers))
}

// mergeForwardWriteBack queues the supervise slot_credential that writes the slot of issue's owner
// back to the App of its own role (record.OwnerSlotWriteBack), for the end of a merge forward asked
// of the owner. An issue whose key makes no claim token has no owner's slot.
func (e *Engine) mergeForwardWriteBack(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	row, ok := record.OwnerSlotWriteBack(issue)
	if !ok {
		return nil
	}
	e.logOnCommit(ctx, "workflow: a merge forward asked of the owner ended, and its slot is written back to its own App", "issue", issue.Key,
		"tree", issue.Tree, "owner", row.Claim)
	return e.enqueue(ctx, tx, issue.Key, row)
}
