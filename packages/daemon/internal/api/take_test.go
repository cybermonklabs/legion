package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
)

// takeClaim is one launched claim whose agent registered and said it is ready, with what each of
// its two caller forms carries: the registration's secret, and the launch's boot token.
type takeClaim struct {
	token      claim.Token
	boot       string
	session    string
	secret     string
	generation uint64
}

// readyClaim launches a claim of issue for role, registers its session and has its agent say it
// is ready, all through the routes.
func (h *harness) readyClaim(issue string, role claim.Role) takeClaim {
	h.t.Helper()
	token, boot := h.launch(issue, role)
	session := "ses_" + string(role)
	registered := h.ready(token, session, h.machine(token).Claim().Generation)
	return takeClaim{token: token, boot: boot, session: session, secret: registered.Secret, generation: registered.Generation}
}

func (c takeClaim) agent() claim.AgentCaller {
	return claim.AgentCaller{ClaimToken: c.token, SessionID: c.session, Secret: c.secret}
}

func (c takeClaim) byBoot() claim.BootCaller { return claim.BootCaller{BootToken: c.boot} }

// taskValues is the in-memory task bucket the harness's machines write to.
func (h *harness) taskValues() *supervisetest.TaskValues {
	return h.supervisor.deps.TaskValues.(*supervisetest.TaskValues)
}

// machine is the claim's machine, which must exist.
func (h *harness) machine(token claim.Token) *supervise.Machine {
	h.t.Helper()
	m, ok := h.supervisor.Machine(token)
	if !ok {
		h.t.Fatalf("no machine for %s", token)
	}
	return m
}

// deliverTask queues request on its claim, as an operator's delivery or the workflow's start does,
// and answers the value the bucket holds for the claim afterwards.
func (h *harness) deliverTask(request supervise.RequestDeliver) claim.TaskValue {
	h.t.Helper()
	if err := h.machine(request.Claim).Handle(h.ctx, request); err != nil {
		h.t.Fatalf("deliver %q to %s: %v", request.Task, request.Claim, err)
	}
	value, held := h.taskValues().Value(request.Claim)
	if !held {
		h.t.Fatalf("the bucket holds no value for %s after its task was queued", request.Claim)
	}
	return claim.TaskValue{Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}
}

// takeOf is the take of value by the agent form of c.
func takeOf(c takeClaim, value claim.TaskValue) claim.TakeRequest {
	return claim.TakeRequest{AgentCaller: c.agent(), Generation: c.generation,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}
}

// postTake posts a take and answers what the route made of it.
func (h *harness) postTake(req claim.TakeRequest) (*httptest.ResponseRecorder, claim.TakeResponse) {
	h.t.Helper()
	recorder := h.request(http.MethodPost, "/legion/v1/claims/take", req, nil)
	var answer claim.TakeResponse
	if recorder.Code == http.StatusOK {
		decodeInto(h.t, recorder, &answer)
	}
	return recorder, answer
}

// postTask reads a claim's value through the route.
func (h *harness) postTask(req claim.TaskRequest) (*httptest.ResponseRecorder, claim.TaskResponse) {
	h.t.Helper()
	recorder := h.request(http.MethodPost, "/legion/v1/claims/task", req, nil)
	var answer claim.TaskResponse
	if recorder.Code == http.StatusOK {
		decodeInto(h.t, recorder, &answer)
	}
	return recorder, answer
}

// wantAccepted holds a take's answer to an accepted take whose text is text.
func wantAccepted(t *testing.T, recorder *httptest.ResponseRecorder, answer claim.TakeResponse, text string) {
	t.Helper()
	if recorder.Code != http.StatusOK || answer != (claim.TakeResponse{Outcome: string(supervise.TakeOutcomeAccepted), Text: text}) {
		t.Fatalf("the take = %d %+v (body %s), want 200 accepted with %q", recorder.Code, answer, recorder.Body, text)
	}
}

