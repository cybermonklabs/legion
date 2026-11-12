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

// FreshSessionResponse confirms the claim left the session it recorded behind: the daemon
// started it again here, or the phase's restart it queued will, and where neither of those ran
// the sentence in PhaseNotRestarted says so. The controller is told nothing else, since what it
// does next it reads in the state.
type FreshSessionResponse struct {
	// PhaseNotRestarted is what this request did to the claim that no reader of the claim can
	// tell, which is one of two things and never both (freshSession):
	//
	//   - the claim is back in a new session owing no task, and the workflow refused the
	//     restart that would have handed it one (restartWorkedPhase);
	//   - this request stopped the claim and left the session it recorded behind, and the
	//     machine refused the start that would have brought it back.
	//
	// It is absent wherever the claim came back started and holding whatever it owes, and
	// wherever no phase restart was due — a claim that still owes a task, and an architect,
	// work no phase start. The answer carries it because the state records neither: a reader of
	// the claim sees a claim in a new session, or a suspended one, and nothing of what this
	// request asked for and did not get.
	PhaseNotRestarted string `json:"phaseNotRestarted,omitempty"`
}

// The refusals both recoveries write themselves, which all four doors give word for word
// (freshSession, compact).
const (
	// claimInATurn: cutting a running turn off would lose the turn that may be the one ending
	// the run of provider errors, so both recoveries refuse a claim stored working whose
	// process the read at the request finds running, and one whose process that read could not
	// reach at all, which is as much as either door knows of it (refuseWorkingClaim,
	// refuseInATurn, refuseUnreadableProcess). A claim stored working that a suspension is held
	// over takes this code as well: it is stored working, as a claim in a turn is, and what
	// differs is the sentence (refuseHeldSuspension). The claim stored working that read finds
	// no process behind takes it at neither door: the fresh session starts such a claim again,
	// and the compaction names that door in the code of a claim with no session to compact
	// (freshSession, claimNotRunning, refuseState).
	claimInATurn = "CLAIM_IN_A_TURN"
	// claimUnsettled: a claim between states has no settled agent for either recovery to act on
	// — one queued, one with a launch in flight, and one stored working under a launch the wake
	// pacing parks, which has a launch in flight as much as the other two (refuseParkedRelaunch).
	// The compaction gives it for a registered claim as well, which runs no session to ask yet
	// where the fresh session suspends it and starts it again (agentRunning, supervise's
	// onCompact rows).
	// A claim a suspension is held over that is not stored working takes it too: the hold is
	// held for a launch that has not reached ready and the claim has no settled agent either,
	// which is the code such a claim was refused with before the hold was read at all
	// (refuseHeldSuspension).
	claimUnsettled = "CLAIM_UNSETTLED"
)

// askAgainLater is how the refusals that send the caller back end: a claim in a turn, a claim
// whose process the runtime would not answer for, a claim between states, and a claim a
// suspension is held over. The two COMPACTION_FAILED answers whose claim was not given back
// end this way too (compact). The ones that send the caller to another operation instead do
// not: a claim that runs nothing — one that is stopped, and one stored working that the
// compaction's read finds no process behind — and the project's own controller are named what
// starts them again (claimNotRunning, NOT_THE_CONTROLLER, startsAgainName), and a Claude Code
// claim is named the fresh session (claimNotOnOmp). A refusal says what the claim is doing
// when it is given and what the caller may do, and nothing of what the next report, turn, hold
// or relaunch will make of it: each of those ends is the machine's to decide when it comes, and
// a sentence that named one of them sent callers back for states that never arrived. So the
// caller is sent to the list the state is published on rather than to a moment, in both the
// forms its two callers have: the controller reads `read_state`, the one of the two it holds a
// grant for, and the operator runs `legion claims list` (freshSessionName spells its own
// operation both ways for the same reason).
const askAgainLater = "Ask again later; `read_state` (`legion claims list`) shows its state"

