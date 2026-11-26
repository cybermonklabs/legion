package cireconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
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

// absentApp is GitHub's answer that the App is not installed on a repository, as the Reconciler
// holds it between passes (Reconciler.absent).
type absentApp struct {
	// err is the error that answer was read as, which every pass the answer is held for names
	// a project's default repository with.
	err error
	// until is when a pass asks GitHub about the repository again.
	until time.Time
}

// reconciled is one covered repository as a pass resolved it: the token its reads are made with
// and the scopes they are budgeted under, whether that token holds Commit statuses, and the
// owner, name and id GitHub itself gives it. reconcile is false for a repository the pass reads
// no head of -- one the App is not installed on, one it holds no token for, one GitHub did not
// answer about, one another row naming the same repository was read under -- which the pass
// passes over, resolve having said why once.
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
	// statuses says the token holds Commit statuses at read or write, so the pass reads the
	// commit statuses of this repository's heads: in the query (headsQueryWithStatuses), and over
	// REST for a head the query could not answer whole (readStatuses). Where it is false the pass
	// asks GitHub for no status at all, and reads the repository as it did before it read any.
	statuses  bool
	reconcile bool
}

// head is one of this repository's open pull request heads as the pass reads it: the token its
// reads are made with and the scope they are budgeted under, the owner and name GitHub itself
// gives the repository, the pull request's own number and head commit, and whether its commit
// statuses are read.
func (repository reconciled) head(number int64, sha string) pullHead {
	return pullHead{
		token:    repository.token,
		scope:    repository.scope,
		owner:    repository.owner,
		repo:     repository.repo,
		number:   strconv.FormatInt(number, 10),
		sha:      sha,
		statuses: repository.statuses,
	}
}

// resolve reads one covered name, a project's default repository or an issue's own (covered):
// the installation token it is read with, whether that token holds Commit statuses, and the name
// and id GitHub gives it. It answers the stop a rate limit ended the pass at, and counts and
// names at WARN each repository it skips, every one of which is a repository nothing within this
// pass can mend. A repository the App is not installed on is counted and named by its own rule,
// which byDefault decides (appAbsent). A repository skipped on GitHub's answer -- no token, no
// answer about it, a name GitHub gives that is not owner/name, and a project's default repository
// the App is not installed on -- is a GitHub read the pass gave up, which it notes on its attempt
// (githubreads); a stored name that is not an owner/name GitHub allows (splitRepo) is the
// database's, and asks GitHub nothing, and a repository only an issue names that the App is not
// on is no fault.
//
// read is the repositories this pass has read the heads of already, by the id GitHub gives
// them, which is this pass's own: two rows naming one repository are read once within a pass,
// while the cycle's own sets, which say what may be forgotten when the cycle closes, hold
// everything every pass of the cycle reached.
func (r *Reconciler) resolve(ctx context.Context, row string, byDefault bool, c *counts, read map[int64]struct{}) (reconciled, *stop) {
	owner, name, ok := splitRepo(row)
	if !ok {
		c.defaultsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository whose name is not an owner/name GitHub allows", "repository", row)
		return reconciled{}, nil
	}
	see(&r.seenRows, row)
	if held, absent := r.absent[row]; absent && r.clock().Now().Before(held.until) {
		// GitHub said the App is not installed here less than AbsentAppRecheck ago, so this
		// pass asks it nothing about the repository.
		r.appAbsent(ctx, row, byDefault, held.err, false, c)
		return reconciled{}, nil
	}
	// Whether the pass reads this repository's commit statuses is decided by the permissions of
	// the token it reads with, which GitHub's own answer to the token's mint names, rather than by
	// the installation's: a token is kept for about an hour, so after a person accepts Commit
	// statuses the installation says the App holds it for up to an hour before the token in hand
	// does, and a status read with that token would be refused for every head at every pass
	// (githubapp.Client.RepositoryTokenWithStatuses). A grant taken away while a token lives is
	// still refused, and that costs a head its statuses alone (refusedPulls, readStatuses).
	token, statuses, err := r.GitHub.RepositoryTokenWithStatuses(ctx, owner, name)
	if at := r.stopAt(err, githubapp.AppScope); at != nil {
		return reconciled{}, at
	}
	if err != nil {
		if r.cutShort(ctx, c) {
			// The pass's own ending killed this read, which says nothing about the repository.
			return reconciled{}, nil
		}
		if errors.Is(err, githubapp.ErrNoInstallation) {
			// githubapp keeps only an installation it found, so this answer is held here:
			// without it every pass would ask GitHub again, with the App's own JWT, about a
			// repository whose answer changes only when a person installs the App.
			remember(&r.absent, row, absentApp{err: err, until: r.clock().Now().Add(AbsentAppRecheck)})
			r.appAbsent(ctx, row, byDefault, err, true, c)
			return reconciled{}, nil
		}
		// Its token could not be minted, or GitHub did not answer about its installation:
		// neither mends itself within a pass, so the repository is named once and the pass
		// goes on. Neither is held either, since neither is GitHub saying the App is not
		// installed: the next pass asks again
		// (TestALookupGitHubFailedToAnswerIsAskedAgainAtTheNextPass).
		c.defaultsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository it holds no token for", "repository", row, "error", err)
		githubreads.NoteFailure(ctx, err)
		return reconciled{}, nil
	}
	// The App is installed on it, so an answer an earlier pass held to the contrary is over.
	delete(r.absent, row)
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
		c.defaultsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository GitHub did not answer for", "repository", row, "error", err)
		githubreads.NoteFailure(ctx, err)
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
	// a project's default repository and an issue's own are stored lower-cased, while the
	// webhook's key comes from GitHub's own casing and cistore.Key does not lower-case, so a pass
	// keyed from the row would write a second record of the same head and settle it on a second
	// subject.
	ownerOf, nameOf, ok := splitRepo(state.fullName)
	if !ok {
		c.defaultsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository GitHub named oddly", "repository", row, "full_name", state.fullName)
		githubreads.NoteFailure(ctx, fmt.Errorf("ci reconcile skipped %s: GitHub named it %q, which is not owner/name", row, state.fullName))
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
		statuses:    statuses,
		reconcile:   true,
	}, nil
}

