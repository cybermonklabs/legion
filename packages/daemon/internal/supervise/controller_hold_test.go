package supervise

import (
	"errors"
	"testing"
)

// The controller's suspension holds as the operator's does: the rule resumes it no more, the
// workflow's task waits it out (ErrHeldByController), and the phase's task goes through it kept,
// since a resume puts the claim back on the phase it was suspended in.
func TestTheControllersSuspensionHoldsAsTheOperatorsDoes(t *testing.T) {
	if !SuspendedByController.Holds() || SuspendedByController.RetiresPhaseTask() || SuspendedByController.ByRule() {
		t.Fatalf("the controller's source holds %t, retires the phase's task %t, is the rule's %t; want true, false, false",
			SuspendedByController.Holds(), SuspendedByController.RetiresPhaseTask(), SuspendedByController.ByRule())
	}
	h := newHarness(t)
	suspendedBy(h, SuspendedByController)
	if c := h.claim(); !c.RuleHolds() || c.RuleMayResume() || c.HeldAgainst(SuspendedByController) != nil {
		t.Fatalf("the controller's suspension: rule holds %t, rule may resume %t, held against the controller %v; want true, false, nil",
			c.RuleHolds(), c.RuleMayResume(), c.HeldAgainst(SuspendedByController))
	}
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	h.wantState(StateSuspended)
	err := h.handle(RequestDeliver{Claim: testToken, Task: "Test it.", ID: "outbox:7", Phase: "testing", Generation: 1})
	if !errors.Is(err, ErrHeldByController) {
		t.Fatalf("the workflow's task returned %v, want ErrHeldByController", err)
	}
	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
}

// The controller's resume lifts every suspension but the operator's own, which it is refused
// (ErrOperatorHolds) with nothing of the claim changed; the operator's resume lifts the
// controller's as it lifts every other.
func TestTheControllersResumeLiftsEverySuspensionButTheOperators(t *testing.T) {
	for _, by := range []SuspendSource{SuspendedByController, SuspendedByArchitect, SuspendedByDaemon, SuspendedByReleased} {
		t.Run(string(by), func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, by)
			h.must(RequestResume{Claim: testToken, By: SuspendedByController})
			h.wantState(StateLaunching)
			h.wantCalls("Resume", 1)
		})
	}

	t.Run("operator", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByOperator)
		err := h.handle(RequestResume{Claim: testToken, By: SuspendedByController})
		var refused *RefusedError
		if !errors.As(err, &refused) || !errors.Is(err, ErrOperatorHolds) {
			t.Fatalf("the controller's resume of the operator's suspension returned %v, want a refusal naming ErrOperatorHolds", err)
		}
		h.wantState(StateSuspended)
		h.wantCalls("Resume", 0)
		if got := h.store.load(testToken).SuspendedBy; got != SuspendedByOperator {
			t.Fatalf("after the refusal the store records %q, want the operator still", got)
		}
	})

	t.Run("the operator's resume of the controller's", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByController)
		h.must(RequestResume{Claim: testToken, By: SuspendedByOperator})
		h.wantState(StateLaunching)
	})
}

// The controller's suspend of a claim the operator holds suspended changes nothing and is refused
// (ErrOperatorHolds), landed or held for the agent's turn; the operator's suspend of a claim the
// controller holds takes it over, landed or held, so the controller's resume lifts it no more.
func TestTheOperatorsSuspensionOutranksTheControllers(t *testing.T) {
	t.Run("the controller's suspend of the operator's landed suspension", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByOperator)
		err := h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController})
		if !errors.Is(err, ErrOperatorHolds) {
			t.Fatalf("the controller's suspend returned %v, want ErrOperatorHolds", err)
		}
		if got := h.store.load(testToken).SuspendedBy; got != SuspendedByOperator {
			t.Fatalf("after the refusal the store records %q, want the operator still", got)
		}
	})

	t.Run("the controller's suspend of the operator's held suspension", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the operator's suspend of a claim in a turn returned %v, want it held", err)
		}
		err := h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController})
		if !errors.Is(err, ErrOperatorHolds) {
			t.Fatalf("the controller's suspend returned %v, want ErrOperatorHolds", err)
		}
		if c := h.claim(); c.State != StateWorking || !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByOperator {
			t.Fatalf("the claim is %s holding a suspension %t by %q, want it working under the operator's", c.State, c.SuspensionHeld, c.SuspensionHeldBy)
		}
	})

	t.Run("the operator's suspend of the controller's landed suspension", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByController)
		h.must(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator})
		if got := h.store.load(testToken).SuspendedBy; got != SuspendedByOperator {
			t.Fatalf("after the operator's suspend the store records %q, want the operator", got)
		}
		if err := h.handle(RequestResume{Claim: testToken, By: SuspendedByController}); !errors.Is(err, ErrOperatorHolds) {
			t.Fatalf("the controller's resume returned %v, want ErrOperatorHolds", err)
		}
		h.wantState(StateSuspended)
	})

	t.Run("the operator's suspend of the controller's held suspension", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the controller's suspend of a claim in a turn returned %v, want it held", err)
		}
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the operator's suspend of a claim in a turn returned %v, want it held", err)
		}
		if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByOperator {
			t.Fatalf("the claim holds a suspension %t by %q, want the operator's", c.SuspensionHeld, c.SuspensionHeldBy)
		}
	})

	t.Run("the daemon's suspend of the controller's held suspension", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the controller's suspend of a claim in a turn returned %v, want it held", err)
		}
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the tree lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's suspend of a claim in a turn returned %v, want it held", err)
		}
		if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByController {
			t.Fatalf("the claim holds a suspension %t by %q, want the controller's still", c.SuspensionHeld, c.SuspensionHeldBy)
		}
	})
}

