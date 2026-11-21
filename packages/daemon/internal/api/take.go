package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim's task reaches its worker as a value the worker takes, not as a prompt the daemon
// pushes (dispatch://INFRA-2811). The worker learns its claim's value from its relay, from its
// registration (claim.RegisterResponse.Task) and from POST /legion/v1/claims/task; it reports each
// value it is handed on POST /legion/v1/claims/task-seen, and once its host is idle asks to take
// the task the value names on POST /legion/v1/claims/take, whose answer is the claim's machine's
// (supervise's Machine.Take). The routes decide nothing about the claim themselves.
//
// Each takes either caller a turn outcome takes (claimCaller): an Oh My Pi session's pi-legion
// names the claim with the registration's secret, and the Claude bridge, which holds no secret,
// presents the launch's boot token.
//
// Every refusal of these routes is coded (Failure, taskRefusal). A worker sends a seen report or a
// take again on a 502, 503 or 504 that names no code — the answer of a proxy in front of a daemon
// that is not serving — and on nothing the daemon answers itself, so a refusal the daemon made must
// never read as an outage.
//
// None of them refuses a stale plugin with the stale-contract 409 (refuseIfStaleClaim). A take of
// a stale claim is answered refused with the hold `stale-plugin`, by the machine (supervise's
// claimHold), so a take always has an answer it can wait out; a read of the value and a seen
// report are answered as any claim's are, since neither starts anything, and a 409 there would
// leave a worker on an older contract nothing to do but fail its boot while the daemon relaunches
// it onto the current plugin.

// task is the worker reading its claim's value: the value the bucket holds now, null for none,
// and the number of the last task the claim finished (supervise's Machine.CurrentTask).
func (s *server) task(w http.ResponseWriter, r *http.Request) {
	var req claim.TaskRequest
	if !s.readTaskBody(w, r, &req) {
		return
	}
	caller, _ := req.Caller()
	token, m, ok := s.taskCaller(w, r, "task read", caller, req.Generation)
	if !ok {
		return
	}
	current, err := m.CurrentTask(r.Context())
	if err != nil {
		s.logRequestFailure(r, err, "api: read a claim's task value", "claim", token)
		s.writeFailure(w, r, http.StatusInternalServerError, "TASK_VALUE_UNAVAILABLE", "task read failed: the daemon could not read the task bucket")
		return
	}
	s.writeAnswer(w, r, AnswerTask, http.StatusOK, taskResponseOf(current))
}

// taskResponseOf is the claim's current task as the wire carries it.
func taskResponseOf(current supervise.CurrentTask) claim.TaskResponse {
	return claim.TaskResponse{Task: (*claim.TaskValue)(current.Value), Finished: current.Finished}
}

