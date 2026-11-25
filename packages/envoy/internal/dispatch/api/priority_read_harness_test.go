//go:build !race

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The harness the two readings of the priorities read share (dispatch://INFRA-5791, R1 and R3):
// a server whose pool holds one connection, which runs every statement under a forced plan with
// JIT off, the statements one request sent, each statement's plan on that connection under
// EXPLAIN (ANALYZE, BUFFERS), the plans the connection ran each statement under
// (pg_prepared_statements), and the database's own time for each (pg_stat_statements). Its
// readers are TestThePrioritiesReadAnswersWithinItsBoundOnEveryPlan and
// TestThePrioritiesReadKeepsItsPlansWithAProjectNamed.

// prioritiesReadForm is one of the read's two forms: the full read, which the priorities page,
// a priority's own page and the issue page send, and the summary, which the sidebar sends.
// name is how a reading line names it, called how a sentence does.
type prioritiesReadForm struct {
	name, called string
	summary      bool
}

var prioritiesReadForms = []prioritiesReadForm{
	{name: "full", called: "the full read"},
	{name: "summary", called: "the summary", summary: true},
}

// target is the form's request with projects named, as a client sends it.
func (f prioritiesReadForm) target(projects []string) string {
	values := url.Values{}
	for _, project := range projects {
		values.Add("project", project)
	}
	if f.summary {
		values.Set("summary", "true")
	}
	if encoded := values.Encode(); encoded != "" {
		return "/api/v1/priorities?" + encoded
	}
	return "/api/v1/priorities"
}

// prioritiesReadPlan is one of the two plans a connection runs a prepared statement under: the
// generic plan Postgres may keep from a statement's sixth run, and the plan made for the values
// of each run. mode is the plan_cache_mode that forces it.
type prioritiesReadPlan struct {
	name, mode, called string
}

var prioritiesReadPlans = []prioritiesReadPlan{
	{name: "generic", mode: "force_generic_plan", called: "the generic plan"},
	{name: "custom", mode: "force_custom_plan", called: "the plan made for its values"},
}

// prioritiesReadWarmReads is how many times a request is read on a connection before it is
// measured: the first, which names its statements, and nine more. The plan is forced, so they
// warm the caches and choose nothing.
const prioritiesReadWarmReads = 10

// prioritiesReadServer is a server over the corpus whose pool holds one connection, which runs
// every statement under plan: store is that pool, which every read of handler runs on, and trace
// sees what a counted request sends. The connection also runs with
// pg_stat_statements.track_planning on, so the database's time of a read counts the planning a
// plan made for its values pays on every run.
type prioritiesReadServer struct {
	plan    prioritiesReadPlan
	handler http.Handler
	store   *store.Store
	trace   *speedTrace
}

// newPrioritiesReadServer opens that server over database, whose connections run with JIT off
// (withJITOff), and stops the test unless its connection does and runs plan's mode.
func newPrioritiesReadServer(t *testing.T, database *store.Store, plan prioritiesReadPlan) prioritiesReadServer {
	t.Helper()
	held := storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
		// One connection for the whole reading: a pool that closed it for its age or its idling
		// would open another with an empty plan cache, under which requirePlansRun would fail.
		config.MaxConnLifetime, config.MaxConnIdleTime = 24*time.Hour, 24*time.Hour
		config.ConnConfig.RuntimeParams["plan_cache_mode"] = plan.mode
		config.ConnConfig.RuntimeParams["pg_stat_statements.track_planning"] = "on"
	})
	trace := &speedTrace{}
	handler, served, _, _ := newRegisteredTestServer(t, testServerOptions{store: held, tracer: trace, identity: headerIdentity(database)})
	requireReadCorpusJITOff(t, served, "the one connection the reads under "+plan.called+" run on")
	var mode string
	if err := served.Pool.QueryRow(context.Background(), "show plan_cache_mode").Scan(&mode); err != nil {
		t.Fatalf("show plan_cache_mode on the connection of %s: %v", plan.called, err)
	}
	if mode != plan.mode {
		t.Fatalf("the connection of %s runs plan_cache_mode = %s, want %s", plan.called, mode, plan.mode)
	}
	return prioritiesReadServer{plan: plan, handler: handler, store: served, trace: trace}
}

