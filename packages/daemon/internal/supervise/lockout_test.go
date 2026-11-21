package supervise

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// While the worker stream refuses a claim's newer process because an older generation's
// connection is still bound, the claim's registration deadline does not run: the lock-out is not
// the newer process's fault, so it charges the claim nothing and launches no newer generation over
// it (R3). Once the lock-out ends, the newer process has a whole deadline to register.
func TestALockedOutLaunchIsChargedNothingAndNotRelaunched(t *testing.T) {
	h := newBareHarness(t)
	var locked atomic.Bool
	locked.Store(true)
	h.deps.LockedOut = func(token claim.Token) bool { return token == testToken && locked.Load() }
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.launch()
	generation := h.generation()

	for elapsed := time.Duration(0); elapsed < deadline+10*time.Minute; elapsed += testBoot {
		h.advance(testBoot)
	}

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{})
	if h.generation() != generation {
		t.Fatalf("generation = %d, want the locked-out launch %d kept", h.generation(), generation)
	}
	h.wantCalls("Suspend", 0)

	// The lock-out ends and the newer process never registers: the deadline is no longer held,
	// and charges as today.
	locked.Store(false)
	for elapsed := time.Duration(0); elapsed <= deadline+testBoot && h.budgets().LaunchFailures == 0; elapsed += testBoot {
		h.advance(testBoot)
	}
	if h.budgets().LaunchFailures != 1 {
		t.Fatalf("budgets once the lock-out is over = %+v, want the one launch failure today charges", h.budgets())
	}
}

// The close of a connection of a generation the claim has left is not the claim's close: the
// older shim's connection ending while the newer one is bound leaves the claim as it is, and its
// process unprobed (R3).
func TestAnOlderGenerationsCloseIsNotTheClaimsClose(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	// A claim relaunched twice since, so a generation it has left is one no zero stands for.
	h.m.claim.Generation = 3
	probes := len(h.calls("Probe"))

	h.must(StreamClosed{Claim: h.token, Generation: h.generation() - 1})

	h.wantState(StateIdle)
	if got := len(h.calls("Probe")); got != probes {
		t.Fatalf("the older generation's close probed the claim %d times, want none", got-probes)
	}

	// The claim's own connection closing is its close, as today: the process is probed.
	h.must(StreamClosed{Claim: h.token, Generation: h.generation()})
	if got := len(h.calls("Probe")); got != probes+1 {
		t.Fatalf("the claim's own close probed it %d times, want once", got-probes)
	}
}
