package api

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// repo_project_repairs.go names the request of the settings route that mends a mapping whose
// name, or whose renamed_to, is no repository GitHub could hold: what the CI reconcile's WARN
// names beside its skip of such a name (cireconcile.Reconciler.MappingRepairs). Each reader here
// inverts a reader of the route beside it in repo_projects.go, so the request the WARN names is
// one the route reads as that mapping: spelledPath inverts the DELETE's
// deletedRepoProjectSpelling, and mappingPath the PUT's repoProjectPath. The route test
// (repo_project_repairs_test.go) sends each request through the route itself.

// repoProjectsPath is the settings route's path a mapping's {owner}/{repo} follows.
const repoProjectsPath = "/api/v1/settings/repo-projects/"

// MappingRepairs is mappingRepairs bound to pool, which cmd/dispatch gives the CI reconcile
// (cireconcile.Reconciler.MappingRepairs): package cireconcile cannot call this package, which
// imports it.
func MappingRepairs(pool *store.Pool) func(ctx context.Context, row string) []any {
	return func(ctx context.Context, row string) []any { return mappingRepairs(ctx, pool, row) }
}

// mappingRepairs is what the CI reconcile names beside its skip of row, a name refs.SplitGitHubRepo
// refuses that a mapping names, so the person reading the WARN can mend it. The reconcile names a
// mapping by its canonical name, which is its renamed_to where it names one, so row is one of two
// kinds: a mapping stored under row, which its removal mends (`removed_by`, mappingRemoval), or
// the renamed_to of a mapping stored under another name, which no mapping is stored under, so a
// removal of row removes nothing, and which that mapping's PUT mends instead, with a renamed_to
// GitHub allows or `""`, which clears it (`mended_by`, mappingRename). Where no PUT writes the
// mapping's own name, which the PUT refuses or reads as another mapping's, the mapping's removal
// mends it in the PUT's place (`removed_by`). Both are read from repo_projects when the WARN is
// written, rather than carried through every pass for a row that is almost never refused. A read
// that fails is named in their place.
func mappingRepairs(ctx context.Context, pool *store.Pool, row string) []any {
	rows, err := pool.Query(ctx, `select repo, renamed_to is not null from repo_projects
		where lower(coalesce(renamed_to, repo)) = lower($1) order by repo`, row)
	if err != nil {
		return []any{"error", fmt.Errorf("read the mappings that name it: %w", err)}
	}
	defer rows.Close()
	var removedBy, mendedBy []string
	for rows.Next() {
		var repo string
		var renamed bool
		if err := rows.Scan(&repo, &renamed); err != nil {
			return []any{"error", fmt.Errorf("read a mapping that names it: %w", err)}
		}
		if renamed {
			if put, ok := mappingRename(repo); ok {
				mendedBy = append(mendedBy, put)
				continue
			}
		}
		removedBy = append(removedBy, mappingRemoval(repo))
	}
	if err := rows.Err(); err != nil {
		return []any{"error", fmt.Errorf("read the mappings that name it: %w", err)}
	}
	var repairs []any
	if len(removedBy) > 0 {
		repairs = append(repairs, "removed_by", strings.Join(removedBy, ", "))
	}
	if len(mendedBy) > 0 {
		repairs = append(repairs, "mended_by", strings.Join(mendedBy, ", "))
	}
	return repairs
}

// mappingRemoval is the request that removes the mapping stored under repo: the settings route's
// DELETE of the path that spells repo as it is stored (spelledPath), since that DELETE removes
// only the mapping stored under its path's own spelling (deleteRepoProject). A mapping stored
// under `acme/..` before the route held names to GitHub's rule is removed by
// `DELETE /api/v1/settings/repo-projects/acme/%2E%2E`, one stored under `acme/.git` by
// `DELETE /api/v1/settings/repo-projects/acme/.git`, one stored under `Zeta/..` by
// `DELETE /api/v1/settings/repo-projects/Zeta/%2E%2E`, and one direct SQL stored under `/a/b` by
// `DELETE /api/v1/settings/repo-projects/%2Fa/b`. A mapping no path spells (spelledPath), such as
// one stored under an empty name (`acme/`, which a PUT of `acme/.git` stored before the rule), is
// removed by no request, and the text says so and that it is removed by SQL.
func mappingRemoval(repo string) string {
	if path, ok := spelledPath(repo); ok {
		return "DELETE " + path
	}
	return fmt.Sprintf("no request reaches the mapping stored under %q: remove it by SQL", repo)
}

