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

// A primary GraphQL limit is recorded under a scope of its own, so it holds no REST read:
// GitHub budgets an installation's REST calls apart from the App's GraphQL points, and every
// other caller in the process -- the delivery reconcile, a person's click, the pass's own
// identity reads -- goes on reading while the pass's queries wait the limit out. GitHub answers
// that limit at either status, and the status is never what tells one.
func TestAPrimaryGraphQLRateLimitLeavesRESTReadsAlone(t *testing.T) {
	for name, limit := range map[string]graphQLLimit{
		"at 200": limitByErrorEntry,
		"at 403": limitForbiddenByErrorEntry,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			h.mapRepository(rowRepository)
			h.listHead()
			other := h.replica()

			h.pass()
			other.Pass(h.ctx)
			h.github.rateLimitQuery(limit)

			h.pass()

			line := h.passLine()
			if line["rate_limit_scope"] != githubapp.GraphQLScope {
				t.Fatalf("the pass names scope %v, want %q, GraphQL's own: %v", line["rate_limit_scope"], githubapp.GraphQLScope, line)
			}
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
		})
	}
}

// A secondary limit is the other half of that rule. GitHub shares its secondary limits between
// REST and GraphQL -- too many requests a minute is too many of either -- so a query that meets
// one is recorded under the installation whose token met it, where it holds that installation's
// REST reads too: another caller reading on inside it is the thing GitHub warns may get an
// integration banned. The pass stops at it as it stops at any limit, and reads no head of the
// page it was refused over REST.
//
// The three shapes are the three GitHub answers a secondary limit with: the wording alone at
// either status, and the wording with a retry-after of its own.
func TestASecondaryQueryLimitHoldsTheInstallationsRESTReads(t *testing.T) {
	for name, limit := range map[string]graphQLLimit{
		"the wording alone":         limitBySecondaryMessage,
		"a retry-after beside it":   limitBySecondaryRetryAfter,
		"the wording alone, at 403": limitForbiddenBySecondaryMessage,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			h.mapRepository(rowRepository)
			h.listHead()
			other := h.replica()

			// Both processes mint their token first: a mint is the App's call, and this test is
			// about the installation's own budget.
			h.pass()
			other.Pass(h.ctx)
			h.github.rateLimitQuery(limit)

			h.pass()

			line := h.passLine()
			if line["rate_limited"] != true {
				t.Fatalf("the pass reports rate_limited = %v, want true: GitHub answered its query a secondary rate limit: %v",
					line["rate_limited"], line)
			}
			installation := githubapp.InstallationScope(h.github.installationID())
			if line["rate_limit_scope"] != installation {
				t.Fatalf("the pass names scope %v, want %q: GitHub shares a secondary limit between the installation's REST calls and its queries: %v",
					line["rate_limit_scope"], installation, line)
			}
			if _, found, err := h.reconciler.Limits.Get(h.ctx, installation); err != nil || !found {
				t.Fatalf("the secondary limit was recorded under %s: %v, %v", installation, found, err)
			}
			if got := number(t, line, "heads"); got != 0 {
				t.Errorf("the pass read %d heads under a secondary rate limit: %v", got, line)
			}

			// The other process's REST read under that installation is refused without a
			// request: the limit the query met is the installation's.
			token, err := other.GitHub.RepositoryToken(h.ctx, "acme", "widgets")
			if err != nil {
				t.Fatalf("the other process's token for %s: %v", rowRepository, err)
			}
			sent := h.github.count()
			_, _, _, err = other.GitHub.Read(h.ctx, token, "/repos/"+rowRepository)
			held, limited := githubapp.AsRateLimit(err)
			if !limited || !held.Held {
				t.Fatalf("the other process's REST read inside a secondary limit: %v; want the recorded limit, held without a request", err)
			}
			if got := h.github.count(); got != sent {
				t.Fatalf("GitHub was sent %d requests inside the secondary limit, want none: %+v", got-sent, h.github.received())
			}
		})
	}
}

// A repository's own data is never a rate limit. GitHub's answer to a query carries the names
// people gave their check runs, and one of them naming GitHub's secondary limit -- a check that
// watches for one, a job named after the incident it was written for -- is a good page of a
// good repository. A pass that read it as a limit would stop at that page at every pass, leave
// its cursor where it was, and never reconcile a head behind it, while the App's queries waited
// out a minute it was never asked for. The limit is read from GitHub's own envelope and headers
// (githubapp.GraphQLRateLimit).
func TestACheckRunNamedForTheLimitIsNotALimit(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, secondaryLimitMessage, "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)

	h.pass()

	line := h.passLine()
	if line["rate_limited"] != false {
		t.Fatalf("the pass reports rate_limited = %v, want false: GitHub answered its query a page of a repository: %v", line["rate_limited"], line)
	}
	if got := number(t, line, "heads"); got != 1 {
		t.Fatalf("the pass read %d heads, want the one the query answered: %v", got, line)
	}
	if _, found, err := h.reconciler.Limits.Get(h.ctx, githubapp.GraphQLScope); err != nil || found {
		t.Errorf("a GraphQL limit was recorded for a good answer: %v, %v", found, err)
	}
	state, _, held := h.record()
	if !held {
		t.Fatal("the head the query answered has no record: its check run is named for a rate limit, and nothing else about it is unusual")
	}
	if _, recorded := state.Checks[secondaryLimitMessage]; !recorded {
		t.Errorf("the record holds %+v, want the run whose name reads like GitHub's limit", state.Checks)
	}
}
