package supervise

import (
	"errors"
	"testing"
)

// Once an operator suspend is held through a rule-resume launch, a later daemon linger/leave
// suspend cannot overwrite its source before ready.
func TestAnOperatorLaunchHoldOutranksADaemonSuspendBeforeReady(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("operator suspend = %v, want ErrSuspendHeld", err)
	}
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the tree lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("daemon suspend = %v, want ErrSuspendHeld", err)
	}
	h.connect()
	h.register()
	h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
	h.wantState(StateSuspended)
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("the claim records %q, want operator", got)
	}
}

// The same precedence applies after registration, and after ready while the runtime refused the
// operator's stop: the suspension's record is written first, so the claim is suspended by the
// operator with its stop owed (D7 of dispatch://INFRA-4099's plan), and no later daemon suspend may
// replace that source.
func TestAnOperatorLaunchHoldOutranksADaemonSuspendAfterRegistration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
	}{
		{name: "registered"},
		{name: "ready whose stop the runtime refused", ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, SuspendedByDaemon)
			h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
			if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("operator suspend = %v, want ErrSuspendHeld", err)
			}
			h.connect()
			h.register()
			if tc.ready {
				h.rt.FailSuspend(errors.New("the first stop failed"))
				h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
				h.rt.FailSuspend(nil)
			}
			if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the tree lingers", By: SuspendedByDaemon}); err != nil && !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("daemon suspend = %v", err)
			}
			if got := h.claim().SuspendedBy; got != SuspendedByOperator && got != "" {
				t.Fatalf("the claim records %q before the hold completes, want the operator held", got)
			}
			if h.claim().State == StateRegistered {
				h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
			} else if h.claim().State == StateReady {
				h.must(RequestReady{Claim: testToken, Generation: h.generation(), Session: session})
			}
			h.wantState(StateSuspended)
			if got := h.claim().SuspendedBy; got != SuspendedByOperator {
				t.Fatalf("the claim records %q, want operator", got)
			}
		})
	}
}
