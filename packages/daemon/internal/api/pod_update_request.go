package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// PodUpdateRequest is the project's controller asking the daemon to replace the pod one issue of
// its project runs in (POST /legion/v1/issues/pod-update, issuePodUpdate): its grant and the
// issue. The round it opens is the one a drift opens (the daemon's requestPodUpdate): each role
// of the pod is asked to push its work and answer ready_for_update, and the pod is replaced once
// each has answered, no turn runs and no role with a process in the pod reports background work
// that runs or has sent no report since its last turn started (the daemon's roundEnd.holding), or
// at the round's bound. A suspension by the operator, the controller or an architect over a role
// the round asks keeps the pod past the bound until a resume lifts it (the daemon's heldClaims).
// The controller's own suspension counts too, so a controller that suspended a role and then asks
// for the replacement waits on itself, and the round comes back as update-stalled. It is the one
// way the controller removes an issue pod whose workspace may hold work.
type PodUpdateRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
}

// PodUpdateResponse is the round over the issue's pod after the request: its id
// (supervise.PodRound), the reasons it records — which its ask to each role states — and whether
// this request opened it. Opened is false where a round was open over the pod already, a drift's
// or an earlier request's, which the request leaves as it stands and answers instead.
type PodUpdateResponse struct {
	Round   string `json:"round"`
	Reasons string `json:"reasons"`
	Opened  bool   `json:"opened"`
}

// PodUpdateRefusal is what refused a request for a pod's update round, each answered with its
// own status and code (podUpdateRefusal).
type PodUpdateRefusal int

const (
	// PodUpdateNoClaim: this daemon supervises no claim of the issue that has not retired — an
	// issue it never ran, one whose every claim retired, or an issue of another project. 404
	// NO_CLAIM, the code the claim operations give a claim this daemon does not supervise, which is
	// the form another project's claim takes there (claimDoor).
	PodUpdateNoClaim PodUpdateRefusal = iota + 1
	// PodUpdateNoPod: the issue's claims run no worker in a pod a round could ask, in either of two
	// ways: no claim records a process in a pod (each is suspended or between launches, or the
	// runtime runs no pods), or the claims record a pod and none of them is a worker the round could
	// ask (none has a session, or each one that has one has failed), so no claim would record the
	// round. 404 NO_POD: the claims are there, and a worker in the pod the round is opened over is
	// what is missing.
	PodUpdateNoPod
	// PodUpdatePodChanging: the issue's pod is changing under the request — its claims record two
	// pods, a replacement in flight, or a round over the pod before this one is still ending — and
	// a sweep settles it once that replacement or that round ends. 409 POD_CHANGING.
	PodUpdatePodChanging
	// PodUpdateLimited: a limit of the project (`pod_update_max_open_rounds` or
	// `pod_update_rounds_per_minute`) holds the opening back, as it holds a drift's opening back
	// at a sweep. 409 POD_UPDATE_LIMITED, whose sentence names the limit.
	PodUpdateLimited
	// PodUpdateNotRecorded: the daemon posted the round, and no claim of the issue recorded it
	// within the request's bound — a claim's machine is held in a long decision, a relaunch, or
	// the claim stopped — so it is no round the sweeps carry yet, and none a restart would find.
	// 503 POD_UPDATE_NOT_RECORDED: the post may still land, and a later request answers the round
	// once a claim records it. A 200 is only ever a round a claim records.
	PodUpdateNotRecorded
	// PodUpdateBusy: a sweep's walk over the project's issues held the turn the request waits for
	// until the request's own context ended, or for the whole of the daemon's bound on that wait,
	// the bound of its wait for a claim's record of the round (30 seconds), so it read nothing and
	// opened nothing; its sentence names which of the two ended the wait, and the bound. 503
	// POD_UPDATE_BUSY, with a Retry-After of the sweep's interval (PodUpdateRefused.RetryAfter).
	PodUpdateBusy
	// PodUpdatePauseHeld: a project's pause holds the issue's pod (the daemon's podPauseHolds),
	// and no update round opens over a pod a pause holds,
	// whoever asks for it: the pause kept the pod and the work on its disk for the roles it
	// stopped. The hold ends when the project is resumed with a role of the pod live, or when a
	// role of the pod is ready again. 409 POD_HELD_BY_PAUSE. While the project is paused the route
	// answers PROJECT_PAUSED before it asks the daemon (issuePodUpdate), so this is a pause that
	// began after that read, or a hold the resume left on a pod none of whose roles is live yet.
	PodUpdatePauseHeld
)

