package supervise

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Every registration the claim persists is told to Deps.Registered with the claim as written — the
// first one and the recorded session registering again, which may have moved its file — and
// nothing else is: a suspension and the issue's close, which forgets the claim's session, tell it
// nothing, so the daemon's record of the conversation outlives both.
func TestEveryRegistrationAndNothingElseIsToldToRegistered(t *testing.T) {
	h := newBareHarness(t)
	var told []Claim
	h.deps.Registered = func(_ context.Context, c Claim) { told = append(told, c) }
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())

	h.reach(StateRegistered)
	if len(told) != 1 || told[0].Session != session || told[0].SessionFile != sessionFile || told[0].State != StateRegistered || told[0].Token != testToken {
		t.Fatalf("told after the registration = %+v, want the registered claim once", told)
	}
	h.ready()
	h.must(RequestRegister{Claim: testToken, Generation: h.generation(), Session: session, SessionFile: "/state/sessions/moved.jsonl",
		CapabilityHash: []byte("capability-2"), PluginContract: h.deps.PluginContract})
	if len(told) != 2 || told[1].Session != session || told[1].SessionFile != "/state/sessions/moved.jsonl" {
		t.Fatalf("told after the re-registration = %+v, want it told with the moved file", told)
	}

	h.must(RequestSuspend{Claim: testToken, By: SuspendedByOperator})
	h.wantState(StateSuspended)
	h.must(RequestIssueClose{Claim: testToken})
	h.wantState(StateRetired)
	if len(told) != 2 {
		t.Errorf("told after the suspension and the close = %+v, want nothing more", told[2:])
	}
}

// Deps.Registered writes the conversation's row while the machine decides and holds its lock, so it
// runs under the claim-write bound as every store write of a decision does (Machine.write): a
// database that has stopped answering holds the registration one bound, not the length of the
// outage. The call is made once and its failure answers nothing: the claim's registration has
// landed, and the next boot's backfill records the conversation. Here Deps.Registered returns only
// when its context ends, and the registration still answers within a small multiple of the bound.
func TestARegisteredThatNeverAnswersEndsAtTheClaimWriteBound(t *testing.T) {
	const bound = 200 * time.Millisecond
	const answerWithin = 10 * bound
	h := newBareHarness(t)
	h.deps.Timeouts.ClaimWrite = bound
	type told struct {
		left  time.Duration
		ended error
	}
	telling := make(chan told, 1)
	h.deps.Registered = func(ctx context.Context, _ Claim) {
		left := time.Duration(-1)
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
		<-ctx.Done()
		telling <- told{left: left, ended: ctx.Err()}
	}
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	h.reach(StateShimConnected)

	registration := RequestRegister{Claim: testToken, Generation: h.generation(), Session: session, SessionFile: sessionFile,
		CapabilityHash: []byte("capability"), PluginContract: h.deps.PluginContract}
	answered, done := make(chan error, 1), make(chan struct{})
	// A registration that never answers is let go by the daemon's stop, so the test ends.
	t.Cleanup(func() { h.shutdown(); <-done })
	go func() {
		defer close(done)
		answered <- h.handle(registration)
	}()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the registration = %v, want it answered", err)
		}
	case <-time.After(answerWithin):
		t.Fatalf("the registration did not answer within %s of a %s claim-write bound: Deps.Registered held the machine's lock past the bound", answerWithin, bound)
	}
	got := <-telling
	if got.left <= 0 || got.left > bound || !errors.Is(got.ended, context.DeadlineExceeded) {
		t.Fatalf("Deps.Registered began with %s left on its deadline and ended with %v, want at most the %s claim-write bound and its deadline", got.left, got.ended, bound)
	}
	h.wantState(StateRegistered)
	if stored := h.store.load(testToken); stored.State != StateRegistered || stored.Session != session {
		t.Fatalf("the stored claim reads %s with session %q, want registered with %q", stored.State, stored.Session, session)
	}
}
