package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A project imports its component model from any number of sources, one per repository, and a
// source is addressed by its project and repository, as a repository mapping is. Every route here
// is authAny: agents save, remove and refresh sources as people do, and a bearer save or removal
// names its session in actor.

// listArchitectureSources answers every source of every project the caller sees, by project and
// then in the order each project's sources were added.
func (s *server) listArchitectureSources(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	sources, err := querySources(r.Context(), s.deps.Store.Pool, who, `true`, `project_key, id`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, sources)
}

// listProjectArchitectureSources answers the project's sources in the order they were added. A
// project with none answers [], since whether a project has a model is a question with an
// ordinary negative answer; a project that does not exist is still a 404, so a mistyped key is
// reported rather than read as "no model". A project the caller does not see is one that does
// not exist.
func (s *server) listProjectArchitectureSources(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	sources, err := querySources(r.Context(), s.deps.Store.Pool, who, `project_key = $1`, `id`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(sources) == 0 {
		if err := s.requireVisibleProject(r.Context(), who, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, sources)
}

// getArchitectureSource answers the singular read that dispatch clients older than this release
// make on every issue create: the project's first source in id order, or null when it has none
// (they read only whether it is null), and 404 for a project that does not exist or that the
// caller does not see. The CLI and the server ship separately, so a removed route turned every
// create into false "Could not check" advice (#1522 fixed the same skew); it stays until no such
// client is left.
func (s *server) getArchitectureSource(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	slog.Warn("dispatch: GET /api/v1/projects/{key}/architecture-source called by a dispatch client older than this release; upgrade it, the route will be removed", "project", key)
	sources, err := querySources(r.Context(), s.deps.Store.Pool, who, `project_key = $1`, `id limit 1`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if len(sources) == 0 {
		if err := s.requireVisibleProject(r.Context(), who, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, nil)
		return
	}
	WriteJSON(w, http.StatusOK, sources[0])
}

// requireVisibleProject answers pgx.ErrNoRows, writeHandlerError's 404 NOT_FOUND, for a project
// key no project holds, or one who does not see (access.ProjectClause).
func (s *server) requireVisibleProject(ctx context.Context, who model.Actor, key string) error {
	return s.deps.Store.Pool.QueryRow(ctx, "select key from projects where key = $1 and "+access.ProjectClause("key", "$2"),
		key, refs.VisibilityArg(who)).Scan(new(string))
}

// querySources reads the source rows that condition (over architecture_sources' columns, its
// placeholders args' from $1) selects, in order, of the projects who sees: the viewer is bound
// after args, so a project who does not see holds no source.
func querySources(ctx context.Context, q queryer, who model.Actor, condition, order string, args ...any) ([]model.ArchitectureSource, error) {
	seen := "$" + strconv.Itoa(len(args)+1)
	rows, err := q.Query(ctx, `select `+architecture.SourceColumns+` from architecture_sources
		where (`+condition+`) and `+access.ProjectClause("project_key", seen)+` order by `+order,
		append(args, refs.VisibilityArg(who))...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.ArchitectureSource, error) { return architecture.ScanSource(row) })
}

// putArchitectureSourceInput is the body of PUT
// /api/v1/projects/{key}/architecture-sources/{owner}/{repo}.
type putArchitectureSourceInput struct {
	Branch string       `json:"branch" required:"true"`
	Actor  *model.Actor `json:"actor"`
}

// putArchitectureSource adds the repository in the path as a source of the project, or changes
// the branch of the source it already is. A re-save keeps the source's id and its components and
// resets its sync bookkeeping, so the next sync re-imports from the new branch.
func (s *server) putArchitectureSource(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var input putArchitectureSourceInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	branch := strings.TrimSpace(input.Branch)
	if branch == "" {
		writeError(w, "SOURCE_INPUT", http.StatusBadRequest, "branch is required")
		return
	}
	// The project as the caller reads it, before GitHub is asked anything: a project it does not
	// see is a key no project holds, whatever the repository's access would answer.
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	if err := s.requireVisibleProject(r.Context(), who, key); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	// The repository is held to the GitHub proxy's rule for the writer before GitHub is asked
	// anything (hiddenSourceRefusal): one the proxy would not show the writer answers exactly as
	// one the App is not installed on.
	refusal, err := s.hiddenSourceRefusal(r.Context(), who, owner, name)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The access check, of the repository and of the branch, runs before any write: a value that
	// is not exactly the name of a branch the repository has (a commit SHA, a relative ref such as
	// main~1, HEAD, a refs/ path) is refused like a missing branch, and a failed re-PUT leaves the
	// stored source untouched.
	var installationID int64
	if refusal != nil {
		err = refusal
	} else {
		installationID, err = s.checkArchitectureSource(r.Context(), who, owner, name, branch)
	}
	if err != nil {
		s.writeSourceRefusal(w, err)
		return
	}

	actorJSON, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	source, err := architecture.ScanSource(tx.QueryRow(r.Context(), `
		insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
		values ($1, $2, $3, true, $4, $5)
		on conflict (project_key, repo) do update set
			branch = excluded.branch,
			enabled = true,
			installation_id = excluded.installation_id,
			created_by = excluded.created_by,
			last_sync_at = null,
			last_commit = null,
			last_tree_sha = null,
			last_error = null
		returning `+architecture.SourceColumns,
		key, owner+"/"+name, branch, installationID, actorJSON))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(key).event(
		"settings.architecture_source.updated", actor, map[string]any{"source": source, "deleted": false},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), event)
	WriteJSON(w, http.StatusOK, source)
}

// checkArchitectureSource proves the GitHub App can read owner/name and that branch is the name of
// a branch it has, and answers the installation that reads it. The sync refuses a missing branch
// the same way, so a save that skipped the branch would store a source no sync can import. It
// reads the branch's exact ref (githubapp.Client.Branch), not the commit the sync's Ref resolves:
// GitHub's commits read takes any commit-ish, so a SHA or main~1 would pass it and be stored as
// the source's branch.
//
// While a project is private every read is held to owner/name (githubapp.HoldToRepository), as
// the GitHub proxy holds its reads: GitHub answers an old name of a renamed or transferred
// repository with a redirect to it as it is now, which the repository rule never judged, so such
// a redirect answers as a repository the App is not installed on. With no private project the check
// follows GitHub's redirects, as before (R9).
func (s *server) checkArchitectureSource(ctx context.Context, who model.Actor, owner, name, branch string) (int64, error) {
	if who.Viewer != nil && who.Viewer.AnyPrivate {
		ctx = githubapp.HoldToRepository(ctx, owner, name)
	}
	installation, err := s.checkHeldSource(ctx, owner, name, branch)
	if errors.Is(err, githubapp.ErrRedirectedOffRepository) {
		return 0, fmt.Errorf("%w on %s/%s", githubapp.ErrNoInstallation, owner, name)
	}
	return installation, err
}

// checkHeldSource is checkArchitectureSource's three GitHub reads, on ctx as it is held.
func (s *server) checkHeldSource(ctx context.Context, owner, name, branch string) (int64, error) {
	check, err := s.deps.GitHub.CheckSource(ctx, owner, name)
	if err != nil {
		return 0, err
	}
	token, err := s.deps.GitHub.Token(ctx, check.InstallationID)
	if err != nil {
		return 0, err
	}
	if _, err := s.deps.GitHub.Branch(ctx, token, owner, name, branch); err != nil {
		return 0, err
	}
	return check.InstallationID, nil
}

// deleteArchitectureSource removes the source and, in the same transaction, the model it
// projected: its components (by the components foreign key's cascade), their dependency
// edges (graph_edges loses their component arms with them) and the times recorded for them
// (component_facts). Every other source's components stay, and snapshots stay as history.
func (s *server) deleteArchitectureSource(w http.ResponseWriter, r *http.Request) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	key := r.PathValue("key")
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo := owner + "/" + name
	if _, ok := s.sourceProjectVisible(w, r, key, func() { writeSourceNotFound(w, key, repo) }); !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	source, err := architecture.ScanSource(tx.QueryRow(r.Context(), `
		delete from architecture_sources where project_key = $1 and repo = $2
		returning `+architecture.SourceColumns,
		key, repo))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeSourceNotFound(w, key, repo)
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	if err := architecture.DeleteOrphanComponentFacts(r.Context(), tx, source.Project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(key).event(
		"settings.architecture_source.updated", actor, map[string]any{"source": source, "deleted": true},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), event)
	w.WriteHeader(http.StatusNoContent)
}

