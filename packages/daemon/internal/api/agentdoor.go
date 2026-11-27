package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ClaimRequest is what every action of the agent door names (agentDoor): the agent on Role of
// Issue, under the caller's controller grant. Role is a workflow role or `architect`; the
// project controller is no role here, so the controller's own agent cannot be named at all
// (claim.IsRole, which refuses the controller). The six actions take the same members, so they
// take one type.
//
// Claim, from the stranded token's entry (strandedTokenContract), names the agent by its claim
// token: two claims of one issue can hold one role — the agent of a key on a step of another
// role, beside that role's own claim of an earlier version of the plan — and the issue and the
// role reach the one that works the role's step, while a notice of provider errors can name the
// other. It must be a claim of Issue that holds Role. Without it the request reaches the worker on
// the role's step, as it did before that entry; a daemon below it refuses the member as unknown.
type ClaimRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
	Claim   string `json:"claim,omitempty"`
}

// agentAction is one of the six agent actions in the words its own refusals name it by, which
// every door it has gives; each action is described in this one place. name reads inside a
// sentence about the action, and invalidRole is what the door's refusal of the role `controller`
// (INVALID_ROLE) says the action does not do to the project's own controller, which no role names
// (agentDoor). request is the word for the request the stop, the suspension and the resume each
// make as a single event to the claim's machine, which the daemon's stop and the machine's
// refusals name it by (agentEventAction, postClaimEvent, operationFailure), and did is what the
// log line of a stop, a retry, a suspension or a resume that ran says the caller did to the agent
// (answerAgentAction). The retry starts the agent through retryStart, the start the operator's
// own retry route makes too, rather than one event, so it leaves request empty (retryAgent,
// retryStart); the two recoveries write their own answers and log lines, and leave both empty.
//
// refusesATurn is whether the machine's refusal of one of those three events in state working
// is answered as a claim in a turn (refuseAgentAction, refuseClaimInATurn): the resume's
// alone is, since a resume reaches no agent in a turn, and reaches names the states it does
// reach. The stop's is not: the machine refuses a stop of an agent whose claim is working only
// for a tree's root, and that is the root's own refusal, in the machine's words
// (operationFailure). Nor is the suspension's, which is held for a turn rather than refused for it.
// The retry reaches no agent in a turn either, and refuses one from retryAgent's own read of the
// claim's state before the machine is asked anything, so it leaves refusesATurn false and names
// its states in reaches.
// controllerPassesHold is whether the controller's request is let past the operator's
// suspension at the door rather than refused there (agentDoor, supervise.Claim.HeldAgainst):
// the compaction's alone, which stops, takes over and lifts no suspension, so an agent the
// operator's suspension holds is answered by the compaction's own refusals of its claim as it
// stands, which name the operator's resume as what starts it (refuseHeldSuspension,
// refuseState, holdSentences).
//
// cutsTheTurn is what the refusal of an agent whose claim is stored working says the action would
// do to that agent's running turn (refuseInATurn), and reaches is the states the action does
// reach, which every refusal of a claim's state that ends in askAgainLater gives as a
// sentence of its own: it is the one thing a refused caller can act on, since the claim's state
// is published and this says which door that state belongs to (askAgainLater).
//
// liveStates is the live half of reaches on its own — the states of a claim in which the action
// reaches a running agent — for the one refusal that writes its own sentence around it, the
// held suspension's (refuseHeldSuspension). The two recoveries take different sets there, so
// each carries its own: a registered agent is one the fresh session suspends and the
// compaction has no session to ask yet (agentRunning, supervise's onCompact rows), and a
// sentence that named one set for both doors would be short by `registered` at one of them.
// TestEachDoorNamesTheStatesItsOwnPredicatesReach holds both fields to the predicates that
// decide them.
//
// No field says which action a paused project refuses: each door that starts a turn refuses it
// itself, 409 PROJECT_PAUSED with the pause that stands, after the door and before an event for the
// agent action is posted to its machine (refusedWhilePaused), as the operator's doors do. The
// resume refuses it in its own event builder (resumeAgent), as the operator's resume does
// (resumeEvent), the retry before its read of the claim's state (retryAgent), and the two
// recoveries in the paths they share with the operator's door (freshSession, compact). No agent
// action undoes the pause, which the project's resume alone lifts. The stop and the suspension
// start nothing and are let through, as the operator's are.
type agentAction struct {
	name                 string
	invalidRole          string
	request              string
	did                  string
	refusesATurn         bool
	controllerPassesHold bool
	cutsTheTurn          string
	reaches              string
	liveStates           string
}

// claimTarget is the claim of the agent an action of the agent door named, read and
// authenticated: the machine to run the effect on, the issue and role the caller spelled, which
// the route logs as the caller named them, by, who the caller is — the project's controller,
// supervise.SuspendedByController — which is the source its suspension records and what decides
// which suspensions refuse it (supervise.Claim.HeldAgainst), and request, the caller's request
// carrying the step's standing, which the two recoveries run on (agentDoor, claimFreshSession,
// claimCompact).
type claimTarget struct {
	machine *supervise.Machine
	token   claim.Token
	issue   string
	role    claim.Role
	by      supervise.SuspendSource
	request *http.Request
}

