package supervise

// What the claim keeps of the pod end its process died of (Claim.PodEnd): written where the death
// is recorded, cleared before a retry as the failure is, and stored, so a daemon that restarted
// between the pod's death and the moment the relaunch budget ran out still reports the cause the
// role never chose (INFRA-2009).

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// evictedPodEnd is the pod end the runtime reads off an evicted pod
// (runtime.Observation.PodEnd).
const evictedPodEnd = "Evicted: The node was low on resource: ephemeral-storage. Container roles was using 48Gi, which exceeds its request of 0."

// The end that killed the process is on the claim the workflow is told from. It is cleared by the
// relaunch that follows each death it survives — a claim being retried carries nothing of the end
// before it — and the end that runs the budget out is the one the failed claim keeps, in the
// store, which is what a restarted daemon replays the failure from.
func TestAFailedClaimKeepsThePodEndThatRanItsBudgetOut(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.must(StreamTurnStart{Claim: testToken})

	evicted := func() {
		t.Helper()
		h.must(RuntimeObservation{Observation: runtime.Observation{
			Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), PodEnd: evictedPodEnd,
		}})
	}
	limit := testLimits().LaunchFailures
	for death := 1; death < limit; death++ {
		evicted()
		h.relaunched()
		// The relaunch cleared it: this claim is running again and carries nothing of the end
		// before it, exactly as it carries no failure.
		if stored := h.store.load(testToken); stored.PodEnd != "" || stored.Failure != "" {
			t.Fatalf("death %d: the relaunched claim holds pod end %q and failure %q, want both cleared",
				death, stored.PodEnd, stored.Failure)
		}
		h.must(StreamTurnStart{Claim: testToken})
		h.wantState(StateWorking)
	}

	evicted()

	h.wantState(StateFailed)
	stored := h.store.load(testToken)
	if stored.State != StateFailed || stored.Failure == "" {
		t.Fatalf("the stored claim is %s with failure %q, want it failed with the supervisor's sentence", stored.State, stored.Failure)
	}
	if stored.PodEnd != evictedPodEnd {
		t.Fatalf("the failed claim holds pod end %q, want %q: it is what the workflow names the cause from after a restart",
			stored.PodEnd, evictedPodEnd)
	}
}

// A death the runtime reports with no pod end — every runtime but the Sandbox one, and a pod that
// carries no reason — leaves the claim naming none, and a later death of something else replaces
// what an earlier one wrote rather than leaving it standing.
func TestAClaimsPodEndIsTheEndThatLastKilledIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.must(StreamTurnStart{Claim: testToken})

	h.must(RuntimeObservation{Observation: runtime.Observation{
		Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), PodEnd: evictedPodEnd,
	}})
	h.relaunched()
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)

	h.observe(runtime.Gone)
	if stored := h.store.load(testToken); stored.PodEnd != "" {
		t.Fatalf("after an end the pod named nothing for, the claim holds pod end %q, want none", stored.PodEnd)
	}
}
