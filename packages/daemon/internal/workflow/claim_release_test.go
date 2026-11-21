package workflow

import (
	"context"
	"testing"

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
// a root's or a child's move to backlog, icebox or triage, a root released for want of a slot —
// leaves the claim of the session the daemon just stopped standing on the board. The one
// claim_release row each park queues is what clears it, and a close queues none: the close
// released it already. The row carries nothing of the holder, which it reads with the issue's own
// record when it runs.
//
// Each row asserts the whole set of rows the departure queued, not the release alone: the leave
// stops every role of the issue that leaves and queues its suspension, and stops nothing above or
// below it (R20 of dispatch://INFRA-3320), so no linger and no second member's rows. That set is
// how the test shows the release was added to the leave and replaced nothing. No row disarms a
// pull request, since the daemon changes no merge setting of any (dispatch://INFRA-714 R2).
func TestLeaveQueuesTheClaimRelease(t *testing.T) {
	// A root's leave ends the root alone: six stops and its IssueSuspend.
	parkedTree := map[string]int{"supervise": 6, "issue_suspend": 1}
	// A child's leave ends the child alone, and tells the architect above it (here the tree's)
	// which child left.
	parkedChild := map[string]int{"supervise": 6, "issue_suspend": 1, "notice": 1}
	for _, tc := range []struct {
		name string
		// operator seeds a root whose tree the operator holds.
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

			applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
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

// applyInTx runs one of engine's departures as a fact applies it (factTx), after the boot's open
// of each run no start counted (bootBefore), which comes before any fact the daemon applies.
func applyInTx(t *testing.T, pool *pgxpool.Pool, engine *Engine, apply func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	bootBefore(t, pool, engine)
	factTx(t, pool, engine, apply)
}

// factTx runs apply in a transaction of its own and commits it, with the commit scope the
// engine's deferred logging needs (intake.WithCommitHooks) and the settle of the runs its writes
// end (Engine.OpenFact, Engine.SettleFact), as a fact's application gives them, and keeps what it
// left for the next boot pass's check (factLeft). It runs no boot pass before it: a test that
// writes what the store held before the daemon booted calls it alone.
func factTx(t *testing.T, pool *pgxpool.Pool, engine *Engine, apply func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(t.Context())
	ctx, committed := intake.WithCommitHooks(t.Context())
	ctx = engine.OpenFact(ctx)
	if err := apply(ctx, tx); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if err := engine.SettleFact(ctx, tx); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed()
	factLeft(t, pool)
}

// seedOperatorTree opens key's tree lifecycle under the operator's authority, as an operator root
// spawn under the root's key does, and the issue lifecycle of each issue recorded in that tree
// under it too: an operator's hold of a tree is a hold of each issue of it (R22 of
// dispatch://INFRA-3320), and the workflow records nothing that acts on the claims of an issue the
// operator holds (record.OperatorHolds). The tree's issues are recorded before it is seeded.
func seedOperatorTree(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			return err
		}
		records := record.NewStore()
		if _, err := records.OpenTreeLifecycle(t.Context(), tx, project, key, treelifecycle.AuthorityOperator); err != nil {
			return err
		}
		rows, err := tx.Query(t.Context(), `select key from issues where tree = $1 order by key`, key)
		if err != nil {
			return err
		}
		issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, issue := range issues {
			if _, err := records.OpenIssueLifecycle(t.Context(), tx, project, issue, treelifecycle.AuthorityOperator); err != nil {
				return err
			}
		}
		return nil
	})
}

// claimReleases is the issue of each queued claim_release row, oldest first. Each row's payload is
// read too: it holds nothing of the claim's holder, which the row reads when it runs, and nothing
// but the run and the phase entry it was queued at (record.Fence).
func claimReleases(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select issue, (payload - 'generation' - 'entry')::text from outbox where kind = 'claim_release' order by id")
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
			t.Fatalf("claim release of %s carries %s beside its fence, want nothing more", issue, payload)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate claim releases: %v", err)
	}
	return issues
}

// queuedKinds counts every queued row by kind, with no start passed over. In this package a phase
// worker's start is one row and carries its task (StartOrOwe); a child's own architect's plain
// start carries none (gateOpened, reviveArchitect), and the task-less starts admission brings back
// are written in internal/admit, never here. A step record is left out: the tests of the step sites
// (step_sites_test.go, step_sites_linger_test.go) read those rows exactly (stepWrites), so every
// row of any other kind is still counted here.
func queuedKinds(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select kind from outbox where kind <> $1`, string(record.OutboxKindStepRecord))
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
