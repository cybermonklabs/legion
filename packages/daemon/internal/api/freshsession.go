package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ClaimRequest is what either of the controller's two claim recoveries names: the claim on Role
// of Issue, under the caller's controller grant. Role is a workflow role or `architect`; the
// project controller is no role here, so the controller's own claim cannot be named at all
// (claim.IsRole, whose six roles the controller is not one of). The fresh session and the
// compaction take the same three members, so they take one type (controllerClaim).
type ClaimRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Role    string `json:"role"`
}

// FreshSessionResponse confirms the claim left its session behind and its start ran: either the
// daemon relaunched it here, or the phase's restart it queued will. The controller is told
// nothing else, since what it does next it reads in the state.
type FreshSessionResponse struct {
	// PhaseNotRestarted is why nothing restarted the claim's phase: the claim is back in a new
	// session owing no task, and the workflow refused the restart that would have handed it
	// one. It is absent wherever the phase was restarted, and wherever none was due — a claim
	// that still owes a task, and an architect, work no phase start. The answer carries it
	// because the state records nowhere that the phase restart did not run, and that is the one
	// thing this answer knows which a reader of the claim cannot tell.
	PhaseNotRestarted string `json:"phaseNotRestarted,omitempty"`
}

// The refusals both recoveries write themselves, which all four doors give word for word
// (freshSession, compact).
const (
	// claimInATurn: cutting a running turn off would lose the turn that may be the one ending
	// the run of provider errors, so both recoveries refuse a claim in one (refuseInATurn). A
	// claim a suspension is held over takes this code as well: it is stored working, as a claim
	// in a turn is, and what differs is the wait, which its own sentence says
	// (refuseHeldSuspension).
	claimInATurn = "CLAIM_IN_A_TURN"
	// claimUnsettled: a claim between states has no settled agent for either recovery to act on
	// — one queued, one with a launch in flight, and one stored working under a launch the wake
	// pacing parks, which waits for that launch as much as the other two (refuseParkedRelaunch).
	claimUnsettled = "CLAIM_UNSETTLED"
)

// freshSessionReason is what the suspension of a live claim records as its cause.
const freshSessionReason = "a fresh session was asked for"

// controllerOp is one of the two claim recoveries in the words its own refusals name it by,
// which both of its doors give: name reads inside a sentence about the operation,
// notTheController is what the refusal of the role `controller` says the operation does not do
// to the project's own controller (controllerClaim), and cutsTheTurn is what the refusal of a
// claim in a turn says the operation would do to that turn (refuseInATurn).
type controllerOp struct {
	name             string
	notTheController string
	cutsTheTurn      string
}

// freshSessionOp is the fresh session in the words its own refusals name it by, which both of
// its doors give; the compaction's own is compact.go's (compactOp).
var freshSessionOp = controllerOp{
	name:             "a fresh session",
	notTheController: "its own claim takes no fresh session",
	cutsTheTurn:      "a fresh session would cut that turn off",
}

// controllerTarget is the claim a controller's recovery named, read and authenticated: the
// machine to run the effect on, and the issue and role the caller spelled, which the route logs
// as the controller named them.
type controllerTarget struct {
	machine *supervise.Machine
	token   claim.Token
	issue   string
	role    claim.Role
}

// controllerClaim is the door both of the controller's claim recoveries come through
// (claimFreshSession, claimCompact): the claim is named by its issue and role rather than by a
// token the controller never sees, and the caller is its project's controller rather than the
// operator. It answers every refusal of the body, the grant and the lookup itself, and reports
// false when it has; only the effect behind it differs between the two.
//
// The controller is refused its own claim here and nowhere else: the token is built from the
// issue and the role, and no issue's role spells the controller's token (claim.ControllerToken),
// so a role this check takes can never name it.
func (s *server) controllerClaim(w http.ResponseWriter, r *http.Request, op controllerOp) (controllerTarget, bool) {
	var req ClaimRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"role", req.Role}) {
		return controllerTarget{}, false
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue must be a Dispatch issue key")
		return controllerTarget{}, false
	}
	role := claim.Role(req.Role)
	if !claim.IsRole(role) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ROLE", fmt.Sprintf(
			"role must be one of %s; the project's controller is none of them, and %s",
			roleList(), op.notTheController))
		return controllerTarget{}, false
	}
	token, err := claim.NewToken(s.project, req.Issue, role)
	if err != nil {
		s.logFailure("api: build the claim token of "+op.name, "issue", req.Issue, "role", req.Role, "error", err)
		writeFailure(w, http.StatusInternalServerError, "CLAIM_TOKEN_FAILED", err.Error())
		return controllerTarget{}, false
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return controllerTarget{}, false
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", op.name+" requires a controller grant")
		return controllerTarget{}, false
	}
	m, supervised := s.supervisor.Machine(token)
	if !supervised {
		writeFailure(w, http.StatusNotFound, "NO_CLAIM", "no claim "+string(token))
		return controllerTarget{}, false
	}
	return controllerTarget{machine: m, token: token, issue: req.Issue, role: role}, true
}

