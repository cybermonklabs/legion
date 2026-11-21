package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's gated steps are the design-gate steps the Legion deployment that serves it gates,
// in the order it gates them (model.Project.GatedSteps). Each step names one document of a root
// issue a person must approve before that tree's work runs, so the row tells a person reading
// Dispatch what the deployment holds work for, without reading its configuration.
//
// The write belongs to the deployment's daemon and to no person, which is why the route declares
// no personal scope: the daemon presents the credential the deployment configured, as it does on
// PATCH /api/v1/issues/{key}, and such a credential holds no scope at all. The list is the
// daemon's own configuration published back, not a choice a person makes here, so nothing is
// lost by letting a machine write it; a person who sets a deployment's gate sets it in that
// deployment's configuration.

// projectGatedSteps is the stored row the route answers: the project and the steps it gates.
type projectGatedSteps struct {
	Project string   `json:"project"`
	Steps   []string `json:"steps"`
}

// putProjectGatedSteps publishes the design-gate steps of a project's deployment. The daemon
// republishes at every boot, so the update names `gated_steps is distinct from $2`: a boot on an
// unchanged list touches no row and appends no event, which is what keeps a restarting daemon
// out of the project's event log. A body that names no list is refused (400 INVALID_PROJECT)
// rather than read as the empty list, so a malformed publish cannot clear a gate, and a list
// naming anything but the three steps is refused there too, before the database's own check
// would raise. A step named twice is refused as well, as the deployment's own loader refuses it:
// the deployment keeps one gate record per step, so a second naming is either a mistake or an
// order it cannot keep, and the database's check admits it (it holds the shape, not the route's
// input rules).

// projectGatedStepsInput is the body PUT /api/v1/projects/{key}/gated-steps takes. Steps is
// required: a body without it is refused (400 INVALID_PROJECT, putProjectGatedSteps below)
// rather than read as the empty list, so a malformed publish cannot clear a gate. It is a
// pointer so that an absent list and an explicit `[]` are told apart, and the enum is the three
// steps model.GatedStepsKnown names, which the handler refuses anything outside.
type projectGatedStepsInput struct {
	Steps *[]string    `json:"steps" required:"true" enum:"decisions,criteria,spec"`
	Actor *model.Actor `json:"actor"`
}

func (s *server) putProjectGatedSteps(w http.ResponseWriter, r *http.Request) {
	var input projectGatedStepsInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	steps, ok := validGatedSteps(w, r, "steps", input.Steps)
	if !ok {
		return
	}

	project, err := s.loadProject(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	event, err := s.writeProjectGatedSteps(r.Context(), tx, actor, &project, steps)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if event == nil {
		// The deployment published the list the row already holds. Nothing changed, so
		// nothing is written and no event is appended; the answer is still the stored row.
		WriteJSON(w, http.StatusOK, projectGatedSteps{Project: project.Key, Steps: steps})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(r.Context(), *event)
	WriteJSON(w, http.StatusOK, projectGatedSteps{Project: project.Key, Steps: project.GatedSteps})
}

// validGatedSteps is the list of design-gate steps a publish sent in member, or false once it has
// written the refusal (400 INVALID_PROJECT): a body that names no list, a step that is none of
// the three, and a step named twice. A null list and an empty one are the same publish, the empty
// array. PUT /api/v1/projects/{key}/gated-steps (steps) and PATCH /api/v2/projects/{key}
// (gated_steps) hold the list to it alike.
func validGatedSteps(w http.ResponseWriter, r *http.Request, member string, sent *[]string) ([]string, bool) {
	if sent == nil {
		writeError(w, r, apicode.InvalidProject, fmt.Sprintf(
			"%s is required: the design-gate steps this deployment gates (%s), in the order it gates them, or [] for a deployment that gates none",
			member, strings.Join(model.GatedStepsKnown, ", ")))
		return nil, false
	}
	// A nil list and an empty one are the same publish, and the column is not null: the empty
	// array, which reads as a deployment that gates nothing.
	steps := *sent
	if steps == nil {
		steps = []string{}
	}
	for index, step := range steps {
		if !slices.Contains(model.GatedStepsKnown, step) {
			writeError(w, r, apicode.InvalidProject, fmt.Sprintf(
				"%q is not a design-gate step: %s names %s", step, member, strings.Join(model.GatedStepsKnown, ", ")))
			return nil, false
		}
		if slices.Contains(steps[:index], step) {
			writeError(w, r, apicode.InvalidProject, fmt.Sprintf(
				"%q is named twice: %s names each design-gate step once, in the order this deployment gates them", step, member))
			return nil, false
		}
	}
	return steps, true
}

// writeProjectGatedSteps stores steps as project's gated steps in tx where they differ from the
// list the row holds, setting project.GatedSteps to the stored list, and appends project.updated,
// answering the event for the caller to publish once tx commits. A list the row already holds
// writes nothing and answers no event.
func (s *server) writeProjectGatedSteps(ctx context.Context, tx pgx.Tx, actor model.Actor, project *model.Project, steps []string) (*model.Event, error) {
	// The project's owner lock before its row's update, as every projects writer takes them
	// (createIssue, putProjectDefaultAssignee, lockEventOwner). Taking the row first and the lock
	// at the append below waited, holding the row, on an issue create that holds the lock in its
	// read phase and waits for the row to number the issue: the daemon publishes this list at
	// every boot, beside the creates of the project's sessions, and Postgres broke that cycle by
	// failing one of the two (`deadlock detected`, 500). The lock assigns no transaction id; the
	// update is the first statement that writes, and one that finds the list unchanged writes
	// nothing.
	if err := store.LockOwner(ctx, tx, store.ProjectOwner(project.Key)); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `
		update projects set gated_steps = $2
		where key = $1 and gated_steps is distinct from $2
		returning gated_steps
	`, project.Key, steps).Scan(&project.GatedSteps); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	event, err := s.appendEvent(ctx, tx, projectOwner(project.Key).event("project.updated", actor, *project))
	if err != nil {
		return nil, err
	}
	return &event, nil
}