// freshSessionReason is what the suspension of a live claim records as its cause.
const freshSessionReason = "a fresh session was asked for"

// controllerOp is one of the two claim recoveries in the words its own refusals name it by,
// which both of its doors give: name reads inside a sentence about the operation,
// notTheController is what the refusal of the role `controller` says the operation does not do
// to the project's own controller (controllerClaim), cutsTheTurn is what the refusal of a
// claim stored working says the operation would do to a turn of that claim that is running
// (refuseInATurn), and reaches is the states the operation does reach, which every refusal of a
// claim's state that ends in askAgainLater gives as a sentence of its own: it is the one thing
// a refused caller can act on, since the claim's state is published and this says which door
// that state belongs to (askAgainLater).
//
// liveStates is the live half of reaches on its own — the states the operation acts on in a
// claim that runs an agent — for the one refusal that writes its own sentence around it, the
// held suspension's (refuseHeldSuspension). The two operations take different sets there, so
// each door carries its own: a registered agent is one the fresh session suspends and the
// compaction has no session to ask yet (agentRunning, supervise's onCompact rows), and a
// sentence that named one set for both doors would be short by `registered` at one of them.
// TestEachDoorNamesTheStatesItsOwnPredicatesReach holds both fields to the predicates that
// decide them.
type controllerOp struct {
	name             string
	notTheController string
	cutsTheTurn      string
	reaches          string
	liveStates       string
}

