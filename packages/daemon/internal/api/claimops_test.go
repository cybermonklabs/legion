package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimOperationRoutes is the controller's four claim operations, each by its route and the
// words its refusal of the role `controller` gives.
var claimOperationRoutes = []struct {
	name        string
	route       string
	invalidRole string
}{
	{"stop", "/legion/v1/claims/stop", stopOp.invalidRole},
	{"retry", "/legion/v1/claims/retry", retryOp.invalidRole},
	{"suspend", "/legion/v1/claims/suspend", suspendOp.invalidRole},
	{"resume", "/legion/v1/claims/resume", resumeOp.invalidRole},
}

// askClaimOperation posts one of the controller's claim operations with a fresh controller grant.
func askClaimOperation(t *testing.T, h *harness, route, issue string, role claim.Role) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(http.MethodPost, route, ClaimRequest{GrantID: controllerGrant(t, h).GrantID, Issue: issue, Role: string(role)}, nil)
}

// wantClaimOperationAnswer holds a success of the controller's claim operations to its status and
// its body: the claim as the request left it.
func wantClaimOperationAnswer(t *testing.T, recorder *httptest.ResponseRecorder, status int, want ClaimOperationResponse) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var got ClaimOperationResponse
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
	if recorder := askClaimOperation(t, h, "/legion/v1/claims/suspend", "LEGION-209", claim.RoleImplementer); recorder.Code != http.StatusOK {
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
func TestTheControllersClaimOperationsNeedAControllerGrantAClaimRoleAndAClaimOfItsProject(t *testing.T) {
	for _, op := range claimOperationRoutes {
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

			refusedSaying(t, askClaimOperation(t, h, op.route, "LEGION-209", claim.RoleController),
				http.StatusBadRequest, "INVALID_ROLE",
				"role must be one of architect, planner, implementer, tester, reviewer, merger; "+
					"the project's controller is none of them, and "+op.invalidRole)

			// Another project's claim: only an issue this daemon supervises no claim for.
			refused(t, askClaimOperation(t, h, op.route, "LEGION-777", claim.RoleTester),
				http.StatusNotFound, "NO_CLAIM")

			if after := h.stored(token); after.State != before.State || after.Session != before.Session {
				t.Errorf("the refused requests left the claim %s on session %q, want it %s on %q as they found it",
					after.State, after.Session, before.State, before.Session)
			}
		})
	}
}

// The controller's suspend runs the operator's effect and records the suspension as its own:
// the claim's process stops, its session is kept, and the answer and the store name the
// controller. Its own resume lifts it, and starts the claim again in that session.
func TestTheControllerSuspendsAClaimAsItsOwnAndResumesIt(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	token := readyImplementer(t, h)

	wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/suspend", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "suspended", SuspendedBy: "controller"})
	stored := h.stored(token)
	if stored.State != supervise.StateSuspended || stored.SuspendedBy != supervise.SuspendedByController || stored.Session != "ses_implementer" {
		t.Fatalf("the claim is stored %s suspended by %q on session %q, want it suspended by the controller on ses_implementer",
			stored.State, stored.SuspendedBy, stored.Session)
	}
	if !stored.RuleHolds() {
		t.Fatal("the controller's suspension does not hold against the daemon's rules")
	}

	wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/resume", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "launching"})
	if resume := h.runtime.CallsOf("Resume"); len(resume) != 1 {
		t.Fatalf("the resume made %d resumes of the session, want 1", len(resume))
	}
}

// A suspension that reaches a claim in a turn is held for that turn's end, as the operator's is,
// and answered 202 with the hold named as the controller's.
func TestTheControllersSuspensionOfAClaimInATurnIsHeldForTheTurn(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	token := workingImplementer(t, h)

	wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/suspend", "LEGION-209", claim.RoleImplementer),
		http.StatusAccepted, ClaimOperationResponse{State: "working", SuspensionHeld: true, SuspensionHeldBy: "controller"})
	m, _ := h.supervisor.Machine(token)
	if err := m.Handle(h.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end the turn: %v", err)
	}
	if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByController {
		t.Fatalf("after the turn the claim is %s suspended by %q, want it suspended by the controller", got.State, got.SuspendedBy)
	}
}

