package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// analyzedNode is what these tests read of one node of an `EXPLAIN (ANALYZE, FORMAT JSON)` plan:
// how many times the node ran, how many rows it answered on each of those runs, what it reads,
// and where it hangs. `loops` and `actual rows` are only in an ANALYZE plan, which is why these
// tests run the statements rather than only plan them. The rows each filter threw away and the
// shared buffers the node and the nodes below it accessed are what the speed test counts
// (speedWork); the buffer counts are only in a plan run with BUFFERS. A term of the statement's
// `with` list is planned as a subplan named "CTE <term>", and a scan of one names its term.
type analyzedNode struct {
	NodeType         string         `json:"Node Type"`
	Relationship     string         `json:"Parent Relationship,omitempty"`
	SubplanName      string         `json:"Subplan Name,omitempty"`
	RelationName     string         `json:"Relation Name,omitempty"`
	IndexName        string         `json:"Index Name,omitempty"`
	CTEName          string         `json:"CTE Name,omitempty"`
	Rows             float64        `json:"Actual Rows"`
	Loops            float64        `json:"Actual Loops"`
	RemovedByFilter  float64        `json:"Rows Removed by Filter,omitempty"`
	RemovedByJoin    float64        `json:"Rows Removed by Join Filter,omitempty"`
	RemovedByRecheck float64        `json:"Rows Removed by Index Recheck,omitempty"`
	SharedHitBlocks  float64        `json:"Shared Hit Blocks,omitempty"`
	SharedReadBlocks float64        `json:"Shared Read Blocks,omitempty"`
	Plans            []analyzedNode `json:"Plans,omitempty"`
}

// name is the node as a failure cites it: the type, and the relation or index it reads.
func (node analyzedNode) name() string {
	described := node.NodeType
	if node.RelationName != "" {
		described += " on " + node.RelationName
	}
	if node.IndexName != "" {
		described += " using " + node.IndexName
	}
	return described
}

// attachSpine is every join that attaches a value to the rows the caller gets: the joins on the
// plan's outer edge, read from the top down. Each of them takes the rows below it and adds one
// relation's columns, so its own row count is the answer's. A join inside the relation being
// attached — the grouped read of a listing's blockers, say — is below an inner edge and is not
// one of these: it builds the thing attached, not the attachment.
func (node analyzedNode) attachSpine() []analyzedNode {
	spine := []analyzedNode{}
	for {
		if slices.Contains(joinTypes, node.NodeType) {
			spine = append(spine, node)
		}
		outer := -1
		for index, child := range node.Plans {
			if child.Relationship == "Outer" {
				outer = index
				break
			}
		}
		if outer < 0 {
			return spine
		}
		node = node.Plans[outer]
	}
}

// everyNode is every node of the plan, the CTE and subplan subtrees included.
func (node analyzedNode) everyNode() []analyzedNode {
	nodes := []analyzedNode{node}
	for _, child := range node.Plans {
		nodes = append(nodes, child.everyNode()...)
	}
	return nodes
}

// joinTypes is every node that attaches one relation's rows to another's. A listing's six
// resolved values each reach the page through one of these.
var joinTypes = []string{"Nested Loop", "Hash Join", "Merge Join"}

// analyzePlan runs statement under `EXPLAIN (ANALYZE, FORMAT JSON)` and answers its root node
// with the plan's text, which a failure prints.
func analyzePlan(t *testing.T, database *store.Store, statement string, arguments ...any) (analyzedNode, string) {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), "explain (analyze, format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	var plans []struct {
		Plan analyzedNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode the explain answer (%v): %s", err, raw)
	}
	return plans[0].Plan, string(raw)
}