// startAndEndTurn has the claim's worker start the turn of the task it took and end it, as its
// stream's agent_start and agent_end do.
func (h *harness) startAndEndTurn(token claim.Token) {
	h.t.Helper()
	m := h.machine(token)
	for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: token}, supervise.StreamTurnEnd{Claim: token}} {
		if err := m.Handle(h.ctx, ev); err != nil {
			h.t.Fatalf("%T on %s: %v", ev, token, err)
		}
	}
}

// A worker takes its task under either caller a turn outcome takes: an Oh My Pi session names its
// claim with the registration's secret, and the Claude bridge, which holds none, presents the
// launch's boot token. Both are answered accepted with the task's text, and the claim records the
// take of that number.
func TestTakeRouteAcceptsTheAgentFormAndTheBootForm(t *testing.T) {
	h := newHarness(t)
	agent := h.readyClaim("LEGION-208", claim.RoleArchitect)
	bridge := h.readyClaim("LEGION-209", claim.RoleImplementer)

	agentValue := h.deliverTask(supervise.RequestDeliver{Claim: agent.token, Task: "the architect's task"})
	recorder, answer := h.postTake(takeOf(agent, agentValue))
	wantAccepted(t, recorder, answer, "the architect's task")

	bridgeValue := h.deliverTask(supervise.RequestDeliver{Claim: bridge.token, Task: "the implementer's task"})
	recorder, answer = h.postTake(claim.TakeRequest{BootCaller: bridge.byBoot(), Generation: bridge.generation,
		Number: bridgeValue.Number, Epoch: bridgeValue.Epoch, Revision: bridgeValue.Revision})
	wantAccepted(t, recorder, answer, "the implementer's task")

	for _, c := range []struct {
		token claim.Token
		value claim.TaskValue
	}{{agent.token, agentValue}, {bridge.token, bridgeValue}} {
		stored := h.stored(c.token)
		if stored.TaskTaken != c.value.Number || stored.TaskTakeState != supervise.TakeAccepted ||
			stored.Pending == nil || stored.Pending.ConfirmedAt.IsZero() {
			t.Errorf("%s records taken %d, take state %q, pending %+v; want task %d taken and accepted",
				c.token, stored.TaskTaken, stored.TaskTakeState, stored.Pending, c.value.Number)
		}
	}
}

