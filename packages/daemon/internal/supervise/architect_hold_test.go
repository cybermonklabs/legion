package supervise

import (
	"errors"
	"strings"
	"testing"
)

// The ranks run released, architect, daemon, controller, operator, weakest first: an architect's
// suspension outranks a release's and ranks under the daemon's, which is a park, a linger, a close
// or a carry of the claim's issue, under the controller's, whose suspension is project-wide work a
// tree's architect must not undo, and under the operator's.
func TestSuspensionRankPutsTheArchitectUnderTheDaemonAndTheController(t *testing.T) {
	ordered := []SuspendSource{SuspendedByReleased, SuspendedByArchitect, SuspendedByDaemon, SuspendedByController, SuspendedByOperator}
	for i, source := range ordered {
		if got, want := suspensionRank(source), i+1; got != want {
			t.Errorf("suspensionRank(%q) = %d, want %d", source, got, want)
		}
	}
}

// A start or a delivery that meets a suspension that holds waits, and RuleHoldError says why: it
// names the source that holds the claim, landed or held for its agent's turn, and wraps that
// holder's own wait, which says whose resume lifts it. A controller's or an architect's hold
// named as the operator's would send its reader to the operator for a resume the controller or
// an architect makes.
//
// The negative control is RuleHoldError without its controller's and architect's arms: their rows
// then wrap the operator's wait.
func TestARuleHoldErrorNamesTheSuspensionThatHolds(t *testing.T) {
	for _, tc := range []struct {
		by   SuspendSource
		wait error
	}{
		{SuspendedByOperator, ErrHeldByOperator},
		{SuspendedByController, ErrHeldByController},
		{SuspendedByArchitect, ErrHeldByArchitect},
	} {
		for _, c := range []Claim{
			{State: StateSuspended, SuspendedBy: tc.by},
			{State: StateWorking, SuspensionHeld: true, SuspensionHeldBy: tc.by},
		} {
			err := c.RuleHoldError()
			if want := "the claim is suspended by " + string(tc.by) + ";"; !errors.Is(err, tc.wait) || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("RuleHoldError of a claim %s with a suspension held %t, held by %q, suspended by %q = %v; want it to begin %q and wrap %v",
					c.State, c.SuspensionHeld, c.SuspensionHeldBy, c.SuspendedBy, err, want, tc.wait)
			}
		}
	}
}

// An architect's suspension holds as the controller's does: the rule resumes it no more, the
// workflow's task waits it out (ErrHeldByArchitect), and the phase's task goes through it kept.
func TestAnArchitectsSuspensionHoldsAsTheControllersDoes(t *testing.T) {
	if !SuspendedByArchitect.Holds() || SuspendedByArchitect.RetiresPhaseTask() || SuspendedByArchitect.ByRule() {
		t.Fatalf("the architect's source holds %t, retires the phase's task %t, is the rule's %t; want true, false, false",
			SuspendedByArchitect.Holds(), SuspendedByArchitect.RetiresPhaseTask(), SuspendedByArchitect.ByRule())
	}
	h := newHarness(t)
	suspendedBy(h, SuspendedByArchitect)
	if c := h.claim(); !c.RuleHolds() || c.RuleMayResume() || c.HeldAgainst(SuspendedByArchitect) != nil || c.HeldAgainst(SuspendedByController) != nil {
		t.Fatalf("the architect's suspension: rule holds %t, rule may resume %t, held against an architect %v, against the controller %v; want true, false, nil, nil",
			c.RuleHolds(), c.RuleMayResume(), c.HeldAgainst(SuspendedByArchitect), c.HeldAgainst(SuspendedByController))
	}
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
	h.wantState(StateSuspended)
	err := h.handle(RequestDeliver{Claim: testToken, Task: "Test it.", ID: "outbox:7", Phase: "testing", Generation: 1})
	if !errors.Is(err, ErrHeldByArchitect) {
		t.Fatalf("the workflow's task returned %v, want ErrHeldByArchitect", err)
	}
	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
}