// The controller's stop runs the operator's: the claim is released and retired. The
// controller's plain retry then starts a stopped phase worker through its issue's phase, as the
// operator's retry does, and asks for no fresh session.
func TestTheControllerStopsAClaimAndRetriesIt(t *testing.T) {
	facts := &phaseRestarter{}
	h := freshSessionHarness(t, facts)
	token := readyImplementer(t, h)

	wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/stop", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "retired"})
	if got := h.stored(token); got.State != supervise.StateRetired || got.Session != "ses_implementer" {
		t.Fatalf("the stopped claim is %s on session %q, want it retired with its session kept", got.State, got.Session)
	}

	wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/retry", "LEGION-209", claim.RoleImplementer),
		http.StatusAccepted, ClaimOperationResponse{State: "retired"})
	applied := facts.recorded()
	if len(applied) != 1 {
		t.Fatalf("the retry applied %#v, want the phase's restart alone", applied)
	}
	restart, ok := applied[0].(intake.RestartPhase)
	if !ok || restart.Issue != "LEGION-209" || restart.Role != claim.RoleImplementer || restart.FreshSession {
		t.Fatalf("the retry applied %#v, want LEGION-209's implementing restarted with no fresh session", applied[0])
	}
	if got := h.stored(token).Session; got != "ses_implementer" {
		t.Errorf("the retry left the claim recording session %q, want the session it had: a plain retry keeps it", got)
	}
}

// The retry and the resume reach no claim in a turn, and each refuses it in the code both
// recoveries give a claim stored working, the retry before anything is asked and the resume from
// the machine's own refusal, with nothing of the claim changed.
func TestTheControllersRetryAndResumeRefuseAClaimInATurn(t *testing.T) {
	for _, op := range []struct {
		route string
		op    claimOp
	}{
		{"/legion/v1/claims/retry", retryOp},
		{"/legion/v1/claims/resume", resumeOp},
	} {
		t.Run(op.op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := workingImplementer(t, h)

			refusedSaying(t, askClaimOperation(t, h, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, claimInATurn,
				string(token)+" is stored working, and "+op.op.name+" reaches no claim in a turn. "+op.op.reaches+". "+wantAskAgainLater)
			m, _ := h.supervisor.Machine(token)
			if view := m.View(); view.State != supervise.StateWorking || view.SuspensionHeld {
				t.Fatalf("the refused claim is %s with a suspension held %t, want it working with none", view.State, view.SuspensionHeld)
			}
		})
	}
}

// controllerHoldOperations is the operations of the controller's door that the operator's own
// suspension refuses: every one of the claim door's but the one it lets past, the compaction
// (compactOp.controllerPassesHold).
var controllerHoldOperations = slices.DeleteFunc(slices.Clone(claimDoorOperations), func(op claimDoorOperation) bool {
	return op.as.controllerPassesHold
})

// operatorHoldRefusal is the sentence of SUSPENDED_BY_OPERATOR for operation by caller that the
// operator's suspension refused before it changed anything (api's refuseHold, nothingChanged).
func operatorHoldRefusal(token claim.Token, operation, caller string) string {
	return string(token) + " is held by the operator's own suspension, which only the operator lifts, so " + operation +
		" by " + caller + " is refused and nothing of the claim changed. Ask the operator: `legion claims resume --claim " +
		string(token) + "` is theirs to run"
}

// operatorLifts is how a compaction's refusal of a claim the operator's suspension holds, asked
// by a caller that suspension outranks, ends: it names the holder and the operator's resume, in
// the words of the hold's own refusal (api's holdSentences), where the claim's own refusal would
// name the fresh session the claim door refuses that caller.
func operatorLifts(token claim.Token) string {
	return "it is held by the operator's own suspension, which only the operator lifts. Ask the operator: `legion claims resume --claim " +
		string(token) + "` is theirs to run"
}

// operatorHeldCompactionRefusal is the compaction's refusal of a claim the operator's suspension
// is held over, asked by the controller (api's refuseHeldSuspension).
func operatorHeldCompactionRefusal(token claim.Token) string {
	return string(token) + " has a suspension held over it, and a compaction reaches no claim a suspension waits over, whatever state it is in; " +
		operatorLifts(token)
}

