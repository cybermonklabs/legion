package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *server) listRepoProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+repoProjectColumns+`
		from repo_projects
		order by repo
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()

	mappings := []model.RepoProject{}
	for rows.Next() {
		mapping, err := scanRepoProject(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, mappings)
}

// putRepoProjectInput is the body of PUT /api/v1/settings/repo-projects/{owner}/{repo}.
type putRepoProjectInput struct {
	Project   string       `json:"project" required:"true"`
	Default   *bool        `json:"default"`
	RenamedTo *string      `json:"renamed_to"`
	Actor     *model.Actor `json:"actor"`
}

// putRepoProject maps a repository to a project. Default, when given, sets or clears the mapping as
// its project's default, and setting it demotes the project's previous default in the same
// transaction; left out, the mapping keeps its default while its project is unchanged, and a mapping
// moved to another project is never that project's default unless the body says so. A mapping
// that issues name as their repository moves like any other: an issue's own repository is any
// owner/name and needs no mapping, so no issue holds a mapping in place. Every mapping the write
// changes emits settings.repo_project.updated on its project, and a moved mapping emits one on
// the project it left too, deleted:true there. lockRepoProjectWrite orders the write against
// every other one.
//
// Renamed_to is the full `owner/name` this repository is called today, canonicalised and held to
// GitHub's names as a source repository is (parseRepoRename). **Absent, and JSON null, leave the
// stored pair exactly as it is**, as Default does, and `""` clears it: the page and the client
// that write this route send `{project, default}` and nothing else
// (packages/dispatch/web/src/api/client.ts, putRepoProject), so clearing on absence would drop an
// operator's rename every time someone moved a mapping's project or its default in the
// dashboard. A pair naming this mapping's own
// repository, one naming a repository that is itself renamed, and one on a repository another
// mapping is already renamed to are each refused (400 INVALID_REPO_PROJECT), so the canonical
// name of a repository is reached in one step and no cycle exists (refs.CanonicalRepo).
func (s *server) putRepoProject(w http.ResponseWriter, r *http.Request) {
	owner, name, err := repoProjectPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo := owner + "/" + name
	var input putRepoProjectInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	input.Project = strings.TrimSpace(input.Project)
	if !projectKeyPattern.MatchString(input.Project) {
		writeError(w, "INVALID_REPO_PROJECT", http.StatusBadRequest, projectKeyProblem("project", input.Project))
		return
	}
	renamedTo, err := parseRepoRename(repo, input.RenamedTo)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), "select key from projects where key = $1", input.Project).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
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
	previous, err := lockRepoProjectWrite(r.Context(), tx, s.deps.Events, repo, actor, input.Project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	moved := previous != nil && previous.Project != input.Project
	isDefault := previous != nil && !moved && previous.Default
	if input.Default != nil {
		isDefault = *input.Default
	}
	// An absent or null renamed_to keeps the pair the mapping already holds, and a row this
	// write creates holds none; `""` clears one, which adds no edge to the rename graph and so
	// needs no check of it.
	switch {
	case input.RenamedTo == nil:
		if previous != nil {
			renamedTo = previous.RenamedTo
		}
	case renamedTo != nil:
		if err := refuseRepoRenameChain(r.Context(), tx, repo, *renamedTo); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var changed []model.RepoProject
	if isDefault {
		rows, err := tx.Query(r.Context(), `
			update repo_projects set is_default = false
			where project = $1 and is_default and repo <> $2
			returning `+repoProjectColumns, input.Project, repo)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.RepoProject, error) { return scanRepoProject(row) })
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	mapping, err := scanRepoProject(tx.QueryRow(r.Context(), `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ($1, $2, $3, $4, $5)
		on conflict (repo) do update set project = excluded.project, created_by = excluded.created_by,
			is_default = excluded.is_default, renamed_to = excluded.renamed_to
		returning `+repoProjectColumns, repo, input.Project, actorJSON, isDefault, renamedTo))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var events []model.Event
	appendMappingEvent := func(mapping model.RepoProject, deleted bool) bool {
		event, err := s.appendEvent(r.Context(), tx, repoProjectEvent(mapping, actor, deleted))
		if err != nil {
			s.writeHandlerError(w, err)
			return false
		}
		events = append(events, event)
		return true
	}
	for _, demoted := range changed {
		if !appendMappingEvent(demoted, false) {
			return
		}
	}
	if moved && !appendMappingEvent(*previous, true) {
		return
	}
	if !appendMappingEvent(mapping, false) {
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), events...)
	WriteJSON(w, http.StatusOK, mapping)
}

// deleteRepoProject removes a mapping, whether or not issues name its repository: an issue's own
// repository is any owner/name and needs no mapping, so an issue that names this repository
// keeps it and still resolves to it. An issue that names none resolves, as before, through its
// parent chain and then its project's default, which is gone where this mapping was that default.
//
// Its path removes the mapping stored under the path's own spelling (deletedRepoProjectSpelling)
// and no other. The lookup is `repo = $1` on the table's primary key, which compares the stored
// name with the spelling byte for byte. A path the canonical reading (refs.ParseExternalRepo,
// lower-cased and one trailing `.git` trimmed) reads as itself, `acme/x`, so names the canonical
// row, and a path that reading reads as another name or refuses, `acme/x.git`, `Acme/X`,
// `acme/x.git.git` or `acme/a%20b`, names only the mapping stored under that exact spelling, never
// the row the reading names. So of `acme/x.git` (from a PUT of `acme/x.git.git`) and `acme/x`
// stored together, each is removed by its own spelling, and the DELETE sent again removes nothing
// more. A path that names no stored mapping is 204 where the canonical reading takes it, and 400
// where that reading refuses it (checkRepoProjectPath).
//
// The write takes the locks of the path's own spelling (lockRepoProjectWrite), so the mapping
// stored under it cannot appear or go between the lookup and the removal.
func (s *server) deleteRepoProject(w http.ResponseWriter, r *http.Request) {
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	spelled := deletedRepoProjectSpelling(r)
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	previous, err := lockRepoProjectWrite(r.Context(), tx, s.deps.Events, spelled, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if previous == nil {
		if err := checkRepoProjectPath(spelled); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from repo_projects where repo = $1`, previous.Repo); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, repoProjectEvent(*previous, actor, true))
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