// An architect's resume lifts an architect's suspension and nothing else: the controller's and the
// operator's refuse it in their own words (ErrControllerHolds, ErrOperatorHolds), and one the
// daemon placed refuses it too (ErrNotTheArchitectsSuspension), each with nothing of the claim
// changed. The controller's resume and the operator's lift an architect's suspension.
func TestAnArchitectsResumeLiftsOnlyAnArchitectsSuspension(t *testing.T) {
	t.Run("an architect's", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByArchitect)
		h.must(RequestResume{Claim: testToken, By: SuspendedByArchitect})
		h.wantState(StateLaunching)
		h.wantCalls("Resume", 1)
	})

	for _, tc := range []struct {
		by   SuspendSource
		want error
	}{
		{SuspendedByController, ErrControllerHolds},
		{SuspendedByOperator, ErrOperatorHolds},
		{SuspendedByDaemon, ErrNotTheArchitectsSuspension},
		{SuspendedByReleased, ErrNotTheArchitectsSuspension},
	} {
		t.Run(string(tc.by), func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, tc.by)
			err := h.handle(RequestResume{Claim: testToken, By: SuspendedByArchitect})
			var refused *RefusedError
			if !errors.As(err, &refused) || !errors.Is(err, tc.want) {
				t.Fatalf("the architect's resume of a suspension by %s returned %v, want a refusal naming %v", tc.by, err, tc.want)
			}
			h.wantState(StateSuspended)
			h.wantCalls("Resume", 0)
			if got := h.store.load(testToken).SuspendedBy; got != tc.by {
				t.Fatalf("after the refusal the store records %q, want %q still", got, tc.by)
			}
		})
	}

	for _, tc := range []struct {
		name string
		by   SuspendSource
	}{
		{"the controller's resume of an architect's", SuspendedByController},
		{"the operator's resume of an architect's", SuspendedByOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, SuspendedByArchitect)
			h.must(RequestResume{Claim: testToken, By: tc.by})
			h.wantState(StateLaunching)
		})
	}
}

// The controller's suspension outranks an architect's: an architect's suspend of a claim the
// controller holds, landed or held for the agent's turn, is refused (ErrControllerHolds) and one
// the operator holds is refused too (ErrOperatorHolds); the controller's suspend of a claim an
// architect holds takes it over, landed or held, so an architect's resume lifts it no more, and so
// does the daemon's suspend of an architect's held suspension. An architect's suspend takes over a
// release's, which ranks under it, and its resume then lifts it.
func TestTheControllersSuspensionOutranksAnArchitects(t *testing.T) {
	for _, tc := range []struct {
		holder SuspendSource
		want   error
	}{
		{SuspendedByController, ErrControllerHolds},
		{SuspendedByOperator, ErrOperatorHolds},
	} {
		t.Run("an architect's suspend of the "+string(tc.holder)+"'s landed suspension", func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, tc.holder)
			err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect})
			if !errors.Is(err, tc.want) {
				t.Fatalf("the architect's suspend returned %v, want %v", err, tc.want)
			}
			if got := h.store.load(testToken).SuspendedBy; got != tc.holder {
				t.Fatalf("after the refusal the store records %q, want %q still", got, tc.holder)
			}
		})

		t.Run("an architect's suspend of the "+string(tc.holder)+"'s held suspension", func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateWorking)
			if err := h.handle(RequestSuspend{Claim: testToken, Reason: "held first", By: tc.holder}); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the %s's suspend of a claim in a turn returned %v, want it held", tc.holder, err)
			}
			err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect})
			if !errors.Is(err, tc.want) {
				t.Fatalf("the architect's suspend returned %v, want %v", err, tc.want)
			}
			if c := h.claim(); c.State != StateWorking || !c.SuspensionHeld || c.SuspensionHeldBy != tc.holder {
				t.Fatalf("the claim is %s holding a suspension %t by %q, want it working under the %s's", c.State, c.SuspensionHeld, c.SuspensionHeldBy, tc.holder)
			}
		})
	}

	t.Run("the controller's suspend of an architect's landed suspension", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByArchitect)
		h.must(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController})
		if got := h.store.load(testToken).SuspendedBy; got != SuspendedByController {
			t.Fatalf("after the controller's suspend the store records %q, want the controller", got)
		}
		if err := h.handle(RequestResume{Claim: testToken, By: SuspendedByArchitect}); !errors.Is(err, ErrControllerHolds) {
			t.Fatalf("the architect's resume returned %v, want ErrControllerHolds", err)
		}
		h.wantState(StateSuspended)
	})

	t.Run("the controller's suspend of an architect's held suspension", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the architect's suspend of a claim in a turn returned %v, want it held", err)
		}
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the controller's suspend of a claim in a turn returned %v, want it held", err)
		}
		if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByController {
			t.Fatalf("the claim holds a suspension %t by %q, want the controller's", c.SuspensionHeld, c.SuspensionHeldBy)
		}
	})

	t.Run("the daemon's suspend of an architect's held suspension", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the architect's suspend of a claim in a turn returned %v, want it held", err)
		}
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the tree lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's suspend of a claim in a turn returned %v, want it held", err)
		}
		if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByDaemon {
			t.Fatalf("the claim holds a suspension %t by %q, want the daemon's, which took the architect's over", c.SuspensionHeld, c.SuspensionHeldBy)
		}
	})

	t.Run("an architect's suspend of a released claim", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByReleased)
		h.must(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect})
		if got := h.store.load(testToken).SuspendedBy; got != SuspendedByArchitect {
			t.Fatalf("after the architect's suspend the store records %q, want the architect", got)
		}
		h.must(RequestResume{Claim: testToken, By: SuspendedByArchitect})
		h.wantState(StateLaunching)
	})
}

