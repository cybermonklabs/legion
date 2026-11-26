package supervise

import (
	"errors"
	"testing"
)

// The claims of a tree whose work hold stands that the project's controller may meet, each by the
// way the plan's decision 16 names (dispatch://INFRA-4528's plan): a claim the hold stopped; one
// the controller suspended before the close, which the hold's stop does not take over, since the
// controller's suspension outranks the hold's (suspensionRank); one the hold stopped that the
// controller's suspension then took over; and one the hold stopped that the controller's stop then
// retired, which the opening skips.
var controllerHoldCases = []struct {
	name string
	walk func(h *harness)
}{
	{"a claim the hold stopped", func(h *harness) { h.stoppedByTheHold() }},
	{"a claim the controller suspended before the close", func(h *harness) {
		suspendedBy(h, SuspendedByController)
		h.must(directHoldStop(h))
		if c := h.claim(); c.SuspendedBy != SuspendedByController || c.WorkHeld() {
			h.t.Fatalf("after the close the claim is suspended by %q, WorkHeld %v; want the controller's suspension left as it was",
				c.SuspendedBy, c.WorkHeld())
		}
	}},
	{"a held claim the controller took over", func(h *harness) {
		h.stoppedByTheHold()
		h.must(RequestSuspend{Claim: h.token, Reason: "the controller suspended it", By: SuspendedByController})
		if c := h.claim(); c.SuspendedBy != SuspendedByController || c.WorkHeld() {
			h.t.Fatalf("the claim is suspended by %q, WorkHeld %v; want the controller's suspension in the hold's place",
				c.SuspendedBy, c.WorkHeld())
		}
	}},
	{"a held claim the controller stopped", func(h *harness) {
		h.stoppedByTheHold()
		h.must(RequestStop{Claim: h.token, By: SuspendedByController})
		h.wantState(StateRetired)
	}},
}

// t-4, decision 16: the controller is no operator, so while its tree's work hold stands it starts
// no phase worker of the tree. Its resume, its retry and its fresh session's forget of the
// session are each refused with the hold's error, whatever suspended the claim — the hold, or the
// controller itself before the close or after the hold's stop — and nothing of the claim
// changes: its state, its source, its session and the task it kept. The refusal reads the tree's
// hold (workHoldRefuses), not the claim's source alone.
func TestWorkHoldRefusesTheControllersStartsWhileTheTreeIsHeld(t *testing.T) {
	for _, tc := range controllerHoldCases {
		for _, act := range []struct {
			name string
			ev   func(h *harness) Event
		}{
			{"resume", func(h *harness) Event { return RequestResume{Claim: h.token, By: SuspendedByController} }},
			{"retry", func(h *harness) Event { return RequestAskedRetry{Claim: h.token, By: SuspendedByController} }},
			{"fresh session's forget", func(h *harness) Event {
				return RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByController}
			}},
		} {
			t.Run(tc.name+"/"+act.name, func(t *testing.T) {
				h := newHarness(t)
				tc.walk(h)
				before, calls := h.claim(), len(h.rt.Methods())

				err := h.handle(act.ev(h))

				if !errors.Is(err, ErrHeldByWorkHold) {
					t.Fatalf("the controller's %s answered %v, want the work hold's refusal", act.name, err)
				}
				after := h.claim()
				if after.State != before.State || after.SuspendedBy != before.SuspendedBy || after.Session != before.Session {
					t.Fatalf("the refused %s left the claim %s by %q with session %q, want it %s by %q with session %q",
						act.name, after.State, after.SuspendedBy, after.Session, before.State, before.SuspendedBy, before.Session)
				}
				if (before.Pending == nil) != (after.Pending == nil) || (before.Pending != nil && after.Pending.ID != before.Pending.ID) {
					t.Fatalf("the refused %s left the claim holding %+v, want the task it kept, %+v", act.name, after.Pending, before.Pending)
				}
				if got := h.rt.Methods(); len(got) != calls {
					t.Fatalf("the refused %s asked the runtime %v, want nothing", act.name, got[calls:])
				}
			})
		}
	}
}

// t-4, decision 16: once the tree's gate opens, the opening starts only the claims the hold
// stopped, and sends nothing to a claim the controller suspended — before the close, or taking the
// hold's stop over — nor to one its stop retired; the controller's resume, its fresh session and
// its retry then start such a claim, as they start any claim they reach.
func TestWorkHoldLetsTheControllerStartItsClaimOnceTheTreeOpens(t *testing.T) {
	for _, tc := range controllerHoldCases[1:] {
		t.Run(tc.name, func(t *testing.T) {
			for _, act := range []struct {
				name    string
				retired bool
				start   func(h *harness)
				call    string
			}{
				{"resume", false, func(h *harness) { h.must(RequestResume{Claim: h.token, By: SuspendedByController}) }, "Resume"},
				{"fresh session", false, func(h *harness) {
					h.must(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByController})
					h.must(RequestResume{Claim: h.token, By: SuspendedByController})
				}, "Spawn"},
				{"retry", true, func(h *harness) { h.must(RequestAskedRetry{Claim: h.token, By: SuspendedByController}) }, "Resume"},
				{"fresh session", true, func(h *harness) {
					h.must(RequestForgetSession{Claim: h.token, Session: session, By: SuspendedByController})
					h.must(RequestAskedRetry{Claim: h.token, By: SuspendedByController, FreshSession: true})
				}, "Spawn"},
			} {
				// A suspended agent is resumed, not retried, and a retired one is retried, not
				// resumed: each act is asked of the claims it reaches.
				if act.retired != (tc.name == "a held claim the controller stopped") {
					continue
				}
				t.Run(act.name, func(t *testing.T) {
					h := newHarness(t)
					tc.walk(h)
					before := h.claim()
					prompts, calls := len(h.prompts()), len(h.rt.Methods())
					h.store.openHold(before.Tree)
					start := openingStart(h)

					if resumed, err := h.m.EndHold(h.ctx, &start); resumed || err != nil {
						t.Fatalf("the opening's EndHold of %s = %v, %v; want it to start nothing", tc.name, resumed, err)
					}
					if after := h.claim(); after.State != before.State || after.SuspendedBy != before.SuspendedBy {
						t.Fatalf("the opening left the claim %s by %q, want it %s by %q", after.State, after.SuspendedBy, before.State, before.SuspendedBy)
					}
					if got := len(h.prompts()); got != prompts {
						t.Fatalf("the opening sent %d prompts, want none", got-prompts)
					}
					if got := h.rt.Methods(); len(got) != calls {
						t.Fatalf("the opening asked the runtime %v, want nothing", got[calls:])
					}
					spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))

					act.start(h)

					h.wantState(StateLaunching)
					if act.call == "Spawn" {
						if got := len(h.calls("Spawn")); got != spawns+1 {
							t.Fatalf("spawns = %d, want one more for the fresh session", got-spawns)
						}
					} else if got := len(h.calls("Resume")); got != resumes+1 {
						t.Fatalf("resumes = %d, want one more for the controller's %s", got-resumes, act.name)
					}
				})
			}
		})
	}
}
