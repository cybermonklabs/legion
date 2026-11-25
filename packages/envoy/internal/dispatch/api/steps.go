package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// An issue's step records are the record of each run of each step of its workflow, which the
// Legion daemon of the issue's project writes twice, at the run's start and at its end, under one
// id it derives for the run (PUT /api/v1/issues/{key}/steps/{id}, and PUT
// /api/v2/issues/{key}/steps/{id}), and which every reader of the project reads (GET
// /api/v1/issues/{key}/steps, and GET /api/v2/issues/{key}/steps). A retro's record also takes the
// rating the agent that did the retro gave the plan, and its reason, through a route of its own
// (PUT /api/v1/issues/{key}/steps/{id}/rating, and PUT /api/v2/issues/{key}/steps/{id}/rating),
// and the record of a run of an issue that a draw put in an experiment takes the experiment and
// the variant drawn through another (PUT /api/v1/issues/{key}/steps/{id}/experiment, and PUT
// /api/v2/issues/{key}/steps/{id}/experiment): decodeJSON ignores a member a body's route does not
// declare, so a rating or a variant sent as a member of the step write to a Dispatch older than
// its route would be dropped in silence, where a route that Dispatch does not serve answers its
// router's NOT_FOUND, which the daemon sends again until Dispatch serves it. All of them live in
// internal/dispatch/workflowstore, the one writer of the nodes and tuples that hold them.
//
// Each write names the session of the issue's project's daemon, legion-daemon:<PROJECT>, as its
// actor, or is refused 403 STEP_WRITER. That holds the session the body names, which a bearer
// bound to a session cannot make another's (403 ACTOR_SESSION_MISMATCH); a credential that proves
// no session, such as the shared agent token, a personal agent token or a service account's, can
// name the daemon's session and write, as the role-holders publish can while
// DISPATCH_LEGION_STATUS_SCOPE is off (issue_role_holders.go). Like the design gates'
// publish the writes append no issue event and notify nobody, since an event would feed the
// daemon's intake with facts it caused; and they read the issue with no open check, because the
// merge that closes an issue ends a step of it.

// The refusals of a step write, beside the package's own decode error and 404 ISSUE_NOT_FOUND,
// are each the code contracts gives it, which the Legion daemon reads to end a row on:
// apicode.StepInput, a value outside its form, or a first write of a run that does not name its
// identity, the message naming the field (workflowstore.ErrStepInput); apicode.StepWriter, a
// writer that is not the session of the issue's project's daemon; and apicode.StepOtherIssue, an
// id whose record is a run of another issue. /api/v2 answers the write that names no identity to
// an id no record has 404 STEP_NOT_FOUND instead (putIssueStepV2, putIssueStepRatingV2,
// putIssueStepExperimentV2).

// stepRecordInput is the body PUT /api/v1/issues/{key}/steps/{id} and its /api/v2 route take: the
// writer, and each field of the run the write states. A field the body leaves out, or sends null
// or "", states nothing, so the write leaves it to the write that knows it. A first write of a run
// names its task, step, kind, role and generation. returned_to 0 is place 0 and states it
// (workflowstore.StepWrite). The enum tags are the words of contracts' STEP_KINDS and
// STEP_RESULTS, which a tag cannot name; TestTheStepRouteNamesContractsWords holds them to those.
type stepRecordInput struct {
	Actor      *model.Actor `json:"actor"`
	Task       *string      `json:"task"`
	Step       *int         `json:"step"`
	Kind       *string      `json:"kind" enum:"step,retro,merge"`
	Role       *string      `json:"role"`
	Type       *string      `json:"type"`
	Agent      *string      `json:"agent"`
	Session    *string      `json:"session"`
	Model      *string      `json:"model"`
	Generation *int64       `json:"generation"`
	StartedAt  *time.Time   `json:"started_at"`
	EndedAt    *time.Time   `json:"ended_at"`
	Result     *string      `json:"result" enum:"passed,failed,stopped"`
	ReturnedTo *int         `json:"returned_to"`
	Report     *string      `json:"report"`
}

// write is the step write input states, each field as the body sent it: the fields the record
// shares with the rating route's body, which adds the rating and its reason to it, and with the
// experiment route's body, which adds the experiment and the variant.
func (input stepRecordInput) write() workflowstore.StepWrite {
	return workflowstore.StepWrite{
		Task: input.Task, Kind: input.Kind, Role: input.Role, Type: input.Type, Agent: input.Agent,
		Session: input.Session, Model: input.Model, Result: input.Result, Report: input.Report,
		Generation: input.Generation, Place: input.Step, ReturnedTo: input.ReturnedTo,
		StartedAt: input.StartedAt, EndedAt: input.EndedAt,
	}
}

