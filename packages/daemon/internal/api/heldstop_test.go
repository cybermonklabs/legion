package api

// The operator's stop route over a held stop (rule two of T3 of dispatch://INFRA-2811's plan v150,
// rows 39e and 39h of its state table): the operator's stop of one claim whose take's turn ended
// with its report still to come waits for that report (supervise's holdStop), and the route answers
// it as it answers a held suspension, 202 with the claim as it is (claimEvent, which reads
// supervise.ErrSuspendHeld). The operator's close of a tree stops the tree's other claims at once,
// as before (closeTree). Each test is named so c2's Take selects it.
//
// The negative control of each is e63fe1f5 (e63fe1f57842a88c48f18bee9054ef6b6a97d8a0), where the
// stop route posts RequestStop{Claim} (stopEvent, operator.go:581) and the machine ends the claim
// at once (supervise's stop): the route answers 200 with the claim retired.

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// heldStopBound is the report wait's bound (supervise.Timeouts.TurnStart) the harness's machines
// run, at the daemon's default (worker_turn_start_timeout_seconds); the harness's clock stands
// still, so it never passes.
const heldStopBound = 1500 * time.Second

// takesTurnEndsWithItsReportToCome hands c a task its worker takes and runs that take's turn to its
// end, the turn-outcome report naming its number still to come, and answers the task's value.
func (h *harness) takesTurnEndsWithItsReportToCome(c takeClaim) claim.TaskValue {
	h.t.Helper()
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})
	recorder, answer := h.postTake(takeOf(c, value))
	wantAccepted(h.t, recorder, answer, "the task")
	h.startAndEndTurn(c.token)
	if stored := h.stored(c.token); stored.State != supervise.StateIdle || stored.Pending == nil || stored.Pending.Number != value.Number {
		h.t.Fatalf("after its take's turn ended %s is %s holding %+v, want idle holding task %d", c.token, stored.State, stored.Pending, value.Number)
	}
	return value
}

// stopRoute posts the operator's stop of token.
func (h *harness) stopRoute(token claim.Token) (int, OperatorClaim) {
	h.t.Helper()
	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil)
	var got OperatorClaim
	if recorder.Code/100 == 2 {
		decodeInto(h.t, recorder, &got)
	}
	return recorder.Code, got
}

// The stop route answers a held stop 202 with the claim as it is: idle over the process its take's
// turn ran on, holding that take, nothing released. The report naming the task then finishes it,
// and the stop runs: the claim retires and its process is released once.
func TestTheStopRouteAnswersAHeldStopOfATakeWhoseTurnEnded202(t *testing.T) {
	h := newHarness(t)
	h.supervisor.deps.Timeouts.TurnStart = heldStopBound
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.takesTurnEndsWithItsReportToCome(c)

	status, got := h.stopRoute(c.token)

	if status != http.StatusAccepted {
		t.Fatalf("the stop of %s, whose take's turn ended with its report to come = %d answering it %s, want 202 with the stop held",
			c.token, status, got.State)
	}
	if got.State != string(supervise.StateIdle) || got.Locator == nil || got.Pending == nil || len(h.runtime.CallsOf("Release")) != 0 {
		t.Fatalf("the held stop answered the claim %s over %+v holding %+v with %d releases, want the claim as it is: idle over its process, holding its task, nothing released",
			got.State, got.Locator, got.Pending, len(h.runtime.CallsOf("Release")))
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

// Row 39h, the operator's close of a tree: the close stops the tree's other claims (closeTree), and
// a worker of the tree whose take's turn ended with its report still to come is stopped at once,
// with no hold, as before — the close counts any error of those stops as a claim it did not stop —
// its process released, its claim retired, and its task taken back with the note of a cut turn.
// The control is the operator's stop of a worker of another tree at the same point, which is held.
//
// The negative control is e63fe1f5: the control fails there, the stop route answering 200.
func TestTakeWhoseTurnEndedIsStoppedAtOnceByTheOperatorsCloseOfItsTree(t *testing.T) {
	h := newHarness(t)
	h.supervisor.deps.Timeouts.TurnStart = heldStopBound
	other := h.readyClaim("LEGION-300", claim.RoleImplementer)
	h.takesTurnEndsWithItsReportToCome(other)
	if status, got := h.stopRoute(other.token); status != http.StatusAccepted {
		t.Fatalf("the control: the operator's stop of %s at the same point = %d answering it %s, want 202 with the stop held", other.token, status, got.State)
	}
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	spawn := SpawnRequest{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."}
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawn); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn %s %s = %d; body %s", spawn.Issue, spawn.Role, recorder.Code, recorder.Body)
	}
	token := claim.Token("legion-legion-legion-209-implementer")
	registered := h.ready(token, "ses_tree_implementer", h.machine(token).Claim().Generation)
	worker := takeClaim{token: token, session: "ses_tree_implementer", secret: registered.Secret, generation: registered.Generation}
	value := h.takesTurnEndsWithItsReportToCome(worker)

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("close = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	released := 0
	for _, call := range h.runtime.CallsOf("Release") {
		if call.Released.Claim == token {
			released++
		}
	}
	stored := h.stored(token)
	if stored.State != supervise.StateRetired || released != 1 {
		t.Fatalf("after its tree's close %s is %s and was released %d times, want retired and released once, at once", token, stored.State, released)
	}
	if p := stored.Pending; p == nil || p.Number != value.Number || !p.Interrupted || stored.TaskFinished == value.Number {
		t.Fatalf("after its tree's close %s holds %+v with task %d finished, want task %d taken back with the note of a cut turn", token, p, stored.TaskFinished, value.Number)
	}
}