// The operator's own suspension is the operator's decision, and only the operator undoes it: the
// controller's resume would lift it, its suspend would take it over, its stop would retire the
// claim for its retry to relaunch, its retry would start it, and its fresh session would resume
// it. Each of the five is refused SUSPENDED_BY_OPERATOR with nothing of the claim changed, and the
// operator's own resume then lifts the suspension as before.
func TestTheControllerCannotGetPastTheOperatorsSuspension(t *testing.T) {
	for _, op := range controllerHoldOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := operatorSuspendedImplementer(t, h)
			before := h.stored(token)

			refusedSaying(t, askClaimOperation(t, h, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, suspendedByOperator, operatorHoldRefusal(token, op.as.name, "the controller"))
			after := h.stored(token)
			if after.State != supervise.StateSuspended || after.SuspendedBy != supervise.SuspendedByOperator || after.Session != before.Session {
				t.Fatalf("the refused claim is %s suspended by %q on session %q, want it suspended by the operator on %q",
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
// three of them — the resume's and the retry's claim in a turn, and the fresh session's held
// suspension — so the controller would read a claim it may ask again for where only the operator
// can act (claimDoor).
func TestTheControllerCannotGetPastTheOperatorsHeldSuspension(t *testing.T) {
	for _, op := range controllerHoldOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			token := workingImplementer(t, h)
			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/suspend", nil); recorder.Code != http.StatusAccepted {
				t.Fatalf("the operator's suspend of a claim in a turn = %d, want 202; body %s", recorder.Code, recorder.Body)
			}
			calls := len(h.runtime.Calls())

			refusedSaying(t, askClaimOperation(t, h, op.route, "LEGION-209", claim.RoleImplementer),
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
// refuse (compactOp.controllerPassesHold): a compaction stops, takes over and lifts nothing, so a
// claim that suspension holds is answered in the compaction's own words and left as it was — a
// landed one runs no session to compact, and a held one is the held suspension's refusal. What
// either names as starting the claim is the operator's resume, since the controller's
// fresh_session of that claim is refused SUSPENDED_BY_OPERATOR at the door: the answer sends the
// controller to the operator rather than to a door that refuses it (api's holdSentences).
//
// The negative control is refuseState with no hold read (a nil hold for refuseNotRunning and
// refuseHeldSuspension): both rows then name fresh_session as what starts the claim again.
func TestTheControllersCompactionIsNotRefusedTheOperatorsSuspension(t *testing.T) {
	t.Run("landed", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		token := operatorSuspendedImplementer(t, h)

		recorder := askClaimOperation(t, h, "/legion/v1/claims/compact", "LEGION-209", claim.RoleImplementer)

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
			t.Fatalf("the operator's suspend of a claim in a turn = %d, want 202; body %s", recorder.Code, recorder.Body)
		}

		recorder := askClaimOperation(t, h, "/legion/v1/claims/compact", "LEGION-209", claim.RoleImplementer)

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
// retry --fresh-session of a claim the operator suspended starts it in a new session, and the
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

// The controller's fresh session of a claim it suspended itself runs: the suspension is its own,
// and so is the resume that starts the claim in a new session.
func TestTheControllersFreshSessionLiftsItsOwnSuspension(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	token := controllerSuspendedImplementer(t, h)

	recorder := askClaimOperation(t, h, "/legion/v1/claims/fresh-session", "LEGION-209", claim.RoleImplementer)

	if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
		t.Fatalf("the controller's fresh session = %d, want it run; body %s", recorder.Code, recorder.Body)
	}
	if got := h.stored(token); got.State != supervise.StateLaunching || got.Session != "" {
		t.Fatalf("the claim is %s on session %q, want it launching in a new session", got.State, got.Session)
	}
}

// claimDoorOperation is one of the claim door's operations, by its route and the words its
// refusals name it by.
type claimDoorOperation struct {
	name  string
	route string
	as    claimOp
}

// claimDoorOperations is the six operations of the claim door: the four the operator's `legion
// claims` gives the controller, and the two recoveries. An architect takes all six for the claims
// of the issues it owns.
var claimDoorOperations = []claimDoorOperation{
	{"stop", "/legion/v1/claims/stop", stopOp},
	{"retry", "/legion/v1/claims/retry", retryOp},
	{"suspend", "/legion/v1/claims/suspend", suspendOp},
	{"resume", "/legion/v1/claims/resume", resumeOp},
	{"compact", "/legion/v1/claims/compact", compactOp},
	{"fresh_session", "/legion/v1/claims/fresh-session", freshSessionOp},
}

// treeArchitect is LEGION-208's root architect, live and in a turn as it is when it calls its
// tool, with the tree's root recorded: the caller of every architect case below. LEGION-209, the
// child readyImplementer works, is recorded in the same tree (workingIssue).
func treeArchitect(t *testing.T, h *harness) liveClaim {
	t.Helper()
	architect := newLiveClaim(t, h, "LEGION-208", claim.RoleArchitect)
	seedTree(t, h, "LEGION-208")
	return architect
}

// askAsArchitect posts one of the claim door's operations with a fresh grant of architect's.
func askAsArchitect(t *testing.T, h *harness, architect liveClaim, route, issue string, role claim.Role) *httptest.ResponseRecorder {
	t.Helper()
	return h.request(http.MethodPost, route, ClaimRequest{GrantID: architect.grant(t), Issue: issue, Role: string(role)}, nil)
}

// architectSuspendedImplementer is readyImplementer suspended through an architect's own route,
// which records the suspension as the architect's.
func architectSuspendedImplementer(t *testing.T, h *harness, architect liveClaim) claim.Token {
	t.Helper()
	token := readyImplementer(t, h)
	wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/suspend", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "suspended", SuspendedBy: "architect"})
	if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByArchitect {
		t.Fatalf("the worker is %s suspended by %q, want it suspended by the architect", got.State, got.SuspendedBy)
	}
	return token
}

// An architect's grant opens each of the six on a claim of an issue it owns, and each runs the
// effect the controller's runs: the stop retires the worker, the retry starts the stopped worker
// through its issue's phase, the suspension is recorded as the architect's and holds against the
// daemon's rules, the architect's resume lifts its own suspension, the compaction compacts the
// worker's session, and the fresh session starts it in a new one.
func TestAnArchitectsClaimOperationsServeAClaimOfItsOwnTree(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		token := readyImplementer(t, h)

		wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/stop", "LEGION-209", claim.RoleImplementer),
			http.StatusOK, ClaimOperationResponse{State: "retired"})
		if got := h.stored(token); got.State != supervise.StateRetired || got.Session != "ses_implementer" {
			t.Fatalf("the stopped claim is %s on session %q, want it retired with its session kept", got.State, got.Session)
		}
	})

	t.Run("retry", func(t *testing.T) {
		facts := &phaseRestarter{}
		h := freshSessionHarness(t, facts)
		architect := treeArchitect(t, h)
		token := readyImplementer(t, h)
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil); recorder.Code != http.StatusOK {
			t.Fatalf("the operator's stop = %d, want 200; body %s", recorder.Code, recorder.Body)
		}

		wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/retry", "LEGION-209", claim.RoleImplementer),
			http.StatusAccepted, ClaimOperationResponse{State: "retired"})
		applied := facts.recorded()
		if len(applied) != 1 {
			t.Fatalf("the retry applied %#v, want the phase's restart alone", applied)
		}
		if restart, ok := applied[0].(intake.RestartPhase); !ok || restart.Issue != "LEGION-209" || restart.Role != claim.RoleImplementer || restart.FreshSession {
			t.Fatalf("the retry applied %#v, want LEGION-209's implementing restarted with no fresh session", applied[0])
		}
	})

	t.Run("suspend", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		token := architectSuspendedImplementer(t, h, architect)
		stored := h.stored(token)
		if stored.Session != "ses_implementer" || !stored.RuleHolds() {
			t.Fatalf("the architect's suspension left session %q and holds against the rules %t, want ses_implementer kept and the hold",
				stored.Session, stored.RuleHolds())
		}
	})

	t.Run("resume", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		architectSuspendedImplementer(t, h, architect)

		wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/resume", "LEGION-209", claim.RoleImplementer),
			http.StatusOK, ClaimOperationResponse{State: "launching"})
		if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 1 {
			t.Fatalf("the resume made %d resumes of the session, want 1", len(resumes))
		}
	})

	t.Run("compact", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		token := readyImplementer(t, h)
		conn := connected(h, token)

		answer := decodeCompaction(t, askAsArchitect(t, h, architect, "/legion/v1/claims/compact", "LEGION-209", claim.RoleImplementer))
		if !answer.Compacted || answer.Reason != "" {
			t.Fatalf("the compaction answered %+v, want it compacted with no reason", answer)
		}
		settled(h, token)
		if got := len(conn.Compactions()); got != 1 {
			t.Fatalf("Oh My Pi was asked for %d compactions, want one", got)
		}
		if got := h.stored(token); got.State != supervise.StateReady || got.Session != "ses_implementer" {
			t.Fatalf("the compacted claim is %s on session %q, want it ready again on ses_implementer", got.State, got.Session)
		}
	})

	t.Run("fresh_session", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		token := readyImplementer(t, h)

		recorder := askAsArchitect(t, h, architect, "/legion/v1/claims/fresh-session", "LEGION-209", claim.RoleImplementer)

		if recorder.Code != http.StatusOK && recorder.Code != http.StatusAccepted {
			t.Fatalf("the architect's fresh session = %d, want it run; body %s", recorder.Code, recorder.Body)
		}
		if got := h.stored(token); got.State != supervise.StateLaunching || got.Session != "" {
			t.Fatalf("the claim is %s on session %q, want it launching in a new session", got.State, got.Session)
		}
	})
}

