package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// sameHold says whether a and b are one hold: none, or one from the same phase for the same reason.
func sameHold(a, b *record.Hold) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// needsDone says whether each task that task needs is done in stored, the plan of its run. A need
// the plan does not hold is never done: a later version that removes a task another needs is
// refused (plan.Validate).
func needsDone(stored *record.StoredPlan, task record.PlanTask) bool {
	for _, need := range task.Needs {
		if row, ok := stored.TaskOf(need); !ok || row.State != record.TaskDone {
			return false
		}
	}
	return true
}

// partsStart says whether the parts of task, a task of stored, may start: it runs at its parts,
// where its sub-owner acts, and its step waits for nothing, so the join of its needs has ended
// (record.StepWaitJoin) and each part starts from the joined work of those needs. A task that
// waits, is stopped or held, waits for its join, or stands at its report holds its parts back.
func partsStart(stored *record.StoredPlan, task string) bool {
	row, ok := stored.TaskOf(task)
	return ok && row.State == record.TaskRunning && row.Place.Kind == record.PlaceParts && row.StepWait == record.StepWaitNone
}

// mayStart says whether task, a task of stored that waits or is stopped, may start now: each task
// it needs is done (needsDone), and, for a part, its task runs at its parts and waits for nothing
// (partsStart). It is the one rule of a task's start, which each door into running asks: the start
// of each task that waits (startReady), and the start again of a stopped task (startAtFirstStep),
// which a later version and a person's todo both reach. A task that may not start waits at no act,
// and startReady starts it once it may.
func mayStart(stored *record.StoredPlan, task record.PlanTask) bool {
	return needsDone(stored, task) && (task.PartOf == "" || partsStart(stored, task.PartOf))
}

// startReadyNow starts each task of the issue keyed key that may start (startReady), on the issue
// as the store holds it in tx now: what a caller that moved a task, and holds an older copy of the
// issue, runs once the move may let other tasks start. An issue the store no longer holds starts
// nothing.
func (e *Engine) startReadyNow(ctx context.Context, tx pgx.Tx, key string) error {
	now, err := e.store.Issue(ctx, tx, key)
	if err != nil || now == nil {
		return err
	}
	_, err = e.startReady(ctx, tx, *now)
	return err
}

// lastToFinish says whether the end of s's task ends its issue: each other task of the plan of s's
// run, as the store holds it in tx, is done or left the workflow (record.TaskLeft). That holds for a
// plan of one task, and for a run with no plan rows, whose one walk is the issue's own. It is the one
// statement of "this task's end ends the issue", which two readers ask before they wait for the
// issues beneath: the close of a merge (closeMerged) and the hold before a retro (beneathRetro).
// Every other task's merge, and its retro, waits for nothing beneath.
func (e *Engine) lastToFinish(ctx context.Context, tx pgx.Tx, s TaskScope) (bool, error) {
	if s.plan == nil {
		return true, nil
	}
	stored, err := e.store.Plan(ctx, tx, s.issue.Key, s.issue.Generation)
	if err != nil || stored == nil {
		return true, err
	}
	return !slices.ContainsFunc(stored.Tasks, func(row record.PlanTask) bool {
		return row.Task != s.walk.Task && row.State != record.TaskDone && row.State != record.TaskLeft
	}), nil
}

// tasksWaitingOn is each task of stored, the plan of a run, that needs task, itself or through
// other tasks, and is neither done nor left, in task id order: the tasks a stop of task holds,
// while each other task goes on. None for no plan.
func tasksWaitingOn(stored *record.StoredPlan, task string) []string {
	if stored == nil {
		return nil
	}
	needing := map[string]bool{task: true}
	for grew := true; grew; {
		grew = false
		for _, row := range stored.Tasks {
			if needing[row.Task] {
				continue
			}
			if slices.ContainsFunc(row.Needs, func(need string) bool { return needing[need] }) {
				needing[row.Task], grew = true, true
			}
		}
	}
	var waiting []string
	for _, row := range stored.Tasks {
		if row.Task != task && needing[row.Task] && row.State != record.TaskDone && row.State != record.TaskLeft {
			waiting = append(waiting, row.Task)
		}
	}
	return waiting
}