// claimFreshSession is the controller's door onto the fresh session (freshSession). The effect
// is the operator's own, and so are its refusals; only the authentication differs
// (controllerClaim).
func (s *server) claimFreshSession(w http.ResponseWriter, r *http.Request) {
	target, ok := s.controllerClaim(w, r, freshSessionOp)
	if !ok {
		return
	}
	_, notRestarted, ok := s.freshSession(w, r, target.machine, target.token)
	if !ok {
		return
	}
	s.log.Info("api: the controller started a claim in a fresh session",
		"claim", target.token, "issue", target.issue, "role", target.role, "phaseNotRestarted", notRestarted)
	writeJSON(w, http.StatusOK, FreshSessionResponse{PhaseNotRestarted: notRestarted})
}

// roleList names the roles a claim can be on, in the order the workflow reaches them.
func roleList() string {
	names := make([]string, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		names = append(names, string(role))
	}
	return strings.Join(names, ", ")
}

// freshSession is the one fresh-session effect, which `legion claims retry --fresh-session`
// (retryClaim) and the controller's fresh_session (claimFreshSession) both run: the claim leaves
// the session it recorded behind and starts again in a new one. It answers every refusal itself
// and reports the status its caller's success answer takes, with the sentence that answer
// carries when nothing restarted the claim's phase (FreshSessionResponse.PhaseNotRestarted).
//
// It reaches every claim of the project that is not in a turn and whose launch is not in
// flight — one idle on the provider-error ladder, a suspended one, one an operator stopped, a
// failed one, and a root architect as much as a phase worker. The claim a run of provider
// errors names is live and idle between the ladder's sends, not stopped, so a recovery that
// reached only stopped and failed claims would miss the one the notice is sent for.
//
// It is three operator commands in a row, and nothing else: `legion claims suspend` for a claim
// that runs, then `legion claims forget-session`, then the start — `legion claims resume` for a
// suspended claim, and the retry for one that failed or an operator stopped, which a resume is
// refused for (supervise's table). A phase worker is started through its issue's phase whichever
// of those two started it, since that is what hands the relaunched role the phase's task
// (restartWorkedPhase). The new session also puts the claim's run of provider-error turns back
// to zero (supervise.Machine.forgetSession), so a second notice can come for the same claim.
//
// The forget goes before the start, and for a phase worker that means before intake.ApplyFact,
// because the start the fact queues is an outbox row a lane runs once the fact commits:
// ApplyFact runs its commit hooks on the committed fact, those hooks wake the outbox lane, and
// the lane starts the claim on a goroutine of its own (the daemon's outbox lanes). A forget
// sent after the fact races that lane. A lane that reaches the machine first relaunches the
// claim on the very session this request means to drop, and the forget then arrives at a
// launching claim, where the table ignores it (supervise's onForgetSession rows) and the
// machine answers a refusal: the caller reads an error and the phase runs on in the session the
// provider keeps refusing.
//
// What this order costs is the session id of a refused start, which the forget has dropped by
// then. The claim such a refusal leaves behind records no session, so its next accepted start
// spawns a fresh agent rather than resuming the one it had. Only the claim's record of the
// session goes: the session and its file stand, and this logs the id it dropped, so an operator
// who wanted that session can read it there. A forget that loses the race to the lane, by
// contrast, leaves no way at all to drop the session the request was for.
//
// The resume goes before the phase's restart, so the claim is back in its new session before
// anything the workflow may refuse is asked for. Only an operator's own suspension holds the
// start a restart queues (supervise's Claim.RuleHolds, which the outbox's start row and
// supervise.Machine.StartedBy both answer with a wait): a claim the daemon suspended, and one
// an idle release suspended, has no such hold. A restart run first on such a claim wakes the
// outbox lane on the committed fact, the lane's start row finds a claim the rule may resume and
// resumes it, and this route's own resume then meets a launching claim, which the table ignores
// and the machine answers a refusal for (supervise's onResume rows): the caller reads a 409 for
// a claim that is already relaunching. Run first, the resume has the claim launching before the
// fact commits, and the lane's rule resume finds a live claim with nothing left to do
// (supervise's onRuleResume rows).
//
// A restart the workflow refuses is therefore no refusal of the fresh session — not a fact the
// store would not apply, not the workflow's own refusal of it, not an issue the retry leaves
// held because its tree lingers. The claim is back in a new session whichever of them comes,
// and this answers success with the refusal named in it (FreshSessionResponse.PhaseNotRestarted,
// OperatorClaim.PhaseNotRestarted). A refusal here would be false twice over: it would tell the
// caller that nothing happened, for a worker this route had just relaunched, and for a ready or
// idle worker, which this route suspends itself as the operator, it would leave the worker
// stopped behind a suspension no rule of the daemon's lifts and the controller's own door
// cannot — the controller asks for a fresh session and a compaction, never a resume, so such a
// worker would stand until an operator resumed it.
//
// Two states and one claim are refused. A claim in a turn is refused because suspending it would
// cut that turn off, and the turn may be the one that ends the run; the caller is told to ask
// again after it, and so is a claim that entered a turn between the read below and the machine's
// lock, whose suspension the machine refuses rather than holds (suspendForFreshSession). A claim
// a suspension is held over takes that refusal's code and a sentence of its own, whatever it is
// stored working under — a turn, a compaction, or the process a parked launch replaces — and the
// switch below reads it ahead of both of the others: what such a claim waits for is the hold's
// end, and what that end leaves it is what its own refusal answers (refuseHeldSuspension).
//
// A claim between states — queued, with a launch in flight, or stored working under a launch
// the wake pacing parks — is refused because it has no settled agent to stop: its suspension
// would be held for the launch it is still waiting for, and the request would answer long after
// the claim moved on. The parked launch's turn comes by itself, so that claim is told to ask
// again once it has run (refuseParkedRelaunch). The switch reaches that case only with no
// suspension held, which is why the test for it reads the parked launch alone (relaunchWaits).
// The project's controller is refused whatever state it is in: its keeper starts it, as that
// keeper alone is told when it must (supervise's operatorRetry refuses the controller's retry for
// the same reason), and the controller's own door cannot name it at all, since `controller` is no
// role a claim token is built from (claimFreshSession).
func (s *server) freshSession(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) (status int, notRestarted string, ok bool) {
	c := m.Claim()
	if c.Role == claim.RoleController {
		// The keeper's resume is the rule's, which an operator's suspension holds against (the
		// daemon's controllerKeeper, supervise.Claim.RuleHolds), so the sentence names the
		// suspension that keeper does lift: the daemon's own.
		writeFailure(w, http.StatusConflict, "NOT_THE_CONTROLLER", fmt.Sprintf(
			"%s is the project's controller: the daemon's controller keeper is what starts it — it launches a queued controller, resumes one the daemon suspended, and retries one that failed or retired — so the controller is left to that keeper, whatever state it is in, and no fresh session is run on it here", token))
		return 0, "", false
	}
	view := m.View()
	switch {
	case c.State == supervise.StateWorking && view.SuspensionHeld:
		return 0, "", refuseHeldSuspension(w, token, freshSessionOp)
	case c.State == supervise.StateWorking && relaunchWaits(view):
		return 0, "", refuseParkedRelaunch(w, token, freshSessionOp)
	case c.State == supervise.StateWorking:
		return 0, "", refuseInATurn(w, token, freshSessionOp)
	case startsAgain(c.State):
	case agentRunning(c.State):
		if !s.suspendForFreshSession(w, r, m, token) {
			return 0, "", false
		}
	default:
		writeFailure(w, http.StatusConflict, claimUnsettled, fmt.Sprintf(
			"%s is %s: a fresh session reaches a claim whose agent is ready or idle, and one that is suspended, stopped or failed; ask again when %s is one of those",
			token, c.State, token))
		return 0, "", false
	}
	if session := m.Claim().Session; session != "" && !s.forgetRecordedSession(w, r, m, token, session) {
		return 0, "", false
	}
	if m.Claim().State != supervise.StateSuspended {
		status, ok := s.retryStart(w, r, m, token)
		return status, "", ok
	}
	if _, ok := s.claimEvent(w, r, m, token, "resume", supervise.RequestResume{Claim: token}); !ok {
		return 0, "", false
	}
	// A phase worker that owes no task is handed one by its issue's phase, through the same
	// restart the start of a stopped claim runs (restartWorkedPhase): the resume above starts
	// the agent and nothing more, so without it a tester waiting on CI, or a worker an idle
	// release suspended, comes back in a new session with nothing to do. A claim that still
	// owes a task keeps it — the resume sends that one — and an architect is never started
	// through a phase at all.
	status = http.StatusOK
	if m.Claim().Pending == nil {
		if phaseStatus, refusal, restarted := s.restartWorkedPhase(r, token, m.Claim()); restarted {
			status = phaseStatus
			if refusal != "" {
				status, notRestarted = http.StatusOK, fmt.Sprintf(
					"%s is back in a new session owing no task, and nothing restarted its phase: %s", token, refusal)
				s.log.Info("api: a fresh session started a claim whose phase was not restarted",
					"claim", token, "refusal", refusal)
			}
		}
	}
	s.log.Info("api: a fresh session was started for a claim that was suspended", "claim", token)
	return status, notRestarted, true
}

