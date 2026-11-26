package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// AgentActionResponse is what four of the agent door's six agent actions answer — the
// controller's stop, retry, suspend and resume (stopAgent, retryAgent, suspendAgent, resumeAgent):
// the claim's state after the request, read from its machine's published view as every operator
// request on an agent reads it (claimRequest).
//
// SuspendedBy and SuspensionHeldBy say who holds a suspension over the agent: `controller` for
// one the controller placed, which its own resume lifts, and `operator` for the operator's, which
// only the operator lifts (suspendedByOperator). These four routes are the one session-facing
// surface that names a suspender. The state does not, and must not: a session parses the state
// with the schema of its own plugin's contract, and a key added there refuses the whole state in
// every pod whose plugin predates the stranded-claim reading (StrandedClaim). Only a plugin at the
// contract that added these routes calls them, so no reader of this body predates its keys.
type AgentActionResponse struct {
	// State is the claim's state after the request: retired after a stop; suspended after a
	// suspension that landed, and still working or launching after one held for the agent's
	// turn or its ready (the 202 answer); launching after a resume, or a retry the claim's own
	// machine ran; and still failed or retired after a retry of a phase worker, whose start the
	// issue's phase queued and runs once that start comes (the 202 answer, retryStart).
	State string `json:"state"`
	// SuspendedBy is who suspended the agent while its claim is suspended — operator, controller,
	// daemon, exit, released or unknown (supervise.SuspendSource) — and absent otherwise.
	SuspendedBy string `json:"suspendedBy,omitempty"`
	// SuspensionHeld is a suspension held for the agent's turn or ready, and SuspensionHeldBy who
	// asked for it; both are absent when none is held.
	SuspensionHeld   bool   `json:"suspensionHeld,omitempty"`
	SuspensionHeldBy string `json:"suspensionHeldBy,omitempty"`
}

// suspendedByOperator is the refusal of a request the agent door's caller may not make past the
// operator's suspension (refuseHold), 409: a request of the controller's that would get past a
// suspension the operator holds, held for a turn or landed — its resume, which would lift it; its
// suspend, which would take it over or change nothing; its stop, which would retire the agent
// under it for a retry to relaunch; its retry; and its fresh session, which would resume it — so
// every action of the door but the compaction (supervise.ErrOperatorHolds,
// agentAction.controllerPassesHold). Only the operator undoes the operator's decision, so the
// sentence sends the controller to the operator. It says what the request did to the agent, which
// is nothing but where a fresh session's own suspension of the agent ran first or its forget left
// the agent's recorded session behind (nothingChanged, itsSuspensionTakenOver,
// sessionLeftBehind).
const suspendedByOperator = "SUSPENDED_BY_OPERATOR"

// What a refusal of a hold says the request did to the agent (refuseHold): nothing, for a request
// the hold refused before it did anything, which is every such refusal but two; its own
// suspension of the agent and nothing more, for the fresh session whose forget a suspension above
// the caller refused after taking the agent over from that suspension, after which the claim's
// state is suspended, by the holder, with the session it recorded; and the agent's session left
// behind and nothing more, for the fresh session of an agent it found suspended whose resume such
// a suspension refused after the forget had dropped that session, after which the claim's state
// is suspended, by the holder, with no session to resume (freshSession). Each reads after
// "so <operation> by <caller>", or after "so it" in the daemon's sentence.
const (
	nothingChanged         = "is refused and nothing of the claim changed"
	itsSuspensionTakenOver = "went no further than its own suspension of the agent, which that suspension then took over, and the agent keeps the session its claim recorded"
	sessionLeftBehind      = "went no further than leaving behind the session the claim recorded before that suspension took the agent over, so the agent starts in a new session once it is started"
)

