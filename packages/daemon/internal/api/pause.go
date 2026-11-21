package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// PauseService is the project's pause (internal/pause's Service) as the routes use it: the four
// pause and resume routes act through it, and the claim routes read its flag to refuse a turn
// while the project is paused (refusedWhilePaused). A test hands the real *pause.Service over a
// database of its own.
type PauseService interface {
	Pause(ctx context.Context, by store.PauseActor, reason string) (store.Pause, error)
	Resume(ctx context.Context, by store.PauseActor) (store.Pause, []record.PauseHeldRow, error)
	OpenPause(ctx context.Context) (store.Pause, bool, error)
	LastEndedPause(ctx context.Context) (store.Pause, bool, error)
	Paused() (int64, bool)
}

// ProjectPauseRequest is the controller's POST /legion/v1/project/pause: its grant, and why the
// project is paused. It names no project: the daemon pauses its own.
type ProjectPauseRequest struct {
	GrantID string `json:"grantId"`
	Reason  string `json:"reason"`
}

// ProjectResumeRequest is the controller's POST /legion/v1/project/resume: its grant.
type ProjectResumeRequest struct {
	GrantID string `json:"grantId"`
}

// OperatorPauseRequest is the operator's POST /legion/v1/operator/project/pause: why the project
// is paused. The operator's resume takes no body.
type OperatorPauseRequest struct {
	Reason string `json:"reason"`
}

// PauseActorView is who paused the project or ended its pause (decision 19 of
// dispatch://INFRA-3759): `kind` is `controller`, with the session the controller acted from, or
// `operator`, with no session.
type PauseActorView struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
}

// PauseView is one pause of the project: its id, when it began, who paused and why, and, once a
// resume has ended it, when and by whom. The pause and resume routes answer it, the refusals that
// name a pause carry it, and the state's `lastPause` is the newest ended one.
type PauseView struct {
	ID      int64           `json:"id"`
	Since   time.Time       `json:"since"`
	By      PauseActorView  `json:"by"`
	Reason  string          `json:"reason"`
	EndedAt *time.Time      `json:"endedAt,omitempty"`
	EndedBy *PauseActorView `json:"endedBy,omitempty"`
}

// PausedView is the state's `paused` (decision 25): the open pause, the claims whose stop it still
// waits on (its open stop rows, store.OpenPauseStopsTx), and every row it holds, each with the
// claim it is for where it names one (record.Postgres.PauseHeldRows). A held row's reason is this
// pause, which is why the rows are listed under it. Both lists are never null.
type PausedView struct {
	ID      int64           `json:"id"`
	Since   time.Time       `json:"since"`
	By      PauseActorView  `json:"by"`
	Reason  string          `json:"reason"`
	WaitsOn []PauseStopView `json:"waitsOn"`
	Held    []PauseHeldView `json:"held"`
}

// PauseStopView is one claim whose stop the open pause still waits on, and when it was asked.
type PauseStopView struct {
	Claim   claim.Token `json:"claim"`
	Issue   string      `json:"issue"`
	Role    claim.Role  `json:"role"`
	AskedAt time.Time   `json:"askedAt"`
}

// PauseHeldView is one outbox row the open pause holds: its id, its kind, its issue, the claim it
// is for (absent for a row that names none, such as an architect's notice, which the tree walk
// routes at run time) and how long it has been due.
type PauseHeldView struct {
	ID            int64       `json:"id"`
	Kind          string      `json:"kind"`
	Issue         string      `json:"issue"`
	Claim         claim.Token `json:"claim,omitempty"`
	WaitedSeconds int64       `json:"waitedSeconds"`
}

// ProjectPauseResponse answers a pause: the pause it began.
type ProjectPauseResponse struct {
	Pause PauseView `json:"pause"`
}

// ProjectResumeResponse answers a resume: the pause it ended, and how many held rows it released
// to run, which the daemon's log names one by one (pause: a held row released).
type ProjectResumeResponse struct {
	Pause    PauseView `json:"pause"`
	Released int       `json:"released"`
}