// taskSeen is the worker reporting the value it was handed at an epoch and revision, whether or
// not its host is idle (supervise's RequestTaskSeen). It answers 204.
func (s *server) taskSeen(w http.ResponseWriter, r *http.Request) {
	var req claim.TaskSeenRequest
	if !s.readTaskBody(w, r, &req) {
		return
	}
	caller, _ := req.Caller()
	token, m, ok := s.taskCaller(w, r, "seen report", caller, req.Generation)
	if !ok {
		return
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if err := m.Handle(ctx, supervise.RequestTaskSeen{
		Claim: token, Generation: req.Generation, Session: callerSession(caller),
		Number: req.Number, Revision: supervise.TaskRevision{Epoch: req.Epoch, Revision: req.Revision},
	}); err != nil {
		s.taskRefusal(w, r, "seen report", token, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// take is the worker asking to take the task its value names, because its host is idle. The
// answer is the machine's (supervise's Machine.Take): accepted with the text the worker runs as
// its turn's user message, refused naming the hold that stands, or dropped. The machine answers
// every take with an outcome or fails it with an error, and a take it failed with no answer is
// refused in the coded shape (taskRefusal). A decision that failed after its answer was made
// still answers it: the worker's take was answered, and the failure (logged by the machine) is
// the rest of the decision's.
func (s *server) take(w http.ResponseWriter, r *http.Request) {
	var req claim.TakeRequest
	if !s.readTaskBody(w, r, &req) {
		return
	}
	caller, _ := req.Caller()
	token, m, ok := s.taskCaller(w, r, "take", caller, req.Generation)
	if !ok {
		return
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	answer, err := m.Take(ctx, supervise.RequestTake{
		Claim: token, Generation: req.Generation, Session: callerSession(caller),
		Number: req.Number, Revision: supervise.TaskRevision{Epoch: req.Epoch, Revision: req.Revision},
	})
	if answer.Outcome == "" {
		s.taskRefusal(w, r, "take", token, err)
		return
	}
	s.writeAnswer(w, r, AnswerTake, http.StatusOK, claim.TakeResponse{Outcome: string(answer.Outcome), Text: answer.Text, Hold: answer.Hold})
}

// callerSession is the session a worker's request names, which the machine holds to the claim's
// (supervise's fenceTaskRequest): the agent form's own, and none for the boot form, whose token
// already names the launch.
func callerSession(caller claim.Caller) string {
	if agent, ok := caller.(claim.AgentCaller); ok {
		return agent.SessionID
	}
	return ""
}

// taskCaller is a task route's claim and its machine once its body is decoded: the generation is
// required, and the caller resolved (claimCaller), each refused in the coded shape (Failure).
func (s *server) taskCaller(w http.ResponseWriter, r *http.Request, request string, caller claim.Caller, generation uint64) (claim.Token, *supervise.Machine, bool) {
	if !s.requireTaskGeneration(w, r, generation) {
		return "", nil, false
	}
	token, m, refusal := s.claimCaller(r, request, caller, generation)
	if refusal != nil {
		s.writeFailure(w, r, refusal.status, refusal.code, refusal.message)
		return "", nil, false
	}
	return token, m, true
}

// readTaskBody decodes a task route's one JSON object strictly, refusing a member the route does
// not read, as readBody does, in the coded shape (Failure). It names no stale caller: these routes
// answer a stale claim as any other.
func (s *server) readTaskBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if _, err := decodeJSON(w, r, into, true); err != nil {
		s.log.Warn("api: refused an undecodable request body", "route", r.URL.Path, "error", err)
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_BODY", "invalid request body: "+err.Error())
		return false
	}
	return true
}

// requireTaskGeneration is requireGeneration in the coded shape (Failure).
func (s *server) requireTaskGeneration(w http.ResponseWriter, r *http.Request, generation uint64) bool {
	if generation == 0 {
		s.writeFailure(w, r, http.StatusBadRequest, "MISSING_FIELD", "generation is required")
		return false
	}
	return true
}

// taskRefusal is claimFailure in the coded shape (Failure): a claim refusal under its own status
// and sentence (refusalCode names it), a request the claim's state does not allow with 409 and the
// machine's reason, and anything else — the store refusing a write — with a 500 that says what
// failed and nothing the caller could use.
func (s *server) taskRefusal(w http.ResponseWriter, r *http.Request, request string, token claim.Token, err error) {
	var refusal claim.Refusal
	if errors.As(err, &refusal) {
		s.log.Info("api: refused a claim request", "request", request, "claim", token, "reason", err)
		s.writeFailure(w, r, refusal.Status, refusalCode(refusal), refusal.Message)
		return
	}
	var refused *supervise.RefusedError
	if errors.As(err, &refused) {
		s.writeFailure(w, r, http.StatusConflict, "CLAIM_STATE_REFUSED", refused.Error())
		return
	}
	s.logFailure("api: a claim request failed", "request", request, "claim", token, "error", err)
	s.writeFailure(w, r, http.StatusInternalServerError, "CLAIM_WRITE_FAILED", request+" failed: the daemon could not record it")
}
