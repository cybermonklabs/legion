package supervise

// A process the runtime reports ended with a pod the daemon itself replaced while it ran
// (runtime.Observation.Displaced: the Sandbox runtime replacing a role's issue pod to launch
// another role there, LEGION-729) is not a death: these tests pin that the claim is relaunched at
// once on its recorded session, charging neither a launch failure nor a death, however often it
// happens, and that a held suspension ends in a suspension instead.

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// observeDisplaced is the runtime's verdict that the claim's process ended with a pod the daemon
// replaced: the field, beside the kind the runtime read the pod as.
func (h *harness) observeDisplaced(kind runtime.ObservationKind) {
	h.t.Helper()
	h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: kind, At: h.clock.Now(), Displaced: true}})
}

// A claim in any live state whose pod the daemon replaced, whether the runtime reads the pod gone
// or another pod in its place, is relaunched at once and charged nothing, where a death charges a
// launch failure (TestAProcessFoundGoneIsRelaunchedAsTheSameSession). A registered claim resumes
// its recorded session after the ended incarnation; one that never registered spawns again.
func TestAProcessEndedWithAPodTheDaemonReplacedIsRelaunchedChargingNothing(t *testing.T) {
	for _, kind := range []runtime.ObservationKind{runtime.Gone, runtime.NotRecordedProcess} {
		for _, state := range liveStates {
			t.Run(string(kind)+"/"+string(state), func(t *testing.T) {
				h := newHarness(t)
				h.reach(state)
				ended := h.locator()

				h.observeDisplaced(kind)

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{})
				if registered := state != StateLaunching && state != StateShimConnected; registered {
					resume := h.wantCalls("Resume", 1)[0]
					if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile || resume.Spec.Generation != 2 {
						t.Fatalf("resumed %+v from %+v, want the recorded session after the ended incarnation, at generation 2", resume.Spec, resume.Previous)
					}
				} else {
					spawned := h.wantCalls("Spawn", 2)[1]
					if spawned.Spec.ResumeSessionFile != "" || spawned.Spec.Generation != 2 {
						t.Fatalf("relaunched with %+v, want a fresh spawn at generation 2: the claim recorded no session", spawned.Spec)
					}
				}
				h.relaunched()
				h.wantState(StateReady)
			})
		}
	}
}

// A process the daemon's replacement ends mid-turn gives its task back (interrupted), as a death in
// a turn does, but counts no death with work outstanding: more replacements in a row than the launch
// failure limit leave the claim relaunched each time, its task taken again under its number, never
// failed.
func TestTakeBackByAReplacementKeepsTheNumberWithoutEverFailingTheClaim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	first := h.wantPrompts(1)[0]

	for replaced := 1; replaced <= testLimits().LaunchFailures+1; replaced++ {
		h.observeDisplaced(runtime.NotRecordedProcess)

		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{})
		if p := h.pending(); !p.Interrupted || p.ID != first.DeliveryID || p.Number != first.Number || p.taken() {
			t.Fatalf("pending after replacement %d = %+v, want task %d taken back under its number and interrupted", replaced, p, first.Number)
		}
		h.relaunched()
		again := h.wantPrompts(1 + replaced)[replaced]
		if !strings.HasSuffix(again.Message, "implement the plan") || again.Number != first.Number {
			t.Fatalf("took %+v after replacement %d, want task %d again", again, replaced, first.Number)
		}
		h.must(StreamTurnStart{Claim: testToken})
	}
}

// A held suspension is not waited out for a process that ended with its pod: there is no turn left
// to end. The replacement ends the hold as a death does (endHeld): the claim is suspended at once,
// charged nothing, and nothing is launched only to be suspended again.
func TestAReplacementWithAHeldSuspensionSuspendsTheClaimChargingNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan"})
	h.must(StreamTurnStart{Claim: testToken})
	if err := h.handle(RequestSuspend{Claim: testToken, By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}

	h.observeDisplaced(runtime.Gone)

	h.wantState(StateSuspended)
	h.wantBudgets(Budgets{})
	h.wantCalls("Spawn", 1)
	h.wantCalls("Resume", 0)
	if claim := h.claim(); claim.SuspensionHeld {
		t.Fatalf("claim %+v after the replacement, want the held suspension ended", claim)
	}
}
