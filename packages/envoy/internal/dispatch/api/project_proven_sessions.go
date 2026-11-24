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
// that proves legion-daemon:<project>.
//
// The two routes are not alike, on purpose. Turning the setting on makes a check stronger, so any
// credential that may write the project's issues may turn it on: the PUT is scopeProject and
// declares no OAuth scope, so the project's daemon can turn it on with its own project-scoped
// token. Turning it off makes a check weaker, so the DELETE needs the person scope (routeScopes),
// as PUT /api/v1/projects/{key}/default-assignee does, and records the person (actingPerson). No
// credential the deployment configured holds that scope, so no machine turns the setting off.
//
// Turning it on before the project's daemon writes with a credential that proves its session
// stops every architect write and every role-holder publication on the shared token, until a
// person turns it off. That costs time and loses no data.

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
// transaction id.
func (s *server) setProjectProvenSessions(w http.ResponseWriter, r *http.Request, actor model.Actor, on bool) {
	key := r.PathValue("key")
	if err := s.storeProjectProvenSessions(r.Context(), key, actor, on); err != nil {
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

// storeProjectProvenSessions writes the setting under the project's owner lock and appends
// project.updated with the project row and its writer. An unknown project is 404
// PROJECT_NOT_FOUND (loadProject), before anything is written. A write that finds the setting
// already as asked changes nothing: the update names `proven_sessions is distinct from $2`, so
// it touches no row, and no event is appended.
func (s *server) storeProjectProvenSessions(ctx context.Context, key string, actor model.Actor, on bool) error {
	project, err := s.loadProject(ctx, s.deps.Store.Pool, key)
	if err != nil {
		return err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// The project's owner lock before its row's update, as every projects writer takes them
	// (putProjectGatedSteps says why: the other order deadlocks with an issue create). The lock
	// assigns no transaction id; the update is the first statement that writes.
	if err := store.LockOwner(ctx, tx, store.ProjectOwner(project.Key)); err != nil {
		return err
	}
	// The update returns the gated steps too, read under the owner lock, so the event carries
	// the row as this write left it.
	if err := tx.QueryRow(ctx, `
		update projects set proven_sessions = $2
		where key = $1 and proven_sessions is distinct from $2
		returning gated_steps, proven_sessions
	`, project.Key, on).Scan(&project.GatedSteps, &project.ProvenSessions); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	event, err := s.appendEvent(ctx, tx, projectOwner(project.Key).event("project.updated", actor, project))
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.publish(ctx, event)
	return nil
}
