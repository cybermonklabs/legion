package api

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// The plan each run of an issue ran from, which the Legion daemon of the issue's project writes
// when the run's work starts and again at each later version of the owner's plan that Legion takes
// (PUT /api/v1/issues/{key}/plans/{id}), and the read of a project's finished issues
// (GET /api/v1/projects/{key}/plans): each with its plan, the rating each retro gave it with the
// reason, the time from the first start of its step records to the last end, and its count of
// failed steps. Both live in internal/dispatch/workflowstore, the one writer of the nodes and
// tuples that hold the plans and the step records.
//
// The write is the daemon's alone, as a step record's is (403 STEP_WRITER), appends no issue
// event and reads the issue with no open check. The read writes nothing, reads the project, then
// all its finished issues in one statement whatever their number, and is a read of the read
// class (readClasses), which a read-only token may call.

// The refusals of a plan write, beside the step write's STEP_WRITER, the package's own decode
// error and 404 ISSUE_NOT_FOUND, each the code contracts gives it, which the Legion daemon reads to
// end a row on.
const (
	// codePlanInput is a plan value outside its form, or a plan that does not name its
	// generation, its source and its tasks; the message names the field
	// (workflowstore.ErrPlanInput).
	codePlanInput = contracts.PlanInputCode
	// codePlanOtherIssue is an id whose plan is the plan of another issue.
	codePlanOtherIssue = contracts.PlanOtherIssueCode
)

// planRecordInput is the body PUT /api/v1/issues/{key}/plans/{id} takes: the writer, the run the
// plan is of, its source, the owner's document and version for the source owner, and its tasks.
type planRecordInput struct {
	Actor      *model.Actor    `json:"actor"`
	Generation *int64          `json:"generation" required:"true"`
	Source     *string         `json:"source" enum:"default,owner" required:"true"`
	ArtifactID *string         `json:"artifact_id"`
	Version    *int            `json:"version"`
	Tasks      []planTaskInput `json:"tasks" required:"true"`
}

// planTaskInput is one task of a written plan: its id, its steps in order, each a role or
// `role:key`, and the tasks of the plan it needs, each by its id, [] for a task that needs none. A
// write with no needs, a daemon's of the release before, records the task's needs as not recorded,
// as does a write a Dispatch of the release before took, since it ignores the member.
//
// So needs is an exception to decodeJSON's rule that a member whose loss changes what a request
// does ships as a new route: a Dispatch of the release before drops it, and a later write at the
// same version cannot put it back (workflowstore.PutPlan). It ships as a member because the loss
// changes only what the steps read shows, never what Legion runs: the daemon, the one writer of
// this route, reads no need back from Dispatch. And the release that adds it deploys Dispatch
// before the daemon (docs/kubernetes.md, "Rolling back the release that records each task's
// needs"), so a daemon that writes needs reaches a Dispatch that drops them only when that order
// is not kept, and then each such plan reads its needs as not recorded.
//
// It has the fields of workflowstore.PlanTask and model.PlanTask, so a task converts between the
// three, and a field added to one of them alone fails the build rather than being dropped on the
// way.
type planTaskInput struct {
	ID    string    `json:"id" required:"true"`
	Steps []string  `json:"steps" required:"true"`
	Needs *[]string `json:"needs"`
}

