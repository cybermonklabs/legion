package api

// production_events.go serves the record of a project's deploys and failed changes
// (internal/dispatch/prodevents; dispatch://INFRA-978, R6 to R10): one page of a project's rows,
// the post of one event, and the state route that rejects, re-counts or restores a failure. The
// store makes every write in a transaction of its own and drains the change rows it logged after
// that commit (delivery.DrainAfterCommit), so a handler here holds no connection while it calls
// it.
//
// Who writes what:
//
//   - A system posts a deploy or a failure with an agent or service token: any credential that
//     does not hold the person scope. Its failure is the signal `api`, and its actor the one its
//     credential and body name (requireActor), as every bearer write records.
//   - A person (the person scope: a browser, a personal token, a broker-issued token acting for
//     its person) posts a failure alone, the drill-down's flag: signal `person`, the person as
//     actor, and the server's own source_ref, `person:<deploy id>:<login>`, so a person's second
//     flag of one deploy writes nothing.
//   - A person rejects, re-counts and restores a failure; the project's controller rejects and
//     re-counts one. The controller is proven, never claimed: the request's credential proved its
//     session (a broker-issued token, callerSession) and that session is the one the project's
//     Legion daemon names as its registered controller at the moment of the write, read from the
//     daemon's GET /legion/v1/controller before the write's transaction opens (legion's
//     Client.Controller). The precedent for who may write a state and what the row keeps is
//     POST /api/v1/results/{id}/retract.
//
// The route class is scopeProject, which reads the path's key alone, so the state route holds the
// row to that project itself: a row of another project is 404 PRODUCTION_EVENT_NOT_FOUND. And both
// writes hold the deploy they name, a failure's blamed deploy and a restore's restoring deploy, to
// a deploy that counts for that project (evals.DeployCountsFor), whichever project's row it is,
// asked in the write's transaction: a deploy that counts for other projects alone is answered as a
// deploy no row holds, so neither write reaches another project's numbers through its deploy, nor
// tells the caller which deploys another project made.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const (
	// codeProductionEventDeployForbidden refuses a deploy posted by a person's credential: a
	// deploy is a system's event.
	codeProductionEventDeployForbidden = "PRODUCTION_EVENT_DEPLOY_FORBIDDEN"
	// codeProductionEventStateForbidden refuses a state write by any caller but a person and, for
	// a reject or a re-count, the project's controller.
	codeProductionEventStateForbidden = "PRODUCTION_EVENT_STATE_FORBIDDEN"
	// codeLegionControllerUnread refuses a reject or a re-count by a caller that is not a person
	// where Dispatch could not learn the project's controller from its Legion daemon: the daemon
	// answered GET /legion/v1/controller 404 (a build before that route), did not answer within
	// legion.ControllerTimeout, or answered something unreadable.
	codeLegionControllerUnread = "LEGION_CONTROLLER_UNREAD"
	// codeProductionEventNotFound is a state write naming a row the project does not hold.
	codeProductionEventNotFound = "PRODUCTION_EVENT_NOT_FOUND"
)

// productionEventsQueryKeys is the query keys the list read takes, read from its shape's query
// struct as an eval read's are (evalQueryKeysOf): any other key is refused, and so is a key named
// twice.
var productionEventsQueryKeys = evalQueryKeysOf(productionEventsQuery{})

