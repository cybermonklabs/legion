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
// step, which an issue with no plan rows keeps on its implementer's phases row
// (record.ImplementerCounts), which tells each implementing, testing, and reviewing pass from the
// last — the place the task stands at and its steps, the issue's phases rows, the handoff record
// the implementer last named for a file-backed phase (record.PhaseRow.LastHandoff), its pull
// request record, nil while none is recorded, and its production check, nil while none was ever
// opened. The row a completion is held to is read from these once the entry its claim serves is
// known (run).
type completionPosition struct {
	issue          *record.Issue
	treeGeneration uint64
	lingers        bool
	task           string
	pass           int
	at             record.TaskPlace
	steps          []record.PlanStep
	rows           []record.PhaseRow
	lastHandoff    string
	pullRequest    *record.PullRequest
	check          *record.ProductionCheck
}

// completingRun is the run a completion of a role is of, read from its position: that run's row
// (record.CompletionRow), whose entry, move-back mark and claim the entry fence reads
// (intake.EntryFence), the highest entry another row of that role holds, another step's or the
// implementer's retro row (record.OtherStepsEntry), the claim of the run the served entry is of
// (record.ServedClaim, served), and the place of its step where the task names the role at two
// places (0 otherwise), which the completion's key names.
type completingRun struct {
	row             record.PhaseRow
	otherStepsEntry int64
	served          claim.Token
	place           int
}

// run is the run role's completion serving entry is of (record.CompletionRow), as the workflow
// reads it: where role works the place the task stands at (works), its row there, so a role that
// stands at two steps is held to the entry of the step it works and a turn of its other step is
// told apart; otherwise the row of the entry it serves, so a late report of either of its steps is
// held to that step's run and keyed by that step's place.
func (p completionPosition) run(role claim.Role, entry int64, works bool) completingRun {
	row := record.CompletionRow(p.rows, role, entry, works, p.task, p.at, p.steps)
	run := completingRun{row: row, otherStepsEntry: record.OtherStepsEntry(p.rows, row), served: record.ServedClaim(p.rows, role, entry)}
	if record.StandsTwice(role, p.steps) {
		run.place = record.RunPlace(row, p.at, p.steps)
	}
	return run
}

// handoffPosition reads key's position (completionPosition); an issue the workflow does not record
// is a position with no issue and no error. The pull request is its row alone
// (record.Store.PullRequestRow): the route reads its head and its state and writes nothing of it.
func (s *server) handoffPosition(ctx context.Context, key string) (completionPosition, error) {
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
		stored, err := s.records.Plan(ctx, tx, key, issue.Generation)
		if err != nil {
			return err
		}
		var task string
		var at record.TaskPlace
		var steps []record.PlanStep
		counts := record.ImplementerCounts(rows)
		if stored != nil {
			running, err := stored.Task()
			if err != nil {
				return err
			}
			task, counts, at, steps = running.Task, running.Counts, running.Place, stored.Steps[running.Task]
		}
		pr, err := s.records.PullRequestRow(ctx, tx, key)
		if err != nil {
			return err
		}
		check, err := s.records.ProductionCheck(ctx, tx, key)
		if err != nil {
			return err
		}
		position = completionPosition{issue: issue, treeGeneration: root.Generation, lingers: root.Lingers(), task: task, pass: counts.Pass, at: at, steps: steps,
			rows: rows, lastHandoff: record.PhaseRowAt(rows, claim.RoleImplementer, task, at, steps).LastHandoff, pullRequest: pr, check: check}
		return nil
	})
	return position, err
}
