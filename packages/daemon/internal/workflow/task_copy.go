package workflow

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// startTaskCopy gives s's task its branch at a start of the task, in the transaction
// that starts it (record.Store.AssignTaskBranch: once, the issue's own branch for the first task of
// the generation to start where no earlier generation gave it, or the one task of a plan of one,
// else the first give of the task's slug no generation of the issue gave), and answers whether it
// queued the join of the task's needs. A task whose branch is not the
// issue's own, or that needs other tasks, gets one task_branch row (record.TaskBranch), which makes
// its branch on GitHub at the joined work of its needs and ends with the fact that starts its step
// (intake.TaskJoined). The task that holds the issue's own branch and needs none queues nothing:
// the issue_branch row its first step queues makes that branch (gateOpened). Its one caller is the
// start of every step (Engine.start), at each start of a task, the first and a start again. A task
// the plan does not hold is refused, naming it. The plan it reads after the give is s's plan from
// then on, so the start's text reads the task's branch and the base of its pull request from it
// (standingPart) and reads no plan again.
func (e *Engine) startTaskCopy(ctx context.Context, tx pgx.Tx, s *TaskScope) (bool, error) {
	issue, task := s.issue, s.walk.Task
	branch, err := e.store.AssignTaskBranch(ctx, tx, issue.Key, issue.Generation, task)
	if err != nil {
		return false, err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return false, err
	}
	// AssignTaskBranch refused a task the plan does not hold, and holds the generation's task rows
	// locked, each of which names a version of the plan (its foreign key): the plan holds the task,
	// and one that does not is an error.
	row, ok := stored.TaskOf(task)
	if !ok {
		return false, fmt.Errorf("start the copy of task %s of %s: its plan holds no such task", task, issue.Key)
	}
	s.plan = stored
	if branch == claim.IssueBranch(issue.Key) && len(row.Needs) == 0 {
		return false, nil
	}
	return true, e.enqueue(ctx, tx, issue.Key, record.TaskBranch{Generation: issue.Generation, Task: task})
}

// taskJoined applies the end of a task's join (intake.TaskJoined) in the fact's transaction: where
// the step of the task waits for this join (the task running at the fact's generation, waiting for
// its join, on the branch and with the needs the join read), it writes the needs whose join GitHub
// refused as a conflict (record.Store.SetJoinConflicts) and whether its join branch merged into its
// branch (record.Store.SetJoinUnmerged), ends the wait and starts the step at the task's place
// through Engine.start, the start of every step, so at no commit does the step wait for nothing with
// nothing of it started: start raises its entry, or writes that it waits for its agent where that
// agent runs a step of another task. Its task is told where its work stands and its own pull
// request, as the start after the wait for an agent tells it (handOn). Any other end of a join
// changes nothing: one of a generation the issue has left, of a task its plan no longer holds or
// whose step waits for no join, and one onto another branch or from other needs, a row that a stop
// and a later version of the task left behind, whose start again queued a join of its own.
func (e *Engine) taskJoined(ctx context.Context, tx pgx.Tx, fact intake.TaskJoined) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Generation != fact.Generation {
		return intake.Result{}, err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return intake.Result{}, err
	}
	joined, ok := stored.TaskOf(fact.Task)
	if !ok {
		return intake.Result{}, nil
	}
	if joined.State != record.TaskRunning || joined.StepWait != record.StepWaitJoin || joined.BranchOf(issue.Key) != fact.Branch ||
		!slices.Equal(joined.Needs, fact.Needs) {
		e.logOnCommit(ctx, "workflow: the end of a join the task's step does not wait for changes nothing", "issue", issue.Key, "task", fact.Task,
			"state", joined.State, "wait", joined.StepWait, "branch", joined.BranchOf(issue.Key), "needs", joined.Needs,
			"joined_branch", fact.Branch, "joined_needs", fact.Needs)
		return intake.Result{}, nil
	}
	row, err := e.store.TaskBranchRow(ctx, tx, issue.Key, fact.Generation, fact.Task)
	if err != nil {
		return intake.Result{}, err
	}
	if row == 0 || row != fact.Row {
		e.logOnCommit(ctx, "workflow: the end of an earlier task join leaves the current wait standing", "issue", issue.Key, "task", fact.Task, "row", fact.Row, "current", row)
		return intake.Result{}, nil
	}
	if err := e.store.SetJoinConflicts(ctx, tx, issue.Key, issue.Generation, fact.Task, fact.Conflicts); err != nil {
		return intake.Result{}, err
	}
	if err := e.store.SetJoinUnmerged(ctx, tx, issue.Key, issue.Generation, fact.Task, fact.Unmerged); err != nil {
		return intake.Result{}, err
	}
	if err := e.store.SetStepWait(ctx, tx, issue.Key, issue.Generation, fact.Task, record.StepWaitNone, nil); err != nil {
		return intake.Result{}, err
	}
	s, err := e.ScopeOf(ctx, tx, *issue, fact.Task)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.startAfterWait(ctx, tx, s, "workflow: a task's join ended, and its step starts",
		"branch", fact.Branch, "needs", fact.Needs, "conflicts", fact.Conflicts, "unmerged", fact.Unmerged)
}

