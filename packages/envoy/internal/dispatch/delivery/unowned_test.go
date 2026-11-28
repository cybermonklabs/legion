package delivery

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// seedUnownedIssueProject writes project key and its issues, directly, each parent before its
// child: the P0 rule reads issues.priority, status, claimed_by, route and parent_key and nothing
// else.
func seedUnownedIssueProject(t *testing.T, ctx context.Context, pool *store.Pool, key string, issues []unownedSeed) {
	t.Helper()
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, $1, 'alice') on conflict do nothing`, key); err != nil {
		t.Fatalf("seed project %s: %v", key, err)
	}
	for _, issue := range issues {
		var parent *string
		if issue.parent != "" {
			parent = &issue.parent
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, priority, route, claimed_by, claimed_at, parent_key, created_by, rank)
			values ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, case when $8::jsonb is null then null else now() end, $9, '{"kind":"system","id":"delivery-test"}', 'U')
		`, issue.key, key, issue.number, issue.title, issue.status, issue.priority, issue.route, issue.claimedBy, parent); err != nil {
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
	parent    string
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

	unowned, err := ListUnownedP0(ctx, pool, "")
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

	// An issue with no priority of its own takes the priority of the nearest issue above it that
	// has one, at any status of that issue and in any project, within refs.IssueDepthCap links
	// (dispatch://INFRA-5793/spec#b-j-inherit). One that takes P0 so is held to the rule as an
	// issue with its own P0 is: open, and neither claimed nor routed.
	seedUnownedIssueProject(t, ctx, pool, "ACME", []unownedSeed{
		{key: "ACME-110", number: 110, title: "under another project's P0", status: "todo", parent: "ACMEB-1"},
		{key: "ACME-120", number: 120, title: "one level under a P0", status: "todo", parent: "ACME-2"},
		{key: "ACME-121", number: 121, title: "two levels under a P0", status: "todo", parent: "ACME-120"},
		{key: "ACME-122", number: 122, title: "three levels under a P0", status: "in_progress", parent: "ACME-121"},
		{key: "ACME-130", number: 130, title: "its own P1 under a P0", status: "todo", priority: &p1, parent: "ACME-103"},
		{key: "ACME-131", number: 131, title: "under its parent's own P1", status: "todo", parent: "ACME-130"},
		{key: "ACME-140", number: 140, title: "under a closed P0", status: "todo", parent: "ACME-106"},
		{key: "ACME-150", number: 150, title: "inheriting, claimed", status: "in_progress", parent: "ACME-2", claimedBy: &claim},
		{key: "ACME-151", number: 151, title: "inheriting, routed", status: "todo", parent: "ACME-2", route: &route},
		{key: "ACME-160", number: 160, title: "inheriting, closed", status: "done", parent: "ACME-2"},
	})
	atCap, pastCap := seedChainPastTheCap(t, ctx, pool, "ACMEC")
	unowned, err = ListUnownedP0(ctx, pool, "")
	if err != nil {
		t.Fatalf("ListUnownedP0 with the inheriting issues: %v", err)
	}
	listed := map[string]bool{}
	for _, issue := range unowned {
		listed[issue.Key] = true
	}
	for _, row := range []struct {
		key     string
		unowned bool
		why     string
	}{
		{"ACME-110", true, "an open child with no priority under a P0 parent in another project"},
		{"ACME-120", true, "an open child with no priority under an unowned P0"},
		{"ACME-122", true, "an open issue three levels under a P0, none between with a priority"},
		{"ACME-130", false, "a child with its own P1 under a P0 parent"},
		{"ACME-131", false, "under a parent with its own P1 under a P0, so it takes P1, the nearest"},
		{"ACME-140", true, "an open child with no priority under a done P0 parent"},
		{"ACME-150", false, "a claimed child that takes P0"},
		{"ACME-151", false, "a routed child that takes P0"},
		{"ACME-160", false, "a done child that takes P0"},
		{atCap, true, "an open issue refs.IssueDepthCap links under its chain's one P0"},
		{pastCap, false, "an open issue one link past refs.IssueDepthCap under its chain's one P0"},
	} {
		if listed[row.key] != row.unowned {
			t.Errorf("ListUnownedP0 lists %s: %t, want %t (%s)", row.key, listed[row.key], row.unowned, row.why)
		}
	}
	var got []string
	for _, issue := range unowned {
		got = append(got, issue.Key)
	}
	wantKeys := []string{"ACME-2", "ACME-103", "ACME-110", "ACME-120", "ACME-121", "ACME-122", "ACME-140", "ACMEB-1", "ACMEC-1", atCap}
	if !slices.Equal(got, wantKeys) {
		t.Fatalf("ListUnownedP0 with the inheriting issues = %v, want %v (project then number)", got, wantKeys)
	}
	// One project's read holds its issues that take P0 from another project's, ACME-110 among them.
	acme, err = ListUnownedP0(ctx, pool, "ACME")
	if err != nil {
		t.Fatalf("ListUnownedP0 of ACME with the inheriting issues: %v", err)
	}
	got = got[:0]
	for _, issue := range acme {
		got = append(got, issue.Key)
	}
	if want := wantKeys[:7]; !slices.Equal(got, want) {
		t.Fatalf("ListUnownedP0 of ACME with the inheriting issues = %v, want %v (by number)", got, want)
	}
	for project, want := range map[string]int{"ACME": 7, "ACMEB": 1, "ACMEC": 2} {
		if count, err := CountUnownedP0(ctx, pool, project); err != nil || count != want {
			t.Errorf("CountUnownedP0 of %s with the inheriting issues = %d, %v; want %d", project, count, err, want)
		}
	}
	// The claimed and the routed child are P0 all the same, and so in the live hub's open-P0 set.
	open, err := OpenP0Keys(ctx, pool)
	if err != nil {
		t.Fatalf("OpenP0Keys: %v", err)
	}
	for _, key := range []string{"ACME-150", "ACME-151"} {
		if _, p0 := open[key]; !p0 {
			t.Errorf("OpenP0Keys does not hold %s, an open child of the P0 ACME-2 with no priority of its own", key)
		}
	}
}