// PauseFailure is the refusal of a pause, a resume, a turn while the project is paused, or a
// project release while a pause holds the project: the Failure every workflow route refuses with,
// and what the refusal names beside its sentence. A PAUSE_UNAVAILABLE names the setting that turns
// the pause off (absent for a daemon with no workflow, whose sentence says why); a
// PROJECT_ALREADY_PAUSED and a PROJECT_PAUSED name the pause that stands; a PROJECT_NOT_PAUSED
// names the project's last pause, where one has ended. The project release's PROJECT_PAUSED
// (releasePausedFailure) names the pause where one stands, and each pod a pause still holds
// (HeldPods). A PROJECT_RELEASING names nothing beside its sentence, which names the release that
// runs.
type PauseFailure struct {
	Failure
	Setting   string             `json:"setting,omitempty"`
	Pause     *PauseView         `json:"pause,omitempty"`
	LastPause *PauseView         `json:"lastPause,omitempty"`
	HeldPods  []PauseHeldPodView `json:"heldPods,omitempty"`
}

// PauseHeldPodView is one pod a pause still holds, as a refused project release names it
// (store.HeldPausePod): its issue, the pause that holds it and since when, and the claims of the
// pod stored suspended by `released`, the source a pause stops a role by, in token order and
// never null. An operator's `legion claims resume --claim <token>` of one of them runs a role of
// the pod again, and its ready ends the hold.
type PauseHeldPodView struct {
	Issue  string        `json:"issue"`
	Pause  int64         `json:"pause"`
	HeldAt time.Time     `json:"heldAt"`
	Claims []claim.Token `json:"claims"`
}

// projectPaused is the code of a request refused because the project is paused (decision 20).
const projectPaused = "PROJECT_PAUSED"

// projectPause is the controller's pause (decision 19): a controller grant, the controller
// session's or one the operator's bearer minted, pauses the daemon's project, and the pause is the
// operator's when the operator's bearer minted the grant (pauseActor). Its answer is a plugin's
// (AnswerProjectPause).
func (s *server) projectPause(w http.ResponseWriter, r *http.Request) {
	var req ProjectPauseRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}) {
		return
	}
	if by, ok := s.pauseActor(w, r, req.GrantID, "a pause"); ok {
		s.pauseProject(w, r, AnswerProjectPause, by, req.Reason)
	}
}

// projectResume is the controller's resume, which ends any pause of the project, the operator's
// included. Its answer is a plugin's (AnswerProjectResume).
func (s *server) projectResume(w http.ResponseWriter, r *http.Request) {
	var req ProjectResumeRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !s.requireFailureFields(w, r, field{"grantId", req.GrantID}) {
		return
	}
	if by, ok := s.pauseActor(w, r, req.GrantID, "a resume"); ok {
		s.resumeProject(w, r, AnswerProjectResume, by)
	}
}

// operatorProjectPause is `legion project pause`: the operator's bearer pauses the project. Its
// answer is the `legion` command's, which no plugin parses (answerUnparsed, writeUnparsedAnswer).
func (s *server) operatorProjectPause(w http.ResponseWriter, r *http.Request) {
	var req OperatorPauseRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	s.pauseProject(w, r, answerUnparsed, store.PauseActor{Kind: store.PauseByOperator}, req.Reason)
}

// operatorProjectResume is `legion project resume`: the operator's bearer ends the project's pause.
// It reads no body, and refuses one that names anything. Its answer is the `legion` command's, as
// operatorProjectPause's is.
func (s *server) operatorProjectResume(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !s.readOptionalBody(w, r, &req) {
		return
	}
	s.resumeProject(w, r, answerUnparsed, store.PauseActor{Kind: store.PauseByOperator})
}

// pauseActor redeems grantID for act and answers who acts through it: the controller, naming the
// session that minted the grant, or the operator, whose bearer minted it (credential.Grant's
// Session and Operator). A grant that is not a controller grant is refused CONTROLLER_REQUIRED.
func (s *server) pauseActor(w http.ResponseWriter, r *http.Request, grantID, act string) (store.PauseActor, bool) {
	grant, ok := s.redeem(w, r, grantID)
	if !ok {
		return store.PauseActor{}, false
	}
	if !grant.Controller {
		s.writeFailure(w, r, http.StatusForbidden, "CONTROLLER_REQUIRED", act+" of the project requires a controller grant")
		return store.PauseActor{}, false
	}
	if grant.Operator {
		return store.PauseActor{Kind: store.PauseByOperator}, true
	}
	return store.PauseActor{Kind: store.PauseByController, Session: grant.Session}, true
}