// An architect does not take over the daemon's own suspension, which outranks it, nor end the
// claim under it: its suspend or stop of a claim the daemon parked, lingers or closed, a root's
// exit suspended, or one suspended before sources existed is refused
// (ErrNotTheArchitectsSuspension) with nothing of the claim changed, and so is either over the
// daemon's suspension held for the agent's turn. Taken over, the claim would be the architect's,
// whose resume would restart a worker on a parked or closed issue, and a stop would retire it for
// a retry to relaunch. The refused suspend cancels no start in flight either (Handle). The other
// order, an architect's suspension the daemon's then reaches, is
// TestTheDaemonsSuspensionTakesOverAnArchitectsPlacedBeforeIt.
func TestAnArchitectDoesNotTakeOverTheDaemonsSuspension(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness func(t *testing.T) *harness
		suspend func(h *harness)
		want    SuspendSource
	}{
		{"the daemon's", newHarness, func(h *harness) { suspendedBy(h, SuspendedByDaemon) }, SuspendedByDaemon},
		{"unknown, before migration 0038", newHarness, func(h *harness) {
			suspendedBy(h, SuspendedByDaemon)
			c := h.claim()
			c.SuspendedBy = SuspendedByUnknown
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				h.t.Fatal(err)
			}
			h.restart()
		}, SuspendedByUnknown},
		{"a root's exit", func(t *testing.T) *harness { return newHarnessOf(t, rootClaim()) }, func(h *harness) {
			h.reach(StateIdle)
			h.exit()
			h.wantState(StateSuspended)
		}, SuspendedByExit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.harness(t)
			tc.suspend(h)
			cancelled := startInFlight(t, h)

			err := h.handle(RequestSuspend{Claim: h.token, Reason: "an architect suspended it", By: SuspendedByArchitect})

			var refused *RefusedError
			if !errors.As(err, &refused) || !errors.Is(err, ErrNotTheArchitectsSuspension) {
				t.Fatalf("the architect's suspend returned %v, want a refusal naming ErrNotTheArchitectsSuspension", err)
			}
			if got := h.store.load(h.token).SuspendedBy; got != tc.want {
				t.Fatalf("after the refusal the store records %q, want %q still", got, tc.want)
			}
			if *cancelled != 0 {
				t.Fatalf("the refused suspension cancelled the start in flight %d times, want none", *cancelled)
			}
			if err := h.handle(RequestResume{Claim: h.token, By: SuspendedByArchitect}); !errors.Is(err, ErrNotTheArchitectsSuspension) {
				t.Fatalf("the architect's resume returned %v, want ErrNotTheArchitectsSuspension", err)
			}
			if err := h.handle(RequestStop{Claim: h.token, By: SuspendedByArchitect}); !errors.As(err, &refused) || !errors.Is(err, ErrNotTheArchitectsSuspension) {
				t.Fatalf("the architect's stop returned %v, want a refusal naming ErrNotTheArchitectsSuspension", err)
			}
			if got := h.store.load(h.token); got.State != StateSuspended || got.SuspendedBy != tc.want {
				t.Fatalf("after the refused stop the store records %s by %q, want suspended by %q still", got.State, got.SuspendedBy, tc.want)
			}
			h.wantState(StateSuspended)
			h.wantCalls("Resume", 0)
		})
	}

	t.Run("the daemon's held suspension", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the tree lingers", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the daemon's suspend of a claim in a turn returned %v, want it held", err)
		}
		err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect})
		if !errors.Is(err, ErrNotTheArchitectsSuspension) {
			t.Fatalf("the architect's suspend returned %v, want ErrNotTheArchitectsSuspension", err)
		}
		if err := h.handle(RequestStop{Claim: testToken, By: SuspendedByArchitect}); !errors.Is(err, ErrNotTheArchitectsSuspension) {
			t.Fatalf("the architect's stop returned %v, want ErrNotTheArchitectsSuspension", err)
		}
		if c := h.claim(); c.State != StateWorking || !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByDaemon {
			t.Fatalf("the claim is %s holding a suspension %t by %q, want it working under the daemon's", c.State, c.SuspensionHeld, c.SuspensionHeldBy)
		}
	})
}

