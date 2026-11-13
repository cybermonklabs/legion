package supervise

// The live case behind dispatch://INFRA-714 R7: INFRA-264's tester registered on a fresh claim, an
// Envoy message reached its role topic six seconds later and started its first turn, it ran the
// whole testing round in that turn, and its handoff_complete was refused 409 HANDOFF_NO_RUN —
// "this claim has taken none" — because the daemon's phase task had not been delivered yet.
//
// A task is taken now, and a turn before the take reads no text of it: the claim answers for no
// run of a task it holds untaken (serving's rule 3), and the completion route's own fallback is
// what credits such a worker's completion to a held task of its issue's current run and phase
// (api/handoff.go). What this pins of the machine is that half, and that the task the issue has
// left the phase of is dropped at its take rather than run after the work it names is done.

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

func TestAFreshClaimsFirstTurnIsCreditedTheTaskTheDaemonHasNotSentYet(t *testing.T) {
	h := newBareHarness(t)
	issuePhase := phase.Testing
	generation := uint64(0)
	h.deps.PhaseHolds = func(_ context.Context, _ string, p phase.Phase, run uint64) (bool, error) {
		return p == issuePhase && run == generation, nil
	}
	h.store.PutClaim(h.ctx, queuedClaim())
	h.start(queuedClaim())
	h.manualTakes()
	h.reach(StateReady)

	// The phase's task is queued for run 7, and the worker has not taken it.
	generation = 7
	h.must(RequestDeliver{Claim: testToken, Task: "test the branch", Phase: issuePhase, Generation: 7})

	// An Envoy message starts the claim's first turn, with no take behind it: the claim serves no
	// run of its own, and the completion route reads the held task of the current run itself.
	h.must(StreamTurnStart{Claim: testToken})
	if run := h.claim().ServingRun(); run != 0 {
		t.Fatalf("the claim serves run %d during a turn before any take, want none: no take reached the task", run)
	}
	if p := h.pending(); p.Generation != 7 || p.taken() {
		t.Fatalf("pending %+v, want the untaken task of run 7 the completion route falls back to", p)
	}

	// The completion is accepted on that fallback, and the issue leaves testing with it. The turn
	// then ends, and the worker's take of the task it did is dropped rather than accepted.
	issuePhase = phase.Reviewing
	h.must(StreamTurnEnd{Claim: testToken})
	if answer := h.take(); answer.Outcome != TakeOutcomeDropped {
		t.Fatalf("the take after the phase left = %+v, want it dropped", answer)
	}

	if p := h.claim().Pending; p != nil {
		t.Fatalf("the claim still holds %+v, want the completed phase's task retired", p)
	}
	if stored := h.store.load(testToken); stored.Pending != nil {
		t.Fatalf("the store still holds %+v, want the task retired durably", stored.Pending)
	}
	if _, held := h.values.value(testToken); held {
		t.Fatal("the bucket still holds the dropped task's value")
	}
	h.wantPrompts(0)
}