// A paged listing resolves its page and nothing else. The node that attaches each of the six
// values to the page runs once and answers at most the page's rows, and each parent walk starts
// from at most the page's keys, which is what says the walk is no longer one per matching
// issue. The fixture's project holds several hundred issues, so a plan that resolved them all
// would show it.
func TestAPagedListingResolvesItsPageAlone(t *testing.T) {
	t.Parallel()
	fixture := newIssueListFixture(t)
	for _, listing := range []struct {
		name      string
		statement string
		arguments []any
		limit     int
	}{
		{
			name:      "the paged listing",
			statement: listIssuesPageQuery,
			arguments: []any{"LIST", []string{}, "", nil, []string{}, false, []int16{}, false, nil, nil, []string{}, false, false, false, false, "", "", 5, 10},
			limit:     5,
		},
		{
			name:      "the paged pinned listing",
			statement: listPinnedIssuesPageQuery,
			arguments: []any{"LIST", []string{}, "", nil, []string{}, false, []int16{}, false, nil, nil, []string{}, false, false, false, false, "", "", "alice", 2, 1},
			limit:     2,
		},
	} {
		t.Run(listing.name, func(t *testing.T) {
			plan, text := analyzePlan(t, fixture.database, listing.statement, listing.arguments...)
			name, limit := listing.name, listing.limit
			joins := plan.attachSpine()
			for _, node := range joins {
				if node.Loops != 1 || node.Rows > float64(limit) {
					t.Errorf("%s attaches rows at %s with %.0f rows over %.0f loops, want at most %d rows over one loop:\n%s",
						name, node.name(), node.Rows, node.Loops, limit, text)
				}
			}
			if len(joins) < 6 {
				t.Errorf("%s attaches its values through %d joins, want one for each of the six:\n%s", name, len(joins), text)
			}
			walks := 0
			for _, node := range plan.everyNode() {
				if node.NodeType != "Recursive Union" {
					continue
				}
				walks++
				if len(node.Plans) == 0 {
					t.Fatalf("%s has a parent walk with no base term:\n%s", name, text)
				}
				// A base term reported with no loop at all never ran, which is each walk of the
				// advances filter on a request that names no priority, and the priority filter's on
				// one that names no priority level: each seed is gated off (issueAdvancesFilterTerms,
				// issuePriorityFilterTerms), so it starts from no key rather than from many.
				if base := node.Plans[0]; base.Rows > float64(limit) || base.Loops > 1 {
					t.Errorf("%s starts a parent walk from %.0f keys over %.0f loops, want at most the page's %d over one loop:\n%s",
						name, base.Rows, base.Loops, limit, text)
				}
			}
			if walks != 7 {
				t.Errorf("%s runs %d parent walks, want the components walk, the repository walk, the priority walk,"+
					" the walk up every page row's chain for its advances relations, the walk down from the advances"+
					" filter's own relations, the walk up from them that `own` reads and the priority filter's walk:\n%s",
					name, walks, text)
			}
		})
	}
}

// An unpaged listing resolves every issue its filters match, and reads each of the three tables
// its joins reach once for all of them rather than once per issue: the blockers' issue_links, the
// components' issue_components, and projects, for each project's default repository.
func TestAnUnpagedListingReadsEachResolvedTableOnce(t *testing.T) {
	t.Parallel()
	fixture := newIssueListFixture(t)
	plan, text := analyzePlan(t, fixture.database, listIssuesQuery, "LIST", []string{}, "", nil, []string{}, false, []int16{}, false, nil, nil, []string{}, false, false, false, false, "", "")
	for _, relation := range []string{"issue_links", "issue_components", "projects"} {
		read := 0
		for _, node := range plan.everyNode() {
			if node.RelationName != relation {
				continue
			}
			read++
			if node.Loops != 1 {
				t.Errorf("the listing reads %s at %s over %.0f loops, want one:\n%s", relation, node.name(), node.Loops, text)
			}
		}
		if read == 0 {
			t.Errorf("the listing reads no %s at all:\n%s", relation, text)
		}
	}
}

// The listing reaches its open asks through the partial index on them, asks_open, rather than
// reading the table: an instance's ask volume is far larger than any one listing's. The index
// the scan reads is the assertion, not the absence of a sequential scan, because a scan can
// carry an index condition on a column its index does not lead with and still read the whole
// index (api/audited_reads_test.go). A condition is not required here: the index's own
// predicate, `state = 'open'`, is this read's filter, so the whole of it is exactly the rows
// the grouped read counts, and a condition it does carry is held to its first key column.
func TestTheListingReadsOpenAsksThroughTheirIndex(t *testing.T) {
	t.Parallel()
	fixture := newIssueListFixture(t)
	firstKeys := explaintest.NewIndexKeys(fixture.database.Pool)
	for name, read := range map[string]struct {
		statement string
		arguments []any
	}{
		"the listing":       {listIssuesQuery, []any{"LIST", []string{}, "", nil, []string{}, false, []int16{}, false, nil, nil, []string{}, false, false, false, false, "", ""}},
		"the paged listing": {listIssuesPageQuery, []any{"LIST", []string{}, "", nil, []string{}, false, []int16{}, false, nil, nil, []string{}, false, false, false, false, "", "", 5, 10}},
	} {
		plan := planWithoutSeqScans(t, fixture.database, read.statement, read.arguments...)
		if explaintest.SeqScansRelation(t, plan, "asks") {
			t.Fatalf("%s scans asks sequentially; want a search of asks_open:\n%s", name, plan)
		}
		firstKeys.RequireIndexKeyed(t, plan, "asks_open", name, explaintest.CondOptional)
	}
}