// A caller the route cannot resolve to a claim is refused on every task route, in the coded shape
// — a worker sends a take or a seen report again on a 502, 503 or 504 that names no code, so a
// refusal the daemon made must never read as an outage — and nothing is recorded: no take, no
// seen revision, no read.
func TestTakeRouteRefusesAnUnknownCallerWithACodeAndRecordsNothing(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})
	before := h.stored(c.token)

	agentBody := func() map[string]any {
		return map[string]any{"claimToken": c.token, "sessionId": c.session, "secret": c.secret, "generation": c.generation}
	}
	for name, testCase := range map[string]struct {
		body    func() map[string]any
		status  int
		code    string
		message string
	}{
		"naming no caller": {
			body:   func() map[string]any { return map[string]any{"generation": c.generation} },
			status: http.StatusBadRequest, code: "INVALID_CALLER",
		},
		"naming both callers": {
			body:   func() map[string]any { b := agentBody(); b["bootToken"] = c.boot; return b },
			status: http.StatusBadRequest, code: "INVALID_CALLER",
		},
		"with no generation": {
			body:   func() map[string]any { b := agentBody(); delete(b, "generation"); return b },
			status: http.StatusBadRequest, code: "MISSING_FIELD", message: "generation is required",
		},
		"naming the claim with no session": {
			body:   func() map[string]any { b := agentBody(); delete(b, "sessionId"); return b },
			status: http.StatusBadRequest, code: "MISSING_FIELD", message: "sessionId is required",
		},
		"with the wrong secret": {
			body:   func() map[string]any { b := agentBody(); b["secret"] = "not the secret"; return b },
			status: claim.InvalidSecret.Status, code: "INVALID_SESSION_SECRET", message: claim.InvalidSecret.Message,
		},
		"naming a claim the daemon does not hold": {
			body:   func() map[string]any { b := agentBody(); b["claimToken"] = "legion-legion-legion-999-tester"; return b },
			status: claim.InvalidSecret.Status, code: "INVALID_SESSION_SECRET", message: claim.InvalidSecret.Message,
		},
		"with a boot token no launch minted": {
			body: func() map[string]any {
				return map[string]any{"bootToken": "not a boot token", "generation": c.generation}
			},
			status: claim.InvalidBootToken.Status, code: "INVALID_BOOT_TOKEN", message: claim.InvalidBootToken.Message,
		},
		"with the boot token for another generation": {
			body:   func() map[string]any { return map[string]any{"bootToken": c.boot, "generation": c.generation + 1} },
			status: claim.StaleGeneration.Status, code: "STALE_GENERATION", message: claim.StaleGeneration.Message,
		},
		"with a member the route does not read": {
			body:   func() map[string]any { b := agentBody(); b["prompt"] = "a pushed task"; return b },
			status: http.StatusBadRequest, code: "INVALID_BODY",
		},
	} {
		for _, route := range []struct {
			path  string
			value bool
		}{{"/legion/v1/claims/take", true}, {"/legion/v1/claims/task-seen", true}, {"/legion/v1/claims/task", false}} {
			t.Run(name+" on "+route.path, func(t *testing.T) {
				body := testCase.body()
				if route.value {
					body["number"], body["epoch"], body["revision"] = value.Number, value.Epoch, value.Revision
				}
				recorder := h.request(http.MethodPost, route.path, body, nil)
				var failure Failure
				if recorder.Code != testCase.status {
					t.Fatalf("status = %d, want %d; body %s", recorder.Code, testCase.status, recorder.Body)
				}
				decodeInto(t, recorder, &failure)
				if failure.Code != testCase.code || failure.Error == "" || (testCase.message != "" && failure.Error != testCase.message) {
					t.Fatalf("refusal = %+v, want code %s and the sentence %q", failure, testCase.code, testCase.message)
				}
				stored := h.stored(c.token)
				if stored.TaskTakeState != before.TaskTakeState || stored.TaskSeenRevision != before.TaskSeenRevision ||
					stored.TaskTaken != 0 || !stored.Pending.ConfirmedAt.IsZero() {
					t.Fatalf("the refused request left take state %q, seen revision %d, taken %d, confirmed %v; want nothing recorded",
						stored.TaskTakeState, stored.TaskSeenRevision.Revision, stored.TaskTaken, stored.Pending.ConfirmedAt)
				}
			})
		}
	}

	// A session the claim does not record, with the claim's own secret, is refused by the claim's
	// machine (supervise's fenceTaskRequest), under the same coded shape.
	wrongSession := takeOf(c, value)
	wrongSession.SessionID = "ses_someone_else"
	recorder, _ := h.postTake(wrongSession)
	refused(t, recorder, claim.SameAgentRefusal.Status, "SESSION_MISMATCH")
	if stored := h.stored(c.token); stored.TaskTaken != 0 || stored.TaskTakeState != "" {
		t.Fatalf("the refused take left taken %d and take state %q, want nothing recorded", stored.TaskTaken, stored.TaskTakeState)
	}
}

