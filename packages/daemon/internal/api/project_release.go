package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ProjectReleaseRoute is the operator's route of the project release (dispatch://INFRA-3583 R6):
// `POST` starts it, or a dry run of it, and `GET` reads the newest release of the project.
const ProjectReleaseRoute = "/legion/v1/operator/project/release"

// ProjectReleaseRunsNoPod is the sentence a daemon on the tmux runtime refuses a project release
// with: the release moves Kubernetes pods, and that runtime runs none.
const ProjectReleaseRunsNoPod = "the tmux runtime runs no pod; a project release moves Kubernetes pods"

// ErrProjectReleaseRunsNoPod is ProjectRelease.Start's answer on a runtime that runs no pod, which
// the route answers 409 with ProjectReleaseRunsNoPod.
var ErrProjectReleaseRunsNoPod = errors.New(ProjectReleaseRunsNoPod)

// projectReleaseNoPod is the code of that refusal (Failure.Code).
const projectReleaseNoPod = "PROJECT_RELEASE_NO_POD"

// ProjectRelease is the daemon's project release as the routes reach it (internal/daemon's
// projectReleaser): one release per process, its record in the daemon's database, and the answers
// of the roles it asked.
type ProjectRelease interface {
	// Start starts the release with bound, or the daemon's pod_update_grace_minutes when bound
	// is zero, or runs a dry run of it. While a release of this process runs, or after it finished,
	// it starts nothing and answers that release with started false, a dry run's included, whether
	// or not a pause holds the project: no pause commits while that release runs. Otherwise a dry
	// run answers its report with started false. A runtime that runs no pod answers
	// ErrProjectReleaseRunsNoPod. A pause that holds the project — the project paused, or a pod a
	// pause stopped still held — answers every other start a *store.ProjectPausedError, a dry run's
	// included, before anything of the release acts (dispatch://INFRA-3583 R13).
	Start(ctx context.Context, bound time.Duration, dryRun bool) (view ProjectReleaseResponse, started bool, err error)
	// Read is the newest release of the project as its record holds it, and whether one ran.
	Read(ctx context.Context) (ProjectReleaseResponse, bool, error)
	// AskRound is the round the running release's asks name ("release:<id>"), "" while none runs.
	AskRound() string
	// Answer records the answer of token's claim to the running release's ask: the claim's
	// session and generation must be the ones the claim holds now (claim.SameAgentRefusal,
	// claim.StaleGeneration), and the claim must be one the release asked. It records the
	// answer's time on the release and asks the machine nothing. An answer that comes once the
	// bound of the claim's pod has passed is accepted and not recorded (ReleasedAsk).
	Answer(ctx context.Context, token claim.Token, session string, generation uint64, head string) error
}