// productionEventList is the body GET /api/v1/projects/{key}/production-events answers: one page
// of the project's rows, newest first, and the count of every row the query matches.
type productionEventList struct {
	Items  []prodevents.Event `json:"items"`
	Total  int                `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

// productionEventNoteBytes is the most a note may hold: a paragraph, not a log.
const productionEventNoteBytes = 4096

// productionEventClockSkew is how far past the server's clock a posted finished_at or
// detected_at may be: a sender's clock a little ahead of it, not an event that has not happened.
const productionEventClockSkew = 5 * time.Minute

// productionEventInput is the body POST /api/v1/projects/{key}/production-events takes: one
// event. Where OpenTelemetry names a field (the deployment and CI/CD semantic conventions), the
// body may carry that name instead of the plain one, or both alike.
type productionEventInput struct {
	// Kind is deploy or failure.
	Kind string `json:"kind" required:"true" enum:"deploy,failure"`
	// SourceRef is the system's own key for the event: a repeat under it answers the first row
	// and writes nothing. A system's post needs one; a person's flag takes the server's.
	SourceRef string `json:"source_ref"`
	// Environment is the deployment environment, production where the body names none.
	Environment string `json:"environment"`
	// DeploymentEnvironmentName is OTel's name for Environment.
	DeploymentEnvironmentName string `json:"deployment.environment.name"`
	// Status is the system's own status word, stored as sent.
	Status string `json:"status"`
	// DeploymentStatus is OTel's name for Status.
	DeploymentStatus string `json:"deployment.status"`
	// URL is the page of the deploy or of the failure.
	URL string `json:"url"`
	// PipelineRunURL is OTel's name for URL.
	PipelineRunURL string `json:"cicd.pipeline.run.url.full"`
	// PipelineRunID names a failure's deploy by its GitHub run: the deploy row the GitHub adapter
	// wrote for run PipelineRunID of Repo.
	PipelineRunID string `json:"cicd.pipeline.run.id"`
	// StartedAt and FinishedAt are a deploy's start and the moment it reached production, which
	// a deploy needs, FinishedAt at or after StartedAt and at most productionEventClockSkew past
	// the server's clock; a failure carries no FinishedAt, since it counts on its deploy's day.
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	// DetectedAt is when a failure was found, the server's time of arrival where it names none, and
	// at most productionEventClockSkew past the server's clock.
	DetectedAt *time.Time `json:"detected_at"`
	// Repo is the deploy's repository, and the repository of a failure's PipelineRunID.
	Repo string `json:"repo"`
	// Note is what the writer says of the event, which the record keeps as sent (at most
	// productionEventNoteBytes).
	Note string `json:"note"`
	// Commits are the commits a deploy shipped, compared in lower case.
	Commits []string `json:"commits"`
	// PullRequests are the pull requests a deploy shipped or a failure blames, each owner/repo#n
	// (pullRequestRef).
	PullRequests []string `json:"pull_requests"`
	// DeployID names a failure's deploy by its row's id: the uuid of a deploy row that counts for
	// the route's project, whichever project's row it is.
	DeployID *string `json:"deploy_id"`
	// CommitSHA names a failure's deploy by a commit: the earliest deploy that shipped it among
	// those that count for the route's project, whichever project's row it is.
	CommitSHA string `json:"commit_sha"`
	// Actor is a bearer's own session, as every bearer write names it (bearerSessionActor).
	Actor *model.Actor `json:"actor"`
}

// productionEventStateInput is the body PUT /api/v1/projects/{key}/production-events/{id}/state
// takes.
type productionEventStateInput struct {
	// State is rejected (out of the numbers), counted (back in) or restored (the failure ended).
	State string `json:"state" required:"true" enum:"counted,rejected,restored"`
	// Reason is why a reject or a re-count is right, which the row keeps; a restore keeps none.
	Reason string `json:"reason"`
	// RestoredByDeploy names the deploy that restored the failure, with state restored alone.
	RestoredByDeploy *string `json:"restored_by_deploy"`
}

// GET /api/v1/projects/{key}/production-events  (every authenticated reader)
//
// One page of the project's rows, newest first: a deploy by its finished_at and a failure by its
// detected_at, within [from, to).
func (s *server) listProductionEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query, err := parseProductionEventsQuery(r.URL.Query())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	query.Project = r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, query.Project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, total, err := prodevents.List(r.Context(), s.deps.Store.Pool, query)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, productionEventList{Items: events, Total: total, Limit: query.Limit, Offset: query.Offset})
}

// parseProductionEventsQuery reads the list read's query: the window as every delivery and eval
// read takes it (parseDeliveryWindow: RFC3339 in the UTC years 1 to 9999, the last 7 days by
// default, at most 84 days), one kind, one state, one deploy whose failures it keeps (a uuid, its
// canonical spelling passed on), and the eval reads' page (limit 1 to evalPageMax,
// evalPageDefault where absent; offset from 0). A malformed key, a key the route does not take and
// a key named twice are each 400 INVALID_QUERY naming it.
func parseProductionEventsQuery(query url.Values) (prodevents.ListQuery, error) {
	if err := undeclaredQueryKey(query, productionEventsQueryKeys.declared); err != nil {
		return prodevents.ListQuery{}, err
	}
	if err := repeatedQueryKey(query, productionEventsQueryKeys.oneValue); err != nil {
		return prodevents.ListQuery{}, err
	}
	from, to, err := parseDeliveryWindow(query)
	if err != nil {
		return prodevents.ListQuery{}, invalidEvalQuery(err)
	}
	kind := query.Get("kind")
	if kind != "" && kind != prodevents.KindDeploy && kind != prodevents.KindFailure {
		return prodevents.ListQuery{}, invalidEvalQuery(fmt.Errorf("kind %q is not deploy or failure", kind))
	}
	state := query.Get("state")
	if state != "" && state != prodevents.StateCounted && state != prodevents.StateRejected {
		return prodevents.ListQuery{}, invalidEvalQuery(fmt.Errorf("state %q is not counted or rejected", state))
	}
	deploy := ""
	if query.Has("deploy_id") {
		parsed, err := uuid.Parse(query.Get("deploy_id"))
		if err != nil {
			return prodevents.ListQuery{}, invalidEvalQuery(fmt.Errorf("deploy_id %q is not a production event id, a uuid", evals.BoundedField(query.Get("deploy_id"))))
		}
		deploy = parsed.String()
	}
	limit, offset, err := parseOffsetPage(query, evalPageDefault, evalPageMax)
	if err != nil {
		return prodevents.ListQuery{}, err
	}
	return prodevents.ListQuery{From: from, To: to, Kind: kind, State: state, DeployID: deploy, Limit: limit, Offset: offset}, nil
}

// POST /api/v1/projects/{key}/production-events  (a system's deploy or failure, a person's failure)
//
// Stores one event: 201 with the new row, or 200 with the row the project already holds under the
// event's source_ref, writing nothing. A failure blames a deploy that counts for the route's
// project (evals.DeployCountsFor), asked in the write's transaction once the store holds the
// deploy's row (prodevents.DeployCheck): a deploy that counts for other projects alone, public or
// private and whoever posts, is answered as a deploy no row holds is by the same selector, and the
// failure, which counts for the route's project alone (evals' failure_days.go), writes nothing.
func (s *server) postProductionEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input productionEventInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Asked before any write opens a transaction, as the retract route asks it: a cookie caller's
	// credential is read through the pool, which refuses a caller holding a connection a second.
	person := s.callerHoldsScope(r, model.ScopePerson)
	event, err := input.event(person)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if event.Kind == prodevents.KindDeploy && person {
		writeError(w, codeProductionEventDeployForbidden, http.StatusForbidden,
			"a deploy is a system's event, posted with an agent or service token; a person's credential (the person scope) posts a failure, the drill-down's flag")
		return
	}
	var actor model.Actor
	var ok bool
	if person {
		if actor, ok = s.actingPerson(w, r); ok {
			actor, ok = actSessionFromBody(w, actor, input.Actor)
		}
	} else {
		actor, ok = s.requireActor(w, r, input.Actor)
	}
	if !ok {
		return
	}
	event.Actor = actor
	event.ProjectKey = r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, event.ProjectKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var blamed blame
	var check prodevents.DeployCheck
	if event.Kind == prodevents.KindFailure {
		if blamed, err = s.blamedDeploy(r.Context(), event.ProjectKey, input); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		event.DeployID = &blamed.id
		supplied := strings.TrimSpace(input.SourceRef)
		if person {
			event.SourceRef = prodevents.PersonSourceRef(blamed.id, actor.ID)
		}
		// The deploy's own questions, once the store holds its row: whether it counts for the
		// route's project, and only then whether a person's source_ref is the server's key of it,
		// whose refusal names that deploy's id.
		check = func(ctx context.Context, tx pgx.Tx, deploy prodevents.Event) error {
			counts, err := evals.DeployCountsFor(ctx, tx, s.deps.Attributor, event.ProjectKey, deploy)
			switch {
			case err != nil:
				return err
			case !counts:
				return blamed.none
			case person && supplied != "" && supplied != event.SourceRef:
				return productionEventInputError(
					"a person's flag is keyed by the server, %s, so a second flag of one deploy writes nothing; send no source_ref, or that one",
					event.SourceRef)
			}
			return nil
		}
	}
	stored, created, err := prodevents.WriteChecked(r.Context(), s.deps.Store.Pool, event, check)
	// A blamed deploy the store holds no row of, or no deploy row of (a run's or a commit's row
	// dropped since its lookup among them), is answered in the selector's words too.
	var refused *prodevents.Error
	if errors.As(err, &refused) && refused.Code == prodevents.CodeNoDeploy && blamed.none != nil {
		err = blamed.none
	}
	if err != nil {
		s.writeProductionEventError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	WriteJSON(w, status, stored)
}

// event is the row input asks for, before its project, its actor and a failure's deploy are
// known: the plain and the OTel name of one field taken as one, a system's source_ref required and
// refused where it starts with an automatic source's prefix (prodevents.AutomaticSourceRefPrefix),
// the times no event has refused (checkedTimes) and each pull request it names held to
// `owner/repo#n` (pullRequestRef), the fields of a failure's deploy refused on a deploy, a
// failure's signal and source the caller's (person or api), and its detected_at the server's time
// where the body names none.
func (input productionEventInput) event(person bool) (prodevents.Event, error) {
	if input.Kind != prodevents.KindDeploy && input.Kind != prodevents.KindFailure {
		return prodevents.Event{}, productionEventInputError("kind must be deploy or failure, not %q", input.Kind)
	}
	if !person && strings.TrimSpace(input.SourceRef) == "" {
		return prodevents.Event{}, productionEventInputError("source_ref is required: it is the system's own key for the event, which makes a repeat write no second row")
	}
	// A person's flag is keyed by the server (postProductionEvent), under the person prefix, so the
	// refusal is a system's alone.
	if prefix, automatic := prodevents.AutomaticSourceRefPrefix(input.SourceRef); !person && automatic {
		return prodevents.Event{}, productionEventInputError(
			"source_ref %q starts with %s, the prefix an automatic source writes its own rows under, whose write a post under its key would leave writing nothing; send a key of the system's own",
			input.SourceRef, prefix)
	}
	environment, err := otelField("environment", input.Environment, "deployment.environment.name", input.DeploymentEnvironmentName)
	if err != nil {
		return prodevents.Event{}, err
	}
	status, err := otelField("status", input.Status, "deployment.status", input.DeploymentStatus)
	if err != nil {
		return prodevents.Event{}, err
	}
	link, err := otelField("url", input.URL, "cicd.pipeline.run.url.full", input.PipelineRunURL)
	if err != nil {
		return prodevents.Event{}, err
	}
	commits := make([]string, len(input.Commits))
	for index, commit := range input.Commits {
		commits[index] = strings.ToLower(strings.TrimSpace(commit))
	}
	pulls := make([]string, len(input.PullRequests))
	for index, pull := range input.PullRequests {
		if pulls[index], err = pullRequestRef(pull); err != nil {
			return prodevents.Event{}, err
		}
	}
	event := prodevents.Event{
		Kind: input.Kind, Source: prodevents.SourceAPI, Environment: environment, Status: status, SourceRef: input.SourceRef,
		StartedAt: input.StartedAt, FinishedAt: input.FinishedAt, Repo: input.Repo, Commits: commits,
		PullRequests: pulls, URL: link, Note: input.Note,
	}
	if len(input.Note) > productionEventNoteBytes {
		return prodevents.Event{}, productionEventInputError("note holds %d bytes, past the %d a note may hold", len(input.Note), productionEventNoteBytes)
	}
	now := time.Now().UTC()
	if err := input.checkedTimes(now); err != nil {
		return prodevents.Event{}, err
	}
	if input.Kind == prodevents.KindDeploy {
		if input.PipelineRunID != "" || input.CommitSHA != "" || input.DeployID != nil || input.DetectedAt != nil {
			return prodevents.Event{}, productionEventInputError(
				"deploy_id, cicd.pipeline.run.id, commit_sha and detected_at name a failure's deploy and its detection; a deploy's own run is its source_ref and url")
		}
		return event, nil
	}
	signal := prodevents.SignalAPI
	if person {
		signal = prodevents.SignalPerson
		event.Source = prodevents.SourcePerson
	}
	event.Signal = &signal
	detected := now
	if input.DetectedAt != nil {
		detected = *input.DetectedAt
	}
	event.DetectedAt = &detected
	return event, nil
}

// checkedTimes refuses the times of input no event has, each naming its field: a finished_at
// before started_at, and a finished_at or a detected_at past now, the server's clock, by more than
// productionEventClockSkew.
func (input productionEventInput) checkedTimes(now time.Time) error {
	latest := now.Add(productionEventClockSkew)
	switch {
	case input.StartedAt != nil && input.FinishedAt != nil && input.FinishedAt.Before(*input.StartedAt):
		return productionEventInputError("finished_at %s is before started_at %s: an event finishes at or after its start",
			input.FinishedAt.Format(time.RFC3339Nano), input.StartedAt.Format(time.RFC3339Nano))
	case input.FinishedAt != nil && input.FinishedAt.After(latest):
		return productionEventInputError("finished_at %s is past the server's clock, %s, by more than %s: post an event once it has happened",
			input.FinishedAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), productionEventClockSkew)
	case input.DetectedAt != nil && input.DetectedAt.After(latest):
		return productionEventInputError("detected_at %s is past the server's clock, %s, by more than %s: post a failure once it was found",
			input.DetectedAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), productionEventClockSkew)
	}
	return nil
}

