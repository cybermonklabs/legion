package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The harness the listing's tests of dispatch://INFRA-5609 share: the request that reads back
// the listing statement it sent, a server whose one connection runs every statement under a
// forced plan, and that statement's plan and counts on that connection. Its three readers are
// TestAPriorityListAnswersWithinItsBoundOnEveryPlan, TestAPriorityListAnswersAsTheMergeBaseAnswers
// and TestNoListingRequestReadsMoreOnTheGenericPlan.

// listingStatements are the four statement texts listIssues sends (issueSummaryQuery).
var listingStatements = []string{listIssuesQuery, listPinnedIssuesQuery, listIssuesPageQuery, listPinnedIssuesPageQuery}

// listingRequest makes one GET of target as alice on a context trace counts, holds it to 200 OK,
// and answers the response and the one listing statement the server sent for it, with the
// arguments it sent: a request that sent none, or more than one, stops the test.
func listingRequest(t *testing.T, handler http.Handler, trace *speedTrace, target string) (*httptest.ResponseRecorder, speedStatement) {
	t.Helper()
	trace.take()
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), countedRead{}, true), http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%.300s", target, response.Code, response.Body.String())
	}
	sent, _ := trace.take()
	listings := slices.DeleteFunc(sent, func(statement speedStatement) bool { return !slices.Contains(listingStatements, statement.sql) })
	if len(listings) != 1 {
		t.Fatalf("%s sent %d listing statements of the %d statements the trace saw, want one", target, len(listings), len(sent))
	}
	return response, listings[0]
}

// planCacheStore opens a pool of at most connections on database whose every connection runs
// under plan_cache_mode = mode with JIT off, as production's Cloud SQL runs, closed when t ends.
func planCacheStore(t *testing.T, database *store.Store, mode string, connections int32) *store.Store {
	t.Helper()
	config := database.Pool.Config()
	config.MaxConns, config.MinConns = connections, 0
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
	config.ConnConfig.RuntimeParams["jit"] = "off"
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool under plan_cache_mode = %s: %v", mode, err)
	}
	planned := &store.Store{Pool: store.NewPool(opened)}
	t.Cleanup(planned.Pool.Close)
	return planned
}

// forcedPlan is a server over database whose pool holds one connection, which runs every
// statement under plan_cache_mode = mode: store is the server's own pool, the one connection
// every read of handler runs on, and trace sees what a counted request sends.
type forcedPlan struct {
	mode    string
	handler http.Handler
	store   *store.Store
	trace   *speedTrace
}

func newForcedPlan(t *testing.T, database *store.Store, mode string) forcedPlan {
	t.Helper()
	trace := &speedTrace{}
	handler, served, _, _ := newRegisteredTestServer(t, testServerOptions{
		store: planCacheStore(t, database, mode, 1), tracer: trace, identity: headerIdentity(database),
	})
	return forcedPlan{mode: mode, handler: handler, store: served, trace: trace}
}

// read is one GET of target as alice, held to 200 OK.
func (p forcedPlan) read(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	response := dispatchRequest(t, p.handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s under %s: status=%d body=%.300s", target, p.mode, response.Code, response.Body.String())
	}
	return response
}

// forcedPlanReading is one statement read under EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) on the
// forced plan's connection: the plan's top node, its execution time and its text, the
// connection's jit and plan_cache_mode, and how many runs of the statement the connection has
// planned of each kind.
type forcedPlanReading struct {
	top             analyzedNode
	milliseconds    float64
	plan            string
	jit, mode       string
	generic, custom int64
}

