package cireconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

const (
	// VerifyInterval is how often the verify loop lists the CI store's verify requests: a head a
	// listener holds with ENVOY_CI_VERIFY on waits about this long, and the head's GitHub read,
	// before its settlement is published.
	VerifyInterval = 5 * time.Second

	// VerifyLease names the lease a verify pass answers its requests under, apart from Lease, so a
	// reconcile pass that takes minutes never holds a verify back. Two processes answering one
	// request would both be safe -- every write is a compare-and-swap of what GitHub holds, and an
	// answer only raises VerifiedRev -- but each would read the head from GitHub. A pass that lists
	// no request takes no lease at all (Verify).
	VerifyLease = "ci-verify"

	// verifyRetry is how long the verify loop leaves a head after the first failed answer of an
	// episode before it reads the head again: a repository whose token or read fails fails again
	// at once, and every five seconds would spend the App's budget on it. A minute rides out a
	// GitHub error or a NATS reconnect at the cost of one more read.
	verifyRetry = time.Minute

	// verifyRetryMax is the longest the verify loop leaves a head it fails to read: the wait after
	// each failed answer of one episode doubles from verifyRetry up to it (verifyRetryAfter), and
	// the listener's rewrites of the request do not cut it short, so a GitHub or NATS outage costs
	// a waiting head reads at 1, 3, 7 and 15 minutes after the first and every 8 minutes from then,
	// and the head is read at most 8 minutes after the outage ends, or at once in the listener's
	// next episode. The loop never gives an episode up for such a failure: the listener's own
	// episode, ten minutes, bounds how long a request is there to read.
	verifyRetryMax = 8 * time.Minute

	// verifyForget is how long the verify loop remembers a head it failed on after its last
	// failure: an hour, past the ten minutes a listener asks for one episode, so a request of an
	// episode the loop gave up (permanentVerifyFailure) that the listener writes again is
	// deleted unread rather than read anew.
	verifyForget = time.Hour
)

// verifyFailure is what the verify loop remembers of a head it could not answer: the settlement
// episode and the request revision it failed at, how many answers of that episode failed in a row,
// when it reads the head again, whether it gave the episode up, and when it last failed.
type verifyFailure struct {
	episode  uint64
	revision uint64
	failures int
	retryAt  time.Time
	gaveUp   bool
	at       time.Time
}

// errWriteNotLanded is the failure of a verify read whose write of what GitHub answered did not
// land: record logs the write's own error.
var errWriteNotLanded = errors.New("the CI store did not take a write of what GitHub answered")