// The daemon's own suspension takes over an architect's placed before it, landed or held for the
// agent's turn: an architect suspends a worker of a child, and then the child is parked, its tree
// lingers or closes, or it is carried to another tree. The claim is the daemon's from then on, so
// every rule that keeps an architect off the daemon's suspension applies, on each path that would
// start the worker on the parked issue: the architect's resume is refused, and so is its stop,
// which its retry would follow; the retry then finds the claim still suspended, or still in its
// turn, and is refused as the retry of any claim that has not failed or retired is. The claim is
// left suspended by the daemon with no session resumed, and the daemon's own rules start it
// again, as they start any worker the workflow stopped (RequestRuleResume).
//
// The negative control is suspensionRank with the architect above the daemon, as it stood, and
// holdSuspension's takeover with the daemon left out: the claim then stays the architect's, whose
// resume starts the worker on the parked issue, and whose stop retires it for the retry to start.
func TestTheDaemonsSuspensionTakesOverAnArchitectsPlacedBeforeIt(t *testing.T) {
	resumeIsRefused := func(h *harness) {
		h.t.Helper()
		var refused *RefusedError
		if err := h.handle(RequestResume{Claim: testToken, By: SuspendedByArchitect}); !errors.As(err, &refused) || !errors.Is(err, ErrNotTheArchitectsSuspension) {
			h.t.Errorf("the architect's resume after the park returned %v, want a refusal naming ErrNotTheArchitectsSuspension", err)
		}
	}
	stopThenRetryIsRefused := func(h *harness) {
		h.t.Helper()
		var refused *RefusedError
		if err := h.handle(RequestStop{Claim: testToken, By: SuspendedByArchitect}); !errors.As(err, &refused) || !errors.Is(err, ErrNotTheArchitectsSuspension) {
			h.t.Errorf("the architect's stop after the park returned %v, want a refusal naming ErrNotTheArchitectsSuspension", err)
		}
		if err := h.handle(RequestAskedRetry{Claim: testToken, By: SuspendedByArchitect}); !errors.As(err, &refused) {
			h.t.Errorf("the architect's retry after its stop returned %v, want the refusal of a retry of a claim that has not failed or retired", err)
		}
	}
	for _, tc := range []struct {
		name    string
		inATurn bool
		ask     func(h *harness)
	}{
		{"landed: the architect's resume", false, resumeIsRefused},
		{"landed: the architect's stop, then its retry", false, stopThenRetryIsRefused},
		{"held for the agent's turn: the architect's stop, then its retry", true, stopThenRetryIsRefused},
		{"held for the agent's turn: the architect's resume once the turn ends", true, func(h *harness) {
			h.must(StreamTurnEnd{Claim: testToken})
			resumeIsRefused(h)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.inATurn {
				h.reach(StateWorking)
				if err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect}); !errors.Is(err, ErrSuspendHeld) {
					t.Fatalf("the architect's suspend of a claim in a turn returned %v, want it held", err)
				}
				if err := h.handle(RequestSuspend{Claim: testToken, Reason: "LEGION-209 is parked", By: SuspendedByDaemon}); !errors.Is(err, ErrSuspendHeld) {
					t.Fatalf("the park's suspend of a claim in a turn returned %v, want it held", err)
				}
				if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByDaemon {
					t.Errorf("after the park the claim holds a suspension %t by %q, want the daemon's", c.SuspensionHeld, c.SuspensionHeldBy)
				}
			} else {
				suspendedBy(h, SuspendedByArchitect)
				h.must(RequestSuspend{Claim: testToken, Reason: "LEGION-209 is parked", By: SuspendedByDaemon})
				if got := h.store.load(testToken).SuspendedBy; got != SuspendedByDaemon {
					t.Errorf("after the park the store records the claim suspended by %q, want the daemon", got)
				}
			}
			resumes := len(h.calls("Resume"))

			tc.ask(h)
			if h.state() == StateWorking {
				h.must(StreamTurnEnd{Claim: testToken})
			}

			if got := h.store.load(testToken); got.State != StateSuspended || got.SuspendedBy != SuspendedByDaemon {
				t.Fatalf("after the architect's requests the store records %s by %q, want suspended by the daemon", got.State, got.SuspendedBy)
			}
			if got := len(h.calls("Resume")); got != resumes {
				t.Fatalf("the architect's requests resumed the session %d times, want none", got-resumes)
			}
			h.must(RequestRuleResume{Claim: testToken, Reason: "LEGION-209 is back in its phase"})
			h.wantState(StateLaunching)
		})
	}
}

