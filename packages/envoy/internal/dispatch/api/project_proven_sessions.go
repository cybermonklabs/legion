package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's proven-sessions setting (model.Project.ProvenSessions) makes two checks take a
// session's name only from a credential that proves that session: the spec-writer check
// (refuseSpecWriter, spec_writer.go), where an architect's write of an issue's spec or
// Verification document then passes only on a credential that proves the architect's session,
// and the role-holder publication (putIssueRoleHolders), which then passes only on a credential
// that proves legion-daemon:<project>, or legion-daemon:<P> of another project's daemon on a
// broker token that names P (isIssueDaemon).
//
// The two routes are not alike, on purpose. Turning the setting on makes a check stronger, so any
// credential that may write the project's issues may turn it on: the PUT is scopeProject and
// declares no OAuth scope, so the project's daemon can turn it on with its own project-scoped
// token. Turning it off makes a check weaker, so the DELETE needs the person scope (its
// declaration's OAuthScopes), as PUT /api/v1/projects/{key}/default-assignee does, and records the
// person (actingPerson). No credential the deployment configured holds that scope, so no machine
// turns the setting off.
//
// Turning it on before the project's daemon writes with a credential that proves its session
// stops every architect write and every role-holder publication on the shared token, until a
// person turns it off. That costs time and loses no data.
//
// The command bar offers the setting as a row of a plan (dispatch_project_update), and its Undo
// sends the other route, on a condition this file's routes decide under the project's lock
// (goingCondition.ExpectProvenSessions): the project must hold the setting the change left, and
// no project.updated event after the one that recorded it may have changed the setting, a change
// that put it back included. A change another writer made is theirs to keep.

// putProjectProvenSessions turns the project's setting on. A bearer names its session in actor;
// a cookie caller sends no body.
func (s *server) putProjectProvenSessions(w http.ResponseWriter, r *http.Request) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	s.setProjectProvenSessions(w, r, actor, true)
}

// deleteProjectProvenSessions turns the project's setting off. The table gives the route the
// person scope, so the actor is the person the credential acts for.
func (s *server) deleteProjectProvenSessions(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	s.setProjectProvenSessions(w, r, actor, false)
}

// setProjectProvenSessions stores the setting and answers the project as the list shows it. The
// read of the answer runs after the write's transaction has ended, so it holds no lock and no
// transaction id. The command bar's undo of a change of the setting makes the write conditional
// (goingCondition.ExpectProvenSessions, on the request's context): it is refused, 409
// PROJECT_CHANGED, unless the project still holds what that change left, and a write back that
// carries no such condition is refused, 500 GOING_CONDITION_MISSING, with nothing written.
func (s *server) setProjectProvenSessions(w http.ResponseWriter, r *http.Request, actor model.Actor, on bool) {
	condition := goingConditionOf(r)
	if refusal := condition.writeBackRefusal(condition.ExpectProvenSessions != nil, "setting it expects the project to hold"); refusal != nil {
		s.writeHandlerError(w, refusal)
		return
	}
	key := r.PathValue("key")
	if err := s.storeProjectProvenSessions(r.Context(), key, actor, on, condition.ExpectProvenSessions); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	project, err := scanListedProject(s.deps.Store.Pool.QueryRow(r.Context(), projectRowQuery, key))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, project)
}

// codeProjectChanged is the refusal of a write of a project's proven-sessions setting made on a
// condition the project no longer meets when the write takes the project's lock
// (refuseProvenSessionsChanged).
const codeProjectChanged = "PROJECT_CHANGED"

// provenSessionsLanded is what a change of the setting hands the command bar's landing of its
// request (goingRecordAnswer): the project, the setting the change left, and the sequence of the
// project's event that recorded it, as last_seq, which an issue's update records for its own
// event too (goingUndoer.recorded reads it). The undo of the change looks for a write after it.
type provenSessionsLanded struct {
	Key            string `json:"key"`
	ProvenSessions bool   `json:"proven_sessions"`
	LastSeq        int    `json:"last_seq"`
}

