package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// agentActionRoutes is four of the controller's six agent actions, each by its route and the
// words its refusal of the role `controller` gives.
var agentActionRoutes = []struct {
	name        string
	route       string
	invalidRole string
}{
	{"stop", "/legion/v1/agents/stop", stopOp.invalidRole},
	{"retry", "/legion/v1/agents/retry", retryOp.invalidRole},
	{"suspend", "/legion/v1/agents/suspend", suspendOp.invalidRole},
	{"resume", "/legion/v1/agents/resume", resumeOp.invalidRole},
}

// askAgentAction posts one of the controller's agent actions with a fresh controller grant.
func askAgentAction(t *testing.T, h *harness, route, issue string, role claim.Role) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(http.MethodPost, route, ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: issue, Role: string(role)}, nil)
}

// wantAgentActionAnswer holds a success of the controller's agent actions to its status and
// its body: the claim's state after the request.
func wantAgentActionAnswer(t *testing.T, recorder *httptest.ResponseRecorder, status int, want AgentActionResponse) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var got AgentActionResponse
	decodeInto(t, recorder, &got)
	if got != want {
		t.Fatalf("answer = %+v, want %+v", got, want)
	}
}

// operatorSuspendedImplementer is readyImplementer suspended through the operator's own route,
// whose suspension only the operator lifts.
func operatorSuspendedImplementer(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusOK {
		t.Fatalf("the operator's suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	return token
}

// controllerSuspendedImplementer is readyImplementer suspended through the controller's own
// route, which records the suspension as the controller's.
func controllerSuspendedImplementer(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	if recorder := askAgentAction(t, h, "/legion/v1/agents/suspend", "LEGION-209", claim.RoleImplementer); recorder.Code != http.StatusOK {
		t.Fatalf("the controller's suspend = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByController {
		t.Fatalf("the worker is %s suspended by %q, want it suspended by the controller", got.State, got.SuspendedBy)
	}
	return token
}

// workingImplementer is readyImplementer in a turn: the claim stored working whose process the
// fake runtime answers for as running.
func workingImplementer(t *testing.T, h *harness) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start %s's turn: %v", token, err)
	}
	if got := m.View().State; got != supervise.StateWorking {
		t.Fatalf("the worker is %s, want working", got)
	}
	return token
}

// Each of the four goes through the door fresh_session and compact share: a grant that is not
// the controller's is refused, the role `controller` names no claim, and a claim the daemon does
// not supervise is answered 404.
//
// The 404 is also the whole of the refusal of another project's claim. The door builds the token
// from the daemon's own project (claim.NewToken(s.project, …)), so a request can name an issue
// and a role and never a project: another project's claim reaches this daemon only as an issue it
// supervises no claim for, which is what LEGION-777's tester is here. That is the only form such
// a claim can take, so NO_CLAIM is the only answer it can get.
func TestTheControllersAgentActionsNeedAControllerGrantAClaimRoleAndAClaimOfItsProject(t *testing.T) {
	for _, op := range agentActionRoutes {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
			registered := h.registered(h.bootToken(token), "ses_implementer")
			var worker GrantResponse
			minted := h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
				SessionID: "ses_implementer", Secret: registered.Secret, Tree: "LEGION-208", Issue: "LEGION-209",
			}, nil)
			if minted.Code != http.StatusOK {
				t.Fatalf("mint the worker's grant = %d; body %s", minted.Code, minted.Body)
			}
			decodeInto(t, minted, &worker)
			before := h.stored(token)

			refused(t, h.request(http.MethodPost, op.route,
				ClaimRequest{GrantID: worker.GrantID, Issue: "LEGION-209", Role: string(claim.RoleImplementer)}, nil),
				http.StatusForbidden, "CONTROLLER_REQUIRED")

			refusedSaying(t, askAgentAction(t, h, op.route, "LEGION-209", claim.RoleController),
				http.StatusBadRequest, "INVALID_ROLE",
				"role must be one of architect, planner, implementer, tester, reviewer, merger, a role of this deployment, or a role of the issue's plan; "+
					"the project's controller is none of them, and "+op.invalidRole)

			// Another project's claim: only an issue this daemon supervises no claim for.
			refused(t, askAgentAction(t, h, op.route, "LEGION-777", claim.RoleTester),
				http.StatusNotFound, "NO_CLAIM")

			if after := h.stored(token); after.State != before.State || after.Session != before.Session {
				t.Errorf("after the refused requests the claim is %s on session %q, want it %s on %q as before",
					after.State, after.Session, before.State, before.Session)
			}
		})
	}
}

