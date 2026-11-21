package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The paths that queue the daemon's own close of an issue it records (record.PendingClose.Kind).
const (
	closeKindMerge = "merge"
	closeKindRoot  = "close_root"
)

// childrenNotDone is each Dispatch child of key that the copy shows at a status other than done,
// as "KEY (status)", in key order (record.Chains.Children). Dispatch closes a parent only once
// each of its children is done (R24 of dispatch://INFRA-3320), whatever stands in Legion's work,
// so a child filed and never released, one in the backlog and one in icebox each hold the close.
func childrenNotDone(chains *record.Chains, key string) []string {
	var open []string
	for _, child := range chains.Children(key) {
		if child.Status != "done" {
			open = append(open, fmt.Sprintf("%s (%s)", child.Key, child.Status))
		}
	}
	return open
}

// childrenNotDoneOf is childrenNotDone over the Dispatch chain of issue, read from issue alone
// (record.ReadChainsOf: its children are the walk's start's), so a close costs the same whatever
// the number of other issues at work.
func (e *Engine) childrenNotDoneOf(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]string, error) {
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return nil, err
	}
	return childrenNotDone(chains, issue.Key), nil
}

// childrenNotDoneRefusal is the refusal of a close the caller asked for while a Dispatch child of
// issue is not done: CHILDREN_NOT_DONE, naming each child with its status (R24 of
// dispatch://INFRA-3320), which close_root and close_child answer after R8's codes.
func childrenNotDoneRefusal(call, key string, open []string) intake.Result {
	return refused("CHILDREN_NOT_DONE", fmt.Sprintf("%s of %s is refused: Dispatch closes an issue only once each of its children is done, and these are not: %s. Close, park or run each one to done, and close %s again",
		call, key, strings.Join(open, ", "), key))
}

// writeClose queues the daemon's own close of issue (done, with reason posted first), decided by
// kind's path, and marks the record with it (record.PendingClose). Nothing else of the issue
// moves: it keeps its phase, its claims and its slot until Dispatch shows it done
// (closeLanded), since Dispatch may refuse the close (closeRefused). A close already queued at
// the run and the phase entry the issue stands at, and not refused, is on its way, so nothing
// more is queued; a refused one, or one queued at an earlier run or entry, is queued again.
func (e *Engine) writeClose(ctx context.Context, tx pgx.Tx, issue record.Issue, kind, reason string) error {
	fence, err := record.ReadFence(ctx, tx, e.store, issue.Key, false)
	if err != nil {
		return err
	}
	if pending := issue.PendingClose; pending != nil && !pending.Refused && !fence.Outlives(record.Fence{Generation: pending.Generation, Entry: pending.Entry}) {
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.CloseStatusWrite("done", issue.Status, reason, issue.Generation)); err != nil {
		return err
	}
	issue.PendingClose = &record.PendingClose{Generation: fence.Generation, Entry: fence.Entry, Kind: kind, Reason: reason}
	e.logOnCommit(ctx, "workflow: the daemon's close of an issue is queued", "issue", issue.Key, "kind", kind,
		"generation", fence.Generation, "entry", fence.Entry)
	return e.store.PutIssue(ctx, tx, issue)
}

// closeLanded is Dispatch showing done on an issue whose close the daemon queued
// (record.PendingClose), the echo of that close or a person's own: the issue ends its run. The
// merge's close and close_root move the record to phase done (decision 8 of
// dispatch://INFRA-4730), and the issue leaves as every done does (leave): its claims retire, after
// the production check a merged issue opens now, and its pod goes.
func (e *Engine) closeLanded(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	kind := issue.PendingClose.Kind
	from := issue.Phase
	trigger := kind
	if kind == closeKindMerge {
		// The merge step ends with the close, as the merge's transition ended it (nameMoveEnd).
		trigger = string(TriggerPullRequestMerged)
		if err := e.nameMoveEnd(ctx, issue, TriggerPullRequestMerged, phase.Done, record.PhaseRow{}); err != nil {
			return err
		}
	}
	issue.PendingClose, issue.Status = nil, "done"
	issue.Phase, issue.Hold, issue.Awaiting, issue.ChildrenWait = phase.Done, nil, nil, nil
	if err := e.store.PutIssue(ctx, tx, issue); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: phase changed", "issue", issue.Key, "tree", issue.Tree, "from", from, "to", phase.Done, "trigger", trigger, "status", "done")
	return e.leave(ctx, tx, issue, "done")
}