// syncArchitectureSource imports one source's model now, inline, and answers its updated row. A
// model or upstream failure is a 200 whose row carries last_error: the HTTP call itself succeeded
// and the source's previous projection is still up. Only a credential- or configuration-shaped
// failure — no App key, no installation, no Contents: read, no such branch — is a 409 the caller
// must act on.
func (s *server) syncArchitectureSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	owner, name, err := sourcePathRepo(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo := owner + "/" + name
	who, ok := s.sourceProjectVisible(w, r, key, func() { writeSourceNotFound(w, key, repo) })
	if !ok {
		return
	}
	// A source whose repository the GitHub proxy would not show the caller, which only a source
	// written before that rule held its writes can be, answers what a source of a repository the
	// App is not installed on answers, and nothing is imported or written for it
	// (hiddenSourceRefusal). A source nobody configured answers 404 first, as Sync answers it, so
	// the hidden answer is given only where a source is there to answer it; the row is read only
	// for a hidden repository, so every other sync reads what it read before.
	refusal, err := s.hiddenSourceRefusal(r.Context(), who, owner, name)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if refusal != nil {
		stored, err := querySources(r.Context(), s.deps.Store.Pool, who, "project_key = $1 and repo = $2", "id", key, repo)
		switch {
		case err != nil:
			s.writeHandlerError(w, err)
		case len(stored) == 0:
			writeSourceNotFound(w, key, repo)
		case architecture.IsAccessFailure(refusal):
			writeError(w, "SOURCE_ACCESS", http.StatusConflict, refusal.Error())
		default:
			// Under a held App rate limit the sync of an uninstalled source records that limit on
			// its row and answers the row: a hidden one's is answered as it stands.
			WriteJSON(w, http.StatusOK, stored[0])
		}
		return
	}
	source, err := s.deps.Architecture.Sync(r.Context(), key, repo)
	switch {
	case err == nil:
		WriteJSON(w, http.StatusOK, source)
	case errors.Is(err, architecture.ErrNoSource):
		writeSourceNotFound(w, key, repo)
	case source.Project == "":
		// Sync could not even record the failure on the row: the database, not
		// the model or GitHub, is what broke.
		s.writeHandlerError(w, err)
	case architecture.IsAccessFailure(err):
		writeError(w, "SOURCE_ACCESS", http.StatusConflict, err.Error())
	default:
		WriteJSON(w, http.StatusOK, source)
	}
}

