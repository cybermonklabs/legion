package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The finishing tree (seedFinishingTree): a root in merging, holding a slot, and under it, each a
// child on Dispatch's chain, an issue in every place a run can stand.
const (
	finishedRoot = "LEGION-1"
	planning     = "LEGION-2" // mid-run in its first phases
	heldTesting  = "LEGION-3" // held from testing
	inRetro      = "LEGION-4" // in retro, past an approved review
	awaiting     = "LEGION-5" // in merging, not merged yet
	inAccepting  = "LEGION-6" // waiting on its architect's acceptance
	finished     = "LEGION-7" // done
	parked       = "LEGION-8" // stopped by a person, in the backlog
	unstarted    = "LEGION-9" // still admitted, waiting for its first phase
)

// seedFinishingTree records the finishing tree, each record with its row of the copy of Dispatch's
// issues (putCopyTx), and answers the pool, an admission with a cap of 3 and the engine.
func seedFinishingTree(t *testing.T) (*pgxpool.Pool, *Admission, intake.Handler) {
	t.Helper()
	pool := migratedPool(t)
	admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		ReviewRoundCap: 3, ProductionCheckRole: claim.RoleTester, Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, finishedRoot, "A")
	parent := finishedRoot
	child := func(key string, at phase.Phase, status string) record.Issue {
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: finishedRoot, Project: testProject,
			Title: key, Parent: &parent, Phase: at, Generation: 1, Status: status, DispatchStatus: status, Rank: "B", LastDispatchSeq: 3}
	}
	held := child(heldTesting, phase.Held, "testing")
	held.Hold = &record.Hold{From: phase.Testing}
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		root, err := records.Issue(ctx, tx, finishedRoot)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		root.Phase = phase.Merging
		if err := records.PutIssue(ctx, tx, *root); err != nil {
			t.Fatalf("seed: %v", err)
		}
		for _, put := range []error{
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: finishedRoot, Role: claim.RoleMerger, Claim: "root-merger", HandoffCommit: "head-1", Summary: "READY #1 at head-1"}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: finishedRoot, Repo: "acme/widgets", Number: 1, Branch: "legion/LEGION-1", HeadSHA: "head-1", Generation: 1, State: record.PullRequestOpen}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
		for _, issue := range []record.Issue{
			child(planning, phase.Planning, "in_progress"),
			held,
			child(inRetro, phase.Retro, "retro"),
			child(awaiting, phase.Merging, "retro"),
			child(inAccepting, phase.Accepting, "needs_review"),
			child(finished, phase.Done, "done"),
			child(parked, phase.Planning, "backlog"),
			child(unstarted, phase.Admitted, "todo"),
		} {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				t.Fatalf("seed %s: %v", issue.Key, err)
			}
			putCopyTx(t, tx, issue)
		}
	})
	return pool, admission, engine
}

// roleStarts is every start of role on issue the outbox holds, in the order it was queued.
func roleStarts(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == issue && request.Op == "start" && request.Role == role {
			starts = append(starts, request)
		}
	}
	return starts
}

// phaseStarts is how many starts of role on issue at phase at the outbox holds.
func phaseStarts(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role, at phase.Phase) int {
	t.Helper()
	starts := 0
	for _, start := range roleStarts(t, pool, issue, role) {
		if start.Phase == at {
			starts++
		}
	}
	return starts
}

// architectStarts is how many starts of key's own architect the outbox holds.
func architectStarts(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	return len(roleStarts(t, pool, key, claim.RoleArchitect))
}

// seedOpenTree seeds root holding an admission slot with its design gate approved at its latest
// version, so a todo child under it starts planning at once.
func seedOpenTree(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	seedSlotted(t, pool, root, "A")
	approved := 1
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	})
}

// queuedRoot is a root recorded waiting for its first start: its own tree, admitted at generation
// 1, todo with the label, its parent recorded as parent ("" for none).
func queuedRoot(key, parent, rank string, seq int64) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: key, Tree: key,
		Parent: record.ParentOf(parent), Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: rank, HandedOver: true, LastDispatchSeq: seq,
		HoldsGate: true}
}

// issueOpen says whether key's own lifecycle stands open under the workflow's authority, which is
// what the promotion that starts an issue writes.
func issueOpen(t *testing.T, pool *pgxpool.Pool, key string) bool {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	var open bool
	inTx(t, pool, func(tx pgx.Tx) {
		lifecycles, err := record.NewStore().IssueLifecycles(context.Background(), tx, project, []string{key})
		if err != nil {
			t.Fatalf("read the issue lifecycles: %v", err)
		}
		_, open = lifecycles[key]
	})
	return open
}

// carriedRoot records key as a root a hand-over of the release before carried in, carrying the
// phase it ran there at generation 2, as the first boot's conversion leaves it
// (record.CarriedIn): todo with the label, its run's mark set where it carries a run, and its
// first start under this release still to come.
func carriedRoot(t *testing.T, pool *pgxpool.Pool, key string, carries phase.Phase, rank string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: key, Tree: key, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a carried root", Phase: carries,
		Generation: 2, Status: "todo", DispatchStatus: "todo", Rank: rank, HandedOver: true, HoldsGate: true,
		TakenBack: carries != phase.Admitted, CarriedGeneration: 2})
}
