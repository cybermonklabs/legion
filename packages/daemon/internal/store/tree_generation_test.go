package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// runRoot, runMember and runLeftDone are the tree of the tests of the tree's run: its root, a
// member at work in it, and a member that left it done.
const runRoot, runMember, runLeftDone = "ACME-1", "ACME-2", "ACME-3"

// runIssue is key in runRoot's tree at generation one, standing in phase at status.
func runIssue(key string, at phase.Phase, status string) record.Issue {
	issue := record.Issue{Key: key, Tree: runRoot, Project: "ACME", Title: key, Phase: at, Generation: 1, Status: status, Rank: "U",
		LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if key != runRoot {
		parent := runRoot
		issue.Parent = &parent
	}
	return issue
}

// putRunIssue records issue through the record's own write (record.Postgres.PutIssue), the write
// the workflow records every issue with, a root's next change among them.
func putRunIssue(t *testing.T, st *Store, issue record.Issue) {
	t.Helper()
	if err := Transact(context.Background(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, issue)
	}); err != nil {
		t.Fatalf("record %s at generation %d: %v", issue.Key, issue.Generation, err)
	}
}

// runTreeGeneration is the tree_generation the record holds for key.
func runTreeGeneration(t *testing.T, st *Store, key string) uint64 {
	t.Helper()
	var generation int64
	if err := st.Pool().QueryRow(context.Background(), `select tree_generation from issues where key = $1`, key).Scan(&generation); err != nil {
		t.Fatalf("read the tree generation of %s: %v", key, err)
	}
	return uint64(generation)
}

