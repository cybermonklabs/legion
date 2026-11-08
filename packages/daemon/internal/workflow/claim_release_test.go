package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// Dispatch releases an issue's claim when the issue closes and at no other departure, so a park —
// a root's tree lingering for backlog, icebox or triage, a root released for want of a slot, a
// child parked alone — leaves the claim of the session the daemon just stopped standing on the
// board. The one claim_release row each park queues is what clears it, and a close queues none:
// the close released it already. The row carries nothing of the holder, which it reads with the
// issue's own record when it runs.
//
// Each parking row asserts the whole set of rows the departure queued, not the release alone: the
// park suspends every role of what it parks, disarms its open pull requests, and queues the
// suspension and, for a root, the linger's close. That set is how the test shows the release was
// added to the park and replaced nothing.
func TestLeaveQueuesTheClaimRelease(t *testing.T) {
	// A root's park acts on its whole tree: six suspends, a disarm and an IssueSuspend for each
	// of its two members, and the one row that closes the linger.
	parkedTree := map[string]int{"supervise": 12, "auto_merge_disable": 2, "issue_suspend": 2, "linger_close": 1}
	// A child's park ends the child alone, and tells the tree's architect which child left.
	parkedChild := map[string]int{"supervise": 6, "auto_merge_disable": 1, "issue_suspend": 1, "notice": 1}
	for _, tc := range []struct {
		name string
		// lingers seeds a root whose tree already lingers, and operator one whose tree the
		// operator holds.
		lingers  bool
		operator bool
		leave    func(ctx context.Context, tx pgx.Tx, engine *Engine, root, child record.Issue) error
		// releases is the issue of each claim_release row the departure queued, oldest first.
		releases []string
		kinds    map[string]int
	}{
		{
			name: "a root moved to backlog",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "backlog")
			},
			releases: []string{"LEGION-208"},
			kinds:    withRelease(parkedTree),
		},
		{
			name: "a root moved to icebox",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "icebox")
			},
			releases: []string{"LEGION-208"},
			kinds:    withRelease(parkedTree),
		},
		{
			name: "a root moved to triage",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "triage")
			},
			releases: []string{"LEGION-208"},
			kinds:    withRelease(parkedTree),
		},
		{
			name: "a child moved to backlog",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, _, child record.Issue) error {
				return engine.leave(ctx, tx, child, "backlog")
			},
			releases: []string{"LEGION-209"},
			kinds:    withRelease(parkedChild),
		},
		{
			// The unslotted release leaves at the active status the root stands at, so the row is
			// the leave's own rather than any status test's.
			name: "a root released for want of an admission slot",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.LeaveUnslotted(ctx, tx, root)
			},
			releases: []string{"LEGION-208"},
			kinds:    withRelease(parkedTree),
		},
		{
			name: "a root closed as done",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "done")
			},
			kinds: parkedTree,
		},
		{
			name: "a child closed as done",
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, _, child record.Issue) error {
				return engine.leave(ctx, tx, child, "done")
			},
			kinds: parkedChild,
		},
		{
			// The first leave of this tree queued its release; a root that already lingers leaves
			// no second time and queues nothing at all.
			name:    "a root whose tree already lingers",
			lingers: true,
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "backlog")
			},
			kinds: map[string]int{},
		},
		{
			// The claims of a tree the operator runs are the operator's to release with the rest
			// of its cleanup, as its pod and its workspaces are.
			name:     "a root whose tree the operator holds",
			operator: true,
			leave: func(ctx context.Context, tx pgx.Tx, engine *Engine, root, _ record.Issue) error {
				return engine.leave(ctx, tx, root, "backlog")
			},
			kinds: map[string]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
			if tc.lingers {
				until := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
				root.LingerUntil = &until
			}
			parent := root.Key
			child := record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "V", LastDispatchSeq: 1}
			seedIssue(t, pool, root)
			seedIssue(t, pool, child)
			seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleImplementer, Claim: "implementer"})
			seedPhase(t, pool, record.PhaseRow{Issue: child.Key, Role: claim.RoleImplementer, Claim: "child-implementer"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-42", Failing: []string{}})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: child.Key, Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
			if tc.operator {
				seedOperatorTree(t, pool, root.Tree)
			} else {
				seedTreeLifecycle(t, pool, root.Tree)
			}
			engine := testEngine(config.DesignGateRootIssues, nil)

			applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				return tc.leave(ctx, tx, engine, root, child)
			})

			if got := claimReleases(t, pool); !sameStrings(got, tc.releases) {
				t.Fatalf("claim_release rows = %v, want %v", got, tc.releases)
			}
			if got := queuedKinds(t, pool); !sameCounts(got, tc.kinds) {
				t.Fatalf("queued rows = %v, want %v", got, tc.kinds)
			}
		})
	}
}

// withRelease is the rows a park queues with the one claim_release row of its own.
func withRelease(park map[string]int) map[string]int {
	kinds := map[string]int{"claim_release": 1}
	for kind, count := range park {
		kinds[kind] = count
	}
	return kinds
}

// applyInTx runs one of the engine's departures in a transaction of its own and commits it, with
// the commit scope the engine's deferred logging needs (intake.WithCommitHooks), as a fact's
// application gives it.
func applyInTx(t *testing.T, pool *pgxpool.Pool, apply func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(t.Context())
	ctx, committed := intake.WithCommitHooks(t.Context())
	if err := apply(ctx, tx); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed()
}

// seedOperatorTree opens key's tree lifecycle under the operator's authority, as an operator root
// spawn under the root's key does: the workflow records nothing that acts on such a tree's claims.
func seedOperatorTree(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			return err
		}
		_, err = record.NewStore().OpenTreeLifecycle(t.Context(), tx, project, key, treelifecycle.AuthorityOperator)
		return err
	})
}

// claimReleases is the issue of each queued claim_release row, oldest first. Each row's payload is
// read too: it holds nothing of the claim's holder, which the row reads when it runs.
func claimReleases(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select issue, payload::text from outbox where kind = 'claim_release' order by id")
	if err != nil {
		t.Fatalf("read claim releases: %v", err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue, payload string
		if err := rows.Scan(&issue, &payload); err != nil {
			t.Fatalf("scan claim release: %v", err)
		}
		if payload != "{}" {
			t.Fatalf("claim release of %s carries %s, want an empty payload", issue, payload)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate claim releases: %v", err)
	}
	return issues
}

// queuedKinds counts every queued row by kind, with the relaunch half of a phase-worker start left
// out as the package's other outbox assertions leave it out (assertOutboxCount).
func queuedKinds(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select kind from outbox
		where not (kind = 'supervise' and payload->>'op' = 'start' and coalesce(payload->>'task', '') = '')`)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan outbox kind: %v", err)
		}
		counts[kind]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox kinds: %v", err)
	}
	return counts
}