// ProjectReleaseRequest is the body of `POST /legion/v1/operator/project/release`: the bound, a
// Go duration ("" for the daemon's pod_update_grace_minutes), and whether this is a dry run,
// which reads and stops nothing.
type ProjectReleaseRequest struct {
	Bound  string `json:"bound,omitempty"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// ProjectReleaseResponse is one project release as the routes and the state show it: its record's
// id (none on a dry run), when it started and when it deletes every pod still waiting (BoundAt),
// that bound as a Go duration, when it finished its acts and when a boot ended it, each absent
// until it happens, who ended it (store.ProjectReleaseEndedByBoot), and its result as the release
// last recorded it. A dry run is finished when it is answered and is never recorded.
type ProjectReleaseResponse struct {
	ID         int64                     `json:"id,omitempty"`
	DryRun     bool                      `json:"dryRun,omitempty"`
	StartedAt  time.Time                 `json:"startedAt"`
	BoundAt    time.Time                 `json:"boundAt"`
	Bound      string                    `json:"bound"`
	FinishedAt *time.Time                `json:"finishedAt,omitempty"`
	EndedAt    *time.Time                `json:"endedAt,omitempty"`
	EndedBy    store.ProjectReleaseEnder `json:"endedBy,omitempty"`
	Result     ProjectReleaseResult      `json:"result"`
}

// ProjectReleaseResult is what the release did, or on a dry run what it would do now: one line per
// Sandbox of the project, in name order, followed in name order by any Sandbox the release found
// only when it listed the project again from the API server, and one per claim of its stop set, in
// token order. Both are never null. Failure is what stopped the release short of its acts, absent
// while nothing did.
type ProjectReleaseResult struct {
	Sandboxes []ReleasedSandbox `json:"sandboxes"`
	Claims    []ReleasedClaim   `json:"claims"`
	Failure   string            `json:"failure,omitempty"`
}

// ReleaseOutcome is how the release ended a Sandbox's line (ReleasedSandbox.Outcome).
type ReleaseOutcome string

// The outcomes of a Sandbox's line. An issue pod ends as one of the four deleted_* outcomes
// (deleted_clean, deleted_unread, deleted_lost or deleted_read_refused) or as left, and every
// other Sandbox as deleted or left; a line not yet ended is waiting.
const (
	// ReleaseWaiting: the release has not ended this Sandbox yet.
	ReleaseWaiting ReleaseOutcome = "waiting"
	// ReleaseDeletedClean: deleted, its last read finding nothing unpushed; for a pod no role
	// process ran in at any read, that read is its launcher's, whose narrower count the reason names.
	ReleaseDeletedClean ReleaseOutcome = "deleted_clean"
	// ReleaseDeletedUnread: deleted unread: no role process ran in it at any read, and its launcher
	// did not answer the read or refused it, which the reason gives.
	ReleaseDeletedUnread ReleaseOutcome = "deleted_unread"
	// ReleaseDeletedLost: deleted with the work its last read found unpushed; for a pod no role
	// process ran in at any read, that read is its launcher's, whose narrower count the reason names.
	ReleaseDeletedLost ReleaseOutcome = "deleted_lost"
	// ReleaseDeletedReadRefused: deleted with its workspace unread at the end: its shim refused
	// the last read, or no role process ran at the last read while an earlier read reached one.
	// The line then keeps that earlier read's count or refusal, and its reason says so.
	ReleaseDeletedReadRefused ReleaseOutcome = "deleted_read_refused"
	// ReleaseDeleted: a controller's, a cache's or a probe's Sandbox deleted.
	ReleaseDeleted ReleaseOutcome = "deleted"
	// ReleaseLeft: the Sandbox stays, with its disk, for the reason the line gives.
	ReleaseLeft ReleaseOutcome = "left"
)

// ReleaseOutcomes is every outcome, in the order declared above: the roster of the set, which
// nothing in Go can read off the constants themselves. TestProjectReleaseWordsGolden writes it to
// `packages/contracts/fixtures/daemon-api/project-release-words.json`, and
// `packages/contracts/src/legion-api.test.ts` holds that file equal to `LEGION_RELEASE_OUTCOMES`,
// the strict enum a plugin reads `outcome` through. An outcome added above and left out of this
// list fails no test, so add every new outcome here as well.
var ReleaseOutcomes = []ReleaseOutcome{ReleaseWaiting, ReleaseDeletedClean, ReleaseDeletedUnread, ReleaseDeletedLost,
	ReleaseDeletedReadRefused, ReleaseDeleted, ReleaseLeft}

// releaseDeletedOutcomes is every outcome of a Sandbox the release deleted (ReleasedSandbox.Deleted).
var releaseDeletedOutcomes = []ReleaseOutcome{ReleaseDeletedClean, ReleaseDeletedUnread, ReleaseDeletedLost, ReleaseDeletedReadRefused, ReleaseDeleted}

// ReleasedSandbox is one Sandbox's line of a release: its name, its kind (issue, controller, cache,
// probe or unknown), its issue for an issue pod, and its outcome. A line of work lost carries the
// count of unpushed commits and whether the working copy held a change at the last read; one of a
// workspace unread, the shim's refusal. A deleted_read_refused line whose last read found no role
// process keeps the count, the flag or the refusal of the latest read that reached one instead. A
// left line gives its reason — such as an ask to its roles that could not be written by the bound,
// so no role of it was asked — and a deleted_read_refused line whose last read found no role
// process says that as its reason. The line of a pod no role process ran in at any read gives as
// its reason that its launcher read the workspace, counting only the commits under a working copy
// or a local bookmark, which is fewer than a role's shim counts, or that the launcher did not read
// it and why; several reasons are joined by "; ". OtherImage marks an issue pod whose pod runs an
// image other than the daemon's own. Asked is, until the bound, each role the release's ask was
// written to, and from the bound only those of them the ask reached a session of before the bound
// or that answered; on a dry run it is each role a release would ask. Each carries when it answered
// or whether the bound passed first.
type ReleasedSandbox struct {
	Name               string                     `json:"name"`
	Kind               runtime.ProjectSandboxKind `json:"kind"`
	Issue              string                     `json:"issue,omitempty"`
	Outcome            ReleaseOutcome             `json:"outcome"`
	Reason             string                     `json:"reason,omitempty"`
	Commits            *int                       `json:"commits,omitempty"`
	WorkingCopyChanged *bool                      `json:"workingCopyChanged,omitempty"`
	Refusal            string                     `json:"refusal,omitempty"`
	OtherImage         bool                       `json:"otherImage,omitempty"`
	Asked              []ReleasedAsk              `json:"asked,omitempty"`
}

// Deleted is whether the release deleted the line's Sandbox: an issue pod by any of its four
// deleted outcomes, any other Sandbox as ReleaseDeleted. A line left, or still waiting, is not.
func (s ReleasedSandbox) Deleted() bool { return slices.Contains(releaseDeletedOutcomes, s.Outcome) }

// ReleasedAsk is one role of a pod's line (ReleasedSandbox.Asked): when it answered, or Bound once
// the bound passed with no answer; neither while it may still answer, and neither on a dry run. The
// line keeps a role at the bound only when the ask reached a session of it before the bound or it
// answered, so a role with Bound set is one the ask reached. An answer that comes once Bound is set
// is not recorded, so the two are never both set.
type ReleasedAsk struct {
	Role       claim.Role `json:"role"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
	Bound      bool       `json:"bound,omitempty"`
}

