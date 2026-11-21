package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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

// answerLimit is whether answer, GitHub's answer other than 2xx to a call, is GitHub's rate limit,
// with how long GitHub asks the caller to wait: an answer githubrest reads as the limit
// (Answer.RateLimited: a 429, or a 403 its headers or the secondary limit's message name a limit
// on), waited its RetryAfter, or a 403 whose body names the primary limit (primaryLimitMessage)
// while its headers do not, waited unnamedLimitWait. GitHub has refused an installation's reads of
// a repository with that 403 while the same installation's other requests were answered with
// thousands of calls left in x-ratelimit-remaining. The body is read here, for the reads of a
// default branch and the start's App calls alone, and not in githubrest, whose other callers each
// decide what such an answer means for their own reads.
func answerLimit(answer *githubrest.Answer) (time.Duration, bool) {
	switch {
	case answer.RateLimited:
		return answer.RetryAfter, true
	case answer.Status == http.StatusForbidden && strings.Contains(strings.ToLower(answer.Body), primaryLimitMessage):
		return unnamedLimitWait, true
	}
	return 0, false
}

// defaultBranchLimit is GitHub's answer in err, a read of a default branch that failed, when that
// answer is GitHub's rate limit (answerLimit), with how long GitHub asks the reader to wait. A
// failed mint of the read's token is none: appauth keeps GitHub's answer to it out of the error's
// chain (appauth.AnswerError), and the start waits out a rate limit there itself (bootLimit).
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

// bootLimit is GitHub's answer to an App call of the start that failed with err — the answer
// appauth's error carries (appauth.AnswerError) — when that answer is GitHub's rate limit
// (answerLimit), with how long GitHub asks the start to wait. A token exchange's 422 carrying the
// limit's headers is no limit githubrest names, so it is none here: the start retries it as
// GitHub's trouble (appTransient), as before.
func bootLimit(err error) (*githubrest.Answer, time.Duration, bool) {
	var refused *appauth.AnswerError
	if !errors.As(err, &refused) {
		return nil, 0, false
	}
	wait, limited := answerLimit(refused.Answer)
	if !limited {
		return nil, 0, false
	}
	return refused.Answer, wait, true
}

// rateLimitPolicy is how the start waits for GitHub's rate limit (dispatch://INFRA-2597/spec R4):
// Max, the longest one wait lasts, whatever GitHub names; Bound, how long after the start's first
// such wait every one of them ends, none ending at all when it is zero; and NoTime, the wait for a
// call's first refusal that names no time, doubled for each further one of the same call.
type rateLimitPolicy struct {
	Max, Bound, NoTime time.Duration
}

// startRateLimits is the start's policy. A wait lasts at most an hour, the window of GitHub's REST
// limit, so a spent counter is waited until its reset and a Retry-After longer than that is not
// waited whole. All waits of one start end 70 minutes after the first, the hour and ten minutes
// more: the refusals of 2026-11-14 and 2026-11-15 ended 5.6 to 13.9 minutes after the first exit.
// A refusal that names no time waits a minute, the least GitHub's REST documentation gives. Tests
// shorten its fields.
var startRateLimits = rateLimitPolicy{Max: time.Hour, Bound: 70 * time.Minute, NoTime: time.Minute}

// startWaits is one start's waits for GitHub's rate limit on one policy and one clock, now: the
// App logins read and the token mints (wait) and the default-branch reads (readUntil). end is the
// start's end, zero until its first wait sets it Bound after that wait begins; noTime is the last
// wait each App call took for a refusal that named no time; waited is the time the start has spent
// in at least one wait, and latest the end of the latest wait. It is safe for concurrent use: the
// default-branch reads' waits overlap (waitOutLimits).
type startWaits struct {
	policy rateLimitPolicy
	now    func() time.Time

	mu     sync.Mutex
	end    time.Time
	noTime map[string]time.Duration
	waited time.Duration
	latest time.Time
}

// newStartWaits is one start's waits on policy and the clock now.
func newStartWaits(policy rateLimitPolicy, now func() time.Time) *startWaits {
	return &startWaits{policy: policy, now: now, noTime: map[string]time.Duration{}}
}

// orUnbounded is s, or, for a nil s — a runtime a test builds as a literal — waits of their own for
// one wait, on the wall clock, with the start's maximum and no end.
func (s *startWaits) orUnbounded() *startWaits {
	if s != nil {
		return s
	}
	return newStartWaits(rateLimitPolicy{Max: startRateLimits.Max, NoTime: startRateLimits.NoTime}, time.Now)
}

// wait is the wait for GitHub's rate limit on the start's App call call (the logins read, a mint),
// asked as soon as the call's attempt returned with answer, GitHub's refusal, from which
// answerLimit read githubWait. Where GitHub named a time (namesTime), the wait is githubWait with
// at most Max; where it named none, NoTime for the call's first such refusal of the start and two
// times its wait before for each further one. It says until when, at the start's end at the
// latest, and the time the start has waited so far, this wait included (register); or, once now is
// at or past the start's end, ended, with no wait: the call then ends the start.
func (s *startWaits) wait(call string, answer *githubrest.Answer, githubWait time.Duration) (until time.Time, waited time.Duration, ended bool) {
	s = s.orUnbounded()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.end.IsZero() && !now.Before(s.end) {
		return now, s.waited, true
	}
	wait := min(githubWait, s.policy.Max)
	if !namesTime(answer.Limit, now) {
		wait = s.policy.NoTime
		if last, ok := s.noTime[call]; ok {
			wait = 2 * last
		}
		s.noTime[call] = wait
	}
	until, waited = s.register(now, now.Add(wait))
	return until, waited, false
}