// A claim whose plugin speaks another contract, while the daemon relaunches it onto the current
// one, has its take answered refused with the hold `stale-plugin` rather than the stale-contract
// 409 its ready gets: the stale-plugin hold has an answer, and the worker waits it out like any
// other hold. Its read of its value and its seen report are answered as any claim's are, so a
// worker on an older contract is never left with nothing to do but fail its boot.
func TestTakeRouteAnswersAStaleClaimRefusedStalePluginRatherThan409(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})
	h.makeStale(t, c.token, c.session, c.secret, DaemonAPIVersion-1)

	// The control: the same claim, as the claim routes read it, is stale.
	ready := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: c.token, SessionID: c.session, Secret: c.secret, Generation: c.generation,
	}, nil)
	if ready.Code != http.StatusConflict {
		t.Fatalf("the stale claim's ready = %d (body %s), want the stale-contract 409", ready.Code, ready.Body)
	}

	recorder, answer := h.postTake(takeOf(c, value))
	if recorder.Code != http.StatusOK || answer != (claim.TakeResponse{Outcome: string(supervise.TakeOutcomeRefused), Hold: supervise.HoldStalePlugin}) {
		t.Fatalf("the stale claim's take = %d %+v (body %s), want 200 refused with hold %q", recorder.Code, answer, recorder.Body, supervise.HoldStalePlugin)
	}
	if stored := h.stored(c.token); stored.TaskTaken != 0 || stored.TaskTakeState != supervise.TakeRefused {
		t.Fatalf("the stale claim records taken %d and take state %q, want nothing taken and the take refused", stored.TaskTaken, stored.TaskTakeState)
	}

	seen := h.request(http.MethodPost, "/legion/v1/claims/task-seen", claim.TaskSeenRequest{AgentCaller: c.agent(), Generation: c.generation,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision + 1}, nil)
	if seen.Code != http.StatusNoContent {
		t.Fatalf("the stale claim's seen report = %d (body %s), want 204", seen.Code, seen.Body)
	}
	read, current := h.postTask(claim.TaskRequest{AgentCaller: c.agent(), Generation: c.generation})
	if read.Code != http.StatusOK || current.Task == nil || *current.Task != value {
		t.Fatalf("the stale claim's read = %d %+v (body %s), want 200 with its value %+v", read.Code, current, read.Body, value)
	}
}

// The report of a take's turn is not refused by the stale-contract 409 either: it is the only
// thing that finishes the task of a worker whose plugin names its number (supervise's
// reportedTask), so refused, it would leave the task to be taken back as a cut turn at the
// stale-plugin relaunch and run again by the next process. The claim here is the shape every
// contract raise leaves: idle, its take's turn ended on the worker stream before the report
// arrived, and its plugin on a contract other than the daemon's that names the number (the
// contract after this one stands in for the take contract once the next raise lands, since this
// daemon's own contract is the take contract). The report naming the number finishes the task,
// while the claim's ready, which the stale refusal covers, is still refused.
func TestTurnOutcomeOfAStaleClaimNamingItsTakeFinishesTheTask(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})
	recorder, answer := h.postTake(takeOf(c, value))
	wantAccepted(t, recorder, answer, "the task")
	h.startAndEndTurn(c.token)
	stale := DaemonAPIVersion + 1
	if supervise.TakesNoTask(stale) {
		t.Fatalf("contract %d takes no task: this test proves nothing, since the end frame finishes such a claim's task", stale)
	}
	h.makeStale(t, c.token, c.session, c.secret, stale)
	if stored := h.stored(c.token); stored.State != supervise.StateIdle || stored.Pending == nil || stored.Pending.Number != value.Number || stored.TaskFinished != 0 {
		t.Fatalf("the stale claim is %s holding %+v with task %d finished, want idle with task %d's turn over and unfinished",
			stored.State, stored.Pending, stored.TaskFinished, value.Number)
	}

	// The control: the same claim, as the claim routes read it, is stale.
	ready := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: c.token, SessionID: c.session, Secret: c.secret, Generation: c.generation,
	}, nil)
	wantRefusal(t, ready, http.StatusConflict, claim.StalePluginContract(stale, DaemonAPIVersion).Message)

	outcome, _ := h.turnOutcome(claim.TurnOutcomeRequest{AgentCaller: c.agent(), Generation: c.generation, Sequence: 1,
		Outcome: claim.TurnClean, Task: value.Number})
	if outcome.Code != http.StatusOK {
		t.Fatalf("the stale claim's turn outcome naming task %d = %d (body %s), want 200", value.Number, outcome.Code, outcome.Body)
	}
	if stored := h.stored(c.token); stored.TaskFinished != value.Number || (stored.Pending != nil && stored.Pending.Number == value.Number) {
		t.Fatalf("after the report the claim records task %d finished, pending %+v; want task %d finished and no longer pending",
			stored.TaskFinished, stored.Pending, value.Number)
	}
	if _, held := h.taskValues().Value(c.token); held {
		t.Fatal("the bucket still holds the finished task's value")
	}
}

