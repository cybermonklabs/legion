package delivery

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// seedUnownedIssueProject replaces project key's issues with issues, written directly: the P0
// rule reads issues.priority, status, claimed_by and route and nothing else. deliveryTestPool's
// database is the shared one the package's tests migrate, not a clone per test, so the test owns
// only the projects it names and clears their issues first.
func seedUnownedIssueProject(t *testing.T, ctx context.Context, pool *store.Pool, key string, issues []unownedSeed) {
	t.Helper()
	if _, err := pool.Exec(ctx, `delete from issues where project_key = $1`, key); err != nil {
		t.Fatalf("clear %s issues: %v", key, err)
	}
	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ($1, $1) on conflict do nothing`, key); err != nil {
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
	// Other packages' tests can leave issues in the shared database; the rule is checked on the
	// projects this test owns, which the query returns in project-then-number order like any other.
	ours := slices.DeleteFunc(slices.Clone(unowned), func(issue UnownedIssue) bool {
		return !slices.Contains([]string{"ACME-2", "ACME-103", "ACME-104", "ACME-105", "ACME-106", "ACME-107", "ACMEB-1"}, issue.Key)
	})
	want := []UnownedIssue{
		{Key: "ACME-2", Title: "an earlier unowned P0"},
		{Key: "ACME-103", Title: "Production deploy gate flakes"},
		{Key: "ACMEB-1", Title: "another project's unowned P0"},
	}
	if !slices.Equal(ours, want) {
		t.Fatalf("ListUnownedP0 = %+v, want %+v (open, P0, neither claimed nor routed; project then number)", ours, want)
	}

	// One project's are that project's alone, in number order.
	acme, err := ListUnownedP0(ctx, pool, "ACME")
	if err != nil {
		t.Fatalf("ListUnownedP0 of ACME: %v", err)
	}
	if want := want[:2]; !slices.Equal(acme, want) {
		t.Fatalf("ListUnownedP0 of ACME = %+v, want %+v (its open P0 issues neither claimed nor routed, by number)", acme, want)
	}
}

// Of four P0 issues, one done, one claimed, one routed and one none of these, the unowned P0
// count is 1: the rule of ListUnownedP0, which the delivery measures' P0 card and the unowned-p0
// eval answer (dispatch://INFRA-2916, R13).
func TestOfFourP0IssuesOnlyTheOneNeitherDoneClaimedNorRoutedIsUnowned(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	p0 := 0
	route := "role:dora"
	claim := `{"kind":"session","id":"01a1-test-session"}`
	seedUnownedIssueProject(t, ctx, pool, "PZERO", []unownedSeed{
		{key: "PZERO-1", number: 1, title: "done", status: "done", priority: &p0},
		{key: "PZERO-2", number: 2, title: "claimed", status: "in_progress", priority: &p0, claimedBy: &claim},
		{key: "PZERO-3", number: 3, title: "routed", status: "todo", priority: &p0, route: &route},
		{key: "PZERO-4", number: 4, title: "none of these", status: "todo", priority: &p0},
	})
	unowned, err := ListUnownedP0(ctx, pool, "PZERO")
	if err != nil {
		t.Fatalf("ListUnownedP0: %v", err)
	}
	if want := []UnownedIssue{{Key: "PZERO-4", Title: "none of these"}}; !slices.Equal(unowned, want) {
		t.Fatalf("the unowned P0 issues of done, claimed, routed and none of these = %+v, want %+v: a count of 1", unowned, want)
	}
}

// Both reads of ListUnownedP0, one project's and every project's, read issues through
// issues_open_p0 (migration 20261113082158), the partial index of the open P0 issues, with the
// claim and the route a filter over it, and never scan issues: `EXPLAIN (ANALYZE, BUFFERS)` of each
// statement as ListUnownedP0 sends it. The project read is keyed by the index's project_key; the
// read of every project is the whole of an index that holds the open P0 issues alone. The
// fixture's project holds 200 issues, one of them an unowned P0, and issues is analyzed before the
// plans: unanalyzed, the planner takes the table for a few rows, and reads one project's in number
// order through issues_project_key_number_key, filtering each, as cheaply as it reads the partial
// index and sorts, which says nothing about the statement.
func TestTheUnownedP0ReadsReadTheirPartialIndex(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	p0, p2 := 0, 2
	seeds := []unownedSeed{{key: "PINDEX-1", number: 1, title: "unowned", status: "todo", priority: &p0}}
	for number := 2; number <= 200; number++ {
		seeds = append(seeds, unownedSeed{key: "PINDEX-" + strconv.Itoa(number), number: number, title: "not a P0", status: "todo", priority: &p2})
	}
	seedUnownedIssueProject(t, ctx, pool, "PINDEX", seeds)
	if _, err := pool.Exec(ctx, `analyze issues`); err != nil {
		t.Fatalf("analyze issues: %v", err)
	}

	for _, read := range []struct {
		name      string
		statement string
		arguments []any
	}{
		{"every project's", unownedP0Query, nil},
		{"one project's", projectUnownedP0Query, []any{"PINDEX"}},
	} {
		plan, raw := analyzedPlan(t, ctx, pool, read.statement, read.arguments...)
		if explaintest.SeqScansRelation(t, plan, "issues") {
			t.Fatalf("%s unowned P0 read scans issues sequentially; want a read of issues_open_p0:\n%s", read.name, raw)
		}
		scan := indexScanOf(t, plan, raw, "issues", "issues_open_p0")
		if read.arguments != nil && !strings.Contains(scan.Cond, "project_key") {
			t.Fatalf("%s unowned P0 read's index condition %q does not key the project; want one range of issues_open_p0:\n%s", read.name, scan.Cond, raw)
		}
	}
}

// The delivery live hub's read of every open P0 issue's key (OpenP0Keys), which the hub makes each
// time it stands up its event feed, reads issues through issues_open_p0 (migration 20261113082158),
// the partial index of the open P0 issues, and never scans issues: `EXPLAIN (ANALYZE, BUFFERS)` of
// the statement as OpenP0Keys sends it. The fixture's project holds 200 issues, two open P0 issues
// and a done one among them, and issues is analyzed first, as TestTheUnownedP0ReadsReadTheirPartialIndex
// says why. The keys it answers are the two open ones.
func TestTheOpenP0KeysReadReadsTheOpenP0Index(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	p0, p2 := 0, 2
	seeds := []unownedSeed{
		{key: "POPEN-1", number: 1, title: "open", status: "todo", priority: &p0},
		{key: "POPEN-2", number: 2, title: "open and claimed", status: "in_progress", priority: &p0, claimedBy: new(`{"kind":"session","id":"ses_claimer"}`)},
		{key: "POPEN-3", number: 3, title: "done", status: "done", priority: &p0},
	}
	for number := 4; number <= 200; number++ {
		seeds = append(seeds, unownedSeed{key: "POPEN-" + strconv.Itoa(number), number: number, title: "not a P0", status: "todo", priority: &p2})
	}
	seedUnownedIssueProject(t, ctx, pool, "POPEN", seeds)
	if _, err := pool.Exec(ctx, `analyze issues`); err != nil {
		t.Fatalf("analyze issues: %v", err)
	}
	keys, err := OpenP0Keys(ctx, pool)
	if err != nil {
		t.Fatalf("OpenP0Keys: %v", err)
	}
	for _, key := range []string{"POPEN-1", "POPEN-2"} {
		if _, held := keys[key]; !held {
			t.Fatalf("OpenP0Keys answers %v, which lacks the open P0 issue %s", keys, key)
		}
	}
	if _, held := keys["POPEN-3"]; held {
		t.Fatalf("OpenP0Keys answers %v, which holds the done P0 issue POPEN-3", keys)
	}
	plan, raw := analyzedPlan(t, ctx, pool, openP0KeysQuery)
	if explaintest.SeqScansRelation(t, plan, "issues") {
		t.Fatalf("the open P0 keys read scans issues sequentially; want a read of issues_open_p0:\n%s", raw)
	}
	indexScanOf(t, plan, raw, "issues", "issues_open_p0")
	t.Logf("the open P0 keys read plans as:\n%s", raw)
}