// An architect reaches the claims of the issues it owns alone. A claim of another tree is refused
// 403 ISSUE_OUTSIDE_TREE before it is looked up, with nothing of it changed, as every architect
// operation refuses an issue of another tree (issueOfTree).
func TestAnArchitectsClaimOperationsRefuseAnotherTreesClaim(t *testing.T) {
	for _, op := range claimDoorOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)
			seedTree(t, h, "LEGION-300", "LEGION-301")
			token, _ := h.launchIn("LEGION-300", "LEGION-301", claim.RoleImplementer)
			h.ready(token, "ses_other_tree", 1)
			before := h.stored(token)

			refused(t, askAsArchitect(t, h, architect, op.route, "LEGION-301", claim.RoleImplementer),
				http.StatusForbidden, "ISSUE_OUTSIDE_TREE")

			if after := h.stored(token); after.State != before.State || after.Session != before.Session || after.SuspendedBy != before.SuspendedBy {
				t.Fatalf("the refused request left the other tree's claim %s on %q suspended by %q, want it %s on %q as it found it",
					after.State, after.Session, after.SuspendedBy, before.State, before.Session)
			}
		})
	}
}

// A sub-architect owns its own issue and the issues under it, never its tree's root or a
// sibling's subtree (owns), though its grant names the whole tree, and the claim door holds it
// to what it owns as sign_off and retry_or_escalate do: its stop of the root architect's claim,
// and of the worker of a sibling issue, is refused 403 ISSUE_NOT_OWNED with nothing of either
// claim changed, and its stop of the worker of its own issue retires that worker.
func TestASubArchitectsClaimOperationsReachTheIssuesItOwnsAlone(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	root := treeArchitect(t, h)
	worker := readyImplementer(t, h)
	parent := "LEGION-208"
	h.recordIssue(record.Issue{
		Key: "LEGION-210", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: testProject,
		Title: "LEGION-210", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress",
	})
	sibling, _ := h.launchIn("LEGION-208", "LEGION-210", claim.RoleImplementer)
	h.ready(sibling, "ses_sibling", 1)
	subArchitect := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleArchitect)

	for _, refused := range []struct {
		issue string
		role  claim.Role
		token claim.Token
	}{
		{"LEGION-208", claim.RoleArchitect, root.token},
		{"LEGION-210", claim.RoleImplementer, sibling},
	} {
		before := h.stored(refused.token)
		refusedSaying(t, askAsArchitect(t, h, subArchitect, "/legion/v1/claims/stop", refused.issue, refused.role),
			http.StatusForbidden, "ISSUE_NOT_OWNED",
			"a stop of "+refused.issue+" belongs to its own architect or one above it in the tree, not the architect of LEGION-209")
		if after := h.stored(refused.token); after.State != before.State || after.Session != before.Session {
			t.Fatalf("the refused stop left %s %s on session %q, want it %s on %q as it found it",
				refused.token, after.State, after.Session, before.State, before.Session)
		}
	}

	wantClaimOperationAnswer(t, askAsArchitect(t, h, subArchitect, "/legion/v1/claims/stop", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "retired"})
	if got := h.stored(worker); got.State != supervise.StateRetired || got.Session != "ses_implementer" {
		t.Fatalf("the sub-architect's stop left its own issue's worker %s on session %q, want it retired with its session kept",
			got.State, got.Session)
	}
}

