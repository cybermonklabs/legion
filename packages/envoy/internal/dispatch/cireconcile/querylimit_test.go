package cireconcile

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// GitHub answers a GraphQL query past the App's budget with HTTP 200 and an `errors` entry of
// type RATE_LIMITED, beside `x-ratelimit-remaining: 0` and the reset: a 200 the client's REST
// rate-limit detection cannot see at all. That answer ends the pass where it stands, is
// recorded in github_rate_limits under the GraphQL scope, and no request of either Dispatch
// process reaches GitHub before the reset. It is k7.
func TestAGraphQLRateLimitStopsEveryProcess(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	other := h.replica()

	// Both processes mint their installation token first, so neither has to mint one while the
	// limit stands: a mint is budgeted under the App, and this test is about the queries the
	// GraphQL limit stops.
	h.pass()
	other.Pass(h.ctx)

	h.github.rateLimitQuery(limitByErrorEntry)

	h.pass()

	line := h.passLine()
	if line["rate_limited"] != true {
		t.Fatalf("the pass reports rate_limited = %v, want true: GitHub answered its query a rate limit: %v", line["rate_limited"], line)
	}
	if line["rate_limit_scope"] != githubapp.GraphQLScope {
		t.Fatalf("the pass names scope %v, want %q, GraphQL's own: %v", line["rate_limit_scope"], githubapp.GraphQLScope, line)
	}
	if got := number(t, line, "heads"); got != 0 {
		t.Errorf("the pass read %d heads under a rate-limited query: %v", got, line)
	}
	if got := number(t, line, "mappings_skipped"); got != 0 {
		t.Errorf("the pass skipped %d mappings; a rate limit stops the pass, it does not skip a repository: %v", got, line)
	}

	limit, found, err := h.reconciler.Limits.Get(h.ctx, githubapp.GraphQLScope)
	if err != nil {
		t.Fatalf("read the shared GitHub rate limits: %v", err)
	}
	if !found {
		t.Fatalf("github_rate_limits holds no limit under %s", githubapp.GraphQLScope)
	}
	if limit.Wait <= 0 {
		t.Fatalf("the limit under %s has %s left, want GitHub's own reset still in force", githubapp.GraphQLScope, limit.Wait)
	}

	// The stand-in answers as it did before; nothing may reach it from either process while the
	// limit it recorded stands.
	h.github.forgetRequests()

	h.pass()
	other.Pass(h.ctx)

	if sent := h.github.count(); sent != 0 {
		t.Fatalf("the two processes sent GitHub %d requests inside a GraphQL rate limit: %+v", sent, h.github.received())
	}
}

// The GraphQL limit is recorded under a scope of its own, so it holds no REST read: GitHub
// budgets an installation's REST calls apart from the App's GraphQL points, and every other
// caller in the process -- the delivery reconcile, a person's click, the pass's own identity
// reads -- goes on reading while the pass's queries wait the limit out.
func TestAGraphQLRateLimitLeavesRESTReadsAlone(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	other := h.replica()

	h.pass()
	other.Pass(h.ctx)
	h.github.rateLimitQuery(limitByErrorEntry)

	h.pass()

	if _, found, err := h.reconciler.Limits.Get(h.ctx, githubapp.GraphQLScope); err != nil || !found {
		t.Fatalf("the GraphQL limit was recorded under %s: %v, %v", githubapp.GraphQLScope, found, err)
	}
	installation := githubapp.InstallationScope(h.github.installationID())
	if _, found, err := h.reconciler.Limits.Get(h.ctx, installation); err != nil || found {
		t.Fatalf("a limit was recorded under %s as well: %v, %v; the GraphQL budget is not the installation's REST budget",
			installation, found, err)
	}

	// Another caller's REST read, made with the other process's client under the same
	// installation, still reaches GitHub and is answered.
	// The row's own name, which is the name the stand-in's installation lookup answers for.
	token, err := other.GitHub.RepositoryToken(h.ctx, "acme", "widgets")
	if err != nil {
		t.Fatalf("the other process's token for %s: %v", rowRepository, err)
	}
	body, status, _, err := other.GitHub.Read(h.ctx, token, "/repos/"+rowRepository)
	if err != nil {
		t.Fatalf("the other process's REST read inside a GraphQL limit: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("the other process's REST read answered %d: %s; want GitHub's own answer", status, body)
	}
}

// A secondary GraphQL limit at its barest -- HTTP 200, no errors type, no rate-limit header,
// and GitHub's wording about a secondary rate limit in the body alone -- is read as the limit
// it is and stops the pass.
func TestASecondaryLimitMessageAloneStopsThePass(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.rateLimitQuery(limitBySecondaryMessage)

	h.pass()

	line := h.passLine()
	if line["rate_limited"] != true {
		t.Fatalf("the pass reports rate_limited = %v, want true: the body says GitHub refused it for a secondary rate limit: %v",
			line["rate_limited"], line)
	}
	if line["rate_limit_scope"] != githubapp.GraphQLScope {
		t.Fatalf("the pass names scope %v, want %q: %v", line["rate_limit_scope"], githubapp.GraphQLScope, line)
	}
	if _, found, err := h.reconciler.Limits.Get(h.ctx, githubapp.GraphQLScope); err != nil || !found {
		t.Fatalf("the secondary limit was recorded under %s: %v, %v", githubapp.GraphQLScope, found, err)
	}
	if got := number(t, line, "heads"); got != 0 {
		t.Errorf("the pass read %d heads under a secondary rate limit: %v", got, line)
	}
}