// Every outcome a take has reaches the worker through the route, decided by a real machine: a
// number the claim's record does not owe it is refused `not-owed` and records nothing (R3), and
// the pending task of a phase its issue has left is dropped, its value deleted, so the read that
// follows finds none.
func TestTakeRouteAnswersRefusedNotOwedAndDroppedOverARealMachine(t *testing.T) {
	h := newHarness(t)
	holds := true
	h.supervisor.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return holds, nil }
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "implement it", Phase: phase.Implementing, Generation: 1})

	notOwed := value
	notOwed.Number++
	recorder, answer := h.postTake(takeOf(c, notOwed))
	if recorder.Code != http.StatusOK || answer != (claim.TakeResponse{Outcome: string(supervise.TakeOutcomeRefused), Hold: supervise.HoldNotOwed}) {
		t.Fatalf("the take of a number not owed = %d %+v (body %s), want 200 refused %q", recorder.Code, answer, recorder.Body, supervise.HoldNotOwed)
	}
	if stored := h.stored(c.token); stored.TaskTakeState != "" || stored.TaskSeenRevision.Revision != 0 || stored.TaskTaken != 0 {
		t.Fatalf("the refused take of a number not owed left take state %q, seen revision %d, taken %d; want nothing recorded",
			stored.TaskTakeState, stored.TaskSeenRevision.Revision, stored.TaskTaken)
	}

	holds = false
	recorder, answer = h.postTake(takeOf(c, value))
	if recorder.Code != http.StatusOK || answer != (claim.TakeResponse{Outcome: string(supervise.TakeOutcomeDropped)}) {
		t.Fatalf("the take of a task whose phase was left = %d %+v (body %s), want 200 dropped", recorder.Code, answer, recorder.Body)
	}
	if stored := h.stored(c.token); stored.Pending != nil || stored.TaskTaken != 0 {
		t.Fatalf("the dropped task left pending %+v and taken %d, want it retired untaken", stored.Pending, stored.TaskTaken)
	}
	if _, held := h.taskValues().Value(c.token); held {
		t.Fatal("the bucket still holds the dropped task's value")
	}
	read, current := h.postTask(claim.TaskRequest{AgentCaller: c.agent(), Generation: c.generation})
	if read.Code != http.StatusOK || !reflect.DeepEqual(current, claim.TaskResponse{}) {
		t.Fatalf("the read after the drop = %d %+v (body %s), want no value and nothing finished", read.Code, current, read.Body)
	}
}

// A take the worker sends again — its answer was lost, or a daemon restart cut it off — is
// answered the same text and applies nothing a second time: no second write of the value, no
// second record of the take, across the restart too, since the text is kept on the pending row
// (dispatch://INFRA-2811).
func TestTakeRouteAnswersARepeatedTakeTheSameTextAndAppliesNothingAgain(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})

	recorder, first := h.postTake(takeOf(c, value))
	wantAccepted(t, recorder, first, "the task")
	taken := h.stored(c.token)
	writes, deletes := len(h.taskValues().Writes()), len(h.taskValues().Deletes())

	recorder, again := h.postTake(takeOf(c, value))
	wantAccepted(t, recorder, again, first.Text)
	h.supervisor.restart(t, c.token)
	recorder, afterRestart := h.postTake(takeOf(c, value))
	wantAccepted(t, recorder, afterRestart, first.Text)

	stored := h.stored(c.token)
	if stored.TaskTaken != taken.TaskTaken || stored.TaskTakeState != taken.TaskTakeState ||
		stored.Pending == nil || !stored.Pending.ConfirmedAt.Equal(taken.Pending.ConfirmedAt) || stored.Pending.TakenText != first.Text {
		t.Fatalf("after the repeated takes the claim records taken %d (%q), pending %+v; want the first take's record unchanged, taken %d confirmed at %v",
			stored.TaskTaken, stored.TaskTakeState, stored.Pending, taken.TaskTaken, taken.Pending.ConfirmedAt)
	}
	if got, gone := len(h.taskValues().Writes()), len(h.taskValues().Deletes()); got != writes || gone != deletes {
		t.Fatalf("the repeated takes made %d writes and %d deletes of the value, want none", got-writes, gone-deletes)
	}
}

