package api

// The controller's stop through the agent door (stopAgent, cybermonklabs/legion#4065) over a take
// of dispatch://INFRA-2811's take model: a stop of one agent, as the operator's stop route's is, so
// it asks the machine to wait for the report of the take's turn (supervise.RequestStop.
// WaitForReport, holdStop), and the door answers that held stop as it answers a held suspension,
// 202 with the claim as it is (agentEventAction, which reads supervise.ErrSuspendHeld).
//
// The negative control is stopAgent posting RequestStop with no WaitForReport, as #4065 wrote it:
// the door answers 200 with the claim retired, its process released before the report, and settle
// takes back as a cut turn a task whose turn ran whole.

import (
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The controller's stop of an agent whose take's turn ended with its report still to come is
// answered 202 with the claim idle over its process, nothing released; the report naming the task
// then finishes it, and the stop runs: the claim retires and its process is released once.
func TestTakeWhoseTurnEndedHoldsTheControllersStopForItsReport(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	h.supervisor.deps.Timeouts.TurnStart = heldStopBound
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.takesTurnEndsWithItsReportToCome(c)

	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/stop", "LEGION-209", claim.RoleImplementer),
		http.StatusAccepted, AgentActionResponse{State: string(supervise.StateIdle)})
	if released := len(h.runtime.CallsOf("Release")); released != 0 {
		t.Fatalf("the held stop released %d processes, want none before the report", released)
	}

	outcome, _ := h.turnOutcome(claim.TurnOutcomeRequest{AgentCaller: c.agent(), Generation: c.generation, Sequence: 1,
		Outcome: claim.TurnClean, Task: value.Number})

	if outcome.Code != http.StatusOK {
		t.Fatalf("the report naming task %d = %d (body %s), want 200", value.Number, outcome.Code, outcome.Body)
	}
	if stored := h.stored(c.token); stored.State != supervise.StateRetired || stored.TaskFinished != value.Number || len(h.runtime.CallsOf("Release")) != 1 {
		t.Fatalf("after the report %s is %s with task %d finished and %d releases, want retired, task %d finished, released once",
			c.token, stored.State, stored.TaskFinished, len(h.runtime.CallsOf("Release")), value.Number)
	}
}