// ReleasedClaimEnd is how a claim of the stop set ended as its own record holds it at the
// release's end (ReleasedClaim.Ended): the source of its suspension (supervise.SuspendSource)
// when it is suspended, its state (supervise.ClaimState) otherwise, so never `suspended`, or
// ReleasedClaimUnrecorded.
type ReleasedClaimEnd string

// ReleasedClaimUnrecorded is how a claim of the stop set ended when the store holds no record of
// it at the release's end: the next boot starts no claim it holds no record of.
const ReleasedClaimUnrecorded ReleasedClaimEnd = "unrecorded"

// ReleasedClaimEndOf is how c ended, by the rule above: the source of its suspension when it is
// suspended, its state otherwise.
func ReleasedClaimEndOf(c supervise.Claim) ReleasedClaimEnd {
	if c.State == supervise.StateSuspended {
		return ReleasedClaimEnd(c.SuspendedBy)
	}
	return ReleasedClaimEnd(c.State)
}

// ReleasedClaimEnds is every ReleasedClaimEnd, in order: each claim state but suspended
// (supervise.States), each suspension source (supervise.SuspendSources), and
// ReleasedClaimUnrecorded. TestProjectReleaseWordsGolden writes it to
// `packages/contracts/fixtures/daemon-api/project-release-words.json`, and
// `packages/contracts/src/legion-api.test.ts` holds that file equal to
// `LEGION_RELEASED_CLAIM_ENDS`, the strict enum a plugin reads `ended` through. supervise holds
// each of its two lists to the constants of its type, so a state or a source added there reaches
// this list, and fails that golden test and then the contracts suite until the enum names it.
var ReleasedClaimEnds = releasedClaimEnds()

// releasedClaimEnds builds ReleasedClaimEnds.
func releasedClaimEnds() []ReleasedClaimEnd {
	states, sources := supervise.States(), supervise.SuspendSources()
	ends := make([]ReleasedClaimEnd, 0, len(states)+len(sources))
	for _, state := range states {
		if state != supervise.StateSuspended {
			ends = append(ends, ReleasedClaimEnd(state))
		}
	}
	for _, source := range sources {
		ends = append(ends, ReleasedClaimEnd(source))
	}
	return append(ends, ReleasedClaimUnrecorded)
}

