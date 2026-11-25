package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// limitedRead is a repository whose default-branch read GitHub refused for its rate limit
// (defaultBranchLimit): GitHub's answer, the read's error, which wraps it, how long GitHub asks the
// reader to wait, and until, when that wait ends, counted from when the answer came, so a caller
// that does other work first, such as reading the records, waits no longer for it.
type limitedRead struct {
	repo   ghrepo.Repository
	answer *githubrest.Answer
	err    error
	wait   time.Duration
	until  time.Time
}

// primaryLimitMessage is what GitHub's primary rate limit says in the body of its 403: "API rate
// limit exceeded for installation ID <id>", or for a user or an address.
const primaryLimitMessage = "api rate limit exceeded"

// unnamedLimitWait is how long a limit named by its body alone is waited: githubrest's wait for a
// rate limit whose answer names no wait (its retryAfter), the minute GitHub's REST documentation
// gives as the least to wait.
const unnamedLimitWait = time.Minute

// answerLimit is whether answer, GitHub's answer other than 2xx to a call, is GitHub's rate limit,
// with how long GitHub asks the caller to wait: an answer githubrest reads as the limit
// (Answer.RateLimited: a 429, or a 403 its headers or the secondary limit's message name a limit
// on), waited its RetryAfter, or a 403 whose body names the primary limit (primaryLimitMessage)
// while its headers do not, waited unnamedLimitWait. GitHub has refused an installation's reads of
// a repository with that 403 while the same installation's other requests were answered with
// thousands of calls left in x-ratelimit-remaining. The body is read here, as it came
// (githubrest.Answer.Says), for the reads of a default branch and the start's App calls alone, and
// not in githubrest, whose other callers each decide what such an answer means for their own reads.
func answerLimit(answer *githubrest.Answer) (time.Duration, bool) {
	switch {
	case answer.RateLimited:
		return answer.RetryAfter, true
	case answer.Status == http.StatusForbidden && answer.Says(primaryLimitMessage):
		return unnamedLimitWait, true
	}
	return 0, false
}

// defaultBranchLimit is GitHub's answer in err, a read of a default branch that failed, when that
// answer is GitHub's rate limit (answerLimit), with how long GitHub asks the reader to wait. A
// failed mint of the read's token is none: appauth keeps GitHub's answer to it out of the error's
// chain (appauth.AnswerError), which only the start's App calls read (bootLimit).
func defaultBranchLimit(err error) (*githubrest.Answer, time.Duration, bool) {
	var answer *githubrest.Answer
	if !errors.As(err, &answer) {
		return nil, 0, false
	}
	wait, limited := answerLimit(answer)
	if !limited {
		return nil, 0, false
	}
	return answer, wait, true
}

// heldThroughLimit is told of each repository served on a default branch the daemon holds in place
// of a read GitHub refused for its rate limit (holdLimited, and defaultBranches.Named for any read
// of a repository read or held before): it says so, naming the repository, the branch, the limit as
// GitHub's headers gave it and when the hold ends, and keeps the repository for the refresher's
// fifth job, which reads it from GitHub again once the hold ends (heldReads, rereadHeldBranches). A
// hold made before the refresher runs, during the boot, waits for it.
func (w *workflowRuntime) heldThroughLimit(read limitedRead, held ghbranch.Repository) {
	w.heldMu.Lock()
	if w.heldReads == nil {
		w.heldReads = map[string]limitedRead{}
	}
	w.heldReads[strings.ToLower(read.repo.String())] = read
	w.heldMu.Unlock()
	wake(w.heldWake)
	w.log.Warn("workflow: GitHub's rate limit refused the read of a repository's default branch, so the daemon serves it on the default branch it holds and reads it again after the limit's reset",
		append([]any{"repo", read.repo.String(), "defaultBranch", held.DefaultBranch, "readAgainAt", read.until.UTC().Format(time.RFC3339)}, limitAttributes(read)...)...)
}

// holdLimited serves read, a read GitHub refused for its rate limit, on the default branch a
// record of the project's issues holds for its repository, where one does, at once: GitHub's rate
// limit refuses a read for a while and says nothing about the repository, and the branch a record
// holds is the one GitHub named when the issue was recorded, which a read after the limit corrects
// if GitHub has since renamed it. read is only ever of a repository the daemon held no read of,
// since Named answers a limited read of one it holds from what it holds; the records are read
// once for the pass or the launch recorded is made for, and only when a read was refused
// (onceRecordedBranches). A held one stands until the limit's reset (defaultBranches.hold), so
// nothing reads it inside the limit, and every later read GitHub's limit refuses is answered from
// it too, the boot's own included (defaultBranches.Named); heldThroughLimit says so and has the
// refresher read it again then.
//
// A record holds a branch only for the repository it names itself, without case, and gives it in
// the record's own spelling, GitHub's as recorded, which is what intake's case-sensitive subjects
// are built from. A record recorded before a configured rename's transfer names the repository as
// it was, and gives nothing for the name it moved to: nothing the daemon holds names GitHub's
// spelling of that name (a configured pair holds only the spelling it was configured with), and a
// wrong case would leave intake deaf to the repository for as long as the hold stands. Such a
// repository is held only from a read of it the daemon made itself (Named), and otherwise read
// again as any repository with nothing held is.
//
// It answers the branch so served, as the record holds it, and whether one was; one no record holds
// a branch for is for the caller to keep as a read that did not answer (prepareName), which the
// refresher reads again while a live record names the repository.
func (w *workflowRuntime) holdLimited(read limitedRead, recorded recordBranches) (ghbranch.Repository, bool, error) {
	branches, err := recorded()
	if err != nil {
		return ghbranch.Repository{}, false, err
	}
	branch, ok := branches[strings.ToLower(read.repo.String())]
	if !ok {
		return ghbranch.Repository{}, false, nil
	}
	w.defaultBranches.hold(read.repo, branch, read.until)
	w.heldThroughLimit(read, branch)
	return branch, true, nil
}