// An architect's grant names an issue the daemon records, and then a claim it supervises: an
// issue no record holds is 404 ISSUE_NOT_FOUND, and a role of its own tree's issue with no claim
// is 404 NO_CLAIM, as the controller's is. A phase worker's grant opens none of the six.
func TestAnArchitectsClaimOperationsNeedARecordedIssueAndAClaim(t *testing.T) {
	for _, op := range claimDoorOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)
			worker := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleImplementer)

			refused(t, askAsArchitect(t, h, architect, op.route, "LEGION-404", claim.RoleImplementer),
				http.StatusNotFound, "ISSUE_NOT_FOUND")
			refused(t, askAsArchitect(t, h, architect, op.route, "LEGION-208", claim.RoleTester),
				http.StatusNotFound, "NO_CLAIM")
			refused(t, h.request(http.MethodPost, op.route,
				ClaimRequest{GrantID: worker.grant(t), Issue: "LEGION-209", Role: string(claim.RoleImplementer)}, nil),
				http.StatusForbidden, "CONTROLLER_REQUIRED")
		})
	}
}

// An architect's own claim is refused in a code of its own: stopping, suspending, compacting or
// restarting the session that makes the call ends or cuts off the turn that would report it, so
// its recovery stays the controller's, the operator's and any architect's above it in the tree.
// The claim is left in its turn.
func TestAnArchitectsClaimOperationsRefuseItsOwnClaim(t *testing.T) {
	for _, op := range claimDoorOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)

			refusedSaying(t, askAsArchitect(t, h, architect, op.route, "LEGION-208", claim.RoleArchitect),
				http.StatusForbidden, ownClaim,
				string(architect.token)+" is the claim of the architect that asks, and "+op.as.name+
					" of it would end or cut off the very turn that asks for it, so it is refused and nothing of the claim changed. "+
					"An architect's own claim is the controller's, the operator's and any architect's above it in the tree to recover")
			m, _ := h.supervisor.Machine(architect.token)
			if view := m.View(); view.State != supervise.StateWorking || view.SuspensionHeld || view.Session != architect.session {
				t.Fatalf("the architect's claim is %s with a suspension held %t on session %q, want it working on %q with none",
					view.State, view.SuspensionHeld, view.Session, architect.session)
			}
		})
	}
}

