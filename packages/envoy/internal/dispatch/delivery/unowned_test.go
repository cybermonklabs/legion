package delivery

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// seedUnownedIssueProject writes project key and its issues, directly: the P0 rule reads
// issues.priority, status, claimed_by and route and nothing else.
func seedUnownedIssueProject(t *testing.T, ctx context.Context, pool *store.Pool, key string, issues []unownedSeed) {
	t.Helper()
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, $1, 'alice') on conflict do nothing`, key); err != nil {
		t.Fatalf("seed project %s: %v", key, err)
	}
	for _, issue := range issues {
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, priority, route, claimed_by, claimed_at, created_by, rank)
			values ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, case when $8::jsonb is null then null else now() end, '{"kind":"system","id":"delivery-test"}', 'U')
		`, issue.key, key, issue.number, issue.title, issue.status, issue.priority, issue.route, issue.claimedBy); err != nil {
			t.Fatalf("seed issue %s: %v", issue.key, err)
		}
	}
}

type unownedSeed struct {
	key       string
	number    int
	title     string
	status    string
	priority  *int
	route     *string
	claimedBy *string
}

func TestListUnownedP0(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	p0, p1 := 0, 1
	route := "role:dora"
	claim := `{"kind":"session","id":"01a1-test-session"}`
	seedUnownedIssueProject(t, ctx, pool, "ACME", []unownedSeed{
		{key: "ACME-103", number: 103, title: "Production deploy gate flakes", status: "triage", priority: &p0},
		{key: "ACME-104", number: 104, title: "claimed", status: "in_progress", priority: &p0, claimedBy: &claim},
		{key: "ACME-105", number: 105, title: "routed", status: "todo", priority: &p0, route: &route},
		{key: "ACME-106", number: 106, title: "closed", status: "done", priority: &p0},
		{key: "ACME-107", number: 107, title: "not a P0", status: "todo", priority: &p1},
		{key: "ACME-2", number: 2, title: "an earlier unowned P0", status: "backlog", priority: &p0},
	})
	seedUnownedIssueProject(t, ctx, pool, "ACMEB", []unownedSeed{
		{key: "ACMEB-1", number: 1, title: "another project's unowned P0", status: "todo", priority: &p0},
	})

	unowned, err := ListUnownedP0(ctx, pool, everyViewer)
	if err != nil {
		t.Fatalf("ListUnownedP0: %v", err)
	}
	// The test's database is its own (deliveryTestPool), so every issue the rule answers is one
	// this test wrote, in project-then-number order.
	want := []UnownedIssue{
		{Key: "ACME-2", Title: "an earlier unowned P0"},
		{Key: "ACME-103", Title: "Production deploy gate flakes"},
		{Key: "ACMEB-1", Title: "another project's unowned P0"},
	}
	if !slices.Equal(unowned, want) {
		t.Fatalf("ListUnownedP0 = %+v, want %+v (open, P0, neither claimed nor routed; project then number)", unowned, want)
	}

	// One project's are that project's alone, in number order.
	acme, err := ListUnownedP0(ctx, pool, "ACME")
	if err != nil {
		t.Fatalf("ListUnownedP0 of ACME: %v", err)
	}
	if want := want[:2]; !slices.Equal(acme, want) {
		t.Fatalf("ListUnownedP0 of ACME = %+v, want %+v (its open P0 issues neither claimed nor routed, by number)", acme, want)
	}
	// The count is the length of that list.
	for project, want := range map[string]int{"ACME": 2, "ACMEB": 1} {
		if count, err := CountUnownedP0(ctx, pool, project); err != nil || count != want {
			t.Errorf("CountUnownedP0 of %s = %d, %v; want %d, the length of its list", project, count, err, want)
		}
	}
}