// counted makes one GET of target as alice on a context the trace counts, holds it to 200 OK,
// and answers the response, every statement the server sent for it, in the order it sent them,
// each with the arguments it was sent with, and the round trips it sent them in, as the trace of
// TestPrioritiesReadSpeed counts them (speedTrace).
func (s prioritiesReadServer) counted(t *testing.T, target string) (*httptest.ResponseRecorder, []speedStatement, int) {
	t.Helper()
	s.trace.take()
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), countedRead{}, true), http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s under %s: status=%d body=%.300s", target, s.plan.called, response.Code, response.Body.String())
	}
	sent, roundTrips := s.trace.take()
	if len(sent) == 0 {
		t.Fatalf("read %s under %s: the trace saw no statement, so nothing would be read of it", target, s.plan.called)
	}
	return response, sent, roundTrips
}

// read is one GET of target as alice, held to 200 OK, and the time the server took to answer it,
// from the request's start to the last byte the recorder took.
func (s prioritiesReadServer) read(t *testing.T, target string) time.Duration {
	t.Helper()
	began := time.Now()
	response := dispatchRequest(t, s.handler, http.MethodGet, target, nil, "alice")
	took := time.Since(began)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s under %s: status=%d body=%.300s", target, s.plan.called, response.Code, response.Body.String())
	}
	return took
}

// warm makes the first, counted read of target and the reads after it, prioritiesReadWarmReads
// in all, and answers the first's response, the statements it sent, named
// (prioritiesReadStatements), and the round trips it sent them in.
func (s prioritiesReadServer) warm(t *testing.T, target string) (*httptest.ResponseRecorder, []prioritiesReadStatement, int) {
	t.Helper()
	response, sent, roundTrips := s.counted(t, target)
	for range prioritiesReadWarmReads - 1 {
		s.read(t, target)
	}
	return response, prioritiesReadStatements(sent), roundTrips
}

// prioritiesReadStatement is one statement a read sent, with the name a line gives it.
type prioritiesReadStatement struct {
	name string
	speedStatement
}

// prioritiesReadStatements names each statement a read sent by the package's own text of it, so
// a reading says which statement it is of; a text that is none of these is named by its place.
func prioritiesReadStatements(sent []speedStatement) []prioritiesReadStatement {
	named := make([]prioritiesReadStatement, 0, len(sent))
	for place, statement := range sent {
		name := fmt.Sprintf("statement %d", place+1)
		switch {
		case statement.sql == priorityReadQuery:
			name = "the full statement"
		case statement.sql == prioritySummaryQuery:
			name = "the summary statement"
		case place == 0 && strings.Contains(statement.sql, "from priority_nodes"):
			name = "the live priorities"
		}
		named = append(named, prioritiesReadStatement{name: name, speedStatement: statement})
	}
	return named
}

// requirePlansRun stops the test unless the connection ran each statement at least runs times,
// every time under the plan it forces: a reading of a run under another plan is no reading of
// this one. Every statement of the read takes a parameter, so plan_cache_mode decides each of
// its plans.
func (s prioritiesReadServer) requirePlansRun(t *testing.T, statements []prioritiesReadStatement, runs int) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range statements {
		var generic, custom int64
		var parameters int
		if err := s.store.Pool.QueryRow(ctx,
			`select generic_plans, custom_plans, cardinality(parameter_types) from pg_prepared_statements where statement = $1`,
			statement.sql).Scan(&generic, &custom, &parameters); err != nil {
			t.Fatalf("read how the connection of %s planned %s: %v", s.plan.called, statement.name, err)
		}
		ran, other := custom, generic
		if s.plan.mode == "force_generic_plan" {
			ran, other = generic, custom
		}
		if parameters == 0 || ran < int64(runs) || other != 0 {
			t.Fatalf("the connection of %s ran %s (%d parameters) under %d generic and %d custom plans, want all of at least %d runs under %s: the reads were not all on that connection under that plan",
				s.plan.called, statement.name, parameters, generic, custom, runs, s.plan.called)
		}
	}
}

// prioritiesStatementPlan is one statement run under EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) on
// a forced plan's connection: its execution and planning times, the shared buffers its top node
// read, hit and read, which count every node below it, the walk's share of both, each part's,
// and the plan.
//
// The walk is priorityMemberTerms' descent from the relations down the parent links: the plan
// of the CTE members_spread, which each form's one statement holds. Its node's time and buffers
// count every node below it.
//
// The parts are every subplan of the statement, each CTE and each subquery the select list runs
// once (InitPlan), with its time and shared buffers. A node's counts take in every node below it,
// and a materialized CTE runs inside whichever node first reads it, so a part's counts take in
// those of every CTE it was the first to read.
//
// The event probes are the runs of each scan of events_issue_key_seq_key (newestEventIndex), the
// Actual Loops of every node that names it: the newest event's reads of an event of last_seq.
type prioritiesStatementPlan struct {
	milliseconds, planning float64
	hit, read              float64
	walks                  int
	walkMilliseconds       float64
	walkBuffers            float64
	eventProbes            float64
	parts                  []prioritiesPlanPart
	text                   string
}