// The listing reads the move into `backlog` or `icebox` only when a request asks for it
// (issueMovedJoin), and then only for the rows standing in one of the two. Two of the fixture's
// issues are moved into `backlog`, so each has an event the read would find. A request without
// `?moved=true` never runs the events read, whichever plan it gets: a plan made for its own
// arguments drops the read, and the generic plan a prepared statement settles on after five runs
// holds it under a one-time filter that is false. A request with the key runs it once for each
// of the two parked rows and for no other of the several hundred it lists.
func TestTheListingReadsTheMoveOnlyWhenAskedAndOnlyForParkedRows(t *testing.T) {
	t.Parallel()
	fixture := newIssueListFixture(t)
	parked := []string{fixture.keys[4], fixture.keys[5]}
	for _, key := range parked {
		moveTestIssue(t, fixture.handler, key, "backlog", "alice")
	}
	for _, listing := range []struct {
		name    string
		moved   bool
		generic bool
		loops   float64
	}{
		{name: "a listing that does not ask for the move", moved: false, loops: 0},
		{name: "a listing that does not ask for the move, under the generic plan", moved: false, generic: true, loops: 0},
		{name: "a listing that asks for the move", moved: true, loops: float64(len(parked))},
		{name: "a listing that asks for the move, under the generic plan", moved: true, generic: true, loops: float64(len(parked))},
	} {
		var plan analyzedNode
		var text string
		if listing.generic {
			plan, text = analyzeGenericListingPlan(t, fixture.database, "{}", false, listing.moved)
		} else {
			plan, text = analyzePlan(t, fixture.database, listIssuesQuery,
				"LIST", []string{}, "", nil, []string{}, false, []int16{}, false, nil, nil, []string{}, false, false, listing.moved, false, "", "")
		}
		reads, loops := 0, 0.0
		for _, node := range plan.everyNode() {
			if node.RelationName == "events" {
				reads++
				loops += node.Loops
			}
		}
		// A plan that holds no events read at all is one that read none, which only a request
		// without the key may be answered by; the generic plan must hold it, or it says nothing.
		if reads == 0 && (listing.moved || listing.generic) {
			t.Fatalf("%s holds no read of events in its plan, so the plan cannot say whether the gate holds:\n%s", listing.name, text)
		}
		if loops != listing.loops {
			t.Errorf("%s reads events over %.0f loops, want %.0f:\n%s", listing.name, loops, listing.loops, text)
		}
	}
}

// scans reports whether the node, or a node below it, scans the `with` term named cte.
func (node analyzedNode) scans(cte string) bool {
	return slices.ContainsFunc(node.everyNode(), func(below analyzedNode) bool { return below.CTEName == cte })
}

