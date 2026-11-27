package api

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// completionPosition is where issue key stands as the completion route reads it, in one repeatable-read
// snapshot: the issue record, its tree's generation (the root's) and whether the tree lingers, the
// task of its plan whose step completes (completingTask; "" for an issue with no plan rows at its
// generation, which walks today's line), its pass — that task's count of returns to an earlier
// step, which an issue with no plan rows keeps on its implementer's row
// (record.ImplementerCounts), which tells each implementing, testing, and reviewing pass from the
// last — the handoff record the implementer last named for a file-backed phase
// (record.PhaseRow.LastHandoff), the phase entry the completing role's current phase is on
// (record.PhaseRow.Entry, 0 on a row no start has stamped) and whether the start that raised it
// moved the issue back (record.PhaseRow.EntryMovedBack), its pull request record, nil while none
// is recorded, and its production check opened at the time a check report names
// (HandoffCompleteRequest.CheckOpenedAt), nil where no check was opened then.
//
// Of a plan of any count of tasks, one among them, the task is named in ofTask, and the position is
// that task's: issue stands where the issue stands for that task alone (standingOfTask), the pass
// is that task's, the pull request is that task's own row (record.Store.PullRequestRowOfTask),
// which for the one task of a plan of one is the issue's merge pull request, branch its branch
// (record.PlanTask.BranchOf) and base the branch its pull request is against
// (record.StoredPlan.BaseOf), the default branch for the task that merges. noStep says the
// completion is of no task of the plan, and of no phase of it. Of an issue with no plan rows,
// ofTask is "", branch the issue's own and the pull request the issue's own row, as before tasks,
// and no base is read ("").
type completionPosition struct {
	issue          *record.Issue
	treeGeneration uint64
	lingers        bool
	task           string
	ofTask         string
	noStep         bool
	branch         string
	base           string
	pass           int
	entry          int64
	entryMovedBack bool
	lastHandoff    string
	pullRequest    *record.PullRequest
	check          *record.ProductionCheck
}

// handoffPosition reads key's position (completionPosition) for the completing role and the claim
// that completes, whose phases row names the entry it was last raised to, and the check opened at
// checkOpenedAt, the time a check report names (the issue's newest for the zero time).
// An issue the workflow does not record is a position with no issue and no error. The pull
// request is the row of the completion's task (record.Store.PullRequestRowOfTask): the route reads
// its head and its state and writes nothing of it.
func (s *server) handoffPosition(ctx context.Context, key string, role claim.Role, completing claim.Token, checkOpenedAt time.Time) (completionPosition, error) {
	var position completionPosition
	err := store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := s.records.Issue(ctx, tx, key)
		if err != nil || issue == nil {
			return err
		}
		root := issue
		if !claim.IsTreeRoot(issue.Key, issue.Tree) {
			if root, err = s.records.Issue(ctx, tx, issue.Tree); err != nil {
				return err
			}
			if root == nil {
				return fmt.Errorf("the tree root %s of %s is not recorded", issue.Tree, issue.Key)
			}
		}
		rows, err := s.records.Phases(ctx, tx, key)
		if err != nil {
			return err
		}
		row := record.PhaseRowOf(rows, role)
		stored, err := s.records.Plan(ctx, tx, key, issue.Generation)
		if err != nil {
			return err
		}
		position = completionPosition{issue: issue, treeGeneration: root.Generation, lingers: root.Lingers(), branch: claim.IssueBranch(key),
			lastHandoff: record.PhaseRowOf(rows, claim.RoleImplementer).LastHandoff, entry: row.Entry, entryMovedBack: row.EntryMovedBack}
		counts := record.ImplementerCounts(rows)
		if stored != nil {
			task, act, ok, err := s.completingTask(ctx, tx, *issue, stored, rows, completing)
			switch {
			case err != nil:
				return err
			case !ok:
				position.noStep = true
			default:
				standing := *issue
				standing.Phase, standing.Hold = standingOfTask(*issue, task, act)
				position.issue, position.task, position.ofTask, position.branch = &standing, task.Task, task.Task, task.BranchOf(key)
				if position.base, _, err = stored.BaseOf(*issue, task.Task); err != nil {
					return err
				}
				counts = task.Counts
			}
		}
		position.pass = counts.Pass
		if !position.noStep {
			if position.pullRequest, err = s.records.PullRequestRowOfTask(ctx, tx, key, position.task); err != nil {
				return err
			}
		}
		if position.check, err = s.records.ProductionCheckOf(ctx, tx, key, checkOpenedAt); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return completionPosition{}, err
	}
	return position, nil
}