// pullRequestRef is ref, a pull request an event names, trimmed of the space around it, and
// refuses one that is not `owner/repo#n` by the one reading of such a reference
// (refs.ParseExternalRef) with n a number a pull request's row holds, a positive int32, as the
// evals read the record's pull requests (evals' parsePullRequestRef).
func pullRequestRef(ref string) (string, error) {
	if _, number, ok := refs.ParseExternalRef(ref); ok {
		if _, err := strconv.ParseInt(number, 10, 32); err == nil {
			return strings.TrimSpace(ref), nil
		}
	}
	return "", productionEventInputError("pull_requests holds %q, which is no pull request owner/repo#n with n a positive number",
		evals.BoundedField(ref))
}

// otelField is one field the body may name twice, by its plain name and by OpenTelemetry's: the
// value either carries, refused where both carry different values.
func otelField(plainName, plain, otelName, otel string) (string, error) {
	if plain != "" && otel != "" && plain != otel {
		return "", productionEventInputError("%s and %s name one field: send one of them, or both alike", plainName, otelName)
	}
	if plain != "" {
		return plain, nil
	}
	return otel, nil
}

// blame is the deploy a failure blames (server.blamedDeploy), and none, the refusal the route
// answers where no deploy the route's project counts is it, in the words of the selector the body
// named it by, whether no deploy row is it or one that counts for other projects alone is.
type blame struct {
	id   string
	none error
}

