package supervise

import (
	"errors"
	"testing"
)

// A second ready while a launch-time hold is pending completes that hold; it must not publish ready
// and release catch-up/notice work to a claim that is being stopped. The first ready's stop the
// runtime refuses: the hold is written at that ready all the same, the claim suspended by the
// operator with its stop owed (D6 and D7 of dispatch://INFRA-4099's plan), so the second ready
// finds it suspended and moves it nowhere.
func TestARereadyCompletesThePendingLaunchTimeHold(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend during launch = %v, want ErrSuspendHeld", err)
	}
	h.connect()
	h.register()
	h.rt.FailSuspend(errors.New("the first stop failed"))
	h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
	h.rt.FailSuspend(nil)
	h.wantState(StateSuspended)
	_ = h.handle(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the re-ready left the claim suspended by %q, want operator", got)
	}
}
