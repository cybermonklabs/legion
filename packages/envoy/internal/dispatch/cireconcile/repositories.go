package cireconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// rowState is what a pass learned of one reconciled repository from GitHub.
type rowState struct {
	// etag is the ETag of GET /repos/{row}, which the next pass reads conditionally on.
	etag string
	// fullName and id are what that answer gave: the name GitHub itself spells the repository
	// with, and the id two names of one repository share.
	fullName string
	id       int64
}

// reconciled is one mapped repository as a pass resolved it: the token its reads are made with
// and the scopes they are budgeted under, and the owner, name and id GitHub itself gives it.
// reconcile is false for a repository the pass reads no head of -- one it holds no token for,
// one GitHub did not answer about, one another row naming the same repository was read under --
// which the pass passes over, resolve having said why once.
type reconciled struct {
	owner      string
	repo       string
	id         int64
	token      string
	scope      string
	headScopes []string
	// queryScopes is headScopes with GraphQL's own scope beside them, which is what the limits
	// before a query are read under: a query is budgeted under the App's GraphQL points as well
	// as under the token's own scopes. It is built once with the repository, so the pages of a
	// repository share the one slice however many queries the pass asks.
	queryScopes []string
	reconcile   bool
}

// head is one of this repository's open pull request heads as the pass reads it: the token its
// reads are made with and the scope they are budgeted under, the owner and name GitHub itself
// gives the repository, and the pull request's own number and head commit.
func (repository reconciled) head(number int64, sha string) pullHead {
	return pullHead{
		token:  repository.token,
		scope:  repository.scope,
		owner:  repository.owner,
		repo:   repository.repo,
		number: strconv.FormatInt(number, 10),
		sha:    sha,
	}
}

// resolve reads one mapped row: the installation token it is read with, and the name and id
// GitHub gives it. It answers the stop a rate limit ended the pass at, and
// counts and names at WARN each repository it skips, every one of which is a repository nothing
// within this pass can mend.
//
// read is the repositories this pass has read the heads of already, by the id GitHub gives
// them, which is this pass's own: two rows naming one repository are read once within a pass,
// while the cycle's own sets, which say what may be forgotten when the cycle closes, hold
// everything every pass of the cycle reached.
func (r *Reconciler) resolve(ctx context.Context, row string, c *counts, read map[int64]struct{}) (reconciled, *stop) {
	owner, name, ok := splitRepo(row)
	if !ok {
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository mapping that is not owner/name", "repository", row)
		return reconciled{}, nil
	}
	see(&r.seenRows, row)
	token, err := r.GitHub.RepositoryToken(ctx, owner, name)
	if at := stopAt(err, githubapp.AppScope); at != nil {
		return reconciled{}, at
	}
	if err != nil {
		if r.cutShort(ctx, c) {
			// The pass's own ending killed this read, which says nothing about the repository.
			return reconciled{}, nil
		}
		// The App is not installed on it, or its token is refused the repository: neither
		// mends itself within a pass, so the repository is named once and the pass goes on.
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository it holds no token for", "repository", row, "error", err)
		return reconciled{}, nil
	}
	// Every read made with this token is budgeted under its installation, so that scope is read
	// beside the App's before each of this repository's head reads, and is what a limit GitHub
	// answers one of those reads with is named by.
	scope := r.GitHub.TokenScope(token)
	state, at, err := r.identify(ctx, row, owner, name, token, scope, c)
	if at != nil {
		return reconciled{}, at
	}
	if err != nil {
		if r.cutShort(ctx, c) {
			return reconciled{}, nil
		}
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository GitHub did not answer for", "repository", row, "error", err)
		delete(r.rows, row)
		return reconciled{}, nil
	}
	remember(&r.rows, row, state)
	if _, done := read[state.id]; done {
		// Two rows name one repository -- an old name and the one it was renamed to -- and
		// GitHub answers both with one id, so its heads are read once.
		return reconciled{}, nil
	}
	read[state.id] = struct{}{}
	see(&r.seenRepositories, state.id)
	// Every record is keyed by the name GitHub itself gives the repository, never by the row:
	// repo_projects.repo is stored lower-cased, while the webhook's key comes from GitHub's own
	// casing and cistore.Key does not lower-case, so a pass keyed from the row would write a
	// second record of the same head and settle it on a second subject.
	ownerOf, nameOf, ok := splitRepo(state.fullName)
	if !ok {
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository GitHub named oddly", "repository", row, "full_name", state.fullName)
		return reconciled{}, nil
	}
	headScopes := scopes(scope)
	return reconciled{
		owner:       ownerOf,
		repo:        nameOf,
		id:          state.id,
		token:       token,
		scope:       scope,
		headScopes:  headScopes,
		queryScopes: withGraphQL(headScopes),
		reconcile:   true,
	}, nil
}

