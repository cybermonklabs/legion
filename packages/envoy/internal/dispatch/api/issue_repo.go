package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
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