// agentDoor is the door all six agent actions come through — the two recoveries
// (claimFreshSession, claimCompact) and the four that stop, retry, suspend and resume an agent
// (stopAgent, retryAgent, suspendAgent, resumeAgent): the agent is named by its issue and role
// rather than by a token the caller never sees, and the caller is its project's controller rather
// than the operator. It answers every refusal of the body, the grant and the lookup itself, and
// reports false when it has; only the effect behind it differs between the six.
//
// The controller's grant is the one grant it takes, and the controller reaches every agent of its
// project. Every other grant, an architect's or a phase worker's, is refused 403
// CONTROLLER_REQUIRED before the claim is looked up, so nothing of any claim is read or changed
// for it.
//
// An agent the operator's suspension holds, held for a turn or landed, is refused here before the
// action asks anything (supervise.Claim.HeldAgainst, refuseHold: SUSPENDED_BY_OPERATOR). So a held
// suspension and a landed one take one code at every action, where the action's own refusals
// would give a held one the code of the claim's state (refuseHeldSuspension, refuseClaimInATurn).
// An operator's suspension that lands after this read is still the machine's to refuse, and each
// action that names its caller to the machine answers that refusal as the hold it is
// (refuseAgentAction, freshSession). The compaction never meets that refusal: the machine's
// compaction carries no caller (supervise.RequestCompact), so an agent such a suspension holds is
// answered by the compaction's own refusals of its claim's state, which name the holder's resume
// as what starts it (refuseState, holdSentences). The compaction is the one request let past such
// a suspension at this door (agentAction.controllerPassesHold).
//
// The controller is refused its own agent here and nowhere else: the token is the claim of the
// agent that works the role's step of the issue (workerToken), which the issue's role names, or
// the claim the body names, which must be an agent's claim of the issue (claim.AgentToken refuses
// the controller's name); no issue's role spells the controller's token (claim.ControllerToken),
// so neither can name it.
//
// The worker's claim is read before any lock an action's acts take, so the request this door
// hands back carries its standing, which the two recoveries run on (worksStep,
// supervise.Standing, claimTarget.request): the recovery's first act on the machine judges again
// under the machine's lock that the record still names the claim on the role's step, and a step
// that moved to another claim before it is refused there with nothing of the claim changed
// (operationFailure, compact's refusal of a claim not taken). A move after that act finds the
// effect under way, and the phase's restart the fresh session applies names the claim, so a step
// entered for another claim since is not restarted (restartWorkedPhase).
//
// A claim the body names must hold the role the body names (409 CLAIM_NOT_OF_ROLE), whether or not
// it is the step's worker, so the log line's role is the claim's: the role's own token names a
// claim that a re-role moved to another role's step, and that claim answers for the role it holds.
// A named claim that is not the step's worker — another claim of the role, which two claims of one
// issue can hold — runs on no standing, since no step's move changes which claim the body names.
// Its phase's restart names it and is refused for a step that is another claim's, so the claim
// comes back owing what it owed and the answer says so.
func (s *server) agentDoor(w http.ResponseWriter, r *http.Request, op agentAction) (claimTarget, bool) {
	var req ClaimRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"role", req.Role}) {
		return claimTarget{}, false
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue must be a Dispatch issue key")
		return claimTarget{}, false
	}
	role := claim.Role(req.Role)
	if !claim.IsRole(role) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ROLE", fmt.Sprintf(
			"role must be one of %s; the project's controller is none of them, and %s",
			roleList(), op.invalidRole))
		return claimTarget{}, false
	}
	// The claim the body names is judged by its form here, with the body's other fields: that form
	// reads nothing of any claim. The record is read only below the controller's grant.
	if req.Claim != "" {
		_, agent, cut := claim.Token(req.Claim).Cut()
		if of, err := claim.AgentToken(s.project, req.Issue, agent); !cut || err != nil || of != claim.Token(req.Claim) {
			s.writeFailure(w, r, http.StatusBadRequest, "INVALID_CLAIM", fmt.Sprintf(
				"claim %s is no claim of %s: name the token of an agent of the issue, as a notice names it, or leave claim out to reach the claim that works the %s's step",
				req.Claim, req.Issue, role))
			return claimTarget{}, false
		}
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return claimTarget{}, false
	}
	if !grant.Controller {
		s.writeFailure(w, r, http.StatusForbidden, "CONTROLLER_REQUIRED", op.name+" requires a controller grant")
		return claimTarget{}, false
	}
	worker, err := s.workerToken(r.Context(), req.Issue, role)
	if err != nil {
		s.logFailure("api: build the claim token of "+op.name, "issue", req.Issue, "role", req.Role, "error", err)
		s.writeFailure(w, r, http.StatusInternalServerError, "CLAIM_TOKEN_FAILED", err.Error())
		return claimTarget{}, false
	}
	token := worker
	if req.Claim != "" {
		token = claim.Token(req.Claim)
	}
	by := supervise.SuspendedByController
	m, supervised := s.supervisor.Machine(token)
	if !supervised {
		s.writeFailure(w, r, http.StatusNotFound, "NO_CLAIM", "no claim "+string(token))
		return claimTarget{}, false
	}
	if req.Claim != "" {
		if holds := m.Claim().Role; holds != role {
			s.writeFailure(w, r, http.StatusConflict, "CLAIM_NOT_OF_ROLE", fmt.Sprintf(
				"claim %s holds the role %s of %s, not %s: name the role it holds", token, holds, req.Issue, role))
			return claimTarget{}, false
		}
	}
	if !op.controllerPassesHold {
		if err := m.View().HeldAgainst(by); err != nil {
			s.refuseHold(w, r, token, op, by, err, nothingChanged)
			return claimTarget{}, false
		}
	}
	request := r
	if token == worker {
		request = r.WithContext(supervise.Standing(r.Context(), s.worksStep(req.Issue, role, token)))
	}
	return claimTarget{machine: m, token: token, issue: req.Issue, role: role, by: by, request: request}, true
}

// roleList names the roles a claim can be on: Legion's six, in claim.Roles' order, and each role
// the deployment defines, which a claim of an issue's step is on.
func roleList() string {
	names := make([]string, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		names = append(names, string(role))
	}
	return strings.Join(names, ", ") + ", or a role of this deployment"
}