// ReleasedClaim is one claim of the release's stop set: its token, issue, role and session, and how
// it ended as its own record holds it at the end — `project_release` for a claim the daemon's next
// boot starts (BootStarts), or the source or state of a claim it does not, such as `operator`,
// `daemon`, `exit`, `failed` or `retired`, or ReleasedClaimUnrecorded for a claim the store holds
// no record of; absent while the release has not ended it, and when the release could not read
// the claims at its end. Detail is why a claim the release stopped, or held for an operator, was
// not stored suspended in time, or that its record could not be read at the end.
type ReleasedClaim struct {
	Token      claim.Token      `json:"token"`
	Issue      string           `json:"issue,omitempty"`
	Role       claim.Role       `json:"role"`
	Session    string           `json:"session,omitempty"`
	Ended      ReleasedClaimEnd `json:"ended,omitempty"`
	BootStarts bool             `json:"bootStarts"`
	Detail     string           `json:"detail,omitempty"`
}

// ProjectReleaseResponseOf is a release's record as the routes and the state show it. The result
// is the release's own JSON, which the store keeps unread; a record whose result names nothing yet
// (`{}`) shows empty lists.
func ProjectReleaseResponseOf(release store.ProjectRelease) (ProjectReleaseResponse, error) {
	view := ProjectReleaseResponse{
		ID: release.ID, StartedAt: release.StartedAt.UTC(), BoundAt: release.BoundAt.UTC(),
		Bound: release.BoundAt.Sub(release.StartedAt).String(), EndedBy: release.EndedBy,
	}
	if !release.FinishedAt.IsZero() {
		at := release.FinishedAt.UTC()
		view.FinishedAt = &at
	}
	if !release.EndedAt.IsZero() {
		at := release.EndedAt.UTC()
		view.EndedAt = &at
	}
	if len(release.Result) > 0 {
		if err := json.Unmarshal(release.Result, &view.Result); err != nil {
			return ProjectReleaseResponse{}, fmt.Errorf("read the result of project release %d: %w", release.ID, err)
		}
	}
	view.Result = view.Result.Listed()
	return view, nil
}

// Listed is result with each list present, so a result that names nothing yet reads as two empty
// lists rather than two absent ones.
func (r ProjectReleaseResult) Listed() ProjectReleaseResult {
	if r.Sandboxes == nil {
		r.Sandboxes = []ReleasedSandbox{}
	}
	if r.Claims == nil {
		r.Claims = []ReleasedClaim{}
	}
	return r
}

// startProjectRelease is `POST /legion/v1/operator/project/release`: 202 with the release's record
// once it is recorded, 200 with the report of a dry run, and 200 with this process's release, which
// starts nothing, once one ran here. A bound that is no positive Go duration is refused 400 before
// anything starts, and, while a pause holds the project, a release or a dry run of a process that
// ran none 409 PROJECT_PAUSED (releasePausedFailure). Its answer is the `legion` command's, which
// no plugin parses (answerUnparsed, writeUnparsedAnswer), as the operator's pause's is; every
// refusal is AnswerRefusal.
func (s *server) startProjectRelease(w http.ResponseWriter, r *http.Request) {
	if s.projectRelease == nil {
		s.refuse(w, r, http.StatusInternalServerError, "the project release is not served by this daemon")
		return
	}
	var req ProjectReleaseRequest
	if !s.readOptionalBody(w, r, &req) {
		return
	}
	var bound time.Duration
	if req.Bound != "" {
		parsed, err := time.ParseDuration(req.Bound)
		if err != nil || parsed <= 0 {
			s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("bound %q is no positive Go duration, such as 30m", req.Bound))
			return
		}
		bound = parsed
	}
	view, started, err := s.projectRelease.Start(r.Context(), bound, req.DryRun)
	var paused *store.ProjectPausedError
	switch {
	case errors.Is(err, ErrProjectReleaseRunsNoPod):
		s.writeFailure(w, r, http.StatusConflict, projectReleaseNoPod, ProjectReleaseRunsNoPod)
		return
	case errors.As(err, &paused):
		held := make([]string, len(paused.Held))
		for i, pod := range paused.Held {
			held[i] = pod.Issue
		}
		s.log.Info("api: refused a project release: a pause holds the project", "dry_run", req.DryRun,
			"paused", paused.Paused, "pause", paused.Pause.ID, "held", held)
		s.writeAnswer(w, r, AnswerRefusal, http.StatusConflict, releasePausedFailure(paused))
		return
	case err != nil:
		s.logRequestFailure(r, err, "api: the project release could not start")
		s.refuse(w, r, http.StatusInternalServerError, "start the project release: "+err.Error())
		return
	}
	status := http.StatusOK
	if started {
		status = http.StatusAccepted
	}
	s.writeUnparsedAnswer(w, r, status, view)
}