// syncArchitectureSources imports every source of the project now, inline, each attempted
// whatever an earlier one did, and answers every row in the order the sources were added. Each
// failure, an access failure included, is on its own row's last_error; the call answers 200
// because the import was attempted and recorded. Only a failure that could not be recorded on its
// row is a 500.
func (s *server) syncArchitectureSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	sources, ok := s.syncProjectSources(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, sources)
}

// syncSingularArchitectureSource answers the singular sync that `dispatch architecture-sync`
// from dispatch clients older than this release makes. It imports every source as
// syncArchitectureSources does and answers one row in the shape those clients read: the first
// row in id order whose last_error is set, or the first row when none failed, so a failure on any
// source reaches them. A project with no source is a 404 SOURCE_NOT_FOUND, and a failure that
// could not be recorded is a 500, as before. An access failure (no installation, no Contents:
// read, no such branch) was a 409 SOURCE_ACCESS; it is now recorded on its row, so it answers
// 200 with that row's last_error, which those clients print as "Sync failed for ...". It stays
// until no such client is left.
func (s *server) syncSingularArchitectureSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	slog.Warn("dispatch: POST /api/v1/projects/{key}/architecture-source/sync called by a dispatch client older than this release; upgrade it, the route will be removed", "project", r.PathValue("key"))
	sources, ok := s.syncProjectSources(w, r)
	if !ok {
		return
	}
	answer := sources[0]
	for _, source := range sources {
		if source.LastError != nil {
			answer = source
			break
		}
	}
	WriteJSON(w, http.StatusOK, answer)
}

