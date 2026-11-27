package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// An issue's step records are the record of each run of each step of its workflow, which the
// Legion daemon writes twice, at the run's start and at its end, under one id it derives for the
// run (PUT /api/v1/issues/{key}/steps/{id}), and which every reader of the project reads
// (GET /api/v1/issues/{key}/steps). A retro's record also takes the rating the agent
// that did the retro gave the plan, and its reason, through a route of its own
// (PUT /api/v1/issues/{key}/steps/{id}/rating), and the record of a run of an issue that a draw put
// in an experiment takes the experiment and the variant drawn through another
// (PUT /api/v1/issues/{key}/steps/{id}/experiment): decodeJSON ignores a member a body's route does
// not declare, so a rating or a variant sent as a member of the step write to a Dispatch older
// than its route would be dropped in silence, where a route that Dispatch does not serve answers
// its router's NOT_FOUND, which the daemon sends again until Dispatch serves it. All four live in
// internal/dispatch/workflowstore, the one writer of the nodes and tuples that hold them.
//
// Each write names as its actor legion-daemon:<PROJECT>, the session of the issue's project's
// daemon, or the session of another project's daemon that the request's broker token proves
// (isIssueDaemon). Dispatch refuses any other writer 403 STEP_WRITER. That holds the session
// the body names, which a bearer bound to a session cannot make another's (403
// ACTOR_SESSION_MISMATCH); a credential that proves no session, such as the shared agent token, a
// personal agent token or a service account's, can name the session of the issue's project's
// daemon and write, as the role-holders publish can while DISPATCH_LEGION_STATUS_SCOPE is off
// (issue_role_holders.go). Like the design gates'
// publish the writes append no issue event and notify nobody, since an event would feed the
// daemon's intake with facts it caused; and they read the issue with no open check, because the
// merge that closes an issue ends a step of it.

// The refusals of a step write, beside the package's own decode error and 404 ISSUE_NOT_FOUND,
// each the code contracts gives it, which the Legion daemon reads to end a row on.
const (
	// codeStepInput is a value outside its form, or a first write of a run that does not name
	// its identity; the message names the field (workflowstore.ErrStepInput).
	codeStepInput = contracts.StepInputCode
	// codeStepWriter is a writer that is not a daemon that writes the issue's records (isIssueDaemon).
	codeStepWriter = contracts.StepWriterCode
	// codeStepOtherIssue is an id whose record is a run of another issue.
	codeStepOtherIssue = contracts.StepOtherIssueCode
)

// stepRecordInput is the body PUT /api/v1/issues/{key}/steps/{id} takes: the writer, and each
// field of the run the write states. A field the body leaves out, or sends null or "", states
// nothing, so the write leaves it to the write that knows it. A first write of a run names its
// task, step, kind, role and generation. returned_to 0 is place 0 and states it
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

// stepRatingInput is the body PUT /api/v1/issues/{key}/steps/{id}/rating takes: the step write's
// body, which makes the retro's record where its start and end have not landed yet, and the
// rating with its reason, both required.
type stepRatingInput struct {
	stepRecordInput
	Rating       *int    `json:"rating" required:"true"`
	RatingReason *string `json:"rating_reason" required:"true"`
}

// stepExperimentInput is the body PUT /api/v1/issues/{key}/steps/{id}/experiment takes: the step
// write's body, which makes the run's record where its start and end have not landed yet, and the
// experiment and the variant of the run's issue, both required.
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
	actor, key, ok := s.requireIssueDaemon(w, r, input.Actor, "a step record")
	if !ok {
		return
	}
	s.writeIssueStep(w, r, input.write(), actor, key)
}

// putIssueStepRating writes the rating of a retro's run, with every field of the run the body
// states, as putIssueStep writes them: the rating and its reason are set once, and the record is
// made where the retro's start and end have not landed yet. The record must be a retro's. A body
// that names no rating is refused once its writer is checked, before the transaction and the
// issue's lock, as the package refuses a required field it lacks: the store takes a step write
// with no rating, so this route alone holds one required. A reason the body leaves out is "",
// which the store refuses as a rating_reason left out, after it has checked the write's other
// fields and the rating.
func (s *server) putIssueStepRating(w http.ResponseWriter, r *http.Request) {
	var input stepRatingInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	actor, key, ok := s.requireIssueDaemon(w, r, input.Actor, "a step record")
	if !ok {
		return
	}
	if input.Rating == nil {
		writeError(w, codeStepInput, http.StatusBadRequest, fmt.Sprintf(
			"rating is required: the rating route writes a retro's rating, %d to %d, with its rating_reason", contracts.PlanRatingLeast, contracts.PlanRatingMost))
		return
	}
	write := input.write()
	write.Rating = &workflowstore.StepRating{Rating: *input.Rating}
	if input.RatingReason != nil {
		write.Rating.Reason = *input.RatingReason
	}
	s.writeIssueStep(w, r, write, actor, key)
}

