package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/timewords"
)

// RelaunchRequest is the controller's `relaunch` of a claim Envoy cannot reach (R5): the claim
// named by its token, as `legion claims list` and the daemon's unreachable notices name it
// (record.Notice.Claim), under the controller's grant.
type RelaunchRequest struct {
	GrantID string `json:"grantId"`
	Claim   string `json:"claim"`
}

// RelaunchResponse confirms the relaunch: the claim's process was stopped and its same session is
// being resumed, now or, where the daemon's store did not answer the launch, by the daemon itself
// at the probe interval. It says nothing else, since what the claim does next the controller reads
// in the state.
type RelaunchResponse struct{}

// casePerson ends the sentence of each refusal only a person can resolve: the controller asks one
// with a Dispatch ask, who can have the pod replaced or the old process ended on its node.
const casePerson = " This is a case for a person."

// claimRelaunch is the controller's door onto the relaunch of a claim Envoy cannot reach, on its
// same session (R5). The daemon checks what only it knows — the claim is marked out of reach, its
// last relaunch is 6 hours old, and its pod holds a claim Envoy can still reach — and the machine
// checks the guard, asks the runtime and rechecks the tree lifecycle before it stops anything
// (Options.Relaunch, supervise.RequestRelaunch). Every refusal is answered with the code the
// controller acts on: the guard's refusal and a claim not out of reach are tried again later, a
// claim whose tree allows it no launch is its tree's close's or its next admission's to decide,
// and the five a relaunch cannot get past — the launcher not connected, a launch that would replace
// the pod, a process that outlived its stop, a pod whose every claim is out of reach, and a
// relaunch in the last 6 hours — say they are a case for a person. A relaunch whose launch waits
// for the daemon's store after the stop is one that ran: the machine answers it as made, and the
// daemon makes the launch itself (supervise's launchOrWait).
//
// The project's controller is refused: nobody else can relaunch it. The daemon relaunches it
// itself, once in 6 hours, and only where it runs the controller itself (`controller: daemon` in
// legion.yaml; internal/daemon's reach keeper, reachWatched).
func (s *server) claimRelaunch(w http.ResponseWriter, r *http.Request) {
	var req RelaunchRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w,
		field{"grantId", req.GrantID}, field{"claim", req.Claim}) {
		return
	}
	token := claim.Token(req.Claim)
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "relaunch requires a controller grant")
		return
	}
	if token == claim.ControllerToken(s.project) {
		writeFailure(w, http.StatusConflict, "CLAIM_IS_CONTROLLER",
			"the project's controller is relaunched by the daemon alone, once in "+timewords.Words(supervise.RelaunchAgainAfter)+
				", and only where the daemon runs the controller itself (controller: daemon in legion.yaml); nobody else can relaunch it")
		return
	}
	m, supervised := s.supervisor.Machine(token)
	if !supervised {
		writeFailure(w, http.StatusNotFound, "NO_CLAIM", "no claim "+string(token))
		return
	}
	if s.relaunch == nil {
		writeFailure(w, http.StatusInternalServerError, "RELAUNCH_FAILED", "this daemon has no relaunch of an unreachable claim")
		return
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, "relaunch") {
		return
	}
	err := s.relaunch(ctx, m)
	if err == nil {
		s.log.Warn("api: the controller relaunched a claim Envoy could not reach, on its same session", "claim", token)
		writeJSON(w, http.StatusOK, RelaunchResponse{})
		return
	}
	status, code, person := relaunchRefusal(err)
	sentence := err.Error()
	if person {
		sentence += "." + casePerson
	}
	if status == http.StatusInternalServerError {
		s.logFailure("api: the relaunch of an unreachable claim failed", "claim", token, "error", err)
	}
	writeFailure(w, status, code, sentence)
}

// relaunchRefusal is the status and code a relaunch's error is answered with, and whether it is a
// case for a person. A refusal whose record the daemon could not write is a failure first
// (supervise.ErrReachUnwritten), whatever refused it: the notice the controller would wait for
// counts from that record, so it must not read the answer as the refusal alone.
func relaunchRefusal(err error) (status int, code string, person bool) {
	var refused *supervise.RefusedError
	switch {
	case errors.Is(err, supervise.ErrReachUnwritten):
		return http.StatusInternalServerError, "RELAUNCH_FAILED", false
	case errors.Is(err, supervise.ErrClaimReachable):
		return http.StatusConflict, "CLAIM_REACHABLE", false
	case errors.Is(err, supervise.ErrRelaunchGuard):
		return http.StatusConflict, "RELAUNCH_GUARD", false
	case errors.Is(err, supervise.ErrRelaunchedRecently):
		return http.StatusConflict, "RELAUNCHED_RECENTLY", true
	case errors.Is(err, supervise.ErrPodUnreachable):
		return http.StatusConflict, "POD_UNREACHABLE", true
	case errors.Is(err, runtime.ErrLauncherNotConnected):
		return http.StatusConflict, "LAUNCHER_NOT_CONNECTED", true
	case errors.Is(err, runtime.ErrRelaunchReplacesPod):
		return http.StatusConflict, "RELAUNCH_REPLACES_POD", true
	case errors.Is(err, runtime.ErrProcessOutlivedStop):
		return http.StatusConflict, "PROCESS_OUTLIVED_STOP", true
	case errors.Is(err, supervise.ErrTreeRefusesLaunch):
		return http.StatusConflict, "TREE_REFUSES_LAUNCH", false
	case errors.As(err, &refused):
		return http.StatusConflict, claimUnsettled, false
	}
	return http.StatusInternalServerError, "RELAUNCH_FAILED", false
}