// putIssuePlan writes the plan of one run of an issue and answers it as it stands after the
// write. A write of a version above the one the plan holds replaces its source, document, version
// and tasks, and every other write changes nothing (workflowstore.PutPlan).
func (s *server) putIssuePlan(w http.ResponseWriter, r *http.Request) {
	var input planRecordInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, key, ok := s.requireIssueDaemon(w, r, input.Actor, "a plan")
	if !ok {
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	project, ok := s.lockRecordedIssue(w, r, tx, key)
	if !ok {
		return
	}
	// A body with no tasks stays nil, which the store refuses as a list left out (tasks is
	// required), apart from an empty list, which it refuses as a plan with no task.
	var tasks []workflowstore.PlanTask
	if input.Tasks != nil {
		tasks = make([]workflowstore.PlanTask, len(input.Tasks))
		for index, task := range input.Tasks {
			tasks[index] = workflowstore.PlanTask(task)
		}
	}
	written, err := workflowstore.PutPlan(r.Context(), tx, workflowstore.PlanWrite{
		ID: r.PathValue("id"), Issue: key, Project: &project, Source: input.Source,
		Generation: input.Generation, Document: input.document(), Tasks: tasks,
	}, actor, time.Now())
	switch {
	case errors.Is(err, workflowstore.ErrPlanInput):
		writeError(w, codePlanInput, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, workflowstore.ErrPlanOtherIssue):
		writeError(w, codePlanOtherIssue, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, planRecord(written))
}

// document is the owner's document input states as the store's one value: each half the body
// leaves out, or sends null, "" or 0, is "" or 0, which states nothing. The store refuses a half an
// owner's plan leaves out, and a half a default plan names, naming it, after it has checked the
// plan's generation and source.
func (input planRecordInput) document() workflowstore.PlanDocument {
	var document workflowstore.PlanDocument
	if input.ArtifactID != nil {
		document.ArtifactID = *input.ArtifactID
	}
	if input.Version != nil {
		document.Version = *input.Version
	}
	return document
}

// listProjectPlans is each finished issue of one project, newest close first:
// GET /api/v1/projects/{key}/plans, its close bounded by ?since= and ?until=.
func (s *server) listProjectPlans(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	window, err := parsePlansWindow(r.URL.Query())
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	key := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	finished, err := workflowstore.ProjectPlans(r.Context(), s.deps.Store.Pool, key, window)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.ProjectPlans{Project: key, Issues: make([]model.FinishedIssue, len(finished))}
	for index, issue := range finished {
		listed := model.FinishedIssue{
			Issue: issue.Issue, Title: issue.Title, ClosedAt: issue.ClosedAt,
			Ratings:   make([]model.PlanRating, len(issue.Ratings)),
			StartedAt: issue.StartedAt, EndedAt: issue.EndedAt, FailedSteps: issue.FailedSteps,
		}
		if issue.Plan != nil {
			plan := planRecord(*issue.Plan)
			listed.Plan = &plan
		}
		for place, rating := range issue.Ratings {
			listed.Ratings[place] = model.PlanRating(rating)
		}
		// The span is known only where the latest end is not before the earliest start: a run whose
		// start Dispatch never got that ended before a later run started, whose end has not landed,
		// holds no time to count.
		if issue.StartedAt != nil && issue.EndedAt != nil && !issue.EndedAt.Before(*issue.StartedAt) {
			seconds := int64(issue.EndedAt.Sub(*issue.StartedAt) / time.Second)
			listed.DurationSeconds = &seconds
		}
		read.Issues[index] = listed
	}
	WriteJSON(w, http.StatusOK, read)
}

// parsePlansWindow is the plans read's [since, until) window on the close (parseWindow); the read
// rounds each bound up to the microsecond it compares.
func parsePlansWindow(query url.Values) (workflowstore.Window, error) {
	since, until, err := parseWindow(windowBound{name: "since", text: query.Get("since")}, windowBound{name: "until", text: query.Get("until")})
	return workflowstore.Window{Since: since, Until: until}, err
}

// planRecord is a plan as the routes answer it: the owner's document and version are null for a
// plan of the source default, which has neither, and its tasks are never null.
func planRecord(plan workflowstore.Plan) model.PlanRecord {
	record := model.PlanRecord{
		ID: plan.ID, Issue: plan.Issue, Generation: plan.Generation, Source: plan.Source,
		Tasks: make([]model.PlanTask, len(plan.Tasks)),
	}
	if plan.Document != nil {
		document := *plan.Document
		record.ArtifactID, record.Version = &document.ArtifactID, &document.Version
	}
	for index, task := range plan.Tasks {
		record.Tasks[index] = model.PlanTask(task)
	}
	return record
}