// The controller's suspend runs the operator's effect and records the suspension as its own:
// the agent's process stops, its session is kept, and the answer and the store name the
// controller. Its own resume lifts it, and starts the agent again in that session.
func TestTheControllerSuspendsAnAgentAsItsOwnAndResumesIt(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	token := readyImplementer(t, h)

	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/suspend", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, AgentActionResponse{State: "suspended", SuspendedBy: "controller"})
	stored := h.stored(token)
	if stored.State != supervise.StateSuspended || stored.SuspendedBy != supervise.SuspendedByController || stored.Session != "ses_implementer" {
		t.Fatalf("the claim is stored %s suspended by %q on session %q, want it suspended by the controller on ses_implementer",
			stored.State, stored.SuspendedBy, stored.Session)
	}
	if !stored.RuleHolds() {
		t.Fatal("the controller's suspension does not hold against the daemon's rules")
	}

	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/resume", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, AgentActionResponse{State: "launching"})
	if resume := h.runtime.CallsOf("Resume"); len(resume) != 1 {
		t.Fatalf("the resume made %d resumes of the session, want 1", len(resume))
	}
}

// A suspension that reaches an agent in a turn is held for that turn's end, as the operator's is,
// and answered 202 with the hold named as the controller's.
func TestTheControllersSuspensionOfAnAgentInATurnIsHeldForTheTurn(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	token := workingImplementer(t, h)

	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/suspend", "LEGION-209", claim.RoleImplementer),
		http.StatusAccepted, AgentActionResponse{State: "working", SuspensionHeld: true, SuspensionHeldBy: "controller"})
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end the turn: %v", err)
	}
	if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByController {
		t.Fatalf("after the turn the claim is %s suspended by %q, want it suspended by the controller", got.State, got.SuspendedBy)
	}
}

// The controller's stop runs the operator's: the agent is stopped, and its claim released and
// retired. The controller's plain retry then starts a stopped phase worker through its issue's
// phase, as the operator's retry does, and asks for no fresh session.
func TestTheControllerStopsAnAgentAndRetriesIt(t *testing.T) {
	facts := &phaseRestarter{}
	h := freshSessionHarness(t, facts)
	token := readyImplementer(t, h)

	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/stop", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, AgentActionResponse{State: "retired"})
	if got := h.stored(token); got.State != supervise.StateRetired || got.Session != "ses_implementer" {
		t.Fatalf("the stopped agent's claim is %s on session %q, want it retired with its session kept", got.State, got.Session)
	}

	wantAgentActionAnswer(t, askAgentAction(t, h, "/legion/v1/agents/retry", "LEGION-209", claim.RoleImplementer),
		http.StatusAccepted, AgentActionResponse{State: "retired"})
	applied := facts.recorded()
	if len(applied) != 1 {
		t.Fatalf("the retry applied %#v, want the phase's restart alone", applied)
	}
	restart, ok := applied[0].(intake.RestartPhase)
	if !ok || restart.Issue != "LEGION-209" || restart.Role != claim.RoleImplementer || restart.FreshSession {
		t.Fatalf("the retry applied %#v, want LEGION-209's implementing restarted with no fresh session", applied[0])
	}
	if got := h.stored(token).Session; got != "ses_implementer" {
		t.Errorf("after the retry the claim records session %q, want the session it had: a plain retry keeps it", got)
	}
}

// The retry and the resume reach no agent in a turn, and each refuses it in the code both
// recoveries give a claim stored working, the retry before anything is asked and the resume from
// the machine's own refusal, with nothing of the claim changed.
func TestTheControllersRetryAndResumeRefuseAnAgentInATurn(t *testing.T) {
	for _, op := range []struct {
		route string
		op    agentAction
	}{
		{"/legion/v1/agents/retry", retryOp},
		{"/legion/v1/agents/resume", resumeOp},
	} {
		t.Run(op.op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := workingImplementer(t, h)

			refusedSaying(t, askAgentAction(t, h, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, claimInATurn,
				string(token)+" is stored working, and "+op.op.name+" reaches no agent in a turn. "+op.op.reaches+". "+wantAskAgainLater)
			m, _ := h.supervisor.Machine(token)
			if view := m.View(); view.State != supervise.StateWorking || view.SuspensionHeld {
				t.Fatalf("after the refusal the claim is %s with a suspension held %t, want it working with none", view.State, view.SuspensionHeld)
			}
		})
	}
}

