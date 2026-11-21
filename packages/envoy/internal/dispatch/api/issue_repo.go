package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The two halves of a repository GitHub could hold, copied from the daemon's own reading of the
// same grammar (packages/daemon/internal/ghrepo's ownerName and repositoryName): packages/envoy
// cannot import packages/daemon, and the two gates of one product must hold a repository to one
// rule, so the rule is written out here with its source named. An owner is a letter or digit and
// then up to 38 letters, digits, hyphens and underscores, which is looser than GitHub's sign-up
// form on purpose: that form's rule, single hyphens with none leading or trailing, binds new
// accounts alone, and GitHub still serves a login with a doubled hyphen (`hello--world`, the
// organisation `foo--bar`), one ending in a hyphen (`ArtOfCode-`) and a managed user's
// `_<shortcode>` (`mona-cat_octo`), whose repositories an issue has to be able to name. A
// repository's own name is at most 100 letters, digits, `.`, `-` and `_`. Both cases are written
// out because the grammar is the grammar whatever the case, though the value these are read
// against is lowercased first (canonicalRepo), so a name GitHub holds is taken however it was
// typed. The two names `.` and `..`, which no working copy could hold, are refused beside the
// patterns, where a pattern would state them worse.
var (
	repoOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	repoNamePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// githubCouldHoldRepo answers whether GitHub could hold a repository at this canonical
// `owner/name`. Since migration 20261112004945 no repo_projects row stands behind an issue's own
// repository, so this grammar is the whole gate on the value, and the value goes on to GitHub
// reads of its own: the CI repair asks GitHub about every distinct repository an issue names
// (cireconcile's covered), once a pass, and the Legion daemon prepares a working copy from it. A
// name GitHub's grammar refuses can only answer "no such repository" there, every pass, so it is
// refused at the write instead.
func githubCouldHoldRepo(canonical string) bool {
	owner, name, found := strings.Cut(canonical, "/")
	if !found || !repoOwnerPattern.MatchString(owner) {
		return false
	}
	return name != "." && name != ".." && repoNamePattern.MatchString(name)
}

// parseIssueRepo decodes the tri-state `repo` field of an issue write. Absent → (nil, false): leave
// it alone. JSON null → (nil, true): clear it, so the issue resolves through its parent chain and the
// project's default again. A string → its canonical `owner/name` (externalRef's canonicalisation,
// which lowercases and drops a `.git` suffix), provided GitHub could hold a repository of that
// name (githubCouldHoldRepo); anything else → 400 INVALID_ISSUE. The grammar is checked here and
// not in externalRef, which this borrows the shape check and the canonicalisation from, because
// externalRef also resolves a reference to an external issue already linked
// (resolveIssueRef): a row written before this gate existed is still to be found by the
// reference that names it, and refusing the grammar there would answer INVALID_ISSUE_REF for a
// link the server itself stored.
func parseIssueRepo(raw json.RawMessage) (repo *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", `repo must be "owner/name" or null`)
	}
	canonical, _, err := externalRef(strings.TrimSpace(value) + "#1")
	if err != nil || !githubCouldHoldRepo(canonical) {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", `repo must be "owner/name" (got %q)`, value)
	}
	return &canonical, true, nil
}

// refuseRepoOfRestrictedMember refuses an issue write by a restricted member, or by a credential
// acting for one, that names `repo` at all, a string or null, and answers whether it did. An issue's
// own repository may name any owner/name, and every repository an issue resolves to is one its
// project owns (refs.ProjectRepositoriesSQL), which a viewer that is not open reads through the
// GitHub proxy, the App-installation route and the Delivery views once a project it sees owns it
// (refs.RepositoryClause): a restricted member who named one could make any repository the App
// reaches theirs to read. So their issues take the repository they resolve to, their nearest
// parent's or their project's default, and a restricted member sets no issue's own, nor changes or
// clears one a full member set. A full member's write is refused nothing here and answers as
// before (R9).
//
// It is 403 RESTRICTED_MEMBER, the code a restricted member's write of the deployment is refused
// with (refuseDeploymentWrite), and it runs before the write reads the issue, its project or its
// parent and before anything is written: it depends on the caller and the body alone, so it
// answers alike for an issue or a project the caller does not see and for one nobody holds.
func refuseRepoOfRestrictedMember(w http.ResponseWriter, who model.Actor, raw json.RawMessage) bool {
	if len(raw) == 0 || who.Viewer == nil || !who.Viewer.Restricted {
		return false
	}
	writeError(w, codeRestrictedMember, http.StatusForbidden,
		"a restricted member cannot set or clear an issue's repo; only a full member names an issue's own repository")
	return true
}