// blamedDeploy is the deploy a failure posted to project blames, named exactly one way: its row's
// id, in its canonical spelling where it is a uuid, so a person's flag keys one deploy one way
// (person:<deploy id>:<login>) however the body spells it, and which the store holds to a deploy
// row as it writes (a body's id that is no uuid included); the GitHub run it was
// (prodevents.DeployForRun); or a commit it shipped: the earliest deploy holding it
// (prodevents.DeploysForCommit) that counts for project (evals.DeployCountsFor), so a deploy of
// another project that shipped the commit first neither hides project's own nor turns the post
// into a refusal, where the automatic signals blame the first deploy of any project
// (prodevents.DeployForCommit). A run no deploy row holds, or a commit no deploy that counts for
// project holds, is refused none at once, 409 PRODUCTION_EVENT_NO_DEPLOY, and the route refuses
// none as well a deploy that does not count for its project once its write holds the row
// (postProductionEvent): a commit's deploy dropped or no longer counted since this lookup is
// refused so, never swapped for a later one.
func (s *server) blamedDeploy(ctx context.Context, project string, input productionEventInput) (blame, error) {
	named := 0
	for _, set := range []bool{input.DeployID != nil, input.PipelineRunID != "", input.CommitSHA != ""} {
		if set {
			named++
		}
	}
	switch {
	case named == 0:
		return blame{}, productionEventInputError("a failure names the deploy it blames: deploy_id, cicd.pipeline.run.id with repo, or commit_sha")
	case named > 1:
		return blame{}, productionEventInputError("a failure names the deploy it blames one way: deploy_id, cicd.pipeline.run.id or commit_sha, not two of them")
	case input.DeployID != nil:
		id := *input.DeployID
		if parsed, err := uuid.Parse(id); err == nil {
			id = parsed.String()
		}
		return blame{id: id, none: prodevents.NoDeploy(id)}, nil
	case input.PipelineRunID != "":
		run, err := strconv.ParseInt(input.PipelineRunID, 10, 64)
		if err != nil || run <= 0 {
			return blame{}, productionEventInputError("cicd.pipeline.run.id %q is not a GitHub run id, a positive integer", input.PipelineRunID)
		}
		if strings.TrimSpace(input.Repo) == "" {
			return blame{}, productionEventInputError("cicd.pipeline.run.id names a run of repo: name repo too")
		}
		none := errorf(http.StatusConflict, prodevents.CodeNoDeploy, "no deploy row is run %d of %s", run, input.Repo)
		deploy, found, err := prodevents.DeployForRun(ctx, s.deps.Store.Pool, input.Repo, run)
		if err != nil {
			return blame{}, err
		}
		if !found {
			return blame{}, none
		}
		return blame{id: deploy.ID, none: none}, nil
	}
	none := errorf(http.StatusConflict, prodevents.CodeNoDeploy, "no deploy row holds commit %s", input.CommitSHA)
	candidates, err := prodevents.DeploysForCommit(ctx, s.deps.Store.Pool, project, input.CommitSHA)
	if err != nil {
		return blame{}, err
	}
	for _, deploy := range candidates {
		counts, err := evals.DeployCountsFor(ctx, s.deps.Store.Pool, s.deps.Attributor, project, deploy)
		if err != nil {
			return blame{}, err
		}
		if counts {
			return blame{id: deploy.ID, none: none}, nil
		}
	}
	return blame{}, none
}

