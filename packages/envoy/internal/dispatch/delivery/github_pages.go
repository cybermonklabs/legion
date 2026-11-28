package delivery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
)

// githubPageAttempts bounds how many times one list/search page is asked for before the step
// gives up. A reconcile pass of a busy repository makes thousands of page requests and GitHub
// times a slow query out often enough that one of them usually does: three attempts turn a
// single slow page from a failed pass into a page that costs a few more seconds.
const githubPageAttempts = 3

// githubPageRetryWait is how long a retry waits before asking again, multiplied by the attempt
// just made (1 s, then 2 s). A var rather than a const so a test can shrink it; nothing outside
// a test writes it.
//
// One attempt's own deadline is githubapp.RequestTimeout, which githubapp.Client.request applies
// to every call it makes. This file adds none of its own, so a page has one deadline however it
// is read.
var githubPageRetryWait = time.Second

// readGitHubPage performs one GET of a list page, retrying a timeout or one of GitHub's own
// timeout answers up to githubPageAttempts times. Returns the last attempt's answer exactly as
// githubapp.Client.Read would, so a caller's own status handling (a 404 that means "gone", a
// rate limit) is unchanged.
//
// token is the caller's. Every read of more than two requests asks githubapp for a token before
// each page, never once for the whole read (ListWorkflowRuns, ListWorkflowRunJobs, the merged-PR
// search, fetchCommitMessagesWithToken): githubapp.Client.Token hands back a cached token until
// five minutes (tokenExpirySlack) before it expires, and one page can take githubPageAttempts
// attempts of githubapp.RequestTimeout (20 s) and the waits between them, about 63 s, so five slow
// pages can outlast what a cached token has left; a walk whose visit reconciles between its pages
// can outlast a fresh token's hour too. An ask the cache answers costs no GitHub call. A read of
// one or two requests (FetchWorkflowRun, FetchPullRequest) takes one token, which lasts it: two
// requests take at most about two minutes.
//
// Each try runs in a held scope of the pass's attempt (githubreads.Hold): the failures the client
// notes on a try asked again are dropped, since the page's outcome is the try it returns, and that
// try's notes are kept. A page GitHub answers 502 once and then reads is no failure of the pass,
// and one it answers 502 every try is the last try's 502.
func readGitHubPage(ctx context.Context, client *githubapp.Client, token, path string) ([]byte, int, http.Header, error) {
	for attempt := 1; ; attempt++ {
		asked, held := githubreads.Hold(ctx)
		body, status, header, err := client.Read(asked, token, path)
		if retryGitHubPage(ctx, attempt, status, err) && waitBeforeRetry(ctx, attempt) == nil {
			held.Drop()
			continue
		}
		held.Keep()
		return body, status, header, err
	}
}

// queryGitHubPage is readGitHubPage for one GraphQL search page: the same bounded retry, each try
// held the same way. A GraphQL query is a read however GitHub spells it, so retrying one repeats
// nothing.
func queryGitHubPage(ctx context.Context, client *githubapp.Client, token, query string, variables map[string]any) ([]byte, int, http.Header, error) {
	for attempt := 1; ; attempt++ {
		asked, held := githubreads.Hold(ctx)
		body, status, header, err := client.GraphQL(asked, token, query, variables)
		if retryGitHubPage(ctx, attempt, status, err) && waitBeforeRetry(ctx, attempt) == nil {
			held.Drop()
			continue
		}
		held.Keep()
		return body, status, header, err
	}
}

// retryGitHubPage reports whether an attempt's outcome is worth asking again for: this attempt's
// own deadline passing while the caller's context is still alive, any other network timeout, or
// one of GitHub's own gateway-timeout answers (it reports a query it gave up on as a 502 or a
// 504). Everything else -- a 404, a rate limit, a decode failure, a response past the size cap,
// the caller's own context ending -- is the answer, not a reason to ask again.
//
// The other outage statuses (a 408, a 500, a 503: httpoutage.Is) go back to the caller,
// whose retry is the next reconcile pass: a step that fails a page fails its window, and the next
// pass, at most ReconcileInterval later, asks for it again from the last window it recorded; an
// intake handler logs the failed fetch and acknowledges it, leaving the event to that pass. Asking
// again within seconds helps a query GitHub gave up on under load. A 500 or a 503 says GitHub is
// failing, which seconds rarely mend and the next pass meets minutes later, and each try would
// hold an intake handler 1 s and then 2 s more (githubPageRetryWait). A 429 is a limit whose wait
// the caller keeps (githubapp.RateLimit).
func retryGitHubPage(ctx context.Context, attempt, status int, err error) bool {
	if attempt >= githubPageAttempts {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return true
		}
		var netErr net.Error
		return errors.As(err, &netErr) && netErr.Timeout()
	}
	return status == http.StatusBadGateway || status == http.StatusGatewayTimeout
}

// waitBeforeRetry sleeps between attempts, or returns the caller's context error when it ends
// first.
func waitBeforeRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt) * githubPageRetryWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// pageAttemptsNote names the retry budget in an error, so a failure that exhausted it reads as
// "GitHub was asked three times" rather than looking like a single unlucky call.
func pageAttemptsNote() string {
	return fmt.Sprintf("after %d attempts", githubPageAttempts)
}