// putRunClaim records key's architect claim at generation one, suspended by by.
func putRunClaim(t *testing.T, st *Store, key string, by supervise.SuspendSource) {
	t.Helper()
	token, err := claim.NewToken("acme", key, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutClaim(context.Background(), supervise.Claim{Token: token, Project: "acme", Tree: runRoot, Issue: key, Role: claim.RoleArchitect,
		TreeEpoch: 1, Generation: 1, State: supervise.StateSuspended, SuspendedBy: by}); err != nil {
		t.Fatalf("record the claim of %s: %v", key, err)
	}
}

// countedNews is how many of what a delivery's copy carries under field ("items" or "openings")
// still count, as read reads them: design_news_credits for the items and design_news_openings for
// the openings, each of which reads design_news_current.
func countedNews(t *testing.T, st *Store, read, field string, news ...map[string]any) int {
	t.Helper()
	carried, err := json.Marshal(map[string]any{field: news})
	if err != nil {
		t.Fatal(err)
	}
	var counted int
	if err := st.Pool().QueryRow(context.Background(), `select count(*) from `+read+`($1::jsonb)`, carried).Scan(&counted); err != nil {
		t.Fatalf("read %s of %s: %v", read, carried, err)
	}
	return counted
}

// runItem is an item of the spec told to audience on issue, rendered at generation in the tree's
// run treeGeneration, as a delivery's copy carries it (record.DeliveryNewsItem).
func runItem(issue string, audience record.DesignAudience, generation, treeGeneration uint64) map[string]any {
	return map[string]any{"issue": issue, "audience": string(audience), "step": string(record.DesignStepSpec), "from": 1, "to": 2,
		"generation": generation, "tree_generation": treeGeneration}
}

// runOpening is the opening told to audience on issue, rendered at generation in the tree's run
// treeGeneration, as a delivery's copy carries it (record.DeliveryOpening).
func runOpening(issue string, audience record.DesignAudience, generation, treeGeneration uint64) map[string]any {
	return map[string]any{"issue": issue, "audience": string(audience), "seq": 1, "closed": false,
		"generation": generation, "tree_generation": treeGeneration}
}

// A root's next change raises the root's own generation and not its tree's, as the next change's
// write records it (record.Postgres.PutIssue, which leaves tree_generation). Each fence of a
// member's work queued before the change still holds in the tree's run: the member's idle release
// still tears its pod down, the suspension of a member that left the tree done still acts, and a
// design-news credit of the member still counts. Each is read before the change too, where each
// holds, so the reads are seen to read the fences.
func TestARootsNextChangeKeepsEachMembersFences(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	root := runIssue(runRoot, phase.Implementing, "in_progress")
	putRunIssue(t, st, root)
	putRunIssue(t, st, runIssue(runMember, phase.Implementing, "in_progress"))
	putRunIssue(t, st, runIssue(runLeftDone, phase.Done, "done"))
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
	putRunClaim(t, st, runMember, supervise.SuspendedByReleased)
	// The member that left done: its close stopped its role, and its suspension is queued at row 10.
	putRunClaim(t, st, runLeftDone, supervise.SuspendedByDaemon)

	fences := func(when string) {
		t.Helper()
		release, err := st.IssueRelease(ctx, "acme", IssueReleased{Issue: runMember, Tree: runRoot, IssueGeneration: 1, TreeGeneration: 1, Row: 10},
			record.OutOfWorkflow)
		if err != nil || release.Act != ReleaseActs {
			t.Errorf("%s: the idle release of %s = %s (%s), %v; want it to tear the pod down", when, runMember, release.Act, release.Cause, err)
		}
		act, err := st.IssueSuspension(ctx, "acme", IssueClose{Issue: runLeftDone, Tree: runRoot, IssueGeneration: 1, TreeGeneration: 1, Row: 10},
			record.OutOfWorkflow)
		if err != nil || !act {
			t.Errorf("%s: the suspension of %s = %t, %v; want it to act", when, runLeftDone, act, err)
		}
		if counted := countedNews(t, st, "design_news_credits", "items", runItem(runMember, record.AudienceHolder, 1, 1)); counted != 1 {
			t.Errorf("%s: %d credits of %s's holder count, want its one", when, counted, runMember)
		}
	}
	fences("before the root's next change")
	if t.Failed() {
		t.FailNow()
	}

	root.Generation = 2
	putRunIssue(t, st, root)
	if got := runTreeGeneration(t, st, runRoot); got != 1 {
		t.Errorf("the tree generation of %s after its next change to generation 2 = %d, want 1", runRoot, got)
	}
	fences("after the root's next change")
}

// The tree's architect runs on the tree's run, which a root's next change leaves: news told to the
// architect of the root at the tree's generation still counts after the root's own generation
// rises, as an item and as an opening, while news told to the holder of the root in the run that
// ended counts no more. Both count before the change.
func TestAnArchitectsNewsOfItsRootStandsWhileTheRootsGenerationRises(t *testing.T) {
	st := migratedStore(t)
	root := runIssue(runRoot, phase.Implementing, "in_progress")
	putRunIssue(t, st, root)
	counts := func(audience record.DesignAudience) (items, openings int) {
		t.Helper()
		return countedNews(t, st, "design_news_credits", "items", runItem(runRoot, audience, 1, 1)),
			countedNews(t, st, "design_news_openings", "openings", runOpening(runRoot, audience, 1, 1))
	}
	for _, audience := range []record.DesignAudience{record.AudienceArchitect, record.AudienceHolder} {
		if items, openings := counts(audience); items != 1 || openings != 1 {
			t.Fatalf("before the root's next change, the %s's item counts %d and its opening %d, want 1 each", audience, items, openings)
		}
	}

	root.Generation = 2
	putRunIssue(t, st, root)
	if items, openings := counts(record.AudienceArchitect); items != 1 || openings != 1 {
		t.Errorf("after the root's next change, the architect's item of the tree's run counts %d and its opening %d, want 1 each", items, openings)
	}
	if items, openings := counts(record.AudienceHolder); items != 0 || openings != 0 {
		t.Errorf("after the root's next change, the holder's item of the root's old run counts %d and its opening %d, want none", items, openings)
	}
}