// refuseInATurn is the refusal of a claim whose turn is running, which both recoveries give:
// the operation names itself and what it would do to the turn, so a controller told to compact
// first is not told to ask for a fresh session instead (controllerOp). It always reports false,
// so a caller returns it as its own answer.
func refuseInATurn(w http.ResponseWriter, token claim.Token, op controllerOp) bool {
	writeFailure(w, http.StatusConflict, claimInATurn, fmt.Sprintf(
		"%s is in a turn: %s, so ask again after the turn ends", token, op.cutsTheTurn))
	return false
}

// refuseParkedRelaunch is the refusal of a claim stored working while a launch the daemon's
// wake pacing holds waits over it (relaunchWaits), which both recoveries give: such a claim is
// a claim between states with a launch in flight, which is what claimUnsettled says, and the
// operation names itself as the one that reaches the claim once that launch has had its turn
// (controllerOp). It always reports false, so a caller returns it as its own answer.
//
// The refusal names no session and no agent, because the claim may still have both: the park
// can hold over a process that is alive and in a turn of its own (supervise's repoint), and
// what every such claim has in common is the launch, not what it is running under it.
func refuseParkedRelaunch(w http.ResponseWriter, token claim.Token, op controllerOp) bool {
	writeFailure(w, http.StatusConflict, claimUnsettled, fmt.Sprintf(
		"%s has a relaunch in flight: the daemon is starting its process again, and that relaunch waits its turn in the wake pacing. The turn comes by itself; ask again once %s is running, which is when %s reaches it",
		token, token, op.name))
	return false
}