// startReady is the one place tasks of a plan of several start, in the caller's transaction: it
// moves each task that waits and each of whose needs is done, in task id order, to the place its
// start puts it (Walk.startPlace): its first step, or the parts of a task with a sub-owner, where
// its sub-owner acts. The task runs there (moveTask, which writes that its step waits for its
// join); startReady writes the issue (putScope) and starts the step's role through Engine.start,
// the start of every step, which gives the task its branch and joins its needs (startTaskCopy). At
// the parts no step's role works the task (Walk.RoleAt), so that start only joins its needs. A
// part of a task (record.PlanTask.PartOf) starts only while that task runs at its parts and waits
// for nothing (partsStart), so a part never starts before the join of its task's needs ended: the
// end of that join starts it (taskJoined, through startAfterWait). Where a task started at its parts with no wait, its
// parts that wait start in the same call. It answers the issue as written. A plan of one task, or
// none, starts nothing here: its one task starts at the gate's opening as before (transition),
// and so the issue is answered as given. It runs at the first step of the work (gateOpened), at
// each move of a task to done (transition), and, through startReadyNow, once at each other door
// whose fact may let a task start: a version of the plan taken after the work started, after each
// stopped task it starts again (afterSeveral); a person's todo on a held root, after each task it
// releases or starts again (releaseHeldRoot); the architect's and the operator's release of a hold
// (retryOrEscalate, restartPhase); and the start of a step at its parts that waited, at the end of
// its join or at its join queued again (startAfterWait, which taskJoined and joinAgain pass). So a
// task that comes to run at its parts with no wait, by any of them, lets its parts start. No setting
// bounds how many tasks run at one time: the plan bounds it with each task's needs and parts.
func (e *Engine) startReady(ctx context.Context, tx pgx.Tx, issue record.Issue) (record.Issue, error) {
	written := issue
	for {
		stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
		if err != nil || stored == nil || len(stored.Tasks) <= 1 {
			return written, err
		}
		var ready []string
		for _, row := range stored.Tasks {
			if row.State == record.TaskWaiting && mayStart(stored, row) {
				ready = append(ready, row.Task)
			}
		}
		// atParts says whether a task of ready started at its parts, whose own parts can start
		// once it waits for nothing: the plan is read again for them.
		atParts := false
		for _, id := range ready {
			s, err := e.ScopeOf(ctx, tx, written, id)
			if err != nil {
				return record.Issue{}, err
			}
			// The step's text names the pull request of its own task (taskPullRequest), none for a
			// task that has not opened one.
			pr, err := e.taskPullRequest(ctx, tx, s)
			if err != nil {
				return record.Issue{}, err
			}
			to, first, ok := s.walk.startPlace()
			if !ok {
				return record.Issue{}, fmt.Errorf("start task %s of %s: its line %v holds no first step", id, issue.Key, s.walk.Line)
			}
			if err := e.moveTask(ctx, tx, &s, to, first, record.TaskRunning); err != nil {
				return record.Issue{}, err
			}
			s.issue.Hold = nil
			if written, err = e.putScope(ctx, tx, &s); err != nil {
				return record.Issue{}, err
			}
			if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
				return record.Issue{}, err
			}
			e.logOnCommit(ctx, "workflow: a task whose needs are done starts", "issue", issue.Key, "tree", issue.Tree, "task", id,
				"needs", s.walk.Needs, "to", to, "place", placeWords(first), "issuePhase", written.Phase)
			role := s.walk.RoleAt(first)
			if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
				return record.Issue{}, err
			}
			view := s.issue
			if err := e.start(ctx, tx, s, role, func(at Standing) string {
				return task(view, at, record.PhaseRow{}, pr, "")
			}, false); err != nil {
				return record.Issue{}, err
			}
			atParts = atParts || first.Kind == record.PlaceParts
		}
		if !atParts {
			return written, nil
		}
	}
}
