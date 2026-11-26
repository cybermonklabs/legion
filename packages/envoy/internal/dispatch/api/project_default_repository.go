package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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
// A github_workflow deploy source that names no repository reads the project's default
// (delivery.SourceRepoOf), so a move of the default moves the source (dispatch://INFRA-978, D1):
// for a delivery-on project holding such a source, the write is held to the rules the settings
// save holds (defaultRepositoryMove), in its own transaction, before the row is written.
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
	// The project's owner lock before its row's update, as every projects writer takes them
	// (createIssue, putProjectGatedSteps, lockEventOwner): taking the row first and the lock at
	// the append below would wait, holding the row, on an issue create that holds the lock in its
	// read phase and waits for the row to number the issue. The lock assigns no transaction id,
	// nor do the delivery locks defaultRepositoryMove takes after it; its clear of the reconcile
	// progress, or else the update, is the first statement that writes.
	if err := store.LockOwner(r.Context(), tx, store.ProjectOwner(r.PathValue("key"))); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if refusal, err := defaultRepositoryMove(r.Context(), tx, r.PathValue("key"), repo); err != nil {
		s.writeHandlerError(w, err)
		return
	} else if refusal != nil {
		s.writeSettingsRefusal(w, refusal)
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
	// An unknown project is 404 PROJECT_NOT_FOUND here. The update wrote no row for it, so
	// nothing is left to roll back.
	project, err := s.loadProject(r.Context(), tx, r.PathValue("key"))
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

// defaultRepositoryMove holds a write of project's default repository to next to the delivery
// sources' rules (D1), in tx, after the project's owner lock and before the row is written, and
// answers the refusal where the move would break one. It applies to a delivery-on project holding
// a github_workflow source that names no repository, which reads the default; every other project
// moves its default freely, a project whose every source names its own repository among them. For
// such a project it takes the project's delivery lock and the lock of the old and the new
// repository (delivery.LockProject, LockRepositories: the order the settings save takes them in),
// so a save and this write cannot both make one repository a source of two projects. It refuses a
// clear, 409 DELIVERY_NEEDS_REPOSITORY, since the source would read no repository; and a repository
// another delivery-on project's source reads, or another source of this project names, 409
// DELIVERY_REPOSITORY_SHARED naming the project that reads it. A move it lets through clears the
// reconcile progress of the old and the new repository (delivery.ClearRepositoryProgress), as a
// save that changes a source's repository does, so the next reconcile imports the new one's runs
// from the start; no other delivery-on project's source reads either. An unknown project, or a
// write of the value the row holds, is none of its business, and it answers nil.
func defaultRepositoryMove(ctx context.Context, tx pgx.Tx, project string, next *string) (*delivery.SettingsError, error) {
	var current *string
	if err := tx.QueryRow(ctx, `select default_repo from projects where key = $1`, project).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the default repository of %s: %w", project, err)
	}
	if equalPointers(current, next) {
		return nil, nil
	}
	if err := delivery.LockProject(ctx, tx, project); err != nil {
		return nil, err
	}
	settings, err := delivery.GetProjectSettings(ctx, tx, project)
	if errors.Is(err, delivery.ErrNoSettings) || (err == nil && !settings.Delivery) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defaulting := -1
	for i, source := range settings.Sources {
		if source.Kind == delivery.SourceKindGitHubWorkflow && source.Repo == nil {
			defaulting = i
			break
		}
	}
	if defaulting < 0 {
		return nil, nil
	}
	var repos []string
	for _, repo := range []*string{current, next} {
		if repo != nil {
			repos = append(repos, *repo)
		}
	}
	if err := delivery.LockRepositories(ctx, tx, repos); err != nil {
		return nil, err
	}
	if next == nil {
		return &delivery.SettingsError{
			Code: delivery.CodeDeliveryNeedsRepository, Field: "repo", Source: new(defaulting), Project: project,
			Message: fmt.Sprintf("project %s has delivery on and its source %d names no repository, so it reads the default repository: name the source's repository in the project's delivery settings, or turn delivery off, before clearing the default", project, defaulting),
		}, nil
	}
	for i, source := range settings.Sources {
		if source.Kind == delivery.SourceKindGitHubWorkflow && source.Repo != nil && strings.EqualFold(*source.Repo, *next) {
			return &delivery.SettingsError{
				Code: delivery.CodeDeliveryRepositoryShared, Field: "repo", Source: new(i), Project: project,
				Message: fmt.Sprintf("%s is already source %d of project %s; a repository is one source of one project", *next, i, project),
			}, nil
		}
	}
	other, shared, err := delivery.RepositoryOwner(ctx, tx, *next, project)
	if err != nil {
		return nil, err
	}
	if shared {
		return &delivery.SettingsError{
			Code: delivery.CodeDeliveryRepositoryShared, Field: "repo", Source: new(defaulting), Project: other,
			Message: fmt.Sprintf("%s is already a deploy source of project %s, which has delivery on, and source %d of project %s would read it as its default repository; a repository is the deploy source of one project at most", *next, other, defaulting, project),
		}, nil
	}
	return nil, delivery.ClearRepositoryProgress(ctx, tx, repos)
}
