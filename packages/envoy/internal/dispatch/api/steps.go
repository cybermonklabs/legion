package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// An issue's step records are the record of each run of each step of its workflow, which the
// Legion daemon of the issue's project writes twice, at the run's start and at its end, under one
// id it derives for the run (PUT /api/v1/issues/{key}/steps/{id}), and which every reader of the
// project reads (GET /api/v1/issues/{key}/steps). Both live in internal/dispatch/workflowstore,
// the one writer of the nodes and tuples that hold them.
//
// A write names the session of the issue's project's daemon, legion-daemon:<PROJECT>, as its actor,
// or is refused 403 STEP_WRITER. That holds the session the body names, which a bearer bound to a
// session cannot make another's (403 ACTOR_SESSION_MISMATCH); a credential that proves no session,
// such as the shared agent token, a personal agent token or a service account's, can name the
// daemon's session and write, as the role-holders publish can. Like the design gates' publish it
// appends no issue event and notifies nobody, since an event would feed the daemon's intake with
// facts it caused; and it reads the issue with no open check, because the merge that closes an
// issue ends a step of it. Both read the issue as the request's caller sees it (access.ProjectClause)
// before anything else of it: an issue of a project the caller does not see is a key no issue
// holds, answered 404 before the writer is judged, whatever session the body names.

// The refusals of a step write, beside the package's own decode error and 404 ISSUE_NOT_FOUND,
// each the code contracts gives it, which the Legion daemon reads to end a row on.
const (
	// codeStepInput is a value outside its form, or a first write of a run that does not name
	// its identity; the message names the field (workflowstore.ErrStepInput).
	codeStepInput = contracts.StepInputCode
	// codeStepWriter is a writer that is not the session of the issue's project's daemon.
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

// putIssueStep writes one run of one step of an issue and answers its record as it stands after
// the write, with the attempt the read gives it. A write sets each field the record does not hold
// yet and leaves every field it holds, so a start and an end land in either order and a write
// sent again changes nothing; no write is refused for its order. returned_to is set only with the
// result, by the end that reports both (workflowstore.PutStep).
func (s *server) putIssueStep(w http.ResponseWriter, r *http.Request) {
	var input stepRecordInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The issue's owner lock, shared, then its read in a statement of its own: a step write waits for
	// the issue's writer that holds the owner and holds off the next one, and two step writes do not
	// wait on each other. workflowstore.PutStep then locks the run's own row, which only it writes,
	// so the two are always taken in this order. The read has no open check, so the merge that closes
	// an issue still records its step. A missing issue, or one the caller does not see, is refused
	// with a code of its own rather than loadIssue's NOT_FOUND: the daemon must tell a Dispatch that
	// holds no such issue from one that does not serve this route, whose router answers NOT_FOUND.
	if err := store.LockOwnersShared(r.Context(), tx, store.IssueOwner(key)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var issue, issueProject string
	if err := tx.QueryRow(r.Context(), `
		select key, project_key from issues where key = $1 and `+access.ProjectClause("project_key", "$2")+`
	`, key, refs.VisibilityArg(who)).Scan(&issue, &issueProject); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "ISSUE_NOT_FOUND", http.StatusNotFound, fmt.Sprintf("no issue %s", key))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	writer := legionDaemonSessionOf(key)
	if actor.Kind != "session" || actor.ID != writer {
		writeError(w, codeStepWriter, http.StatusForbidden,
			fmt.Sprintf("only the session %s writes a step record of %s", writer, key))
		return
	}
	written, err := workflowstore.PutStep(r.Context(), tx, workflowstore.StepWrite{
		ID: r.PathValue("id"), Issue: issue, Project: &issueProject,
		Task: input.Task, Kind: input.Kind, Role: input.Role, Type: input.Type, Agent: input.Agent,
		Session: input.Session, Model: input.Model, Result: input.Result, Report: input.Report,
		Generation: input.Generation, Place: input.Step, ReturnedTo: input.ReturnedTo,
		StartedAt: input.StartedAt, EndedAt: input.EndedAt,
	}, actor, time.Now())
	switch {
	case errors.Is(err, workflowstore.ErrStepInput):
		writeError(w, codeStepInput, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, workflowstore.ErrStepOtherIssue):
		writeError(w, codeStepOtherIssue, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.writeHandlerError(w, err)
		return
	}
	// The attempt is the run's place among the issue's runs, which no write stores, so the
	// answer reads the issue's runs in the write's own transaction, as GET would after it. That
	// read lists the run: the write made it part of the issue, or found it part of the issue.
	steps, err := workflowstore.IssueSteps(r.Context(), tx, issue)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	index := slices.IndexFunc(steps, func(step workflowstore.StepRead) bool { return step.ID == written.ID })
	if index < 0 {
		s.writeHandlerError(w, fmt.Errorf("the read of %s does not list step %s, which this write holds", issue, written.ID))
		return
	}
	record := stepRecord(steps[index])
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, record)
}

// listIssueSteps is every run of every step of one issue, task by task in the order of each
// task's first run: GET /api/v1/issues/{key}/steps. A closed issue's runs read as an open one's,
// and a key no issue carries answers as GET /api/v1/issues/{key} does, 404 NOT_FOUND, so a
// mistyped key never reads as an issue Legion never ran; so does an issue of a project the caller
// does not see, which is why the issue is read before its runs, every time.
func (s *server) listIssueSteps(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	// The issue first, as the caller sees it: a key no issue carries, or an issue of a project the
	// caller does not see, is 404 NOT_FOUND before any run of it is read.
	var seen bool
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select exists (select 1 from issues where key = $1 and `+
		access.ProjectClause("project_key", "$2")+`)`, key, refs.VisibilityArg(who)).Scan(&seen); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !seen {
		s.writeHandlerError(w, pgx.ErrNoRows)
		return
	}
	steps, err := workflowstore.IssueSteps(r.Context(), s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.IssueSteps{Issue: key, Tasks: []model.StepTask{}}
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
	WriteJSON(w, http.StatusOK, read)
}

// stepRecord is a run as the routes answer it: an absent ReturnedTo and an empty report are a
// run that has none, which the answer carries as null; a return to place 0 carries 0.
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
	return record
}