// prioritiesPlanPart is one subplan of a statement's plan: its name, the node it begins with, how
// many times it ran, and the time and the shared buffers, hit and read, of it and every node
// below it over those runs.
type prioritiesPlanPart struct {
	name, node            string
	loops                 float64
	milliseconds, buffers float64
}

// partsRead says what each part of the statement took, in the order the plan holds them.
func (p prioritiesStatementPlan) partsRead() string {
	if len(p.parts) == 0 {
		return "no subplan"
	}
	described := make([]string, 0, len(p.parts))
	for _, part := range p.parts {
		described = append(described, fmt.Sprintf("%s (%s, %.0f loops) %.1f ms, %.0f shared buffers",
			part.name, part.node, part.loops, part.milliseconds, part.buffers))
	}
	return strings.Join(described, "; ")
}

func (p prioritiesStatementPlan) buffers() float64 { return p.hit + p.read }

// walk says what the walk took of the statement.
func (p prioritiesStatementPlan) walk() string {
	if p.walks == 0 {
		return "the statement holds no walk"
	}
	return fmt.Sprintf("%d walk of %.1f ms (%.0f%% of %.1f) and %.0f shared buffers (%.0f%% of %.0f)",
		p.walks, p.walkMilliseconds, percentOf(p.walkMilliseconds, p.milliseconds), p.milliseconds,
		p.walkBuffers, percentOf(p.walkBuffers, p.buffers()), p.buffers())
}

// percentOf is part as a percentage of whole, 0 of nothing.
func percentOf(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * part / whole
}

// prioritiesReadWalkCTE is the name EXPLAIN gives the plan of the walk (priorityMemberTerms).
const prioritiesReadWalkCTE = "CTE members_spread"

