package codesurvival

// github.go holds the run's reads of GitHub, through Dispatch's GitHub App (R2): a repository's
// default branch and its tip, the first-parent chain of that branch, the merged pull requests of
// the chain with each one's issue as Dispatch's attribution gives it, and each commit's patch in
// the replay's form. Each read counts what it cost, and every answer's rate-limit headers are read
// into one budget the run consults (R12). Nothing here stores anything: what the reads answer is
// held in memory by the run, and a patch is released once the replay applied it (R3).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// readLimit bounds every answer a read here takes: a patch through either diff route, a page of a
// commit's files[], and a page of the commits and closed pull requests lists. 32 MiB is R2's
// bound for one patch. A list page shares it because a page of 100 pull requests carries each body
// whole, and one body of cybermonklabs/legion runs past 150 KB (#4592 of
// testdata/github/pulls-page.txt), so the client's own 1 MiB would refuse a page of such bodies;
// one bound is one thing to reason about.
const readLimit = 32 << 20

// perPage is the page size each list read asks GitHub for, its own maximum.
const perPage = 100

// readAttempts is how many times one request is sent: once, and four more times after a failure
// that may pass (transient), each after retryWait.
const readAttempts = 5

// retryWait is the wait before the try after attempt: 1, 2, 4 and 8 s, as
// scripts-feasibility/fetch.py waited after a 5xx.
func retryWait(attempt int) time.Duration { return time.Second << (attempt - 1) }

// chainMargin is how far before its oldest day a chain read reaches. A commit can be dated before
// its parent: on cybermonklabs/legion 67 of 4,411 first-parent commits since 2026-07-01 are dated
// later than their child, by 20.9 hours at most, and the walk stops at the first commit dated
// before its since, so three days keep such a commit near the oldest day on the chain.
const chainMargin = 3 * 24 * time.Hour

// ChainSince is the since of a chain read whose oldest day is day, the oldest day holding a
// fingerprint or the start day: that day's UTC midnight less chainMargin.
func ChainSince(day time.Time) time.Time { return survival.DayOf(day).Add(-chainMargin) }

// Route is a route of GitHub's API the reads send requests to. A patch names the route it was
// read through (PatchRead.Route): RouteDiff, RouteCompare or RouteFiles.
type Route string

// The routes the reads count their requests by (Counts).
const (
	// RouteRepository is the repository's default branch and its branch's tip (Open).
	RouteRepository Route = "repository"
	// RouteChain is a page of the commits list (Chain).
	RouteChain Route = "chain"
	// RoutePulls is a page of the closed pull requests list (PullRequests).
	RoutePulls Route = "pulls"
	// RouteAttribution is a page of a pull request's commit messages that the attribution read
	// (Changes, delivery.Attributor.IssueOf).
	RouteAttribution Route = "attribution"
	// RouteDiff is a commit's patch through the commit route, RouteCompare through the compare
	// route from its first parent, and RouteFiles a page of the commit's files[] read in place of
	// a patch GitHub refused (Patch).
	RouteDiff    Route = "diff"
	RouteCompare Route = "compare"
	RouteFiles   Route = "files"
)

// Counts is what the reads have cost so far: the requests sent to each route and the bytes GitHub
// answered them with. Each try of a request asked again counts; a request a recorded rate limit
// held, which was never sent, does not, and neither do the App's own calls for its installation
// and its token, which GitHub budgets as the App's and not the installation's.
type Counts struct {
	Repository  int
	Chain       int
	Pulls       int
	Attribution int
	Diff        int
	Compare     int
	Files       int
	Bytes       int64
}

// Requests is every request counted, on every route.
func (c Counts) Requests() int {
	return c.Repository + c.Chain + c.Pulls + c.Attribution + c.Diff + c.Compare + c.Files
}

// add counts n requests to route.
func (c *Counts) add(route Route, n int) {
	switch route {
	case RouteRepository:
		c.Repository += n
	case RouteChain:
		c.Chain += n
	case RoutePulls:
		c.Pulls += n
	case RouteAttribution:
		c.Attribution += n
	case RouteDiff:
		c.Diff += n
	case RouteCompare:
		c.Compare += n
	case RouteFiles:
		c.Files += n
	}
}