// spelledPath is the settings route's path whose DELETE reads it as repo, and false where no
// path's does. The DELETE reads a path's two segments as the mux unescapes them, joined by `/`
// (deletedRepoProjectSpelling), so a path spells repo wherever one `/` of repo splits it into two
// segments the mux carries. The mux splits the escaped path at each `/` it holds and unescapes
// each segment after that (net/http's firstSegment), so each half is escaped (rawSegment) and a
// `/` inside a half is written `%2F`. It matches a wildcard to any segment but two: an empty one,
// which it cleans out of a `//` (cleanPath) or reads as the trailing slash that ends a path, and
// one that is `/` alone, which it reads as that trailing slash too (routingNode.matchPath). So each
// `/` of repo is tried in turn, and the first that leaves both halves neither empty nor `/` is the
// path's: `/a/b` is `%2Fa/b`. A repo none of whose `/` leaves such halves is no path's spelling:
// one with no `/`, one whose only `/` begins or ends it (`/acme`, `acme/`), and one whose every
// `/` leaves nothing or a lone `/` on one side (`acme//`, `//acme`).
func spelledPath(repo string) (string, bool) {
	for at := range len(repo) {
		if repo[at] != '/' {
			continue
		}
		owner, name := repo[:at], repo[at+1:]
		if owner != "" && owner != "/" && name != "" && name != "/" {
			return repoProjectsPath + rawSegment(owner) + "/" + rawSegment(name), true
		}
	}
	return "", false
}

// mappingRename is the request that mends the renamed_to of the mapping stored under repo: its
// PUT (mappingPath), which takes the mapping's project and a renamed_to GitHub allows, or `""` to
// clear it. ok is false where no PUT writes the mapping stored under repo.
func mappingRename(repo string) (put string, ok bool) {
	path, ok := mappingPath(repo)
	if !ok {
		return "", false
	}
	return `PUT ` + path + ` with its project and a renamed_to GitHub allows, or "" to clear it`, true
}

// mappingPath is the settings route's path whose PUT writes the mapping stored under repo, and
// false where no path's does. The PUT writes the name its path reads as (storedName), and that
// reading trims one trailing `.git` off the name, so the name is written as it is where that reads
// as repo, and otherwise with one `.git` more, which the trim takes off again: `acme/x.git`, which
// as it is reads as `acme/x`, is written `acme/x.git.git`. Whatever path reads as repo, the name
// with that one `.git` more reads as repo too, so where neither form does, no PUT writes the
// mapping: a name with a letter the reading lower-cases, a half it trims of surrounding space, an
// empty owner, ASCII whitespace in either half or a `/` or `#` in the name, which the reading
// refuses, and a name GitHub's rule refuses as it is stored (`acme/..`, `acme/.git`, `acme/`),
// which the PUT refuses.
func mappingPath(repo string) (string, bool) {
	owner, name, _ := strings.Cut(repo, "/")
	for _, segment := range []string{name, name + ".git"} {
		if read, valid := storedName(owner, segment); valid && read == repo {
			return repoProjectsPath + rawSegment(owner) + "/" + rawSegment(segment), true
		}
	}
	return "", false
}

// storedName is the mapping name the settings route's PUT writes from the path segments owner and
// name, as the mux unescapes them, and false where the PUT refuses them: repoProjectPath reads
// them through parseSourceRepo, canonicalised (refs.ParseExternalRepo) and held to GitHub's rule
// as stored (refs.SplitGitHubRepo).
func storedName(owner, name string) (string, bool) {
	owner, name, err := parseSourceRepo(owner + "/" + name)
	return owner + "/" + name, err == nil
}

// rawSegment is segment as a request path carries it: escaped, and `.` and `..` written as `%2E`
// and `%2E%2E`, which the mux does not clean away as it cleans a `.` or `..` segment. A caller
// that sends the path as it is written keeps them; a browser's URL parser resolves them away as
// it does `.` and `..` (deletedRepoProjectSpelling names the callers of each kind).
func rawSegment(segment string) string {
	if segment == "." || segment == ".." {
		return strings.ReplaceAll(segment, ".", "%2E")
	}
	return url.PathEscape(segment)
}