// The four lifecycle actions on an agent in the words their refusals name them by (agentAction),
// with what the log line of each says it did (answerAgentAction) and, for the three that are a
// single event to the claim's machine, the request that event makes (agentEventAction). Each
// takes the name and the refusal of the role `controller` its door gives (agentDoor); the two
// that refuse a claim in a turn, the retry and the resume, say so and name the states they do
// reach (refuseClaimInATurn). The resume's is the machine's refusal, which refusesATurn answers
// as a claim in a turn; the retry's is retryAgent's own read of the claim's state, made before
// anything is asked of the machine, so the retry carries neither request nor refusesATurn. The
// turn's cut and the live states are the two recoveries' own (freshSessionOp, compactOp): a stop
// ends a turn by design, and a suspension is held for the turn's end.
var (
	stopOp = agentAction{
		name:        "a stop",
		invalidRole: "the controller does not stop its own agent",
		request:     "stop",
		did:         "stopped",
	}
	retryOp = agentAction{
		name:        "a retry",
		invalidRole: "the controller does not retry its own agent",
		did:         "retried",
		reaches:     "A retry reaches an agent that failed and one that a stop retired",
	}
	suspendOp = agentAction{
		name:        "a suspension",
		invalidRole: "the controller does not suspend its own agent",
		request:     "suspend",
		did:         "suspended",
	}
	resumeOp = agentAction{
		name:         "a resume",
		invalidRole:  "the controller does not resume its own agent",
		request:      "resume",
		did:          "resumed",
		refusesATurn: true,
		reaches:      "A resume reaches a suspended agent: the controller's every one but one the operator's own suspension holds",
	}
)

// callerName is who came through a door of the agent actions (claimTarget.by, and the
// operator's fresh session), in the words its refusals and its log lines name it by. Each source
// that comes through one has its own words, and any other is named as the source it is, so a
// sentence never gives one caller's request another's name.
func callerName(by supervise.SuspendSource) string {
	switch by {
	case supervise.SuspendedByController:
		return "the controller"
	case supervise.SuspendedByOperator:
		return "the operator"
	default:
		return fmt.Sprintf("a caller of source %q, which no door of the agent actions lets in", by)
	}
}

// stopAgent is the agent door's `legion claims stop` (stopEvent): the agent is stopped and
// retired, a turn it is in cut off with it. As the operator's stop of one agent does, it waits
// for the report of the take's turn where it reaches the agent after that turn ended and before
// the report came, and answers 202 (supervise.RequestStop.WaitForReport, holdStop). The one
// difference is a hold the controller may not get past, which its stop does not end (agentDoor,
// supervise.RequestStop.By, supervise.Claim.HeldAgainst): the operator's (SUSPENDED_BY_OPERATOR).
// A stop would retire the agent, and a retry would then relaunch it past that hold.
func (s *server) stopAgent(w http.ResponseWriter, r *http.Request) {
	s.agentEventAction(w, r, stopOp, func(_ http.ResponseWriter, _ *http.Request, target claimTarget) (supervise.Event, bool) {
		return supervise.RequestStop{Claim: target.token, By: target.by, WaitForReport: true}, true
	})
}

// suspendAgent is the agent door's `legion claims suspend` (suspendEvent): the agent's process is
// stopped and its session kept, and a suspension that reaches an agent in a turn, or one whose
// launch has not said ready, is held for that turn's end or that ready, bounded at the stop
// timeout as the operator's is, and answered 202. The suspension is recorded as the controller's
// (supervise.SuspendedByController): it holds against every rule as the operator's does, and the
// controller's own resume lifts it, as the operator's does. One the operator's suspension already
// holds is refused (SUSPENDED_BY_OPERATOR).
func (s *server) suspendAgent(w http.ResponseWriter, r *http.Request) {
	s.agentEventAction(w, r, suspendOp, func(_ http.ResponseWriter, _ *http.Request, target claimTarget) (supervise.Event, bool) {
		return supervise.RequestSuspend{Claim: target.token, Reason: callerName(target.by) + " suspended it", By: target.by}, true
	})
}

// resumeAgent is the agent door's `legion claims resume` (resumeEvent): the suspended agent starts
// again in the session its claim records. The controller's resume_agent lifts every suspension
// but the operator's own, which only the operator lifts (agentDoor, supervise.RequestResume.By,
// supervise.Claim.HeldAgainst). While the project is paused its event builder refuses it
// PROJECT_PAUSED, before the event is posted, and nothing of the claim changes
// (refusedWhilePaused), as the operator's resume is (resumeEvent): an agent the pause stopped is
// the pause's to bring back at the project's resume, and no agent action undoes the pause.
func (s *server) resumeAgent(w http.ResponseWriter, r *http.Request) {
	s.agentEventAction(w, r, resumeOp, func(w http.ResponseWriter, r *http.Request, target claimTarget) (supervise.Event, bool) {
		if s.refusedWhilePaused(w, r, "resume", target.machine.View().Claim) {
			return nil, false
		}
		return supervise.RequestResume{Claim: target.token, By: target.by}, true
	})
}

