package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// parseIssueRepo decodes a tri-state `repo` field: an issue's own repository on an issue write,
// and a project's default repository on PUT /api/v1/projects/{key}/default-repository. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it, so the issue resolves through
// its parent chain and the project's default again, or the project has no default. A string,
// trimmed of the space around it → its canonical `owner/name`, provided GitHub could hold a
// repository of that name (refs.ParseGitHubRepo, which lowercases, drops a `.git` suffix and
// holds the stored form to GitHub's names); anything else → 400 with code, the write's own
// invalid-input code (INVALID_ISSUE, INVALID_PROJECT). Since migration 20261112004945 no
// repo_projects row stands behind an issue's own repository, and a project's default repository
// is a column of the project, so this is the whole gate on the value, and the value goes on to
// GitHub reads of its own: the CI repair asks GitHub about every distinct repository an issue
// names or a project has as its default (cireconcile's covered), once a pass, and the Legion
// daemon prepares a working copy from it. A name GitHub's rule refuses can only answer "no such
// repository" there, every pass, so it is refused at the write instead. The rule is held here
// and not in the reading of an issue reference's repository (refs.ParseExternalRef), because
// externalRef resolves a reference to an external issue already linked (resolveIssueRef): a row
// written before this gate existed is still to be found by the reference that names it, and
// refusing the rule there would answer INVALID_ISSUE_REF for a link the server itself stored.
func parseIssueRepo(raw json.RawMessage, code string) (repo *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, errorf(http.StatusBadRequest, code, `repo must be "owner/name" or null`)
	}
	owner, name, err := refs.ParseGitHubRepo(strings.TrimSpace(value))
	if err != nil {
		return nil, true, errorf(http.StatusBadRequest, code, `repo must be "owner/name" (got %q)`, value)
	}
	canonical := owner + "/" + name
	return &canonical, true, nil
}

// renamedRepo is repo under its new name where a rename pair renames it away
// (refs.Renames.Canonical). nil, no repository, stays nil. An issue's create and patch and a
// project's default repository write take it, so no write stores an old name, and the results
// read takes it for the repository a check's issue resolves to (checkIssueRepository).
func (s *server) renamedRepo(repo *string) *string {
	if repo == nil {
		return nil
	}
	canonical := s.deps.Renames.Canonical(*repo)
	return &canonical
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

// checkIssueRepository is the repository a check's issue resolves to: its own, the nearest one
// above it, or its project's default (refs.IssueRepository), under the canonical name that
// repository is called today (refs.Renames.Canonical over Deps.Renames). It answers three
// states, and every caller says which it has: a repository; nil answered, the issue that
// resolves to none; and nil unanswered, the read having failed, where Dispatch cannot say which
// of the two the issue is. q is the pool or transaction the read is made on.
//
// The two nothings part here, where the read is made and the error is in hand, rather than as
// one nil standing for both that every caller has to guess the meaning of, because the guess has
// one right answer and it is not the same one at each call site: an issue that resolves to no
// repository is a fact about the work, and no pass of such a check stands, while a read of
// Dispatch's own that did not answer is model.ReasonReadFailed, and a database hiccup must not
// make a reviewer refuse an approval.
//
// The canonical name is taken here, before anything asks GitHub, because everything downstream
// takes this answer: the results read's default branch and onBranch, and the record route's
// refusal of a commit the repository does not hold and its read-through of the pull requests
// GitHub names for that commit (results_linked.go's refuseUnknownCommit, results.go's
// readThroughPullRequests).
// An issue recorded under a name that was renamed away would otherwise have the App asked
// about that name, which GitHub answers 404 for, and every result of that issue would read
// unknown. The pairs were read once at boot, so the canonical name costs no read, and a
// repository with no pair is its own canonical name.
//
// Both routes resolve through this one function: two hand-kept copies of this resolve are how
// a refusal and a result's answer come to name different repositories.
//
// It answers a repository whether or not Dispatch can read it — the deployment holding no App
// key, the App having lost the repository — because the results read needs it for an answer
// that asks GitHub nothing: onBranch answers from the branch_commits row Dispatch already
// holds while that row stands, with no call at all (delivery.Branches.CommitOnBranch), and a
// nil here would turn every such answer unknown. branchBases walks the branches for that row
// whenever Dispatch can name one with no read of GitHub, readable just now or not: an
// `after_merge` check whose issue merged asks the merge's own branch first, and every check
// compared with a default branch asks the name this process has already resolved once, which
// delivery keeps for the purpose. A repository no read of this process ever named a default
// branch for has that one name fewer to ask the stored answer about, since a row is keyed by
// a branch: an `after_merge` check whose issue merged asks the branch of the merge there all
// the same, and a check with no branch left to name is the one that compares nothing. The
// record route makes the no-App-key check at its own call site, where every use of the answer
// is a GitHub round trip, and takes the repository alone: a resolve that did not answer stores
// the result there exactly as an issue with no repository does, since neither is GitHub's
// proof that the repository holds no such commit (results_linked.go, refuseUnknownCommit).
func (s *server) checkIssueRepository(ctx context.Context, q refs.Queryer, issueKey string) (*string, bool) {
	repo, err := refs.IssueRepository(ctx, q, issueKey)
	if err != nil {
		slog.Warn("dispatch: resolve the repository of a check's issue", "issue", issueKey, "error", err)
		return nil, false
	}
	return s.renamedRepo(repo), true
}