// explain runs statement on the forced plan's connection under EXPLAIN (ANALYZE, BUFFERS), with
// the arguments it was sent with, prepared so the plan is the one the connection's mode forces,
// and reads its plan. A plan that reads no shared buffer was not read, and stops the test.
func (s prioritiesReadServer) explain(t *testing.T, statement prioritiesReadStatement) prioritiesStatementPlan {
	t.Helper()
	ctx := context.Background()
	connection, err := s.store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take the connection of %s: %v", s.plan.called, err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, "prepare priorities_read_statement as "+statement.sql); err != nil {
		t.Fatalf("prepare %s: %v", statement.name, err)
	}
	defer func() {
		if _, err := connection.Exec(ctx, "deallocate priorities_read_statement"); err != nil {
			t.Errorf("deallocate %s: %v", statement.name, err)
		}
	}()
	var raw []byte
	if err := connection.QueryRow(ctx, "explain (analyze, buffers, format json) execute priorities_read_statement"+speedLiterals(t, statement.arguments)).Scan(&raw); err != nil {
		t.Fatalf("explain %s under %s: %v", statement.name, s.plan.called, err)
	}
	var plans []struct {
		Plan          prioritiesPlanNode `json:"Plan"`
		Planning      float64            `json:"Planning Time"`
		ExecutionTime float64            `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("read the plan of %s under %s: %d plans, %v", statement.name, s.plan.called, len(plans), err)
	}
	top := plans[0].Plan
	reading := prioritiesStatementPlan{
		milliseconds: plans[0].ExecutionTime, planning: plans[0].Planning,
		hit: top.SharedHitBlocks, read: top.SharedReadBlocks,
	}
	var visit func(node prioritiesPlanNode)
	visit = func(node prioritiesPlanNode) {
		if node.SubplanName == prioritiesReadWalkCTE {
			reading.walks++
			reading.walkMilliseconds += node.TotalTime * node.Loops
			reading.walkBuffers += node.SharedHitBlocks + node.SharedReadBlocks
		}
		if node.SubplanName != "" {
			reading.parts = append(reading.parts, prioritiesPlanPart{
				name: node.SubplanName, node: node.NodeType, loops: node.Loops,
				milliseconds: node.TotalTime * node.Loops, buffers: node.SharedHitBlocks + node.SharedReadBlocks,
			})
		}
		if node.IndexName == newestEventIndex {
			reading.eventProbes += node.Loops
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	visit(top)
	var text strings.Builder
	top.render(&text, 0)
	reading.text = text.String()
	if reading.buffers() == 0 {
		t.Fatalf("the plan of %s under %s reads no shared buffer: the plan was not read, so nothing was measured:\n%s", statement.name, s.plan.called, reading.text)
	}
	return reading
}

// prioritiesPlanNode is what the readings take of one node of an EXPLAIN (ANALYZE, BUFFERS,
// FORMAT JSON) plan, to say its cost and to draw it as EXPLAIN's text form draws it.
type prioritiesPlanNode struct {
	NodeType         string               `json:"Node Type"`
	Strategy         string               `json:"Strategy"`
	JoinType         string               `json:"Join Type"`
	Relationship     string               `json:"Parent Relationship"`
	SubplanName      string               `json:"Subplan Name"`
	RelationName     string               `json:"Relation Name"`
	Alias            string               `json:"Alias"`
	CTEName          string               `json:"CTE Name"`
	IndexName        string               `json:"Index Name"`
	FunctionName     string               `json:"Function Name"`
	IndexCond        string               `json:"Index Cond"`
	RecheckCond      string               `json:"Recheck Cond"`
	HashCond         string               `json:"Hash Cond"`
	MergeCond        string               `json:"Merge Cond"`
	JoinFilter       string               `json:"Join Filter"`
	Filter           string               `json:"Filter"`
	SortMethod       string               `json:"Sort Method"`
	PlanRows         float64              `json:"Plan Rows"`
	StartupTime      float64              `json:"Actual Startup Time"`
	TotalTime        float64              `json:"Actual Total Time"`
	Rows             float64              `json:"Actual Rows"`
	Loops            float64              `json:"Actual Loops"`
	RemovedByFilter  float64              `json:"Rows Removed by Filter"`
	RemovedByJoin    float64              `json:"Rows Removed by Join Filter"`
	SharedHitBlocks  float64              `json:"Shared Hit Blocks"`
	SharedReadBlocks float64              `json:"Shared Read Blocks"`
	TempReadBlocks   float64              `json:"Temp Read Blocks"`
	TempWritten      float64              `json:"Temp Written Blocks"`
	Plans            []prioritiesPlanNode `json:"Plans"`
}

// render draws node and every node below it, one line each, indented by depth.
func (node prioritiesPlanNode) render(to *strings.Builder, depth int) {
	indent := strings.Repeat("  ", depth)
	label := node.NodeType
	if node.Strategy != "" && node.Strategy != "Plain" {
		label = node.Strategy + " " + label
	}
	if node.JoinType != "" && node.JoinType != "Inner" {
		label += " " + node.JoinType
	}
	if node.IndexName != "" {
		label += " using " + node.IndexName
	}
	switch {
	case node.RelationName != "":
		label += " on " + node.RelationName
		if node.Alias != "" && node.Alias != node.RelationName {
			label += " " + node.Alias
		}
	case node.CTEName != "":
		label += " on " + node.CTEName
	case node.FunctionName != "":
		label += " on " + node.FunctionName
	}
	if node.SubplanName != "" {
		label = "[" + node.Relationship + " " + node.SubplanName + "] " + label
	}
	fmt.Fprintf(to, "%s-> %s (est %.0f rows; actual %.3f..%.3f ms, %.0f rows, %.0f loops; shared hit %.0f read %.0f",
		indent, label, node.PlanRows, node.StartupTime, node.TotalTime, node.Rows, node.Loops, node.SharedHitBlocks, node.SharedReadBlocks)
	if node.TempReadBlocks+node.TempWritten > 0 {
		fmt.Fprintf(to, "; temp read %.0f written %.0f", node.TempReadBlocks, node.TempWritten)
	}
	to.WriteString(")")
	for _, condition := range []struct{ name, value string }{
		{"index", node.IndexCond}, {"recheck", node.RecheckCond}, {"hash", node.HashCond}, {"merge", node.MergeCond},
		{"join filter", node.JoinFilter}, {"filter", node.Filter}, {"sort", node.SortMethod},
	} {
		if condition.value != "" {
			fmt.Fprintf(to, " %s: %s", condition.name, condition.value)
		}
	}
	if node.RemovedByFilter+node.RemovedByJoin > 0 {
		fmt.Fprintf(to, " (removed %.0f by filter, %.0f by join filter)", node.RemovedByFilter, node.RemovedByJoin)
	}
	to.WriteString("\n")
	for _, child := range node.Plans {
		child.render(to, depth+1)
	}
}

// prioritiesStatementTime is one statement's count in pg_stat_statements on the corpus's
// database: its calls and the milliseconds it spent executing and planning over them.
type prioritiesStatementTime struct {
	calls              int64
	executing, planned float64
}

// prioritiesReadQueryIDs is each statement's query id, the key pg_stat_statements counts it
// under, read as EXPLAIN (GENERIC_PLAN, VERBOSE) names it for the statement's own text. It stops
// the test where the server does not preload pg_stat_statements or EXPLAIN names no id, since the
// database's time of a read could then not be read.
func prioritiesReadQueryIDs(t *testing.T, database *store.Store, statements []prioritiesReadStatement) []int64 {
	t.Helper()
	ctx := context.Background()
	var libraries string
	if err := database.Pool.QueryRow(ctx, "select current_setting('shared_preload_libraries')").Scan(&libraries); err != nil {
		t.Fatalf("read shared_preload_libraries: %v", err)
	}
	if !strings.Contains(libraries, "pg_stat_statements") {
		t.Fatalf("cannot read the database's time of the read: the server does not preload pg_stat_statements (shared_preload_libraries = %q)", libraries)
	}
	if _, err := database.Pool.Exec(ctx, "create extension if not exists pg_stat_statements"); err != nil {
		t.Fatalf("cannot read the database's time of the read: create pg_stat_statements, which the server preloads: %v", err)
	}
	// The EXPLAIN is sent by the simple protocol, which carries the statement's text and its
	// placeholders as they are, where a prepared EXPLAIN would take them for its own parameters.
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("cannot read the database's time of the read: take a connection: %v", err)
	}
	defer connection.Release()
	ids := make([]int64, 0, len(statements))
	for _, statement := range statements {
		results, err := connection.Conn().PgConn().Exec(ctx, "explain (generic_plan, verbose, format json) "+statement.sql).ReadAll()
		if err != nil || len(results) != 1 || len(results[0].Rows) != 1 {
			t.Fatalf("cannot read the database's time of %s: explain it for its query id: %d results, %v", statement.name, len(results), err)
		}
		raw := results[0].Rows[0][0]
		var plans []struct {
			QueryID *int64 `json:"Query Identifier"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 || plans[0].QueryID == nil {
			t.Fatalf("cannot read the database's time of %s: EXPLAIN (VERBOSE) names no query id for it (%v): %.300s", statement.name, err, raw)
		}
		ids = append(ids, *plans[0].QueryID)
	}
	return ids
}

