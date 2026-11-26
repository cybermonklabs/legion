package record

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// valid says whether p is a place a task can stand at.
func (p TaskPlace) valid() bool {
	switch p.Kind {
	case PlaceStep:
		return p.Position >= 1
	case "", PlaceRetro, PlaceMerge:
		return p.Position == 0
	}
	return false
}

// AssignTaskBranch gives task of the plan of issue at generation its GitHub branch and answers it:
// the branch its row holds already, else the first name not given, in this order: the issue's own
// branch (claim.IssueBranch), then each give of the task's slug (claim.TaskBranchOfGive over
// plan.TaskSlug), legion/<issue>.<slug> first. GitHub keeps a branch and its commits, so a name
// given is never given to another task of the issue, at any generation: a name is given where any
// generation of the issue gave it (plan_branches, which neither a later removal of the task nor a
// carry of the plan from its start deletes, and where RecordBranchGiven keeps the branch of each
// pull request of an earlier run whose row a run started again deletes, a run before tasks had
// branches of their own among them), and where another task of the generation holds it. Each
// branch another task of the generation holds is read by BranchOf: a task that a release before
// this one started works on the issue's own branch with no branch on its row, so the issue's
// branch counts as given while such a task stands. The one task of a plan of one task takes the
// issue's own branch whatever an earlier generation gave it to, as an issue always did, so an
// issue run again goes on on its branch and the open pull request a new generation keeps there
// (ClearGeneration); a later version that adds tasks to that plan gives them names no generation
// gave. The name is written on the task's row and among the names the generation gave. The
// generation's task rows are locked first, so two transactions that start two tasks of the issue
// at one time give their names one after the other. A task the plan does not hold is refused,
// naming it.
func (s *Postgres) AssignTaskBranch(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string) (string, error) {
	describe := fmt.Sprintf("give task %s of %s at generation %d its branch", task, issue, generation)
	rows, err := tx.Query(ctx, `select task, branch, state from plan_tasks where issue = $1 and generation = $2
		order by task for update`, issue, int64(generation))
	if err != nil {
		return "", fmt.Errorf("%s: %w", describe, err)
	}
	type taskRow struct {
		task, branch string
		state        TaskState
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (taskRow, error) {
		var r taskRow
		return r, row.Scan(&r.task, &r.branch, &r.state)
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", describe, err)
	}
	given := map[string]bool{}
	held := false
	for _, r := range tasks {
		switch {
		case r.task == task && r.branch != "":
			return r.branch, nil
		case r.task == task:
			held = true
		case r.state != TaskWaiting || r.branch != "":
			given[PlanTask{Branch: r.branch}.BranchOf(issue)] = true
		}
	}
	if !held {
		return "", fmt.Errorf("%s: the plan holds no such task", describe)
	}
	// own is the issue's branch where the plan holds this task alone, which no name an earlier
	// generation gave takes from it.
	own := ""
	if len(tasks) == 1 {
		own = claim.IssueBranch(issue)
	}
	names, err := tx.Query(ctx, `select branch from plan_branches where issue = $1 and (generation = $2 or branch <> $3)`,
		issue, int64(generation), own)
	if err != nil {
		return "", fmt.Errorf("%s: %w", describe, err)
	}
	gave, err := pgx.CollectRows(names, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("%s: %w", describe, err)
	}
	for _, name := range gave {
		given[name] = true
	}
	branch := claim.IssueBranch(issue)
	for give := 1; given[branch]; give++ {
		branch = claim.TaskBranchOfGive(issue, plan.TaskSlug(task), give)
	}
	if _, err := tx.Exec(ctx, `insert into plan_branches (issue, generation, branch) values ($1, $2, $3)`,
		issue, int64(generation), branch); err != nil {
		return "", fmt.Errorf("%s: %w", describe, err)
	}
	if _, err := tx.Exec(ctx, `update plan_tasks set branch = $4 where issue = $1 and generation = $2 and task = $3`,
		issue, int64(generation), task, branch); err != nil {
		return "", fmt.Errorf("%s: %w", describe, err)
	}
	noteRecordWrite(ctx)
	return branch, nil
}

// RecordBranchGiven records branch among the names generation of issue gave (plan_branches), so
// AssignTaskBranch never gives it to a task of the issue again: the branch of a pull request of an
// earlier run whose row the first step of a run started again deletes (the workflow's
// closeEarlierPullRequests), which GitHub keeps with its commits while no row of the record names
// it any longer. A name recorded already is left as it is.
func (s *Postgres) RecordBranchGiven(ctx context.Context, tx pgx.Tx, issue string, generation uint64, branch string) error {
	if _, err := tx.Exec(ctx, `insert into plan_branches (issue, generation, branch) values ($1, $2, $3) on conflict do nothing`,
		issue, int64(generation), branch); err != nil {
		return fmt.Errorf("record %s among the names %s gave at generation %d: %w", branch, issue, generation, err)
	}
	return nil
}

// SetJoinConflicts writes needs as the needs of task of the plan of issue at generation whose merge
// GitHub refused as a conflict, into the branch the join of its needs is made on, in place of what
// the row held: each task_branch row of the task joins its needs again, so the newest join is the
// one its first step repairs. A task the plan does not hold is refused, naming it.
func (s *Postgres) SetJoinConflicts(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, needs []string) error {
	describe := fmt.Sprintf("write the needs whose join conflicted of task %s of %s at generation %d", task, issue, generation)
	if needs == nil {
		needs = []string{}
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set join_conflicts = $4 where issue = $1 and generation = $2 and task = $3`,
		issue, int64(generation), task, needs)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task", describe)
	}
	noteRecordWrite(ctx)
	return nil
}

// SetJoinUnmerged writes whether GitHub refused as a conflict the merge of the join branch of task
// of the plan of issue at generation into the task's branch, in place of what the row held, as
// SetJoinConflicts writes the needs that conflicted: each task_branch row of the task joins again,
// so the newest join is the one its first step merges by hand. A task the plan does not hold is
// refused, naming it.
func (s *Postgres) SetJoinUnmerged(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, unmerged bool) error {
	tag, err := tx.Exec(ctx, `update plan_tasks set join_unmerged = $4 where issue = $1 and generation = $2 and task = $3`,
		issue, int64(generation), task, unmerged)
	if err != nil {
		return fmt.Errorf("write whether the join branch of task %s of %s at generation %d merged into its branch: %w", task, issue, generation, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("write whether the join branch of task %s of %s at generation %d merged into its branch: the plan holds no such task", task, issue, generation)
	}
	noteRecordWrite(ctx)
	return nil
}

// SetStartReason writes reason as why the step task of the plan of issue at generation stands at
// was started: the start again of a stopped task at its first step, and a move back to an earlier
// step (the report of the step that sent the work back, then the move's reason), each in the
// transaction that moves the task there, after the move (MoveTask, which clears it at every move),
// so the step that starts after a wait for its join or its agent is told why and counts as a move
// back. A task that is not running, and a task the plan does not hold, are refused, naming it.
func (s *Postgres) SetStartReason(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task, reason string) error {
	tag, err := tx.Exec(ctx, `update plan_tasks set start_reason = $4 where issue = $1 and generation = $2 and task = $3 and state = 'running'`,
		issue, int64(generation), task, reason)
	if err != nil {
		return fmt.Errorf("write why the step of task %s of %s at generation %d was started: %w", task, issue, generation, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("write why the step of task %s of %s at generation %d was started: the plan holds no such task that is running", task, issue, generation)
	}
	noteRecordWrite(ctx)
	return nil
}

// DeleteTasks deletes each of tasks from the plan of issue at generation, with its steps: what a
// later version that removes a task writes. Only a waiting or a stopped task is removed, since a
// later version changes nothing that runs or ran; a name that is neither, or that the plan does not
// hold, is refused, naming it.
func (s *Postgres) DeleteTasks(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tasks []string) error {
	describe := fmt.Sprintf("delete tasks %v of the plan of %s at generation %d", tasks, issue, generation)
	rows, err := tx.Query(ctx, `delete from plan_tasks where issue = $1 and generation = $2 and task = any($3)
		and state in ('waiting', 'stopped') returning task`, issue, int64(generation), tasks)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	deleted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	var kept []string
	for _, task := range tasks {
		if !slices.Contains(deleted, task) && !slices.Contains(kept, task) {
			kept = append(kept, task)
		}
	}
	if len(kept) > 0 {
		return fmt.Errorf("%s: the plan holds no waiting or stopped task %v", describe, kept)
	}
	if len(deleted) > 0 {
		noteRecordWrite(ctx)
	}
	return nil
}

// MoveTask writes where task stands in the plan of issue at generation, and its state: the one
// write of the walk's moveTask. A stop is StopTask's, which records why, and a hold HoldTask's, so
// the states stopped and held are refused here. A move takes a stop's reason away, but a move to
// left (TaskLeft): a stopped task whose issue leaves the workflow keeps why it was stopped, the one
// record of why it was not done. A move ends a hold's reason, why the step the task stood at was
// started (SetStartReason, which a start again and a move back write after their move) and the wait
// of the step the task stood at, whose start it no longer waits to deliver. A move into running
// from waiting or stopped is a start of the task, whose step waits for the join of its needs
// (StepWaitJoin) since this transaction's moment, until the start that follows the move (the
// workflow's Engine.start) ends that wait or the end of the join does; every other move writes no
// wait. A place no task can stand at, a step the task does not have, a task the plan does not hold
// and a task that left the workflow, which nothing moves again, are refused.
func (s *Postgres) MoveTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, place TaskPlace, state TaskState) error {
	describe := fmt.Sprintf("move task %s of %s at generation %d to %s %d, %s", task, issue, generation, place.Kind, place.Position, state)
	switch state {
	case TaskWaiting, TaskRunning, TaskDone, TaskLeft:
	case TaskStopped:
		return fmt.Errorf("%s: a stop is StopTask's, which records why", describe)
	case TaskHeld:
		return fmt.Errorf("%s: a hold is HoldTask's, which records why", describe)
	default:
		return fmt.Errorf("%s: no task has that state", describe)
	}
	if !place.valid() {
		return fmt.Errorf("%s: no task stands at that place", describe)
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set running_kind = $4, position = $5, state = $6,
		stopped_reason = case when $6 = 'left' then stopped_reason else '' end,
		hold_reason = '', start_reason = '',
		step_wait = case when $6 = 'running' and state in ('waiting', 'stopped') then 'join' else '' end,
		step_wait_since = case when $6 = 'running' and state in ('waiting', 'stopped') then now() end
		where issue = $1 and generation = $2 and task = $3 and state <> 'left'
		and ($4 <> 'step' or exists (select 1 from plan_steps where plan_steps.issue = $1 and plan_steps.generation = $2
			and plan_steps.task = $3 and plan_steps.position = $5))`,
		issue, int64(generation), task, place.Kind, place.Position, state)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task that is in the workflow, or the task no such step", describe)
	}
	noteRecordWrite(ctx)
	return nil
}