// lockRepoProjectWrite takes, in this order, every lock a write to repo's mapping needs, and returns
// the mapping as it stands under them (nil when repo has none). projects are the projects the write
// may put the mapping in.
//
//   - repo's own advisory lock, which every writer of the mapping takes whether or not its row
//     exists yet, so the mapping's project and its existence hold still from here on (a default
//     switch in its project may still clear its is_default until the project lock below is held,
//     and the row read after that lock sees it);
//   - the event owners of the mapping's current project and of projects, in one LockOwners call
//     before the first event, which serializes each project's default switch and orders two
//     moves between the same projects;
//   - the mapping's row, for update, which holds the row and its project still until this write
//     commits, so the default it read and the project it is moving the mapping out of cannot
//     change under it. No issue write takes a lock on this row: an issue names any owner/name as
//     its repository, mapped or not, so nothing an issue write does depends on this row.
//
// Every writer of a mapping goes through here: the PUT, the DELETE and the boot seed
// (SeedRepoProjects).
func lockRepoProjectWrite(ctx context.Context, tx pgx.Tx, broker *events.Broker, repo string, actor model.Actor, projects ...string) (*model.RepoProject, error) {
	if _, err := tx.Exec(ctx, `
		select pg_advisory_xact_lock(hashtext('dispatch-repo-project'), hashtext($1))
	`, repo); err != nil {
		return nil, fmt.Errorf("lock repository mapping %s: %w", repo, err)
	}
	var current string
	err := tx.QueryRow(ctx, `select project from repo_projects where repo = $1`, repo).Scan(&current)
	switch {
	case err == nil:
		projects = append(projects, current)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if len(projects) == 0 {
		return nil, nil
	}
	owners := make([]model.Event, 0, len(projects))
	for _, project := range projects {
		owners = append(owners, projectOwner(project).event("", actor, nil))
	}
	if err := broker.LockOwners(ctx, tx, owners...); err != nil {
		return nil, err
	}
	mapping, err := scanRepoProject(tx.QueryRow(ctx, `
		select `+repoProjectColumns+` from repo_projects where repo = $1 for update
	`, repo))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mapping, nil
}

// SeedRepoProjects adds the DISPATCH_REPO_PROJECTS mappings (`owner/repo=KEY,...`) whose
// repository has no mapping yet, at boot; a mapping already stored, set in the dashboard, wins.
// Each repository is seeded in its own transaction under a mapping write's locks
// (lockRepoProjectWrite), so a seed racing a PUT or DELETE of the same repository waits for it and
// then sees what it left. A mapping the seed adds emits settings.repo_project.updated on its project,
// deleted:false, as the PUT's does, actor the environment session; a repository already mapped, in
// the seeded project or another, gets no write and no event. A project that does not exist refuses
// the seed, naming the repository and the project.
func SeedRepoProjects(ctx context.Context, database *store.Store, broker *events.Broker, raw string) error {
	mappings, err := ParseRepoProjects(raw)
	if err != nil {
		return err
	}
	repos := make([]string, 0, len(mappings))
	for repo := range mappings {
		repos = append(repos, repo)
	}
	slices.Sort(repos)
	for _, repo := range repos {
		if err := seedRepoProject(ctx, database, broker, repo, mappings[repo]); err != nil {
			return fmt.Errorf("seed repository project %q: %w", repo, err)
		}
	}
	return nil
}

func seedRepoProject(ctx context.Context, database *store.Store, broker *events.Broker, repo, project string) error {
	actor := model.Actor{Kind: "session", ID: "environment"}
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		return err
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from projects where key = $1)`, project).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("project %s does not exist", project)
	}
	previous, err := lockRepoProjectWrite(ctx, tx, broker, repo, actor, project)
	if err != nil || previous != nil {
		return err
	}
	mapping, err := scanRepoProject(tx.QueryRow(ctx, `
		insert into repo_projects (repo, project, created_by)
		values ($1, $2, $3)
		returning `+repoProjectColumns, repo, project, actorJSON))
	if err != nil {
		return err
	}
	event, err := broker.Append(ctx, tx, repoProjectEvent(mapping, actor, false))
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	broker.Publish(event)
	return nil
}

// repoProjectEvent is settings.repo_project.updated for one mapping, on the mapping's project:
// deleted when the mapping left that project (removed, or moved to another).
func repoProjectEvent(mapping model.RepoProject, actor model.Actor, deleted bool) model.Event {
	return projectOwner(mapping.Project).event(
		"settings.repo_project.updated", actor, map[string]any{"mapping": mapping, "deleted": deleted},
	)
}

func (s *server) repoProject(ctx context.Context, repo string) (string, error) {
	var project string
	err := s.deps.Store.Pool.QueryRow(ctx, "select project from repo_projects where repo = $1", repo).Scan(&project)
	if err == nil {
		return project, nil
	}
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return "", err
}

// repoProjectPath reads the {owner}/{repo} path of a mapping write, and of the App's installation
// read (getAppInstallation), as parseSourceRepo reads a repository (sourcePathRepo): an owner and
// a name of the characters GitHub allows, and never a name of `.` or `..`, before or after a
// trailing `.git` is trimmed, canonicalised, held to that rule as it is stored, and answered as
// its two halves. A path the rule refuses is 400 INVALID_REPO_PROJECT. The mapping's repository
// becomes the repository of every issue of its project that names none, whose halves delivery
// puts into GitHub paths, and the installation read puts its halves into one at once; `...git`
// and `.git` are no dot segments, so the mux does not clean them, and the trailing `.git`
// refs.ExternalRepo trims would leave `..` or an empty name behind.
func repoProjectPath(r *http.Request) (owner, name string, err error) {
	owner, name, err = sourcePathRepo(r)
	if err != nil {
		return "", "", errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT", `the path must name a repository "owner/name" (got %q)`,
			r.PathValue("owner")+"/"+r.PathValue("repo"))
	}
	return owner, name, nil
}

// deletedRepoProjectSpelling is the {owner}/{repo} path of a mapping removal as it is spelled:
// its two segments as the mux unescapes them, joined by `/`, nothing trimmed and nothing
// lower-cased, so it names a mapping stored under any name the mux carries in two segments. The
// mux unescapes each segment after it splits the path, so a `/` in a name is written `%2F`, and a
// `.` or `..` segment `%2E` or `%2E%2E`. A caller that sends the path as it is written keeps such
// a segment: curl with `--path-as-is`, or a Go client building the path by hand. A browser's URL
// parser (WHATWG URL) reads `%2E` and `%2E%2E` as dot segments, as it reads `.` and `..`, and
// resolves them away before the request is sent. The dashboard's DELETE is such a request: it
// splits the stored name on `/` and puts each half into the path through encodeURIComponent,
// which leaves a dot as it is (packages/dispatch/web/src/api/paths.ts, repoParams and routeUrl),
// so its DELETE of a mapping stored under `acme/..` is sent as `/api/v1/settings/repo-projects/`,
// no path of this route. An empty segment, and one that is `/` alone, reaches no wildcard of the
// route, so a name each of whose `/` leaves nothing or a lone `/` on one side is no path's
// spelling (spelledPath, which inverts this reading for the CI reconcile's WARN). No GitHub path
// is built from it here.
func deletedRepoProjectSpelling(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo")
}

// checkRepoProjectPath refuses the spelling of a mapping removal's {owner}/{repo} path where the
// canonical reading refuses it (refs.ParseExternalRepo), as every mapping's path was read before
// repoProjectPath held it to GitHub's names: 400 INVALID_ISSUE_REF, and nil where that reading
// takes it. A removal whose path names no stored mapping answers by it: 204 where it is nil, and
// its 400 otherwise. Space before the owner is trimmed. After the name, a space, a tab, a newline,
// a carriage return or a form feed is refused, since nothing trims it, while any other space, such
// as a vertical tab or U+00A0, the reading trims (refs.ParseExternalRepo). That is how the route
// read a path when it read it as the reference `<path>#1`, whose leading space alone a trim of the
// reference took off. No GitHub path is built from it here.
func checkRepoProjectPath(spelled string) error {
	if _, ok := refs.ParseExternalRepo(strings.TrimLeftFunc(spelled, unicode.IsSpace)); !ok {
		return errorf(http.StatusBadRequest, "INVALID_ISSUE_REF", `the path must name a repository "owner/name" (got %q)`, spelled)
	}
	return nil
}

// parseRepoRename reads the `renamed_to` of a mapping write: nil for an absent or null field,
// which leaves the stored pair alone, nil for `""`, which clears it, and otherwise the
// canonical `owner/name` the value names. The value is held to the rule a source repository is
// (parseSourceRepo): an owner and a name of the characters GitHub allows, and never a name of
// `.` or `..`, before or after a trailing `.git` is trimmed, in the form it is stored. The pair
// becomes the name every comparison of the repository asks GitHub about, and delivery puts its
// two halves into GitHub paths as they are (refs.SplitGitHubRepo), so `acme/..` would ask about
// a path above the repository's. A pair naming the mapping's own repository is refused here,
// where the one cycle a single row can hold is cheapest to see; the database refuses it too, so
// direct SQL is held to it (migration 20261105075455_repo_projects_renamed_to).
func parseRepoRename(repo string, raw *string) (*string, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	owner, name, err := parseSourceRepo(*raw)
	if err != nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT", `renamed_to must be "owner/name" (got %.140q)`, *raw)
	}
	canonical := owner + "/" + name
	if canonical == repo {
		return nil, errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT", "%s cannot be renamed to itself", repo)
	}
	return &canonical, nil
}

