package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// The plan each run of an issue ran from, which the Legion daemon of the issue's project writes
// when the run's work starts (PUT /api/v1/issues/{key}/plans/{id}), and the read of a project's
// finished issues (GET /api/v1/projects/{key}/plans): each with its plan, the rating each retro
// gave it with the reason, the time from the first start of its step records to the last end,
// and its count of failed steps. Both live in internal/dispatch/workflowstore, the one writer of
// the nodes and tuples that hold the plans and the step records.
//
// The write is the daemon's alone, as a step record's is (403 STEP_WRITER), appends no issue
// event and reads the issue with no open check. The read writes nothing, is one statement for the
// whole project whatever the number of its issues, and a read-only token may call it
// (readScopeRoutes).

// The refusals of a plan write, beside the step write's STEP_WRITER, the package's own decode
// error and 404 ISSUE_NOT_FOUND.
const (
	// codePlanInput is a plan value outside its form, or a plan that does not name its
	// generation, its source and its tasks; the message names the field
	// (workflowstore.ErrPlanInput).
	codePlanInput = "PLAN_INPUT"
	// codePlanOtherIssue is an id whose plan is the plan of another issue.
	codePlanOtherIssue = "PLAN_OTHER_ISSUE"
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

// planTaskInput is one task of a written plan: its id and its steps in order, each a role or
// `role:key`.
type planTaskInput struct {
	ID    string   `json:"id" required:"true"`
	Steps []string `json:"steps" required:"true"`
}

// putIssuePlan writes the plan of one run of an issue and answers it as it stands after the
// write. Each field is set once, so a plan sent again changes nothing.
func (s *server) putIssuePlan(w http.ResponseWriter, r *http.Request) {
	var input planRecordInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if !requireIssueDaemon(w, actor, key, "a plan") {
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	issue, project, ok := s.lockRecordedIssue(w, r, tx, key)
	if !ok {
		return
	}
	var tasks []workflowstore.PlanTask
	if input.Tasks != nil {
		tasks = make([]workflowstore.PlanTask, len(input.Tasks))
		for index, task := range input.Tasks {
			tasks[index] = workflowstore.PlanTask{ID: task.ID, Steps: task.Steps}
		}
	}
	written, err := workflowstore.PutPlan(r.Context(), tx, workflowstore.PlanWrite{
		ID: r.PathValue("id"), Issue: issue, Project: &project, Source: input.Source,
		ArtifactID: input.ArtifactID, Generation: input.Generation, Version: input.Version, Tasks: tasks,
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

// listProjectPlans is each finished issue of one project, newest close first:
// GET /api/v1/projects/{key}/plans, its close bounded by ?since= and ?until=.
func (s *server) listProjectPlans(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	since, until, err := parsePlansWindow(r.URL.Query())
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	key := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	finished, err := workflowstore.ProjectPlans(r.Context(), s.deps.Store.Pool, key, since, until)
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
			listed.Ratings[place] = model.PlanRating{Rating: rating.Rating, Reason: rating.Reason, StepID: rating.StepID, EndedAt: rating.EndedAt}
		}
		if issue.StartedAt != nil && issue.EndedAt != nil {
			seconds := int64(issue.EndedAt.Sub(*issue.StartedAt) / time.Second)
			listed.DurationSeconds = &seconds
		}
		read.Issues[index] = listed
	}
	WriteJSON(w, http.StatusOK, read)
}

// parsePlansWindow is the plans read's [since, until) window on the close: each bound an RFC 3339
// time, nil when the query leaves it out or blank, and since before until when both are given. Its
// error is the text of the route's 400 INVALID_QUERY.
func parsePlansWindow(query url.Values) (since, until *time.Time, err error) {
	bounds := [2]*time.Time{}
	for index, bound := range []string{"since", "until"} {
		text := strings.TrimSpace(query.Get(bound))
		if text == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, nil, fmt.Errorf("%s must be an RFC 3339 time, such as 2026-11-01T00:00:00Z", bound)
		}
		bounds[index] = &parsed
	}
	if bounds[0] != nil && bounds[1] != nil && !bounds[0].Before(*bounds[1]) {
		return nil, nil, errors.New("since must be before until")
	}
	return bounds[0], bounds[1], nil
}

// planRecord is a plan as the routes answer it: the owner's document and version are null for a
// plan of the source default, which has neither, and its tasks are never null.
func planRecord(plan workflowstore.Plan) model.PlanRecord {
	record := model.PlanRecord{
		ID: plan.ID, Issue: plan.Issue, Generation: plan.Generation, Source: plan.Source,
		Tasks: make([]model.PlanTask, len(plan.Tasks)),
	}
	if plan.ArtifactID != "" {
		artifact := plan.ArtifactID
		record.ArtifactID = &artifact
	}
	if plan.Version != 0 {
		version := plan.Version
		record.Version = &version
	}
	for index, task := range plan.Tasks {
		record.Tasks[index] = model.PlanTask{ID: task.ID, Steps: task.Steps}
	}
	return record
}