// closeRefused is Dispatch's refusal of the close the daemon queued for an issue
// (intake.CloseRefused), because a child of it was not done: the close changes nothing (R24 of
// dispatch://INFRA-3320). The issue stays in its phase with its claims and its slot, the mark
// stays (record.PendingClose, now refused), and the issue's architect is told the children
// Dispatch named; the decision pass writes the close again once the copy shows each child the
// issue then has done (WriteDueClose). A refusal of a close of an earlier run or phase entry,
// or of an issue whose close has landed since, changes nothing.
func (e *Engine) closeRefused(ctx context.Context, tx pgx.Tx, fact intake.CloseRefused) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	pending := issue.PendingClose
	if pending == nil || pending.Generation != fact.Generation || pending.Entry != fact.Entry {
		e.logOnCommit(ctx, "workflow: a refused close of an earlier run or phase entry changes nothing", "issue", issue.Key,
			"generation", fact.Generation, "entry", fact.Entry)
		return intake.Result{}, nil
	}
	pending.Refused = true
	if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
		return intake.Result{}, err
	}
	named := "a child of it"
	if len(fact.Children) > 0 {
		named = strings.Join(fact.Children, ", ")
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "message-refused", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("Dispatch refused the daemon's close of %s, since these children of it are not done: %s. %s stays in %s with its claims and its slot, and the daemon writes the close again once each child it then has is done. Close, park or run each one to done.",
			issue.Key, named, issue.Key, issue.Phase)})
}

// ClosePassCandidate says whether the decision pass asks WriteDueClose of issue: it carries a
// close the daemon queued (record.PendingClose), or it awaits its merge (awaitsMerge).
func ClosePassCandidate(issue record.Issue) bool {
	return issue.PendingClose != nil || awaitsMerge(issue)
}

// WriteDueClose is the decision pass's reading of the close issue owes, once the pass's copy shows
// each Dispatch child the issue has now done: a close Dispatch refused
// (record.PendingClose.Refused) is queued again (writeClose), at the run and the phase entry it
// was decided at and at no other, and a merged issue that waits in merging for its children
// queues its close (closeMerged), which catches the child the daemon records nothing of whose
// own done no departure reads. A child a person unlinked since holds the close no more, since the
// copy no longer shows it under the issue. A close on its way, one of an earlier run or phase
// entry and an issue out of Legion's work queue nothing. It answers whether it queued a close.
func (e *Engine) WriteDueClose(ctx context.Context, tx pgx.Tx, issue record.Issue, chains *record.Chains) (bool, error) {
	if !record.InLegionsWork(issue) || len(childrenNotDone(chains, issue.Key)) > 0 {
		return false, nil
	}
	if pending := issue.PendingClose; pending != nil {
		fence, err := record.ReadFence(ctx, tx, e.store, issue.Key, false)
		if err != nil {
			return false, err
		}
		if !fence.Outlives(record.Fence{Generation: pending.Generation, Entry: pending.Entry}) {
			if !pending.Refused {
				return false, nil
			}
			return true, e.writeClose(ctx, tx, issue, pending.Kind, pending.Reason)
		}
	}
	if !awaitsMerge(issue) {
		return false, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil || pr.State != record.PullRequestMerged {
		return false, err
	}
	waiting, err := e.closeMerged(ctx, tx, issue, pr)
	return err == nil && len(waiting) == 0, err
}
