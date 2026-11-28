package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
// moved the issue back (record.PhaseRow.EntryMovedBack), whether that role's row records its
// completion of its current phase (record.PhaseRow.Completed, which the owed route reads and the
// completion route does not), its pull request record, nil while none is recorded — the
// completion route judges the head a completion names against it, and the owed route reads it in
// merging, where a merged pull request is what finishes the phase (workflow.PhaseFinished) — and
// its production check, nil while none was ever opened.
type completionPosition struct {
	issue          *record.Issue
	treeGeneration uint64
	lingers        bool
	task           string
	pass           int
	entry          int64
	entryMovedBack bool
	completed      bool
	lastHandoff    string
	pullRequest    *record.PullRequest
	check          *record.ProductionCheck
}

// merged is whether the position's phase is merging and its pull request is merged: the fact
// that finishes merging, read by the rule workflow.PhaseFinished applies, since the merger
// merges and a merged root then stands in merging until the rest of its tree leaves.
func (p completionPosition) merged() bool {
	return p.issue != nil && p.issue.Phase == phase.Merging &&
		p.pullRequest != nil && p.pullRequest.State == record.PullRequestMerged
}

// handoffPosition reads key's position (completionPosition) for role, the completing role or the
// role asking what it owes, whose phases row names the entry it was last raised to and whether it
// has completed; an issue the workflow does not record is a position with no issue and no error.
// The pull request is its row alone (record.Store.PullRequestRow): each route reads its head and
// its state and writes nothing of it.
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
		position = completionPosition{issue: issue, treeGeneration: root.Generation, lingers: root.Lingers(), task: task, pass: counts.Pass,
			lastHandoff: record.PhaseRowOf(rows, claim.RoleImplementer).LastHandoff, entry: completing.Entry, entryMovedBack: completing.EntryMovedBack,
			completed: completing.Completed(), pullRequest: pr, check: check}
		return nil
	})
	return position, err
}
