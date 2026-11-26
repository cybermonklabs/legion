package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
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

// startReady is the one place a task of a plan starts at its first step, whatever the plan's count
// of tasks, in the caller's transaction, on stored, the plan of issue's run as the caller read it
// (nil for none, which starts nothing). It moves each task that waits and each of whose needs is
// done, in task id order, to its first step, running (moveTask, which writes that the step waits for
// its join), writes the issue (putScope), and starts the step's role through Engine.start, the start
// of every step, which gives the task its branch and joins its needs (startTaskCopy). It answers the
// issue as written. Each task's scope is read on stored as the writes before it left it
// (TaskScope.plan, which putScope keeps current), so a ready start reads the plan only where the
// start gives the task its branch and where the outbox queues the start (startBranch).
//
// A move into its first step does what any move into a step does (transition): a first step whose
// role the deployment no longer defines starts no role, and the task stops where it stands
// (stopLostRole); a first step of the owner's acceptance starts its step under the agent it names
// (startNamedStep); and the merge forward follows the start (mergeForwardOnEntry). At the gate's
// opening (trigger TriggerGateOpened) the run's plan node is written after the first task's
// status, so the rows read status, node, start for every plan, as the one task of a plan of one
// always wrote them. The journal names the task only where the plan holds a choice of tasks
// (textNamesTask): a plan of one keeps the line of its move out of admitted. It runs at the gate's
// opening (gateOpened), at each move of a task to done (transition), and after each version of the
// plan taken once the work started (afterVersion). No setting bounds how many tasks run at one
// time: the plan bounds it with each task's needs.
func (e *Engine) startReady(ctx context.Context, tx pgx.Tx, issue record.Issue, stored *record.StoredPlan, trigger TriggerKind) (record.Issue, error) {
	node := trigger == TriggerGateOpened
	if stored == nil {
		return issue, nil
	}
	var ready []string
	for _, row := range stored.Tasks {
		if row.State == record.TaskWaiting && needsDone(stored, row) {
			ready = append(ready, row.Task)
		}
	}
	written := issue
	for _, id := range ready {
		s, ok := scopeOn(written, stored, id)
		if !ok {
			return record.Issue{}, fmt.Errorf("start task %s of %s: its plan holds no such task", id, issue.Key)
		}
		steps := s.walk.steps()
		if len(steps) == 0 {
			return record.Issue{}, fmt.Errorf("start task %s of %s: its line %v holds no first step", id, issue.Key, s.walk.Line)
		}
		first := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
		if lost, ok := s.walk.lostAt(first, e.cfg.Deployment); ok {
			refusal, err := e.stopLostRole(ctx, tx, s, lost, stepStopped)
			if err == nil && refusal != nil {
				err = fmt.Errorf("stop task %s of %s at its lost role: %s", id, issue.Key, refusal.Message)
			}
			if err != nil {
				return record.Issue{}, err
			}
			// The stop wrote the issue and the task: the next task reads both again.
			now, err := e.store.Issue(ctx, tx, issue.Key)
			if err != nil {
				return record.Issue{}, err
			}
			if now == nil {
				return record.Issue{}, fmt.Errorf("start the tasks of %s: the issue is no longer recorded", issue.Key)
			}
			if stored, err = e.store.Plan(ctx, tx, issue.Key, issue.Generation); err != nil {
				return record.Issue{}, err
			}
			written = *now
			continue
		}
		// The step's text names the pull request of its own task (taskPullRequest), none for a task
		// that has not opened one, read once as the task stands before its start, at the gate's
		// opening too: no task of a plan of several holds a pull request on the issue's branch before
		// its first start, and the task of a plan of one, which takes that branch, holds the one
		// there (record.StoredPlan.TasksOfBranch). The start names that row, and the merge forward
		// that follows the start reads it, with no read of their own.
		pr, err := e.taskPullRequest(ctx, tx, s)
		if err != nil {
			return record.Issue{}, err
		}
		from := s.issue.Phase
		if err := e.moveTask(ctx, tx, &s, steps[0], first, record.TaskRunning); err != nil {
			return record.Issue{}, err
		}
		s.issue.Hold = nil
		if written, err = e.putScope(ctx, tx, &s); err != nil {
			return record.Issue{}, err
		}
		stored = s.plan
		if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
			return record.Issue{}, err
		}
		if !textNamesTask(s) {
			e.logOnCommit(ctx, "workflow: phase changed", "issue", issue.Key, "tree", issue.Tree, "from", from, "to", steps[0], "trigger", trigger, "status", written.Status)
		} else {
			e.logOnCommit(ctx, "workflow: a task whose needs are done starts at its first step", "issue", issue.Key, "tree", issue.Tree, "task", id,
				"needs", s.walk.Needs, "to", steps[0], "issuePhase", written.Phase)
		}
		// The node holds each task of the plan before any of them starts, so a step record or a
		// need of any task names a task the node holds.
		if node {
			if err := e.writePlanOn(ctx, tx, issue, stored); err != nil {
				return record.Issue{}, err
			}
			node = false
		}
		role := s.walk.RoleAt(first)
		if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
			return record.Issue{}, err
		}
		view := s.issue
		if err := e.start(ctx, tx, s, role, readPull(pr), func(at Standing) string {
			return task(view, at, record.PhaseRow{}, "")
		}, false); err != nil {
			return record.Issue{}, err
		}
		if steps[0] == phase.Accepting {
			if step, ok := standingStep(s.walk, s.issue); ok {
				if err := e.startNamedStep(ctx, tx, s, step); err != nil {
					return record.Issue{}, err
				}
			}
		}
		if err := e.mergeForwardOnEntry(ctx, tx, s, pr); err != nil {
			return record.Issue{}, err
		}
	}
	// A gate's opening that starts no task still writes the node of the plan the work walks.
	if node {
		if err := e.writePlanOn(ctx, tx, issue, stored); err != nil {
			return record.Issue{}, err
		}
	}
	return written, nil
}