// StopTask writes task of the plan of issue at generation stopped for reason, at the place it
// stands, which a later start of the task reads. A stop of a held task ends its hold, and with it
// the hold's reason. A stop ends the wait of the step at the task's place (StepWait) in the same
// write: a stopped task's step waits for nothing, and only a later start of the task, which writes
// its own wait (MoveTask), starts it again. A stop without its reason, of a task that is done or
// left the workflow, or of a task the plan does not hold, is refused.
func (s *Postgres) StopTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task, reason string) error {
	describe := fmt.Sprintf("stop task %s of %s at generation %d", task, issue, generation)
	if reason == "" {
		return fmt.Errorf("%s: a stop records why", describe)
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set state = 'stopped', stopped_reason = $4, hold_reason = '',
		step_wait = '', step_wait_since = null
		where issue = $1 and generation = $2 and task = $3 and state not in ('done', 'left')`, issue, int64(generation), task, reason)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task that is not done and in the workflow", describe)
	}
	noteRecordWrite(ctx)
	return nil
}

// HoldTask writes task of the plan of issue at generation held for reason, at the place it stands:
// "" for the failed claim of the agent at its running step, HoldEscalated where its owner escalated
// that hold, and HoldForChildren where the task that merges waits before its retro for the issues
// beneath its issue. A held task takes the new reason, as the failed claim its owner escalates
// does. A hold takes only a step that was delivered, which waits for nothing (SetStepWait), so a
// held step never waits. The reason of a stop is StopTask's, and any other reason, a task that is
// not running or held, and a task the plan does not hold are refused, naming it.
func (s *Postgres) HoldTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, reason HoldReason) error {
	describe := fmt.Sprintf("hold task %s of %s at generation %d for %q", task, issue, generation, reason)
	switch reason {
	case "", HoldEscalated, HoldForChildren:
	case HoldTaskStopped:
		return fmt.Errorf("%s: a stop is StopTask's, which records why", describe)
	default:
		return fmt.Errorf("%s: a task is held for no such reason", describe)
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set state = 'held', hold_reason = $4
		where issue = $1 and generation = $2 and task = $3 and state in ('running', 'held')`, issue, int64(generation), task, string(reason))
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task that is running or held", describe)
	}
	noteRecordWrite(ctx)
	return nil
}

