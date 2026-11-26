//go:build !race

package api

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// What a priority's issue list is held to, from R1 of dispatch://INFRA-5609: 9 of 10 reads within
// 200 ms, read 200 times on one connection after ten reads that warm it. The bound is a time, which
// the race detector stretches, so this file is out of the race build and envoy-go's plain step runs
// it (ci_job_reads.py derives that step's list from this tag).
const (
	priorityListBound    = 200 * time.Millisecond
	priorityListReads    = 200
	priorityListPage     = 50
	priorityListQuantile = 0.9
)

// priorityListRequest is one of the two requests R1 holds: the list with no `limit`, as the
// priorities page and a priority's own page send it, which runs the unpaged statement, and its
// first page of 50, which runs the paged one.
type priorityListRequest struct {
	name  string
	query string
	paged bool
}

var priorityListRequests = []priorityListRequest{
	{name: "the list"},
	{name: "a page of 50", query: fmt.Sprintf("&limit=%d", priorityListPage), paged: true},
}

// TestAPriorityListAnswersWithinItsBoundOnEveryPlan holds GET /api/v1/issues?advances=<the
// largest priority>, as the priorities page and a priority's own page send it with no `limit`,
// and the same request's first page of 50, to 200 ms at the 90th percentile under each of the two
// plans a connection runs a statement with (speedCustomPlans): the generic plan every run after
// the fifth may take, and the plan made for its arguments. Each plan is forced on the connection
// (newForcedPlan), so the plan measured is the one named whatever Postgres would choose, and the
// connection runs with JIT off, as production's Cloud SQL does. The corpus is production's counts
// (seedListCorpus), whose largest priority has 598 open issues.
//
// The reads are timed as the server answers them, sign-in and encoding included, on a server
// whose pool holds one connection, and the connection is held to having run every read under the
// plan named (forcedPlan.explain). The test prints each request's p50 and p90 on each plan, the
// database's time for each read where the server preloads pg_stat_statements, and the plan
// itself, run under EXPLAIN (ANALYZE, BUFFERS).
//
// A plan that cannot meet the bound is not read out to the end: once more reads are over it than
// the 90th percentile allows of 200, the percentile is over it whatever the rest read, and the
// test stops there and names the plan's widest join filter.
func TestAPriorityListAnswersWithinItsBoundOnEveryPlan(t *testing.T) {
	corpus := seedListCorpus(t)
	t.Log(corpus.String())
	for _, plan := range []struct{ name, mode string }{
		{"the generic plan", "force_generic_plan"},
		{"the plan made for its arguments", "force_custom_plan"},
	} {
		t.Run(plan.name, func(t *testing.T) {
			forced := newForcedPlan(t, corpus.database, plan.mode)
			for _, request := range priorityListRequests {
				t.Run(request.name, func(t *testing.T) {
					measurePriorityList(t, corpus, forced, request, plan.name)
				})
			}
		})
	}
}

// measurePriorityList reads one request of the largest priority's list on the forced plan and
// holds it to the bound.
func measurePriorityList(t *testing.T, corpus listCorpus, forced forcedPlan, request priorityListRequest, name string) {
	t.Helper()
	target := "/api/v1/issues?advances=" + corpus.priorities[0] + request.query
	described := request.name + " under " + name

	// What the read answers, before any read is timed: a timing of a wrong answer is worth nothing.
	answer, statement := listingRequest(t, forced.handler, forced.trace, target)
	if request.paged {
		page := decodeBody[model.IssueSummaryPage](t, answer)
		if len(page.Issues) != priorityListPage || page.Total != listCorpusLargest {
			t.Fatalf("%s answers %d issues of %d, want %d of %d", target, len(page.Issues), page.Total, priorityListPage, listCorpusLargest)
		}
	} else if issues := decodeBody[[]model.IssueSummary](t, answer); len(issues) != listCorpusLargest {
		t.Fatalf("%s answers %d issues, want %d", target, len(issues), listCorpusLargest)
	}
	warmed := forced.warm(t, target, time.Now(), time.Hour)

	// Once more reads than this are over the bound, the 90th percentile of 200 is over it too.
	allowedOver := priorityListReads - int(math.Ceil(priorityListQuantile*priorityListReads))
	callsBefore, msBefore, counted := listingStatementTime(t, corpus.database, statement.sql)
	reads := make([]time.Duration, 0, priorityListReads)
	over := 0
	for len(reads) < priorityListReads && over <= allowedOver {
		began := time.Now()
		forced.read(t, target)
		read := time.Since(began)
		reads = append(reads, read)
		if read > priorityListBound {
			over++
		}
	}
	// Where the server preloads pg_stat_statements, it must count every read: a count of none
	// means the reading looked at no statement, and the time it prints would be no time at all.
	database := "not read: the server does not preload pg_stat_statements"
	if callsAfter, msAfter, _ := listingStatementTime(t, corpus.database, statement.sql); counted {
		calls := callsAfter - callsBefore
		if calls != int64(len(reads)) {
			t.Fatalf("pg_stat_statements counted %d calls of the listing for %d reads: its text up to the first constant, %q, names no statement it holds",
				calls, len(reads), statementPrefix(statement.sql))
		}
		database = fmt.Sprintf("%.2f ms a read (pg_stat_statements, %d calls)", (msAfter-msBefore)/float64(calls), calls)
	}

	reading := forced.explain(t, statement, warmed+len(reads))
	t.Logf("EXPLAIN (ANALYZE, BUFFERS) of %s's statement under %s (plan_cache_mode = %s, jit = %s):\n%s", request.name, name, reading.mode, reading.jit, reading.plan)
	sorted := slices.Clone(reads)
	slices.Sort(sorted)
	p50, p90 := nearestRank(sorted, 0.5), nearestRank(sorted, priorityListQuantile)
	t.Logf("%s: %d reads of %s on one connection after %d warm ones (plan_cache_mode = %s, jit = %s; the connection ran the statement under %d generic and %d custom plans): p50 %s, p90 %s; database time %s",
		described, len(reads), target, warmed, reading.mode, reading.jit, reading.generic, reading.custom,
		p50.Round(10*time.Microsecond), p90.Round(10*time.Microsecond), database)
	if p90 > priorityListBound {
		node, removed := reading.widestJoinFilter()
		t.Errorf("%s answers %s at the 90th percentile, over the bound of %s (%d of %d reads over it); its plan's widest join filter is %q, which threw away %.0f rows over its runs",
			described, p90.Round(time.Millisecond), priorityListBound, over, len(reads), node.name(), removed)
	}
}