// stepRatingInput is the body PUT /api/v1/issues/{key}/steps/{id}/rating and its /api/v2 route
// take: the step write's body, which makes the retro's record where its start and end have not
// landed yet, and the rating with its reason, both required.
type stepRatingInput struct {
	stepRecordInput
	Rating       *int    `json:"rating" required:"true"`
	RatingReason *string `json:"rating_reason" required:"true"`
}

// stepExperimentInput is the body PUT /api/v1/issues/{key}/steps/{id}/experiment and its /api/v2
// route take: the step write's body, which makes the run's record where its start and end have not
// landed yet, and the experiment and the variant of the run's issue, both required.
type stepExperimentInput struct {
	stepRecordInput
	Experiment *string `json:"experiment" required:"true"`
	Variant    *string `json:"variant" required:"true"`
}

// putIssueStep writes one run of one step of an issue and answers its record as it stands after
// the write, with the attempt the read gives it. A write sets each field the record does not hold
// yet and leaves every field it holds, so a start and an end land in either order and a write
// sent again changes nothing; no write is refused for its order. returned_to is set only with the
// result, by the end that reports both (workflowstore.PutStep).
func (s *server) putIssueStep(w http.ResponseWriter, r *http.Request) {
	var input stepRecordInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.putIssueStepFrom(w, r, input, apicode.StepInput, nil)
}

// putIssueStepFrom is the write both versions make of one run of one step, input its body: the
// writer held to the issue's daemon's session, then the write (writeIssueStep), answering the
// record. unmade and unchanged are writeIssueStep's: /api/v1 passes 400 STEP_INPUT and nil, and
// /api/v2 404 STEP_NOT_FOUND and its no-op rule (putIssueStepV2).
func (s *server) putIssueStepFrom(w http.ResponseWriter, r *http.Request, input stepRecordInput, unmade apicode.Code, unchanged func(workflowstore.StepRead) bool) {
	actor, key, ok := s.requireIssueDaemon(w, r, input.Actor, "a step record")
	if !ok {
		return
	}
	s.writeIssueStep(w, r, input.write(), actor, key, unmade, unchanged)
}

// putIssueStepRating writes the rating of a retro's run, with every field of the run the body
// states, as putIssueStep writes them: the rating and its reason are set once, and the record is
// made where the retro's start and end have not landed yet. The record must be a retro's.
func (s *server) putIssueStepRating(w http.ResponseWriter, r *http.Request) {
	var input stepRatingInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.putIssueStepRatingFrom(w, r, input, apicode.StepInput, nil)
}

// putIssueStepRatingFrom is the rating write both versions make, input its body, as
// putIssueStepFrom makes a step write. A body that names no rating is refused once its writer is
// checked, before the transaction and the issue's lock, as the package refuses a required field it
// lacks: the store takes a step write with no rating, so this route alone holds one required. A
// reason the body leaves out is "", which the store refuses as a rating_reason left out, after it
// has checked the write's other fields and the rating. /api/v2's decoder refuses either left out
// before the handler runs.
func (s *server) putIssueStepRatingFrom(w http.ResponseWriter, r *http.Request, input stepRatingInput, unmade apicode.Code, unchanged func(workflowstore.StepRead) bool) {
	actor, key, ok := s.requireIssueDaemon(w, r, input.Actor, "a step record")
	if !ok {
		return
	}
	if input.Rating == nil {
		writeError(w, r, apicode.StepInput, fmt.Sprintf(
			"rating is required: the rating route writes a retro's rating, %d to %d, with its rating_reason", contracts.PlanRatingLeast, contracts.PlanRatingMost))
		return
	}
	write := input.write()
	write.Rating = &workflowstore.StepRating{Rating: *input.Rating}
	if input.RatingReason != nil {
		write.Rating.Reason = *input.RatingReason
	}
	s.writeIssueStep(w, r, write, actor, key, unmade, unchanged)
}

// putIssueStepExperiment writes the experiment and the variant of a run's issue, with every field
// of the run the body states, as putIssueStep writes them: the two are set once, and the record is
// made where the run's start and end have not landed yet.
func (s *server) putIssueStepExperiment(w http.ResponseWriter, r *http.Request) {
	var input stepExperimentInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.putIssueStepExperimentFrom(w, r, input, apicode.StepInput, nil)
}

