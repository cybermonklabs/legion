package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// claimFailures is a workflow handler that keeps every failure fact applied through it, so a test
// reads the fact the daemon built rather than the notice the engine writes from it.
type claimFailures struct {
	mu   sync.Mutex
	seen []intake.ClaimFailed
}

func (h *claimFailures) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	if failed, ok := fact.(intake.ClaimFailed); ok {
		h.mu.Lock()
		h.seen = append(h.seen, failed)
		h.mu.Unlock()
	}
	return intake.Result{}, nil
}

func (h *claimFailures) applied() []intake.ClaimFailed {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]intake.ClaimFailed(nil), h.seen...)
}

// A notice of a failed claim can only name what the fact carries, and the workflow engine holds
// neither the claim nor the deployment's limits: it reads both off the fact alone. So the fact a
// real machine's failure applies must carry the cause the site that failed the claim chose, the
// four counters that failure left on it and the three limits those counters run out against, not
// the sentence alone.
//
// The failure here is a real one: an agent that registers on a plugin contract this daemon does
// not speak is relaunched onto the current plugin, and the relaunches run out (supervise's
// staleRegistration), which fails the claim under stale_plugin with its launch failures at the
// limit. The hook the fact travels is the daemon's own, as the boot's attach wires it
// (claimDurableState, applyTerminal), limits included.
func TestTheHeldNoticeNamesTheFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	ctx := context.Background()
	limits := supervise.Limits{LaunchFailures: 2, PromptFailures: 3, PromptRetires: 2}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PluginRelaunchable = true
	sup.deps.Limits = limits
	facts := &claimFailures{}
	w := &workflowRuntime{pool: pool, records: record.NewStore(), handlers: []intake.Handler{facts}, log: quietLogger()}
	w.attach(&supervision{supervisor: sup, log: quietLogger()})

	const issue = "LEGION-208"
	planner, err := claim.NewToken("legion", issue, claim.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: issue, Issue: issue,
		Role: claim.RolePlanner, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	sup.deps.Conns.(*fake.Conns).Register(planner, fake.NewConn())
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: planner}); err != nil {
		t.Fatalf("spawn the planner: %v", err)
	}
	// The first stale registration is relaunched uncharged, the second charges one launch
	// failure, and the third reaches the limit of two and fails the claim. Each is refused, which
	// is what the agent is told and no part of what the daemon applies.
	for range 3 {
		if err := machine.Handle(ctx, supervise.RequestRegister{Claim: planner, Generation: machine.Claim().Generation,
			Session: "ses-plan", SessionFile: "/tmp/plan.jsonl", CapabilityHash: []byte("planner-capability"), PluginContract: 1}); err == nil {
			t.Fatal("a registration on a contract this daemon does not speak was taken, want it refused")
		}
	}
	failed := machine.Claim()
	if failed.State != supervise.StateFailed {
		t.Fatalf("the planner is %s after its relaunches onto the current plugin ran out, want %s", failed.State, supervise.StateFailed)
	}
	if failed.Failure == "" {
		t.Fatal("the failed planner carries no sentence, so the fact below proves nothing of the one the daemon applies")
	}

	want := intake.ClaimFailed{Issue: issue, Role: claim.RolePlanner, Reason: failed.Failure,
		Cause: supervise.CauseStalePlugin, Budgets: supervise.Budgets{LaunchFailures: 2}, Limits: limits}
	var applied []intake.ClaimFailed
	testwait.Eventually(t, "the planner's failure to reach the workflow", func() bool {
		applied = facts.applied()
		return len(applied) > 0
	})
	if len(applied) != 1 || applied[0] != want {
		t.Fatalf("the workflow was told %+v, want exactly %+v: the cause, the counters the failure left and the limits they ran out against travel with the sentence", applied, want)
	}
}
