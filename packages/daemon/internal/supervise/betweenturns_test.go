package supervise

import (
	"errors"
	"testing"
)

// betweenTurnsSuspension is the controller keeper's suspension for the replacement of an old
// session (dispatch://INFRA-2045 R3), as the keeper posts it.
func betweenTurnsSuspension(h *harness) RequestSuspend {
	return RequestSuspend{Claim: h.token, Reason: "the session is old", By: SuspendedByDaemon, NeverHold: true, BetweenTurns: true}
}

// A suspension between turns (RequestSuspend.BetweenTurns) stops a claim the suspend row takes —
// registered, ready or idle — only where the machine, under its own lock, finds it between turns:
// ready or idle, no task waiting, its shim connected to this daemon, and its agent answering that
// it is not streaming (Machine.betweenTurns). A claim there that fails the test refuses the
// suspension with a *RefusedError and keeps its process. The control: without the machine's test,
// each refused row below is suspended at once, and the restarted claim's turn is cut off.
func TestASuspensionBetweenTurnsStopsOnlyAClaimShownBetweenTurns(t *testing.T) {
	wantRefused := func(t *testing.T, h *harness, state ClaimState) {
		t.Helper()
		h.wantState(state)
		suspends := len(h.calls("Suspend"))
		var refused *RefusedError
		if err := h.handle(betweenTurnsSuspension(h)); !errors.As(err, &refused) {
			t.Fatalf("the suspension between turns answered %v, want a refusal", err)
		}
		if got := len(h.calls("Suspend")); got != suspends {
			t.Fatalf("the runtime was asked %d times to suspend the claim, want never", got-suspends)
		}
		h.wantState(state)
	}

	t.Run("a registered claim, not yet ready", func(t *testing.T) {
		h := newHarnessOf(t, controllerClaim())
		h.reach(StateRegistered)
		wantRefused(t, h, StateRegistered)
	})

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

	t.Run("an idle claim with a task waiting", func(t *testing.T) {
		h := controllerUp(t)
		h.conn.RefusePrompt("the agent is busy")
		_ = h.handle(RequestDeliver{Claim: h.token, Task: controllerStart})
		if h.claim().Pending == nil {
			t.Fatal("the claim holds no task, want the one its agent refused still waiting")
		}
		wantRefused(t, h, StateIdle)
	})

	t.Run("an idle claim whose agent says it is in no turn is suspended", func(t *testing.T) {
		h := controllerUp(t)
		h.must(betweenTurnsSuspension(h))
		h.wantState(StateSuspended)
	})
}

// The controller keeper's forget names the daemon's suspension it made
// (RequestForgetSession.SuspendedBy), and a claim an operator suspended refuses it, keeping the
// session the operator's resume promises. Two ways reach that claim from the keeper's suspension:
// an operator's suspension held through a turn whose stop the runtime refused, which the keeper's
// suspension runs as the operator's (suspendRequest), and an operator's suspension decided before
// the keeper's, which the keeper's suspension answers with nil (suspendedAgain). The control:
// without the forget's test, both rows forget the session.
func TestTheKeepersForgetRefusesAClaimAnOperatorSuspended(t *testing.T) {
	forget := func(h *harness) RequestForgetSession {
		return RequestForgetSession{Claim: h.token, Session: session, Why: "the session is old", SuspendedBy: SuspendedByDaemon}
	}
	wantKept := func(t *testing.T, h *harness) {
		t.Helper()
		c := h.claim()
		if c.State != StateSuspended || c.SuspendedBy != SuspendedByOperator {
			t.Fatalf("the claim is %s, suspended by %q, want it suspended by the operator", c.State, c.SuspendedBy)
		}
		var refused *RefusedError
		if err := h.handle(forget(h)); !errors.As(err, &refused) {
			t.Fatalf("the keeper's forget answered %v, want a refusal", err)
		}
		for where, got := range map[string]Claim{"the machine": h.claim(), "the store": h.store.load(h.token)} {
			if got.Session != session || got.Abandoned.Session != "" {
				t.Errorf("%s records session %q and abandoned %q, want %s kept for the operator's resume", where, got.Session, got.Abandoned.Session, session)
			}
		}
	}

	t.Run("an idle claim holding an operator's suspension whose stop the runtime refused", func(t *testing.T) {
		h := controllerUp(t)
		h.inATurn()
		if err := h.handle(RequestSuspend{Claim: h.token, Reason: "an operator drains the daemon", By: SuspendedByOperator}); !errors.Is(err, ErrSuspendHeld) {
			t.Fatalf("the operator's suspension in a turn answered %v, want it held", err)
		}
		h.rt.FailSuspend(errors.New("the runtime could not stop the process"))
		_ = h.handle(StreamTurnEnd{Claim: h.token})
		h.wantState(StateIdle)
		h.rt.FailSuspend(nil)
		h.must(betweenTurnsSuspension(h))
		wantKept(t, h)
	})

	t.Run("a claim the operator suspended before the keeper's suspension", func(t *testing.T) {
		h := controllerUp(t)
		h.must(RequestSuspend{Claim: h.token, Reason: "an operator stops the controller", By: SuspendedByOperator})
		h.must(betweenTurnsSuspension(h))
		wantKept(t, h)
	})
}
