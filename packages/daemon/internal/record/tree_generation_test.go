package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// runRoot and runMember are the tree of the tests of the tree's run: its root and a member at
// work in it.
const runRoot, runMember = "ACME-1", "ACME-2"

// runIssue is key standing in runRoot's tree at generation one, in the workflow.
func runIssue(key string) Issue {
	issue := Issue{Key: key, Tree: runRoot, Project: "ACME", Title: key, Phase: phase.Implementing, Generation: 1,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if key != runRoot {
		parent := runRoot
		issue.Parent = &parent
	}
	return issue
}

// runGenerationOf is the tree_generation the record holds for key, read from its row.
func runGenerationOf(t *testing.T, st *store.Store, key string) uint64 {
	t.Helper()
	var generation int64
	if err := st.Pool().QueryRow(context.Background(), `select tree_generation from issues where key = $1`, key).Scan(&generation); err != nil {
		t.Fatalf("read the tree generation of %s: %v", key, err)
	}
	return uint64(generation)
}

// A whole tree's rerun is admission's readmit: it writes the root at its next generation and then
// clears the generation of the whole tree (ClearTreeGeneration). The clear is what raises the
// tree's run, to the root's new generation, and a member's idle release queued in the run that
// ended then finishes without tearing its pod down, as it does today. The release is read before
// the rerun too, where it tears the pod down, so the fence is seen to read the release.
func TestAWholeTreeRerunRaisesTheTreeGeneration(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	root := runIssue(runRoot)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, root))
		must(t, records.PutIssue(ctx, tx, runIssue(runMember)))
	})
	if _, err := st.OpenTreeLifecycle(ctx, "acme", runRoot, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", runRoot, err)
	}
	// The member's idle release as ReleaseIdleIssue marks it, its rows 5 to 10, and the claim its
	// stops suspended.
	if _, err := st.Pool().Exec(ctx, `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, suspended, marked_at)
		values ('acme', $1, 1, 5, 6, 10, 0, 'architect', '{}', now())`, runMember); err != nil {
		t.Fatalf("mark the release of %s: %v", runMember, err)
	}
	token, err := claim.NewToken("acme", runMember, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "acme", Tree: runRoot, Issue: runMember, Role: claim.RoleArchitect,
		TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatal(err)
	}
	release := func() store.ReleaseDecision {
		t.Helper()
		decision, err := st.IssueRelease(ctx, "acme", store.IssueReleased{Issue: runMember, Tree: runRoot, IssueGeneration: 1, TreeGeneration: 1, Row: 10},
			OutOfWorkflow)
		if err != nil {
			t.Fatalf("decide the release of %s: %v", runMember, err)
		}
		return decision
	}
	if decision := release(); decision.Act != store.ReleaseActs {
		t.Fatalf("the member's release before the rerun = %s (%s), want it to tear the pod down", decision.Act, decision.Cause)
	}

	root.Generation = 2
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, root)) })
	if got := runGenerationOf(t, st, runRoot); got != 1 {
		t.Fatalf("the root's tree generation after its write at generation 2 = %d, want 1: only the clear of the whole tree raises it", got)
	}
	inTx(t, st, func(tx pgx.Tx) { must(t, records.ClearTreeGeneration(ctx, tx, runRoot, time.Now().UTC())) })
	if got := runGenerationOf(t, st, runRoot); got != 2 {
		t.Fatalf("the root's tree generation after the clear of its tree = %d, want 2, the root's new generation", got)
	}
	if decision := release(); decision.Act != store.ReleaseCancel || decision.Cause != store.ReleaseCauseUnauthorized {
		t.Fatalf("the member's release after the rerun = %s (%s), want it cancelled as unauthorized: its tree's run is over", decision.Act, decision.Cause)
	}
}

// A member handed over or re-admitted as an orphan becomes the root of a tree of its own: the write
// that moves it to that tree starts the tree's run at its own generation, and the tree it left
// keeps its own run. A later write of the new root in the same tree, a next change among them,
// leaves that run.
func TestARootAHandOverMadeStartsItsTreeGenerationAtItsOwn(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	member := runIssue(runMember)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, runIssue(runRoot)))
		must(t, records.PutIssue(ctx, tx, member))
	})

	member.Tree, member.Generation, member.HandedOverFrom = runMember, 3, runRoot
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, member)) })
	if got := runGenerationOf(t, st, runMember); got != 3 {
		t.Fatalf("the tree generation of %s as a root of its own at generation 3 = %d, want 3", runMember, got)
	}
	if got := runGenerationOf(t, st, runRoot); got != 1 {
		t.Fatalf("the tree generation of %s, which %s left = %d, want 1", runRoot, runMember, got)
	}

	member.Generation = 4
	inTx(t, st, func(tx pgx.Tx) { must(t, records.PutIssue(ctx, tx, member)) })
	if got := runGenerationOf(t, st, runMember); got != 3 {
		t.Fatalf("the tree generation of %s after its write at generation 4 in its own tree = %d, want 3", runMember, got)
	}
}
