package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's proven-sessions setting (model.Project.ProvenSessions) makes two checks take a
// session's name only from a credential that proves that session: the spec-writer check
// (refuseSpecWriter, spec_writer.go), where an architect's write of an issue's spec or
// Verification document then passes only on a credential that proves the architect's session,
// and the role-holder publication (putIssueRoleHoldersFrom), which then passes only on a
// credential that proves legion-daemon:<project>, or legion-daemon:<P> of another project's
// daemon on a broker token that names P (isIssueDaemon).
//
// The two writes are not alike, on purpose. Turning the setting on makes a check stronger, so any
// credential that may write the project's issues may turn it on: PUT
// /api/v1/projects/{key}/proven-sessions, and PATCH /api/v2/projects/{key} with proven_sessions
// true, are scopeProject and declare no OAuth scope, so the project's daemon can turn it on with
// its own project-scoped token. Turning it off makes a check weaker, so it needs the person scope,
// as the project's default assignee does: DELETE /api/v1/projects/{key}/proven-sessions declares
// it (its declaration's OAuthScopes), and PATCH /api/v2/projects/{key} with proven_sessions false
// needs it by the row's authority rule. Both record the person (actingPerson). No credential the
// deployment configured holds that scope, so no machine turns the setting off.
//
// Turning it on before the project's daemon writes with a credential that proves its session
// stops every architect write and every role-holder publication on the shared token, until a
// person turns it off. That costs time and loses no data.

// putProjectProvenSessions answers PUT /api/v1/projects/{key}/proven-sessions: it turns the
// project's setting on. A bearer names its session in actor; a cookie caller sends no body.
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

// deleteProjectProvenSessions answers DELETE /api/v1/projects/{key}/proven-sessions: it turns the
// project's setting off. The table gives the route the person scope, so the actor is the person
// the credential acts for.
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
		s.writeHandlerError(w, r, err)
		return
	}
	project, err := scanListedProject(s.deps.Store.Pool.QueryRow(r.Context(), projectRowQuery, key))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, project)
}

// storeProjectProvenSessions writes the setting in a transaction of its own
// (writeProjectProvenSessions) and publishes the event of a write that changed it. An unknown
// project is 404 PROJECT_NOT_FOUND, before anything is written, and a write that finds the
// setting already as asked changes nothing and appends no event.
func (s *server) storeProjectProvenSessions(ctx context.Context, key string, actor model.Actor, on bool) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	project := model.Project{Key: key}
	event, err := s.writeProjectProvenSessions(ctx, tx, actor, &project, on)
	if err != nil || event == nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.publish(ctx, *event)
	return nil
}

// writeProjectProvenSessions writes project's setting to on in tx, under the project's owner
// lock, and appends project.updated with the project row and its writer, answering the event for
// the caller to publish once tx commits; it sets *project to the row as the write leaves it.
// project names the project by its Key, and an unknown project is 404 PROJECT_NOT_FOUND
// (loadProject), before anything is written. A write that finds the setting already as asked
// changes nothing: the update names `proven_sessions is distinct from $2`, so it touches no row,
// and it answers no event. The PUT and the DELETE of /api/v1 (storeProjectProvenSessions) and
// PATCH /api/v2/projects/{key} (patchProjectV2) write the setting through it alike.
func (s *server) writeProjectProvenSessions(ctx context.Context, tx pgx.Tx, actor model.Actor, project *model.Project, on bool) (*model.Event, error) {
	// The project's owner lock before its row's update, as every projects writer takes them
	// (writeProjectGatedSteps says why: the other order deadlocks with an issue create). The lock
	// assigns no transaction id; the update is the first statement that writes.
	if err := store.LockOwner(ctx, tx, store.ProjectOwner(project.Key)); err != nil {
		return nil, err
	}
	// The row is read under the owner lock, which every writer of the row takes first, so the
	// event carries the row as this write leaves it; the read comes before the update, so it adds
	// no statement to the window an open transaction id holds (writeProjectGatedSteps).
	read, err := s.loadProject(ctx, tx, project.Key)
	if err != nil {
		return nil, err
	}
	*project = read
	written, err := tx.Exec(ctx, `
		update projects set proven_sessions = $2
		where key = $1 and proven_sessions is distinct from $2
	`, project.Key, on)
	if err != nil {
		return nil, err
	}
	if written.RowsAffected() == 0 {
		return nil, nil
	}
	project.ProvenSessions = on
	event, err := s.appendEvent(ctx, tx, projectOwner(project.Key).event("project.updated", actor, *project))
	if err != nil {
		return nil, err
	}
	return &event, nil
}
