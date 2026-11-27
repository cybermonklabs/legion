package supervise

import (
	"errors"
	"testing"
)

// A runtime suspend that fails while a rule-resuming agent becomes ready leaves the operator's
// launch-time hold written: the suspension is recorded first, so the claim is suspended by the
// operator in memory and in the row, and the stop the runtime refused is made again at the probe
// interval (D6 and D7 of dispatch://INFRA-4099's plan). The claim must not remain ready with an
// in-memory-only operator hold.
func TestAReadyTimeOperatorSuspendFailureRetriesItsHold(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend during launch = %v, want ErrSuspendHeld", err)
	}
	h.connect()
	h.register()
	h.rt.FailSuspend(errors.New("the runtime refused the stop"))
	suspends := len(h.calls("Suspend"))
	if err := h.handle(RequestReady{Claim: testToken, Generation: h.generation(), Session: session}); err != nil {
		t.Fatalf("ready with a refused operator stop = %v, want none: the suspension is written", err)
	}
	for what, got := range map[string]Claim{"memory": h.claim(), "the row": h.store.load(testToken)} {
		if got.State != StateSuspended || got.SuspendedBy != SuspendedByOperator {
			t.Fatalf("after the refused ready-time stop %s reads %s by %q, want suspended by the operator", what, got.State, got.SuspendedBy)
		}
	}
	h.wantCalls("Suspend", suspends+1)
	h.rt.FailSuspend(nil)
	h.advance(testProbe)
	h.wantCalls("Suspend", suspends+2)
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the retry left the claim suspended by %q, want operator", got)
	}
}

// If a rule-resuming agent never becomes ready, its registration deadline completes the retained
// operator suspend instead of dropping it and relaunching. The operator's hold is durable and the
// launch budget is unchanged.
func TestRegistrationDeadlineCompletesTheHeldOperatorSuspend(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend during launch = %v, want ErrSuspendHeld", err)
	}
	h.advance(deadline)
	h.wantState(StateSuspended)
	if got := h.claim(); got.SuspendedBy != SuspendedByOperator || got.Budgets.LaunchFailures != 0 {
		t.Fatalf("after the deadline %+v, want the operator suspension and no launch failure", got)
	}
	h.wantCalls("Resume", 1)
	h.wantCalls("Suspend", 2)
}