// appAbsent counts and names one repository the App is not installed on. err is GitHub's answer
// as the pass read it, and asked says this pass read it from GitHub rather than from the answer
// an earlier pass held.
//
// The two kinds of covered repository are told apart here because the same answer means two
// things. A project's default repository is a person's statement that the project's work is
// done there, so Dispatch reads it, and the App missing from it is a fault a person mends by
// installing the App or changing the default: it is skipped at WARN and counted in
// defaults_skipped on every pass the fault stands for, which is what that count and that WARN
// mean to the person reading them. A repository only an issue names is any `owner/name` a person
// or an agent wrote, and one the App is not installed on is expected and refuses that issue no
// work: it is counted apart (repositories_without_app), and named at INFO only by the pass that
// asked GitHub, which is once every AbsentAppRecheck and not once a pass.
//
// That keeps the App missing from such a repository out of defaults_skipped, but it does not
// make every defaults_skipped above zero a project default to mend: every other skip of resolve
// counts there whoever names the repository. One such case stands at every pass. An issue-only
// repository whose Dispatch App installation is suspended answers the installation lookup 200
// and the token mint 403, which is no ErrNoInstallation, so resolve skips it at WARN as a
// repository it holds no token for and counts it in defaults_skipped at every pass, exactly as
// it does a project's default repository the App is missing from.
//
// The App missing from a project's default repository is also a GitHub read the pass gave up, so
// it is noted on the pass's attempt (githubreads) on every pass the fault stands for, whether
// this pass asked GitHub or read the answer an earlier pass held, so the attempt does not read
// succeeded for the passes that only held it. The App missing from a repository only an issue
// names is no fault, and is not noted.
func (r *Reconciler) appAbsent(ctx context.Context, row string, byDefault bool, err error, asked bool, c *counts) {
	if byDefault {
		c.defaultsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository it holds no token for", "repository", row, "error", err)
		githubreads.NoteFailure(ctx, err)
		return
	}
	c.withoutApp++
	if asked {
		r.Logger.Info("ci reconcile passed over a repository the App is not installed on", "repository", row)
	}
}

