package supervise

// A prompt's failure at the prompt-failure limit while a held stop waits for the copy its turn's
// end started (R1 of dispatch://INFRA-1117). The retirement that limit makes is the held stop's
// own stop where a suspension is held (chargePrompt), and that stop would take the process the
// copy still asks, as the stale-plugin relaunch, the stale address, the parked wake and the ready
// said again would (turncopy_replace_test.go).

import "testing"

// The next task goes to an idle claim while the copy of its last turn's end is still being taken
// (R2: nothing of the turn waits for the copy), and the agent acknowledges the prompt and starts
// no turn. A daemon's stop that reaches the claim then is held for the copy (suspend). The
// prompt's wait for its turn runs out at the prompt-failure limit, which retires the process, and
// with a suspension held the retirement is that suspension's stop: run there, it stops the process
// while the copy still asks its shim, the copy fails and the turn's end is in no copy once the
// role is stopped. The failure is charged and the stop keeps waiting for the copy, whose end runs
// it.
//
// The negative control is the code at b68beb83, where chargePrompt runs the held stop at once:
// the claim is then suspended at the prompt's bound, before the copy ends.
func TestAPromptRetirementAtTheLimitWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	copies := &turnEndCopies{}
	h := newBareHarness(t)
	h.deps.CopyTurnEnd = copies.start
	h.deps.Limits = Limits{LaunchFailures: 3, PromptFailures: 1, PromptRetires: 1}
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	h.reach(StateIdle)
	if started := copies.started(); len(started) != 1 {
		t.Fatalf("the turn's end started %d copies, want one", len(started))
	}
	loc := h.locator()
	h.must(RequestDeliver{Claim: h.token, Task: "the task"})
	holdStop(t, h, "LEGION-208 left implementing")
	waitsOnItsProcess(t, h, loc)

	h.advance(testRPC)

	waitsOnItsProcess(t, h, loc)
	stopsAfterTheCopy(t, h, copies, loc)
}