// syncProjectSources imports every source of the request's project and answers the rows in id
// order, at least one. When there is nothing to answer it writes the error itself and reports
// false: 404 SOURCE_NOT_FOUND for a project with no source, and the handler error for a failure
// that could not be recorded on its row.
func (s *server) syncProjectSources(w http.ResponseWriter, r *http.Request) ([]model.ArchitectureSource, bool) {
	key := r.PathValue("key")
	who, ok := s.sourceProjectVisible(w, r, key, func() { writeProjectSourceNotFound(w, key) })
	if !ok {
		return nil, false
	}
	// A source whose repository the GitHub proxy would not show the caller, which only a source
	// written before that rule held its writes can be, is not imported for this caller: its row is
	// answered as it stands, and GitHub is asked nothing about it (hiddenSourceRefusal's rule).
	// Every other source is imported as SyncProject imports it, one at a time in id order. A
	// reader that sees every object, and a deployment with no App key, sync as before, with no
	// statement more (R9).
	if s.deps.GitHub == nil || seesEveryObject(who) {
		return s.syncEverySource(w, r, key)
	}
	held, err := querySources(r.Context(), s.deps.Store.Pool, who, "project_key = $1", "id", key)
	if err != nil {
		s.writeHandlerError(w, err)
		return nil, false
	}
	repos := make([]string, 0, len(held))
	for _, stored := range held {
		repos = append(repos, stored.Repo)
	}
	hidden, err := s.repositoriesHidden(r.Context(), who, repos)
	if err != nil {
		s.writeHandlerError(w, err)
		return nil, false
	}
	sources := make([]model.ArchitectureSource, 0, len(held))
	for _, stored := range held {
		if hidden[stored.Repo] {
			sources = append(sources, stored)
			continue
		}
		source, err := s.deps.Architecture.Sync(r.Context(), key, stored.Repo)
		switch {
		case errors.Is(err, architecture.ErrNoSource):
			// Deleted while this call ran: left out, as SyncProject leaves it out.
		case err != nil && source.Project == "":
			s.writeHandlerError(w, err)
			return nil, false
		default:
			sources = append(sources, source)
		}
	}
	if len(sources) == 0 {
		writeProjectSourceNotFound(w, key)
		return nil, false
	}
	return sources, true
}

// syncEverySource is syncProjectSources for a caller from whom no repository can be hidden: every
// source of project key imported by SyncProject, the rows answered in id order.
func (s *server) syncEverySource(w http.ResponseWriter, r *http.Request, key string) ([]model.ArchitectureSource, bool) {
	results, err := s.deps.Architecture.SyncProject(r.Context(), key)
	if errors.Is(err, architecture.ErrNoSource) || (err == nil && len(results) == 0) {
		writeProjectSourceNotFound(w, key)
		return nil, false
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return nil, false
	}
	sources := make([]model.ArchitectureSource, 0, len(results))
	for _, result := range results {
		if result.Err != nil && result.Source.Project == "" {
			s.writeHandlerError(w, result.Err)
			return nil, false
		}
		sources = append(sources, result.Source)
	}
	return sources, true
}

// writeSourceRefusal answers a source write whose access check failed: an access failure (no App
// key, no installation, no Contents: read, no such branch; architecture.IsAccessFailure) is 409
// SOURCE_ACCESS with its text, and any other failure the handler error.
func (s *server) writeSourceRefusal(w http.ResponseWriter, err error) {
	if architecture.IsAccessFailure(err) {
		writeError(w, "SOURCE_ACCESS", http.StatusConflict, err.Error())
		return
	}
	s.writeHandlerError(w, err)
}