// controllerHoldActions is the actions of the controller's door that the operator's own
// suspension refuses: every one of the agent door's but the one it lets past, the compaction
// (compactOp.controllerPassesHold).
var controllerHoldActions = slices.DeleteFunc(slices.Clone(agentDoorActions), func(op agentDoorAction) bool {
	return op.as.controllerPassesHold
})

// operatorHoldRefusal is the sentence of SUSPENDED_BY_OPERATOR for operation by caller that the
// operator's suspension refused before it changed anything (api's refuseHold, nothingChanged).
func operatorHoldRefusal(token claim.Token, operation, caller string) string {
	return string(token) + " is held by the operator's own suspension, which only the operator lifts, so " + operation +
		" by " + caller + " is refused and nothing of the claim changed. Ask the operator: `legion claims resume --claim " +
		string(token) + "` is theirs to run"
}

// operatorLifts is how a compaction's refusal of an agent the operator's suspension holds, asked
// by a caller that suspension outranks, ends: it names the holder and the operator's resume, in
// the words of the hold's own refusal (api's holdSentences), where the claim's own refusal would
// name the fresh session the agent door refuses that caller.
func operatorLifts(token claim.Token) string {
	return "it is held by the operator's own suspension, which only the operator lifts. Ask the operator: `legion claims resume --claim " +
		string(token) + "` is theirs to run"
}

// operatorHeldCompactionRefusal is the compaction's refusal of an agent the operator's suspension
// is held over, asked by the controller (api's refuseHeldSuspension).
func operatorHeldCompactionRefusal(token claim.Token) string {
	return string(token) + " has a suspension held over it, and a compaction reaches no agent a suspension waits over, whatever state it is in; " +
		operatorLifts(token)
}

// The operator's own suspension is the operator's decision, and only the operator undoes it: the
// controller's resume would lift it, its suspend would take it over, its stop would retire the
// agent for its retry to relaunch, its retry would start it, and its fresh session would resume
// it. Each of the five is refused SUSPENDED_BY_OPERATOR with nothing of the claim changed, and the
// operator's own resume then lifts the suspension as before.
func TestTheControllerCannotGetPastTheOperatorsSuspension(t *testing.T) {
	for _, op := range controllerHoldActions {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := operatorSuspendedImplementer(t, h)
			before := h.stored(token)

			refusedSaying(t, askAgentAction(t, h, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, suspendedByOperator, operatorHoldRefusal(token, op.as.name, "the controller"))
			after := h.stored(token)
			if after.State != supervise.StateSuspended || after.SuspendedBy != supervise.SuspendedByOperator || after.Session != before.Session {
				t.Fatalf("after the refusal the claim is %s suspended by %q on session %q, want it suspended by the operator on %q",
					after.State, after.SuspendedBy, after.Session, before.Session)
			}
			if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 0 {
				t.Fatalf("the refused request resumed the session %d times, want none", len(resumes))
			}

			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/resume", nil); recorder.Code != http.StatusOK {
				t.Fatalf("the operator's resume = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
			if got := h.stored(token).State; got != supervise.StateLaunching {
				t.Fatalf("after the operator's resume the claim is %s, want launching", got)
			}
		})
	}
}

// The operator's suspension waiting for the turn's end is the operator's decision already, so the
// controller's door refuses it as it refuses the landed one: each of the five is refused
// SUSPENDED_BY_OPERATOR in the same words, and the claim is left working under the hold with the
// session it had. Left to each operation, the hold would take the code of the claim's state at
// three of them — the resume's and the retry's CLAIM_IN_A_TURN, and the fresh session's held
// suspension — so the controller would read an agent it may ask again for where only the operator
// can act (agentDoor).
func TestTheControllerCannotGetPastTheOperatorsHeldSuspension(t *testing.T) {
	for _, op := range controllerHoldActions {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := workingImplementer(t, h)
			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusAccepted {
				t.Fatalf("the operator's suspend of an agent in a turn = %d, want 202; body %s", recorder.Code, recorder.Body)
			}
			calls := len(h.runtime.Calls())

			refusedSaying(t, askAgentAction(t, h, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, suspendedByOperator, operatorHoldRefusal(token, op.as.name, "the controller"))
			m, _ := h.supervisor.Machine(token)
			if view := m.View(); view.State != supervise.StateWorking || !view.SuspensionHeld ||
				view.SuspensionHeldBy != supervise.SuspendedByOperator || view.Session != "ses_implementer" {
				t.Fatalf("the claim is %s on session %q holding a suspension %t by %q, want it working on ses_implementer under the operator's",
					view.State, view.Session, view.SuspensionHeld, view.SuspensionHeldBy)
			}
			if asked := h.runtime.Calls()[calls:]; len(asked) != 0 {
				t.Fatalf("the refused request asked the runtime %+v, want nothing", asked)
			}
		})
	}
}

