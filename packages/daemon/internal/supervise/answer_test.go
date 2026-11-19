package supervise

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The machine keeps the model that answered last, as the agent's stream names it: the
// record of a step names the model that served it. A fresh session answers with its own model, so
// what the session before it answered is no answer of the claim's any more, also when that answer
// reaches the machine after the fresh session registered. The read never waits on a decision, so
// a transaction that writes a step record may make it while the machine is deciding.
func TestTheMachineKeepsTheLastAnswer(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	if got := h.m.LastAnswer(); got != (Answer{}) {
		t.Fatalf("the answer before any = %+v, want none", got)
	}

	h.must(StreamAnswered{Claim: h.token, Model: "a/1", Generation: h.generation()})
	h.must(StreamAnswered{Claim: h.token, Model: "b/2", Generation: h.generation()})
	if got := h.m.LastAnswer(); got.Model != "b/2" {
		t.Fatalf("the answer after two = %+v, want b/2", got)
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

	replaced := h.generation()
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
	// The registration is handled on its own HTTP request (api's claims register), while an
	// answer of the replaced process waits behind the stream's queue and the claim's inbox, so the
	// answer can reach the machine after the fresh session registered.
	h.must(StreamAnswered{Claim: h.token, Model: "old/late", Generation: replaced})
	if got := h.m.LastAnswer(); got != (Answer{}) {
		t.Fatalf("the fresh session's answer after a late answer of the replaced launch = %+v, want none: old/late answered the session before it", got)
	}
	h.must(StreamAnswered{Claim: h.token, Model: "c/3", Generation: h.generation()})
	if got := h.m.LastAnswer(); got.Model != "c/3" {
		t.Fatalf("the fresh session's answer = %+v, want c/3", got)
	}
	// The shim reconnecting keeps the launch and its session, so an answer read on the connection
	// the reconnect replaced, and handled after it, is still the session's.
	h.conns.Remove(h.token)
	h.conns.Register(h.token, h.conn)
	h.must(StreamAnswered{Claim: h.token, Model: "d/4", Generation: h.generation()})
	if got := h.m.LastAnswer(); got.Model != "d/4" {
		t.Fatalf("the answer of the session's launch read before its shim reconnected = %+v, want d/4", got)
	}
}

// A resume continues the session it resumes, so an answer an earlier launch of that session gave
// is the session's own, also when it reaches the machine after the operator's suspend and resume
// did: those are handled on their own requests, while the answer waits behind the stream's queue
// and the claim's inbox. The session stays and the generation moves, and the queued answer is the
// last one until the resumed process answers. The stream keeps the order a claim's frames arrive
// in across its connections (stream.Listener's register), so no answer of the earlier launch comes
// after one of the resumed process.
func TestAQueuedAnswerSurvivesAResumeOfTheSameSession(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	before, generation := h.claim().Session, h.generation()
	h.must(StreamAnswered{Claim: h.token, Model: "earlier/1", Generation: generation})
	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator})
	h.must(RequestResume{Claim: h.token})
	h.wantCalls("Resume", 1)
	h.relaunched()
	if before == "" || h.claim().Session != before || h.generation() == generation {
		t.Fatalf("the fixture resumed session %q at generation %d from %q at %d, want the same session at another launch",
			h.claim().Session, h.generation(), before, generation)
	}
	h.must(StreamAnswered{Claim: h.token, Model: "queued/2", Generation: generation})
	if got := h.m.LastAnswer(); got.Model != "queued/2" {
		t.Fatalf("the answer queued behind the resume = %+v, want queued/2: an earlier launch of the same session gave it", got)
	}
	h.must(StreamAnswered{Claim: h.token, Model: "resumed/3", Generation: h.generation()})
	if got := h.m.LastAnswer(); got.Model != "resumed/3" {
		t.Fatalf("the resumed process's answer = %+v, want resumed/3", got)
	}
}

// A resume the runtime answers with a missing session spawns a fresh agent in the same try
// (Machine.startFresh), so that launch starts a session of its own: an answer of the lost
// session's launch is no answer of the fresh one, and the fresh agent's own answers are.
func TestAnAnswerOfALostSessionIsNoneOfTheFreshAgentThatReplacedIt(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	lost := h.generation()
	h.must(StreamAnswered{Claim: h.token, Model: "lost/1", Generation: lost})
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no OMP session anywhere", testToken, runtime.ErrSessionFileMissing)})
	h.observe(runtime.Gone)
	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	h.registerFresh()
	if h.generation() == lost || h.claim().Session == session {
		t.Fatalf("the fixture is at generation %d with session %q, want a fresh session at another launch", h.generation(), h.claim().Session)
	}
	h.must(StreamAnswered{Claim: h.token, Model: "lost/late", Generation: lost})
	if got := h.m.LastAnswer(); got != (Answer{}) {
		t.Fatalf("the fresh agent's answer after a late answer of the lost session = %+v, want none", got)
	}
	h.must(StreamAnswered{Claim: h.token, Model: "fresh/2", Generation: h.generation()})
	if got := h.m.LastAnswer(); got.Model != "fresh/2" {
		t.Fatalf("the fresh agent's answer = %+v, want fresh/2", got)
	}
}
