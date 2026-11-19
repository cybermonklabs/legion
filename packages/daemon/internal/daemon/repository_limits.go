package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// limitedRead is a mapped repository whose default-branch read GitHub refused for its rate limit
// (defaultBranchLimit): GitHub's answer, the read's error, which wraps it, how long GitHub asks the
// reader to wait, and until, when that wait ends, counted from when the answer came, so a caller
// that does other work first, such as reading the records, waits no longer for it.
type limitedRead struct {
	mapped mappedRepository
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

// defaultBranchLimit is GitHub's answer in err, a read of a default branch that failed, when that
// answer is GitHub's rate limit, with how long GitHub asks the reader to wait: an answer githubrest
// reads as the limit (Answer.RateLimited: a 429, or a 403 its headers or the secondary limit's
// message name a limit on), waited its RetryAfter, or a 403 whose body names the primary limit
// (primaryLimitMessage) while its headers do not, waited unnamedLimitWait. GitHub has refused an
// installation's reads of a repository with that 403 while the same installation's other requests
// were answered with thousands of calls left in x-ratelimit-remaining. The body is read here, for
// the reads of a default branch alone, and not in githubrest, whose other callers each decide
// what such an answer means for their own reads.
func defaultBranchLimit(err error) (*githubrest.Answer, time.Duration, bool) {
	var answer *githubrest.Answer
	switch {
	case !errors.As(err, &answer):
		return nil, 0, false
	case answer.RateLimited:
		return answer, answer.RetryAfter, true
	case answer.Status == http.StatusForbidden && strings.Contains(strings.ToLower(answer.Body), primaryLimitMessage):
		return answer, unnamedLimitWait, true
	}
	return nil, 0, false
}

// answeredRefusal is whether err, a read of a default branch that failed, is GitHub's answer about
// the repository, which no later read is expected to change: a 404, or any other answer below 500
// that is no rate limit (defaultBranchLimit). Every other failure is a read GitHub did not answer:
// its rate limit, a 5xx, a failed mint of the token, a transport failure or a read cut off by its
// bound.
func answeredRefusal(err error) bool {
	var answer *githubrest.Answer
	if !errors.As(err, &answer) || answer.Status >= 500 {
		return false
	}
	_, _, limited := defaultBranchLimit(err)
	return !limited
}

// bootReadTransient is a failure of the boot's read of a repository that GitHub reports as its own
// trouble, which the boot retries on appMintRetry: the App token's mint failing so
// (appTransient), or a 5xx.
func bootReadTransient(err error) bool {
	var answer *githubrest.Answer
	return appTransient(err) || errors.As(err, &answer) && answer.Status >= 500
}

// heldThroughLimit is told of each repository served on a default branch the daemon holds in place
// of a read GitHub refused for its rate limit (holdLimited, and defaultBranches.Named for any read
// of a repository read or held before): it says so, naming the repository, the branch, the limit as
// GitHub's headers gave it and when the hold ends, and asks for a read of the mapping then
// (requestMappingRead), which reads the repository from GitHub again. A request made before the
// refresher runs, during the boot, waits for it.
func (w *workflowRuntime) heldThroughLimit(read limitedRead, held ghbranch.Repository) {
	time.AfterFunc(time.Until(read.until), func() { w.requestMappingRead() })
	w.log.Warn("workflow: GitHub's rate limit refused the read of a repository's default branch, so the daemon serves it on the default branch it holds and reads it again after the limit's reset",
		append([]any{"repo", read.mapped.Repo.String(), "defaultBranch", held.DefaultBranch, "readAgainAt", read.until.UTC().Format(time.RFC3339)}, limitAttributes(read)...)...)
}

// holdLimited serves each of limited that a record of the project's issues holds a default branch
// for on that branch, at once: GitHub's rate limit refuses a read for a while and says nothing
// about the repository, and the branch a record holds is the one GitHub named when the issue was
// recorded, which a read after the limit corrects if GitHub has since renamed it. limited is only
// ever repositories the daemon held no read of, since Named answers a limited read of one it holds
// from what it holds; the records are read once, and only when a read was refused. Each held one
// stands until the limit's reset (defaultBranches.hold), so nothing reads it inside the limit, and
// every later read GitHub's limit refuses is answered from it too, the boot's own included
// (defaultBranches.Named); heldThroughLimit says so and has the mapping read again then.
//
// A record holds a branch only for the repository it names itself, without case, and gives it in
// the record's own spelling, GitHub's as recorded, which is what intake's case-sensitive subjects
// are built from. A record recorded before a configured rename's transfer names the repository as
// it was, and gives nothing for the name it moved to: neither the record nor the mapping holds
// GitHub's spelling of that name (the mapping can hold the pair's spelling as configured), and a
// wrong case would leave intake deaf to the repository for as long as the hold stands. Such a
// repository is held only from a read of it the daemon made itself (Named), and otherwise waited
// out or left out as any repository with nothing held is.
//
// held is each so served; unheld is each no record holds a branch for, for the caller to wait out
// (the boot's waitOutLimits) or to leave out for now (refreshMapping).
func (w *workflowRuntime) holdLimited(ctx context.Context, limited []limitedRead) (held []mappedRepository, unheld []limitedRead, err error) {
	if len(limited) == 0 {
		return nil, nil, nil
	}
	recorded, err := w.recordedBranches(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, read := range limited {
		branch, ok := recorded[strings.ToLower(read.mapped.Repo.String())]
		if !ok {
			unheld = append(unheld, read)
			continue
		}
		w.defaultBranches.hold(read.mapped.Repo, branch, read.until)
		w.heldThroughLimit(read, branch)
		held = append(held, mappedRepository{Repo: branch.Name, Default: read.mapped.Default})
	}
	return held, unheld, nil
}

// recordedBranches is the default branch the project's records hold for each repository a record
// names (record.Issue.GitHubRepo, in GitHub's spelling, and DefaultBranch), keyed by that name
// lowercased, read in one read-only transaction bounded by appMintAttempt. A repository several
// records name takes the first record in the store's order, by key, and not the newest: a record
// carries no time its repository and default branch were recorded at (both are written once, when
// the issue is first recorded, and no column dates that), and a key's number says nothing reliable
// either (LEGION-10 sorts before LEGION-9, and an old issue can be recorded late). Each holds the
// default branch GitHub named when its issue was recorded, and the one taken is served only until
// GitHub answers a read of it again. A record from before the record carried its repository holds
// nothing.
func (w *workflowRuntime) recordedBranches(ctx context.Context) (map[string]ghbranch.Repository, error) {
	read, cancel := context.WithTimeout(ctx, appMintAttempt)
	defer cancel()
	var issues []record.Issue
	if err := pgx.BeginTxFunc(read, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		issues, err = w.records.Issues(read, tx)
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

// waitOutLimits is the boot's answer for each of unheld, a repository whose default-branch read
// GitHub's rate limit refused and that the daemon holds no default branch for (holdLimited): it
// waits as long as GitHub asks (githubrest's retryAfter: the Retry-After, else until the reset when
// no call is left, at most an hour, else a minute; a minute too for a limit named by its body
// alone), logging one line that names the repository, the limit as GitHub's answer gave it and the
// time it waits until, and then reads it once more. A repository that read answers is served as
// GitHub names it (named). One GitHub answers about, such as with a 404, refuses the boot, as it
// does on the first read (answeredRefusal). One it does not answer — the limit again, a 5xx, a
// failed mint of the token, a transport failure, or no answer within appMintAttempt — is left out
// for now (unread), as a repository whose installation read failed is, and read again on
// installReadRetry until GitHub answers, so the limit costs that repository and not the project's
// boot. The waits overlap, each for its own limit, so the boot waits as long as the longest of them
// asks, and the reads after them run mappingReads at a time, as the first reads did.
func (w *workflowRuntime) waitOutLimits(ctx context.Context, unheld []limitedRead) (named []mappedRepository, unread []refusedRepository, _ error) {
	if len(unheld) == 0 {
		return nil, nil, nil
	}
	// Each read writes its own index of answered or failed, never both, and only it does.
	answered := make([]mappedRepository, len(unheld))
	failed := make([]error, len(unheld))
	reading := make(chan struct{}, mappingReads)
	group, waiting := errgroup.WithContext(ctx)
	for index, read := range unheld {
		group.Go(func() error {
			w.log.Warn("workflow: GitHub's rate limit refused the read of a repository's default branch and the daemon holds none for it, so the boot waits as long as GitHub asks and reads it once more",
				append([]any{"repo", read.mapped.Repo.String(), "waitUntil", read.until.UTC().Format(time.RFC3339)}, limitAttributes(read)...)...)
			select {
			case <-waiting.Done():
				return waiting.Err()
			case <-time.After(time.Until(read.until)):
			}
			select {
			case <-waiting.Done():
				return waiting.Err()
			case reading <- struct{}{}:
			}
			attempt, cancel := context.WithTimeout(waiting, appMintAttempt)
			repository, err := w.defaultBranches.Named(attempt, read.mapped.Repo)
			cancel()
			<-reading
			switch {
			case err == nil:
				answered[index] = mappedRepository{Repo: repository.Name, Default: read.mapped.Default}
			case waiting.Err() != nil:
				return waiting.Err()
			case answeredRefusal(err):
				return err
			default:
				failed[index] = err
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, nil, err
	}
	for index, read := range unheld {
		if failed[index] == nil {
			named = append(named, answered[index])
			continue
		}
		w.log.Warn("workflow: GitHub did not answer the read of a repository's default branch after its rate limit, so the daemon boots leaving it out and reads it again",
			"repo", read.mapped.Repo.String(), "retryIn", installReadRetry.Initial.String(), "error", failed[index])
		unread = append(unread, unreadDefaultBranch(read.mapped, failed[index]))
	}
	return named, unread, nil
}

// unreadDefaultBranch is mapped left out for now because GitHub did not answer the read of its
// default branch with err: a refusal carrying err as its readErr, which is what the refresher reads
// the mapping again for (awaitingRead, installReadRetry), and admission reads as a repository
// GitHub is being read again for (lookup's unread). Both role Apps are carried as installed,
// since GitHub answered so before the default branch was read (checkMapping accepted it): legion
// state reports what GitHub answered of the Apps on every refusal it has an answer for, so this
// one row cannot read as an App missing (refusedRepository.apps). The Reason legion state serves
// holds err's text as the daemon keeps any far side's error text (storetext.FarSideError):
// cleaned, its URL credentials removed, and bounded.
func unreadDefaultBranch(mapped mappedRepository, err error) refusedRepository {
	return refusedRepository{Repo: mapped.Repo.String(), Reason: fmt.Sprintf("GitHub did not answer the read of its default branch: %s; Legion reads it again", storetext.FarSideError(err.Error())),
		apps: appAnswers{answered: true, implement: true, review: true}, readErr: err, mappedDefault: mapped.Default}
}

// limitAttributes is what a log line says of read's rate limit: how long GitHub asked the reader to
// wait, the budget githubrest read the limit as spending ("" for a limit named by its body alone),
// the answer's rate-limit headers as GitHub sent them ("" for each it did not send), so the line
// names the limit that refused the read in GitHub's own words, and the read's error, whose body
// names the installation the limit is on.
func limitAttributes(read limitedRead) []any {
	limit := read.answer.Limit
	return []any{"wait", read.wait.String(), "budget", string(read.answer.Budget), "rateLimit", limit.Limit, "rateLimitRemaining", limit.Remaining,
		"rateLimitReset", limit.Reset, "rateLimitResource", limit.Resource, "retryAfter", limit.RetryAfter, "error", read.err}
}

// backfillRead is named's read of repo for the boot's backfill of key's record (fillRepositories,
// nameRepositories). GitHub's rate limit, or a read GitHub does not answer, neither ends the boot
// nor leaves the record unfilled, since every later reader of the record takes its repository and
// GitHub's spelling of it as given. A repository read within named's ttl, or held until a limit's
// reset, is answered from that read or hold and GitHub is not asked (defaultBranches.Named). Past
// that, only a read GitHub's limit refuses is answered from the last read or hold, however old; a
// read GitHub does not answer, such as a 5xx, a transport failure or a timeout, is not, and takes
// the backoff below. One GitHub's limit refuses with nothing held is waited out as GitHub asks
// (defaultBranchLimit); one GitHub does not answer (backfillUnanswered) is waited 1 s, doubling to
// a minute. Each wait is one line naming the issue, the repository and the time it waits until,
// and the read is made again, until GitHub answers or ctx ends. Any other failure — GitHub's
// answer about the repository, such as a 404, or a mint refused — is returned and refuses the
// boot, as before. No record's branch stands in for the read, since what the backfill reads GitHub
// for is GitHub's spelling of the repository the record will carry.
func backfillRead(ctx context.Context, named *defaultBranches, repo ghrepo.Repository, key string, log *slog.Logger) (ghbranch.Repository, error) {
	backoff := time.Second
	for {
		attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
		read, err := named.Named(attempt, repo)
		cancel()
		if err == nil {
			return read, nil
		}
		if ctx.Err() != nil {
			return ghbranch.Repository{}, ctx.Err()
		}
		attributes := []any{"issue", key, "repo", repo.String()}
		answer, wait, limited := defaultBranchLimit(err)
		switch {
		case limited:
			attributes = append(attributes, limitAttributes(limitedRead{answer: answer, err: err, wait: wait})...)
		case backfillUnanswered(err):
			wait, backoff = backoff, min(2*backoff, time.Minute)
			attributes = append(attributes, "wait", wait.String(), "error", err)
		default:
			return ghbranch.Repository{}, err
		}
		log.Warn("workflow backfill: GitHub's rate limit refused the read of a record's repository, or GitHub did not answer it, so the boot waits and reads it again",
			append(attributes, "waitUntil", time.Now().Add(wait).UTC().Format(time.RFC3339))...)
		select {
		case <-ctx.Done():
			return ghbranch.Repository{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// backfillUnanswered is whether err, a read of a default branch that failed, is one GitHub did not
// answer and no rate limit: a 5xx or a mint GitHub reports as its own trouble (bootReadTransient), a
// transport failure (the *url.Error the HTTP client returns), or a read cut off by its bound.
func backfillUnanswered(err error) bool {
	var transport *url.Error
	return bootReadTransient(err) || errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded)
}