// putIssueStepExperimentFrom is the experiment write both versions make, input its body, as
// putIssueStepFrom makes a step write. A body that names neither is refused once its writer is
// checked, before the transaction and the issue's lock, as the package refuses a required field it
// lacks: the store takes a step write with no experiment, so this route alone holds the pair
// required. The one of the two the body leaves out is "", which the store refuses as one stated
// without the other, after it has checked the write's other fields.
func (s *server) putIssueStepExperimentFrom(w http.ResponseWriter, r *http.Request, input stepExperimentInput, unmade apicode.Code, unchanged func(workflowstore.StepRead) bool) {
	actor, key, ok := s.requireIssueDaemon(w, r, input.Actor, "a step record")
	if !ok {
		return
	}
	if input.Experiment == nil && input.Variant == nil {
		writeError(w, r, apicode.StepInput,
			"experiment and variant are required: the experiment route writes the experiment and the variant of the run's issue")
		return
	}
	write := input.write()
	write.Experiment = &workflowstore.StepExperiment{}
	if input.Experiment != nil {
		write.Experiment.Experiment = *input.Experiment
	}
	if input.Variant != nil {
		write.Experiment.Variant = *input.Variant
	}
	s.writeIssueStep(w, r, write, actor, key, unmade, unchanged)
}

// writeIssueStep is the one write of a step record behind every step route, by actor to the issue
// key, each of which the route has checked (requireIssueDaemon): the issue is read, write (its ID,
// Issue and Project filled here from the path and the issue row) is written, and the record is
// answered as the read lists it. A write of an id no record has that names no identity to make one
// is refused unmade: /api/v1's 400 STEP_INPUT, as any other value outside its form, and /api/v2's
// 404 STEP_NOT_FOUND. unchanged is /api/v2's no-op rule (dispatch://INFRA-4071/spec R8), asked of
// the record the issue already holds under the id, read on the pool before the write: where it
// answers true, the record is answered as it stands and nothing is written. /api/v1 passes nil,
// and writes.
func (s *server) writeIssueStep(w http.ResponseWriter, r *http.Request, write workflowstore.StepWrite, actor model.Actor, key string, unmade apicode.Code, unchanged func(workflowstore.StepRead) bool) {
	if unchanged != nil {
		if id, err := uuid.Parse(r.PathValue("id")); err == nil {
			steps, err := workflowstore.IssueSteps(r.Context(), s.deps.Store.Pool, key)
			if err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			index := slices.IndexFunc(steps, func(step workflowstore.StepRead) bool { return step.ID == id.String() })
			if index >= 0 && unchanged(steps[index]) {
				WriteJSON(w, http.StatusOK, stepRecord(steps[index]))
				return
			}
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	project, ok := s.lockRecordedIssue(w, r, tx, key)
	if !ok {
		return
	}
	write.ID, write.Issue, write.Project = r.PathValue("id"), key, &project
	written, err := workflowstore.PutStep(r.Context(), tx, write, actor, time.Now())
	switch {
	case errors.Is(err, workflowstore.ErrStepUnmade):
		writeError(w, r, unmade, err.Error())
		return
	case errors.Is(err, workflowstore.ErrStepInput):
		writeError(w, r, apicode.StepInput, err.Error())
		return
	case errors.Is(err, workflowstore.ErrStepOtherIssue):
		writeError(w, r, apicode.StepOtherIssue, err.Error())
		return
	case err != nil:
		s.writeHandlerError(w, r, err)
		return
	}
	// The attempt is the run's place among the issue's runs, which no write stores, so the
	// answer reads the issue's runs in the write's own transaction, as GET would after it. That
	// read lists the run: the write made it part of the issue, or found it part of the issue.
	steps, err := workflowstore.IssueSteps(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	index := slices.IndexFunc(steps, func(step workflowstore.StepRead) bool { return step.ID == written.ID })
	if index < 0 {
		s.writeHandlerError(w, r, fmt.Errorf("the read of %s does not list step %s, which this write holds", key, written.ID))
		return
	}
	record := stepRecord(steps[index])
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, record)
}

// requireIssueDaemon is the prelude of every record write: the caller's actor (requireActor) and
// the issue key of the path, where the actor is the writer of the records of that issue, the
// session of its project's Legion daemon (legionDaemonSessionOf). It refuses any other writer 403
// STEP_WRITER, naming what record and whose.
func (s *server) requireIssueDaemon(w http.ResponseWriter, r *http.Request, supplied *model.Actor, record string) (model.Actor, string, bool) {
	actor, ok := s.requireActor(w, r, supplied)
	if !ok {
		return model.Actor{}, "", false
	}
	key := r.PathValue("key")
	writer := legionDaemonSessionOf(key)
	if actor.Kind != "session" || actor.ID != writer {
		writeError(w, r, apicode.StepWriter, fmt.Sprintf("only the session %s writes %s of %s", writer, record, key))
		return model.Actor{}, "", false
	}
	return actor, key, true
}

// lockRecordedIssue takes the owner lock of the issue key a record is written to, shared, then
// reads the issue in a statement of its own and answers its project: a record write waits for the
// issue's writer that holds the owner and holds off the next one, and two record writes do not
// wait on each other. The store's write (workflowstore's putNode) then locks the record's own row,
// which only it writes, so the two are always taken in this order. The read has no open check, so
// the merge that closes an issue still records its step. An issue Dispatch does not hold is
// refused with a code of its own rather than loadIssue's NOT_FOUND: the daemon must tell a
// Dispatch that holds no such issue from one that does not serve the route, whose router answers
// NOT_FOUND.
func (s *server) lockRecordedIssue(w http.ResponseWriter, r *http.Request, tx pgx.Tx, key string) (string, bool) {
	if err := store.LockOwnersShared(r.Context(), tx, store.IssueOwner(key)); err != nil {
		s.writeHandlerError(w, r, err)
		return "", false
	}
	var project string
	if err := tx.QueryRow(r.Context(), `
		select project_key from issues where key = $1
	`, key).Scan(&project); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, r, apicode.IssueNotFound, fmt.Sprintf("no issue %s", key))
			return "", false
		}
		s.writeHandlerError(w, r, err)
		return "", false
	}
	return project, true
}