// A suspension that keeps a hold (RequestSuspend.KeepsHold, a production check's suspend between
// looks) takes over no source that holds: a claim the architect, the controller or the operator
// suspended stays that holder's, and its resume still lifts it; an architect's suspension held
// for the agent's turn stays the architect's and lands as its own. A released claim is still taken
// over, as every daemon suspension takes it over.
//
// The negative control is suspendedAgain and suspendRequest without their KeepsHold reads: the
// daemon's suspend then takes the architect's suspension over, landed and held.
func TestASuspensionThatKeepsHoldsTakesOverNoSourceThatHolds(t *testing.T) {
	keep := RequestSuspend{Claim: testToken, Reason: "LEGION-209's production check waits for its next look", By: SuspendedByDaemon, KeepsHold: true}
	for _, by := range []SuspendSource{SuspendedByArchitect, SuspendedByController, SuspendedByOperator} {
		t.Run("landed: "+string(by), func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, by)
			h.must(keep)
			if got := h.store.load(testToken); got.State != StateSuspended || got.SuspendedBy != by {
				t.Fatalf("after the suspend that keeps holds the store records %s by %q, want suspended by %q", got.State, got.SuspendedBy, by)
			}
			h.must(RequestResume{Claim: testToken, By: by})
			h.wantState(StateLaunching)
		})
	}
	t.Run("held for the agent's turn: an architect", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateWorking)
		if err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the architect's suspend of a claim in a turn returned %v, want it held", err)
		}
		if err := h.handle(keep); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the suspend that keeps holds of a claim in a turn returned %v, want it held", err)
		}
		if c := h.claim(); !c.SuspensionHeld || c.SuspensionHeldBy != SuspendedByArchitect {
			t.Fatalf("the claim holds a suspension %t by %q, want the architect's", c.SuspensionHeld, c.SuspensionHeldBy)
		}
		h.must(StreamTurnEnd{Claim: testToken})
		if got := h.store.load(testToken); got.State != StateSuspended || got.SuspendedBy != SuspendedByArchitect {
			t.Fatalf("after the turn the store records %s by %q, want suspended by the architect", got.State, got.SuspendedBy)
		}
	})
	t.Run("released", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByReleased)
		h.must(keep)
		if got := h.store.load(testToken).SuspendedBy; got != SuspendedByDaemon {
			t.Fatalf("after the suspend that keeps holds the store records the released claim suspended by %q, want the daemon", got)
		}
	})
}