// The operator's own suspension refuses an architect as it refuses the controller: each of the
// six is refused SUSPENDED_BY_OPERATOR, the sentence naming the architect, with nothing of the
// claim changed.
func TestAnArchitectCannotGetPastTheOperatorsSuspension(t *testing.T) {
	for _, op := range claimDoorOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)
			token := operatorSuspendedImplementer(t, h)
			before := h.stored(token)

			refusedSaying(t, askAsArchitect(t, h, architect, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, suspendedByOperator, operatorHoldRefusal(token, op.as.name, "the architect"))
			after := h.stored(token)
			if after.State != supervise.StateSuspended || after.SuspendedBy != supervise.SuspendedByOperator || after.Session != before.Session {
				t.Fatalf("the refused claim is %s suspended by %q on session %q, want it suspended by the operator on %q",
					after.State, after.SuspendedBy, after.Session, before.Session)
			}
			if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 0 {
				t.Fatalf("the refused request resumed the session %d times, want none", len(resumes))
			}
		})
	}
}

// The controller's suspension is project-wide work a tree's architect must not undo: its resume
// would lift it, its suspend would change nothing, its stop would retire the claim for a retry to
// relaunch, its retry would start it, its fresh session would resume it, and its compaction has no
// session to ask. Each is refused SUSPENDED_BY_CONTROLLER with nothing of the claim changed, and
// the controller's own resume then lifts the suspension as before.
func TestAnArchitectCannotGetPastTheControllersSuspension(t *testing.T) {
	for _, op := range claimDoorOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)
			token := controllerSuspendedImplementer(t, h)
			before := h.stored(token)

			refusedSaying(t, askAsArchitect(t, h, architect, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, suspendedByController,
				string(token)+" is held by the controller's suspension, which only the controller or the operator lifts, so "+op.as.name+
					" by the architect is refused and nothing of the claim changed. Ask the controller: its `resume_claim` is its own to run")
			after := h.stored(token)
			if after.State != supervise.StateSuspended || after.SuspendedBy != supervise.SuspendedByController || after.Session != before.Session {
				t.Fatalf("the refused claim is %s suspended by %q on session %q, want it suspended by the controller on %q",
					after.State, after.SuspendedBy, after.Session, before.Session)
			}
			if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 0 {
				t.Fatalf("the refused request resumed the session %d times, want none", len(resumes))
			}

			wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/resume", "LEGION-209", claim.RoleImplementer),
				http.StatusOK, ClaimOperationResponse{State: "launching"})
		})
	}
}

// The controller's suspension waiting for the turn's end refuses an architect as its landed one
// does: the hold is the controller's decision already, and the claim is left working under it.
func TestAnArchitectCannotGetPastTheControllersHeldSuspension(t *testing.T) {
	for _, op := range claimDoorOperations {
		t.Run(op.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)
			token := workingImplementer(t, h)
			wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/suspend", "LEGION-209", claim.RoleImplementer),
				http.StatusAccepted, ClaimOperationResponse{State: "working", SuspensionHeld: true, SuspensionHeldBy: "controller"})

			refused(t, askAsArchitect(t, h, architect, op.route, "LEGION-209", claim.RoleImplementer),
				http.StatusConflict, suspendedByController)
			m, _ := h.supervisor.Machine(token)
			if view := m.View(); view.State != supervise.StateWorking || !view.SuspensionHeld || view.SuspensionHeldBy != supervise.SuspendedByController {
				t.Fatalf("the claim is %s holding a suspension %t by %q, want it working under the controller's",
					view.State, view.SuspensionHeld, view.SuspensionHeldBy)
			}
		})
	}
}

// An architect's suspension ranks under the controller's: the controller's resume lifts it, and
// so does the operator's.
func TestTheControllersResumeLiftsAnArchitectsSuspension(t *testing.T) {
	t.Run("the controller's resume", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		token := architectSuspendedImplementer(t, h, architect)

		wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/resume", "LEGION-209", claim.RoleImplementer),
			http.StatusOK, ClaimOperationResponse{State: "launching"})
		if got := h.stored(token).State; got != supervise.StateLaunching {
			t.Fatalf("after the controller's resume the claim is %s, want launching", got)
		}
	})

	t.Run("the operator's resume", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		token := architectSuspendedImplementer(t, h, architect)

		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/resume", nil); recorder.Code != http.StatusOK {
			t.Fatalf("the operator's resume = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		if got := h.stored(token).State; got != supervise.StateLaunching {
			t.Fatalf("after the operator's resume the claim is %s, want launching", got)
		}
	})
}