// refuseRepoRenameChain holds the rename graph to one step for a write that sets the pair
// repo → renamedTo: it refuses a target that is itself renamed, and refuses renaming a
// repository some other mapping is already renamed to. Either would put the canonical name of
// a repository two steps away, which no reader walks (refs.CanonicalRepo takes one step), and
// together they make a cycle impossible. The target's own step is read through
// refs.CanonicalRepo, the rule itself, so what this route refuses and what every reader resolves
// are one answer: a target that canonicalises to itself is the end of the chain, compared
// without case as every comparison of a repository is, since the column's own check refuses only
// a rename to the identical spelling.
//
// It takes one lock over the whole graph first, since the rows it must hold still are not this
// mapping's own: the writer already holds its own mapping's locks by here
// (lockRepoProjectWrite) and takes no other mapping's, so this lock is always the last one
// taken and two renames cannot wait on each other.
func refuseRepoRenameChain(ctx context.Context, tx pgx.Tx, repo, renamedTo string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-repo-rename'), 0)`); err != nil {
		return fmt.Errorf("lock the repository renames: %w", err)
	}
	var pointedAt string
	switch err := tx.QueryRow(ctx, `
		select repo from repo_projects where lower(renamed_to) = lower($1) limit 1
	`, repo).Scan(&pointedAt); {
	case err == nil:
		return errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT",
			"%s is what %s was renamed to, so it cannot be renamed to %s as well; point %s at the new name instead",
			repo, pointedAt, renamedTo, pointedAt)
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	onwards, err := refs.CanonicalRepo(ctx, tx, renamedTo)
	if err != nil {
		return err
	}
	if strings.EqualFold(onwards, renamedTo) {
		return nil
	}
	return errorf(http.StatusBadRequest, "INVALID_REPO_PROJECT",
		"%s was itself renamed to %s, so %s cannot be renamed to it; name %s instead", renamedTo, onwards, repo, onwards)
}

// repoProjectColumns are the columns scanRepoProject reads, in its order.
const repoProjectColumns = `repo, project, is_default, created_by, created_at, renamed_to`

func scanRepoProject(row rowScanner) (model.RepoProject, error) {
	var mapping model.RepoProject
	var createdBy []byte
	if err := row.Scan(&mapping.Repo, &mapping.Project, &mapping.Default, &createdBy, &mapping.CreatedAt, &mapping.RenamedTo); err != nil {
		return model.RepoProject{}, err
	}
	if err := json.Unmarshal(createdBy, &mapping.CreatedBy); err != nil {
		return model.RepoProject{}, fmt.Errorf("decode repository project author: %w", err)
	}
	return mapping, nil
}