// completingTask is the task of stored, the plan of issue's run, whose step the claim completing
// completes, and the act that task stands at, nil for a task at no act: the task of the running
// step the claim's agent works (record.Issue.StepOfAgent), which is that act; for an agent that
// works none, the task of the step the latest phases row of rows that names the claim runs
// (latestStepTask), which a completion reports that arrives after its step ended or before its
// start was delivered; and for a claim that no row names a step for, the one task of the plan that
// has a merge (record.StoredPlan.OnlyTaskThatMerges), whose work a row raised before the plan is
// of, and none where several tasks have one. The act of a task whose step the agent does not work
// is read from the act of each task (record.Store.TaskActs).
// ok is false where none of these is a task of stored: the completion is of no step of the plan.
func (s *server) completingTask(ctx context.Context, tx pgx.Tx, issue record.Issue, stored *record.StoredPlan, rows []record.PhaseRow, completing claim.Token) (record.PlanTask, *record.RunningStep, bool, error) {
	_, agent, _ := completing.Cut()
	if step, running := issue.StepOfAgent(agent); running {
		task, ok := stored.TaskOf(step.Task)
		return task, &step, ok, nil
	}
	id := latestStepTask(rows, completing)
	if id == "" {
		id, _ = stored.OnlyTaskThatMerges()
	}
	task, ok := stored.TaskOf(id)
	if !ok {
		return record.PlanTask{}, nil, false, nil
	}
	acts, err := s.records.TaskActs(ctx, tx, issue)
	if err != nil {
		return record.PlanTask{}, nil, false, err
	}
	if at := slices.IndexFunc(acts, func(act record.RunningStep) bool { return act.Task == id }); at >= 0 {
		return task, &acts[at], true, nil
	}
	return task, nil, true, nil
}

// latestStepTask is the task of the step the latest row of rows that names token runs
// (record.PhaseRow.StepTask), by the moment its entry was raised and then by its entry: "" where no
// row names token, and where the latest one names no step, as a row raised before the plan does.
func latestStepTask(rows []record.PhaseRow, token claim.Token) string {
	var latest *record.PhaseRow
	for at := range rows {
		row := &rows[at]
		if row.Claim == token && (latest == nil || row.EntryRaisedAt.After(latest.EntryRaisedAt) ||
			row.EntryRaisedAt.Equal(latest.EntryRaisedAt) && row.Entry > latest.Entry) {
			latest = row
		}
	}
	if latest == nil {
		return ""
	}
	return latest.StepTask
}

// standingOfTask is where issue stands for task alone, which stands at act (nil for a task at no
// act), as the workflow reads a copy of the issue for one task: at the phase of its act, held from
// it for a held or a stopped task (record.TaskStanding), and held with the issue's own hold for a
// running task whose issue is held from that phase, a hold the release before wrote on the issue
// alone. A task at no act, a done one and one that left stand where the issue stands: the phase of
// a run off every act of its tasks (planning, admitted, awaiting_person, done) is the issue's, and
// so is the phase of the issue whose plan holds that task alone.
func standingOfTask(issue record.Issue, task record.PlanTask, act *record.RunningStep) (phase.Phase, *record.Hold) {
	switch {
	case act == nil || task.State == record.TaskDone || task.State == record.TaskLeft:
		return issue.Phase, issue.Hold
	case task.State == record.TaskRunning && issue.Hold != nil && issue.Hold.From == act.Phase:
		return issue.Phase, issue.Hold
	}
	return record.TaskStanding(act.Phase, task.State, task.HoldReason)
}