// readUntil is the wait for read, a default-branch read GitHub's rate limit refused: #4043's wait,
// counted from when GitHub answered (limitedRead), with at most Max, and at the start's end at the
// latest. until is now, with no wait, when that wait is not ahead of now or now is at or past the
// start's end. It keeps no wait of a refusal that named no time: each repository's read waits once,
// so several refused together each wait GitHub's own wait. waited is the time the start has waited
// so far, this wait included (register).
func (s *startWaits) readUntil(read limitedRead) (until time.Time, waited time.Duration) {
	s = s.orUnbounded()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	until = read.until.Add(min(read.wait, s.policy.Max) - read.wait)
	if !s.end.IsZero() && !now.Before(s.end) || !until.After(now) {
		return now, s.waited
	}
	return s.register(now, until)
}

// register counts a wait from begin until until, cut at the start's end, and says until and the
// time the start has waited so far: the length of the union of its waits, each adding the part of
// it past the latest end so far. The waits are registered in time order under mu, so the sum is
// exact and overlapping waits count once. The start's first wait sets its end, Bound after that
// wait begins.
func (s *startWaits) register(begin, until time.Time) (time.Time, time.Duration) {
	if s.end.IsZero() && s.policy.Bound > 0 {
		s.end = begin.Add(s.policy.Bound)
	}
	if !s.end.IsZero() && until.After(s.end) {
		until = s.end
	}
	from := begin
	if s.latest.After(from) {
		from = s.latest
	}
	if until.After(from) {
		s.waited += until.Sub(from)
		s.latest = until
	}
	return until, s.waited
}

// namesTime is whether limit, the rate-limit headers of GitHub's answer, names when the limit ends,
// as GitHub's REST documentation reads them: a Retry-After of more than 0 seconds, or no call left
// (X-RateLimit-Remaining 0) with an X-RateLimit-Reset still ahead of now. A 403 named by its body
// alone, and a reset already past, name none.
func namesTime(limit githubrest.LimitHeaders, now time.Time) bool {
	if seconds, err := strconv.Atoi(limit.RetryAfter); err == nil && seconds > 0 {
		return true
	}
	reset, err := strconv.ParseInt(limit.Reset, 10, 64)
	return limit.Remaining == "0" && err == nil && time.Unix(reset, 0).After(now)
}

// backfillWait is how long the backfill waits for a read GitHub's rate limit refused, GitHub asking
// for wait: at most the start's Max, with no end, since the backfill reads until GitHub answers
// (backfillRead).
func backfillWait(wait time.Duration) time.Duration { return min(wait, startRateLimits.Max) }

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

// waitOutLimits is the boot's answer for each of unheld, a repository whose default-branch read
// GitHub's rate limit refused and that the daemon holds no default branch for (holdLimited): it
// waits as long as GitHub asks (githubrest's retryAfter: the Retry-After, else until the reset when
// no call is left, at most an hour, else a minute; a minute too for a limit named by its body
// alone), counted from GitHub's answer, with at most the start's maximum and to the start's end at
// the latest (startWaits.readUntil), logging one line that names the repository, the limit as
// GitHub's answer gave it, the time it waits until and the time the start has waited so far, and
// then reads it once more. A repository that read answers is served as GitHub names it (named). One
// GitHub answers about, such as with a 404, refuses the boot, as it does on the first read
// (answeredRefusal). One it does not answer — the limit again, a 5xx, a failed mint of the token, a
// transport failure, or no answer within appMintAttempt — is left out for now (unread), as a
// repository whose installation read failed is, and read again on installReadRetry until GitHub
// answers, so the limit costs that repository and not the project's boot. The waits overlap, each
// for its own limit, so the boot waits as long as the longest of them, and the reads after them run
// mappingReads at a time, as the first reads did.
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
			until, waited := w.startWaits.readUntil(read)
			// The line's wait is the wait the boot takes, counted from GitHub's answer as read.wait
			// is, so a wait the maximum or the start's end cut reads as cut; the headers beside it
			// say what GitHub asked.
			taken := read
			taken.wait = until.Sub(read.until.Add(-read.wait))
			w.log.Warn("workflow: GitHub's rate limit refused the read of a repository's default branch and the daemon holds none for it, so the boot waits as long as GitHub asks and reads it once more",
				append([]any{"repo", read.mapped.Repo.String(), "waitUntil", until.UTC().Format(time.RFC3339), "waited", bootprobe.ShownWait(waited)}, limitAttributes(taken)...)...)
			select {
			case <-waiting.Done():
				return waiting.Err()
			case <-time.After(time.Until(until)):
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
// holds err's text as the daemon keeps any far side's error text (storetext.FarSideError).
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
// (defaultBranchLimit), with at most the start's maximum and no end (backfillWait); one GitHub does
// not answer (backfillUnanswered) is waited 1 s, doubling to a minute. Each wait is one line naming
// the issue, the repository and the time it waits until, and the read is made again, until GitHub
// answers or ctx ends. Any other failure — GitHub's answer about the repository, such as a 404, or
// a mint refused — is returned and refuses the boot, as before. No record's branch stands in for
// the read, since what the backfill reads GitHub for is GitHub's spelling of the repository the
// record will carry.
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
			wait = backfillWait(wait)
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
