package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The one row a transition writes, run on a failed claim: the row that carries the task is the
// row that retries the claim, and it does both when the issue's slot lets it run
// (workflow.StartOrOwe).
//
// A retry revives a failed claim with fresh budgets and sends the task it was working, so a row
// that retried such a claim before the slot came put the worker back to work with no slot — the
// one thing the cap counts. There is nothing left to come behind this row: it carries the task,
// so the retry and the task arrive together, and the worker is prompted once.
func TestTheStartRowRetriesAFailedClaimOnFreshBudgets(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree,
		Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create implementer claim: %v", err)
	}
	runtime.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")}, fake.SpawnResult{Err: errors.New("pane launch failed")})
	_ = machine.Handle(ctx, supervise.RequestSpawn{Claim: token})
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("claim state = %s, want failed: the fixture's own premise", got)
	}

	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir()}
	const task = "Continue. Reason: retry held phase."

	// The transition's own request: the task, the phase it is for, and the mark that held the
	// row until the issue's slot came.
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
		Generation: issue.Generation, Phase: issue.Phase, Task: task, NeedsSlot: true}, time.Now())
	row.ID = 92
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("run the start row of a failed claim: %v", err)
	}
	got := machine.Claim()
	if got.State != supervise.StateLaunching || got.Budgets != (supervise.Budgets{}) {
		t.Fatalf("the claim after the start row = %+v, want it launching on fresh budgets: the row that carries the task is the one that retries it", got)
	}
	if got.Pending == nil || got.Pending.ID != "outbox:92" || got.Pending.Task != task {
		t.Fatalf("the pending delivery = %+v, want the row's own task, delivered once", got.Pending)
	}
}