// The reads of ListUnownedP0, one project's and every project's, and CountUnownedP0's count of one
// project's read issues through issues_open_p0 (migration 20261117100010), the partial index of
// the open P0 issues, with the claim and the route a filter over it: the plan of each statement as
// it is sent, `EXPLAIN (ANALYZE, BUFFERS)` with sequential scans off (explaintest.AnalyzedPlan),
// reads through issues_open_p0 exactly once. A project's read is keyed by the index's
// project_key; the read of every project is the whole of an index that holds the open P0 issues
// alone. The fixture's project holds 200 issues, one of them an unowned P0, and issues is analyzed
// before the plans (seedP0PlanProject says why).
func TestTheUnownedP0ReadsReadTheirPartialIndex(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	p0 := 0
	seedP0PlanProject(t, ctx, pool, "PINDEX", []unownedSeed{{key: "PINDEX-1", number: 1, title: "unowned", status: "todo", priority: &p0}})

	for _, read := range []struct {
		name      string
		statement string
		arguments []any
	}{
		{"every project's", unownedP0Query, nil},
		{"one project's", projectUnownedP0Query, []any{"PINDEX"}},
		{"one project's count", projectUnownedP0CountQuery, []any{"PINDEX"}},
	} {
		plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, read.statement, read.arguments...)
		scan := indexScanOf(t, plan, raw, "issues_open_p0")
		if read.arguments != nil && !strings.Contains(scan.Cond, "project_key") {
			t.Fatalf("%s unowned P0 read's index condition %q does not key the project; want one range of issues_open_p0:\n%s", read.name, scan.Cond, raw)
		}
	}
}

// seedP0PlanProject writes project with head, its first issues numbered from 1, then P2 issues up
// to 200 in all, and analyzes issues, the fixture of a plan test of a read of issues_open_p0.
// Unanalyzed, the planner takes the table for a few rows, and reads one project's in number order
// through issues_project_key_number_key, filtering each, as cheaply as it reads the partial index
// and sorts, which says nothing about the statement.
func seedP0PlanProject(t *testing.T, ctx context.Context, pool *store.Pool, project string, head []unownedSeed) {
	t.Helper()
	p2 := 2
	seeds := head
	for number := len(head) + 1; number <= 200; number++ {
		seeds = append(seeds, unownedSeed{key: project + "-" + strconv.Itoa(number), number: number, title: "not a P0", status: "todo", priority: &p2})
	}
	seedUnownedIssueProject(t, ctx, pool, project, seeds)
	if _, err := pool.Exec(ctx, `analyze issues`); err != nil {
		t.Fatalf("analyze issues: %v", err)
	}
}

// The delivery live hub's read of every open P0 issue's key (OpenP0Keys), which the hub makes each
// time it stands up its event feed, answers the keys of the open P0 issues and no other, and reads
// issues through issues_open_p0 (migration 20261117100010), the partial index of the open P0
// issues: the plan of the statement as OpenP0Keys sends it, `EXPLAIN (ANALYZE, BUFFERS)` with
// sequential scans off (explaintest.AnalyzedPlan), reads through issues_open_p0 exactly once. The
// fixture's project holds 200 issues, two open P0 issues, a done P0 issue and 197 P2 issues, and
// issues is analyzed first (seedP0PlanProject says why).
func TestTheOpenP0KeysReadReadsTheOpenP0Index(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	p0 := 0
	seedP0PlanProject(t, ctx, pool, "POPEN", []unownedSeed{
		{key: "POPEN-1", number: 1, title: "open", status: "todo", priority: &p0},
		{key: "POPEN-2", number: 2, title: "open and claimed", status: "in_progress", priority: &p0, claimedBy: new(`{"kind":"session","id":"ses_claimer"}`)},
		{key: "POPEN-3", number: 3, title: "done", status: "done", priority: &p0},
	})
	keys, err := OpenP0Keys(ctx, pool)
	if err != nil {
		t.Fatalf("OpenP0Keys: %v", err)
	}
	// The test's database is its own (deliveryTestPool), so every key OpenP0Keys answers is one this
	// test wrote: the two open P0 issues, and neither the done P0 issue POPEN-3 nor a P2 issue.
	if want := map[string]struct{}{"POPEN-1": {}, "POPEN-2": {}}; !maps.Equal(keys, want) {
		t.Fatalf("OpenP0Keys answers %v, want %v: the open P0 issues alone", slices.Sorted(maps.Keys(keys)), slices.Sorted(maps.Keys(want)))
	}
	plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, openP0KeysQuery)
	indexScanOf(t, plan, raw, "issues_open_p0")
	t.Logf("the open P0 keys read plans as:\n%s", raw)
}