// The controller's stop of a claim the operator's own suspension holds is refused
// (ErrOperatorHolds): a stop retires the claim, and a retry then relaunches it, past the operator's
// hold. Its stop of a claim it suspended itself retires it, and the operator's stop is refused
// nothing here.
func TestTheControllersStopOfAClaimTheOperatorHoldsIsRefused(t *testing.T) {
	t.Run("the operator's landed suspension", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByOperator)
		if err := h.handle(RequestStop{Claim: testToken, By: SuspendedByController}); !errors.Is(err, ErrOperatorHolds) {
			t.Fatalf("the controller's stop returned %v, want ErrOperatorHolds", err)
		}
		h.wantState(StateSuspended)
	})

	t.Run("the operator's held suspension", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the operator's suspend of a claim in a turn returned %v, want it held", err)
		}
		if err := h.handle(RequestStop{Claim: testToken, By: SuspendedByController}); !errors.Is(err, ErrOperatorHolds) {
			t.Fatalf("the controller's stop returned %v, want ErrOperatorHolds", err)
		}
		h.wantState(StateWorking)
	})

	t.Run("the controller's own suspension", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByController)
		h.must(RequestStop{Claim: testToken, By: SuspendedByController})
		h.wantState(StateRetired)
	})

	t.Run("the operator's stop", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByOperator)
		h.must(RequestStop{Claim: testToken, By: SuspendedByOperator})
		h.wantState(StateRetired)
	})
}

// A forget of the session asked by the controller or an architect — the step of its fresh session
// that drops the session it means to leave behind — is refused, with the session kept, for a
// claim a suspension above the asker took over after the asker's own suspension (suspendedAgain,
// RequestForgetSession.By, Claim.ResumeRefusal): the asker's resume would be refused that hold,
// so a forget let through would drop the session of a claim the asker could then not start. The
// holder's own forget drops it.
func TestAForgetIsRefusedForAClaimASuspensionAboveTheAskerTookOver(t *testing.T) {
	for _, tc := range []struct {
		asker, holder SuspendSource
		want          error
	}{
		{SuspendedByController, SuspendedByOperator, ErrOperatorHolds},
		{SuspendedByArchitect, SuspendedByController, ErrControllerHolds},
		{SuspendedByArchitect, SuspendedByOperator, ErrOperatorHolds},
	} {
		t.Run(string(tc.holder)+" over "+string(tc.asker), func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, tc.asker)
			h.must(RequestSuspend{Claim: testToken, Reason: "a suspension above the asker takes the claim over", By: tc.holder})
			recorded := h.claim().Session

			err := h.handle(RequestForgetSession{Claim: testToken, Session: recorded, By: tc.asker})
			var refused *RefusedError
			if !errors.As(err, &refused) || !errors.Is(err, tc.want) {
				t.Fatalf("the %s's forget returned %v, want a refusal naming %v", tc.asker, err, tc.want)
			}
			if got := h.store.load(testToken); got.State != StateSuspended || got.SuspendedBy != tc.holder || got.Session != recorded {
				t.Fatalf("after the refusal the store records %s by %q on session %q, want suspended by %s on %q",
					got.State, got.SuspendedBy, got.Session, tc.holder, recorded)
			}

			h.must(RequestForgetSession{Claim: testToken, Session: recorded, By: tc.holder})
			if got := h.store.load(testToken).Session; got != "" {
				t.Fatalf("after the holder's forget the store records session %q, want none", got)
			}
		})
	}
}

// startInFlight places a start in flight on h's claim as far as a suspension can see one: the
// cancel a launch registers under startMu (stopStart), here one that counts its calls. It is
// cleared when the test ends.
func startInFlight(t *testing.T, h *harness) *int {
	t.Helper()
	cancelled := 0
	h.m.startMu.Lock()
	h.m.cancelStart, h.m.startStopped = func() { cancelled++ }, false
	h.m.startMu.Unlock()
	t.Cleanup(func() {
		h.m.startMu.Lock()
		h.m.cancelStart, h.m.startStopped = nil, false
		h.m.startMu.Unlock()
	})
	return &cancelled
}

// The controller's suspension of a claim the operator holds is refused, and the refusal changes
// nothing: a start in flight is not cancelled before the row refuses it, and no stopped start is
// left for a later suspension to read (Handle, stoppedAStart). The operator's own suspension of
// the same claim still cancels the start at its arrival, so the gate is the controller's alone.
func TestTheControllersRefusedSuspensionCancelsNoStartInFlight(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(h *harness)
	}{
		{"the operator's landed suspension", func(h *harness) { suspendedBy(h, SuspendedByOperator) }},
		{"the operator's held suspension", func(h *harness) {
			h.reach(StateWorking)
			if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
				h.t.Fatalf("the operator's suspend of a claim in a turn returned %v, want it held", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.hold(h)
			cancelled := startInFlight(t, h)

			err := h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController})

			if !errors.Is(err, ErrOperatorHolds) {
				t.Fatalf("the controller's suspend returned %v, want ErrOperatorHolds", err)
			}
			if *cancelled != 0 {
				t.Fatalf("the refused suspension cancelled the start in flight %d times, want none", *cancelled)
			}
			if h.m.stoppedAStart() {
				t.Fatal("the refused suspension left a stopped start for the next suspension to read")
			}

			// The negative control: the operator's own suspension cancels the start where it arrives.
			startInFlight(t, h)
			_ = h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator})
			if !h.m.stoppedAStart() {
				t.Fatal("the operator's suspension cancelled no start in flight, want it cancelled at its arrival")
			}
		})
	}
}
