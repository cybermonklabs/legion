package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ClaimRequest is what every operation of the claim door names (claimDoor): the claim on Role of
// Issue, under the caller's grant — the project's controller's, or an architect's for a claim of
// an issue it owns. Role is a workflow role or `architect`; the project controller is no role
// here, so the controller's own claim cannot be named at all (claim.IsRole, whose six roles the
// controller is not one of). The six operations take the same three members, so they take one
// type.
type ClaimRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
}

// claimOp is one of the six operations of the claim door in the words its own refusals name it
// by, which every door it has gives; each operation is described in this one place. name reads
// inside a sentence about the operation, and invalidRole is what the door's refusal of the role
// `controller` (INVALID_ROLE) says the operation does not do to the project's own controller,
// which no role names (claimDoor). request is the word for the request the stop, the
// suspension and the resume each make as a single event to the claim's machine, which the
// daemon's stop and the machine's refusals name it by (claimEventOperation, postClaimEvent,
// operationFailure), and did is what the log line of a stop, a retry, a suspension or a resume
// that ran says the caller did to the claim (answerClaimOperation). The retry starts the claim
// through retryStart, the start the operator's own retry route makes too, rather than one event,
// so it leaves request empty (claimRetry, retryStart); the two recoveries write their own
// answers and log lines, and leave both empty.
//
// refusesATurn is whether the machine's refusal of one of those three events in state working
// is answered as a claim in a turn (refuseClaimOperation, refuseClaimInATurn): the resume's
// alone is, since a resume reaches no claim in a turn, and reaches names the states it does
// reach. The stop's is not: the machine refuses a stop of a working claim only for a tree's
// root, and that is the root's own refusal, in the machine's words (operationFailure). Nor is the
// suspension's, which is held for a turn rather than refused for it. The retry reaches no claim
// in a turn either, and refuses one from claimRetry's own read of the claim's state before the
// machine is asked anything, so it leaves refusesATurn false and names its states in reaches.
// controllerPassesHold is whether the controller's request is let past a suspension that
// outranks it at the door rather than refused there (claimDoor, supervise.Claim.HeldAgainst):
// the compaction's alone, which stops, takes over and lifts no suspension, so a claim the
// operator's suspension holds is answered by the compaction's own refusals of the claim as it
// stands, which name the operator's resume as what starts it (refuseHeldSuspension,
// refuseState, holdSentences). An architect's request is refused such a suspension whatever the
// operation.
//
// cutsTheTurn is what the refusal of a claim stored working says the operation would do to a
// turn of that claim that is running (refuseInATurn), and reaches is the states the operation
// does reach, which every refusal of a claim's state that ends in askAgainLater gives as a
// sentence of its own: it is the one thing a refused caller can act on, since the claim's state
// is published and this says which door that state belongs to (askAgainLater).
//
// liveStates is the live half of reaches on its own — the states the operation acts on in a
// claim that runs an agent — for the one refusal that writes its own sentence around it, the
// held suspension's (refuseHeldSuspension). The two recoveries take different sets there, so
// each carries its own: a registered agent is one the fresh session suspends and the
// compaction has no session to ask yet (agentRunning, supervise's onCompact rows), and a
// sentence that named one set for both doors would be short by `registered` at one of them.
// TestEachDoorNamesTheStatesItsOwnPredicatesReach holds both fields to the predicates that
// decide them.
//
// No field says which operation a paused project refuses: each door that starts a turn refuses
// it itself, 409 PROJECT_PAUSED with the pause that stands, after the door and before anything
// is asked of the claim (refusedWhilePaused), as the operator's doors do. The resume refuses it in
// its own event builder (claimResume), as the operator's resume does (resumeEvent), the retry
// before its read of the claim's state (claimRetry), and the two recoveries in the paths they
// share with the operator's door (freshSession, compact). No claim operation undoes the pause,
// which the project's resume alone lifts. The stop and the suspension start nothing and are let
// through, as the operator's are.
type claimOp struct {
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

// claimTarget is the claim an operation of the claim door named, read and authenticated: the
// machine to run the effect on, the issue and role the caller spelled, which the route logs as
// the caller named them, and by, who the caller is — supervise.SuspendedByController for the
// project's controller and supervise.SuspendedByArchitect for an architect — which is the source
// its suspension records and what decides which suspensions refuse it (supervise.Claim.HeldAgainst,
// supervise.Claim.ResumeRefusal). recorded is the issue's record as the door read it for an
// architect's ownership (issueOfTree), which the two operations that start a stopped claim weigh
// with its tree's root for whether the issue still runs (refuseStartOfStoppedWork); the
// controller's door reads none, and leaves it the zero record.
type claimTarget struct {
	machine  *supervise.Machine
	token    claim.Token
	issue    string
	role     claim.Role
	by       supervise.SuspendSource
	recorded record.Issue
}

// ownClaim is the refusal of an architect's operation on its own claim: stopping, suspending,
// compacting or restarting the session that makes the call ends or cuts off the turn that would
// report it, so the architect's own recovery stays the controller's, the operator's and that of
// any architect above it in the tree, which owns its issue (owns).
const ownClaim = "OWN_CLAIM"

// claimDoor is the door all six claim operations come through — the two recoveries
// (claimFreshSession, claimCompact) and the four that stop, retry, suspend and resume a claim
// (claimStop, claimRetry, claimSuspend, claimResume): the claim is named by its issue and role
// rather than by a token the caller never sees, and the caller is its project's controller or an
// architect rather than the operator. It answers every refusal of the body, the grant and the
// lookup itself, and reports false when it has; only the effect behind it differs between the
// six.
//
// The controller reaches every claim of its project. An architect reaches the claims of the
// issues it owns alone, by the rule sign_off, retry_or_escalate and request_backward_move hold it
// to: the issue is checked against its grant (issueOfTree: 404 ISSUE_NOT_FOUND, 403
// ISSUE_OUTSIDE_TREE) and then against what the architect owns (architectOwns: 403
// ISSUE_NOT_OWNED) before the claim is looked up. A tree root's architect owns every issue of
// its tree, and a sub-architect its own issue and those under it, never the root or a sibling's
// (owns). A sub-architect holds a grant for the whole tree and can name any issue of it, so the
// tree alone would let it stop, suspend or restart the root's architect or a sibling subtree's
// workers. An architect's own claim is refused as well (OWN_CLAIM).
//
// A claim a suspension outranking the caller holds, held for a turn or landed, is refused here
// whoever the caller is and before the operation asks anything (supervise.Claim.HeldAgainst,
// refuseHold): the operator's for either caller (SUSPENDED_BY_OPERATOR), and the controller's
// for an architect (SUSPENDED_BY_CONTROLLER). So a held suspension and a landed one take one
// code at every operation, where the operation's own refusals would give a held one the code of
// the claim's state (refuseHeldSuspension, refuseClaimInATurn). A suspension that outranks the
// caller and lands after this read is still the machine's to refuse, and each operation that
// names its caller to the machine answers that refusal as the hold it is (refuseClaimOperation,
// freshSession). The compaction never meets that refusal: the machine's compaction carries no
// caller (supervise.RequestCompact), so a claim such a suspension holds is answered by the
// compaction's own refusals of its state, which name the holder's resume as what starts it
// (refuseState, holdSentences). The one request let past such a suspension at this door is the
// controller's compaction (claimOp.controllerPassesHold).
//
// The controller is refused its own claim here and nowhere else: the token is built from the
// issue and the role, and no issue's role spells the controller's token (claim.ControllerToken),
// so a role this check takes can never name it.
func (s *server) claimDoor(w http.ResponseWriter, r *http.Request, op claimOp) (claimTarget, bool) {
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
	token, err := claim.NewToken(s.project, req.Issue, role)
	if err != nil {
		s.logFailure("api: build the claim token of "+op.name, "issue", req.Issue, "role", req.Role, "error", err)
		s.writeFailure(w, r, http.StatusInternalServerError, "CLAIM_TOKEN_FAILED", err.Error())
		return claimTarget{}, false
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return claimTarget{}, false
	}
	var (
		by       supervise.SuspendSource
		recorded record.Issue
	)
	switch {
	case grant.Controller:
		by = supervise.SuspendedByController
	case grant.Role == claim.RoleArchitect:
		recorded, ok = s.issueOfTree(w, r, grant, req.Issue)
		if !ok || !s.architectOwns(w, r, grant, recorded, op.name) {
			return claimTarget{}, false
		}
		by = supervise.SuspendedByArchitect
	default:
		s.writeFailure(w, r, http.StatusForbidden, "CONTROLLER_REQUIRED",
			op.name+" requires a controller grant, or an architect grant for a claim of an issue it owns")
		return claimTarget{}, false
	}
	m, supervised := s.supervisor.Machine(token)
	if !supervised {
		s.writeFailure(w, r, http.StatusNotFound, "NO_CLAIM", "no claim "+string(token))
		return claimTarget{}, false
	}
	if by == supervise.SuspendedByArchitect && token == grant.Claim {
		s.writeFailure(w, r, http.StatusForbidden, ownClaim, fmt.Sprintf(
			"%s is the claim of the architect that asks, and %s of it would end or cut off the very turn that asks for it, so it is refused and nothing of the claim changed. An architect's own claim is the controller's, the operator's and any architect's above it in the tree to recover",
			token, op.name))
		return claimTarget{}, false
	}
	if by == supervise.SuspendedByArchitect || !op.controllerPassesHold {
		if err := m.View().HeldAgainst(by); err != nil {
			s.refuseHold(w, r, token, op, by, err, nothingChanged)
			return claimTarget{}, false
		}
	}
	return claimTarget{machine: m, token: token, issue: req.Issue, role: role, by: by, recorded: recorded}, true
}

// roleList names the roles a claim can be on, in the order the workflow reaches them.
func roleList() string {
	names := make([]string, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		names = append(names, string(role))
	}
	return strings.Join(names, ", ")
}
