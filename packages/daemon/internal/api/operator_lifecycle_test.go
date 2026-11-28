package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// issueAuthority is the authority issue's own lifecycle stands under, "" where it has none.
func issueAuthority(t *testing.T, h *harness, issue string) string {
	t.Helper()
	var authority string
	err := h.store.Pool().QueryRow(h.ctx, `select authority from issue_lifecycles where project = $1 and issue = $2`, testProject, issue).Scan(&authority)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the lifecycle of issue %s: %v", issue, err)
	}
	return authority
}

// An operator's spawn on a child the workflow records and has not started opens no lifecycle for
// it: a claim of the child binds the lifecycle the workflow opens when it starts the child, so the
// spawn is refused, naming that, and launches nothing. The child's promotion then opens its
// lifecycle under the workflow and starts it. An operator's lifecycle there would hold the
// promotion off at every pass, and the operator's close, which refuses a workflow issue's tree,
// would never release it.
func TestAnOperatorsSpawnOnAChildTheWorkflowHasNotStartedLeavesItsPromotionFree(t *testing.T) {
	const child = "LEGION-212"
	h, handlers, _ := statusHarness(t)
	root := statusRoot
	h.recordIssue(record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot, Parent: &root,
		Project: testDispatchProject, Title: child, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "C", LastDispatchSeq: 5})
	if err := store.Transact(h.ctx, h.store.Pool(), func(_ context.Context, tx pgx.Tx) error {
		return seedCopy(tx, child, statusRoot, "todo")
	}); err != nil {
		t.Fatalf("seed the copy row of %s: %v", child, err)
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", SpawnRequest{Tree: statusRoot, Issue: child, Role: claim.RoleImplementer, Prompt: "Reply ready and wait."})
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "issue "+child+" is the workflow's") {
		t.Fatalf("the operator's spawn on %s, which the workflow records and has not started = %d %s, want 409 naming the workflow's lifecycle", child, recorder.Code, recorder.Body)
	}
	if authority := issueAuthority(t, h, child); authority != "" {
		t.Fatalf("the lifecycle of %s after the operator's spawn stands under %s, want none", child, authority)
	}
	if spawns := h.runtime.CallsOf("Spawn"); len(spawns) != 0 {
		t.Fatalf("the refused spawn launched %d processes, want none", len(spawns))
	}

	if _, err := intake.ApplyFact(h.ctx, h.store.Pool(), "dispatch", "todo-"+child, intake.DispatchIssue{Key: child, Seq: 6, Type: "issue.updated",
		Status: "todo", Title: child, Parent: statusRoot, Rank: "C", ResolvedRepo: "acme/widgets", ClaimKnown: true}, handlers...); err != nil {
		t.Fatalf("apply the event of %s: %v", child, err)
	}
	if authority := issueAuthority(t, h, child); authority != string(treelifecycle.AuthorityWorkflow) {
		t.Fatalf("the lifecycle of %s after its promotion stands under %q, want the workflow's", child, authority)
	}
	if got := recordedChild(t, h, child); got == nil || got.Phase == phase.Admitted {
		t.Fatalf("%s after its promotion = %#v, want it started past admitted", child, got)
	}
}

// An operator's spawn on an issue the workflow does not record opens the issue's lifecycle under
// the operator, which holds the issue's promotion off once the workflow records it: here as a
// child of the live root, whose next event finds it waiting on the operator's authority. The
// operator's close of its claim of that issue answers success, though a workflow issue now backs
// it: the operator holds the issue's lifecycle, so the close retires the operator's claim and
// releases that lifecycle alone. The confirmation of that cleanup lets the issue's promotion open
// its own lifecycle under the workflow and start it.
func TestTheOperatorsCloseOfAnIssueTheWorkflowRecordedLaterLetsItStart(t *testing.T) {
	const issue = "LEGION-212"
	h, handlers, _ := statusHarness(t)
	token, err := claim.NewToken(testProject, issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", SpawnRequest{Tree: issue, Issue: issue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}); recorder.Code != http.StatusCreated {
		t.Fatalf("the operator's spawn on %s, which nothing records = %d %s, want 201", issue, recorder.Code, recorder.Body)
	}
	root := statusRoot
	h.recordIssue(record.Issue{Key: issue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot, Parent: &root,
		Project: testDispatchProject, Title: issue, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "C", LastDispatchSeq: 5})
	if err := store.Transact(h.ctx, h.store.Pool(), func(_ context.Context, tx pgx.Tx) error {
		return seedCopy(tx, issue, statusRoot, "todo")
	}); err != nil {
		t.Fatalf("seed the copy row of %s: %v", issue, err)
	}
	if _, err := intake.ApplyFact(h.ctx, h.store.Pool(), "dispatch", "todo-"+issue, intake.DispatchIssue{Key: issue, Seq: 6, Type: "issue.updated",
		Status: "todo", Title: issue, Parent: statusRoot, Rank: "C", ResolvedRepo: "acme/widgets", ClaimKnown: true}, handlers...); err != nil {
		t.Fatalf("apply the event of %s: %v", issue, err)
	}
	if got := recordedChild(t, h, issue); got == nil || got.Phase != phase.Admitted || issueAuthority(t, h, issue) != string(treelifecycle.AuthorityOperator) {
		t.Fatalf("%s under the operator's lifecycle = %#v, its lifecycle under %q; want it waiting at admitted on the operator's", issue, got, issueAuthority(t, h, issue))
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/close", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the operator's close of %s = %d %s, want 200", token, recorder.Code, recorder.Body)
	}
	if state := h.stored(token).State; state != supervise.StateRetired {
		t.Fatalf("the operator's claim %s is %s after its close, want retired", token, state)
	}
	if cleanups := h.runtime.CallsOf("CleanupIssue"); len(cleanups) != 1 || cleanups[0].Issue != issue {
		t.Fatalf("issue releases = %+v, want %s released alone", cleanups, issue)
	}
	if authority := issueAuthority(t, h, issue); authority != string(treelifecycle.AuthorityWorkflow) {
		t.Fatalf("the lifecycle of %s after the operator's close stands under %q, want the workflow's", issue, authority)
	}
	if got := recordedChild(t, h, issue); got == nil || got.Phase == phase.Admitted {
		t.Fatalf("%s after the operator's close = %#v, want it started past admitted", issue, got)
	}
}