// Budget is GitHub's rate limit as the answers read so far name it (X-RateLimit-Limit,
// X-RateLimit-Remaining and X-RateLimit-Reset): the requests the installation may send in a
// window, how many of them are left, and when the window starts again. The zero Budget is one no
// answer has named.
//
// Answers arrive from several workers at once, so the budget does not simply take the last one
// read: an answer of a later window than the budget's replaces it, and one of the same window
// lowers Remaining, never raises it, since a lower count is the later answer whatever order the
// two arrived in. An answer of an earlier window is a stale one and changes nothing.
type Budget struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// read takes what header names of the budget, as the comment on Budget says.
func (b *Budget) read(header http.Header) {
	limit, limitErr := strconv.Atoi(header.Get("X-RateLimit-Limit"))
	remaining, remainingErr := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
	reset, resetErr := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if limitErr != nil || remainingErr != nil || resetErr != nil {
		return
	}
	at := time.Unix(reset, 0).UTC()
	switch {
	case at.After(b.Reset):
		*b = Budget{Limit: limit, Remaining: remaining, Reset: at}
	case at.Equal(b.Reset):
		b.Limit, b.Remaining = limit, min(b.Remaining, remaining)
	}
}

// GitHub reads repositories' histories for the run through Dispatch's GitHub App. It counts what
// every read cost and reads GitHub's budget from every answer (Counts, Budget), and it is safe for
// concurrent use, since the run reads patches at eight workers.
type GitHub struct {
	client     *githubapp.Client
	pool       *store.Pool
	attributor *delivery.Attributor
	// sleep waits d before a request is asked again, or answers ctx's error when ctx ends first.
	// A test makes it record the wait and return at once.
	sleep func(ctx context.Context, d time.Duration) error

	mu     sync.Mutex
	budget Budget
	counts Counts
}

// NewGitHub is a reader of GitHub through client, whose attribution of a change's pull request
// reads pool through attributor.
func NewGitHub(client *githubapp.Client, pool *store.Pool, attributor *delivery.Attributor) *GitHub {
	return &GitHub{client: client, pool: pool, attributor: attributor, sleep: sleep}
}

// sleep waits d, or answers ctx's error when ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Counts is what the reads have cost so far.
func (g *GitHub) Counts() Counts {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counts
}

// Budget is GitHub's rate limit as the answers read so far name it.
func (g *GitHub) Budget() Budget {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.budget
}

// answered counts one request to route that GitHub answered with header and size bytes of body,
// and reads the budget from header.
func (g *GitHub) answered(route Route, header http.Header, size int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counts.add(route, 1)
	g.counts.Bytes += size
	if header != nil {
		g.budget.read(header)
	}
}

// counted counts n requests to route whose answers the reads here never saw.
func (g *GitHub) counted(route Route, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counts.add(route, n)
}

// Repository is a repository the run reads, as Open answered it: its name ("owner/name", as
// projects.default_repo holds it) and its two parts, its default branch, and that branch's tip.
type Repository struct {
	Repo   string
	Owner  string
	Name   string
	Branch string
	Tip    string
}

// repoPath is the API path of the repository, each part escaped.
func (r Repository) repoPath() string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// UnreadableError is a repository the App cannot read: not installed on it, installed without
// Contents read, refused or not found by GitHub (a 403 or a 404 that is no rate limit, or a
// default branch GitHub has no ref of), or a name GitHub does not allow. Err says which, and its
// text is the reason code_repos keeps beside the state (Repo.Reason). No retry within a pass
// changes it, so the run records the repository unreadable and tries again at its next pass.
type UnreadableError struct {
	Repo string
	Err  error
}

func (e *UnreadableError) Error() string {
	return "the GitHub App cannot read " + e.Repo + ": " + e.Err.Error()
}

func (e *UnreadableError) Unwrap() error { return e.Err }

// Unreadable reports whether err says a repository is unreadable (an *UnreadableError), and its
// reason.
func Unreadable(err error) (reason string, ok bool) {
	var unreadable *UnreadableError
	if !errors.As(err, &unreadable) {
		return "", false
	}
	return unreadable.Err.Error(), true
}

