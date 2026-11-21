package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// seedBeneath records a child of parent in tree, standing in at with the Dispatch status its
// phase carries.
func seedBeneath(t *testing.T, pool *pgxpool.Pool, key, tree, parent string, at phase.Phase, status string) {
	t.Helper()
	above := parent
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: tree, Project: "LEGION", Title: key, Parent: &above, Phase: at, Generation: 1, Status: status, Rank: key})
}

// signOffNow applies the architect's sign_off of key and answers what the daemon made of it.
func signOffNow(t *testing.T, pool *pgxpool.Pool, id, key string) intake.Result {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "api", id, intake.SignOff{Issue: key},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{})
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	return result
}

// A6. A parent is accepted after its children: sign_off is refused while any issue beneath the
// one being accepted is still in the workflow, naming each of them, and is taken once the last
// of them leaves. A parent is accepted once its checks pass on a base commit that carries its
// children's merges, so accepting it first would send a head to the merge queue that no check ran
// with that work.
func TestSignOffIsRefusedWhileAnIssueBeneathIsInTheWorkflow(t *testing.T) {
	pool := migratedPool(t)
	seedAccepting(t, pool)
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Implementing, "in_progress")
	seedBeneath(t, pool, "LEGION-210", "LEGION-208", "LEGION-208", phase.Testing, "testing")
	seedBeneath(t, pool, "LEGION-211", "LEGION-208", "LEGION-208", phase.Done, "done")

	result := signOffNow(t, pool, "signoff-held", "LEGION-208")
	if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
		t.Fatalf("sign-off with two issues beneath = %+v, want 409 SIGNOFF_CHILD_IN_WORKFLOW", result.Refusal)
	}
	for _, want := range []string{"LEGION-209", "LEGION-210"} {
		if !strings.Contains(result.Refusal.Message, want) {
			t.Fatalf("refusal %q does not name %s", result.Refusal.Message, want)
		}
	}
	if strings.Contains(result.Refusal.Message, "LEGION-211") {
		t.Fatalf("refusal %q names LEGION-211, which is done and holds nothing", result.Refusal.Message)
	}
	assertPhase(t, pool, phase.Accepting)

	// One leaves by its own close, the other by a person's park: either empties the set.
	leave(t, pool, "LEGION-209", phase.Done, "done")
	if result := signOffNow(t, pool, "signoff-one-left", "LEGION-208"); result.Refusal == nil || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
		t.Fatalf("sign-off with one issue still beneath = %+v, want it refused again", result.Refusal)
	}
	leave(t, pool, "LEGION-210", phase.Admitted, "backlog")
	if result := signOffNow(t, pool, "signoff-accepted", "LEGION-208"); result.Refusal != nil {
		t.Fatalf("sign-off once the last issue beneath left = %+v, want it accepted", result.Refusal)
	}
	assertPhase(t, pool, phase.Retro)
}

// leave moves key to at with the Dispatch status it leaves on, as a close or a person's park
// leaves it: the record, and the copy of Dispatch's issues the reading of that move writes
// (blockers.Mirror), which the open-children rule reads.
func leave(t *testing.T, pool *pgxpool.Pool, key string, at phase.Phase, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "update issues set phase = $2, status = $3 where key = $1", key, string(at), status); err != nil {
		t.Fatalf("take %s out of the workflow: %v", key, err)
	}
	if _, err := pool.Exec(context.Background(), "update dispatch_issues set status = $2 where key = $1", key, status); err != nil {
		t.Fatalf("move the copy's row of %s to %s: %v", key, status, err)
	}
}

// A6, the recorded departure. A child the daemon itself holds at admitted beneath an ancestor it
// has already accepted does not refuse that ancestor's sign-off. Without the exclusion the rules
// deadlock: the child is pinned at admitted until the ancestor closes, and the ancestor cannot
// be accepted or closed while the child is in the workflow. Decided by this tree's architect on
// dispatch://INFRA-980/comment/d642164a-31d2-4d6c-ae5e-4da5dd0b9104.
func TestAChildHeldUnderAnAcceptedAncestorDoesNotRefuseThatAncestorsSignOff(t *testing.T) {
	pool := migratedPool(t)
	// The root's own pull request has merged, and the root stands in merging, waiting for the
	// rest of its tree. LEGION-209 is in accepting beneath it, and LEGION-300, a person's todo
	// under LEGION-209, stands at admitted held by the root above it.
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "needs_review", Rank: "U"})
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Accepting, "needs_review")
	seedBeneath(t, pool, "LEGION-300", "LEGION-208", "LEGION-209", phase.Admitted, "todo")
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head", CheckedHead: "head", Failing: []string{}})

	if result := signOffNow(t, pool, "signoff-209", "LEGION-209"); result.Refusal != nil {
		t.Fatalf("sign-off of %s = %+v, want it accepted: the only issue beneath it is one the daemon holds at admitted under an accepted ancestor", "LEGION-209", result.Refusal)
	}
	var at phase.Phase
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-209'").Scan(&at); err != nil || at != phase.Retro {
		t.Fatalf("LEGION-209 = %s, %v; want retro", at, err)
	}
	// The hold stands, and is now the nearer ancestor's: the sign-off put LEGION-209 in retro,
	// so LEGION-300 waits for it as well as for the merged root above it. It is still in the
	// workflow for every other reading.
	if got := record.AcceptedAncestor(projectChains(t, pool, "LEGION-300"), "LEGION-300"); got != "LEGION-209" {
		t.Fatalf("the accepted ancestor of LEGION-300 = %q, want the issue just accepted above it, LEGION-209", got)
	}
	if !record.InWorkflow(*issueRecord(t, pool, "LEGION-300")) {
		t.Fatalf("LEGION-300 left the workflow, want it held at admitted inside it")
	}
}