// Verify is one pass of the verify loop (duty.WokenLoop.Pass), and answers when the next is due.
// It lists the verify requests the CI store holds, each at the revision it holds it (V0), with no
// lease: a pass that lists none takes no lease connection, logs nothing and is done, which is what
// every pass is while no listener runs with ENVOY_CI_VERIFY on. A pass that lists some answers them
// under VerifyLease (VerifyLeases; a Reconciler with none, a test's, answers them with no lease).
//
// Each answer reads the head from GitHub whole -- every page of its check runs and its check
// suites, and of its commit statuses where the token holds Commit statuses, unconditionally, as the
// walk reads a head over REST -- writes what it read by the walk's own rules (record), and then
// writes V0 as the record's VerifiedRev (cistore.Writer.MarkVerified). V0 is read before the GitHub
// read and is never read again after it, whatever the request's key holds by then, so an answer
// reaches only requests written before the read began. A read that fails, a listing that does not
// end within maxPages, and a write that does not land leave VerifiedRev as it was: only a complete
// answer counts, an empty one included. A status listing GitHub refuses costs the answer the
// head's statuses alone, as it costs the walk. A head GitHub still runs a check suite of with runs
// in it is written but not answered (runningSuite). An answered request is deleted only where the
// key still holds it at V0 (cistore.Writer.DoneVerifying), so a newer request written meanwhile
// stays for the next pass. A limit in force on the App's budget ends the pass with the requests it
// has not reached left in place; a limit on one installation's budget costs that installation's
// heads alone.
//
// A head the loop cannot answer is read again after verifyRetryAfter, a wait that doubles at each
// failed answer of its episode up to verifyRetryMax, whether or not the listener writes the request
// again meanwhile; a new episode of the head is read at once. The loop gives an episode up only for a
// failure no later read mends (permanentVerifyFailure), at once: it logs one ERROR naming the head
// and why, deletes the request at the revision it read, and deletes every later request of that
// episode unread.
//
// Verify touches none of what a reconcile pass remembers (heads, rows, the cursor): it reads
// GitHub with the Reconciler's collaborators alone, so it runs beside a pass. Only its own record
// of failed requests is kept between its passes, and one Verify runs at a time.
func (r *Reconciler) Verify(ctx context.Context) time.Time {
	next := time.Now().Add(VerifyInterval)
	writer, err := r.Open()
	if err != nil {
		// NATS is not connected, or the listener has not made the bucket: no request can be
		// read, and the next pass binds the writer again.
		return next
	}
	requests, err := writer.VerifyRequests(ctx)
	if err != nil {
		r.Logger.Warn("ci verify did not list the verify requests", "error", err)
		return next
	}
	r.forgetVerifyFailures()
	if len(requests) == 0 {
		return next
	}
	answer := func(ctx context.Context) { r.answer(ctx, writer, requests) }
	if r.VerifyLeases == nil {
		answer(ctx)
		return next
	}
	duty.Exclusive(ctx, nil, r.VerifyLeases, VerifyLease, time.Time{}, 0, answer)
	return next
}

// forgetVerifyFailures drops every failed head whose last failure is verifyForget old.
func (r *Reconciler) forgetVerifyFailures() {
	cutoff := r.clock().Add(-verifyForget)
	for key, failed := range r.verifyFailed {
		if failed.at.Before(cutoff) {
			delete(r.verifyFailed, key)
		}
	}
}

// answer answers requests in turn (verify), until the pass's end or a limit on the App's budget.
func (r *Reconciler) answer(ctx context.Context, writer Writer, requests []contracts.CIVerifyRequest) {
	for _, request := range requests {
		if ctx.Err() != nil {
			return
		}
		if at := r.limited(ctx, githubapp.AppScope); at != nil {
			return
		}
		if !r.verify(ctx, writer, request) {
			return
		}
	}
}

// verifyHeadKey is the key a pass remembers head under (pullHead.key).
func verifyHeadKey(head contracts.CIHead) string {
	return pullHead{owner: head.Owner, repo: head.Repo, number: head.Number, sha: head.SHA}.key()
}