// The listing's priority filter climbs once for the request, and only when the request names a
// priority (issuePriorityFilterTerms). A listing of project LIST by priority 0, and one by none,
// start the filter's walk once, from at most LIST's issues with no priority of their own and from
// none of another project's, and test each issue against the walk's answer through a subplan
// that runs once, a hashed read of the answer rather than a read for each issue, whichever plan
// they get. A listing that names no priority never starts the walk. Each of the fixture's two
// projects holds several hundred issues with no priority of their own, so a walk from every issue
// of the table, or a test that read the answer once for each issue, would show it.
func TestThePriorityFilterClimbsOnceFromItsProjectsIssues(t *testing.T) {
	t.Parallel()
	fixture := newIssueListFixture(t)
	var candidates float64
	if err := fixture.database.Pool.QueryRow(context.Background(),
		`select count(*) from issues where project_key = 'LIST' and priority is null`).Scan(&candidates); err != nil {
		t.Fatalf("count LIST's issues with no priority of their own: %v", err)
	}
	for _, listing := range []struct {
		name    string
		levels  []int16
		literal string
		none    bool
		generic bool
	}{
		{name: "a listing by priority 0", levels: []int16{0}, literal: "{0}"},
		{name: "a listing by priority 0, under the generic plan", levels: []int16{0}, literal: "{0}", generic: true},
		{name: "a listing by none", levels: []int16{}, literal: "{}", none: true},
		{name: "a listing by none, under the generic plan", levels: []int16{}, literal: "{}", none: true, generic: true},
		{name: "a listing that names no priority", levels: []int16{}, literal: "{}"},
		{name: "a listing that names no priority, under the generic plan", levels: []int16{}, literal: "{}", generic: true},
	} {
		t.Run(listing.name, func(t *testing.T) {
			var plan analyzedNode
			var text string
			if listing.generic {
				plan, text = analyzeGenericListingPlan(t, fixture.database, listing.literal, listing.none, false)
			} else {
				plan, text = analyzePlan(t, fixture.database, listIssuesQuery,
					"LIST", []string{}, "", nil, []string{}, false, listing.levels, listing.none, nil, nil, []string{}, false, false, false, false, "", "")
			}
			name, filtered := listing.name, len(listing.levels) > 0 || listing.none
			walks := 0
			for _, node := range plan.everyNode() {
				if node.SubplanName != "CTE priority_inherited_chain" {
					continue
				}
				walks++
				if node.NodeType != "Recursive Union" || len(node.Plans) == 0 {
					t.Fatalf("%s plans the priority filter's walk as %s with %d inputs, want a recursive union with its base term:\n%s",
						name, node.name(), len(node.Plans), text)
				}
				base := node.Plans[0]
				switch {
				case !filtered && base.Loops != 0:
					t.Errorf("%s starts the priority filter's walk over %.0f loops, want never:\n%s", name, base.Loops, text)
				case filtered && (node.Loops != 1 || base.Loops != 1):
					t.Errorf("%s runs the priority filter's walk over %.0f loops from a base term run over %.0f, want one each:\n%s",
						name, node.Loops, base.Loops, text)
				case filtered && (base.Rows == 0 || base.Rows > candidates):
					t.Errorf("%s starts the priority filter's walk from %.0f issues, want at least one and at most LIST's %.0f with no priority of their own:\n%s",
						name, base.Rows, candidates, text)
				}
			}
			if walks != 1 {
				t.Fatalf("%s plans %d walks of the priority filter, want one:\n%s", name, walks, text)
			}
			if !filtered {
				return
			}
			tests := 0
			for _, node := range plan.everyNode() {
				if node.Relationship != "SubPlan" || !node.scans("priority_inherited") {
					continue
				}
				tests++
				if node.Loops > 1 {
					t.Errorf("%s tests the issues against the priority filter's answer at %s over %.0f loops, want one:\n%s",
						name, node.name(), node.Loops, text)
				}
			}
			if tests == 0 {
				t.Errorf("%s tests no issue against the priority filter's answer through a subplan:\n%s", name, text)
			}
		})
	}
}

// analyzeGenericListingPlan runs the unpaged listing of project LIST under the generic plan a
// prepared statement settles on, which is planned without its arguments, and answers its root
// node as analyzePlan does. The arguments are the request that filters by nothing but the priority
// levels (an array literal of smallints, '{}' for none) and none, with moved the one other that
// varies.
func analyzeGenericListingPlan(t *testing.T, database *store.Store, levels string, none, moved bool) (analyzedNode, string) {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	defer conn.Release()
	for _, statement := range []string{
		"set plan_cache_mode = force_generic_plan",
		"prepare generic_listing as " + listIssuesQuery,
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("prepare the generic listing: %v", err)
		}
	}
	defer func() {
		if _, err := conn.Exec(ctx, "deallocate generic_listing"); err != nil {
			t.Errorf("deallocate the generic listing: %v", err)
		}
		if _, err := conn.Exec(ctx, "reset plan_cache_mode"); err != nil {
			t.Errorf("reset plan_cache_mode: %v", err)
		}
	}()
	var raw []byte
	if err := conn.QueryRow(ctx, fmt.Sprintf(
		"explain (analyze, format json) execute generic_listing('LIST', '{}', '', null, '{}', false, '%s', %t, null, null, '{}', false, false, %t, false, '', '')",
		levels, none, moved)).Scan(&raw); err != nil {
		t.Fatalf("explain the generic listing: %v", err)
	}
	var plans []struct {
		Plan analyzedNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode the explain answer (%v): %s", err, raw)
	}
	return plans[0].Plan, string(raw)
}

// The project list counts every project's open asks in one grouped read: no node of its plan is
// a subquery run for each project row, and the asks it counts are read once.
func TestTheProjectListRunsNoSubqueryPerProject(t *testing.T) {
	t.Parallel()
	fixture := newIssueListFixture(t)
	plan, text := analyzePlan(t, fixture.database, projectListQuery)
	for _, node := range plan.everyNode() {
		if node.Relationship == "SubPlan" {
			t.Errorf("the project list runs %s as a subplan, want one grouped read for every project:\n%s", node.name(), text)
		}
		if node.RelationName == "asks" && node.Loops != 1 {
			t.Errorf("the project list reads asks at %s over %.0f loops, want one:\n%s", node.name(), node.Loops, text)
		}
	}
	if !strings.Contains(text, "asks") {
		t.Errorf("the project list's plan reads no asks at all:\n%s", text)
	}
}
