package supervise

// What the claim keeps of the pod end its process died of (Claim.PodEnd): written by each report
// of a death, cleared at each launch as the failure is, and stored, so the boot's replay of a
// failure no daemon applied still reports the cause the role never chose (INFRA-2009).

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
// carries no reason — leaves the claim naming none. The relaunch between the two deaths here
// clears the first one's end in any case (launchTries); a second report with no launch between is
// TestASecondReportOfOneDeathReplacesThePodEndTheFirstWrote's.
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

// Each report of a death writes the pod end that report read, whatever an earlier report of the
// same dead process wrote, so where died wrote them the claim's pod end and its account of the
// death (Claim.LastDeathCause) are one report's — a suspension over a dead process writes the
// account alone (suspendOverADeadProcess): the workflow names that account only where the pod
// end is empty (workflow's diedOf), and a pod end kept from an earlier report beside a later
// report's account would pair two reads. The two reports here have no launch between them:
// the lifecycle recheck the store will not answer refuses the relaunch each raises, which keeps
// the claim over its dead process (relaunchRefused), so it is died and not a launch that replaces
// what the first wrote. The negative control is died keeping an earlier pod end: both rows then
// read the first report's eviction.
func TestASecondReportOfOneDeathReplacesThePodEndTheFirstWrote(t *testing.T) {
	for _, row := range []struct{ name, second string }{
		{"a second report that reads no pod end", ""},
		{"a second report that reads another pod end", "Evicted: The node was low on resource: memory."},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: testToken, Task: "the task"})
			h.must(StreamTurnStart{Claim: testToken})
			dead := h.locator()
			h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
			report := func(podEnd string) {
				t.Helper()
				gone := RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now(), PodEnd: podEnd}}
				if err := h.handle(gone); err == nil {
					t.Fatal("the relaunch the report raised was taken, want the lifecycle recheck's refusal of it")
				}
			}

			report(evictedPodEnd)
			if got := h.claim().PodEnd; got != evictedPodEnd {
				t.Fatalf("after the first report the claim holds pod end %q, want %q", got, evictedPodEnd)
			}
			report(row.second)
			got := h.claim()
			if got.Locator == nil || *got.Locator != dead {
				t.Fatalf("after the second report the claim records %+v, want the dead process %+v: no launch came between the two", got.Locator, dead)
			}
			if got.PodEnd != row.second {
				t.Errorf("after the second report the claim holds pod end %q, want the second report's %q", got.PodEnd, row.second)
			}
		})
	}
}

// The pod end is the kubelet's text, and the claim keeps it as the store keeps a far side's text:
// a NUL as U+FFFD and a URL with no query, in memory and in the claim's write alike, so the
// failure's fact the workflow writes from the claim in memory names what a restart reads back. A
// refused relaunch keeps the claim over its dead process, as above, so no launch clears the end.
func TestAPodEndWithNULAndAQueryTokenIsHeldAsTheStoreKeepsIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "the task"})
	h.must(StreamTurnStart{Claim: testToken})
	h.m.deps.Store = &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	const want = "Evicted: The node was low\uFFFD on resource; see https://kubelet.example.test/why"

	gone := RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
		PodEnd: "Evicted: The node was low\x00 on resource; see https://kubelet.example.test/why?token=planted-query-token-5c1e"}}
	if err := h.handle(gone); err == nil {
		t.Fatal("the relaunch the report raised was taken, want the lifecycle recheck's refusal of it")
	}

	if got := h.claim().PodEnd; got != want {
		t.Errorf("the claim holds pod end %q, want %q, as the store keeps it", got, want)
	}
	if got := h.store.load(testToken).PodEnd; got != want {
		t.Errorf("the claim's write carries pod end %q, want %q", got, want)
	}
}