// The controller's compaction is the one operation of its door the operator's suspension does not
// refuse (compactOp.controllerPassesHold): a compaction stops, takes over and lifts nothing, so the
// compaction of an agent that suspension holds is answered in its own words with nothing of the
// claim changed — under a landed suspension the agent runs no session to compact, and under a held
// one the answer is the held suspension's refusal. What either names as starting the agent is the
// operator's resume, since the controller's fresh_session of that agent is refused
// SUSPENDED_BY_OPERATOR at the door: the answer sends the controller to the operator rather than to
// a door that refuses it (api's holdSentences).
//
// The negative control is refuseState with no hold read (a nil hold for refuseNotRunning and
// refuseHeldSuspension): both rows then name fresh_session as what starts the agent again.
func TestTheControllersCompactionIsNotRefusedTheOperatorsSuspension(t *testing.T) {
	t.Run("landed", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		token := operatorSuspendedImplementer(t, h)

		recorder := askAgentAction(t, h, "/legion/v1/claims/compact", "LEGION-209", claim.RoleImplementer)

		refusedSaying(t, recorder, http.StatusConflict, claimNotRunning,
			string(token)+" is suspended, so it runs no Oh My Pi session to compact; "+operatorLifts(token))
		if strings.Contains(recorder.Body.String(), "fresh_session") {
			t.Errorf("the refusal is %s, want it to name no fresh session: the door refuses the controller's", recorder.Body)
		}
		if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByOperator {
			t.Fatalf("the claim is %s suspended by %q, want it suspended by the operator as it was", got.State, got.SuspendedBy)
		}
	})

	t.Run("held for the turn's end", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		token := workingImplementer(t, h)
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusAccepted {
			t.Fatalf("the operator's suspend of an agent in a turn = %d, want 202; body %s", recorder.Code, recorder.Body)
		}

		recorder := askAgentAction(t, h, "/legion/v1/claims/compact", "LEGION-209", claim.RoleImplementer)

		refusedSaying(t, recorder, http.StatusConflict, claimInATurn, operatorHeldCompactionRefusal(token))
		if strings.Contains(recorder.Body.String(), "fresh_session") {
			t.Errorf("the refusal is %s, want it to name no fresh session: the door refuses the controller's", recorder.Body)
		}
		m, _ := h.supervisor.Machine(token)
		if view := m.View(); view.State != supervise.StateWorking || !view.SuspensionHeld || view.SuspensionHeldBy != supervise.SuspendedByOperator {
			t.Fatalf("the claim is %s holding a suspension %t by %q, want it working under the operator's", view.State, view.SuspensionHeld, view.SuspensionHeldBy)
		}
	})
}

// The operator's door is the operator's, and lifts a suspension of either: the operator's
// retry --fresh-session of an agent the operator suspended starts it in a new session, and the
// operator's resume lifts the controller's suspension.
func TestTheOperatorLiftsItsOwnSuspensionAndTheControllers(t *testing.T) {
	t.Run("the operator's fresh session of its own suspension", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		token := operatorSuspendedImplementer(t, h)
		recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry", RetryRequest{FreshSession: true})
		if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
			t.Fatalf("the operator's fresh session = %d, want it run; body %s", recorder.Code, recorder.Body)
		}
		if got := h.stored(token); got.State != supervise.StateLaunching || got.Session != "" {
			t.Fatalf("the claim is %s on session %q, want it launching in a new session", got.State, got.Session)
		}
	})

	t.Run("the operator's resume of the controller's suspension", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		token := controllerSuspendedImplementer(t, h)
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/resume", nil); recorder.Code != http.StatusOK {
			t.Fatalf("the operator's resume = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		if got := h.stored(token).State; got != supervise.StateLaunching {
			t.Fatalf("after the operator's resume the claim is %s, want launching", got)
		}
	})
}