// An architect neither lifts, takes over nor ends a claim under a suspension the daemon placed: its
// resume of it, its fresh session, which would resume it, its suspend, which would take it over
// for that resume to undo, and its stop, which would retire it for a retry to relaunch, are
// refused SUSPENDED_BY_DAEMON before anything changes. Its retry is refused too, by the machine
// itself, which retries a failed or retired claim and never a suspended one: a 409 carrying the
// machine's words and no code of its own, as the operator's and the controller's retry of a
// suspended claim are answered. The claim is left suspended by the daemon on its session after
// each of the five.
func TestAnArchitectsResumeSuspendStopRetryAndFreshSessionRefuseAClaimTheDaemonSuspended(t *testing.T) {
	for _, op := range []struct {
		route string
		as    claimOp
	}{
		{"/legion/v1/claims/resume", resumeOp},
		{"/legion/v1/claims/suspend", suspendOp},
		{"/legion/v1/claims/stop", stopOp},
		{"/legion/v1/claims/retry", retryOp},
		{"/legion/v1/claims/fresh-session", freshSessionOp},
	} {
		t.Run(op.as.name, func(t *testing.T) {
			h := freshSessionHarness(t, &phaseRestarter{})
			architect := treeArchitect(t, h)
			token := suspendedImplementer(t, h, supervise.SuspendedByDaemon)

			recorder := askAsArchitect(t, h, architect, op.route, "LEGION-209", claim.RoleImplementer)
			if op.as.name == retryOp.name {
				if recorder.Code != http.StatusConflict {
					t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusConflict, recorder.Body)
				}
				var failure Failure
				decodeInto(t, recorder, &failure)
				if failure.Code != "" || !strings.HasPrefix(failure.Error, "retry refused: the claim is suspended") {
					t.Fatalf("refusal = %+v, want the machine's own refusal of a retry of a suspended claim, with no code", failure)
				}
			} else {
				refusedSaying(t, recorder, http.StatusConflict, suspendedByDaemon,
					string(token)+" is suspended by the daemon, not by an architect, and "+op.as.name+
						" by the architect does not reach the daemon's suspension, so it is refused and nothing of the claim changed. "+
						"The daemon's own rules start such a claim again — "+
						"its issue's phase, or its role's next wake — and so do the controller's `resume_claim` and the operator's `legion claims resume`")
			}
			if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon || got.Session != "ses_implementer" {
				t.Fatalf("the refused claim is %s suspended by %q on session %q, want it suspended by the daemon on ses_implementer",
					got.State, got.SuspendedBy, got.Session)
			}
			if resumes := h.runtime.CallsOf("Resume"); len(resumes) != 0 {
				t.Fatalf("the refused request resumed the session %d times, want none", len(resumes))
			}
		})
	}
}

