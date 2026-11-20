package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
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
// App logins read and the key proof (wait). end is the start's end, zero until its first wait sets
// it Bound after that wait begins; noTime is the last wait each App call took for a refusal that
// named no time; waited is the time the start has spent in at least one wait, and latest the end
// of the latest wait. It is safe for concurrent use.
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

// wait is the wait for GitHub's rate limit on the start's App call call (the logins read, the key
// proof), asked as soon as the call's attempt returned with answer, GitHub's refusal, from which
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

// backfillRead is named's read of repo for the backfill of a record (fillRepositories,
// nameRepositories): one read, bounded by installReadAttempt, answered from the read named keeps
// within its ttl, or holds until a limit's reset, without asking GitHub (defaultBranches.Named).
// A read that fails for any reason, GitHub's rate limit and a read GitHub does not answer among
// them, is returned as the cause the backfill logs ("the read of <repo> from GitHub failed: …"),
// for the caller to leave the record as it is and say so: a boot that waited on GitHub for a
// record's repository would serve nothing meanwhile, and the refresher tries the record again on
// installReadRetry (backfillLater), as the next boot does. No record's branch stands in for the
// read, since what the backfill reads GitHub for is GitHub's spelling of the repository the record
// will carry.
func backfillRead(ctx context.Context, named *defaultBranches, repo ghrepo.Repository) (ghbranch.Repository, error) {
	read, cancel := context.WithTimeout(ctx, installReadAttempt)
	defer cancel()
	branch, err := named.Named(read, repo)
	if err != nil {
		return ghbranch.Repository{}, fmt.Errorf("the read of %s from GitHub failed: %w", repo, err)
	}
	return branch, nil
}