// stepOfStart is the running step start starts on issue (record.Issue.Running): the step of the
// agent its claim names, or, for a start that names none, of the agent of the claim the row then
// reaches, worker, the claim of the worker on the start's role's step (record.WorkerClaim). An
// agent runs at most one step, and a step that waits runs none, so a step of another task that
// waits for the same agent is never the answer. False where the claim names no agent of the issue
// or its agent runs no step.
func stepOfStart(issue record.Issue, start record.SuperviseRequest, worker claim.Token) (record.RunningStep, bool) {
	token := start.Claim
	if token == "" {
		token = worker
	}
	if _, agent, ok := token.Cut(); ok {
		return issue.StepOfAgent(agent)
	}
	return record.RunningStep{}, false
}

// startBranch is the branch start names (record.SuperviseRequest.Branch): the branch of the task of
// the step it starts (stepOfStart, record.PlanTask.BranchOf), and "" where that branch is the
// issue's own and where the issue has no plan rows at its generation, so each such start reads as
// every start did before tasks had branches of their own. rows is the issue's phases rows. A start
// that names no claim reads the claim of its role's worker only where a task of the plan holds a
// branch other than the issue's own: a plan whose every task holds the issue's branch names none at
// any start, so its starts derive no claim.
//
// "" is the issue's own branch, never an unknown one. A start that serves no step names it too, as
// before: one that names no phase (an architect's, a production check's look, a wake), and the
// architect's, whose step, the owner's acceptance, works no task's working copy. A phase worker's
// start whose step the plan does not run, where a task works on a branch of its own, is refused,
// naming its claim and its phase: its branch is unknown, and the issue's own would be the branch of
// another task, whose working copy its agent would adopt. So is a running step of a task the plan
// does not hold, which the record's reading of a running step (record.RunningOf) never answers.
func startBranch(ctx context.Context, tx pgx.Tx, store record.Store, issue record.Issue, start record.SuperviseRequest, rows []record.PhaseRow) (string, error) {
	stored, err := store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return "", err
	}
	own := claim.IssueBranch(issue.Key)
	if !slices.ContainsFunc(stored.Tasks, func(row record.PlanTask) bool { return row.BranchOf(issue.Key) != own }) {
		return "", nil
	}
	var worker claim.Token
	if start.Claim == "" {
		project, err := claim.ProjectToken(issue.Project)
		if err != nil {
			return "", err
		}
		if worker, err = record.WorkerClaim(project, issue.Key, start.Role, rows); err != nil {
			return "", err
		}
	}
	step, ok := stepOfStart(issue, start, worker)
	if !ok {
		if start.Phase == "" || start.Role == claim.RoleArchitect {
			return "", nil
		}
		return "", fmt.Errorf("the start of the %s of %s in %s: its claim %s runs no step of the plan, whose tasks work on branches of their own, so the branch of its step is unknown",
			start.Role, issue.Key, start.Phase, cmp.Or(start.Claim, worker))
	}
	task, ok := stored.TaskOf(step.Task)
	if !ok {
		return "", fmt.Errorf("the start of the %s of %s in %s: its claim %s runs a step of task %s, which the plan does not hold, so the branch of its step is unknown",
			start.Role, issue.Key, start.Phase, cmp.Or(start.Claim, worker), step.Task)
	}
	if branch := task.BranchOf(issue.Key); branch != own {
		return branch, nil
	}
	return "", nil
}