// unreadable is err as an *UnreadableError of repo where it says the App cannot read repo, and err
// as it is otherwise, an error that already holds one included. A repository the App is not
// installed on or is refused is the delivery intake's rule (delivery.RepositoryUnreadable); an
// installation without Contents read, which every read here needs, and a default branch GitHub
// has no ref of are the architecture importer's (architecture.IsAccessFailure).
func unreadable(repo string, err error) error {
	if _, already := Unreadable(err); already {
		return err
	}
	if delivery.RepositoryUnreadable(err) || errors.Is(err, githubapp.ErrNoContentsRead) ||
		errors.Is(err, githubapp.ErrNoBranch) || errors.Is(err, refs.ErrNotGitHubRepo) {
		return &UnreadableError{Repo: repo, Err: err}
	}
	return err
}

// sent reports whether a call that answered err sent its request: every call but one a recorded
// rate limit held.
func sent(err error) bool {
	limited, ok := githubapp.AsRateLimit(err)
	return !ok || !limited.Held
}

// Open reads what the run needs of repo before it reads its history: that the App is installed on
// it with Contents read, its default branch (githubapp.Client.DefaultBranch) and that branch's tip
// (githubapp.Client.Branch). A repository the App cannot read is an *UnreadableError with the
// reason (Unreadable); a rate limit is the *githubapp.RateLimitError, and any other failure is
// returned as it is.
func (g *GitHub) Open(ctx context.Context, repo string) (Repository, error) {
	owner, name, err := refs.SplitGitHubRepo(repo)
	if err != nil {
		return Repository{}, unreadable(repo, err)
	}
	if _, err := g.client.Installation(ctx, owner, name); err != nil {
		return Repository{}, unreadable(repo, err)
	}
	token, err := g.client.RepositoryToken(ctx, owner, name)
	if err != nil {
		return Repository{}, unreadable(repo, err)
	}
	branch, err := g.client.DefaultBranch(ctx, token, owner, name)
	if sent(err) {
		g.counted(RouteRepository, 1)
	}
	if err != nil {
		return Repository{}, unreadable(repo, err)
	}
	tip, err := g.client.Branch(ctx, token, owner, name, branch)
	if sent(err) {
		g.counted(RouteRepository, 1)
	}
	if err != nil {
		return Repository{}, unreadable(repo, err)
	}
	return Repository{Repo: repo, Owner: owner, Name: name, Branch: branch, Tip: tip}, nil
}

// answer is one answer GitHub gave a read: its body, status and headers.
type answer struct {
	body   []byte
	status int
	header http.Header
}

// get sends GET path to route under r's installation token and answers GitHub's last answer. A
// read that failed in a way that may pass (transient) is sent again, up to readAttempts tries in
// all with retryWait between them, and the last try's answer is the one returned. A diff route's
// refusal (refusal) is returned at once, for the caller to read the files[] in its place.
//
// Each try runs in a held scope of the pass's attempt (githubreads.Hold), as delivery's
// readGitHubPage runs one: a try asked again leaves no failure on the pass, and neither does a
// refusal, which is GitHub's answer about one patch and no failure of the read; the try returned
// otherwise keeps what it noted.
func (g *GitHub) get(ctx context.Context, r Repository, route Route, path string) (answer, error) {
	diff := route == RouteDiff || route == RouteCompare
	for attempt := 1; ; attempt++ {
		asked, held := githubreads.Hold(ctx)
		got, err := g.send(asked, r, route, path, diff)
		switch {
		case diff && refusal(got, err):
			held.Drop()
			return got, err
		case attempt < readAttempts && ctx.Err() == nil && transient(got, err):
			if g.sleep(ctx, retryWait(attempt)) == nil {
				held.Drop()
				continue
			}
		}
		held.Keep()
		return got, err
	}
}