// A seen report records the revision of the claim's value the worker was handed, under either
// caller, and a revision it records afresh has had no take yet.
func TestTakeRouteRecordsATaskSeenReport(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})

	recorder := h.request(http.MethodPost, "/legion/v1/claims/task-seen", claim.TaskSeenRequest{AgentCaller: c.agent(), Generation: c.generation,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision}, nil)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("the seen report = %d (body %q), want 204 with no body", recorder.Code, recorder.Body)
	}
	if stored := h.stored(c.token); stored.TaskSeenRevision.Epoch != value.Epoch || stored.TaskSeenRevision.Revision != value.Revision || stored.TaskTakeState != "" {
		t.Fatalf("the claim records seen %s/%d with take state %q, want %s/%d and no take", stored.TaskSeenRevision.Epoch, stored.TaskSeenRevision.Revision,
			stored.TaskTakeState, value.Epoch, value.Revision)
	}

	recorder = h.request(http.MethodPost, "/legion/v1/claims/task-seen", claim.TaskSeenRequest{BootCaller: c.byBoot(), Generation: c.generation,
		Number: value.Number, Epoch: value.Epoch, Revision: value.Revision + 1}, nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("the bridge's seen report = %d (body %s), want 204", recorder.Code, recorder.Body)
	}
	if stored := h.stored(c.token); stored.TaskSeenRevision.Revision != value.Revision+1 {
		t.Fatalf("the claim records seen revision %d, want the bridge's %d", stored.TaskSeenRevision.Revision, value.Revision+1)
	}
}

// The read of a claim's value answers what the bucket holds and the number of the last task the
// claim finished, under either caller: nothing before any task, the pending task's value while it
// is owed, and no value with its number finished once the turn-outcome report naming it came.
func TestTakeRouteTaskReadAnswersTheValueAndTheFinishedNumber(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	read := func(req claim.TaskRequest) claim.TaskResponse {
		t.Helper()
		recorder, answer := h.postTask(req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the read = %d; body %s", recorder.Code, recorder.Body)
		}
		return answer
	}
	byAgent := claim.TaskRequest{AgentCaller: c.agent(), Generation: c.generation}
	byBoot := claim.TaskRequest{BootCaller: c.byBoot(), Generation: c.generation}

	if got := read(byAgent); !reflect.DeepEqual(got, claim.TaskResponse{}) {
		t.Fatalf("the read before any task = %+v, want no value and nothing finished", got)
	}
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})
	for _, req := range []claim.TaskRequest{byAgent, byBoot} {
		if got := read(req); got.Task == nil || *got.Task != value || got.Finished != 0 {
			t.Fatalf("the read with the task owed = %+v, want its value %+v and nothing finished", got, value)
		}
	}

	recorder, answer := h.postTake(takeOf(c, value))
	wantAccepted(t, recorder, answer, "the task")
	h.startAndEndTurn(c.token)
	if outcome, _ := h.turnOutcome(claim.TurnOutcomeRequest{AgentCaller: c.agent(), Generation: c.generation, Sequence: 1,
		Outcome: claim.TurnClean, Task: value.Number}); outcome.Code != http.StatusOK {
		t.Fatalf("the turn outcome naming task %d = %d; body %s", value.Number, outcome.Code, outcome.Body)
	}
	if got := read(byBoot); !reflect.DeepEqual(got, claim.TaskResponse{Finished: value.Number}) {
		t.Fatalf("the read after the task finished = %+v, want no value and task %d finished", got, value.Number)
	}
}