// ReleaseTask writes the held task of the plan of issue at generation running at the place it
// stands, with no hold reason: the owner's retry, which starts its step again there. A task that
// is not held, and a task the plan does not hold, are refused, naming it.
func (s *Postgres) ReleaseTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string) error {
	describe := fmt.Sprintf("release task %s of %s at generation %d", task, issue, generation)
	tag, err := tx.Exec(ctx, `update plan_tasks set state = 'running', hold_reason = ''
		where issue = $1 and generation = $2 and task = $3 and state = 'held'`, issue, int64(generation), task)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task that is held", describe)
	}
	noteRecordWrite(ctx)
	return nil
}

// SetStepWait writes what the step at the place of the running task of the plan of issue at
// generation waits for, since at: StepWaitJoin or StepWaitAgent with the moment its wait began,
// and StepWaitNone with a nil at, which ends a wait. A step that waits is one whose start was not
// delivered, which only a running task's place holds. A wait with no moment, no wait with one, a
// wait of no kind, a task that is not running and a task the plan does not hold are refused,
// naming it.
func (s *Postgres) SetStepWait(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, wait StepWait, at *time.Time) error {
	describe := fmt.Sprintf("write what the step of task %s of %s at generation %d waits for, %q", task, issue, generation, wait)
	switch {
	case wait != StepWaitNone && wait != StepWaitJoin && wait != StepWaitAgent:
		return fmt.Errorf("%s: a step waits for no such thing", describe)
	case (wait == StepWaitNone) != (at == nil):
		return fmt.Errorf("%s: a wait names the moment it began, and no wait names one", describe)
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set step_wait = $4, step_wait_since = $5
		where issue = $1 and generation = $2 and task = $3 and state = 'running'`, issue, int64(generation), task, string(wait), at)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task that is running", describe)
	}
	noteRecordWrite(ctx)
	return nil
}