// putIssueStepExperiment writes the experiment and the variant of a run's issue, with every field
// of the run the body states, as putIssueStep writes them: the two are set once, and the record is
// made where the run's start and end have not landed yet. A body that names neither is refused
// once its writer is checked, before the transaction and the issue's lock, as the package refuses
// a required field it lacks: the store takes a step write with no experiment, so this route alone
// holds the pair required. The one of the two the body leaves out is "", which the store refuses
// as one stated without the other, after it has checked the write's other fields.
func (s *server) putIssueStepExperiment(w http.ResponseWriter, r *http.Request) {
	var input stepExperimentInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	actor, key, ok := s.requireIssueDaemon(w, r, input.Actor, "a step record")
	if !ok {
		return
	}
	if input.Experiment == nil && input.Variant == nil {
		writeError(w, codeStepInput, http.StatusBadRequest,
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
	s.writeIssueStep(w, r, write, actor, key)
}

// writeIssueStep is the one write of a step record behind the three routes, by actor to the issue
// key, each of which the route has checked (requireIssueDaemon): the issue is read, write (its ID,
// Issue and Project filled here from the path and the issue row) is written, and the record is
// answered as the read lists it.
func (s *server) writeIssueStep(w http.ResponseWriter, r *http.Request, write workflowstore.StepWrite, actor model.Actor, key string) {
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
	case errors.Is(err, workflowstore.ErrStepInput):
		writeError(w, codeStepInput, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, workflowstore.ErrStepOtherIssue):
		writeError(w, codeStepOtherIssue, http.StatusConflict, err.Error())
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
// the issue key of the path, where the actor is a writer of the records of that issue
// (isIssueDaemon). That writer is the session of its project's Legion daemon, or the session of
// another project's daemon that the request's broker token proves. It refuses any other writer
// 403 STEP_WRITER, naming what record and whose.
func (s *server) requireIssueDaemon(w http.ResponseWriter, r *http.Request, supplied *model.Actor, record string) (model.Actor, string, bool) {
	actor, ok := s.requireActor(w, r, supplied)
	if !ok {
		return model.Actor{}, "", false
	}
	key := r.PathValue("key")
	if !s.isIssueDaemon(r, actor, key) {
		writeError(w, codeStepWriter, http.StatusForbidden, fmt.Sprintf(
			"only a Legion daemon writes %s of %s: the session %s, or the session legion-daemon:<P> of another project's daemon whose broker token names P and proves that session",
			record, key, legionDaemonSessionOf(key)))
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
			writeError(w, "ISSUE_NOT_FOUND", http.StatusNotFound, fmt.Sprintf("no issue %s", key))
			return "", false
		}
		s.writeHandlerError(w, r, err)
		return "", false
	}
	return project, true
}

// listIssueSteps is GET /api/v1/issues/{key}/steps: the tasks of the issue's newest plan
// (workflowstore.NewestPlan), in the plan's order, each with the needs its node records (null
// where the node recorded none), the steps the plan gives it and every run of them ([] where none
// ran); then every other task with a run, in the order of each such task's first run. It reads the
// plan node and the runs alone, and no document. A closed issue's runs read as an open one's, and a
// key no issue carries answers as GET /api/v1/issues/{key} does, 404 NOT_FOUND, so a mistyped key
// never reads as an issue Legion never ran. Only a read that finds no run looks for the issue: the
// write reads the issue before it records a run (404 ISSUE_NOT_FOUND), and no issue is ever
// deleted, so a read that finds a run has found its issue.
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
	plan, err := workflowstore.NewestPlan(r.Context(), s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	read := model.IssueSteps{Issue: key, Tasks: []model.StepTask{}}
	tasks := map[string]int{}
	if plan != nil {
		// A planned step stands where the newest run of it stands among the records of the plan's
		// own run of the issue (its generation): the runs come in the order they happened. A run is
		// of the step only where it stands at the step's place with the step's role: a later version
		// of the plan in force can put another step at a place where an earlier step already ran.
		// A run names no agent key, so a step `role:key` is matched by its role, which this release
		// names at most once in a task. Its state is a word of contracts' PLANNED_STEP_STATES, which
		// PlannedStep.State's enum tag holds (TestTheStepRouteNamesContractsWords).
		type stepOf struct {
			task string
			step int
			role string
		}
		newest := map[stepOf]string{}
		for _, step := range steps {
			if step.Generation == plan.Generation && step.Kind == workflowstore.StepKindStep {
				newest[stepOf{step.Task, step.Place, step.Role}] = step.Result
			}
		}
		for _, task := range plan.Tasks {
			tasks[task.ID] = len(read.Tasks)
			planned := model.StepTask{Task: task.ID, Planned: make([]model.PlannedStep, len(task.Steps)), Steps: []model.StepRecord{}}
			for index, name := range task.Steps {
				role, _, _ := strings.Cut(name, ":")
				state := contracts.PlannedStepNotStarted
				if result, ran := newest[stepOf{task.ID, index + 1, role}]; ran {
					state = cmp.Or(result, contracts.PlannedStepRunning)
				}
				planned.Planned[index] = model.PlannedStep{Step: index + 1, Name: name, State: state}
			}
			if task.Needs != nil {
				needs := append([]string{}, (*task.Needs)...)
				planned.Needs = &needs
			}
			read.Tasks = append(read.Tasks, planned)
		}
	}
	for _, step := range steps {
		index, known := tasks[step.Task]
		if !known {
			index = len(read.Tasks)
			tasks[step.Task] = index
			read.Tasks = append(read.Tasks, model.StepTask{Task: step.Task})
		}
		read.Tasks[index].Steps = append(read.Tasks[index].Steps, stepRecord(step))
	}
	WriteJSON(w, http.StatusOK, read)
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