// freshSessionOp is the fresh session in the words its own refusals name it by, which both of
// its doors give; the compaction's own is compact.go's (compactOp). The states reaches names are
// the three sets the operation acts on, in the words `legion claims list` prints them: the agent
// it suspends first (agentRunning), a registered one among them, the stopped claim it starts
// where it stands (startsAgain), which is retired where an operator stopped it, and the claim
// stored working that the read of its process at the request finds nothing behind
// (workingOverADeadProcess). That last one is named by what the read found and not by the state
// alone, because the state alone is true of a claim in a turn as well, which the operation does
// not reach (refuseWorkingClaim). liveStates is the first of those three sets alone
// (agentRunning).
var freshSessionOp = controllerOp{
	name:             "a fresh session",
	notTheController: "its own claim takes no fresh session",
	cutsTheTurn:      "a fresh session would cut that turn off",
	reaches:          "A fresh session reaches a claim whose agent is registered, ready or idle, one that is suspended, failed or retired, and one stored working that no process stands behind",
	liveStates:       "registered, ready or idle",
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
// carries when the claim came back short of what this operation promises
// (FreshSessionResponse.PhaseNotRestarted).
//
// It reaches a claim whose agent is registered, ready or idle (agentRunning), one that is
// suspended, failed or retired (startsAgain), and one stored working that the read of its
// process at the request finds nothing behind (workingOverADeadProcess) — one idle on the
// provider-error ladder, a suspended one, one an operator stopped, which retires it, a failed
// one, a dead architect whose project controller has no other recovery for it, and a root
// architect as much as a phase worker. The claim a run of provider errors names is live and
// idle between the ladder's sends, not stopped, so a recovery that reached only stopped and
// failed claims would miss the one the notice is sent for. Every other state is refused below:
// a claim stored working whose process that read finds running or cannot read at all, one
// between states — queued, launch_uncertain, launching or shim_connected — one a suspension is
// held over, whatever state that is, and the project's own controller in every state of it.
//
// It is three operator commands in a row, and nothing else: `legion claims suspend` for a claim
// that runs an agent and for the one no process stands behind, then `legion claims
// forget-session`, then the start — `legion claims resume` for a suspended claim, and the retry
// for one that failed or an operator stopped, which a resume is refused for (supervise's table).
// A phase worker is started through its issue's phase whichever of those two started it, since
// that is what hands the relaunched role the phase's task (restartWorkedPhase). The new session
// also puts the claim's run of provider-error turns back to zero
// (supervise.Machine.forgetSession), so a second notice can come for the same claim.
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
// caller that nothing happened, for a worker this route had just relaunched, and for a
// registered, ready or idle worker, which this route suspends itself as the operator
// (agentRunning), it would leave the worker
// stopped behind a suspension no rule of the daemon's lifts and the controller's own door
// cannot — the controller asks for a fresh session and a compaction, never a resume, so such a
// worker would stand until an operator resumed it.
//
// A start the machine refuses after this route has stopped the claim is answered the same way
// and for the same reason: the suspension ran, the session the claim recorded is gone, and a
// failure would tell the caller that nothing happened about a claim this request stopped and
// left suspended. So that refusal goes into the answer too, in the machine's own words
// (FreshSessionResponse.PhaseNotRestarted), and the caller reads a claim it can start again
// rather than an error that names none. A refusal met before anything was stopped stays the
// caller's answer: nothing of the claim changed, and a 200 would say something did.
//
// Two states, one hold and one claim are refused, and one of those states twice over. A claim
// stored working whose process the read at the request finds running is refused because
// suspending it would cut off the turn that process may be in, and that turn may be the one
// that ends the run; so is a claim that entered a turn between that read and the machine's
// lock, whose suspension the machine refuses rather than holds
// (suspendForFreshSession). A claim a suspension is held over has a refusal of its own, in
// whatever state the hold found it (refuseHeldSuspension): the hold is read before the state,
// because a start run against such a claim would be run against a claim that is being stopped.
// What this route would otherwise do to it is worse than a late answer: the suspension it runs
// as the operator lets the held request go with the process (supervise's letGo), so the
// operator's own suspend, answered 202 and still waiting, would be dropped unrun and the
// worker started again in a new session.
//
// A claim between states — queued, with a launch in flight, or stored working under a launch
// the wake pacing parks — is refused because it has no settled agent to stop: its suspension
// would be held for the launch it is still waiting for, and the request would answer long after
// the claim moved on. The refusal names that launch and stops there (refuseParkedRelaunch).
// The project's controller is refused whatever state it is in: its keeper starts it, as that
// keeper alone is told when it must (supervise's operatorRetry refuses the controller's retry
// for the same reason), and the controller's own door cannot name it at all, since `controller`
// is no role a claim token is built from (claimFreshSession).
//
// A claim stored working is in a turn, under a launch the wake pacing parks, being compacted,
// or over no process at all, and one order tells them apart at both doors
// (refuseWorkingClaim). The last of them is the one this operation exists for: the read made
// at the request finds no process, so there is no turn to cut off and no agent to stop, and
// the claim goes to the suspension below exactly as a registered, ready or idle one does
// (workingOverADeadProcess, suspendForFreshSession). The machine lands that suspension where
// it arrives rather than holding it for a turn that will never end (supervise's
// holdSuspension), and the forget and the start every other arm takes follow it. The read is
// what makes that claim reachable at all: the daemon's own report of the dead process is
// memory a restart forgets, and the record a restart leaves reads exactly as a claim in a turn
// does.
func (s *server) freshSession(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token) (status int, notRestarted string, ok bool) {
	c := m.Claim()
	if c.Role == claim.RoleController {
		// The keeper's resume is the rule's, which an operator's suspension holds against (the
		// daemon's controllerKeeper, supervise.Claim.RuleHolds), so the sentence names the
		// suspension that keeper does lift: the daemon's own. It is the one place the keeper is
		// named in full, and the compaction's refusals of the controller name it in the same
		// words (controllerKeeperName, startsAgainName).
		writeFailure(w, http.StatusConflict, "NOT_THE_CONTROLLER", fmt.Sprintf(
			"%s is the project's controller: %s is what starts it — it launches a queued controller, resumes one the daemon suspended, and retries one that failed or retired — so the controller is left to that keeper, whatever state it is in, and no fresh session is run on it here",
			token, controllerKeeperName))
		return 0, "", false
	}
	view := m.View()
	// stopped is whether the suspension below ran, which decides how a start the machine then
	// refuses is answered: by then this request has stopped the claim and dropped the session
	// it recorded, so that refusal is a sentence of the answer rather than the answer itself
	// (FreshSessionResponse.PhaseNotRestarted).
	stopped := false
	switch {
	case view.SuspensionHeld:
		return 0, "", refuseHeldSuspension(w, token, c.State, c.Role, freshSessionOp)
	case workingOverADeadProcess(c.State):
		// Every claim stored working comes here, and the read is what tells them apart: the
		// one no process stands behind falls through to the suspension below, as an agent that
		// runs does, and every other one has been answered already (refuseWorkingClaim).
		if s.refuseWorkingClaim(w, r, m, token, view, freshSessionOp) {
			return 0, "", false
		}
		fallthrough
	case agentRunning(c.State):
		if !s.suspendForFreshSession(w, r, m, token) {
			return 0, "", false
		}
		stopped = true
	case startsAgain(c.State):
	default:
		writeFailure(w, http.StatusConflict, claimUnsettled, fmt.Sprintf(
			"%s is %s. %s. %s", token, c.State, freshSessionOp.reaches, askAgainLater))
		return 0, "", false
	}
	if session := m.Claim().Session; session != "" && !s.forgetRecordedSession(w, r, m, token, session) {
		return 0, "", false
	}
	if m.Claim().State != supervise.StateSuspended {
		status, ok := s.retryStart(w, r, m, token)
		return status, "", ok
	}
	switch posted, err := s.postClaimEvent(w, r, m, token, "resume", supervise.RequestResume{Claim: token}); {
	case !posted:
		return 0, "", false
	case err != nil && stopped:
		// The claim is stopped and the session it recorded is dropped, so a failure would tell
		// the caller that nothing happened about a claim this request changed and left
		// suspended. The machine's own words for the refusal go in the answer instead, which is
		// why the resume is posted rather than run through claimEvent (postClaimEvent,
		// FreshSessionResponse.PhaseNotRestarted). The claim an operator or controller reads
		// after this is suspended and recording no session: a start of it spawns a fresh agent,
		// which is what was asked for.
		s.log.Warn("api: a fresh session stopped the claim and the start that would have brought it back was refused",
			"claim", token, "error", err)
		return http.StatusOK, fmt.Sprintf(
			"%s was stopped and the session it recorded left behind, and nothing started it again: %s", token, err), true
	case err != nil:
		s.operatorFailure(w, "resume", token, err)
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

// refuseInATurn is the refusal of a claim stored working that the read of its process did not
// take out of the record's hands, which both recoveries give (refuseWorkingClaim): one whose
// process that read found running, one whose session is being compacted, which is not read at
// all, and one that records no process for a read to be made of (supervise.ProcessRuns,
// supervise.ProcessNotRead). The sentence says the claim is stored working and no more,
// because that is the whole of what the record holds of it, and for the claim nothing was
// read of it the record is the whole of what either door has. A turn is running under most of
// them; under the claim being compacted none is, and the compaction in flight owns what
// becomes of its agent either way (supervise.ClaimView.Compacting).
//
// What the operation would do to a turn is a fact about the operation, true whether one is
// running or not, and the sentence gives it as the condition it is. The operation names
// itself there, so a controller told to compact first is not told to ask for a fresh session
// instead, and then the states it does reach (controllerOp). It says nothing of the turn's
// end, which is the machine's to bring and not this answer's to promise (askAgainLater). It
// always reports false, so a caller returns it as its own answer.
func refuseInATurn(w http.ResponseWriter, token claim.Token, op controllerOp) bool {
	writeFailure(w, http.StatusConflict, claimInATurn, fmt.Sprintf(
		"%s is stored working: if a turn of it is running, %s. %s. %s", token, op.cutsTheTurn, op.reaches, askAgainLater))
	return false
}

// refuseWorkingClaim is the one read of a claim stored working, which both recoveries make in
// this order, and it reports whether it answered the caller (refuseState, freshSession).
//
// A launch the wake pacing parks is read first, because such a claim is nearest to that
// launch whatever its process is doing: the launch's turn lets that process go and starts the
// next, so what the process is doing now decides nothing (relaunchWaits, supervise's wakeNow).
//
// A claim whose session is being compacted is read next, and its process is not read at all:
// such a claim runs an agent, and the compaction in flight owns what becomes of it, so a read
// could answer nothing but "running" while a door that acted on any other answer would stop
// an agent mid-compaction. The compaction's own door answers such a claim before it ever
// reaches here, in its own words (refuseState), so this arm is the fresh session's.
//
// What is left is read at the runtime, now, and the answer is of the process itself rather
// than of the daemon's memory of a report a restart forgets (supervise.Machine.ReadProcess):
//
//   - a process that is gone is answered by neither door here: this reports that it answered
//     nothing, and each door acts on it in its own words — the fresh session stops the claim
//     and starts it again (freshSession, workingOverADeadProcess), and the compaction, which
//     has no agent to ask, names that door (refuseState);
//   - a runtime that would not say is no statement about the process, so neither door acts
//     and nothing of the claim changes (refuseUnreadableProcess);
//   - a process that is running is a claim in a turn, in the words every claim stored working
//     took before this read existed (refuseInATurn). So is a claim no read was made of, which
//     here is one recording no process at all (supervise.ProcessNotRead): the record is all
//     there is to answer from, and that is what that sentence says.
func (s *server) refuseWorkingClaim(w http.ResponseWriter, r *http.Request, m *supervise.Machine,
	token claim.Token, view supervise.ClaimView, op controllerOp) (answered bool) {
	switch {
	case relaunchWaits(view):
		refuseParkedRelaunch(w, token, op)
		return true
	case view.Compacting:
		refuseInATurn(w, token, op)
		return true
	}
	// The read is a decision of this claim, as every other request either recovery makes of the
	// machine is: it may write the claim, and a caller that hangs up mid-request must not cut
	// that write short or take the claim out of the set the daemon's drain keeps a connection
	// open for (server.decision, postClaimEvent). Nesting one inside another decision is the
	// compaction's case, and the set counts them (RouteDecisions.begin).
	ctx, decided := s.decision(r, token)
	defer decided()
	switch m.ReadProcess(ctx) {
	case supervise.ProcessGone:
		return false
	case supervise.ProcessUnanswered:
		refuseUnreadableProcess(w, token, op)
	default:
		refuseInATurn(w, token, op)
	}
	return true
}

// refuseParkedRelaunch is the refusal of a claim stored working while a launch the daemon's
// wake pacing holds waits over it (relaunchWaits), which both recoveries give: such a claim is
// a claim between states with a launch in flight, which is what claimUnsettled says, and the
// refusal names that launch and the states the operation does reach (controllerOp). What the
// launch's turn brings is not in it (askAgainLater).
//
// The refusal names no session and no agent, because the claim may still have both: the park
// can hold over a process that is alive and in a turn of its own (supervise's repoint), and
// what every such claim has in common is the launch, not what it is running under it.
func refuseParkedRelaunch(w http.ResponseWriter, token claim.Token, op controllerOp) {
	writeFailure(w, http.StatusConflict, claimUnsettled, fmt.Sprintf(
		"%s has a relaunch in flight: the daemon is starting its process again, and that relaunch waits its turn in the wake pacing. %s. %s",
		token, op.reaches, askAgainLater))
}

// refuseUnreadableProcess is the refusal of a claim stored working whose process the runtime
// would not answer for (supervise.ProcessUnanswered), which both recoveries give: a verdict of
// uncertain, which is a verdict of its own precisely so that it is never read as gone, and a
// probe that failed, which is the runtime not being askable and no statement about the process
// (runtime's Probe, Uncertain).
//
// Neither door may act on a claim it could not read: a fresh session that took an unread
// process for a dead one would stop a process that is running and cut its turn off. So the
// answer is the one every claim stored working takes, with the read that did not happen named
// in it, and the claim is left exactly as the request found it — nothing suspended, nothing
// forgotten and nothing started. What the caller does is ask again, once the runtime answers
// again (askAgainLater); the sentence names no moment for that, as no refusal here does.
//
// The code is the turn's, and truly so: a process nobody could read is a process that may be
// running, with a turn of the claim running on it (claimInATurn, refuseInATurn).
func refuseUnreadableProcess(w http.ResponseWriter, token claim.Token, op controllerOp) {
	writeFailure(w, http.StatusConflict, claimInATurn, fmt.Sprintf(
		"%s is stored working and the daemon could not read whether its process is running: the runtime did not answer. %s. %s",
		token, op.reaches, askAgainLater))
}

// refuseHeldSuspension is the refusal of a claim a suspension is held over (supervise's
// holdSuspension), which both recoveries give in whatever state the hold found the claim. A
// claim stored working is the commonest: it is working for a turn, for a compaction, or over
// the process a launch the wake pacing parked will replace. A hold is as live before the agent
// is ready, where it is held for a launch that has not registered and stands through
// shim_connected and registered until the agent's ready runs it (supervise's
// holdSuspendUntilReady, ready), and it stands over a ready or idle claim whose stop the
// runtime refused, waiting on the retry at the probe interval (supervise's suspendHeld). Both
// switches read the hold before the state, because what such a claim waits for is the hold's
// end rather than a turn's, a compaction's or a launch's.
//
// What that end leaves is not one state, and the sentence names none of it: whether a hold
// runs as a suspension, is dropped unrun with the claim running on, or goes with the claim
// altogether is the machine's own, decided where each of those is written (supervise's
// suspendHeld, dropHeld, letGo). The sentence says what the claim is now, and then which door
// reaches each state a caller can read of it afterwards (askAgainLater). What starts a
// suspended, failed or retired claim is the claim's role's (startsAgain, startsAgainName): the
// fresh session for a workflow role, and the daemon's controller keeper for the project's own
// controller, which only this operation's compaction reaches, since the fresh session refuses
// the controller at its door. The live states are the asking operation's own, so the refusal
// names each door's set rather than the pair both share: the fresh session's names the
// registered agent it suspends, and the compaction's does not, since the machine takes a
// compaction in two states and the fresh session suspends three (controllerOp.liveStates,
// agentRunning, supervise's onCompact rows).
//
// The second sentence is of the claims nothing is held over, and that scope is what keeps it
// from denying the first. A hold stands over a ready claim whose stop the runtime refused, and
// that claim is ready and held at once: "a compaction reaches it while it is ready", said of
// the claim in hand, would contradict the refusal it is written in. Both sentences are present
// fact — the hold is over this claim now, and each operation takes the states it takes now —
// and neither says the hold will end or what state would follow it, which is the machine's and
// no part of this answer (askAgainLater).
//
// The code follows the claim rather than the hold, so no caller's answer changes for a state
// that was refused before this refusal read the hold at all: a claim stored working takes the
// turn's code, as a claim in a turn does, and one that is not takes the code of a claim
// between states, which is what the unsettled default gave a claim still coming up
// (claimInATurn, claimUnsettled). It always reports false, so a caller returns it as its own
// answer.
func refuseHeldSuspension(w http.ResponseWriter, token claim.Token, state supervise.ClaimState, role claim.Role, op controllerOp) bool {
	code := claimUnsettled
	if state == supervise.StateWorking {
		code = claimInATurn
	}
	writeFailure(w, http.StatusConflict, code, fmt.Sprintf(
		"%s has a suspension held over it, and %s reaches no claim a suspension waits over, whatever state it is in. Of the claims with no suspension held over them, %s reaches those that are suspended, failed or retired, and %s reaches those whose agent is %s. %s",
		token, op.name, startsAgainName(role), op.name, op.liveStates, askAgainLater))
	return false
}

// relaunchWaits is whether a launch the daemon's wake pacing holds waits over this claim
// (supervise.ClaimView.WakeWaiting, supervise.StartsProcess). Read of a claim that is working,
// it is the window a death under a compaction leaves when the pacing parks the relaunch: the
// claim is stored working over the process it lost, with nothing in a turn and nothing that
// will end one, and the launch it waits for is the one the refusal names (supervise's
// TestACompactionWhoseRelaunchWaitsItsTurnLeavesTheOwedTaskToThatTurn). Both recoveries read
// it before anything else of a claim stored working, because the launch owns the process it
// waits over: its turn lets that process go and starts the next, so what a read of that
// process found would decide nothing (refuseWorkingClaim). The machine says the same from its
// own side — a read asked of such a claim is not made at all
// (supervise.Machine.ReadProcess, supervise.ProcessNotRead).
//
// It asks the supervise package whether the waiting wake starts a process rather than naming a
// kind, because what the refusal says is that a launch is in flight, and every lane that parks
// one in a machine says so: the ordinary launch and the relaunch the stale-plugin refresh lane
// raises, which is told apart on the wire for a reader counting that lane's work and nowhere
// else (supervise.WakePluginRefresh, dispatch://INFRA-2712 R4). A wake of a kind that starts no
// process would leave the claim to the two windows below, which is where such a claim belongs.
//
// A turn that started under a launch parked over a process that is still alive reads the same
// here (supervise's repoint), and the refusal is as true of it, since it says only that the
// launch is in flight. Nothing the claim records tells the two apart — a park writes nothing
// of the claim, and the dead process is kept until the turn — so neither door says which of
// the two it found.
func relaunchWaits(view supervise.ClaimView) bool {
	return view.WakeWaiting != nil && supervise.StartsProcess(view.WakeWaiting.Kind)
}

// startsAgain is whether a claim's process is already gone, so the fresh session starts it rather
// than suspending it first: suspended, failed and retired, the states the forget acts on bar the
// queued one (supervise's onForgetSession rows). What starts each of them differs — a resume for
// the suspended claim, the retry for a failed or retired one, which a resume is refused for
// (supervise's onResume and onOperatorRetry rows) — and freshSession picks between the two.
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

// workingOverADeadProcess is the state the fresh session reaches by reading the claim's
// process rather than by reading the record: working, and no other. The record alone never
// decides it, since a claim stored working is in a turn as often as not, so what the
// operation acts on is the read made at the request, and this names no more than the state
// that read is made of (refuseWorkingClaim, supervise.Machine.ReadProcess). It stands beside
// agentRunning and startsAgain because the sentence the operation gives of what it reaches is
// built from all three (freshSessionOp, TestEachDoorNamesTheStatesItsOwnPredicatesReach).
func workingOverADeadProcess(state supervise.ClaimState) bool {
	return state == supervise.StateWorking
}

// suspendForFreshSession stops the claim freshSession is about to start again in a new
// session: the agent it read in no turn, and the claim stored working that the read of its
// process found nothing behind, which has no agent to stop at all (refuseWorkingClaim,
// workingOverADeadProcess). It is the one request of either recovery the machine may not hold
// (supervise.RequestSuspend.NeverHold): a claim that entered a turn, or a launch, between that
// read and the machine's lock refuses it and is left exactly as the read found it. Held
// instead, it would stop the worker at the turn's end behind an answer that had told the
// caller nothing happened. The claim over the dead process is no such refusal: the machine
// lands its suspension where it arrives, since a hold protects a turn and a process and that
// claim has neither (supervise's holdSuspension, suspendOverADeadProcess). A refusal of a
// claim the machine found working is the one the read a moment earlier would have given
// (refuseInATurn), which is why this writes its own refusals rather than taking claimEvent's;
// every other one is the machine's own answer, in the words operatorFailure gives it. It
// reports false when it answered the caller.
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