// send makes one try of get: the installation token of r, which the client's cache answers with
// no request but the first, and one read of path, asking for the diff media type where diff.
func (g *GitHub) send(ctx context.Context, r Repository, route Route, path string, diff bool) (answer, error) {
	token, err := g.client.RepositoryToken(ctx, r.Owner, r.Name)
	if err != nil {
		return answer{}, fmt.Errorf("mint the installation token of %s: %w", r.Repo, unreadable(r.Repo, err))
	}
	read := g.client.ReadWithin
	if diff {
		read = g.client.ReadDiff
	}
	body, status, header, err := read(ctx, token, path, readLimit)
	if sent(err) {
		size := int64(len(body))
		var tooLarge *githubapp.ResponseTooLargeError
		if errors.As(err, &tooLarge) {
			// The client read one byte past the limit before it refused the rest.
			size = tooLarge.Limit + 1
		}
		g.answered(route, header, size)
	}
	return answer{body: body, status: status, header: header}, err
}

// transient reports whether a try's outcome may pass if it is asked again: a 5xx, and any error
// but a rate limit, an answer past the limit and a repository the App cannot read. Those errors
// are a timeout (the client's own RequestTimeout or DiffTimeout), a connection that failed or was
// cut, and a body that could not be read. get reads a diff route's refusal before it asks this,
// so a 5xx naming the diff never reaches it.
func transient(got answer, err error) bool {
	if err != nil {
		var tooLarge *githubapp.ResponseTooLargeError
		_, limited := githubapp.AsRateLimit(err)
		_, refused := Unreadable(err)
		return !limited && !refused && !errors.As(err, &tooLarge) && !errors.Is(err, githubapp.ErrNoAppKey)
	}
	return got.status/100 == 5
}

// refusal reports whether a diff route's outcome is GitHub refusing the patch, which the files[]
// of the commit are read in place of: a 406 or a 422, a 5xx whose body names the diff
// (namesTheDiff), or an answer past readLimit.
func refusal(got answer, err error) bool {
	if err != nil {
		var tooLarge *githubapp.ResponseTooLargeError
		return errors.As(err, &tooLarge)
	}
	switch {
	case got.status == http.StatusNotAcceptable, got.status == http.StatusUnprocessableEntity:
		return true
	case got.status/100 == 5:
		return namesTheDiff(got.body)
	}
	return false
}

// namesTheDiff reports whether a 5xx answer's body says that the diff is what GitHub could not
// give, in GitHub's words for it: "Sorry, this diff is taking too long to generate", or a diff or
// a patch "too large". A 5xx that names neither is GitHub failing, and is asked again.
func namesTheDiff(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("diff")) || bytes.Contains(lower, []byte("too large"))
}

// readJSON is get of a JSON answer that must be a 200: a rate limit is the *githubapp.RateLimitError,
// and any other status the error check makes of it.
func (g *GitHub) readJSON(ctx context.Context, r Repository, route Route, path string, check func(status int, header http.Header, body []byte) error) (answer, error) {
	got, err := g.get(ctx, r, route, path)
	if err != nil {
		return answer{}, err
	}
	if err := check(got.status, got.header, got.body); err != nil {
		return answer{}, err
	}
	return got, nil
}

// repositoryCheck is the check of a read of the repository's own lists: a 403 or 404 that is no
// rate limit is githubapp.ErrRepositoryUnreadable (githubapp.CheckRepositoryResponse).
func repositoryCheck(r Repository, path string) func(int, http.Header, []byte) error {
	return func(status int, header http.Header, body []byte) error {
		return githubapp.CheckRepositoryResponse(status, header, body, r.Repo, path)
	}
}

// listedCommit is one entry of the commits list, the fields Chain reads.
type listedCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
}

