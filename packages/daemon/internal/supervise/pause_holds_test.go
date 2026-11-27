package supervise

import (
	"errors"
	"testing"
)

// A suspension that holds — the operator's or the controller's — that reaches an
// agent whose turn a project pause's stop is held for takes that hold over, and the hold is its own
// from then on: it names no pause, so the project's resume drops nothing of it
// (dropEndedPauseHold), and the turn's end suspends the agent by that source, with no record of the
// pause's stop. Each hold keeps its source, and lifting the pause brings back only what the pause
// suspended (holdSuspension).
//
// The negative control is the takeover keeping the pause's id on the hold (holdSuspension's
// `m.held.Pause = 0` undone): the resume's event drops the hold as the ended pause's, and the
// claim runs on, idle, at its turn's end.
func TestASuspensionThatHoldsTakesOverAPausesHeldStopAndKeepsItThroughTheResume(t *testing.T) {
	for _, by := range []SuspendSource{SuspendedByOperator, SuspendedByController} {
		t.Run(string(by), func(t *testing.T) {
			h, flag, stops := pauseHarness(t, queuedClaim())
			h.reach(StateWorking)
			flag.set(7)
			if err := h.handle(pauseStop(h.token, 7)); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the pause's stop of an agent in a turn = %v, want it held", err)
			}
			if err := h.handle(RequestSuspend{Claim: h.token, By: by, Reason: string(by) + " suspended it"}); !errors.Is(err, ErrSuspendHeld) {
				t.Fatalf("the %s's suspension of the agent the pause holds = %v, want it held", by, err)
			}
			if pause, held := h.heldPause(); !held || pause != 0 {
				t.Fatalf("after the %s's suspension the hold names pause %d (held %v), want the %s's own hold, naming no pause", by, pause, held, by)
			}
			if got := h.claim().SuspensionHeldBy; got != by {
				t.Fatalf("the hold is %q's, want the %s's", got, by)
			}

			flag.set(0)
			h.must(ResumePause{Claim: h.token, Pause: 7})

			if _, held := h.heldPause(); !held {
				t.Fatalf("the project's resume dropped the %s's hold", by)
			}
			h.must(StreamTurnEnd{Claim: h.token})
			if got := h.claim(); got.State != StateSuspended || got.SuspendedBy != by {
				t.Fatalf("the claim is %s by %q at its turn's end, want it suspended by the %s", got.State, got.SuspendedBy, by)
			}
			stops.wantStops(t)
		})
	}
}

// An agent a pause stopped and the operator then suspended, whose operator's suspension is lifted
// while the project is still paused, does not start: the operator's hold is gone and the pause's is
// not, and main's gate holds every launch of a claim that is not the controller's while the project
// is paused, whatever asked for it and whatever the claim's own holds (reserveWake's pauseHolds,
// checked before the exempt return). The launch runs nothing, and the next decision on the claim
// stops its agent for the pause with a launch owed, so the project's resume brings that agent back
// through the wake pacing. The route refuses the operator's resume itself while paused (api's
// refusedWhilePaused); this is the machine under it, as a resume that reached it would be decided.
//
// The negative control is the gate's pause check undone (reserveWake's `pauseHolds` branch): the
// operator's resume starts the agent while the project is paused.
func TestAClaimWhoseOperatorsHoldIsLiftedWhileThePauseStandsDoesNotStart(t *testing.T) {
	c := wakeClaim(testToken, StateSuspended)
	c.SuspendedBy = SuspendedByReleased
	h, flag, stops := pauseHarness(t, c, pacedDeps(wakeRate))
	h.start(c)
	flag.set(7)

	h.must(RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator suspended it"})
	if got := h.claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByOperator {
		t.Fatalf("the claim is %s by %q after the operator's suspension, want suspended by the operator", got.State, got.SuspendedBy)
	}

	h.must(RequestResume{Claim: h.token, By: SuspendedByOperator})

	h.wantStarts(h.token, 0)
	h.advance(0)
	h.wantStarts(h.token, 0)
	if got := h.claim(); got.State != StateSuspended || got.SuspendedBy != SuspendedByReleased {
		t.Fatalf("the claim is %s by %q, want it suspended as released, its agent stopped again for the pause", got.State, got.SuspendedBy)
	}
	stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
}