// verify answers one request (Verify), and reports whether the pass goes on to the next: false
// when a limit on the App's budget stopped it, or the pass ended.
func (r *Reconciler) verify(ctx context.Context, writer Writer, request contracts.CIVerifyRequest) bool {
	head, key := request.Head, verifyHeadKey(request.Head)
	now := r.clock()
	failed, held := r.verifyFailed[key]
	if held && failed.episode != request.Episode {
		// A new settlement episode of the head is asked afresh, whatever the last one met.
		held, failed = false, verifyFailure{}
	}
	if held && failed.gaveUp {
		r.doneVerifying(ctx, writer, request)
		return true
	}
	if held && now.Before(failed.retryAt) && (failed.failures > 0 || failed.revision == request.Revision) {
		// A failed head waits out its backoff, whichever request of the episode asks; a head whose
		// answer was held (runningSuite) is read at once when its request is written again.
		return true
	}
	fields := []any{"repository", head.Owner + "/" + head.Repo, "pr", head.Number, "sha", head.SHA, "request", request.Revision}
	fail := func(what string, err error) {
		failed.episode, failed.revision, failed.at = request.Episode, request.Revision, now
		failed.failures++
		if permanentVerifyFailure(err) {
			failed.gaveUp = true
			remember(&r.verifyFailed, key, failed)
			r.Logger.Error("ci verify gave up an episode: no later read of the head mends its failure",
				append(fields, "reason", what, "failures", failed.failures, "error", err)...)
			r.doneVerifying(ctx, writer, request)
			return
		}
		retry := verifyRetryAfter(failed.failures)
		failed.retryAt = now.Add(retry)
		remember(&r.verifyFailed, key, failed)
		r.Logger.Warn("ci verify did not answer a verify request: "+what,
			append(fields, "failures", failed.failures, "retry_in", retry.String(), "error", err)...)
	}
	token, statuses, err := r.GitHub.RepositoryTokenWithStatuses(ctx, head.Owner, head.Repo)
	if at := stopAt(err, githubapp.AppScope); at != nil {
		return at.scope != githubapp.AppScope
	}
	if err != nil {
		fail("no token for the repository", err)
		return true
	}
	scope := r.GitHub.TokenScope(token)
	if at := r.limited(ctx, scopes(scope)...); at != nil {
		return at.scope != githubapp.AppScope
	}
	pull := pullHead{token: token, scope: scope, owner: head.Owner, repo: head.Repo, number: head.Number, sha: head.SHA, statuses: statuses}
	// A count of the verify's own: the pass's pace and its line are no part of it, and a count
	// with no pace leaves no gap between requests (pace).
	var c counts
	runs, err := readPages(ctx, r, &c, &c.headRequests, nil, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
		answer, err := r.GitHub.CommitCheckRuns(ctx, token, pull.owner, pull.repo, pull.sha, page, etag)
		return objectPage(answer), err
	})
	var suites []pageOf[object]
	if err == nil {
		suites, err = readPages(ctx, r, &c, &c.headRequests, nil, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
			answer, err := r.GitHub.CommitCheckSuites(ctx, token, pull.owner, pull.repo, pull.sha, page, etag)
			return objectPage(answer), err
		})
	}
	if err != nil {
		if at := stopAt(err, scope); at != nil {
			return at.scope != githubapp.AppScope
		}
		if ctx.Err() != nil {
			return false
		}
		fail("GitHub's answer for the head was not read whole", err)
		return true
	}
	var listedStatuses []pageOf[object]
	if statuses {
		listedStatuses, err = readPages(ctx, r, &c, &c.headRequests, nil, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
			answer, err := r.GitHub.CommitStatuses(ctx, token, pull.owner, pull.repo, pull.sha, page, etag)
			return objectPage(answer), err
		})
		if err != nil {
			if at := stopAt(err, scope); at != nil {
				return at.scope != githubapp.AppScope
			}
			if ctx.Err() != nil {
				return false
			}
			// As in the walk (readStatuses), a status listing GitHub refuses costs the head its
			// statuses alone: a commit status takes no part in whether a head is ready to settle.
			r.statusesUnread(ctx, pull, err)
			listedStatuses = nil
		}
	}
	// The answer is two writes in this order, never one: what GitHub answered (record, by the
	// walk's rules, the runs in one compare-and-swap), and then V0 as the record's verified_rev
	// (MarkVerified), written only once the first landed whole. Two ordered writes are as safe as
	// one: the listener judges readiness on the merged record, the observations of a complete read
	// are durable before the stamp, any write between or after only adds observations (which
	// re-arm the record and take the claim's snapshot away when they change it), and a new
	// settlement episode writes a new request, which V0 is below. One compare-and-swap conditioned
	// on the record's revision before the GitHub read would lose to every write of a waiting head,
	// its own claim's refresh included, and a busy head could wait for ever.
	if _, _, ok := r.record(ctx, writer, pull, runs, suites, listedStatuses); !ok {
		fail("a write of what GitHub answered did not land", errWriteNotLanded)
		return true
	}
	if suite := runningSuite(suites); suite != "" {
		// GitHub is still filling the head: a suite with runs in it has not completed, as an
		// Actions workflow between one job's end and its dependants' runs, which GitHub creates a
		// moment later. The runs read are written, and no answer is, so the head is not announced
		// on runs GitHub is about to add to. This is no failure of the read; the head is read
		// again after verifyRetry, or at once when its request is written again.
		failed.episode, failed.revision, failed.at = request.Episode, request.Revision, now
		failed.failures = 0
		failed.retryAt = now.Add(verifyRetry)
		remember(&r.verifyFailed, key, failed)
		r.Logger.Info("ci verify held its answer: GitHub still runs a check suite of the head", append(fields, "suite", suite)...)
		return true
	}
	if _, err := writer.MarkVerified(ctx, head, request.Revision, request.Stream); err != nil {
		fail("the answer was not written", err)
		return true
	}
	delete(r.verifyFailed, key)
	r.doneVerifying(ctx, writer, request)
	return true
}