// An operator's spawn on an issue the workflow does not record and that stands under no issue in
// Legion's work opens the issue's lifecycle under the operator, as the operator's own tree has
// always done. One the workflow does not record yet that stands under an issue in Legion's work is
// the workflow's, which a release or a person's todo records under that issue: the spawn opens
// nothing for it and is refused.
func TestAnOperatorsSpawnOpensALifecycleOnlyForAnIssueTheWorkflowDoesNotHold(t *testing.T) {
	const under = "LEGION-212"
	h, _, _ := statusHarness(t)
	if err := store.Transact(h.ctx, h.store.Pool(), func(_ context.Context, tx pgx.Tx) error {
		return seedCopy(tx, under, statusRoot, "backlog")
	}); err != nil {
		t.Fatalf("seed the copy row of %s: %v", under, err)
	}
	own := SpawnRequest{Tree: "LEGION-404", Issue: "LEGION-404", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", own); recorder.Code != http.StatusCreated {
		t.Fatalf("the operator's spawn on %s, which nothing records = %d %s, want 201", own.Issue, recorder.Code, recorder.Body)
	}
	if authority := issueAuthority(t, h, own.Issue); authority != string(treelifecycle.AuthorityOperator) {
		t.Fatalf("the lifecycle of %s after the operator's spawn stands under %q, want the operator's", own.Issue, authority)
	}
	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", SpawnRequest{Tree: statusRoot, Issue: under, Role: claim.RoleImplementer, Prompt: "Reply ready and wait."})
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "issue "+under+" is the workflow's") {
		t.Fatalf("the operator's spawn on %s, under %s in Legion's work = %d %s, want 409 naming the workflow's lifecycle", under, statusRoot, recorder.Code, recorder.Body)
	}
	if authority := issueAuthority(t, h, under); authority != "" {
		t.Fatalf("the lifecycle of %s after the operator's spawn stands under %s, want none", under, authority)
	}
}

// The operator's close of a tree whose claims name an issue with no lifecycle of its own, as an
// issue whose only claim retired before any lifecycle was opened for it has none, and an issue
// whose lifecycle the workflow holds, closes: it reserves and releases the root's own issue alone,
// opens nothing for the first, and leaves the workflow's lifecycle of the second open as it stood,
// since that cleanup is the workflow's.
func TestTheOperatorsClosePassesOverAnIssueWithNoLifecycleAndOneTheWorkflowHolds(t *testing.T) {
	const bare, held = "LEGION-209", "LEGION-210"
	h := newHarness(t)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn = %d %s, want 201", recorder.Code, recorder.Body)
	}
	if _, err := h.store.OpenIssueLifecycle(h.ctx, testProject, held, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", held, err)
	}
	for _, issue := range []string{bare, held} {
		token, err := claim.NewToken(testProject, issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.store.PutClaim(h.ctx, supervise.Claim{Token: token, Project: testProject, Tree: "LEGION-208", Issue: issue,
			Role: claim.RoleImplementer, Generation: 1, State: supervise.StateRetired}); err != nil {
			t.Fatalf("store the retired claim of %s: %v", issue, err)
		}
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("close = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if cleanups := h.runtime.CallsOf("CleanupIssue"); len(cleanups) != 1 || cleanups[0].Issue != "LEGION-208" {
		t.Fatalf("issue releases = %+v, want LEGION-208 released alone", cleanups)
	}
	if started, confirmed := h.issueCleanup("LEGION-208"); !started || !confirmed {
		t.Fatalf("the cleanup of LEGION-208 started %t, confirmed %t; want it confirmed", started, confirmed)
	}
	if authority := issueAuthority(t, h, bare); authority != "" {
		t.Fatalf("the close left %s a lifecycle under %s, want none", bare, authority)
	}
	if started, _ := h.issueCleanup(held); started || issueAuthority(t, h, held) != string(treelifecycle.AuthorityWorkflow) {
		t.Fatalf("the lifecycle of %s after the close: cleanup started %t under %s; want the workflow's, open", held, started, issueAuthority(t, h, held))
	}
}
