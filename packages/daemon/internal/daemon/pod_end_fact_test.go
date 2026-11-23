package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The pod end a claim's process died of reaches the workflow's failure fact from the claim, on
// both paths that build one (applyTerminal): the terminal hook the supervisor calls as the claim
// fails, and the boot's replay of a failed claim no daemon applied, which builds the fact from the
// claim the restarted daemon read back (replayTerminal). The second is what lets a worker-died
// written after a restart still name an eviction (INFRA-2009). The claim here fails on its first
// death, an eviction with its turn open, under a launch failure limit of one, so the failure
// keeps the pod end that death wrote (supervise's died). The negative control is applyTerminal
// building the fact without the claim's pod end: both facts then carry none.
func TestAFailedClaimsPodEndReachesTheWorkflowLiveAndAtTheBootReplay(t *testing.T) {
	t.Parallel()
	const evicted = "Evicted: The node was low on resource: ephemeral-storage. Container roles was using 48Gi, which exceeds its request of 0."
	ctx := context.Background()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Limits.LaunchFailures = 1
	live := &outboxFactHandler{}
	sup.OnDurableState((&workflowRuntime{pool: isolatedOutboxPool(t), handlers: []intake.Handler{live}, log: quietLogger(), limits: sup.deps.Limits}).claimDurableState)

	token, err := claim.NewToken("legion", "LEGION-208", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatalf("spawn the implementer: %v", err)
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-implement", SessionFile: "/tmp/implement.jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-implement"},
		supervise.StreamTurnStart{Claim: token},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("bring the implementer into a turn: handle %T: %v", ev, err)
		}
	}
	if err := machine.Handle(ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now(), PodEnd: evicted,
	}}); err != nil {
		t.Fatalf("the implementer's pod is evicted: %v", err)
	}
	failed := machine.Claim()
	if failed.State != supervise.StateFailed || failed.FailureCause != supervise.CauseDied || failed.PodEnd != evicted {
		t.Fatalf("the evicted implementer is %s with cause %q and pod end %q, want failed for its death with the eviction kept",
			failed.State, failed.FailureCause, failed.PodEnd)
	}

	podEndOf := func(facts []intake.Fact) []string {
		var ends []string
		for _, fact := range facts {
			if f, ok := fact.(intake.ClaimFailed); ok {
				ends = append(ends, f.PodEnd)
			}
		}
		return ends
	}
	if ends := podEndOf(live.facts()); len(ends) != 1 || ends[0] != evicted {
		t.Errorf("the live failure facts carry pod ends %q, want one carrying %q", ends, evicted)
	}

	replayed := &outboxFactHandler{}
	restarted := &workflowRuntime{pool: isolatedOutboxPool(t), handlers: []intake.Handler{replayed}, log: quietLogger(), limits: sup.deps.Limits}
	if err := restarted.replayTerminal(ctx, []supervise.Claim{failed}, bootTimeout); err != nil {
		t.Fatalf("replay the failed claim at boot: %v", err)
	}
	if ends := podEndOf(replayed.facts()); len(ends) != 1 || ends[0] != evicted {
		t.Errorf("the replayed failure facts carry pod ends %q, want one carrying %q", ends, evicted)
	}
}