func writeSourceNotFound(w http.ResponseWriter, key, repo string) {
	writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source from "+repo+" configured for "+key)
}

func writeProjectSourceNotFound(w http.ResponseWriter, key string) {
	writeError(w, "SOURCE_NOT_FOUND", http.StatusNotFound, "no architecture source configured for "+key)
}

// sourceProjectVisible answers the caller as a read binds it (readerOf) and whether it sees
// project key (requireVisibleProject), and, where it does not, answers notFound, the answer of a
// source nobody configured, before any lock, delete, GitHub call or sync: a source write to a
// hidden project answers exactly as one to a project with no such source
// (TestNoWriteReachesAHiddenObject). A key no project holds answers the same. A read that fails
// otherwise answers the handler error.
func (s *server) sourceProjectVisible(w http.ResponseWriter, r *http.Request, key string, notFound func()) (model.Actor, bool) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return model.Actor{}, false
	}
	err := s.requireVisibleProject(r.Context(), who, key)
	switch {
	case err == nil:
		return who, true
	case errors.Is(err, pgx.ErrNoRows):
		notFound()
	default:
		s.writeHandlerError(w, err)
	}
	return model.Actor{}, false
}

// hiddenSourceRefusal is what a source of owner/name answers in place of GitHub's answer when the
// GitHub proxy would not show that repository to who (repositoriesHidden, over
// refs.RepositoryClause): exactly what a repository the App is not installed on answers, with
// GitHub asked nothing - the held rate limit on the App's scope where one is in force, as the
// installation lookup of an uninstalled repository is held unsent then, and else the
// not-installed error that lookup returns, which CheckSource and the sync's installation read
// both answer. A source imports its repository's `.dispatch/architecture` prose into the project,
// which every reader of the project then reads, and the not-installed answer would otherwise tell
// who that the App reads a repository a private project's issues use. Nil where the repository is
// not hidden from who, and with no App key, which answers every repository alike before this is
// asked. A reader that sees every object, as every open viewer of a deployment with no private
// project does, reads no statement for it (R9).
func (s *server) hiddenSourceRefusal(ctx context.Context, who model.Actor, owner, name string) (refusal error, err error) {
	if s.deps.GitHub == nil {
		return nil, nil
	}
	repo := owner + "/" + name
	hidden, err := s.repositoriesHidden(ctx, who, []string{repo})
	if err != nil || !hidden[repo] {
		return nil, err
	}
	if limited := s.deps.GitHub.HeldLimit(ctx, githubapp.AppScope); limited != nil {
		return limited, nil
	}
	return fmt.Errorf("%w on %s/%s", githubapp.ErrNoInstallation, owner, name), nil
}

// sourcePathRepo reads the {owner}/{repo} path segments as parseSourceRepo reads a repository.
func sourcePathRepo(r *http.Request) (owner, name string, err error) {
	return parseSourceRepo(r.PathValue("owner") + "/" + r.PathValue("repo"))
}

// parseSourceRepo reads raw, trimmed of the space around it, as a repository GitHub could hold, in
// the form repository mappings are stored, and answers that form's halves: refs.ParseGitHubRepo,
// which lower-cases it, trims one trailing `.git` and holds the result to GitHub's names, so a
// stored name puts no `..` into a GitHub path. Any other raw is 400 SOURCE_INPUT. A source's
// repository, a mapping's path and its renamed_to, and the delivery settings' deploy repository
// are each read through it.
func parseSourceRepo(raw string) (owner, name string, err error) {
	owner, name, err = refs.ParseGitHubRepo(strings.TrimSpace(raw))
	if err != nil {
		return "", "", errorf(http.StatusBadRequest, "SOURCE_INPUT", "repository must be owner/name, got %q", raw)
	}
	return owner, name, nil
}
