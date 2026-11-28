package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// What R3 of dispatch://INFRA-5609 holds a listing request to: no more than twice the shared
// buffers on the generic plan than on the plan made for its arguments. planStableWarmBudget bounds
// the warm reads (forcedPlan.warm): a read that takes longer than this is already the fault.
const (
	planStableRatio      = 2.0
	planStableWarmBudget = time.Minute
)

// TestNoListingRequestReadsMoreOnTheGenericPlan holds the listing requests a page sends, beside
// the two TestAPriorityListAnswersWithinItsBoundOnEveryPlan times, to R3: on the generic plan,
// whose one-issue estimate made the listing's joins loop (issueListFilters), a request reads at
// most twice the shared buffers it reads on the plan made for its arguments. Buffers are what the
// plan reads, not how fast the machine reads them, so the bound holds on any box and under the
// race detector.
//
// Two of the requests are on the seed of cmd/dispatch-seed --scale 1 (ten projects, the largest of
// 8,000 issues) that the database DISPATCH_BENCH_DATABASE_URL names holds: the seed takes about an
// hour to write, so the test reads one a build Job wrote rather than writing its own, as
// TestStatementTimings does. CI leaves the variable unset, so there the test reads the third
// request alone and says so in its first line; c-plan-stable's run refuses to start without the
// variable. The two are the largest project's list as the project list sends it
// (`?project=<p>&open=true`), and the same list with moves and no `open`, an API caller's shape
// that reads the components of every issue of the project. The third is the priorities page's
// list of work in backlog or icebox on the corpus of production's counts (seedListCorpus). The
// test writes nothing to the seeded database but what a test server writes when it starts: its two
// people.
//
// Each request goes through a server whose pool holds one connection under the plan named
// (newForcedPlan), warmed (forcedPlan.warm, within planStableWarmBudget), and then the statement it
// sent, with the arguments it sent, runs under EXPLAIN (ANALYZE, BUFFERS) on that connection. The
// test prints each request's shared buffers and time on both plans and their ratio, and on a
// ratio above 2 the three nodes of the generic plan that ran the most times.
func TestNoListingRequestReadsMoreOnTheGenericPlan(t *testing.T) {
	t.Parallel()
	corpus := seedListCorpus(t)
	type planStableRequest struct {
		name     string
		database *store.Store
		target   string
	}
	requests := []planStableRequest{}
	seed := "not read: " + benchtime.DatabaseURLEnv + " is unset, as CI leaves it, so the two --scale 1 requests are not read"
	if benchURL := os.Getenv(benchtime.DatabaseURLEnv); benchURL != "" {
		bench, err := store.Open(context.Background(), benchURL)
		if err != nil {
			t.Fatalf("open %s: %v", benchtime.DatabaseURLEnv, err)
		}
		t.Cleanup(bench.Pool.Close)
		var project string
		var issues, open int
		if err := bench.Pool.QueryRow(context.Background(), `
			select project_key, count(*), count(*) filter (where closed_at is null)
			from issues group by project_key order by count(*) desc, project_key limit 1`).Scan(&project, &issues, &open); err != nil {
			t.Fatalf("read the seed's largest project: %v", err)
		}
		seed = fmt.Sprintf("the largest project of %s, %s, holds %d issues, %d open", benchtime.DatabaseURLEnv, project, issues, open)
		requests = append(requests,
			planStableRequest{"the largest project's list", bench, "/api/v1/issues?project=" + project + "&open=true"},
			planStableRequest{"the largest project's list with moves", bench, "/api/v1/issues?project=" + project + "&moved=true"})
	}
	t.Logf("%s; seed: %s", corpus.String(), seed)
	requests = append(requests, planStableRequest{"the priorities page's list of work in backlog or icebox", corpus.database,
		"/api/v1/issues?advances=any&status=backlog&status=icebox&moved=true&open=true"})

	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			custom := readPlanStable(t, request.database, request.target, "force_custom_plan")
			generic := readPlanStable(t, request.database, request.target, "force_generic_plan")
			ratio := generic.buffers() / custom.buffers()
			t.Logf("%s (%s): %.0f shared buffers in %.1f ms on the generic plan, %.0f in %.1f ms on the plan made for its arguments: %.2fx, at most %.2fx (jit = %s)",
				request.name, request.target, generic.buffers(), generic.milliseconds, custom.buffers(), custom.milliseconds, ratio, planStableRatio, generic.jit)
			if ratio > planStableRatio {
				t.Errorf("%s reads %.0f shared buffers on the generic plan against %.0f on the plan made for its arguments, %.2fx, above %.2fx; the generic plan's most-run nodes are %s",
					request.target, generic.buffers(), custom.buffers(), ratio, planStableRatio, generic.busiest(3))
			}
		})
	}
}

// readPlanStable sends target through a forced plan of mode over database, warms it, and reads the
// listing statement the first read sent, with its arguments, on its connection.
func readPlanStable(t *testing.T, database *store.Store, target, mode string) forcedPlanReading {
	t.Helper()
	forced := newForcedPlan(t, database, mode)
	began := time.Now()
	_, statement := listingRequest(t, forced.handler, forced.trace, target)
	return forced.explain(t, statement, forced.warm(t, target, began, planStableWarmBudget))
}