// listIssueSteps is every run of every step of one issue, task by task in the order of each
// task's first run: GET /api/v1/issues/{key}/steps. A closed issue's runs read as an open one's,
// and a key no issue carries answers as GET /api/v1/issues/{key} does, 404 NOT_FOUND, so a
// mistyped key never reads as an issue Legion never ran. Only a read that finds no run looks for
// the issue: the write reads the issue before it records a run (404 ISSUE_NOT_FOUND), and no issue
// is ever deleted, so a read that finds a run has found its issue.
func (s *server) listIssueSteps(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	steps, err := workflowstore.IssueSteps(r.Context(), s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if len(steps) == 0 {
		var held bool
		if err := s.deps.Store.Pool.QueryRow(r.Context(),
			`select exists(select 1 from issues where key = $1)`, key).Scan(&held); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		if !held {
			s.writeHandlerError(w, r, pgx.ErrNoRows)
			return
		}
	}
	WriteJSON(w, http.StatusOK, issueStepTasks(key, steps))
}

// issueStepTasks is the runs of issue, steps as workflowstore.IssueSteps reads them, task by task
// in the order of each task's first run: /api/v1's answer, which /api/v2 lists in that order.
func issueStepTasks(issue string, steps []workflowstore.StepRead) model.IssueSteps {
	read := model.IssueSteps{Issue: issue, Tasks: []model.StepTask{}}
	tasks := map[string]int{}
	for _, step := range steps {
		index, known := tasks[step.Task]
		if !known {
			index = len(read.Tasks)
			tasks[step.Task] = index
			read.Tasks = append(read.Tasks, model.StepTask{Task: step.Task})
		}
		read.Tasks[index].Steps = append(read.Tasks[index].Steps, stepRecord(step))
	}
	return read
}

// stepRecord is a run as the routes answer it: an absent ReturnedTo, an empty report, a nil Rating
// and a nil Experiment are a run that has none, which the answer carries as null, the rating and
// its reason both, and the experiment and the variant both; a return to place 0 carries 0.
func stepRecord(step workflowstore.StepRead) model.StepRecord {
	record := model.StepRecord{
		ID: step.ID, Issue: step.Issue, Task: step.Task, Step: step.Place, Kind: step.Kind,
		Role: step.Role, Type: step.Type, Agent: step.Agent, Session: step.Session, Model: step.Model,
		Generation: step.Generation, Attempt: step.Attempt, StartedAt: step.StartedAt,
		EndedAt: step.EndedAt, Result: step.Result, ReturnedTo: step.ReturnedTo,
	}
	if step.Report != "" {
		report := step.Report
		record.Report = &report
	}
	if step.Rating != nil {
		rating := *step.Rating
		record.Rating, record.RatingReason = &rating.Rating, &rating.Reason
	}
	if step.Experiment != nil {
		drawn := *step.Experiment
		record.Experiment, record.Variant = &drawn.Experiment, &drawn.Variant
	}
	return record
}