// doneVerifying deletes request where the key still holds it at its revision.
func (r *Reconciler) doneVerifying(ctx context.Context, writer Writer, request contracts.CIVerifyRequest) {
	head := request.Head
	if err := writer.DoneVerifying(ctx, head, request.Revision); err != nil {
		r.Logger.Warn("ci verify did not delete a verify request",
			"repository", head.Owner+"/"+head.Repo, "pr", head.Number, "sha", head.SHA, "request", request.Revision, "error", err)
	}
}

// verifyRetryAfter is how long the verify loop leaves a head after the failures-th failed answer in
// a row of one episode: verifyRetry, doubled at each failure after the first, up to verifyRetryMax.
func verifyRetryAfter(failures int) time.Duration {
	retry := verifyRetry
	for i := 1; i < failures && retry < verifyRetryMax; i++ {
		retry *= 2
	}
	return min(retry, verifyRetryMax)
}

// permanentVerifyFailure reports whether err is a failure every later read of the head would
// meet again: the App is not installed on the repository (deleted, or left by the App, after its
// webhooks), GitHub answers a read of the repository or the commit 404, or 403 with no rate limit
// behind it (githubapp.ErrRepositoryUnreadable, which githubapp.CheckRepositoryResponse makes of
// both, so no *githubapp.StatusError carries them), or 410, a page past githubapp's response
// limit, a listing that does not end within maxPages, or the bound's refusal of the answer's own
// write (bus.ErrTooLarge from MarkVerified, where the stamp alone would take the record past its
// bound, and the store writes it marked Overflowed). A write of what GitHub answered past the
// record's bound never reaches here: record takes it as written (refused), the store writes the
// record marked Overflowed, and what ends that episode is the listener letting its claim go on an
// Overflowed record (cistore's checkVerify).
func permanentVerifyFailure(err error) bool {
	var tooLarge *githubapp.ResponseTooLargeError
	var status *githubapp.StatusError
	switch {
	case errors.Is(err, githubapp.ErrNoInstallation), errors.Is(err, githubapp.ErrRepositoryUnreadable), errors.Is(err, errTooManyPages), errors.Is(err, bus.ErrTooLarge), errors.As(err, &tooLarge):
		return true
	case errors.As(err, &status):
		return status.Status == http.StatusNotFound || status.Status == http.StatusGone
	}
	return false
}

// runningSuite names a check suite of pages that has not completed and has runs in it
// (latest_check_runs_count above zero), and answers "" when there is none. A suite another App
// leaves queued with no run of its own holds nothing, as it holds nothing in the walk.
func runningSuite(pages []pageOf[object]) string {
	for _, page := range pages {
		for _, raw := range page.items {
			if status, _ := raw["status"].(string); status == "" || status == "completed" {
				continue
			}
			if count, ok := jsonCount(raw["latest_check_runs_count"]); ok && count > 0 {
				return fmt.Sprint(raw["id"])
			}
		}
	}
	return ""
}

// jsonCount is a JSON number as a decoded object carries it.
func jsonCount(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