// productionEventInputError is the 400 PRODUCTION_EVENT_INPUT of a body no row can hold, naming
// the field.
func productionEventInputError(format string, args ...any) *apiError {
	return errorf(http.StatusBadRequest, prodevents.CodeInput, format, args...)
}

// PUT /api/v1/projects/{key}/production-events/{id}/state  (a person; a reject or a re-count also
// the project's controller)
//
// Rejects, re-counts or restores one failure of the project and answers the row: a reject and a
// re-count keep their reason, writer and time; a restore sets the restore time, its writer and the
// restoring deploy where the body names one. A restoring deploy counts for the route's project
// (evals.DeployCountsFor), asked in the write's transaction once the store holds its row
// (prodevents.DeployCheck): one that counts for other projects alone is answered as a deploy no
// row holds is (prodevents.NoDeploy), and one that finished before the failed deploy finished is
// 400 PRODUCTION_EVENT_STATE_INPUT (prodevents.SetState). Who may write is decided before that
// transaction opens (productionEventStateWriter), since it can ask the project's Legion daemon.
func (s *server) putProductionEventState(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input productionEventStateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	switch input.State {
	case prodevents.StateCounted, prodevents.StateRejected, prodevents.StateRestored:
	default:
		writeError(w, prodevents.CodeStateInput, http.StatusBadRequest, fmt.Sprintf(
			"state must be counted, rejected or restored, not %q", input.State))
		return
	}
	project, id := r.PathValue("key"), r.PathValue("id")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.productionEventStateWriter(w, r, project, input.State)
	if !ok {
		return
	}
	updated, err := prodevents.SetState(r.Context(), s.deps.Store.Pool, project, id, prodevents.StateChange{
		State: input.State, Reason: input.Reason, RestoredByDeploy: input.RestoredByDeploy,
		Check: func(ctx context.Context, tx pgx.Tx, deploy prodevents.Event) error {
			counts, err := evals.DeployCountsFor(ctx, tx, s.deps.Attributor, project, deploy)
			if err == nil && !counts {
				err = prodevents.NoDeploy(deploy.ID)
			}
			return err
		},
		Actor: actor, At: time.Now().UTC(),
	})
	if errors.Is(err, prodevents.ErrNotFound) {
		writeError(w, codeProductionEventNotFound, http.StatusNotFound, fmt.Sprintf(
			"project %s holds no production event %s", project, id))
		return
	}
	if err != nil {
		s.writeProductionEventError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, updated)
}

