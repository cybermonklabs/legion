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
//     Legion daemon names as its registered controller (legion.ControllerSession). The precedent
//     for who may write a state and what the row keeps is POST /api/v1/results/{id}/retract.
//
// The route class is scopeProject, which reads the path's key alone, so the state route holds the
// row to that project itself: a row of another project is 404 PRODUCTION_EVENT_NOT_FOUND.

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

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

const (
	// codeProductionEventDeployForbidden refuses a deploy posted by a person's credential: a
	// deploy is a system's event.
	codeProductionEventDeployForbidden = "PRODUCTION_EVENT_DEPLOY_FORBIDDEN"
	// codeProductionEventStateForbidden refuses a state write by any caller but a person and, for
	// a reject or a re-count, the project's controller.
	codeProductionEventStateForbidden = "PRODUCTION_EVENT_STATE_FORBIDDEN"
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
	// a deploy needs; a failure carries no FinishedAt, since it counts on its deploy's day.
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	// DetectedAt is when a failure was found, the server's time of arrival where it names none.
	DetectedAt *time.Time `json:"detected_at"`
	// Repo is the deploy's repository, and the repository of a failure's PipelineRunID.
	Repo string `json:"repo"`
	// Note is what the writer says of the event, which the record keeps as sent (at most
	// productionEventNoteBytes).
	Note string `json:"note"`
	// Commits are the commits a deploy shipped, compared in lower case.
	Commits []string `json:"commits"`
	// PullRequests are the pull requests a deploy shipped or a failure blames, owner/repo#n.
	PullRequests []string `json:"pull_requests"`
	// DeployID names a failure's deploy by its row's id.
	DeployID *string `json:"deploy_id"`
	// CommitSHA names a failure's deploy by a commit: the first deploy of any project that
	// shipped it.
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
// event's source_ref, writing nothing.
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
	if event.Kind == prodevents.KindFailure {
		deploy, err := s.blamedDeploy(r.Context(), input)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		event.DeployID = &deploy
		if person {
			event.SourceRef = prodevents.PersonSourceRef(deploy, actor.ID)
			if supplied := strings.TrimSpace(input.SourceRef); supplied != "" && supplied != event.SourceRef {
				writeError(w, prodevents.CodeInput, http.StatusBadRequest, fmt.Sprintf(
					"a person's flag is keyed by the server, %s, so a second flag of one deploy writes nothing; send no source_ref, or that one",
					event.SourceRef))
				return
			}
		}
	}
	stored, created, err := prodevents.Write(r.Context(), s.deps.Store.Pool, event)
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
// the fields of a failure's deploy refused on a deploy, a failure's signal and source the caller's
// (person or api), and its detected_at the server's time where the body names none.
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
	event := prodevents.Event{
		Kind: input.Kind, Source: prodevents.SourceAPI, Environment: environment, Status: status, SourceRef: input.SourceRef,
		StartedAt: input.StartedAt, FinishedAt: input.FinishedAt, Repo: input.Repo, Commits: commits,
		PullRequests: input.PullRequests, URL: link, Note: input.Note,
	}
	if len(input.Note) > productionEventNoteBytes {
		return prodevents.Event{}, productionEventInputError("note holds %d bytes, past the %d a note may hold", len(input.Note), productionEventNoteBytes)
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
	detected := time.Now().UTC()
	if input.DetectedAt != nil {
		detected = *input.DetectedAt
	}
	event.DetectedAt = &detected
	return event, nil
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

// blamedDeploy is the id of the deploy a failure blames, named exactly one way: its row's id, in
// its canonical spelling where it is a uuid, so a person's flag keys one deploy one way
// (person:<deploy id>:<login>) however the body spells it, and which the store holds to a deploy
// row as it writes (409 PRODUCTION_EVENT_NO_DEPLOY otherwise, a body's id that is no uuid
// included); the GitHub run it was (prodevents.DeployForRun); or a commit it shipped
// (prodevents.DeployForCommit). A run or a commit no deploy row holds is 409
// PRODUCTION_EVENT_NO_DEPLOY, and the deploy may be another project's.
func (s *server) blamedDeploy(ctx context.Context, input productionEventInput) (string, error) {
	named := 0
	for _, set := range []bool{input.DeployID != nil, input.PipelineRunID != "", input.CommitSHA != ""} {
		if set {
			named++
		}
	}
	switch {
	case named == 0:
		return "", productionEventInputError("a failure names the deploy it blames: deploy_id, cicd.pipeline.run.id with repo, or commit_sha")
	case named > 1:
		return "", productionEventInputError("a failure names the deploy it blames one way: deploy_id, cicd.pipeline.run.id or commit_sha, not two of them")
	case input.DeployID != nil:
		if id, err := uuid.Parse(*input.DeployID); err == nil {
			return id.String(), nil
		}
		return *input.DeployID, nil
	case input.PipelineRunID != "":
		run, err := strconv.ParseInt(input.PipelineRunID, 10, 64)
		if err != nil || run <= 0 {
			return "", productionEventInputError("cicd.pipeline.run.id %q is not a GitHub run id, a positive integer", input.PipelineRunID)
		}
		if strings.TrimSpace(input.Repo) == "" {
			return "", productionEventInputError("cicd.pipeline.run.id names a run of repo: name repo too")
		}
		deploy, found, err := prodevents.DeployForRun(ctx, s.deps.Store.Pool, input.Repo, run)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errorf(http.StatusConflict, prodevents.CodeNoDeploy, "no deploy row is run %d of %s", run, input.Repo)
		}
		return deploy.ID, nil
	}
	deploy, found, err := prodevents.DeployForCommit(ctx, s.deps.Store.Pool, input.CommitSHA)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errorf(http.StatusConflict, prodevents.CodeNoDeploy, "no deploy row holds commit %s", input.CommitSHA)
	}
	return deploy.ID, nil
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
// restoring deploy where the body names one.
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
// caller 403 PRODUCTION_EVENT_STATE_FORBIDDEN. A caller holding the person scope writes every
// state, as the person it acts for (actingPerson). A reject and a re-count are also the
// controller's: a caller whose credential proved its session (callerSession), where that session
// is the one the project's daemon names as its controller (legion.ControllerSession), recorded as
// that session. Each question is asked before the store opens its transaction.
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
	if proven {
		controller, named, err := legion.ControllerSession(r.Context(), s.deps.Store.Pool, project)
		if err != nil {
			s.writeHandlerError(w, err)
			return model.Actor{}, false
		}
		if named && controller == session {
			return s.requireActor(w, r, nil)
		}
		writeError(w, codeProductionEventStateForbidden, http.StatusForbidden, fmt.Sprintf(
			"a %s is a person's (the person scope) or the controller's of %s, and session %s is not the session %s's Legion daemon names as its controller",
			state, project, session, project))
		return model.Actor{}, false
	}
	writeError(w, codeProductionEventStateForbidden, http.StatusForbidden, fmt.Sprintf(
		"a %s is a person's (the person scope) or the controller's of %s, proven by the broker-issued token of the session %s's Legion daemon names as its controller; this request's credential proves neither",
		state, project, project))
	return model.Actor{}, false
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