// Chain is the first-parent chain of r's branch from its tip back to since, oldest first, each
// commit with its SHA, its parents (the first first) and its committer time, and no change facts
// (Changes). It reads every page of GitHub's commits list from the tip Open read
// (`GET /repos/{owner}/{repo}/commits?sha={tip}&since={since}&per_page=100`), following each
// page's Link header, and walks it by parents[0] from the tip: a commit GitHub lists that the walk
// does not reach, a side branch's or one no first parent leads to, is no part of the chain. The
// walk stops at the first commit dated before since, and at a parent GitHub did not list. since is
// ChainSince of the oldest day the run holds a fingerprint of, or of the start day.
//
// The list is read from the tip rather than the branch's name, so the chain is the history of the
// one tip the run read, whatever the branch does while the pages are read.
func (g *GitHub) Chain(ctx context.Context, r Repository, since time.Time) ([]survival.Commit, error) {
	query := url.Values{"sha": {r.Tip}, "since": {since.UTC().Format(time.RFC3339)}, "per_page": {strconv.Itoa(perPage)}}
	listed := map[string]survival.Commit{}
	for path := r.repoPath() + "/commits?" + query.Encode(); path != ""; {
		got, err := g.readJSON(ctx, r, RouteChain, path, repositoryCheck(r, path))
		if err != nil {
			return nil, fmt.Errorf("read the commits of %s since %s: %w", r.Repo, since.UTC().Format(time.RFC3339), unreadable(r.Repo, err))
		}
		var page []listedCommit
		if err := json.Unmarshal(got.body, &page); err != nil {
			return nil, fmt.Errorf("decode the commits of %s: %w", r.Repo, githubapp.ShownErr(err))
		}
		for _, entry := range page {
			parents := make([]string, len(entry.Parents))
			for i, parent := range entry.Parents {
				parents[i] = parent.SHA
			}
			listed[entry.SHA] = survival.Commit{SHA: entry.SHA, Parents: parents, CommittedAt: entry.Commit.Committer.Date.UTC()}
		}
		if path, err = g.client.NextPage(got.header); err != nil {
			return nil, fmt.Errorf("read the commits of %s: %w", r.Repo, err)
		}
	}
	var chain []survival.Commit
	for sha := r.Tip; ; {
		commit, ok := listed[sha]
		if !ok || commit.CommittedAt.Before(since) {
			break
		}
		// A commit is taken once, so a list whose parents loop ends the walk.
		delete(listed, sha)
		chain = append(chain, commit)
		if len(commit.Parents) == 0 {
			break
		}
		sha = commit.Parents[0]
	}
	slices.Reverse(chain)
	return chain, nil
}

// PullRequest is one merged pull request of the closed pull requests list, as Changes reads it:
// its number and URL, its title and body, its head branch and the repository that branch lives in
// ("" where GitHub names none), the login that opened it (GitHub's REST user.login, so an App reads
// `<name>[bot]`), and when it merged.
type PullRequest struct {
	Number   int
	URL      string
	Title    string
	Body     string
	HeadRef  string
	HeadRepo string
	Author   string
	MergedAt time.Time
}

