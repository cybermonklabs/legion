package supervise

import "testing"

// An accepted take of the task the daemon gave the claim tells the workflow the agent took it
// (Deps.TaskConfirmed): a root whose wait that untaken task held is decided on that fact rather
// than at the next tick. Nothing else is told of: the queue, the take's turn starting and ending,
// and a take of the same number sent again tell no one.
func TestTakeAcceptedCallsTaskConfirmedOnce(t *testing.T) {
	h := newBareHarness(t)
	var confirmed []Claim
	h.deps.TaskConfirmed = func(c Claim) { confirmed = append(confirmed, c) }
	h.start(queuedClaim())
	h.manualTakes()
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "read the answer"})
	if len(confirmed) != 0 {
		t.Fatalf("told of %d claims before any take, want none", len(confirmed))
	}

	value := h.seen()
	if answer, err := h.takeValue(value); err != nil || answer.Outcome != TakeOutcomeAccepted {
		t.Fatalf("take = %+v, %v, want accepted", answer, err)
	}
	if len(confirmed) != 1 || confirmed[0].Token != testToken {
		t.Fatalf("told of %+v, want the one claim whose task was taken", confirmed)
	}
	if p := confirmed[0].Pending; p == nil || !p.awaitsTurn() {
		t.Fatalf("told of pending %+v, want the accepted take", p)
	}

	if answer, err := h.takeValue(value); err != nil || answer.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take sent again = %+v, %v, want it accepted again", answer, err)
	}
	h.must(StreamTurnStart{Claim: testToken})
	h.wantState(StateWorking)
	h.must(StreamTurnEnd{Claim: testToken})
	h.wantState(StateIdle)
	if len(confirmed) != 1 {
		t.Fatalf("told of %d claims, want the one confirmation", len(confirmed))
	}
}
