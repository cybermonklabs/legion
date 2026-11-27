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
// timeout answers up to githubPageAttempts times. It returns the last attempt's answer as
// githubapp.Client.Read returns it, so a caller's own status handling (a 404 that means "gone")
// is unchanged. The one exception is a rate-limited answer: it comes back as the
// *githubapp.RateLimitError that githubapp.RateLimit reads from it, which each caller's check
// of the answer gave before. It also answers how many times it asked GitHub (attempts), which an
// error names through pageAttemptsNote. A rate limit or a 404 ends the asking at the answer that
// gives it, so no attempt follows it.
//
// That error names its Scope: the installation that token spends (githubapp.Client.TokenScope).
// A limit that the client held before it sent the request names its scope already, and a limit of
// a GraphQL query does too (githubapp.Client.GraphQL). GitHub's REST answer does not say which
// installation it is on, so without this a REST limit has no scope. Then the head pass's line
// names no scope where its first limit is a REST answer (reconcilePullRequestHeads).
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
func readGitHubPage(ctx context.Context, client *githubapp.Client, token, path string) (body []byte, status int, header http.Header, attempts int, err error) {
	for attempts = 1; ; attempts++ {
		asked, held := githubreads.Hold(ctx)
		body, status, header, err = client.Read(asked, token, path)
		if retryGitHubPage(ctx, attempts, status, err) && waitBeforeRetry(ctx, attempts) == nil {
			held.Drop()
			continue
		}
		held.Keep()
		if err == nil {
			if limited := githubapp.RateLimit(status, header, body); limited != nil {
				limited.Scope = client.TokenScope(token)
				return body, status, header, attempts, limited
			}
		}
		return body, status, header, attempts, err
	}
}

// queryGitHubPage is readGitHubPage for one GraphQL search page: the same bounded retry, the same
// count of attempts, and each try held the same way. A GraphQL query is a read however GitHub
// spells it, so retrying one repeats nothing.
func queryGitHubPage(ctx context.Context, client *githubapp.Client, token, query string, variables map[string]any) (body []byte, status int, header http.Header, attempts int, err error) {
	for attempts = 1; ; attempts++ {
		asked, held := githubreads.Hold(ctx)
		body, status, header, err = client.GraphQL(asked, token, query, variables)
		if retryGitHubPage(ctx, attempts, status, err) && waitBeforeRetry(ctx, attempts) == nil {
			held.Drop()
			continue
		}
		held.Keep()
		return body, status, header, attempts, err
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

// pageAttemptsNote names in an error how many times a page was asked for, where it was asked more
// than once: " (after 3 attempts)" for a page that used the whole retry budget, " (after 2
// attempts)" for one whose second answer ended it. A page asked once gets no note, so an error
// never claims a retry that no request made: a rate limit on the first answer is one attempt.
func pageAttemptsNote(attempts int) string {
	if attempts <= 1 {
		return ""
	}
	return fmt.Sprintf(" (after %d attempts)", attempts)
}