// retryAgent is the agent door's plain `legion claims retry` (retryStart), with no fresh
// session: an agent that failed, or that a stop retired, is started again in the session its
// claim recorded — a phase worker through its issue's phase, which hands it that phase's task,
// and an architect by its own machine. The store records no one as having stopped the agent
// (supervise.Machine.retire writes the claim's state alone), so an agent the operator stopped is
// retried here as one the controller stopped is. An agent the operator's suspension holds, landed
// or held for its turn, is refused by the door before anything is asked, as every action of the
// door but the compaction is (agentDoor). A claim stored working is refused as a claim in a turn
// (refuseClaimInATurn); every other refusal is the effect's own, in the words the operator's door
// gives it. While the project is paused it is refused PROJECT_PAUSED before the read of the
// claim's state, and nothing of the claim changes (refusedWhilePaused), as the operator's retry
// is.
func (s *server) retryAgent(w http.ResponseWriter, r *http.Request) {
	target, ok := s.agentDoor(w, r, retryOp)
	if !ok {
		return
	}
	if s.refusedWhilePaused(w, r, "retry", target.machine.View().Claim) {
		return
	}
	if target.machine.View().State == supervise.StateWorking {
		s.refuseClaimInATurn(w, r, target.token, retryOp)
		return
	}
	status, ok := s.retryStart(w, r, target.machine, target.token, false, target.by)
	if !ok {
		return
	}
	s.answerAgentAction(w, r, target, retryOp.did, status)
}

// agentEventAction is the agent door's one operator request a single event to the claim's
// machine makes (claimRequest), op.request: the agent is named by its issue and role and the
// caller authenticated as the project's controller (agentDoor), the event is built and posted,
// and the claim is answered as it then stands, the log line saying the caller op.did it. The
// event builder has the operator's door's shape: it answers the request itself and reports false
// where it refuses it, as the resume's does while the project is paused (resumeAgent,
// refusedWhilePaused), and nothing is posted then. A suspension the machine holds for the agent's
// turn is no refusal and answers 202, as the operator's does.
func (s *server) agentEventAction(w http.ResponseWriter, r *http.Request, op agentAction,
	event func(http.ResponseWriter, *http.Request, claimTarget) (supervise.Event, bool)) {
	target, ok := s.agentDoor(w, r, op)
	if !ok {
		return
	}
	ev, ok := event(w, r, target)
	if !ok {
		return
	}
	posted, err := s.postClaimEvent(w, r, target.machine, target.token, op.request, ev)
	if !posted {
		return
	}
	status := http.StatusOK
	if errors.Is(err, supervise.ErrSuspendHeld) {
		status = http.StatusAccepted
	} else if err != nil {
		s.refuseAgentAction(w, r, target, err, op)
		return
	}
	s.answerAgentAction(w, r, target, op.did, status)
}

// refuseAgentAction answers an event of the caller's that the claim's machine refused: a
// suspension the caller may not get past in that hold's own code (refuseHold), an agent whose
// claim is stored working, which the action does not reach, as a claim in a turn
// (agentAction.refusesATurn, refuseClaimInATurn), and every other refusal as the operator's door
// answers it, in the machine's own words (operationFailure).
func (s *server) refuseAgentAction(w http.ResponseWriter, r *http.Request, target claimTarget, err error, op agentAction) {
	var refused *supervise.RefusedError
	switch {
	case holdRefusal(err):
		s.refuseHold(w, r, target.token, op, target.by, err, nothingChanged)
	case op.refusesATurn && errors.As(err, &refused) && refused.State == supervise.StateWorking:
		s.refuseClaimInATurn(w, r, target.token, op)
	default:
		s.operationFailure(w, r, target.by, op.request, target.token, err)
	}
}

// holdRefusal is whether err is the machine's refusal of a request a suspension stands in the way
// of, which refuseHold answers: the operator's, which stands above the controller
// (supervise.Claim.HeldAgainst). It is read from holdSentences, whose list of those refusals is
// the only one; the token fills only the words, which this reads none of.
func holdRefusal(err error) bool {
	_, _, _, held := holdSentences("", err)
	return held
}

