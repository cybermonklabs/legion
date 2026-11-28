package supervise

// An end from outside a paused role's pod (dispatch://INFRA-3759's pause beside this branch's
// uncharged continue of such an end). The pause deletes no pod, so the cluster can still evict,
// preempt or drain one while the project is paused, and the continue of that end
// (endedFromOutside) is a launch like any other: the pause's gate holds it (reserveWake,
// pauseGate) until the resume, and nothing about the hold is charged to the role.

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// gatedFor is the pause the pause's gate holds the claim's launch for (wake.pause), and whether
// the gate holds one, read under the machine's lock.
func (h *harness) gatedFor() (int64, bool) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if h.m.wake == nil || h.m.wake.pause == 0 {
		return 0, false
	}
	return h.m.wake.pause, true
}

// A role whose pod something outside it ends while its project is paused is not launched while
// the pause stands: the continue of that end is the gate's to hold, for the open pause, with
// nothing started and nothing charged, and the role is stopped for the pause owing that launch.
// Once the pause is over the role is launched once, in its own session after the ended process,
// still charged nothing, and is sent the task it owed: by the resume's event where the launch is
// still held, and by the start the pause held where its stop came first. Launched at the end
// instead, the role ran through the pause; charged for it, a few drains during one pause failed a
// role that did nothing wrong. A role the pause had already stopped stays stopped and uncharged
// when its pod is ended from outside.
func TestAPausedRolesPodEndedFromOutsideIsNotLaunchedUntilTheResume(t *testing.T) {
	// endedWhilePaused brings an implementer to ready and sends it its task, which stays
	// unconfirmed, pauses its project (pause 7), and ends its pod from outside it with an
	// observation of kind. It answers the locator of the process that ended.
	endedWhilePaused := func(t *testing.T, kind runtime.ObservationKind) (*harness, *pauseFlag, *stopTransaction, runtime.Locator) {
		t.Helper()
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: owedTask})
		h.wantPrompts(1)
		h.wantOwed(owedTask)
		ended := h.locator()
		flag.set(7)

		h.observePodEnd(kind, runtime.PodEndNotOurs)

		h.wantStarts(h.token, 1)
		h.wantNothingCharged("the end from outside while paused")
		if pause, held := h.gatedFor(); !held || pause != 7 {
			t.Fatalf("the gate holds the claim's launch for pause %d (held %v), want pause 7's", pause, held)
		}
		if !h.armed(TimerPause) {
			t.Fatal("no stop of the claim is posted for its next decision")
		}
		for _, retry := range []TimerKind{TimerWake, TimerRelaunch} {
			if h.armed(retry) {
				t.Fatalf("timer %s is armed, want no retry of the held launch", retry)
			}
		}
		stops.wantStops(t)
		return h, flag, stops, ended
	}
	// launchedOnceAfterThePause fails unless the claim was launched exactly once since its pod
	// ended — a resume of its own recorded session after the ended process — charged nothing, and
	// sent the task it owed once its relaunched agent is ready.
	launchedOnceAfterThePause := func(t *testing.T, h *harness, ended runtime.Locator) {
		t.Helper()
		h.wantStarts(h.token, 2)
		resume := h.wantCalls("Resume", 1)[0]
		if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
			t.Fatalf("resumed %+v from %+v, want the role's own recorded session after the ended incarnation",
				resume.Spec, resume.Previous)
		}
		h.wantNothingCharged("the launch after the pause")
		h.relaunched()
		if resent := h.wantPrompts(2)[1]; !strings.HasSuffix(resent.Message, owedTask) {
			t.Fatalf("re-sent %+v, want the task the role owed", resent)
		}
		h.wantNothingCharged("the relaunched agent's ready")
	}

	for _, kind := range []runtime.ObservationKind{runtime.Gone, runtime.NotRecordedProcess} {
		t.Run("the resume's event lets out the launch the gate held/"+string(kind), func(t *testing.T) {
			h, flag, stops, ended := endedWhilePaused(t, kind)
			flag.set(0)

			h.must(ResumePause{Claim: h.token, Pause: 7})

			// The gate's stop the end posted finds the launch gone and stops nothing.
			h.advance(0)
			h.wantState(StateLaunching)
			stops.wantStops(t)
			launchedOnceAfterThePause(t, h, ended)
		})

		t.Run("the gate's stop owes the launch and the resume's start makes it/"+string(kind), func(t *testing.T) {
			h, flag, stops, ended := endedWhilePaused(t, kind)

			h.advance(0)

			h.wantState(StateSuspended)
			if got := h.claim().SuspendedBy; got != SuspendedByReleased {
				t.Fatalf("the claim is suspended by %q, want released", got)
			}
			stops.wantStops(t, stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true})
			h.wantStarts(h.token, 1)
			h.wantNothingCharged("the gate's stop")

			flag.set(0)
			h.must(ResumePause{Claim: h.token, Pause: 7})
			// The start row the pause held runs once the resume wakes its lane (the daemon's
			// projectPause.Resumed, outbox's start of a suspended claim).
			h.must(RequestRuleResume{Claim: h.token, Reason: "the start the pause held runs at the resume"})

			launchedOnceAfterThePause(t, h, ended)
		})
	}

	t.Run("a role the pause already stopped stays stopped", func(t *testing.T) {
		h, flag, stops := pauseHarness(t, queuedClaim())
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: owedTask})
		h.wantPrompts(1)
		ended := h.locator()
		flag.set(7)
		h.must(pauseStop(h.token, 7))
		h.wantState(StateSuspended)
		stopped := stopCall{stop: PauseStop{Pause: 7, LaunchOwed: true}, landed: true}
		stops.wantStops(t, stopped)

		h.must(RuntimeObservation{Observation: runtime.Observation{
			Locator: ended, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs,
		}})

		h.wantState(StateSuspended)
		if got := h.claim().SuspendedBy; got != SuspendedByReleased {
			t.Fatalf("the claim is suspended by %q, want released", got)
		}
		h.wantStarts(h.token, 1)
		h.wantNothingCharged("the end from outside of the stopped role")
		if pause, held := h.gatedFor(); held {
			t.Fatalf("the gate holds a launch of the stopped role for pause %d, want none", pause)
		}
		stops.wantStops(t, stopped)
	})
}