// mapped is every repository a pass covers, each named as the repository is called today: every
// repository repo_projects maps to a project, and every repository an issue names as its own.
// A name is the row's or the issue's own `owner/name`, lower-cased with any `.git` stripped as
// both are stored (api's canonicalRepo), or, where a mapping names a rename, what its
// renamed_to names. The two halves are read as one statement, whose `union` answers each
// distinct name once -- a repository a mapping names and an issue names too is one repository
// to read, as are two issues naming one repository -- and the answer is ordered, so two passes
// read GitHub in the same order.
//
// The issues half is there because an issue's own repository is any `owner/name`, one no
// project maps included (api/issue_repo.go, migration 20261112004945_issue_repo_needs_no_mapping):
// a pull request opened for such an issue has a head like any other, whose record the listener
// keeps and whose settlement the Legion daemon waits for, so a repair reading the mappings
// alone would leave exactly those heads short of a check for ever -- never settled, and no WARN
// to say so, since nothing ever looked at that repository.
//
// It is the issue's own repository that is read rather than the one it resolves to
// (refs.IssueRepoResolved), because resolving adds nothing here: an issue naming none resolves
// to the nearest repository above it on `child_of`, which is that ancestor's own and so already
// in this half, or to its project's default mapping, which is a repo_projects row and so
// already in the other. Reading the chain would walk every issue in the database to answer the
// set this `union` answers without a join.
//
// That order is named here as `collate "C"`, which is byte order, on both halves rather than
// left to whatever collation the database holds these names under, because the pass seeks into
// this slice with Go's own comparison of the names (order). Under glibc's en_US, which is what
// a deployment's database usually collates text by, `owner/legionabc` sorts before
// `owner/legion-x` and Go puts `owner/legion-x` first: the seek would then be reading a slice
// ordered by a rule it does not share, and where the cursor's own repository has left the
// order it runs past rows this order put after that repository. Those repositories are read by
// no pass of the cycle, and the pass that reaches the end of the order records that the cycle
// covered them. One rule for both is what the seek rests on, and byte order is the rule Go
// cannot be talked out of.
//
// The canonical name is read rather than the name stored because a pass must never ask GitHub
// about a name a repository was renamed away from: GitHub answers 404 for that name's
// installation, so RepositoryToken fails for it and the repository is skipped with `ci
// reconcile skipped a repository it holds no token for`, a WARN every pass would log for ever
// about a name no one can read by design. The rule is the server's one renaming rule spliced in
// rather than written again here (refs.CanonicalRepoSQL), so this read and every other reader
// of the rule cannot come to different answers. It applies to both halves, since a rename is a
// fact about the repository and not about the row or the issue that names it: an issue recorded
// under a name that was renamed away is a head read under the name the repository has today,
// which is the one answer delivery's attribution holds that issue to as well (attributionHeld).
func (r *Reconciler) mapped(ctx context.Context) ([]string, error) {
	rows, err := r.Pool.Query(ctx, `select `+refs.CanonicalRepoSQL("mapping.repo")+` collate "C" as repository
		from repo_projects mapping
		union
		select `+refs.CanonicalRepoSQL("issue.repo")+` collate "C"
		from issues issue where issue.repo is not null
		order by repository`)
	if err != nil {
		return nil, fmt.Errorf("read the repositories projects map and issues name: %w", err)
	}
	defer rows.Close()
	var repositories []string
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, fmt.Errorf("read a repository to reconcile: %w", err)
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the repositories projects map and issues name: %w", err)
	}
	return repositories, nil
}

// identify resolves one row against GitHub, conditionally: the name GitHub gives the repository
// and its id. A 304 says the answer the last pass kept still stands. scope is what this read is
// budgeted under, which is the token's installation: GitHub records a limit it answers this
// read with under that scope, so the pass names the same one.
func (r *Reconciler) identify(ctx context.Context, row, owner, name, token, scope string, c *counts) (rowState, *stop, error) {
	kept := r.rows[row]
	r.pace(ctx, c)
	c.restRequests++
	answer, err := r.GitHub.RepositoryIdentity(ctx, token, owner, name, kept.etag)
	if at := stopAt(err, scope); at != nil {
		return rowState{}, at, nil
	}
	if err != nil {
		return rowState{}, nil, err
	}
	if answer.NotModified {
		c.restNotModified++
		if kept.id == 0 {
			// Nothing is remembered to stand for the 304, which only a kept ETag can draw, so
			// this cannot happen; reading the row in full next pass is the honest answer to it.
			return rowState{}, nil, errors.New("GitHub answered 304 for a repository this pass remembers nothing of")
		}
		kept.etag = cmp.Or(answer.ETag, kept.etag)
		return kept, nil, nil
	}
	if answer.FullName == "" || answer.ID == 0 {
		return rowState{}, nil, fmt.Errorf("GitHub named the repository %q with id %d", answer.FullName, answer.ID)
	}
	return rowState{etag: answer.ETag, fullName: answer.FullName, id: answer.ID}, nil, nil
}

// openPullRequests is every open pull request of one repository, read page by page and each page
// conditional on the ETag the last pass kept for it. scope is what these reads are budgeted
// under, as identify's is.
func (r *Reconciler) openPullRequests(ctx context.Context, id int64, owner, name, token, scope string, c *counts) ([]githubapp.PullRequestHead, *stop, error) {
	pages, err := readPages(ctx, r, c, &c.listPages, r.pulls[id], func(ctx context.Context, page int, etag string) (conditionalPage[githubapp.PullRequestHead], error) {
		answer, err := r.GitHub.OpenPullRequests(ctx, token, owner, name, page, etag)
		return pullRequestPage(answer), err
	})
	if at := stopAt(err, scope); at != nil {
		return nil, at, nil
	}
	if err != nil {
		// The listing is read again from its first page next pass.
		delete(r.pulls, id)
		return nil, nil, err
	}
	remember(&r.pulls, id, pages)
	var pulls []githubapp.PullRequestHead
	for _, page := range pages {
		pulls = append(pulls, page.items...)
	}
	// The pass reads a repository's heads in pull request number order, whatever order GitHub
	// listed them in: the cursor a pass leaves is a number in this order, and an order that
	// moved between passes would leave the next pass starting somewhere else.
	slices.SortFunc(pulls, func(a, b githubapp.PullRequestHead) int {
		return cmp.Compare(a.Number, b.Number)
	})
	return pulls, nil, nil
}

// splitRepo is the owner and the name of an `owner/name`, and false for anything that is not
// one: no slash, or a side of it empty.
func splitRepo(full string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(full, "/")
	if !found || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
