package githubrest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// secondaryLimitWithItsURL is GitHub's 403 for its secondary rate limit as GitHub writes it: its
// documentation URL sits right before a JSON quote, which the rule cannot read as clean, so the
// rule prints the whole body as not readable.
const secondaryLimitWithItsURL = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits","status":"403"}`

// TestARateLimitIsDecidedOnGitHubsWordsAsTheyCame: GitHub answers a call with its 403 for the
// secondary limit, named by its message alone (thousands of calls left, no Retry-After), in a
// body the rule prints as not readable. The answer is still GitHub's rate limit, on the secondary
// budget, waited a minute: the client decides on the body as it came (oauth2call.Holds), and only
// what it prints is the rule's reading, which names no URL of the body.
func TestARateLimitIsDecidedOnGitHubsWordsAsTheyCame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, secondaryLimitWithItsURL)
	}))
	t.Cleanup(server.Close)
	err := Client{API: server.URL, Token: githubtest.InstallationToken(t.Name())}.Get(context.Background(), "/repos/acme/widgets", "", nil)
	var answer *Answer
	if !errors.As(err, &answer) {
		t.Fatalf("Get = %v, want GitHub's *Answer", err)
	}
	if !answer.RateLimited || answer.Budget != BudgetSecondary || answer.RetryAfter != time.Minute {
		t.Errorf("the answer reads as rate limited %v on budget %q, waited %s; want GitHub's secondary limit, waited a minute", answer.RateLimited, answer.Budget, answer.RetryAfter)
	}
	if strings.Contains(err.Error(), "docs.github.com") {
		t.Errorf("the error is %q, want GitHub's body as the rule prints it, which names none of its URL", err)
	}
}
