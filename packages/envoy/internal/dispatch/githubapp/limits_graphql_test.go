package githubapp

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
)

// A primary GraphQL limit belongs to one installation. GitHub gives each installation a GraphQL
// budget of its own, so a query of installation 7 that spends its budget holds the queries of
// installation 7, and no query of installation 9. Process A meets the limit, and process B obeys
// the ledger the two share: B's query of installation 9 goes to GitHub, and B's query of
// installation 7 is held with no request. The limit is recorded under installation 7's GraphQL
// scope, the text "graphql:installation:7" that every process and release reads, and under no
// scope of the whole App.
func TestAGraphQLLimitOfOneInstallationHoldsNoOther(t *testing.T) {
	fake := startLimitGitHub(t)
	database, a, b := twoProcesses(t, fake)
	ctx := context.Background()
	spender, err := a.Token(ctx, 7)
	if err != nil {
		t.Fatalf("A's token of installation 7: %v", err)
	}
	other, err := b.Token(ctx, 9)
	if err != nil {
		t.Fatalf("B's token of installation 9: %v", err)
	}
	sameInstallation, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token of installation 7: %v", err)
	}
	const query = "query { viewer { login } }"

	reset := strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10)
	fake.answerGraphQL(graphQLAnswer{
		status: http.StatusOK,
		header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
		body:   `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation"}]}`,
	})
	_, _, _, err = a.GraphQL(ctx, spender, query, nil)
	limited, ok := AsRateLimit(err)
	if !ok || limited.Held {
		t.Fatalf("installation 7's query that spends its budget: err = %v, want GitHub's own answer as a *RateLimitError", err)
	}

	fake.answerGraphQL(graphQLAnswer{status: http.StatusOK, body: `{"data":{"viewer":{"login":"dispatch-test"}}}`})
	_, status, _, err := b.GraphQL(ctx, other, query, nil)
	if err != nil || status != http.StatusOK {
		scope := ""
		if held, ok := AsRateLimit(err); ok {
			scope = held.Scope
		}
		t.Fatalf("installation 9's query after installation 7 spent its GraphQL budget: status %d, err = %v (scope %q); want GitHub's answer: the budget was installation 7's",
			status, err, scope)
	}
	if got := fake.count("POST /graphql", "Bearer "+other); got != 1 {
		t.Fatalf("GitHub got installation 9's query %d times, want once", got)
	}

	_, _, _, err = b.GraphQL(ctx, sameInstallation, query, nil)
	requireHeld(t, err, "B's query of installation 7 inside the GraphQL limit A met")
	if got := fake.count("Bearer " + sameInstallation); got != 0 {
		t.Fatalf("GitHub got %d requests under B's token of installation 7 inside its GraphQL limit, want none", got)
	}

	const want = "graphql:installation:7"
	if limited.Scope != want {
		t.Errorf("the limit names scope %q, want %q", limited.Scope, want)
	}
	limits := ratelimit.New(database.Pool)
	if recorded, found, err := limits.Get(ctx, want); err != nil || !found || recorded.Wait <= 0 {
		t.Errorf("github_rate_limits under %s: %+v, found %v, %v; want GitHub's own wait still in force", want, recorded, found, err)
	}
	if _, found, err := limits.Get(ctx, GraphQLScope); err != nil || found {
		t.Errorf("a limit of installation 7 was recorded under %s, the scope of a token this client did not mint: %v, %v", GraphQLScope, found, err)
	}
}
