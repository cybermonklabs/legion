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

// wayNone is the way of a fix a stop, a leave or a new generation ended (record.TaskFix.Way): it
// takes none of R7a's three, and no later end of a step takes one for it (afterFix, besideFix).
const wayNone = 4

// dropFixWay is the one act of a stop of a task, a leave of its issue and a new generation of its
// run on the task's fix: the latest fix of w's task of issue whose way is not taken (Way 0), in
// whatever state, takes no way (wayNone) in one write. A fix that reads its job or runs ends
// stopped at the task's head (endFix), so a later read of its job starts nothing and the task's
// next failure may start a fix of its own; where it ran, its claim gets the step_stop the running
// step of a stop gets, said as said ("" for a leave or a new generation, whose claims their own
// acts stop). A fix that ended with its way pending keeps its result and its end. A fix whose way
// is taken is left as it is.
//
// At a stop that marks a fix so, the owner is told by one task-stopped notice of the fix, with no
// StoppedBy, so it reaches an owner that made the stop as well: the fix ended with the stop, and
// where the work beside it stands (besideWorkSentence). A stop deletes no branch beside a fix, since
// a todo on the root starts the task again and that work can be the only copy. A leave and a new
// generation defer the deletion of every branch beside the task's fixes at that generation until
// every old-generation start of that task ended (factRuns.deleteAfterOldRuns), whatever the latest
// fix's way. The seam besideDeleteAsBefore deletes the branches beside the fix a stop marks too.
func (e *Engine) dropFixWay(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, said string) error {
	if !w.Stored {
		return nil
	}
	if said == "" {
		if err := e.queueBesideDelete(ctx, tx, issue, w.Task, 0); err != nil {
			return err
		}
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.Way != 0 || (e.wayMarkRunningOnly && fix.State != record.FixRunning) {
		return err
	}
	fix.Way = wayNone
	if fix.State == record.FixEnded {
		if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
			return err
		}
		return e.fixStopped(ctx, tx, issue, *fix, said)
	}
	ran := fix.State == record.FixRunning
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	_, head := taskHead(pr)
	if err := e.endFix(ctx, tx, issue, w, fix, StepEnd{Result: ResultStopped}, head); err != nil {
		return err
	}
	if ran && said != "" {
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "step_stop", Tree: issue.Tree, Role: claim.RoleImplementer,
			Generation: issue.Generation, Reason: said}); err != nil {
			return err
		}
	}
	return e.fixStopped(ctx, tx, issue, *fix, said)
}

// fixStopped tells the owner of the fix a stop of its task, said, marked as taking no way
// (dropFixWay): one task-stopped notice of the implementer, with no StoppedBy, so it reaches the
// owner even where the owner made the stop, naming the fix, that it ended with the stop, and where
// the commits of the fix and of each step beside it stand (besideWorkSentence), which the stop keeps.
// A leave and a new generation (said "") tell nothing here. The seam besideDeleteAsBefore deletes
// the branches beside the fix at the stop.
func (e *Engine) fixStopped(ctx context.Context, tx pgx.Tx, issue record.Issue, fix record.TaskFix, said string) error {
	if said == "" {
		return nil
	}
	if e.besideDeleteAsBefore {
		if err := e.queueBesideDelete(ctx, tx, issue, fix.Task, fix.Fix); err != nil {
			return err
		}
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "task-stopped", Role: claim.RoleImplementer, Phase: phase.Implementing,
		Reason: fmt.Sprintf("Fix %d of task %s ended with the stop.", fix.Fix, fix.Task) + besideWorkSentence(issue.Key, issue.Generation, fix.Task, fix.Fix)})
}

// dropRunFix is dropFixWay on the walk of issue's run as the store holds it.
func (e *Engine) dropRunFix(ctx context.Context, tx pgx.Tx, issue record.Issue, said string) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.dropFixWay(ctx, tx, issue, w, said)
}

// fixClaimFailed takes the failure of the implementer's claim while the fix the issue record names
// runs (record.Issue.RunningFix), and answers whether it did: the fix ends stopped with the
// failure (TaskFix.ClaimFailed), at the task's head as it stands, since only the fix pushes while
// it runs, and its end decides what runs next by the three ways (endFix, afterFix). The issue is
// not held, since the step beside the fix goes on, and no second fix starts for the check at that
// head. The owner is told by one worker-died notice naming the fix, its check and head, the
// failure, what runs next, and the move back that starts the implementer again.
func (e *Engine) fixClaimFailed(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.ClaimFailed) (bool, error) {
	if fact.Role != claim.RoleImplementer || issue.RunningFix == 0 {
		return false, nil
	}
	w, fix, err := e.fixNamed(ctx, tx, issue, issue.RunningFix)
	if err != nil || fix == nil {
		return false, err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	_, head := taskHead(pr)
	failure := deathReason(fact, false)
	fix.ClaimFailed = failure
	if err := e.endFix(ctx, tx, issue, w, fix, StepEnd{Result: ResultStopped}, head); err != nil {
		return false, err
	}
	ended, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || ended == nil {
		return false, err
	}
	next := "The step beside it goes on, and what runs next is decided when it ends."
	switch ended.Way {
	case wayReturn:
		next = fmt.Sprintf("The task returns to step %d.", ended.ReturnTo)
	case wayAfterFix:
		next = fmt.Sprintf("The steps after the fix run again at %s.", head)
	case wayOn:
		next = "Each step keeps its result."
	}
	return true, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "worker-died", Role: fact.Role, Phase: issue.Phase, Failure: noticeFailure(fact),
		Reason: fmt.Sprintf("The implementer's claim failed while it ran fix %d of task %s (check %s at %s): %s. "+
			"The fix ended, and Legion starts no second fix for this check at this head.%s %s "+
			"Where the check still fails at the task's end you are told; send the task back with request_backward_move to start the implementer again.",
			fix.Fix, fix.Task, fix.Check, fix.Head, failure, besideWorkSentence(issue.Key, issue.Generation, fix.Task, fix.Fix), next)})
}