// codeRepositoryNotAvailable is the code refuseRepoHidden refuses a write with.
const codeRepositoryNotAvailable = "REPOSITORY_NOT_AVAILABLE"

// repositoryHiddenFromWriterQuery is refs.RepositoryHidden over the repository bound at $1 for the
// writer bound at $2: the GitHub proxy's gate (routes' repositoryVisibleQuery), negated. No
// placeholder differs between writes, so the text is built once.
var repositoryHiddenFromWriterQuery = `select ` + refs.RepositoryHidden("$1::text", "$2")

// refuseRepoHidden refuses a write by who that makes repo, canonical, an issue's own repository or
// a project's default, when the GitHub proxy hides that repository from who: refs.RepositoryHidden
// with who as the viewer. Every repository an issue resolves to is one its
// project owns, and the proxy shows a repository to everyone who sees an issue that resolves to
// it, so a write that gave an issue of a project who sees a repository the proxy hides from who
// would give who that repository to read, through the proxy and Delivery. The proxy's gate is the
// one test, so the write and the read cannot come to different answers: a repository that only
// issues of private projects who does not see resolve to is refused, and a repository an issue who
// sees already resolves to, a shared one, is not. Clearing a repository (repo nil) is refused
// nothing.
//
// It is 403 REPOSITORY_NOT_AVAILABLE in words that name no project and no issue, and do not say
// why the repository is hidden, the same for every spelling of the name. It reads through q, the
// write's transaction, after the write has read its issue or its project as who reads them, so an
// object who does not see still answers as one nobody holds, and before anything is written. The
// server's own viewer and every viewer of a deployment with no private project read nothing for
// it and are refused nothing, as before (R9): there the clause would hide from a credential that
// lists projects a repository no issue of its projects resolves to yet, which it has always named.
func refuseRepoHidden(ctx context.Context, q queryer, who model.Actor, repo *string) error {
	if repo == nil || (who.Viewer != nil && (who.Viewer.All || !who.Viewer.AnyPrivate)) {
		return nil
	}
	var hidden bool
	if err := q.QueryRow(ctx, repositoryHiddenFromWriterQuery, *repo, refs.VisibilityArg(who)).Scan(&hidden); err != nil {
		return fmt.Errorf("read whether the writer may read %s: %w", *repo, err)
	}
	if hidden {
		return errorf(http.StatusForbidden, codeRepositoryNotAvailable,
			"this repository is not available to the caller: an issue's repo or a project's default repository must be one the caller can read through Dispatch")
	}
	return nil
}

// repoScan receives refs.IssueRepoColumns for one issue, the repository-resolution rule the
// server keeps in one place: refs.IssueRepository reads the same terms for a single key, for
// the results read and the pull-request head attribution, so an issue read and those two
// cannot name different repositories for the same issue.
type repoScan struct {
	own, resolved *string
	tooDeep       bool
}

func (r *repoScan) targets() []any { return []any{&r.own, &r.resolved, &r.tooDeep} }

// resolve answers the issue's own and resolved repository, refusing a parent chain the walk could
// not finish within parentDepthCap links: such a chain names no answer, and the project's default
// would be a wrong one.
func (r *repoScan) resolve(key string) (own, resolved *string, err error) {
	if r.tooDeep {
		return nil, nil, fmt.Errorf("resolve the repository of %s: its parent chain is deeper than %d links or loops", key, parentDepthCap)
	}
	return r.own, r.resolved, nil
}