// An architect's stop of a claim a suspension outranking it holds, landed or held for the agent's
// turn, is refused with that hold's own error: a stop retires the claim and a retry then
// relaunches it, past the hold. Its stop of a claim an architect suspended, or one the daemon
// released, retires it (the daemon's own suspension: TestAnArchitectDoesNotTakeOverTheDaemonsSuspension).
func TestAnArchitectsStopOfAClaimAHoldAboveItHoldsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		holder SuspendSource
		want   error
	}{
		{SuspendedByController, ErrControllerHolds},
		{SuspendedByOperator, ErrOperatorHolds},
	} {
		t.Run("the "+string(tc.holder)+"'s landed suspension", func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, tc.holder)
			if err := h.handle(RequestStop{Claim: testToken, By: SuspendedByArchitect}); !errors.Is(err, tc.want) {
				t.Fatalf("the architect's stop returned %v, want %v", err, tc.want)
			}
			h.wantState(StateSuspended)
		})

		t.Run("the "+string(tc.holder)+"'s held suspension", func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateWorking)
			if err := h.handle(RequestSuspend{Claim: testToken, Reason: "held first", By: tc.holder}); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the %s's suspend of a claim in a turn returned %v, want it held", tc.holder, err)
			}
			if err := h.handle(RequestStop{Claim: testToken, By: SuspendedByArchitect}); !errors.Is(err, tc.want) {
				t.Fatalf("the architect's stop returned %v, want %v", err, tc.want)
			}
			h.wantState(StateWorking)
		})
	}

	for _, by := range []SuspendSource{SuspendedByArchitect, SuspendedByReleased} {
		t.Run("the "+string(by)+"'s suspension", func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, by)
			h.must(RequestStop{Claim: testToken, By: SuspendedByArchitect})
			h.wantState(StateRetired)
		})
	}
}

// An architect's suspension of a claim the controller holds is refused, and the refusal changes
// nothing: a start in flight is not cancelled before the row refuses it (Handle). The controller's
// own suspension of the same claim still cancels the start at its arrival.
func TestAnArchitectsRefusedSuspensionCancelsNoStartInFlight(t *testing.T) {
	h := newHarness(t)
	h.reach(StateWorking)
	if err := h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("the controller's suspend of a claim in a turn returned %v, want it held", err)
	}
	cancelled := startInFlight(t, h)

	err := h.handle(RequestSuspend{Claim: testToken, Reason: "an architect suspended it", By: SuspendedByArchitect})

	if !errors.Is(err, ErrControllerHolds) {
		t.Fatalf("the architect's suspend returned %v, want ErrControllerHolds", err)
	}
	if *cancelled != 0 {
		t.Fatalf("the refused suspension cancelled the start in flight %d times, want none", *cancelled)
	}
	if h.m.stoppedAStart() {
		t.Fatal("the refused suspension left a stopped start for the next suspension to read")
	}

	startInFlight(t, h)
	_ = h.handle(RequestSuspend{Claim: testToken, Reason: "the controller suspended it", By: SuspendedByController})
	if !h.m.stoppedAStart() {
		t.Fatal("the controller's suspension cancelled no start in flight, want it cancelled at its arrival")
	}
}