// provenSessionsChangedAfter is the writer of the newest project.updated event of project $1 after
// the event at sequence $2 that changed proven_sessions from what the project.updated event
// before it held, the event at $2 itself being the first of those before: a change and the change
// that put it back are two events, each found. No row where nothing changed the setting since.
// Every event of the project's sequence is read through events_project_key_seq_key, which the
// project's events share with no other owner's.
const provenSessionsChangedAfter = `
	select actor from (
		select actor, seq, payload->>'proven_sessions' as setting,
		       lag(payload->>'proven_sessions') over (order by seq) as earlier
		from events
		where project_key = $1 and type = 'project.updated' and seq >= $2
	) updates
	where seq > $2 and setting is distinct from earlier
	order by seq desc
	limit 1`

// refuseProvenSessionsChanged is the refusal of a conditional write whose project, read under its
// owner lock in tx, no longer holds the setting the condition expects, or has had its setting
// changed since the event the condition names (provenSessionsChangedAfter): 409 PROJECT_CHANGED,
// nothing written. nil where the project stands as the condition expects. The lock is the one
// every writer of the setting takes first, so no change is between this read and the write.
func refuseProvenSessionsChanged(ctx context.Context, tx pgx.Tx, project model.Project, expected goingProvenSessionsExpected) error {
	if project.ProvenSessions != expected.Setting {
		return errorf(http.StatusConflict, codeProjectChanged,
			"the project's proven-sessions setting is %s, not the %s the write expects", provenSessionsWord(project.ProvenSessions), provenSessionsWord(expected.Setting))
	}
	var writer model.Actor
	err := tx.QueryRow(ctx, provenSessionsChangedAfter, project.Key, expected.AfterSeq).Scan(&writer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return errorf(http.StatusConflict, codeProjectChanged,
		"the project's proven-sessions setting was changed after the event the write expects it left at")
}

// provenSessionsWord is the setting as `dispatch projects` and the command bar word it: on or off.
func provenSessionsWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// storeProjectProvenSessions writes the setting under the project's owner lock and appends
// project.updated with the project row and its writer. An unknown project is 404
// PROJECT_NOT_FOUND (loadProject), before anything is written. A write that finds the setting
// already as asked changes nothing: the update names `proven_sessions is distinct from $2`, so
// it touches no row, appends no event and commits nothing, so a command-bar run's request of it
// has no landing. expected is the condition of the command bar's undo, decided under the same
// lock before the update (refuseProvenSessionsChanged), nil for every other write. A change
// hands the landing of a command-bar request what it left (provenSessionsLanded) before it
// commits.
func (s *server) storeProjectProvenSessions(ctx context.Context, key string, actor model.Actor, on bool, expected *goingProvenSessionsExpected) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// The project's owner lock before its row's update, as every projects writer takes them
	// (putProjectGatedSteps says why: the other order deadlocks with an issue create). The lock
	// assigns no transaction id; the update is the first statement that writes.
	if err := store.LockOwner(ctx, tx, store.ProjectOwner(key)); err != nil {
		return err
	}
	// The row is read under the owner lock, which every writer of the row takes first, so the
	// event carries the row as this write leaves it; the read comes before the update, so it adds
	// no statement to the window an open transaction id holds (putProjectGatedSteps). The
	// condition's reads come before the update for the same reason.
	project, err := s.loadProject(ctx, tx, key)
	if err != nil {
		return err
	}
	if expected != nil {
		if err := refuseProvenSessionsChanged(ctx, tx, project, *expected); err != nil {
			return err
		}
	}
	written, err := tx.Exec(ctx, `
		update projects set proven_sessions = $2
		where key = $1 and proven_sessions is distinct from $2
	`, key, on)
	if err != nil {
		return err
	}
	if written.RowsAffected() == 0 {
		return nil
	}
	project.ProvenSessions = on
	event, err := s.appendEvent(ctx, tx, projectOwner(project.Key).event("project.updated", actor, project))
	if err != nil {
		return err
	}
	goingRecordAnswer(ctx, http.StatusOK, func() any {
		return provenSessionsLanded{Key: project.Key, ProvenSessions: on, LastSeq: event.Seq}
	})
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.publish(ctx, event)
	return nil
}