// projectChains is the Dispatch chains of the test project with keys named (record.ReadChains),
// as a decision of the daemon reads them.
func projectChains(t *testing.T, pool *pgxpool.Pool, keys ...string) *record.Chains {
	t.Helper()
	var chains *record.Chains
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		chains, err = record.ReadChains(context.Background(), tx, record.NewStore(), "LEGION", keys)
		return err
	})
	return chains
}

// A6. close_root is refused the same way and with the same code while an issue beneath is in the
// workflow, and after R8's codes while a Dispatch child of it is not done (R24 of
// dispatch://INFRA-3320: CHILDREN_NOT_DONE), a child on the backlog included; once the child is
// done the close is queued, and the root keeps its phase until Dispatch shows it done.
func TestCloseRootIsRefusedWhileAnIssueBeneathIsInTheWorkflow(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"})
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Planning, "in_progress")

	closeNow := func(id string) intake.Result {
		t.Helper()
		result, err := intake.ApplyFact(context.Background(), pool, "api", id, intake.CloseRoot{Issue: "LEGION-208", Reason: "moot"},
			testEngine(config.DesignGateRootIssues, nil), admissionStub{})
		if err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
		return result
	}
	result := closeNow("close-held")
	if result.Refusal == nil || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" || !strings.Contains(result.Refusal.Message, "LEGION-209") {
		t.Fatalf("close_root with an issue beneath = %+v, want 409 SIGNOFF_CHILD_IN_WORKFLOW naming LEGION-209", result.Refusal)
	}
	assertPhase(t, pool, phase.Admitted)

	leave(t, pool, "LEGION-209", phase.Admitted, "backlog")
	result = closeNow("close-backlog")
	if result.Refusal == nil || result.Refusal.Code != "CHILDREN_NOT_DONE" || !strings.Contains(result.Refusal.Message, "LEGION-209 (backlog)") {
		t.Fatalf("close_root with a child on the backlog = %+v, want 409 CHILDREN_NOT_DONE naming LEGION-209 (backlog)", result.Refusal)
	}
	assertOutboxCount(t, pool, "dispatch_status", 0)

	leave(t, pool, "LEGION-209", phase.Done, "done")
	if result := closeNow("close-accepted"); result.Refusal != nil {
		t.Fatalf("close_root once the child is done = %+v, want it accepted", result.Refusal)
	}
	assertPhase(t, pool, phase.Admitted)
	assertOutboxCount(t, pool, "dispatch_status", 1)
}

// A6 on the Dispatch chain. A person's move of an issue to todo under a parent that is done,
// beneath an ancestor the daemon has already accepted (merging), records nothing and tells nobody:
// its parent is out of Legion's work while an issue above it is in it, so the issue is that
// issue's unreleased work, held where it stands (R19 of dispatch://INFRA-3320, `ancestry`). The
// chain is walked past the done parent, so the accepted ancestor above it is still found
// (AcceptedAncestorIn); the line's hold of such an issue is record's
// TestTheLineHoldsAnIssueUnderAnAcceptedAncestorAboveADoneParent.
func TestAPersonsTodoUnderAnAcceptedAncestorIsHeldUnrecorded(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "needs_review", Rank: "U"})
	seedTreeLifecycle(t, pool, "LEGION-208")
	seedBeneath(t, pool, "LEGION-209", "LEGION-208", "LEGION-208", phase.Done, "done")
	// The daemon's copy takes the event before the engine reads it (the daemon's handler order).
	mirrorChild(t, pool, "LEGION-300", "LEGION-209", "todo", 3)

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.DispatchIssue{Key: "LEGION-300", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated",
			Status: "todo", Title: "grandchild", Parent: "LEGION-209", Rank: "Z"})

	if held := issueRecord(t, pool, "LEGION-300"); held != nil {
		t.Fatalf("the person's todo = %#v, want nothing recorded: its parent is out of Legion's work and an issue above it is in it", held)
	}
	if got := record.AcceptedAncestorIn(projectChains(t, pool, "LEGION-300", "LEGION-209"), "LEGION-300", "LEGION-209"); got != "LEGION-208" {
		t.Fatalf("the accepted ancestor of the grandchild = %q, want LEGION-208, found by walking past the done parent", got)
	}
	assertOutboxCount(t, pool, "notice", 0)
}