// An architect's stop retires its worker, and then the daemon stops the issue's work: the child is
// parked or closed, or its tree's root is closed and its tree lingers. A park suspends no retired
// claim, so no suspension of the daemon's stands over the worker, and the claim door reads the
// issue's record and its root's itself: the architect's retry and its fresh session, either of
// which would start the worker on that stopped work, are refused ISSUE_OUT_OF_WORKFLOW with
// nothing of the claim changed — it stays retired on its session, no session is resumed or
// spawned, and no phase restarts. The controller's retry still reaches the claim.
//
// The negative control is the door without refuseStartOfStoppedWork: the retry then starts the
// worker, through its machine or its issue's phase, and so does the fresh session.
func TestAnArchitectsStartOfAStoppedWorkerIsRefusedOnceItsIssuesWorkStopped(t *testing.T) {
	until := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	stops := []struct {
		name string
		stop func(t *testing.T, h *harness)
		why  string
	}{
		{"the child is parked", func(t *testing.T, h *harness) {
			setIssue(t, h, "LEGION-209", func(issue *record.Issue) { issue.Phase, issue.Status = phase.Done, "backlog" })
		}, "LEGION-209 stands at status backlog and phase done, out of the workflow"},
		{"the child closed as done", func(t *testing.T, h *harness) {
			setIssue(t, h, "LEGION-209", func(issue *record.Issue) { issue.Phase, issue.Status = phase.Done, "done" })
		}, "LEGION-209 stands at status done and phase done, out of the workflow"},
		{"its tree lingers", func(t *testing.T, h *harness) {
			setIssue(t, h, "LEGION-208", func(issue *record.Issue) {
				issue.Phase, issue.Status, issue.LingerUntil = phase.Done, "done", &until
			})
		}, "its tree's root LEGION-208 stands at status done and phase done, out of the workflow and lingers after it left the workflow"},
	}
	// stoppedThenStopsWork is LEGION-209's implementer stopped by its architect and then the
	// issue's work stopped by stop, with the starts the runtime had been asked for by then.
	stoppedThenStopsWork := func(t *testing.T, h *harness, architect liveClaim, stop func(t *testing.T, h *harness)) (claim.Token, int) {
		t.Helper()
		token := readyImplementer(t, h)
		wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/stop", "LEGION-209", claim.RoleImplementer),
			http.StatusOK, ClaimOperationResponse{State: "retired"})
		stop(t, h)
		return token, len(h.runtime.CallsOf("Resume")) + len(h.runtime.CallsOf("Spawn"))
	}
	for _, stopped := range stops {
		for _, op := range []struct {
			route string
			as    claimOp
		}{
			{"/legion/v1/claims/retry", retryOp},
			{"/legion/v1/claims/fresh-session", freshSessionOp},
		} {
			t.Run(stopped.name+": "+op.as.name, func(t *testing.T) {
				facts := &phaseRestarter{}
				h := freshSessionHarness(t, facts)
				architect := treeArchitect(t, h)
				token, starts := stoppedThenStopsWork(t, h, architect, stopped.stop)

				recorder := askAsArchitect(t, h, architect, op.route, "LEGION-209", claim.RoleImplementer)

				refusedSaying(t, recorder, http.StatusConflict, issueOutOfWorkflow,
					string(token)+" is retired, and the daemon has stopped its issue's work: "+stopped.why+". "+
						capitalized(op.as.name)+" by the architect would start the claim on that stopped work, so it is refused and nothing of the claim changed. "+
						"The daemon's own rules start the issue's roles again once the issue and its tree are back in the workflow, and "+
						op.as.name+" by the controller or the operator reaches the claim meanwhile")
				if got := h.stored(token); got.State != supervise.StateRetired || got.Session != "ses_implementer" {
					t.Fatalf("the refused claim is %s on session %q, want it retired on ses_implementer", got.State, got.Session)
				}
				if got := len(h.runtime.CallsOf("Resume")) + len(h.runtime.CallsOf("Spawn")); got != starts {
					t.Fatalf("the refused request started the worker %d times, want none", got-starts)
				}
				if applied := facts.recorded(); len(applied) != 0 {
					t.Fatalf("the refused request applied %#v, want no phase restarted", applied)
				}
			})
		}
	}

	t.Run("the controller's retry", func(t *testing.T) {
		h := freshSessionHarness(t, &phaseRestarter{})
		architect := treeArchitect(t, h)
		stoppedThenStopsWork(t, h, architect, stops[0].stop)

		wantClaimOperationAnswer(t, askClaimOperation(t, h, "/legion/v1/claims/retry", "LEGION-209", claim.RoleImplementer),
			http.StatusOK, ClaimOperationResponse{State: "launching"})
	})
}

// A released claim is no claim the daemon's own rules hold, since any wake brings it back: an
// architect's stop of it retires it, as the operator's and the controller's do.
func TestAnArchitectsStopRetiresAReleasedClaim(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	architect := treeArchitect(t, h)
	token := suspendedImplementer(t, h, supervise.SuspendedByReleased)

	wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/stop", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "retired"})
	if got := h.stored(token); got.State != supervise.StateRetired || got.Session != "ses_implementer" {
		t.Fatalf("the stopped claim is %s on session %q, want it retired with its session kept", got.State, got.Session)
	}
}

// A released claim is no suspension of the daemon's own rules: any wake brings it back, so an
// architect's suspend takes it over and records it as the architect's, and the architect's
// resume then lifts it.
func TestAnArchitectsSuspendTakesOverAReleasedClaim(t *testing.T) {
	h := freshSessionHarness(t, &phaseRestarter{})
	architect := treeArchitect(t, h)
	token := suspendedImplementer(t, h, supervise.SuspendedByReleased)

	wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/suspend", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "suspended", SuspendedBy: "architect"})
	if got := h.stored(token); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByArchitect {
		t.Fatalf("the claim is %s suspended by %q, want it suspended by the architect", got.State, got.SuspendedBy)
	}
	wantClaimOperationAnswer(t, askAsArchitect(t, h, architect, "/legion/v1/claims/resume", "LEGION-209", claim.RoleImplementer),
		http.StatusOK, ClaimOperationResponse{State: "launching"})
}