// releasePausedFailure is the refusal of a project release a pause holds (store.ProjectPausedError,
// dispatch://INFRA-3583 R13): PROJECT_PAUSED, naming the pause that stands where one does, and each
// pod a pause still holds with its claims stored suspended by `released`. Its sentence says why the
// release waits — it reads a pod in which no role runs only through the pod's launcher, which does
// not count every commit its roles did not push, and deletes it with no role asked to push — that
// nothing was recorded, stopped or deleted, and how an operator ends each hold:
// the resume of the project, then `legion claims resume --claim <token>` of one role of each held
// pod, whose ready ends that pod's hold.
func releasePausedFailure(refused *store.ProjectPausedError) PauseFailure {
	body := PauseFailure{Failure: Failure{Code: projectPaused}}
	var why []string
	if refused.Paused {
		view := pauseViewOf(refused.Pause)
		body.Pause = &view
		why = append(why, "the project is paused ("+describePause(refused.Pause)+")")
	}
	if len(refused.Held) > 0 {
		body.HeldPods = make([]PauseHeldPodView, len(refused.Held))
		pods := make([]string, len(refused.Held))
		for i, held := range refused.Held {
			claims := held.Claims
			if claims == nil {
				claims = []claim.Token{}
			}
			body.HeldPods[i] = PauseHeldPodView{Issue: held.Issue, Pause: held.Pause, HeldAt: held.HeldAt.UTC(), Claims: claims}
			pods[i] = held.Issue + " (" + heldPodClaimsWords(claims) + ")"
		}
		why = append(why, fmt.Sprintf("a pause still holds %d pod(s), each with its claims stored suspended by `released`: %s", len(pods), strings.Join(pods, "; ")))
	}
	end := "End each pod's hold by resuming one role of the pod (legion claims resume --claim <token>), whose ready ends the hold, then run the release again"
	if refused.Paused {
		end = "Resume the project first (legion project resume); then end the hold of each pod the pause stopped by resuming one role of the pod (legion claims resume --claim <token>), whose ready ends the hold, and run the release again"
	}
	body.Error = "project release refused: " + strings.Join(why, ", and ") +
		". A release reads a pod in which no role runs only through its launcher, which does not count every commit its roles did not push, and deletes it with no role asked to push, so it does not run while a pause holds the project; nothing was recorded, stopped or deleted. " + end
	return body
}

// heldPodClaimsWords is the claims of a held pod as the release's refusal names them: the ones
// whose resume ends the hold, or, where the store holds none of the pod's claims suspended by
// `released`, where to find the pod's claims.
func heldPodClaimsWords(claims []claim.Token) string {
	if len(claims) == 0 {
		return "no claim of it is stored suspended by `released`; legion claims list names its claims"
	}
	tokens := make([]string, len(claims))
	for i, token := range claims {
		tokens[i] = string(token)
	}
	return "resume one of " + strings.Join(tokens, ", ")
}

// readProjectRelease is `GET /legion/v1/operator/project/release`: 200 with the newest release of
// the project, its progress while it runs and its result once it finished, and 404 when none ran.
// Its answer is the `legion` command's, as startProjectRelease's is.
func (s *server) readProjectRelease(w http.ResponseWriter, r *http.Request) {
	if s.projectRelease == nil {
		s.refuse(w, r, http.StatusInternalServerError, "the project release is not served by this daemon")
		return
	}
	view, found, err := s.projectRelease.Read(r.Context())
	if err != nil {
		s.logRequestFailure(r, err, "api: the project release could not be read")
		s.refuse(w, r, http.StatusInternalServerError, "read the project release: "+err.Error())
		return
	}
	if !found {
		s.refuse(w, r, http.StatusNotFound, "no project release of this project was recorded")
		return
	}
	s.writeUnparsedAnswer(w, r, http.StatusOK, view)
}