// listedPull is one entry of the closed pull requests list, the fields PullRequests reads.
type listedPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	MergedAt       *time.Time `json:"merged_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// PullRequests is the merged pull requests of r's repository that landed as a commit of chain, by
// that commit's SHA (merge_commit_sha). It reads GitHub's closed pull requests of the repository,
// whatever their base, newest update first (`GET /repos/{owner}/{repo}/pulls?state=closed&sort=updated&direction=desc&per_page=100`),
// page by page as each page's Link header names the next, and stops after the first page whose
// last pull request was updated before d0, the oldest selected day: a pull request merged on or
// after d0 was updated then too. The list names no base: a pull request merged into another
// branch, one of a stack, lands on the branch's chain when that branch does, as a stacked pull
// request of cybermonklabs/legion (#1284) landed on main as fb96b8da with a base of its own.
// A closed pull request that never merged is no change, whatever commit GitHub names for it,
// neither is one that merged as no commit of chain, and a commit no pull request landed as -- a
// push, or the earlier commits of a rebase merge, whose last commit alone is its
// merge_commit_sha -- has no entry.
func (g *GitHub) PullRequests(ctx context.Context, r Repository, d0 time.Time, chain []survival.Commit) (map[string]PullRequest, error) {
	onChain := make(map[string]bool, len(chain))
	for _, commit := range chain {
		onChain[commit.SHA] = true
	}
	query := url.Values{
		"state": {"closed"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {strconv.Itoa(perPage)},
	}
	pulls := map[string]PullRequest{}
	for path := r.repoPath() + "/pulls?" + query.Encode(); path != ""; {
		got, err := g.readJSON(ctx, r, RoutePulls, path, repositoryCheck(r, path))
		if err != nil {
			return nil, fmt.Errorf("read the closed pull requests of %s: %w", r.Repo, unreadable(r.Repo, err))
		}
		var page []listedPull
		if err := json.Unmarshal(got.body, &page); err != nil {
			return nil, fmt.Errorf("decode the closed pull requests of %s: %w", r.Repo, githubapp.ShownErr(err))
		}
		for _, entry := range page {
			if entry.MergedAt == nil || !onChain[entry.MergeCommitSHA] {
				continue
			}
			pull := PullRequest{
				Number: entry.Number, URL: entry.HTMLURL, Title: entry.Title, Body: entry.Body,
				HeadRef: entry.Head.Ref, Author: entry.User.Login, MergedAt: entry.MergedAt.UTC(),
			}
			if entry.Head.Repo != nil {
				pull.HeadRepo = entry.Head.Repo.FullName
			}
			pulls[entry.MergeCommitSHA] = pull
		}
		if len(page) == 0 || page[len(page)-1].UpdatedAt.Before(d0) {
			break
		}
		if path, err = g.client.NextPage(got.header); err != nil {
			return nil, fmt.Errorf("read the closed pull requests of %s: %w", r.Repo, err)
		}
	}
	return pulls, nil
}

// Changes gives each commit of chain that landed one of pulls (PullRequests) its change facts: the
// pull request's number, head branch and author, and its issue as Dispatch's attribution gives it
// (R5). A commit that landed none gets no change. The issue is the one pull_request_heads holds
// where the rule this build judges by judged the pull request (delivery.Attributor.JudgedIssues),
// read in one statement with no request; every other pull request runs attribution's own path
// (delivery.Attributor.IssueOf), whose commit-message pages are counted on RouteAttribution. A
// failure of that path fails the call: a change's issue is stored with it for good.
func (g *GitHub) Changes(ctx context.Context, r Repository, chain []survival.Commit, pulls map[string]PullRequest) error {
	var numbers []int
	for _, commit := range chain {
		if pull, ok := pulls[commit.SHA]; ok {
			numbers = append(numbers, pull.Number)
		}
	}
	judged, err := g.attributor.JudgedIssues(ctx, g.pool, r.Repo, numbers)
	if err != nil {
		return err
	}
	token := func() (string, error) { return g.client.RepositoryToken(ctx, r.Owner, r.Name) }
	for i := range chain {
		pull, ok := pulls[chain[i].SHA]
		if !ok {
			chain[i].Change = survival.ChangeFacts{}
			continue
		}
		issue, stored := judged[pull.Number]
		if !stored {
			key, pages, err := g.attributor.IssueOf(ctx, g.pool, g.client, token, delivery.PullRequestFacts{
				Repo: r.Repo, Number: pull.Number, URL: pull.URL, Title: pull.Title, Body: pull.Body,
				HeadRef: pull.HeadRef, HeadRepo: pull.HeadRepo,
			})
			g.counted(RouteAttribution, pages)
			if err != nil {
				return fmt.Errorf("the issue of %s#%d: %w", r.Repo, pull.Number, err)
			}
			issue = key
		}
		chain[i].Change = survival.ChangeFacts{PR: pull.Number, HeadRef: pull.HeadRef, Author: pull.Author, IssueKey: issue}
	}
	return nil
}

// PatchRead is one commit's patch as Patch read it: the replay's form of it, the route it came
// through, and the bytes GitHub answered for it, which the run's stream holds until the replay
// applied it. Reason says, for a patch read through files[] in place of a diff GitHub refused, how
// the diff route refused it and how many of the commit's files files[] withheld, as
// code_days.blocked_reason keeps it for a commit that blocks a day: "diff refused (406), files[]
// withheld 3 of 12 files", say.
type PatchRead struct {
	Patch  survival.Patch
	Route  Route
	Bytes  int
	Reason string
}

// Patch reads the patch of commit, a commit of r's first-parent chain, within readLimit and
// githubapp.DiffTimeout. A commit of one parent, or none, is read through the commit route
// (`GET /repos/{owner}/{repo}/commits/{sha}`) and one of two or more through the compare route from
// its first parent (`GET /repos/{owner}/{repo}/compare/{parents[0]}...{sha}`), each asking for the
// diff (githubapp.Client.ReadDiff): what the commit brought onto the branch.
//
// A patch GitHub refuses (refusal) is read once through the commit's files[] in its place, every
// page of it, in the replay's form, with the files GitHub withheld counted (survival.FilesPatch)
// and the refusal and the count named in the read's Reason.
// A failure that may pass is asked again (get), and one that still fails after the last try, or a
// failure that is no refusal, fails the read: the run stores nothing and tries again at its next
// pass. A rate limit is the budget's, returned at once as the *githubapp.RateLimitError.
func (g *GitHub) Patch(ctx context.Context, r Repository, commit survival.Commit) (PatchRead, error) {
	route, path := RouteDiff, r.repoPath()+"/commits/"+url.PathEscape(commit.SHA)
	if len(commit.Parents) > 1 {
		route, path = RouteCompare, r.repoPath()+"/compare/"+url.PathEscape(commit.Parents[0])+"..."+url.PathEscape(commit.SHA)
	}
	got, err := g.get(ctx, r, route, path)
	if refusal(got, err) {
		read, files, filesErr := g.filesPatch(ctx, r, commit.SHA)
		if filesErr != nil {
			return PatchRead{}, filesErr
		}
		read.Reason = fmt.Sprintf("%s, files[] withheld %d of %d files", refused(route, got, err), read.Patch.Withheld, files)
		return read, nil
	}
	if err != nil {
		return PatchRead{}, fmt.Errorf("read the patch of %s %s: %w", r.Repo, commit.SHA, err)
	}
	if err := githubapp.CheckResponse(got.status, got.header, got.body); err != nil {
		return PatchRead{}, fmt.Errorf("read the patch of %s %s: %w", r.Repo, commit.SHA, err)
	}
	files, err := survival.ParseDiff(got.body)
	if err != nil {
		return PatchRead{}, fmt.Errorf("parse the patch of %s %s: %w", r.Repo, commit.SHA, err)
	}
	return PatchRead{Patch: survival.Patch{Files: files}, Route: route, Bytes: len(got.body)}, nil
}

// refused is how route refused a patch, as a reason names it: "diff refused (406)" or "compare
// refused (502)" for a status, and "diff answer past 32 MiB" for an answer past readLimit.
func refused(route Route, got answer, err error) string {
	var tooLarge *githubapp.ResponseTooLargeError
	if errors.As(err, &tooLarge) {
		return fmt.Sprintf("%s answer past %d MiB", route, readLimit>>20)
	}
	return fmt.Sprintf("%s refused (%d)", route, got.status)
}

// filesPatch reads the files[] of commit sha, every page of it
// (`GET /repos/{owner}/{repo}/commits/{sha}?per_page=100&page=n`, following each page's Link
// header), into the replay's form, a page at a time (survival.FilesPatch), so no page's patch text
// is held past its own, and answers how many files the pages listed.
func (g *GitHub) filesPatch(ctx context.Context, r Repository, sha string) (PatchRead, int, error) {
	files, withheld, listed, read := []survival.FileDiff{}, 0, 0, 0
	for path := r.repoPath() + "/commits/" + url.PathEscape(sha) + "?per_page=" + strconv.Itoa(perPage) + "&page=1"; path != ""; {
		got, err := g.readJSON(ctx, r, RouteFiles, path, githubapp.CheckResponse)
		if err != nil {
			return PatchRead{}, 0, fmt.Errorf("read the files of %s %s: %w", r.Repo, sha, err)
		}
		var page struct {
			Files []survival.FilesEntry `json:"files"`
		}
		if err := json.Unmarshal(got.body, &page); err != nil {
			return PatchRead{}, 0, fmt.Errorf("decode the files of %s %s: %w", r.Repo, sha, githubapp.ShownErr(err))
		}
		diffs, held := survival.FilesPatch(page.Files)
		files, withheld, listed = append(files, diffs...), withheld+held, listed+len(page.Files)
		read += len(got.body)
		if path, err = g.client.NextPage(got.header); err != nil {
			return PatchRead{}, 0, fmt.Errorf("read the files of %s %s: %w", r.Repo, sha, err)
		}
	}
	return PatchRead{Patch: survival.Patch{Files: files, Withheld: withheld}, Route: RouteFiles, Bytes: read}, listed, nil
}
