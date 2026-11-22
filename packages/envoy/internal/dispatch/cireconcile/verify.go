package cireconcile

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

const (
	// VerifyInterval is how often the verify loop lists the CI store's verify requests: a head a
	// listener holds with ENVOY_CI_VERIFY on waits about this long, and the head's GitHub read,
	// before its settlement is published.
	VerifyInterval = 5 * time.Second

	// VerifyLease names the lease each verify pass runs under, apart from Lease, so a reconcile
	// pass that takes minutes never holds a verify back. Two processes answering one request would
	// both be safe -- every write is a compare-and-swap of what GitHub holds, and an answer only
	// raises VerifiedRev -- but each would read the head from GitHub.
	VerifyLease = "ci-verify"

	// verifyRetry is how long the verify loop leaves a request it could not answer before it reads
	// that head again: a repository whose token or read fails fails again at once, and every five
	// seconds would spend the App's budget on it. A request written again, under a revision of its
	// own, is read at once.
	verifyRetry = time.Minute
)

// verifyFailure is a request the verify loop could not answer, and when it reads that head again.
type verifyFailure struct {
	revision uint64
	retryAt  time.Time
}

// Verify is one pass of the verify loop (duty.WokenLoop.Pass), and answers when the next is due.
// It lists the verify requests the CI store holds, each at the revision it holds it (V0), and
// answers each in turn: it reads the head from GitHub whole -- every page of its check runs and its
// check suites, and of its commit statuses where the token holds Commit statuses, unconditionally,
// as the walk reads a head over REST -- writes what it read by the walk's own rules (record), and
// then writes V0 as the record's VerifiedRev (cistore.Writer.MarkVerified). V0 is read before the
// GitHub read and is never read again after it, whatever the request's key holds by then, so an
// answer reaches only requests written before the read began. A read that fails, a listing that
// does not end within maxPages, and a write that does not land leave VerifiedRev as it was: only a
// complete answer counts, an empty one included. An answered request is deleted only where the key
// still holds it at V0 (cistore.Writer.DoneVerifying), so a newer request written meanwhile stays
// for the next pass. A limit in force on the App's budget, or on the installation's, ends the pass
// with the requests it has not reached left in place.
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
	listed := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		listed[verifyHeadKey(request.Head)] = struct{}{}
	}
	for key := range r.verifyFailed {
		if _, still := listed[key]; !still {
			delete(r.verifyFailed, key)
		}
	}
	for _, request := range requests {
		if ctx.Err() != nil {
			return next
		}
		if at := r.limited(ctx, githubapp.AppScope); at != nil {
			return next
		}
		if !r.verify(ctx, writer, request) {
			return next
		}
	}
	return next
}

// verifyHeadKey is the key a pass remembers head under (pullHead.key).
func verifyHeadKey(head contracts.CIHead) string {
	return pullHead{owner: head.Owner, repo: head.Repo, number: head.Number, sha: head.SHA}.key()
}

// verify answers one request (Verify), and reports whether the pass goes on to the next: false
// when a rate limit stopped it.
func (r *Reconciler) verify(ctx context.Context, writer Writer, request contracts.CIVerifyRequest) bool {
	head, key := request.Head, verifyHeadKey(request.Head)
	if failed, held := r.verifyFailed[key]; held && failed.revision == request.Revision && time.Now().Before(failed.retryAt) {
		return true
	}
	fail := func(what string, err error) {
		remember(&r.verifyFailed, key, verifyFailure{revision: request.Revision, retryAt: time.Now().Add(verifyRetry)})
		r.Logger.Warn("ci verify did not answer a verify request: "+what,
			"repository", head.Owner+"/"+head.Repo, "pr", head.Number, "sha", head.SHA, "request", request.Revision, "error", err)
	}
	token, statuses, err := r.GitHub.RepositoryTokenWithStatuses(ctx, head.Owner, head.Repo)
	if stopAt(err, githubapp.AppScope) != nil {
		return false
	}
	if err != nil {
		fail("no token for the repository", err)
		return true
	}
	scope := r.GitHub.TokenScope(token)
	if at := r.limited(ctx, scopes(scope)...); at != nil {
		return false
	}
	pull := pullHead{token: token, scope: scope, owner: head.Owner, repo: head.Repo, number: head.Number, sha: head.SHA, statuses: statuses}
	// A count of the verify's own: the pass's pace and its line are no part of it, and a count
	// with no pace leaves no gap between requests (pace).
	var c counts
	runs, err := readPages(ctx, r, &c, &c.headRequests, nil, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
		answer, err := r.GitHub.CommitCheckRuns(ctx, token, pull.owner, pull.repo, pull.sha, page, etag)
		return objectPage(answer), err
	})
	if err == nil {
		var suites []pageOf[object]
		suites, err = readPages(ctx, r, &c, &c.headRequests, nil, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
			answer, err := r.GitHub.CommitCheckSuites(ctx, token, pull.owner, pull.repo, pull.sha, page, etag)
			return objectPage(answer), err
		})
		var listedStatuses []pageOf[object]
		if err == nil && statuses {
			listedStatuses, err = readPages(ctx, r, &c, &c.headRequests, nil, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
				answer, err := r.GitHub.CommitStatuses(ctx, token, pull.owner, pull.repo, pull.sha, page, etag)
				return objectPage(answer), err
			})
		}
		if err == nil {
			if _, _, ok := r.record(ctx, writer, pull, runs, suites, listedStatuses); !ok {
				fail("a write of what GitHub answered did not land", nil)
				return true
			}
		}
	}
	if err != nil {
		if stopAt(err, scope) != nil {
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		fail("GitHub's answer for the head was not read whole", err)
		return true
	}
	if _, err := writer.MarkVerified(ctx, head, request.Revision); err != nil {
		fail("the answer was not written", err)
		return true
	}
	delete(r.verifyFailed, key)
	if err := writer.DoneVerifying(ctx, head, request.Revision); err != nil {
		r.Logger.Warn("ci verify did not delete an answered verify request",
			"repository", head.Owner+"/"+head.Repo, "pr", head.Number, "sha", head.SHA, "request", request.Revision, "error", err)
	}
	return true
}