// refuseHold is the refusal of a request of the controller's (by) that a suspension it may not get
// past stands in the way of, err being the machine's word for which (holdRefusal): the operator's
// own (suspendedByOperator). The sentence names who lifts the suspension, in the words
// holdSentences gives it, and outcome says what the request did to the agent (nothingChanged,
// itsSuspensionTakenOver). An error it does not know is no hold it can name: it is logged and
// answered 500 in its own words, as the daemon's own failure, rather than given the words of a
// hold it may not be.
func (s *server) refuseHold(w http.ResponseWriter, r *http.Request, token claim.Token, op agentAction, by supervise.SuspendSource,
	err error, outcome string) {
	code, heldBy, lifts, held := holdSentences(token, err)
	if !held {
		s.logFailure("api: a refusal of a hold that no answer of the agent door decides", "claim", token, "error", err)
		s.refuse(w, r, http.StatusInternalServerError, fmt.Sprintf(
			"%s by %s of %s met a refusal this daemon has no answer for: %v", op.name, callerName(by), token, err))
		return
	}
	s.writeFailure(w, r, http.StatusConflict, code, fmt.Sprintf(
		"%s is %s, so %s by %s %s. %s", token, heldBy, op.name, callerName(by), outcome, lifts))
}

// holdSentences is how the refusals of a suspension a caller may not get past name it, from the
// machine's word for it: the code, what the claim is held by, in words that read after "<token>
// is", and the sentence naming what starts the agent again, the operator's resume. Each refusal
// of a hold says both (refuseHold), and so does each recovery's refusal of an agent such a hold
// stands over, where it would otherwise name a door that refuses the caller
// (refuseHeldSuspension, refuseNotRunning). held is whether err names such a hold at all, which
// is the one answer every reader takes to that question (holdRefusal among them): an error that
// names no hold, nil among them, has no words, and every string is empty.
func holdSentences(token claim.Token, err error) (code, heldBy, lifts string, held bool) {
	if !errors.Is(err, supervise.ErrOperatorHolds) {
		return "", "", "", false
	}
	return suspendedByOperator, "held by the operator's own suspension, which only the operator lifts",
		"Ask the operator: `legion claims resume --claim " + string(token) + "` is theirs to run", true
}

// refuseClaimInATurn is the retry's and the resume's refusal of a claim stored working
// (claimInATurn): neither reaches an agent in a turn, the machine refuses each for a claim that
// has not failed or is not suspended, and the code is the one both recoveries give a claim stored
// working, so the caller reads one code for it whichever action it asked. A claim stored
// working that a suspension above the caller holds, held for its turn, is no exception: the
// door refuses that claim in the hold's code before any action reads its state, so it too
// takes one code whichever action was asked (agentDoor). The sentence names the states the
// action reaches and sends the caller to the state (askAgainLater).
func (s *server) refuseClaimInATurn(w http.ResponseWriter, r *http.Request, token claim.Token, op agentAction) {
	s.writeFailure(w, r, http.StatusConflict, claimInATurn, fmt.Sprintf(
		"%s is stored working, and %s reaches no agent in a turn. %s. %s", token, op.name, op.reaches, askAgainLater))
}

// answerAgentAction writes the claim's state after the caller's action (AgentActionResponse) under
// status, as the answer the agent actions' row of the table names (AnswerAgentAction), and logs
// what the caller did to which agent, as the caller named it.
func (s *server) answerAgentAction(w http.ResponseWriter, r *http.Request, target claimTarget, did string, status int) {
	view := target.machine.View().Claim
	answer := AgentActionResponse{
		State:            string(view.State),
		SuspensionHeld:   view.SuspensionHeld,
		SuspensionHeldBy: string(view.SuspensionHeldBy),
	}
	if view.State == supervise.StateSuspended {
		answer.SuspendedBy = string(view.SuspendedBy)
	}
	s.log.Info("api: "+callerName(target.by)+" "+did+" an agent", "claim", target.token, "issue", target.issue, "role", target.role,
		"state", answer.State, "suspendedBy", answer.SuspendedBy, "suspensionHeldBy", answer.SuspensionHeldBy, "status", status)
	s.writeAnswer(w, r, AnswerAgentAction, status, answer)
}
