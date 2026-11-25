package supervise

import (
	"errors"
	"testing"
)

// A suspension between turns (RequestSuspend.BetweenTurns, the controller keeper's replacement of
// an old session, dispatch://INFRA-2045 R3) stops the claim only where the machine, under its own
// lock, finds it between turns: ready or idle, no task waiting, its shim connected to this daemon,
// and its agent answering that it is not streaming. Each other claim refuses it with a
// *RefusedError and keeps its process. The control: without the machine's test, every refused row
// below is suspended at once, and the restarted claim's turn is cut off.
func TestASuspensionBetweenTurnsStopsOnlyAClaimShownBetweenTurns(t *testing.T) {
	betweenTurns := func(h *harness) RequestSuspend {
		return RequestSuspend{Claim: h.token, Reason: "the session is old", By: SuspendedByDaemon, NeverHold: true, BetweenTurns: true}
	}
	wantRefused := func(t *testing.T, h *harness, state ClaimState) {
		t.Helper()
		suspends := len(h.calls("Suspend"))
		var refused *RefusedError
		if err := h.handle(betweenTurns(h)); !errors.As(err, &refused) {
			t.Fatalf("the suspension between turns answered %v, want a refusal", err)
		}
		if got := len(h.calls("Suspend")); got != suspends {
			t.Fatalf("the runtime was asked %d times to suspend the claim, want never", got-suspends)
		}
		h.wantState(state)
	}

	t.Run("a claim whose shim has not connected to this daemon", func(t *testing.T) {
		h := controllerUp(t)
		h.restart()
		h.conns.Remove(h.token)
		wantRefused(t, h, StateIdle)
	})

	t.Run("a claim whose agent is streaming a turn this daemon has not read the start of", func(t *testing.T) {
		h := controllerUp(t)
		h.restart()
		h.conn.SetStreaming(true)
		h.connect()
		wantRefused(t, h, StateIdle)
	})

	t.Run("a claim with a task waiting", func(t *testing.T) {
		h := controllerUp(t)
		h.conn.RefusePrompt("the agent is busy")
		_ = h.handle(RequestDeliver{Claim: h.token, Task: controllerStart})
		if h.claim().Pending == nil {
			t.Fatal("the claim holds no task, want the one its agent refused still waiting")
		}
		wantRefused(t, h, h.state())
	})

	t.Run("a claim in a turn", func(t *testing.T) {
		h := controllerUp(t)
		h.inATurn()
		wantRefused(t, h, StateWorking)
	})

	t.Run("an idle claim whose agent says it is in no turn is suspended", func(t *testing.T) {
		h := controllerUp(t)
		h.must(betweenTurns(h))
		h.wantState(StateSuspended)
	})
}