// The claim in a turn reads, on its own task route, the takes of its issue's other claims held
// behind that turn (Supervisor.HeldBehind): each held claim's role and its task's number, so its
// worker can tell its model that the take waits for this turn's end (dispatch://INFRA-4130 R2).
// The key is served to a caller that states taskHoldsContract, the contract that brought it, or a
// later one, and to one that states none, which is served this daemon's own body; a caller that
// states a contract below it is answered its task read without the key (shapeAnswer's AnswerTask
// row). With nothing held the key is absent.
func TestTheTaskRouteNamesTheTakesHeldBehindTheCallersTurn(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	var held []claim.HeldTake
	h.supervisor.holdTakes(func(issue string, holder claim.Token) []claim.HeldTake {
		if issue != "LEGION-209" || holder != c.token {
			return nil
		}
		return held
	})
	// read is the claim's task read under req, stating contract where it is not 0, as the keys of
	// its answer.
	read := func(req claim.TaskRequest, contract int) map[string]json.RawMessage {
		t.Helper()
		var header http.Header
		if contract != 0 {
			header = http.Header{pluginContractHeader: {strconv.Itoa(contract)}}
		}
		recorder := h.request(http.MethodPost, "/legion/v1/claims/task", req, header)
		if recorder.Code != http.StatusOK {
			t.Fatalf("the read = %d; body %s", recorder.Code, recorder.Body)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &keys); err != nil {
			t.Fatalf("decode the read %s: %v", recorder.Body, err)
		}
		return keys
	}
	byAgent := claim.TaskRequest{AgentCaller: c.agent(), Generation: c.generation}
	byBoot := claim.TaskRequest{BootCaller: c.byBoot(), Generation: c.generation}

	held = []claim.HeldTake{{Role: claim.RoleTester, Task: 7}}
	for _, req := range []claim.TaskRequest{byAgent, byBoot} {
		for _, contract := range []int{taskHoldsContract, 0} {
			if got := string(read(req, contract)["holds"]); got != `[{"role":"tester","task":7}]` {
				t.Fatalf("the read of a caller stating contract %d with the tester's take held behind its turn answers holds %s, want the tester's task 7", contract, got)
			}
		}
	}
	if keys := read(byAgent, taskHoldsContract-1); keys["holds"] != nil || keys["task"] == nil {
		t.Fatalf("the read of a caller stating contract %d answers %v, want its task read with no holds key", taskHoldsContract-1, keys)
	}

	held = nil
	if keys := read(byAgent, taskHoldsContract); keys["holds"] != nil {
		t.Fatalf("the read with nothing held answers holds %s, want no key", keys["holds"])
	}
}

// taskHoldsContract is the contract whose entry in version.go added `holds`, and this daemon's own
// contract is never below it: a merge that renumbers this change moves its entry and this
// constant together, and a later contract bump changes neither.
func TestTheTaskHoldsContractIsTheContractThatAddedHolds(t *testing.T) {
	const addedHolds = 209
	if taskHoldsContract != addedHolds {
		t.Fatalf("taskHoldsContract = %d, want %d, the contract whose entry in version.go added holds to the task route",
			taskHoldsContract, addedHolds)
	}
	if taskHoldsContract > DaemonAPIVersion {
		t.Fatalf("taskHoldsContract = %d, above DaemonAPIVersion %d: no caller could be served holds",
			taskHoldsContract, DaemonAPIVersion)
	}
}

