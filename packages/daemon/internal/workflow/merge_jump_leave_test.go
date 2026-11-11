package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A jump mark is the controller's instruction about one issue's next merge (record.MergeJump),
// and an issue that has left the workflow submits no merge: every departure takes the mark with
// it, so that a re-admission weeks later does not spend a decision made about a merge that never
// happened. One clear does that for all of them, standing in leave ahead of every branch, and
// the marked issue leaves here the two ways that reach it from the furthest apart: a close, which
// runs leave to its end, and a waiting root taken out while the operator runs a tree under its
// key, which returns through leaveUnderOperator before leave's other writes.
//
// The mark of an issue that did not leave is the control: it stands, since each departure clears
// the mark of the issue departing and of no other.
func TestLeavingTheWorkflowClearsTheJumpMark(t *testing.T) {
	for _, tc := range []struct {
		name string
		// operator seeds the root's tree lifecycle under the operator's authority, so the root's
		// departure is leaveUnderOperator's.
		operator bool
		// marked is the issue the controller marked and leave is then called for.
		marked string
		leave  func(ctx context.Context, tx pgx.Tx, engine *Engine, root, child record.Issue) error
	}{
		{
			name:   "a marked child closed as done",
			marked: "LEGION-209",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, _, child record.Issue) error {
				return engine.leave(ctx, tx, child, "done")
			},
		},
		{
			name:   "a marked root closed as done",
			marked: "LEGION-208",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "done")
			},
		},
		{
			name:     "a marked root taken out under an operator",
			operator: true,
			marked:   "LEGION-208",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "backlog")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
			parent := root.Key
			child := record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "V", LastDispatchSeq: 1}
			// The control is a root of its own tree, which nothing here takes out of the workflow.
			other := record.Issue{Key: "LEGION-210", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-210", Project: "LEGION", Title: "another root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "W", LastDispatchSeq: 1}
			seedIssue(t, pool, root)
			seedIssue(t, pool, child)
			seedIssue(t, pool, other)
			seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: "implementer"})
			seedPhase(t, pool, record.PhaseRow{Issue: child.Key, Role: claim.RoleImplementer, Claim: "child-implementer"})
			if tc.operator {
				seedOperatorTree(t, pool, root.Tree)
			} else {
				seedTreeLifecycle(t, pool, root.Tree)
			}
			seedMergeJump(t, pool, tc.marked, "the release waits on this merge")
			seedMergeJump(t, pool, other.Key, "another issue's mark")
			engine := testEngine(config.DesignGateRootIssues, nil)

			applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				return tc.leave(ctx, tx, engine, root, child)
			})

			if mark := mergeJumpOf(t, pool, tc.marked); mark != nil {
				t.Fatalf("the jump mark of %s after it left the workflow = %+v, want none", tc.marked, *mark)
			}
			if mark := mergeJumpOf(t, pool, other.Key); mark == nil {
				t.Fatalf("the jump mark of %s is gone, want the mark of an issue that did not leave left where it was", other.Key)
			}
		})
	}
}

// seedMergeJump marks issue for the jump, as the controller's route does. The write lands only
// while the issue is still one the daemon runs (record.Store.SetMergeJump), and this seed runs
// before the departure under test, so the answer is read: a mark that was never written would
// let the leave's own clear pass for free.
func seedMergeJump(t *testing.T, pool *pgxpool.Pool, issue, reason string) {
	t.Helper()
	var wrote bool
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		wrote, err = record.NewStore().SetMergeJump(t.Context(), tx, record.MergeJump{
			Issue: issue, Reason: reason, Session: "ses_controller",
			SetAt: time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC),
		})
		return err
	})
	if !wrote {
		t.Fatalf("the jump mark of %s was not written, want it on an issue still in the workflow", issue)
	}
}

// mergeJumpOf is the jump mark the record holds for issue, nil when it holds none.
func mergeJumpOf(t *testing.T, pool *pgxpool.Pool, issue string) *record.MergeJump {
	t.Helper()
	var mark *record.MergeJump
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		mark, err = record.NewStore().MergeJumpOf(t.Context(), tx, issue)
		return err
	}); err != nil {
		t.Fatalf("read the jump mark of %s: %v", issue, err)
	}
	return mark
}