// pauseProject pauses the project for reason, by `by`, through the pause service, which refuses a
// blank reason, a configuration that cannot hold a pause, a second pause and a pause while a
// project release runs (PROJECT_RELEASING, dispatch://INFRA-3583 R13), each answered here in its
// code (decision 20). A refused pause writes nothing and stops no role. The service logs the pause
// and tells the controller when the operator paused (decisions 23 and 24). The pause it began is
// written as answer, the id of the route's own answer: AnswerProjectPause on the controller's
// route, and answerUnparsed on the operator's, which writeUnparsedAnswer writes its bodies under.
// Every refusal is AnswerRefusal.
func (s *server) pauseProject(w http.ResponseWriter, r *http.Request, answer AnswerID, by store.PauseActor, reason string) {
	if s.pause == nil {
		s.refuseWithoutPause(w, r, "pause")
		return
	}
	began, err := s.pause.Pause(r.Context(), by, reason)
	var (
		unavailable *pause.UnavailableError
		standing    *store.AlreadyPausedError
		releasing   *store.ProjectReleasingError
	)
	switch {
	case err == nil:
		s.writeAnswer(w, r, answer, http.StatusOK, ProjectPauseResponse{Pause: pauseViewOf(began)})
	case errors.Is(err, pause.ErrReasonRequired):
		s.writeFailure(w, r, http.StatusBadRequest, pause.CodeReasonRequired,
			"a pause needs a reason: say in reason why the project is paused; nothing was paused")
	case errors.As(err, &unavailable):
		s.writeAnswer(w, r, AnswerRefusal, http.StatusConflict, PauseFailure{
			Failure: Failure{Code: pause.CodeUnavailable, Error: unavailable.Error() + "; nothing was paused"},
			Setting: string(unavailable.Setting),
		})
	case errors.As(err, &standing):
		view := pauseViewOf(standing.Standing)
		s.writeAnswer(w, r, AnswerRefusal, http.StatusConflict, PauseFailure{
			Failure: Failure{Code: pause.CodeAlreadyPaused, Error: fmt.Sprintf(
				"the project is already paused (%s); this pause changed nothing", describePause(standing.Standing))},
			Pause: &view,
		})
	case errors.As(err, &releasing):
		release := releasing.Release
		s.log.Info("api: refused a pause: a project release runs", "release", release.ID, "by", string(by.Kind), "by_session", by.Session)
		s.writeFailure(w, r, http.StatusConflict, pause.CodeReleasing, fmt.Sprintf(
			"project release %d runs (started %s, bound %s): it reads each pod's unpushed work through a running role and asks those roles to push it, and a pause would stop them, so the release would delete their pods unread; nothing was paused and no role was stopped. Pause once the release has finished (legion project release reads it)",
			release.ID, release.StartedAt.UTC().Format(time.RFC3339), release.BoundAt.UTC().Format(time.RFC3339)))
	default:
		s.logRequestFailure(r, err, "api: the project's pause failed", "by", string(by.Kind), "by_session", by.Session)
		s.writeFailure(w, r, http.StatusInternalServerError, "PAUSE_FAILED", "the project's pause failed: "+err.Error())
	}
}

// resumeProject ends the project's pause, by `by`, through the pause service, which refuses a
// project that is not paused; that refusal names the project's last pause, where one has ended,
// so a caller that finds the pause already ended learns who ended it and when. A daemon that runs
// no workflow has no pause service and refuses a resume PAUSE_UNAVAILABLE, as it refuses a pause
// (refuseWithoutPause); otherwise a resume is never refused for the configuration
// (pause.Service.Resume). The pause it ended is written as answer, as pauseProject writes the
// pause it began.
func (s *server) resumeProject(w http.ResponseWriter, r *http.Request, answer AnswerID, by store.PauseActor) {
	if s.pause == nil {
		s.refuseWithoutPause(w, r, "resume")
		return
	}
	ended, released, err := s.pause.Resume(r.Context(), by)
	var notPaused *store.NotPausedError
	switch {
	case err == nil:
		s.writeAnswer(w, r, answer, http.StatusOK, ProjectResumeResponse{Pause: pauseViewOf(ended), Released: len(released)})
	case errors.As(err, &notPaused):
		body := PauseFailure{Failure: Failure{Code: pause.CodeNotPaused,
			Error: "the project is not paused, and no pause of it has ended; this resume changed nothing"}}
		last, found, err := s.pause.LastEndedPause(r.Context())
		switch {
		case err != nil:
			s.logRequestFailure(r, err, "api: read the project's last pause for a refused resume")
			body.Error = "the project is not paused; this resume changed nothing"
		case found:
			view := pauseViewOf(last)
			body.LastPause = &view
			body.Error = fmt.Sprintf("the project is not paused: its last pause (%s) ended %s by %s; this resume changed nothing",
				describePause(last), last.EndedAt.UTC().Format(time.RFC3339), describeActor(last.EndedBy))
		}
		s.writeAnswer(w, r, AnswerRefusal, http.StatusConflict, body)
	default:
		s.logRequestFailure(r, err, "api: the project's resume failed", "by", string(by.Kind), "by_session", by.Session)
		s.writeFailure(w, r, http.StatusInternalServerError, "RESUME_FAILED", "the project's resume failed: "+err.Error())
	}
}

