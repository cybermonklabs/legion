package supervise

// A failure of the claim while a held stop waits for the copy its turn's end started (R1 of
// dispatch://INFRA-1117). A provider error that holds the issue and a run of refused turns at the
// launch-failure limit each fail the claim, and a failure ends the claim's process (fail,
// endProcess): decided while the held stop waits, it would stop the process the copy still asks,
// as the eight paths of turncopy_replace_test.go and turncopy_prompt_test.go would.

import "testing"

// processStops counts the runtime's stops and releases of the claim's process.
func processStops(h *harness) int {
	return len(h.rt.CallsOf("Suspend")) + len(h.rt.CallsOf("Release"))
}

// stopsOnceAfterTheCopy hands back the end of the copy the turn's end started, and fails unless
// the process was stopped by nothing before that end, is stopped once after it, and the claim is
// then failed or suspended.
func stopsOnceAfterTheCopy(t *testing.T, h *harness, copies *turnEndCopies) {
	t.Helper()
	if n := processStops(h); n != 0 {
		t.Fatalf("the process was stopped %d times while the held stop waited for the copy of its turn's end (state %s), want none before the copy ends",
			n, h.claim().State)
	}
	h.must(copies.copied(t, 0))
	if n := processStops(h); n != 1 {
		t.Fatalf("the process was stopped %d times once the copy ended, want once", n)
	}
	if state := h.claim().State; state != StateFailed && state != StateSuspended {
		t.Fatalf("state = %s once the copy ended, want the claim failed or suspended", state)
	}
}

// The turn of a task ends into a held daemon stop, which waits for the turn's copy, and the
// prompt's result comes behind the end, as Oh My Pi sends it: the turn ended in a provider error
// that holds the issue (a 400, a request too long for the model), which fails the claim. Failed
// there, the claim's process is stopped while the copy still asks its shim, the copy fails, and
// the turn's end is in no copy once the role is failed. The failure waits for the copy as the
// held stop does.
//
// The negative control is the code at 7e5fbdc9, where providerErrored fails the claim at once:
// the process is then stopped at the result, before the copy ends.
func TestAProviderErrorThatFailsTheClaimWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	h, copies := copyingHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: providerErrorTaskText})
	h.must(StreamTurnStart{Claim: h.token})
	task := h.pending().ID
	holdStop(t, h, "LEGION-208 left implementing")
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	if started := copies.started(); len(started) != 1 {
		t.Fatalf("the turn's end started %d copies, want one", len(started))
	}

	if err := h.handle(StreamPromptResult{Claim: h.token, DeliveryID: task, AgentInvoked: true,
		Error: "prompt is too long", HTTPStatus: 400}); err != nil {
		t.Logf("the prompt's result answered %v", err)
	}

	stopsOnceAfterTheCopy(t, h, copies)
}

// A run of refused turns at the launch-failure limit fails the claim (turnOutcome). The plugin
// reports each turn's outcome at its end, so the report that reaches the limit can come while a
// held stop waits for that turn's copy. Failed there, the claim's process is stopped while the
// copy still asks its shim. The failure waits for the copy as the held stop does.
//
// The negative control is the code at 7e5fbdc9, where turnOutcome fails the claim at once: the
// process is then stopped at the report, before the copy ends.
func TestARefusedTurnRunThatFailsTheClaimWaitsForTheTurnsCopyItsHeldStopWaitsFor(t *testing.T) {
	h, copies, _ := copyWaiting(t)

	for sequence := uint64(1); sequence <= uint64(h.deps.Limits.LaunchFailures); sequence++ {
		if err := h.handle(RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: sequence,
			Refused: true, Message: "the provider refused the request"}); err != nil {
			t.Logf("the turn outcome %d answered %v", sequence, err)
		}
	}

	stopsOnceAfterTheCopy(t, h, copies)
}