// explain reads statement's plan on the forced plan's connection, with the arguments it was sent
// with, and stops the test unless the connection ran it at least runs times, every time under the
// plan it forces: a reading of another plan or of another connection is no reading of this one.
func (p forcedPlan) explain(t *testing.T, statement speedStatement, runs int) forcedPlanReading {
	t.Helper()
	ctx := context.Background()
	connection, err := p.store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take the forced plan's connection: %v", err)
	}
	defer connection.Release()
	var reading forcedPlanReading
	if err := connection.QueryRow(ctx, "select current_setting('jit'), current_setting('plan_cache_mode')").Scan(&reading.jit, &reading.mode); err != nil {
		t.Fatalf("read the connection's settings: %v", err)
	}
	if err := connection.QueryRow(ctx, "select generic_plans, custom_plans from pg_prepared_statements where statement = $1",
		statement.sql).Scan(&reading.generic, &reading.custom); err != nil {
		t.Fatalf("read how the connection planned the listing: %v", err)
	}
	ran, other := reading.custom, reading.generic
	if p.mode == "force_generic_plan" {
		ran, other = reading.generic, reading.custom
	}
	if reading.mode != p.mode || ran < int64(runs) || other != 0 {
		t.Fatalf("the connection runs plan_cache_mode = %s and ran the listing under %d generic and %d custom plans, want %s and all of %d runs under the plan it names: the runs were not all on that connection under that plan",
			reading.mode, reading.generic, reading.custom, p.mode, runs)
	}
	if _, err := connection.Exec(ctx, "prepare forced_plan_statement as "+statement.sql); err != nil {
		t.Fatalf("prepare the listing: %v", err)
	}
	defer func() {
		if _, err := connection.Exec(ctx, "deallocate forced_plan_statement"); err != nil {
			t.Errorf("deallocate the listing: %v", err)
		}
	}()
	var raw []byte
	if err := connection.QueryRow(ctx, "explain (analyze, buffers, format json) execute forced_plan_statement"+speedLiterals(t, statement.arguments)).Scan(&raw); err != nil {
		t.Fatalf("explain the listing under %s: %v", p.mode, err)
	}
	var plans []struct {
		Plan          analyzedNode `json:"Plan"`
		ExecutionTime float64      `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("read the listing's plan under %s: %d plans, %v", p.mode, len(plans), err)
	}
	reading.top, reading.milliseconds, reading.plan = plans[0].Plan, plans[0].ExecutionTime, string(raw)
	if reading.buffers() == 0 {
		t.Fatalf("the listing's plan under %s reads no shared buffer: the plan was not read, so nothing was measured", p.mode)
	}
	return reading
}

// buffers is the shared buffers the whole plan accessed, hit and read: the top node's, which
// counts every node below it.
func (r forcedPlanReading) buffers() float64 {
	return r.top.SharedHitBlocks + r.top.SharedReadBlocks
}

// widestJoinFilter is the node that threw away the most rows by its join filter over all its
// runs, and how many, as speedWork counts a node's rows: the loop of a plan that joins every
// issue to a term it reads whole.
func (r forcedPlanReading) widestJoinFilter() (analyzedNode, float64) {
	widest, most := analyzedNode{NodeType: "none"}, 0.0
	for _, node := range r.top.everyNode() {
		if removed := node.Loops * node.RemovedByJoin; removed > most {
			widest, most = node, removed
		}
	}
	return widest, most
}

// busiest names the n nodes of the plan that ran the most times, with how often.
func (r forcedPlanReading) busiest(n int) string {
	nodes := r.top.everyNode()
	slices.SortStableFunc(nodes, func(a, b analyzedNode) int { return cmp.Compare(b.Loops, a.Loops) })
	named := []string{}
	for _, node := range nodes[:min(n, len(nodes))] {
		named = append(named, fmt.Sprintf("%s (%.0f runs)", node.name(), node.Loops))
	}
	return strings.Join(named, ", ")
}

// warmReads is how many times a forced plan's statement runs before it is read: the first
// request, which names the statement, and nine more. The plan is forced, so they warm the caches
// and choose nothing.
const warmReads = 10

// warm makes the reads that follow the first request, up to warmReads in all, stopping once
// budget has passed since began: a read that takes that long is already the fault being read.
func (p forcedPlan) warm(t *testing.T, target string, began time.Time, budget time.Duration) int {
	t.Helper()
	made := 1
	for ; made < warmReads && time.Since(began) < budget; made++ {
		p.read(t, target)
	}
	return made
}

// statementPrefix is statement's text up to its first parameter or constant: a `$` before a digit,
// a quote, or a digit that starts a number rather than continuing a name (`t1`).
func statementPrefix(statement string) string {
	for at := range len(statement) {
		switch c := statement[at]; {
		case c == '\'':
			return statement[:at]
		case c == '$' && at+1 < len(statement) && statement[at+1] >= '0' && statement[at+1] <= '9':
			return statement[:at]
		case c >= '0' && c <= '9' && at > 0:
			before := statement[at-1]
			if before != '_' && !(before >= '0' && before <= '9') &&
				!(before >= 'a' && before <= 'z') && !(before >= 'A' && before <= 'Z') {
				return statement[:at]
			}
		}
	}
	return statement
}

// listingStatementTime is statement's calls and total execution time in database, as
// pg_stat_statements counts them, and false where the server does not preload it. The view holds
// a statement's text with its parameters and constants numbered anew in the order they appear
// (PostgreSQL 18 holds the listing's `any($11::text[])` as `any($1::text[])`), so it is found by
// its text up to its first parameter or constant (statementPrefix). Every listing text starts
// that way, the speed test's among them, so a reading is the difference of two calls around one
// request's reads, which its caller holds to the number of reads it made.
func listingStatementTime(t *testing.T, database *store.Store, statement string) (int64, float64, bool) {
	t.Helper()
	ctx := context.Background()
	var libraries string
	if err := database.Pool.QueryRow(ctx, "select current_setting('shared_preload_libraries')").Scan(&libraries); err != nil {
		t.Fatalf("read shared_preload_libraries: %v", err)
	}
	if !strings.Contains(libraries, "pg_stat_statements") {
		return 0, 0, false
	}
	if _, err := database.Pool.Exec(ctx, "create extension if not exists pg_stat_statements"); err != nil {
		t.Fatalf("create pg_stat_statements, which the server preloads: %v", err)
	}
	prefix := statementPrefix(statement)
	var calls int64
	var milliseconds float64
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(sum(calls), 0)::bigint, coalesce(sum(total_exec_time), 0)::float8
		from pg_stat_statements
		where dbid = (select oid from pg_database where datname = current_database()) and starts_with(query, $1)`,
		prefix).Scan(&calls, &milliseconds); err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	return calls, milliseconds, true
}