// The register answer carries the claim's value and its finished number, so a worker holds its
// task before its ready: a launch handed a task before its agent registered is told that task's
// value, and the relaunch after the task finished is told no value and the number it finished.
func TestTakeRouteRegisterAnswerCarriesTheValueAndTheFinishedNumber(t *testing.T) {
	h := newHarness(t)
	token, boot := h.launch("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: token, Task: "the task"})

	registered := h.registered(boot, "ses_implementer")
	if registered.Task == nil || *registered.Task != value || registered.Finished != 0 {
		t.Fatalf("the registration answered task %+v, finished %d; want the value %+v and nothing finished",
			registered.Task, registered.Finished, value)
	}
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: registered.Secret, Generation: registered.Generation,
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", recorder.Code, recorder.Body)
	}
	c := takeClaim{token: token, boot: boot, session: "ses_implementer", secret: registered.Secret, generation: registered.Generation}
	recorder, answer := h.postTake(takeOf(c, value))
	wantAccepted(t, recorder, answer, "the task")
	h.startAndEndTurn(token)
	if outcome, _ := h.turnOutcome(claim.TurnOutcomeRequest{BootCaller: c.byBoot(), Generation: c.generation, Sequence: 1,
		Outcome: claim.TurnClean, Task: value.Number}); outcome.Code != http.StatusOK {
		t.Fatalf("the turn outcome naming task %d = %d; body %s", value.Number, outcome.Code, outcome.Body)
	}

	again := h.registered(h.relaunch(token), "ses_implementer")
	if again.Task != nil || again.Finished != value.Number {
		t.Fatalf("the relaunch's registration answered task %+v, finished %d; want no value and task %d finished",
			again.Task, again.Finished, value.Number)
	}
}

// A turn-outcome report naming the taken task with the provider error the turn ended in does not
// finish the task: the take is given back, the task stays under its number with its value, and
// the ladder's wait holds it, so a take of it meanwhile is refused `provider-error`. A report
// naming the task with no error finishes it (TestTakeRouteTaskReadAnswersTheValueAndTheFinishedNumber).
func TestTakeRouteTurnOutcomeWithAProviderErrorStartsTheWaitAndKeepsTheNumber(t *testing.T) {
	h := newHarness(t)
	c := h.readyClaim("LEGION-209", claim.RoleImplementer)
	value := h.deliverTask(supervise.RequestDeliver{Claim: c.token, Task: "the task"})
	recorder, answer := h.postTake(takeOf(c, value))
	wantAccepted(t, recorder, answer, "the task")
	h.startAndEndTurn(c.token)

	outcome, _ := h.turnOutcome(claim.TurnOutcomeRequest{AgentCaller: c.agent(), Generation: c.generation, Sequence: 1,
		Outcome: claim.TurnRefused, Message: "overloaded", Task: value.Number,
		ProviderError: &claim.ProviderError{Status: http.StatusServiceUnavailable, Provider: "anthropic", Model: "claude-opus", Message: "overloaded", Retryable: true}})
	if outcome.Code != http.StatusOK {
		t.Fatalf("the turn outcome with a provider error = %d; body %s", outcome.Code, outcome.Body)
	}
	stored := h.stored(c.token)
	if stored.Pending == nil || stored.Pending.Number != value.Number || !stored.Pending.ConfirmedAt.IsZero() ||
		stored.Pending.ProviderErrorRetries != 1 || stored.ProviderErrorRun != 1 || stored.TaskFinished != 0 {
		t.Fatalf("after the provider error the claim records pending %+v, provider-error run %d, finished %d; want task %d kept, its take given back, one retry and nothing finished",
			stored.Pending, stored.ProviderErrorRun, stored.TaskFinished, value.Number)
	}
	if held, ok := h.taskValues().Value(c.token); !ok || held.Number != value.Number {
		t.Fatalf("the bucket holds %+v (%v) after the provider error, want task %d's value kept", held, ok, value.Number)
	}
	recorder, answer = h.postTake(takeOf(c, value))
	if recorder.Code != http.StatusOK || answer != (claim.TakeResponse{Outcome: string(supervise.TakeOutcomeRefused), Hold: supervise.HoldProviderError}) {
		t.Fatalf("the take during the ladder's wait = %d %+v (body %s), want 200 refused %q", recorder.Code, answer, recorder.Body, supervise.HoldProviderError)
	}
}