// productionEventStateWriter answers the actor a state write of project records, or refuses the
// caller. A caller holding the person scope writes every state, as the person it acts for
// (actingPerson). A reject and a re-count are also the controller's: a caller whose credential
// proved its session (callerSession), where that session is the one the project's Legion daemon
// names as its registered controller now, recorded as that session (productionEventController).
// Anyone else is 403 PRODUCTION_EVENT_STATE_FORBIDDEN. Each question is asked before the store
// opens its transaction, the daemon's among them, since Dispatch makes no outside call inside one.
func (s *server) productionEventStateWriter(w http.ResponseWriter, r *http.Request, project, state string) (model.Actor, bool) {
	if s.callerHoldsScope(r, model.ScopePerson) {
		return s.actingPerson(w, r)
	}
	if state == prodevents.StateRestored {
		writeError(w, codeProductionEventStateForbidden, http.StatusForbidden,
			"a restore is a person's: send it with a credential that holds the person scope")
		return model.Actor{}, false
	}
	session, proven := s.callerSession(r)
	if !proven {
		writeError(w, codeProductionEventStateForbidden, http.StatusForbidden, fmt.Sprintf(
			"a %s is a person's (the person scope) or the controller's of %s, proven by the broker-issued token of the session %s's Legion daemon names as its controller; this request's credential proves neither",
			state, project, project))
		return model.Actor{}, false
	}
	if !s.productionEventController(w, r, project, state, session) {
		return model.Actor{}, false
	}
	return s.requireActor(w, r, nil)
}