// prioritiesReadStatementTimes is pg_stat_statements' count of each query id on database's
// database, its top-level calls alone, a statement it holds no row of counting none.
func prioritiesReadStatementTimes(t *testing.T, database *store.Store, ids []int64) map[int64]prioritiesStatementTime {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select queryid, sum(calls)::bigint, sum(total_exec_time)::float8, sum(total_plan_time)::float8
		from pg_stat_statements
		where dbid = (select oid from pg_database where datname = current_database()) and toplevel and queryid = any($1::bigint[])
		group by queryid`, ids)
	if err != nil {
		t.Fatalf("cannot read the database's time of the read: read pg_stat_statements: %v", err)
	}
	defer rows.Close()
	times := make(map[int64]prioritiesStatementTime, len(ids))
	for rows.Next() {
		var id int64
		var counted prioritiesStatementTime
		if err := rows.Scan(&id, &counted.calls, &counted.executing, &counted.planned); err != nil {
			t.Fatalf("cannot read the database's time of the read: read a row of pg_stat_statements: %v", err)
		}
		times[id] = counted
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("cannot read the database's time of the read: read pg_stat_statements: %v", err)
	}
	return times
}

// nearestRankOf is the q quantile of sorted by the nearest-rank rule: the smallest read that at
// least q of the reads are at or under.
func nearestRankOf(sorted []time.Duration, q float64) time.Duration {
	return sorted[int(math.Ceil(q*float64(len(sorted))))-1]
}

// readMilliseconds is d in milliseconds, as the reading lines print a time.
func readMilliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// indentPlan sets a plan's lines off from the line that names it.
func indentPlan(text string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n    ")
}
