package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A project's default repository is the `owner/name` an issue of the project resolves to when
// neither the issue nor an issue above it names a repository of its own (refs.IssueRepoResolved,
// over projects.default_repo). A project has one or none.
//
// The write belongs to the deployment and to no person, so its route is scopeDeployment and
// declares no personal scope (routes_table.go). The default decides the repository of every issue
// of the project that names none, so a credential that names one project is refused it by name,
// the project its path names included. The credential the deployment configured writes it, and
// so does a person. PATCH /api/v2/projects/{key} writes it as its default_repository member
// (patchProjectV2), by the same rule and through the same write.

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
func (s *server) putProjectDefaultRepository(w http.ResponseWriter, r *http.Request) {
	var input projectDefaultRepositoryInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	repo, provided, err := parseIssueRepo(input.Repo, "repo", apicode.InvalidProject)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !provided {
		writeError(w, r, apicode.InvalidProject,
			`repo is required: the project's default repository as "owner/name", or null for none`)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	project, event, err := s.writeProjectDefaultRepository(r.Context(), tx, actor, r.PathValue("key"), s.renamedRepo(repo))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	stored := projectDefaultRepository{Project: project.Key, Repo: project.DefaultRepo}
	if event == nil {
		// The row holds this value already. Nothing changed, so nothing is written and no event
		// is appended; the answer is still the stored value.
		WriteJSON(w, http.StatusOK, stored)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(r.Context(), *event)
	WriteJSON(w, http.StatusOK, stored)
}

// writeProjectDefaultRepository stores repo, nil for none, as the default repository of the
// project key names in tx, answering the project as tx holds it after the write and the
// project.updated event for the caller to publish once tx commits. The update names
// `default_repo is distinct from $2`: a value the row holds already changes no row and answers no
// event. A change appends project.updated with the project row, as the gated steps' publish does.
// The row is read in the transaction that wrote it, so the event carries the row as this write
// left it, and a key no project carries is 404 PROJECT_NOT_FOUND. repo is the value parseIssueRepo
// read, under the name renamedRepo gives it.
func (s *server) writeProjectDefaultRepository(ctx context.Context, tx pgx.Tx, actor model.Actor, key string, repo *string) (model.Project, *model.Event, error) {
	// The project's owner lock before its row's update, as every projects writer takes them
	// (createIssue, writeProjectGatedSteps, lockEventOwner): taking the row first and the lock at
	// the append below would wait, holding the row, on an issue create that holds the lock in its
	// read phase and waits for the row to number the issue. The lock assigns no transaction id;
	// the update is the first statement that writes.
	if err := store.LockOwner(ctx, tx, store.ProjectOwner(key)); err != nil {
		return model.Project{}, nil, err
	}
	written, err := tx.Exec(ctx, `
		update projects set default_repo = $2
		where key = $1 and default_repo is distinct from $2
	`, key, repo)
	if err != nil {
		return model.Project{}, nil, err
	}
	// An unknown project is 404 PROJECT_NOT_FOUND here. The update wrote no row for it, so
	// nothing is left to roll back.
	project, err := s.loadProject(ctx, tx, key)
	if err != nil {
		return model.Project{}, nil, err
	}
	if written.RowsAffected() == 0 {
		return project, nil, nil
	}
	event, err := s.appendEvent(ctx, tx, projectOwner(project.Key).event("project.updated", actor, project))
	if err != nil {
		return model.Project{}, nil, err
	}
	return project, &event, nil
}
