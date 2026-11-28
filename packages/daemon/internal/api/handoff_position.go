package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// completionPosition is where issue key stands as the completion route reads it, in one repeatable-read
// snapshot: the issue record, its tree's generation (the root's) and whether the tree lingers, the
// task the issue runs (record.StoredPlan.Task; "" for an issue with no plan rows at its
// generation, which walks today's line), its pass — that task's count of returns to an earlier
// step, which an issue with no plan rows keeps on its implementer's row
// (record.ImplementerCounts), which tells each implementing, testing, and reviewing pass from the
// last — the handoff record the implementer last named for a file-backed phase
// (record.PhaseRow.LastHandoff), the phase entry the completing role's current phase is on
// (record.PhaseRow.Entry, 0 on a row no start has stamped) and whether the start that raised it
// moved the issue back (record.PhaseRow.EntryMovedBack), its pull request record, nil while none
// is recorded, its production check, nil while none was ever opened, and, for the implementer, the
// fix that runs beside the issue's step (runningFix), nil while none runs.
type completionPosition struct {
	issue          *record.Issue
	treeGeneration uint64
	lingers        bool
	task           string
	pass           int
	entry          int64
	entryMovedBack bool
	lastHandoff    string
	pullRequest    *record.PullRequest
	check          *record.ProductionCheck
	fix            *record.TaskFix
}

// handoffPosition reads key's position (completionPosition) for the completing role, whose phases
// row names the entry it was last raised to; an issue the workflow does not record is a position
// with no issue and no error. The pull request is its row alone (record.Store.PullRequestRow): the
// route reads its head and its state and writes nothing of it.
func (s *server) handoffPosition(ctx context.Context, key string, role claim.Role) (completionPosition, error) {
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
		completing := record.PhaseRowOf(rows, role)
		stored, err := s.records.Plan(ctx, tx, key, issue.Generation)
		if err != nil {
			return err
		}
		var task string
		counts := record.ImplementerCounts(rows)
		if stored != nil {
			running, err := stored.Task()
			if err != nil {
				return err
			}
			task, counts = running.Task, running.Counts
		}
		pr, err := s.records.PullRequestRow(ctx, tx, key)
		if err != nil {
			return err
		}
		check, err := s.records.ProductionCheck(ctx, tx, key)
		if err != nil {
			return err
		}
		// A fix runs the task's first implementation step again, so it is the implementer's work and
		// no other role's.
		var fix *record.TaskFix
		if role == claim.RoleImplementer {
			if fix, err = RunningFixOf(ctx, tx, s.records, issue, task); err != nil {
				return err
			}
		}
		position = completionPosition{issue: issue, treeGeneration: root.Generation, lingers: root.Lingers(), task: task, pass: counts.Pass,
			lastHandoff: record.PhaseRowOf(rows, claim.RoleImplementer).LastHandoff, entry: completing.Entry, entryMovedBack: completing.EntryMovedBack,
			pullRequest: pr, check: check, fix: fix}
		return nil
	})
	return position, err
}

// RunningFixOf is the fix of issue's running task that runs beside the issue's step, read in tx:
// the fix the issue record names (record.Issue.RunningFix), in state running with its entry raised
// on the implementer's phases row, which the task the fix's start queued carries. It is nil while
// none runs: a fix that still reads its failed job has raised no entry, so no task of it is the
// implementer's work yet, and an issue with no plan rows (task "") runs no fix. The completion
// route and the move back read the fix a claim works by it, and the projection the fix the state
// serves (Issue.Fix), so the three read one fix.
func RunningFixOf(ctx context.Context, tx pgx.Tx, records record.Store, issue *record.Issue, task string) (*record.TaskFix, error) {
	if issue.RunningFix == 0 || task == "" {
		return nil, nil
	}
	fix, err := records.RunningFix(ctx, tx, issue.Key, issue.Generation, task)
	if err != nil || fix == nil || fix.Fix != issue.RunningFix || fix.State != record.FixRunning || fix.Entry == 0 {
		return nil, err
	}
	return fix, nil
}