// covered is every repository a pass covers, each named as the repository is called today: every
// project's default repository, and every repository an issue names as its own. That is every
// repository an issue resolves to: an issue resolves to its own repository, to an ancestor's
// own, or to its project's default (refs.IssueRepoResolved). A name is the project's or the
// issue's own `owner/name`, lower-cased with any `.git` stripped as both are stored
// (refs.ExternalRepo), or, where a rename pair's old name is that name, the pair's new name
// (Reconciler.Renames). The two halves are read as one statement, which answers each distinct
// name once -- a repository a project's default and an issue both name is one repository to
// read, as are two issues or two projects naming one repository -- and the answer is ordered,
// so two passes read GitHub in the same order.
//
// defaults is the names among them that a project's default repository names, whether or not
// an issue names them too. It is what tells a repository the App is missing from by a person's
// fault from one an issue happens to name (appAbsent), and it is read in the same statement as
// the names so the two cannot disagree about one pass's repositories.
//
// The issues half is there because an issue's own repository is any `owner/name`, one that no
// project's default names included (api/issue_repo.go, migration
// 20261112004945_issue_repo_needs_no_mapping): a pull request opened for such an issue has a
// head like any other, whose record the listener keeps and whose settlement the Legion daemon
// waits for, so a repair reading the project defaults alone would leave exactly those heads short
// of a check for ever -- never settled, and no WARN to say so, since nothing ever looked at that
// repository.
//
// The issues half reads every issue, closed ones included, and that is deliberate. An issue
// naming no repository resolves to the nearest repository above it on `child_of` in its own
// project, whatever the status of the issue that names it (refs.IssueRepoTerms), so an open
// issue under a closed one works in the closed one's repository: a half bounded to open issues
// would leave that issue's heads unread. Holding it to the issues whose chain is still open
// would walk every issue in the database, and what a status filter would save is the cost of a
// repository no open issue names, which is one conditional read and one query a pass, or, where
// the App is not installed on it, one read every AbsentAppRecheck (resolve).
//
// It is the issue's own repository that is read rather than the one it resolves to
// (refs.IssueRepoResolved), because resolving adds nothing here: an issue naming none resolves
// to that nearest repository, which is its ancestor's own and so already in this half, or to
// its project's default repository, which is already in the other half. Reading the chain would
// walk every issue in the database to answer the set this statement answers without a join.
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
// cannot be talked out of. The halves carry it rather than the statement around them, so the
// column the two make has one collation whatever the two tables' columns are collated by, and
// the grouping and the ordering around them take that one.
//
// The canonical name is read rather than the name stored because a pass must never ask GitHub
// about a name a repository was renamed away from: GitHub answers 404 for that name's
// installation, so RepositoryTokenWithStatuses fails for it and the repository is skipped with
// `ci reconcile skipped a repository it holds no token for`, a WARN every pass would log for ever
// about a name no one can read by design. The boot's pass writes the new name into both columns
// (store.RewriteRenamedRepositories), so this read changes a name only where a failed pass left
// the old one. The rule is the server's one renaming rule spliced in rather than written again
// here (refs.CanonicalRepoSQL over Reconciler.Renames, the pairs the boot read), so this read
// and every other reader of the rule cannot come to different answers. It applies to both
// halves, since a rename is a fact about the repository and not about the project or the issue
// that names it: an issue recorded under a name that was renamed away is a head read under the
// name the repository has today, which is the one answer delivery's attribution holds that
// issue to as well (attributionHeld).
func (r *Reconciler) covered(ctx context.Context) (repositories []string, defaults map[string]struct{}, err error) {
	rows, err := r.Pool.Query(ctx, `select named.repository, bool_or(named.by_default)
		from (
			select `+refs.CanonicalRepoSQL(r.Renames, "project.default_repo")+` collate "C" as repository, true as by_default
			from projects project where project.default_repo is not null
			union all
			select `+refs.CanonicalRepoSQL(r.Renames, "issue.repo")+` collate "C", false
			from issues issue where issue.repo is not null
		) named
		group by named.repository
		order by named.repository`)
	if err != nil {
		return nil, nil, fmt.Errorf("read the repositories projects default to and issues name: %w", err)
	}
	defer rows.Close()
	defaults = map[string]struct{}{}
	for rows.Next() {
		var repository string
		var byDefault bool
		if err := rows.Scan(&repository, &byDefault); err != nil {
			return nil, nil, fmt.Errorf("read a repository to reconcile: %w", err)
		}
		repositories = append(repositories, repository)
		if byDefault {
			defaults[repository] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the repositories projects default to and issues name: %w", err)
	}
	return repositories, defaults, nil
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
	if at := r.stopAt(err, scope); at != nil {
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
	if at := r.stopAt(err, scope); at != nil {
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
// one GitHub allows (refs.SplitGitHubRepo, the one rule every place that builds a GitHub path
// reads): no slash, a side of it empty, a character GitHub's names do not take, a name past 100
// characters, or a name of `.` or `..` before or after a trailing `.git` is trimmed. A project's
// default repository, an issue's own repository or a rename pair's new name stored before the
// routes held it to that rule, or by direct SQL, is such a name, and `..` in a path would ask
// GitHub about the one above the repository's, so the pass skips it, naming it at WARN, before it
// asks GitHub anything.
func splitRepo(full string) (owner, name string, ok bool) {
	owner, name, err := refs.SplitGitHubRepo(full)
	return owner, name, err == nil
}