// refuseHeldSuspension is the refusal of a claim a suspension is held over (supervise's
// holdSuspension), which both recoveries give whatever the claim is stored working under: a
// turn, a compaction, or the process a launch the wake pacing parked will replace. Neither
// operation reaches a working claim, and both switches read the hold ahead of every other
// window a working claim can be in, because what such a claim waits for is the hold's end
// rather than a turn's or a compaction's.
//
// What that end leaves is not one state, and that is the whole of what this sentence may
// promise. The hold runs as a suspension at the end of the turn or the compaction (supervise's
// suspendHeld), at a parked launch's turn instead of launching (supervise's wakeNow, endHeld),
// and at the operator's deadline mid-turn (supervise's suspendAtTimeout). It is dropped unrun,
// the claim running on, when a start is run against a hold of the daemon's own — a newer
// workflow start, which only an operator's hold waits out (supervise's StartedBy, dropHeld) —
// and it goes with the process at every other end of that process, the close or the stop that
// retires the claim and the failure that fails it among them (supervise's letGo). So the
// landing is the operator's hold alone to promise: a start drops the daemon's, the turn then
// ends as any other turn does, and a caller told to ask again once the claim is suspended
// waits for a state that end never brings.
//
// The sentence therefore names the one thing every one of those ends shares — the claim stops
// working — and then splits by what it is once it has. A fresh session is what reaches the
// suspended, stopped or failed claim the ends that take the process leave (startsAgain), and
// the asking operation is what reaches the ready or idle one a dropped hold leaves, so the
// refusal names both (freshSessionName, controllerOp). The code is the turn's, because the
// claim is stored working exactly as a claim in a turn is (claimInATurn). It always reports
// false, so a caller returns it as its own answer.
func refuseHeldSuspension(w http.ResponseWriter, token claim.Token, op controllerOp) bool {
	writeFailure(w, http.StatusConflict, claimInATurn, fmt.Sprintf(
		"%s has a suspension held over it, and %s reaches no claim that is working. The hold ends by itself, and not always in a suspension: a start the daemon runs against the claim drops a hold of the daemon's own, and the end of the claim's process takes any hold with it. Ask again once %s is no longer working: %s reaches it if it is suspended, stopped or failed by then, and %s if it is ready or idle",
		token, op.name, token, freshSessionName, op.name))
	return false
}

