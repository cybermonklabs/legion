package api

import (
	"encoding/json"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's default repository is the `owner/name` an issue of the project resolves to when
// neither the issue nor an issue above it names a repository of its own (refs.IssueRepoResolved,
// over projects.default_repo). A project has one or none.
//
// The write belongs to the deployment and to no person, so its route is scopeDeployment and
// declares no personal scope (routes_table.go). The default decides the repository of every issue
// of the project that names none, so a credential that may write one project's issues is refused
// it by name, the project its path names included. The credential the deployment configured
// writes it, and so does a person.

// projectDefaultRepository is what the route answers: the project, and the default it holds now,
// null for none.
type projectDefaultRepository struct {
	Project string  `json:"project"`
	Repo    *string `json:"repo"`
}

// projectDefaultRepositoryInput is the body PUT /api/v1/projects/{key}/default-repository takes.
// Repo is held as its bytes because it has three states: a string sets the default, null clears
// it, and a body without it is refused (400 INVALID_PROJECT) rather than read as a clear, so a
// malformed write cannot remove a project's default.
type projectDefaultRepositoryInput struct {
	Repo  json.RawMessage `json:"repo" required:"true"`
	Actor *model.Actor    `json:"actor"`
}

// putProjectDefaultRepository sets or clears a project's default repository. The value is read by
// the rule an issue's own repository is read by (parseIssueRepo), so the column holds only an
// `owner/name` GitHub could hold, in lower case and with no `.git`. A name a rename pair renames
// away is stored under its new name (refs.Renames.Canonical), so no write stores an old name.
//
// The update names `default_repo is distinct from $2`: a write of the value the row holds already
// changes no row and appends no event. A change appends project.updated with the project row, as
// PUT /api/v1/projects/{key}/gated-steps does, commits, and then publishes it. The row is read in
// the transaction that wrote it, so the event carries the row as this write left it.
//
// The project is read as its writer reads it before anything is written and before any lock is
// taken: a project the writer does not see is 404 PROJECT_NOT_FOUND, as a key nobody holds, its
// row is never written, and the write never queues behind a writer of that project, which would
// tell its writer one is at work (loadProject, access.ProjectClause). A default is the repository
// every issue of the project that names none resolves to, which makes it a repository the project
// owns (refs.ProjectRepositoriesSQL), so it is held as an issue's own repository is: one the
// GitHub proxy hides from the writer is refused 403 REPOSITORY_NOT_AVAILABLE in words that name no
// project (refuseRepoHidden), and a clear is refused nothing. The read after the update is made as
// the writer reads it too, so a project made private in between is that 404, and nothing is kept.
// A restricted member, and a credential whose writes a list holds, never reach this handler: the
// route is scopeDeployment, which withinProjectScope refuses them (refuseDeploymentWrite).
func (s *server) putProjectDefaultRepository(w http.ResponseWriter, r *http.Request) {
	var input projectDefaultRepositoryInput
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
	repo, provided, err := parseIssueRepo(input.Repo, "INVALID_PROJECT")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !provided {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest,
			`repo is required: the project's default repository as "owner/name", or null for none`)
		return
	}
	repo = s.renamedRepo(repo)

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := s.loadProject(r.Context(), tx, who, r.PathValue("key")); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refuseRepoHidden(r.Context(), tx, who, repo); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The project's owner lock before its row's update, as every projects writer takes them
	// (createIssue, putProjectGatedSteps, lockEventOwner): taking the row first and the lock at
	// the append below would wait, holding the row, on an issue create that holds the lock in its
	// read phase and waits for the row to number the issue. The lock assigns no transaction id;
	// the update is the first statement that writes.
	if err := store.LockOwner(r.Context(), tx, store.ProjectOwner(r.PathValue("key"))); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	written, err := tx.Exec(r.Context(), `
		update projects set default_repo = $2
		where key = $1 and default_repo is distinct from $2
	`, r.PathValue("key"), repo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The row as this write left it, read as the writer reads it. An access change can commit
	// between the read above and the owner lock, so this read can find the project hidden: it is
	// then the 404 PROJECT_NOT_FOUND of a key nobody holds, and the update rolls back with the
	// transaction.
	project, err := s.loadProject(r.Context(), tx, who, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	stored := projectDefaultRepository{Project: project.Key, Repo: project.DefaultRepo}
	if written.RowsAffected() == 0 {
		// The row holds this value already. Nothing changed, so nothing is written and no event
		// is appended; the answer is still the stored value.
		WriteJSON(w, http.StatusOK, stored)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(project.Key).event("project.updated", actor, project))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), event)
	WriteJSON(w, http.StatusOK, stored)
}
