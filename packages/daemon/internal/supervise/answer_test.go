package supervise

import (
	"testing"
	"time"
)

// The machine keeps the model that answered last, as the agent's stream names it, and when: the
// record of a step names the model that served it. A fresh session answers with its own model, so
// what the session before it answered is no answer of the claim's any more. The read never waits
// on a decision, so a transaction that writes a step record may make it while the machine is
// deciding.
func TestTheMachineKeepsTheLastAnswer(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	if got := h.m.LastAnswer(); got != (Answer{}) {
		t.Fatalf("the answer before any = %+v, want none", got)
	}

	h.must(StreamAnswered{Claim: h.token, Model: "a/1"})
	h.advance(time.Second)
	answeredAt := h.clock.Now()
	h.must(StreamAnswered{Claim: h.token, Model: "b/2"})
	if got := h.m.LastAnswer(); got.Model != "b/2" || !got.At.Equal(answeredAt) {
		t.Fatalf("the answer after two = %+v, want b/2 at %s", got, answeredAt)
	}

	read := make(chan Answer, 1)
	h.m.mu.Lock()
	go func() { read <- h.m.LastAnswer() }()
	select {
	case got := <-read:
		if got.Model != "b/2" {
			t.Errorf("the answer read under a decision = %+v, want b/2", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("the answer read waited on the decision lock")
	}
	h.m.mu.Unlock()

	h.must(RequestStop{Claim: h.token})
	h.must(RequestRetry{Claim: h.token, FreshSession: true})
	h.wantState(StateLaunching)
	if got := h.m.LastAnswer(); got != (Answer{}) {
		t.Fatalf("the answer once the session was abandoned = %+v, want none", got)
	}
	h.registerFresh()
	if got := h.m.LastAnswer(); got != (Answer{}) {
		t.Fatalf("the answer of the fresh session before it answered = %+v, want none: b/2 answered the session before it", got)
	}
	h.must(StreamAnswered{Claim: h.token, Model: "c/3"})
	if got := h.m.LastAnswer(); got.Model != "c/3" {
		t.Fatalf("the fresh session's answer = %+v, want c/3", got)
	}
}