// refuseWithoutPause is the pause or resume of a daemon that has no pause service: one with no
// workflow (the daemon's openWorkflow answers none without a dispatch_url), which has no outbox to
// stop the project's roles or to hold their work.
func (s *server) refuseWithoutPause(w http.ResponseWriter, r *http.Request, act string) {
	s.writeAnswer(w, r, AnswerRefusal, http.StatusConflict, PauseFailure{Failure: Failure{Code: pause.CodeUnavailable, Error: fmt.Sprintf(
		"this daemon cannot %s its project: it runs no workflow (legion.yaml names no dispatch_url), and a pause stops the project's roles and holds their work through the workflow's outbox; nothing changed",
		act)}})
}

// PauseStateTx is the state's `paused` and `lastPause` (decision 25), read through tx, the state's
// own snapshot: while the project is paused, its open pause with the claims whose stop it still
// waits on and every row it holds at now, and no last pause; while it runs, its newest ended
// pause, or neither while no pause has ended. One read answers which (store.OpenOrLastPauseTx).
// project is the Dispatch key.
func PauseStateTx(ctx context.Context, tx pgx.Tx, project string, now time.Time) (*PausedView, *PauseView, error) {
	p, found, err := store.OpenOrLastPauseTx(ctx, tx, project)
	if err != nil || !found {
		return nil, nil, err
	}
	if p.Ended() {
		view := pauseViewOf(p)
		return nil, &view, nil
	}
	stops, err := store.OpenPauseStopsTx(ctx, tx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	held, err := record.NewStore().PauseHeldRows(ctx, tx, project, now)
	if err != nil {
		return nil, nil, err
	}
	return pausedViewOf(p, stops, held), nil, nil
}

func pauseActorViewOf(a store.PauseActor) PauseActorView {
	return PauseActorView{Kind: string(a.Kind), Session: a.Session}
}

// pauseViewOf is p on the wire, every time in UTC, with when and by whom it ended once it has.
func pauseViewOf(p store.Pause) PauseView {
	view := PauseView{ID: p.ID, Since: p.Since.UTC(), By: pauseActorViewOf(p.By), Reason: p.Reason}
	if p.Ended() {
		endedAt, endedBy := p.EndedAt.UTC(), pauseActorViewOf(p.EndedBy)
		view.EndedAt, view.EndedBy = &endedAt, &endedBy
	}
	return view
}

// pausedViewOf is the open pause as the state shows it, with its open stops and the rows it holds.
func pausedViewOf(open store.Pause, stops []store.PauseStop, held []record.PauseHeldRow) *PausedView {
	view := &PausedView{ID: open.ID, Since: open.Since.UTC(), By: pauseActorViewOf(open.By), Reason: open.Reason,
		WaitsOn: make([]PauseStopView, len(stops)), Held: make([]PauseHeldView, len(held))}
	for i, stop := range stops {
		view.WaitsOn[i] = PauseStopView{Claim: stop.Claim, Issue: stop.Issue, Role: stop.Role, AskedAt: stop.AskedAt.UTC()}
	}
	for i, row := range held {
		view.Held[i] = PauseHeldView{ID: row.ID, Kind: string(row.Kind), Issue: row.Issue, Claim: row.Claim,
			WaitedSeconds: row.WaitedSeconds}
	}
	return view
}

// describePause is p as a refusal's sentence names it: its id, who paused, since when, and why.
func describePause(p store.Pause) string {
	return fmt.Sprintf("pause %d, by %s since %s: %s", p.ID, describeActor(p.By), p.Since.UTC().Format(time.RFC3339), p.Reason)
}

// describeActor is who acted on a pause, as a sentence names them.
func describeActor(a store.PauseActor) string {
	if a.Kind == store.PauseByController {
		return "the controller (session " + a.Session + ")"
	}
	return "the " + string(a.Kind)
}