// The reads of ListUnownedP0, one project's and every project's, and CountUnownedP0's count of one
// project's read the issues whose own priority is 0 through the two partial indexes that hold them,
// issues_open_p0 (migration 20261117100010) for the open ones and issues_done_p0
// (20261127112234) for the done ones, since a done P0 passes P0 to an open child, and descend
// from them to the issues under them that take that P0 through issues_parent_key, with the
// status, the claim and the route a filter over what they reach: the plan of each statement as it
// is sent, `EXPLAIN (ANALYZE, BUFFERS)` with sequential scans off (explaintest.AnalyzedPlan),
// reads through each partial index exactly once and through issues_parent_key. A project's read
// starts from every project's P0 issues, as the read of every project does, since an issue takes
// the priority of an issue of another project above it: neither keys its seeds by project_key.
// The fixture's project holds 200 issues: an unowned P0 with an open child that has no priority,
// a done P0 with another, and P2 issues; and issues is analyzed before the plans
// (seedP0PlanProject says why).
func TestTheUnownedP0ReadsReadTheirPartialIndex(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	p0 := 0
	seedP0PlanProject(t, ctx, pool, "PINDEX", []unownedSeed{
		{key: "PINDEX-1", number: 1, title: "unowned", status: "todo", priority: &p0},
		{key: "PINDEX-2", number: 2, title: "under the unowned P0", status: "todo", parent: "PINDEX-1"},
		{key: "PINDEX-3", number: 3, title: "done", status: "done", priority: &p0},
		{key: "PINDEX-4", number: 4, title: "under the done P0", status: "todo", parent: "PINDEX-3"},
	})

	for _, read := range []struct {
		name      string
		statement string
		arguments []any
	}{
		{"every project's", unownedP0Query, nil},
		{"one project's", projectUnownedP0Query, []any{"PINDEX"}},
		{"one project's count", projectUnownedP0CountQuery, []any{"PINDEX"}},
	} {
		t.Run(read.name, func(t *testing.T) {
			plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, read.statement, read.arguments...)
			indexScanOf(t, plan, raw, "issues_open_p0")
			indexScanOf(t, plan, raw, "issues_done_p0")
			requireDescentThroughParentKey(t, plan, raw)
		})
	}
}

// requireDescentThroughParentKey fails the test unless plan reads issues through
// issues_parent_key: the descent from the issues whose own priority is 0 to the issues under them
// that take it.
func requireDescentThroughParentKey(t *testing.T, plan json.RawMessage, raw string) {
	t.Helper()
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Index == "issues_parent_key" {
			return
		}
	}
	t.Fatalf("the plan reads through no issues_parent_key; want the descent to the issues that take a P0 from above through it:\n%s", raw)
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
// the issues whose own priority is 0 through issues_open_p0 (migration 20261117100010) and
// issues_done_p0 (20261127112234), the partial indexes of the open and the done ones, and the
// issues under them through issues_parent_key: the plan of the statement as OpenP0Keys sends it,
// `EXPLAIN (ANALYZE, BUFFERS)` with sequential scans off (explaintest.AnalyzedPlan), reads through
// each partial index exactly once and through issues_parent_key. The fixture's project holds 200
// issues: two open P0 issues, a done P0 issue, a done child with no priority of its own under an
// open P0, which is no open P0 since it is done, and 196 P2 issues; and issues is analyzed first
// (seedP0PlanProject says why).
func TestTheOpenP0KeysReadReadsTheOpenP0Index(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	p0 := 0
	seedP0PlanProject(t, ctx, pool, "POPEN", []unownedSeed{
		{key: "POPEN-1", number: 1, title: "open", status: "todo", priority: &p0},
		{key: "POPEN-2", number: 2, title: "open and claimed", status: "in_progress", priority: &p0, claimedBy: new(`{"kind":"session","id":"ses_claimer"}`)},
		{key: "POPEN-3", number: 3, title: "done", status: "done", priority: &p0},
		{key: "POPEN-4", number: 4, title: "done under an open P0", status: "done", parent: "POPEN-1"},
	})
	keys, err := OpenP0Keys(ctx, pool)
	if err != nil {
		t.Fatalf("OpenP0Keys: %v", err)
	}
	// The test's database is its own (deliveryTestPool), so every key OpenP0Keys answers is one this
	// test wrote: the two open P0 issues, and neither the done P0 issue POPEN-3, the done child
	// POPEN-4 nor a P2 issue.
	if want := map[string]struct{}{"POPEN-1": {}, "POPEN-2": {}}; !maps.Equal(keys, want) {
		t.Fatalf("OpenP0Keys answers %v, want %v: the open P0 issues alone", slices.Sorted(maps.Keys(keys)), slices.Sorted(maps.Keys(want)))
	}
	plan, raw := explaintest.AnalyzedPlan(t, ctx, pool, openP0KeysQuery)
	indexScanOf(t, plan, raw, "issues_open_p0")
	indexScanOf(t, plan, raw, "issues_done_p0")
	requireDescentThroughParentKey(t, plan, raw)
	t.Logf("the open P0 keys read plans as:\n%s", raw)
}