// recordBranches is the default branch the project's records hold for each repository a record
// names (recordedBranches), as one pass of preparations, or one launch's, reads them: at most once,
// and only once a read GitHub's rate limit refused asks for them (holdLimited).
type recordBranches func() (map[string]ghbranch.Repository, error)

// onceRecordedBranches is the records' default branches for one pass or launch under ctx
// (recordBranches): recordedBranches, read at the first call and answered again, failure
// included, at every later one.
func (w *workflowRuntime) onceRecordedBranches(ctx context.Context) recordBranches {
	return sync.OnceValues(func() (map[string]ghbranch.Repository, error) { return w.recordedBranches(ctx) })
}

// recordedBranches is the default branch the project's records hold for each repository a record
// names (record.Issue.GitHubRepo, in GitHub's spelling, and DefaultBranch), keyed by that name
// lowercased, read in one read-only transaction bounded by the App-call attempt bound (callBounds). A repository several
// records name takes the first record in the store's order, by key, and not the newest: a record
// carries no time its repository and default branch were recorded at (both are written once, when
// the issue is first recorded, and no column dates that), and a key's number says nothing reliable
// either (LEGION-10 sorts before LEGION-9, and an old issue can be recorded late). Each holds the
// default branch GitHub named when its issue was recorded, and the one taken is served only until
// GitHub answers a read of it again. A record from before the record carried its repository holds
// nothing.
func (w *workflowRuntime) recordedBranches(ctx context.Context) (map[string]ghbranch.Repository, error) {
	read, cancel := context.WithTimeout(ctx, w.callBounds().attempt)
	defer cancel()
	var issues []record.Issue
	if err := store.TransactWith(read, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issues, err = w.records.Issues(ctx, tx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the default branches the project's records hold: %w", err)
	}
	recorded := map[string]ghbranch.Repository{}
	for _, issue := range issues {
		if issue.GitHubRepo == "" || issue.DefaultBranch == "" {
			continue
		}
		name, err := ghrepo.Parse("GitHub's spelling of the repository of "+issue.Key, issue.GitHubRepo)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(name.String())
		if _, taken := recorded[key]; !taken {
			recorded[key] = ghbranch.Repository{Name: name, DefaultBranch: issue.DefaultBranch}
		}
	}
	return recorded, nil
}

// limitAttributes is what a log line says of read's rate limit: how long GitHub asked the reader to
// wait, the budget githubrest read the limit as spending ("" for a limit named by its body alone),
// the answer's rate-limit headers as githubrest keeps them - each number GitHub sent written back
// from it, and any other text as the rule reads it ("" for each it did not send,
// githubrest.LimitHeaders) - so the line names the limit that refused the read as GitHub sent it,
// and the read's error, whose body names the installation the limit is on.
func limitAttributes(read limitedRead) []any {
	limit := read.answer.Limit
	return []any{"wait", read.wait.String(), "budget", string(read.answer.Budget), "rateLimit", limit.Limit, "rateLimitRemaining", limit.Remaining,
		"rateLimitReset", limit.Reset, "rateLimitResource", limit.Resource, "retryAfter", limit.RetryAfter, "error", read.err}
}

// backfillRead is named's read of repo for the backfill of a record (fillRepositories,
// nameRepositories): one read, bounded by attempt (repositoryReadBounds.attempt), answered from the read named keeps
// within its ttl, or holds until a limit's reset, without asking GitHub (defaultBranches.Named).
// A read that fails for any reason, GitHub's rate limit and a read GitHub does not answer among
// them, is returned as the cause the backfill logs ("the read of <repo> from GitHub failed: …"),
// for the caller to leave the record as it is and say so: a boot that waited on GitHub for a
// record's repository would serve nothing meanwhile, and the refresher tries the record again on
// its retry (repositoryReadBounds.retry, backfillLater), as the next boot does. No record's branch stands in for the
// read, since what the backfill reads GitHub for is GitHub's spelling of the repository the record
// will carry.
func backfillRead(ctx context.Context, named *defaultBranches, repo ghrepo.Repository, attempt time.Duration) (ghbranch.Repository, error) {
	read, cancel := context.WithTimeout(ctx, attempt)
	defer cancel()
	branch, err := named.Named(read, repo)
	if err != nil {
		return ghbranch.Repository{}, fmt.Errorf("the read of %s from GitHub failed: %w", repo, err)
	}
	return branch, nil
}
