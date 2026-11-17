package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

// An issue's step records are the record of each run of each step of its workflow, which the
// Legion daemon of the issue's project writes twice, at the run's start and at its end, under one
// id it derives for the run (PUT /api/v1/issues/{key}/steps/{id}), and which every reader of the
// project reads (GET /api/v1/issues/{key}/steps). Both live in internal/dispatch/workflowstore,
// the one writer of the nodes and tuples that hold them.
//
// The write is the daemon's and no one else's: a person, an agent's session and the daemon of
// another project are refused 403 STEP_WRITER, so a record says what that deployment's daemon
// saw. Like the design gates' publish it appends no issue event and notifies nobody, since an
// event would feed the daemon's intake with facts it caused; and it reads the issue with no open
// check, because the merge that closes an issue ends a step of it.

// The refusals of a step write, beside the package's own decode error and 404 ISSUE_NOT_FOUND.
const (
	// codeStepInput is a value outside its form, or a first write of a run that does not name
	// its identity; the message names the field (workflowstore.ErrStepInput).
	codeStepInput = "STEP_INPUT"
	// codeStepWriter is a writer that is not the session of the issue's project's daemon.
	codeStepWriter = "STEP_WRITER"
	// codeStepOtherIssue is an id whose record is a run of another issue.
	codeStepOtherIssue = "STEP_OTHER_ISSUE"
)

// stepRecordInput is the body PUT /api/v1/issues/{key}/steps/{id} takes: the writer, and each
// field of the run the write states. A field the body leaves out, or sends null, "" or (for
// returned_to) 0, states nothing, so the write leaves it to the write that knows it. A first
// write of a run names its task, step, kind, role and generation.
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
// sent again changes nothing; no write is refused for its order.
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
	key := r.PathValue("key")
	// The rule of the daemon's own DaemonSession (packages/daemon/internal/dispatch/client.go):
	// the session is legion-daemon: and the part of the key before its first hyphen.
	project, _, _ := strings.Cut(key, "-")
	writer := legionDaemonSessionPrefix + project
	if actor.Kind != "session" || actor.ID != writer {
		writeError(w, codeStepWriter, http.StatusForbidden,
			fmt.Sprintf("only the session %s writes a step record of %s", writer, key))
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The issue is read under the key-share lock and with no open check, as the design gates'
	// publish reads it, and refused with a code of its own rather than loadIssue's NOT_FOUND: the
	// daemon must tell a Dispatch that holds no such issue from one that does not serve this
	// route, whose router answers NOT_FOUND.
	var issue, issueProject string
	if err := tx.QueryRow(r.Context(), `
		select key, project_key from issues where key = $1 for key share
	`, key).Scan(&issue, &issueProject); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "ISSUE_NOT_FOUND", http.StatusNotFound, fmt.Sprintf("no issue %s", key))
			return
		}
		s.writeHandlerError(w, err)
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
	// answer reads the issue's runs in the write's own transaction, as GET would after it.
	steps, err := workflowstore.IssueSteps(r.Context(), tx, issue)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	record := stepRecord(workflowstore.StepRead{Step: written})
	for _, step := range steps {
		if step.ID == written.ID {
			record = stepRecord(step)
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, record)
}

// listIssueSteps is every run of every step of one issue, task by task in the order of each
// task's first run: GET /api/v1/issues/{key}/steps. It reads as GET
// /api/v1/issues/{key}/handoffs does: a closed issue's runs as an open one's, and a key no issue
// carries as an issue with none.
func (s *server) listIssueSteps(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
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

// stepRecord is a run as the routes answer it: a ReturnedTo of 0 and an empty report are a run
// that has none, which the answer carries as null.
func stepRecord(step workflowstore.StepRead) model.StepRecord {
	record := model.StepRecord{
		ID: step.ID, Issue: step.Issue, Task: step.Task, Step: step.Place, Kind: step.Kind,
		Role: step.Role, Type: step.Type, Agent: step.Agent, Session: step.Session, Model: step.Model,
		Generation: step.Generation, Attempt: step.Attempt, StartedAt: step.StartedAt,
		EndedAt: step.EndedAt, Result: step.Result,
	}
	if step.ReturnedTo != 0 {
		returned := step.ReturnedTo
		record.ReturnedTo = &returned
	}
	if step.Report != "" {
		report := step.Report
		record.Report = &report
	}
	return record
}