// relaunchWaits is whether a launch the daemon's wake pacing holds waits over this claim
// (supervise.ClaimView.WakeWaiting, supervise.WakeLaunch). Read of a claim that is working and
// is not being compacted, it is the window a death under a compaction leaves when the pacing
// parks the relaunch: the claim is stored working over the process it lost, with nothing in a
// turn and nothing that will end one, and what it waits for is that launch's turn, which comes
// by itself (supervise's TestACompactionWhoseRelaunchWaitsItsTurnLeavesTheOwedTaskToThatTurn).
// Both recoveries read it, because both answer such a claim "in a turn" without it, and both
// read it after the held suspension, whose own wait outranks the launch's turn
// (refuseHeldSuspension): nothing is asked of the hold here.
//
// A turn that started under a launch parked over a process that is still alive reads the same
// here (supervise's repoint), and the refusal is as true of it: that launch replaces the
// process at its turn, so the claim is running again after it either way. Nothing the claim
// records tells the two apart — a park writes nothing of the claim, and the dead process is
// kept until the turn — so neither door says which of the two it found.
func relaunchWaits(view supervise.ClaimView) bool {
	return view.WakeWaiting != nil && view.WakeWaiting.Kind == supervise.WakeLaunch
}

// startsAgain is whether a claim's process is already gone, so the fresh session starts it rather
// than suspending it first: the states forgetSession and the retry both act on.
func startsAgain(state supervise.ClaimState) bool {
	return state == supervise.StateSuspended || failedOrRetired(state)
}

// agentRunning is whether a claim runs an agent that is in no turn: one registered, ready or
// idle. It is the set the fresh session suspends at once (suspendForFreshSession), where a claim
// whose launch has not registered yet is refused rather than waited for, because its suspension
// is one the machine would hold for the agent it is still waiting for (supervise's
// holdSuspendUntilReady) and this operation has none held. A compaction takes a narrower set,
// the two the machine itself takes one in (supervise's onCompact rows).
func agentRunning(state supervise.ClaimState) bool {
	return state == supervise.StateRegistered || state == supervise.StateReady || state == supervise.StateIdle
}

// suspendForFreshSession stops the agent of a claim freshSession read in no turn, and is the one
// request of either recovery the machine may not hold (supervise.RequestSuspend.NeverHold): a
// claim that entered a turn, or a launch, between that read and the machine's lock refuses it and
// is left exactly as the read found it. Held instead, it would stop the worker at the turn's end
// behind an answer that had told the caller nothing happened. A refusal from a turn is the one
// the read a moment earlier would have given (refuseInATurn), which is why this writes its own
// refusals rather than taking claimEvent's; every other one is the machine's own answer, in the
// words operatorFailure gives it. It reports false when it answered the caller.
func (s *server) suspendForFreshSession(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) bool {
	posted, err := s.postClaimEvent(w, r, m, token, "suspend", supervise.RequestSuspend{
		Claim: token, Reason: freshSessionReason, By: supervise.SuspendedByOperator, NeverHold: true,
	})
	var refused *supervise.RefusedError
	switch {
	case !posted:
		return false
	case errors.As(err, &refused) && refused.State == supervise.StateWorking:
		return refuseInATurn(w, token, freshSessionOp)
	case err != nil:
		s.operatorFailure(w, "suspend", token, err)
		return false
	}
	return true
}
