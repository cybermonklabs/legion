package supervise

import (
	"strings"
	"testing"
)

// The run of the controller session's ended launches (SessionRun, dispatch://INFRA-2045 R1) read
// on the take model (dispatch://INFRA-2811): a turn of a take is finished by the turn-outcome
// report that finishes the take's task (finishTask), not by the turn's end, and a turn that ran no
// take by its end (turnEnded); a report that carries a provider error finishes no turn
// (providerErrored). Every end the machine charges counts, the reconnect deadline's retirement of
// a restored controller whose shim never says hello among them (retireNeverReady). The control:
// with the run reset at every turn's end, as main's turnEnded resets it, the first two rows fail.
func TestAControllerSessionRunReadsTheTakeModelsFinish(t *testing.T) {
	t.Run("a take's turn is finished by the report that finishes its task, not by the turn's end", func(t *testing.T) {
		h := controllerUp(t)
		h.dies()
		h.relaunched()
		h.wantEnds(1)
		h.must(RequestDeliver{Claim: h.token, Task: controllerStart})
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		// The turn has ended and its report is still to come: the session has not finished it.
		h.wantEnds(1)

		h.mustReport(nil)
		h.wantEnds(0)
		if run := h.claim().SessionRun; !run.TurnFinished {
			t.Fatalf("the run after the report's finish is %+v, want a finished turn", run)
		}
	})

	t.Run("a take's turn whose report carries a provider error finishes no turn, so the next end replaces the session", func(t *testing.T) {
		h := controllerUp(t)
		h.dies()
		h.relaunched()
		h.must(RequestDeliver{Claim: h.token, Task: controllerStart})
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})
		h.mustReport(&ProviderError{Message: "overloaded_error", Status: 529})
		h.wantEnds(1)
		resumes, spawns := h.startsSoFar()
		h.dies()

		h.wantFreshSince(resumes, spawns, session)
	})

	t.Run("a turn that ran no take is finished by its end", func(t *testing.T) {
		h := controllerUp(t)
		h.dies()
		h.relaunched()
		h.wantEnds(1)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("the relaunched controller holds %+v, want no task, so its next turn runs none", p)
		}
		h.must(StreamTurnStart{Claim: h.token})
		h.mustQuiet(StreamTurnEnd{Claim: h.token})

		h.wantEnds(0)
	})

	t.Run("a restored controller whose shim never says hello by its reconnect deadline ends a launch of its session", func(t *testing.T) {
		h := controllerUp(t)
		h.restart()
		h.advance(deadline)

		if cause := h.claim().PendingCause; cause != CauseNeverReconnected {
			t.Fatalf("the relaunch is pending cause %q, want %q: the reconnect deadline did not retire the process", cause, CauseNeverReconnected)
		}
		h.wantEnds(1)
		if endings := h.claim().SessionRun.Endings; len(endings) != 1 || !strings.Contains(endings[0], "shim never reconnected") {
			t.Errorf("the run records the endings %q, want the one end of a shim that never reconnected", endings)
		}
	})
}