// PodUpdateRefused is the daemon's refusal of a request for a pod's update round
// (Options.RequestPodUpdate): what refused it, the sentence the answer carries, which names the
// issue and, for a limit, the limit, and how long the caller should wait before it asks again,
// which the answer carries as its Retry-After in whole seconds, zero for none. No round is
// recorded on any claim.
type PodUpdateRefused struct {
	Refusal    PodUpdateRefusal
	Message    string
	RetryAfter time.Duration
}

func (r *PodUpdateRefused) Error() string { return r.Message }

// podUpdateRefusal is the status and code each refusal is answered with.
func podUpdateRefusal(refusal PodUpdateRefusal) (int, string) {
	switch refusal {
	case PodUpdateNoClaim:
		return http.StatusNotFound, "NO_CLAIM"
	case PodUpdateNoPod:
		return http.StatusNotFound, "NO_POD"
	case PodUpdatePodChanging:
		return http.StatusConflict, "POD_CHANGING"
	case PodUpdateLimited:
		return http.StatusConflict, "POD_UPDATE_LIMITED"
	case PodUpdateNotRecorded:
		return http.StatusServiceUnavailable, "POD_UPDATE_NOT_RECORDED"
	case PodUpdateBusy:
		return http.StatusServiceUnavailable, "POD_UPDATE_BUSY"
	case PodUpdatePauseHeld:
		return http.StatusConflict, "POD_HELD_BY_PAUSE"
	default:
		return http.StatusInternalServerError, "POD_UPDATE_FAILED"
	}
}

// issuePodUpdate is the controller's request for the replacement of one issue's pod
// (PodUpdateRequest). It takes the controller's grant alone, as issues/status does: an
// architect's or a phase worker's grant is refused `403 CONTROLLER_REQUIRED`. An issue of another
// project is refused `404 NO_CLAIM` before the daemon is asked anything, since this daemon runs no
// claim of it. While the project is paused it is refused `409 PROJECT_PAUSED`, with the pause
// that stands, before the daemon is asked anything (refusedForPause): a round would ask each role
// of the pod for a turn, which a paused project starts for no role but the controller, and then
// replace the pod the pause keeps for its stopped roles. Every other answer is the daemon's
// (Options.RequestPodUpdate): 200 with the round, opened by this request or already open, which a
// claim of the issue records in the store by then, or a PodUpdateRefused, with a Retry-After
// header where the refusal names a wait.
//
// The round's ask, its bound and the replacement are the sweep's, as for a round a drift opened:
// the ask reaches each role at the first sweep that reads the round recorded on its claim, and
// a round once open is carried to the replacement of its pod whatever the pod's drift says.
func (s *server) issuePodUpdate(w http.ResponseWriter, r *http.Request) {
	var req PodUpdateRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r,
		field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		s.writeFailure(w, r, http.StatusBadRequest, "INVALID_ISSUE", "issue must be a Dispatch issue key")
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		s.writeFailure(w, r, http.StatusForbidden, "CONTROLLER_REQUIRED", "a pod update requires a controller grant")
		return
	}
	if !s.ownDispatchIssue(req.Issue) {
		s.writeFailure(w, r, http.StatusNotFound, "NO_CLAIM", fmt.Sprintf(
			"%s is an issue of another project than this daemon's %s, so this daemon runs no claim and no pod of it",
			req.Issue, s.dispatchProject))
		return
	}
	if s.stopped(w, r, "the pod update of "+req.Issue) {
		return
	}
	if s.refusedForPause(w, r, "the pod update", req.Issue,
		"a paused project opens no update round, which would ask each role of the issue's pod for a turn and then replace the pod the pause keeps for the roles it stopped",
		"issue", req.Issue) {
		return
	}
	if s.requestPodUpdate == nil {
		s.writeFailure(w, r, http.StatusNotFound, "NO_POD", "this daemon runs no pod update rounds, so it replaces no pod")
		return
	}
	answer, err := s.requestPodUpdate(r.Context(), req.Issue)
	var refused *PodUpdateRefused
	switch {
	case errors.As(err, &refused):
		status, code := podUpdateRefusal(refused.Refusal)
		if refused.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(refused.RetryAfter.Seconds()))))
		}
		s.writeFailure(w, r, status, code, refused.Message)
		return
	case err != nil:
		s.logRequestFailure(r, err, "api: open the update round of an issue's pod", "issue", req.Issue)
		s.writeFailure(w, r, http.StatusInternalServerError, "POD_UPDATE_FAILED", err.Error())
		return
	}
	s.log.Info("api: "+callerName(supervise.SuspendedByController)+" asked for the replacement of an issue's pod",
		"issue", req.Issue, "round", answer.Round, "opened", answer.Opened, "reasons", answer.Reasons, "status", http.StatusOK)
	s.writeAnswer(w, r, AnswerPodUpdate, http.StatusOK, answer)
}