// productionEventController reports whether session, which the caller's credential proved, is the
// one project's Legion daemon names as its registered controller at this moment, and refuses the
// caller where it is not, naming why. It reads the project's Legion setting and then the daemon's
// GET /legion/v1/controller (legion's Client.Controller, bounded by legion.ControllerTimeout), and
// falls back to nothing: no whole state, and nothing Dispatch stored of the daemon.
//
//   - 403 PRODUCTION_EVENT_STATE_FORBIDDEN where the caller is proven not the controller: the
//     project names no Legion daemon, the daemon names no registered controller, or it names
//     another session.
//   - 503 LEGION_CONTROLLER_UNREAD where Dispatch could not learn the controller: the daemon
//     answered 404, a build before the route, or did not answer within the bound, or answered
//     something unreadable.
//
// Each refusal says that a person can reject or re-count the failure.
func (s *server) productionEventController(w http.ResponseWriter, r *http.Request, project, state, session string) bool {
	const person = "a person (the person scope) can reject or re-count it"
	setting, err := legion.GetSettings(r.Context(), s.deps.Store.Pool, project)
	if errors.Is(err, legion.ErrNoSettings) {
		writeError(w, codeProductionEventStateForbidden, http.StatusForbidden, fmt.Sprintf(
			"a %s is a person's or the controller's of %s, and %s names no Legion daemon, so session %s is not its controller; %s",
			state, project, project, session, person))
		return false
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	controller, named, err := legion.New(setting.StateURL).Controller(r.Context())
	switch {
	case errors.Is(err, legion.ErrNoControllerRoute):
		writeError(w, codeLegionControllerUnread, http.StatusServiceUnavailable, fmt.Sprintf(
			"Dispatch could not learn %s's controller: %s's Legion daemon answered 404 to GET %s, so it runs a build before that route; %s",
			project, project, legion.ControllerPath, person))
		return false
	case err != nil:
		// Every other failure of the read wraps legion.ErrUnavailable and names its cause.
		writeError(w, codeLegionControllerUnread, http.StatusServiceUnavailable, fmt.Sprintf(
			"Dispatch could not learn %s's controller from its Legion daemon: %v; %s",
			project, err, person))
		return false
	case !named:
		writeError(w, codeProductionEventStateForbidden, http.StatusForbidden, fmt.Sprintf(
			"a %s is a person's or the controller's of %s, and %s's Legion daemon names no registered controller, so session %s is not its controller; %s",
			state, project, project, session, person))
		return false
	case controller != session:
		writeError(w, codeProductionEventStateForbidden, http.StatusForbidden, fmt.Sprintf(
			"a %s is a person's or the controller's of %s, and session %s is not the session %s's Legion daemon names as its controller; %s",
			state, project, session, project, person))
		return false
	}
	return true
}

// writeProductionEventError answers a refusal of the store (*prodevents.Error) as {code, error}:
// 409 for a deploy no deploy row is (PRODUCTION_EVENT_NO_DEPLOY), 400 for every other; anything
// else as writeHandlerError does.
func (s *server) writeProductionEventError(w http.ResponseWriter, err error) {
	var refused *prodevents.Error
	if !errors.As(err, &refused) {
		s.writeHandlerError(w, err)
		return
	}
	status := http.StatusBadRequest
	if refused.Code == prodevents.CodeNoDeploy {
		status = http.StatusConflict
	}
	writeError(w, refused.Code, status, refused.Message)
}