// The controller's fresh session of an agent it suspended itself runs: the suspension is its own,
// and so is the resume that starts the agent in a new session.
func TestTheControllersFreshSessionLiftsItsOwnSuspension(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	token := controllerSuspendedImplementer(t, h)

	recorder := askAgentAction(t, h, "/legion/v1/claims/fresh-session", "LEGION-209", claim.RoleImplementer)

	if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
		t.Fatalf("the controller's fresh session = %d, want it run; body %s", recorder.Code, recorder.Body)
	}
	if got := h.stored(token); got.State != supervise.StateLaunching || got.Session != "" {
		t.Fatalf("the claim is %s on session %q, want it launching in a new session", got.State, got.Session)
	}
}

// agentDoorAction is one of the six agent actions, by its route and the words its
// refusals name it by.
type agentDoorAction struct {
	name  string
	route string
	as    agentAction
}

// agentDoorActions is the six agent actions of the controller's door: the four the operator's
// `legion claims` gives the controller, and the two recoveries.
var agentDoorActions = []agentDoorAction{
	{"stop", "/legion/v1/agents/stop", stopOp},
	{"retry", "/legion/v1/agents/retry", retryOp},
	{"suspend", "/legion/v1/agents/suspend", suspendOp},
	{"resume", "/legion/v1/agents/resume", resumeOp},
	{"compact", "/legion/v1/claims/compact", compactOp},
	{"fresh_session", "/legion/v1/claims/fresh-session", freshSessionOp},
}

// treeArchitect is LEGION-208's root architect, live and in a turn as it is when it calls its
// tool, with the tree's root recorded. LEGION-209, the child readyImplementer works, is recorded
// in the same tree (workingIssue), so it is an issue this architect owns.
func treeArchitect(t *testing.T, h *harness) liveClaim {
	t.Helper()
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	seedTree(t, h, "LEGION-208")
	return architect
}

// askAsArchitect posts one of the six agent actions with a fresh grant of architect's.
func askAsArchitect(t *testing.T, h *harness, architect liveClaim, route, issue string, role claim.Role) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(http.MethodPost, route, ClaimRequest{GrantID: architect.grant(t), Issue: issue, Role: string(role)}, nil)
}

// An architect's grant opens none of the six agent actions — stop, retry, suspend, resume_agent,
// compact and fresh_session — not even on an agent of an issue it owns: each refuses it 403
// CONTROLLER_REQUIRED, in the words its door gives every grant that is not the controller's,
// before the claim is looked up, and nothing of the claim changes and nothing reaches the
// runtime. Each row starts from an agent its action would change if the architect were served:
// the stop, the suspension, the compaction and the fresh session from a ready worker, the retry
// from one the operator stopped, and the resume from one the controller suspended.
//
// The negative control is agentDoor taking an architect's grant for an agent of an issue it owns,
// as it did at 5205ab53: each row is then served, and the claim changes.
func TestAnArchitectsGrantIsRefusedAtEveryAgentDoor(t *testing.T) {
	stopped := func(t *testing.T, h *harness) claim.Token {
		t.Helper()
		token := readyImplementer(t, h)
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil); recorder.Code != http.StatusOK {
			t.Fatalf("the operator's stop = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		return token
	}
	// agents is, for each action, the agent the action would change if the architect were
	// served.
	agents := map[string]func(t *testing.T, h *harness) claim.Token{
		"stop":          readyImplementer,
		"retry":         stopped,
		"suspend":       readyImplementer,
		"resume":        controllerSuspendedImplementer,
		"compact":       readyImplementer,
		"fresh_session": readyImplementer,
	}
	if len(agents) != len(agentDoorActions) {
		t.Fatalf("the test names an agent for %d actions, want one for each of the door's %d", len(agents), len(agentDoorActions))
	}
	for _, op := range agentDoorActions {
		agent, named := agents[op.name]
		if !named {
			t.Fatalf("the test names no agent for the action %s", op.name)
		}
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)
			token := agent(t, h)
			before := h.stored(token)
			calls := len(h.runtime.Calls())

			refusedSaying(t, askAsArchitect(t, h, architect, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusForbidden, "CONTROLLER_REQUIRED", op.as.name+" requires a controller grant")

			if after := h.stored(token); !reflect.DeepEqual(after, before) {
				t.Fatalf("the refused request changed the claim:\nbefore %+v\nafter  %+v", before, after)
			}
			m, _ := h.supervisor.Machine(token)
			if view := m.View(); view.State != before.State || view.SuspensionHeld {
				t.Fatalf("the refused request left the claim %s with a suspension held %t, want it %s with none",
					view.State, view.SuspensionHeld, before.State)
			}
			if got := len(h.runtime.Calls()); got != calls {
				t.Fatalf("the refused request reached the runtime: %v", h.runtime.Methods()[calls:])
			}
		})
	}
}
