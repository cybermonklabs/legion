package githubrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestARateLimitsBudgetIsAResourceGitHubDocumentsOrTheCoreBudget: GitHub, or whatever answers in
// its place, refuses a read with its primary rate limit - no call left until a reset, and no
// Retry-After - and names in X-RateLimit-Resource the request's Authorization header, echoed back
// verbatim (echo), which is no resource GitHub documents. The Answer's Budget, which a caller
// decides on and prints (the daemon's limitAttributes), is the REST API's core budget, and the
// Answer's rate-limit headers hold the echo as the rule reads it (oauth2call.Header): what the
// Answer prints of that header holds no run of the token. A resource GitHub documents, search, is
// the Budget as it came (documented). The Budget was the header as it came, the token and all.
// Captures githubrest.answer-words/budget-<case>.
func TestARateLimitsBudgetIsAResourceGitHubDocumentsOrTheCoreBudget(t *testing.T) {
	const token = "not-a-secret-token"
	reset := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name string
		// resource is what the stand-in writes as X-RateLimit-Resource, given the request.
		resource func(r *http.Request) string
		budget   Budget
	}{
		{"echo", func(r *http.Request) string { return r.Header.Get("Authorization") }, BudgetCore},
		{"documented", func(*http.Request) string { return "search" }, Budget("search")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked.Store(r.Header.Get("Authorization"))
				w.Header().Set("X-RateLimit-Limit", "5000")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", reset)
				w.Header().Set("X-RateLimit-Resource", tc.resource(r))
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation ID 7."}`))
			}))
			defer server.Close()

			err := Client{Token: token, API: server.URL + "/repos/acme/widgets"}.Get(context.Background(), "", "", nil)
			var answer *Answer
			if !errors.As(err, &answer) || !answer.RateLimited {
				t.Fatalf("the read = %v, want GitHub's rate limit", err)
			}
			if got := asked.Load(); got != "Bearer "+token {
				t.Fatalf("the stand-in was asked with Authorization %v, want the client's token", got)
			}
			printed := "budget " + string(answer.Budget) + ", resource " + answer.Limit.Resource
			oauth2calltest.RequireNone(t, "githubrest.answer-words/budget-"+tc.name, printed)
			if answer.Budget != tc.budget {
				t.Errorf("the limit spent the budget %q, want %q", answer.Budget, tc.budget)
			}
		})
	}
}
