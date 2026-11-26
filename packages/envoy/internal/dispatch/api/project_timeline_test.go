package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// projectTimelineTarget is GET /api/v1/projects/{key}/timeline for project over [from, to), with
// extra's keys beside the window.
func projectTimelineTarget(project string, from, to time.Time, extra url.Values) string {
	query := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}}
	for key, values := range extra {
		query[key] = values
	}
	return "/api/v1/projects/" + project + "/timeline?" + query.Encode()
}

// projectTimelineBody decodes a project timeline body (a page or a view's) generically.
func projectTimelineBody(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the project timeline body: %v\n%s", err, raw)
	}
	return body
}

// projectTimelinePages reads every page of the project's timeline over [from, to) under extra's
// facets, limit rows at a time from offset 0 until a page's offset reaches the total.
func projectTimelinePages(t *testing.T, handler http.Handler, project string, from, to time.Time, extra url.Values, limit int) []map[string]any {
	t.Helper()
	var pages []map[string]any
	for offset := 0; ; offset += limit {
		query := maps.Clone(extra)
		if query == nil {
			query = url.Values{}
		}
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(offset))
		page := projectTimelineBody(t, getBody(t, handler, projectTimelineTarget(project, from, to, query)))
		pages = append(pages, page)
		if offset+limit >= int(page["total"].(float64)) {
			return pages
		}
	}
}

// projectTimelineRows is every row of pages, in order.
func projectTimelineRows(pages []map[string]any) []map[string]any {
	var rows []map[string]any
	for _, page := range pages {
		for _, item := range page["items"].([]any) {
			rows = append(rows, item.(map[string]any))
		}
	}
	return rows
}

// projectTimelineRowKey names a row by its kind and its id.
func projectTimelineRowKey(t *testing.T, row map[string]any) string {
	t.Helper()
	return row["kind"].(string) + " " + liveID(t, row)
}

// projectTimelineRowTime is a row's time: a merge's merged_at, a run's created_at.
func projectTimelineRowTime(t *testing.T, row map[string]any) time.Time {
	t.Helper()
	field := "created_at"
	if row["kind"] == projectTimelineMerge {
		field = "merged_at"
	}
	at, err := time.Parse(time.RFC3339Nano, row[field].(string))
	if err != nil {
		t.Fatalf("row %v: %s: %v", row, field, err)
	}
	return at
}

// compareProjectTimelineRows is the timeline's order over decoded rows, as the route states it:
// newest first, then merge, deploy and failed_run, then id (a merge's in byte order, a run's by
// number).
func compareProjectTimelineRows(t *testing.T, a, b map[string]any) int {
	t.Helper()
	if order := projectTimelineRowTime(t, b).Compare(projectTimelineRowTime(t, a)); order != 0 {
		return order
	}
	if order := slices.Index(projectTimelineKinds, a["kind"].(string)) - slices.Index(projectTimelineKinds, b["kind"].(string)); order != 0 {
		return order
	}
	if a["kind"] == projectTimelineMerge {
		return strings.Compare(a["id"].(string), b["id"].(string))
	}
	return int(a["id"].(float64) - b["id"].(float64))
}

// withoutKeys is row without keys.
func withoutKeys(row map[string]any, keys ...string) map[string]any {
	left := maps.Clone(row)
	for _, key := range keys {
		delete(left, key)
	}
	return left
}

// listByID is a decoded list keyed by each item's id.
func listByID(t *testing.T, list any) map[string]map[string]any {
	t.Helper()
	byID := map[string]map[string]any{}
	items, _ := list.([]any)
	for _, item := range items {
		byID[liveID(t, item)] = item.(map[string]any)
	}
	return byID
}

// fixtureShips is, for each deploy run of the fixture, the pull requests it ships first by the
// fixture's own containment (delivery.ContainingRun over every pull request and every apply).
func fixtureShips(fixture deliverytest.Fixture) map[int64][]delivery.DeliveryPullRequest {
	ships := map[int64][]delivery.DeliveryPullRequest{}
	for _, pr := range fixture.PullRequests {
		if apply := delivery.ContainingRun(pr, fixture.DeployRepo, fixture.Applies); apply != nil {
			ships[apply.RunID] = append(ships[apply.RunID], pr)
		}
	}
	return ships
}

// checkProjectTimelinePages holds pages, read limit at a time, to the envelope and the total order:
// each page names the total, its limit and its offset; no row is on two pages; each page's rows
// are in the order, and a page's oldest row comes before the next page's newest; waiting is on
// the last page alone, the labels on the first alone, and the counts, window and freshness on
// every page, equal to want's.
func checkProjectTimelinePages(t *testing.T, pages []map[string]any, limit int, want map[string]any) {
	t.Helper()
	total := int(pages[0]["total"].(float64))
	seen := map[string]bool{}
	var previous map[string]any
	for n, page := range pages {
		if _, measured := page["measures"]; measured {
			t.Errorf("page %d carries measures", n)
		}
		if got := int(page["total"].(float64)); got != total {
			t.Errorf("page %d counts %d rows, the first %d", n, got, total)
		}
		if int(page["limit"].(float64)) != limit || int(page["offset"].(float64)) != n*limit {
			t.Errorf("page %d names limit %v and offset %v, want %d and %d", n, page["limit"], page["offset"], limit, n*limit)
		}
		items := page["items"].([]any)
		if wantLen := min(limit, total-n*limit); len(items) != wantLen {
			t.Errorf("page %d holds %d rows, want %d", n, len(items), wantLen)
		}
		for _, item := range items {
			row := item.(map[string]any)
			key := projectTimelineRowKey(t, row)
			if seen[key] {
				t.Errorf("row %s is on two pages", key)
			}
			seen[key] = true
			if previous != nil && compareProjectTimelineRows(t, previous, row) >= 0 {
				t.Errorf("row %s comes after %s, against the order (newest first, then kind, then id)", key, projectTimelineRowKey(t, previous))
			}
			previous = row
		}
		last := n == len(pages)-1
		if waiting := page["waiting"]; (waiting != nil) != last {
			t.Errorf("page %d of %d: waiting = %v, want it on the last page alone", n, len(pages), waiting)
		} else if last && !reflect.DeepEqual(waiting, want["waiting"]) {
			t.Errorf("the last page's waiting = %v, want %v", waiting, want["waiting"])
		}
		for _, name := range []string{"issue_titles", "session_titles", "session_pr_counts", "components", "session_groups"} {
			switch {
			case n == 0 && !reflect.DeepEqual(page[name], want[name]):
				t.Errorf("the first page's %s = %v, want %v", name, page[name], want[name])
			case n > 0 && page[name] != nil:
				t.Errorf("page %d carries %s = %v, want it on the first page alone", n, name, page[name])
			}
		}
		for _, name := range []string{"facet_counts", "color_counts", "window", "freshness"} {
			if !reflect.DeepEqual(page[name], want[name]) {
				t.Errorf("page %d's %s = %v, want %v", n, name, page[name], want[name])
			}
		}
	}
	if len(seen) != total {
		t.Errorf("the pages hold %d rows, their total %d", len(seen), total)
	}
}

// TestTheProjectTimelinePagesNewestFirstAndHoldsTheBaseAnswer: on a one-project history with one
// deploy repository (deliverytest's OneProject: every pull request names an issue of DELIV, whose
// deploy source reads the deploy repository), the project timeline read 50 rows at a time over
// eight days is the base timeline's answer for the same window (GET /api/v1/delivery/timeline,
// deliveryTimelineCompute), every field but measures: its merges are the base's prs[] each with its
// kind, its runs the base's runs[] each with its kind, its repository and its creation, its waiting
// line, counts, labels, window and freshness the base's; no row is on two pages, each page's oldest
// row comes before the next page's newest, and waiting is on the last page alone. The same holds
// with a facet set, which narrows the merges and leaves the runs.
//
// The runs are the base's that count for the project by root R27, which here are every one of
// them: the deploys that ship one of its pull requests, and those that ship none, every failed run
// among them (no failed run ships), which count for DELIV as the project whose deploy source reads
// their repository (INFRA-978 R12). That source names no repository and reads DELIV's default, as
// the refresher publishes it, which the test publishes as the refresher's page does.
func TestTheProjectTimelinePagesNewestFirstAndHoldsTheBaseAnswer(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2918, Days: 30, OneProject: true})
	// Every pull request counts for the project of the issue its row names, the oracle below.
	leaveAttributionInputsUnread(t, database.Pool)
	publishDefaultRepos(t, database.Pool)
	to := fixture.To.AddDate(0, 0, -1)
	from := to.AddDate(0, 0, -8)
	ships := fixtureShips(fixture)

	for _, facets := range []url.Values{{}, {"author": {"dev-ana"}}} {
		t.Run("facets="+facets.Encode(), func(t *testing.T) {
			baseQuery := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}}
			for key, values := range facets {
				baseQuery[key] = values
			}
			base := projectTimelineBody(t, getBody(t, handler, "/api/v1/delivery/timeline?"+baseQuery.Encode()))
			pages := projectTimelinePages(t, handler, fixture.Project, from, to, facets, 50)
			// A facet narrows the merges alone, so the faceted pass holds fewer rows; both cross at
			// least one page's edge.
			if len(pages) < 2 {
				t.Fatalf("the window holds %v rows, %d page of 50; want at least two pages", pages[0]["total"], len(pages))
			}
			checkProjectTimelinePages(t, pages, 50, base)

			merges, runs := map[string]map[string]any{}, map[string]map[string]any{}
			kinds := map[string]int{}
			for _, row := range projectTimelineRows(pages) {
				kinds[row["kind"].(string)]++
				if row["kind"] == projectTimelineMerge {
					merges[liveID(t, row)] = withoutKeys(row, "kind")
					continue
				}
				runs[liveID(t, row)] = withoutKeys(row, "kind", "repo", "created_at")
			}
			if want := listByID(t, base["prs"]); !reflect.DeepEqual(merges, want) {
				t.Errorf("the pages' merges differ from the base's prs[]: %d merges, %d prs", len(merges), len(want))
			}
			if want := listByID(t, base["runs"]); !reflect.DeepEqual(runs, want) {
				t.Errorf("the pages' runs differ from the base's runs[]: %d runs, %d in the base", len(runs), len(want))
			}
			shipNone := 0
			for id := range runs {
				if runID, _ := strconv.ParseInt(id, 10, 64); len(ships[runID]) == 0 {
					shipNone++
				}
			}
			if kinds[projectTimelineMerge] == 0 || kinds[projectTimelineDeploy] == 0 || shipNone == 0 {
				t.Fatalf("the window's rows by kind are %v with %d runs shipping none, want merges, deploys and runs shipping none", kinds, shipNone)
			}
			t.Logf("%s..%s: %v rows over %d pages (%v), %d of the runs shipping no pull request",
				from.Format(time.DateOnly), to.Format(time.DateOnly), pages[0]["total"], len(pages), kinds, shipNone)
		})
	}
}

// TestTheProjectTimelineCountsTheProjectsRowsByR27: in deliverytest's default history, whose pull
// requests name issues of DELIV, one in twenty of DELIVX, and some none, and whose deploy
// repository is DELIV's default repository and the one its deploy source reads, over seven whole
// UTC days holding a deploy that ships pull requests of both, with the default repositories
// published as the refresher publishes them: a merge with an issue is on its issue's project's
// timeline alone, and one with no issue on that of each project whose default repository holds it
// (INFRA-2916 R15), DELIV's for the deploy repository's and none for another repository's; each
// project's merges are the pull requests its pr-open-to-merge day rows count for the window, and
// those its pr-open-to-merge items list, DELIVX's too, which has no deploy source of its own; a deploy
// is on the timeline of each project whose pull request it ships first, whenever that merged, the
// one shipping both on both, and on DELIV's where it ships one with no issue; a deploy that ships
// no pull request, and a failed run, are on the timeline of DELIV alone, whose deploy source reads
// their repository (INFRA-978 R12); and a deploy in DELIV's repository that ships none of DELIVX's
// pull requests is not on DELIVX's.
func TestTheProjectTimelineCountsTheProjectsRowsByR27(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Seed: 2916, Days: 40})
	// Each pull request counts for the project of the issue its row names (projectOf, below).
	leaveAttributionInputsUnread(t, database.Pool)
	projects := []string{fixture.Project, fixture.SecondProject}
	issueProject := map[string]string{}
	for project, keys := range fixture.Issues {
		for _, key := range keys {
			issueProject[key] = project
		}
	}
	projectOf := func(pr delivery.DeliveryPullRequest) string {
		if pr.IssueKey == nil {
			return ""
		}
		return issueProject[*pr.IssueKey]
	}
	ships := fixtureShips(fixture)
	runsByID := map[int64]delivery.DeliveryRun{}
	for _, run := range fixture.Runs {
		runsByID[run.RunID] = run
	}
	shipsFor := func(runID int64, project string) bool {
		return slices.ContainsFunc(ships[runID], func(pr delivery.DeliveryPullRequest) bool { return projectOf(pr) == project })
	}
	var target int64
	for _, apply := range fixture.Applies {
		if started := runsByID[apply.RunID].StartedAt; shipsFor(apply.RunID, fixture.Project) && shipsFor(apply.RunID, fixture.SecondProject) &&
			started.After(fixture.From.AddDate(0, 0, 2)) && started.Before(fixture.To.AddDate(0, 0, -8)) {
			target = apply.RunID
			break
		}
	}
	if target == 0 {
		t.Fatalf("the history holds no deploy shipping pull requests of both %s and %s", fixture.Project, fixture.SecondProject)
	}
	from := runsByID[target].StartedAt.UTC().Truncate(24 * time.Hour)
	to := from.AddDate(0, 0, 7)

	// The refresher writes the day rows each project's pr-open-to-merge items are read from, and
	// its page publishes the projects' default repositories, which a row with no issue counts under.
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	for _, project := range projects {
		seededEntries(t, registry, project)
	}
	refreshEvalDays(t, registry, database, fixture.To.Add(-time.Minute))

	// What each project counts in the window, by the fixture's own rows: its merges by issue, and
	// DELIV the deploy repository's merges with no issue; the deploy runs on main GitHub created and
	// started in it that ship one of its pull requests, and DELIV those that ship one with no issue,
	// every one of them being of the deploy repository, or ship none.
	wantMerges := map[string][]string{}
	var noIssue []string
	owned := 0
	for _, pr := range fixture.PullRequests {
		if pr.MergedAt.Before(from) || !pr.MergedAt.Before(to) {
			continue
		}
		id := pr.Repo + "#" + strconv.Itoa(pr.Number)
		project := projectOf(pr)
		if project == "" {
			noIssue = append(noIssue, id)
			if pr.Repo != fixture.DeployRepo {
				continue
			}
			project, owned = fixture.Project, owned+1
		}
		wantMerges[project] = append(wantMerges[project], id)
	}
	wantRuns := map[string][]string{}
	var shipNone []string
	for _, run := range fixture.Runs {
		if run.Kind != delivery.DeliveryRunKindDeploy || run.Created().Before(from) || run.StartedAt.Before(from) || !run.StartedAt.Before(to) {
			continue
		}
		id := strconv.FormatInt(run.RunID, 10)
		none := len(ships[run.RunID]) == 0
		if none {
			shipNone = append(shipNone, id)
		}
		for _, project := range projects {
			if shipsFor(run.RunID, project) || project == fixture.Project && (none || shipsFor(run.RunID, "")) {
				wantRuns[project] = append(wantRuns[project], id)
			}
		}
	}

	gotMerges, gotRuns := map[string][]string{}, map[string][]string{}
	for _, project := range projects {
		for _, row := range projectTimelineRows(projectTimelinePages(t, handler, project, from, to, nil, projectTimelinePageMax)) {
			if row["kind"] == projectTimelineMerge {
				gotMerges[project] = append(gotMerges[project], liveID(t, row))
			} else {
				gotRuns[project] = append(gotRuns[project], liveID(t, row))
			}
		}
		slices.Sort(gotMerges[project])
		slices.Sort(gotRuns[project])
		slices.Sort(wantMerges[project])
		slices.Sort(wantRuns[project])
	}

	for _, project := range projects {
		if !slices.Equal(gotMerges[project], wantMerges[project]) {
			t.Errorf("%s's merges = %v, want the %d pull requests of the window it counts: %v", project, gotMerges[project], len(wantMerges[project]), wantMerges[project])
		}
		if !slices.Equal(gotRuns[project], wantRuns[project]) {
			t.Errorf("%s's runs = %v, want the %d deploys of the window it counts: %v", project, gotRuns[project], len(wantRuns[project]), wantRuns[project])
		}
	}
	targetID := strconv.FormatInt(target, 10)
	if !slices.Contains(gotRuns[fixture.Project], targetID) || !slices.Contains(gotRuns[fixture.SecondProject], targetID) {
		t.Errorf("run %d ships pull requests of both projects, and is on %s's timeline %t and %s's %t, want both", target,
			fixture.Project, slices.Contains(gotRuns[fixture.Project], targetID), fixture.SecondProject, slices.Contains(gotRuns[fixture.SecondProject], targetID))
	}
	// A deploy in DELIV's repository, the deploy repository, that ships none of DELIVX's pull
	// requests is on DELIV's timeline and not on DELIVX's.
	onlyFirst := 0
	for _, id := range gotRuns[fixture.Project] {
		if !slices.Contains(gotRuns[fixture.SecondProject], id) {
			onlyFirst++
		}
	}
	if onlyFirst == 0 || owned == 0 || len(noIssue) == owned || len(shipNone) == 0 {
		t.Fatalf("the window holds %d deploys of %s alone, %d merges naming no issue, %d of them of the deploy repository, and %d deploys shipping none; want some of each, and some merges naming no issue of another repository",
			onlyFirst, fixture.Project, len(noIssue), owned, len(shipNone))
	}

	// Each project's merges are the pull requests its pr-open-to-merge day rows count for the window:
	// the members the refresher wrote with the rows of the evals no deploy repository moves
	// (evals.CountedIn), its own and those of the owner keys it reads; and the pull requests its
	// pr-open-to-merge items list, DELIVX's too, though it has no source: the items read reads each
	// pull request of the window and each deploy repository under that repository's own rule.
	for _, project := range projects {
		members, err := evals.CountedIn(t.Context(), database.Pool, project, evals.KindDelivery, []string{""}, nil, evals.Range{From: from, To: to})
		if err != nil {
			t.Fatalf("read the pull requests %s's day rows count: %v", project, err)
		}
		var counted []string
		for _, member := range members {
			if id, found := strings.CutPrefix(member.RowKey, "pull_request:"); found {
				counted = append(counted, id)
			}
		}
		slices.Sort(counted)
		counted = slices.Compact(counted)
		if !slices.Equal(gotMerges[project], counted) {
			t.Errorf("%s's merges = %v, want the %d pull requests its pr-open-to-merge day rows count: %v", project, gotMerges[project], len(counted), counted)
		}
		var listed []string
		for offset := 0; ; offset += 200 {
			query := url.Values{
				"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)},
				"row_kind": {"pull_request"}, "limit": {"200"}, "offset": {strconv.Itoa(offset)},
			}
			page := decodeBody[evalItemsResponse](t, dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/projects/"+project+"/evals/"+evals.SlugPROpenToMerge+"/items?"+query.Encode(), nil, "alice"))
			for _, item := range page.Items {
				listed = append(listed, item.Cols["repo"].(string)+"#"+strconv.Itoa(int(item.Cols["number"].(float64))))
			}
			if offset+200 >= page.Total {
				break
			}
		}
		slices.Sort(listed)
		if !slices.Equal(gotMerges[project], listed) {
			t.Errorf("%s's merges = %v, want the %d pull requests its pr-open-to-merge items list: %v", project, gotMerges[project], len(listed), listed)
		}
	}
	t.Logf("%s..%s: %s holds %d merges and %d runs, %s %d and %d; run %d ships both; %d merges name no issue, %d of the deploy repository; %d deploys ship none",
		from.Format(time.DateOnly), to.Format(time.DateOnly), fixture.Project, len(gotMerges[fixture.Project]), len(gotRuns[fixture.Project]),
		fixture.SecondProject, len(gotMerges[fixture.SecondProject]), len(gotRuns[fixture.SecondProject]), target, len(noIssue), owned, len(shipNone))

	// DELIV's default repository is the deploy repository (deliverytest), which the refresher
	// published above, so the deploy repository's merges with no issue are DELIV's, and so is each
	// deploy that ships one of them.
	t.Run("a merge with no issue counts for each project whose default repository holds it", func(t *testing.T) {
		for _, pr := range fixture.PullRequests {
			if pr.IssueKey != nil || pr.MergedAt.Before(from) || !pr.MergedAt.Before(to) {
				continue
			}
			id := pr.Repo + "#" + strconv.Itoa(pr.Number)
			if on := slices.Contains(gotMerges[fixture.Project], id); on != (pr.Repo == fixture.DeployRepo) {
				t.Errorf("%s names no issue and is on %s's timeline %t, want %t", id, fixture.Project, on, pr.Repo == fixture.DeployRepo)
			}
			if slices.Contains(gotMerges[fixture.SecondProject], id) {
				t.Errorf("%s names no issue and is on %s's timeline, whose default repository holds none", id, fixture.SecondProject)
			}
		}
	})
	t.Run("a deploy shipping only pull requests with no issue counts for the project whose default repository holds them", func(t *testing.T) {
		for runID, shipped := range ships {
			run := runsByID[runID]
			if run.Created().Before(from) || run.StartedAt.Before(from) || !run.StartedAt.Before(to) ||
				slices.ContainsFunc(shipped, func(pr delivery.DeliveryPullRequest) bool { return pr.IssueKey != nil }) {
				continue
			}
			if id := strconv.FormatInt(runID, 10); !slices.Contains(gotRuns[fixture.Project], id) {
				t.Errorf("run %s ships only pull requests of the deploy repository with no issue, and is not on %s's timeline", id, fixture.Project)
			}
		}
	})
	// The deploy repository is the one DELIV's deploy source reads (deliverytest.SaveRules).
	t.Run("a deploy that ships no pull request counts for the project whose deploy source reads its repository", func(t *testing.T) {
		for _, id := range shipNone {
			if !slices.Contains(gotRuns[fixture.Project], id) || slices.Contains(gotRuns[fixture.SecondProject], id) {
				t.Errorf("run %s ships no pull request, and is on %s's timeline %t and %s's %t, want %s's alone", id,
					fixture.Project, slices.Contains(gotRuns[fixture.Project], id), fixture.SecondProject, slices.Contains(gotRuns[fixture.SecondProject], id), fixture.Project)
			}
		}
	})
	t.Run("a failed run counts for the project whose deploy source reads its repository", func(t *testing.T) {
		for _, run := range fixture.Runs {
			if run.Kind != delivery.DeliveryRunKindDeploy || run.Conclusion == nil || *run.Conclusion != delivery.DeliveryRunConclusionFailure ||
				run.Created().Before(from) || run.StartedAt.Before(from) || !run.StartedAt.Before(to) {
				continue
			}
			if id := strconv.FormatInt(run.RunID, 10); !slices.Contains(gotRuns[fixture.Project], id) {
				t.Errorf("failed run %s is not on %s's timeline", id, fixture.Project)
			}
		}
	})
}

// timelineRigRun writes deploy run id of acme/widgets on main, created at created and started a
// minute later, with jobs, each named with its conclusion and finished in order a few minutes
// apart.
func timelineRigRun(t *testing.T, database *store.Store, id int64, created time.Time, jobs [][2]string) {
	t.Helper()
	ctx := context.Background()
	started := created.Add(time.Minute)
	completed := started.Add(time.Duration(len(jobs)*5) * time.Minute)
	conclusion := delivery.DeliveryRunConclusionSuccess
	written := make([]delivery.DeliveryRunJob, 0, len(jobs))
	for i, job := range jobs {
		begun, done := started.Add(time.Duration(i*5)*time.Minute), started.Add(time.Duration(i*5+4)*time.Minute)
		result := delivery.DeliveryJobConclusion(job[1])
		if result != delivery.DeliveryJobConclusionSuccess {
			conclusion = delivery.DeliveryRunConclusionFailure
		}
		written = append(written, delivery.DeliveryRunJob{Repo: "acme/widgets", RunID: id, Name: job[0], StartedAt: &begun, CompletedAt: &done, Conclusion: &result})
	}
	if err := delivery.UpsertRun(ctx, database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(id, 10),
		HeadCommitAt: created.Add(-time.Minute), StartedAt: started, CreatedAt: &created, CompletedAt: &completed, Conclusion: &conclusion,
		URL: "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(id, 10), HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("upsert deploy run %d: %v", id, err)
	}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, "acme/widgets", id, written, true); err != nil {
		t.Fatalf("upsert deploy run %d's jobs: %v", id, err)
	}
}

// timelineRig is a small delivery history of project HUB in the hub tests' deploy repository
// (seedHubSettings): two pull requests naming HUB issues and one naming none, a deploy that ships
// the first, a run whose production job ships the second and whose smoke job then fails (a failed
// run), and a deploy that ships nothing. It answers the window holding them all.
func timelineRig(t *testing.T, handler http.Handler, database *store.Store) (from, to time.Time) {
	t.Helper()
	seedHubSettings(t, database)
	first := createHubIssue(t, handler, map[string]any{"title": "Ship widgets"})
	second := createHubIssue(t, handler, map[string]any{"title": "Ship gadgets"})
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	upsertPR(t, database, prNaming(1, at, first))
	upsertPR(t, database, hubPR(2, at.Add(10*time.Minute)))
	timelineRigRun(t, database, 41, at.Add(time.Hour), [][2]string{{"release / release", "success"}})
	upsertPR(t, database, prNaming(3, at.Add(2*time.Hour), second))
	timelineRigRun(t, database, 42, at.Add(3*time.Hour), [][2]string{{"release / release", "success"}, {"smoke / run", "failure"}})
	timelineRigRun(t, database, 43, at.Add(5*time.Hour), [][2]string{{"release / release", "success"}})
	return at.Add(-24 * time.Hour), at.Add(24 * time.Hour)
}

// TestTheProjectTimelineAnswersNoMeasures: no type the route or the project-timeline view answers
// holds a measures member or a type of delivery/measures, and neither body carries one, where the
// delivery timeline's body over the same window does.
func TestTheProjectTimelineAnswersNoMeasures(t *testing.T) {
	var walk func(typ reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(typ reflect.Type, path string, seen map[reflect.Type]bool) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if strings.HasSuffix(typ.PkgPath(), "/delivery/measures") {
			t.Errorf("%s is %s, a type of delivery/measures", path, typ)
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		shapes := []reflect.Type{typ}
		if union, ok := reflect.Zero(typ).Interface().(interface{ JSONSchemaOneOf() []any }); ok {
			shapes = shapes[:0]
			for _, shape := range union.JSONSchemaOneOf() {
				shapes = append(shapes, reflect.TypeOf(shape))
			}
		}
		for _, shape := range shapes {
			for _, field := range liveJSONFields(shape) {
				if field.name == "measures" {
					t.Errorf("%s.%s is a measures member", path, field.name)
				}
				walk(field.typ, path+"."+field.name, seen)
			}
		}
	}
	walk(deliveryLiveKinds[deliveryLiveProjectTimeline].body, "project-timeline", map[reflect.Type]bool{})
	walk(reflect.TypeOf(routeShapes["GET /api/v1/projects/{key}/timeline"].Response), "GET /api/v1/projects/{key}/timeline", map[reflect.Type]bool{})

	rig := newHubRig(t, nil)
	from, to := timelineRig(t, rig.handler, rig.database)
	window := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}}
	if base := projectTimelineBody(t, getBody(t, rig.handler, "/api/v1/delivery/timeline?"+window.Encode())); base["measures"] == nil {
		t.Fatal("the delivery timeline's body carries no measures, so this test cannot see one")
	}
	page := projectTimelineBody(t, getBody(t, rig.handler, projectTimelineTarget("HUB", from, to, nil)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", projectTimelineRange("HUB", from, to, "")))
	snapshot := projectTimelineBody(t, conn.snapshot(t, "t").Body)
	for what, body := range map[string]map[string]any{"the route's page": page, "the view's snapshot": snapshot} {
		if _, measured := body["measures"]; measured {
			t.Errorf("%s carries measures", what)
		}
		items, _ := body["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("%s holds no row", what)
		}
		for _, item := range items {
			if _, measured := item.(map[string]any)["measures"]; measured {
				t.Errorf("a row of %s carries measures: %v", what, item)
			}
		}
	}
}

// TestTheProjectTimelineRowsNameTheirRunsRepository: every deploy and failed_run row names the
// repository its run is in and when GitHub created it, which order it among the rows, and a run
// whose smoke job failed after its production job shipped a pull request is a failed_run, with
// that job among its failed jobs; a merge names its pull request's repository, as the base's
// prs[] does. A run that ships none of HUB's pull requests is on no row.
func TestTheProjectTimelineRowsNameTheirRunsRepository(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	from, to := timelineRig(t, handler, database)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	page := projectTimelineBody(t, getBody(t, handler, projectTimelineTarget("HUB", from, to, nil)))
	got := map[string]map[string]any{}
	for _, item := range page["items"].([]any) {
		row := item.(map[string]any)
		got[projectTimelineRowKey(t, row)] = row
	}
	want := map[string]time.Time{"deploy 41": at.Add(time.Hour), "failed_run 42": at.Add(3 * time.Hour)}
	for key, created := range want {
		row := got[key]
		if row == nil {
			t.Errorf("no row %s among %v", key, slices.Sorted(maps.Keys(got)))
			continue
		}
		if row["repo"] != "acme/widgets" {
			t.Errorf("row %s names repo %v, want acme/widgets, the repository its run is in", key, row["repo"])
		}
		if got := projectTimelineRowTime(t, row); !got.Equal(created) {
			t.Errorf("row %s names created_at %v, want %s", key, row["created_at"], created.Format(time.RFC3339))
		}
	}
	if failed, _ := got["failed_run 42"]["failed_jobs"].([]any); len(failed) != 1 || failed[0].(map[string]any)["name"] != "smoke / run" {
		t.Errorf("run 42's failed_jobs = %v, want its smoke job", got["failed_run 42"]["failed_jobs"])
	}
	for _, key := range []string{"merge acme/widgets#1", "merge acme/widgets#3"} {
		if row := got[key]; row == nil || row["repo"] != "acme/widgets" {
			t.Errorf("merge row %s = %v, want it naming acme/widgets", key, row)
		}
	}
	if len(got) != 4 {
		t.Errorf("rows %v, want the two merges of HUB and the two runs shipping them; #2 names no issue and run 43 ships nothing", slices.Sorted(maps.Keys(got)))
	}
}

// projectTimelineRange is a project-timeline view of project over [from, to) with query.
func projectTimelineRange(project string, from, to time.Time, query string) DeliveryLiveView {
	view := timelineRange(from, to, query)
	view.Kind, view.Project = deliveryLiveProjectTimeline, &project
	return view
}

// TestTheProjectTimelineViewParsesAsItsRoute: a project-timeline view is refused what its route
// refuses, by the same code and words (a key that is no project key, a key named twice, a bad
// limit, a window the route refuses), takes the limits past the route's page it grows to, refuses
// an offset, which its route takes, and a project no row holds is told what the route answers;
// and the view's snapshot, under facets and a search, is the route's body for the same query.
func TestTheProjectTimelineViewParsesAsItsRoute(t *testing.T) {
	rig := newHubRig(t, nil)
	from, to := timelineRig(t, rig.handler, rig.database)
	for _, refused := range []struct {
		name, project, query string
		from, to             time.Time
	}{
		{name: "a key that is no project key", project: "hub", query: ""},
		{name: "q named twice", project: "HUB", query: "q=a&q=b"},
		{name: "limit named twice", project: "HUB", query: "limit=1&limit=2"},
		{name: "an inverted window", project: "HUB", query: "", from: to, to: from},
		{name: "a window past 84 days", project: "HUB", query: "", from: from.AddDate(0, 0, -90), to: to},
	} {
		t.Run(refused.name, func(t *testing.T) {
			windowFrom, windowTo := from, to
			if !refused.from.IsZero() {
				windowFrom, windowTo = refused.from, refused.to
			}
			extra, _ := url.ParseQuery(refused.query)
			response := dispatchRequest(t, rig.handler, http.MethodGet, projectTimelineTarget(refused.project, windowFrom, windowTo, extra), nil, "alice")
			body := decodeBody[errorBody](t, response)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("the route answers %d %s, want 400", response.Code, response.Body.String())
			}
			_, refusal := parseDeliveryLiveView(projectTimelineRange(refused.project, windowFrom, windowTo, refused.query))
			if refusal == nil {
				t.Fatalf("the view is held, where the route answers %s %s", body.Code, body.Error)
			}
			if refusal.Code != body.Code || refusal.Message != body.Error {
				t.Errorf("the view is refused %s %q, the route %s %q", refusal.Code, refusal.Message, body.Code, body.Error)
			}
		})
	}

	// The page's bounds are the route's and the view's own: each takes a limit from 1 to its
	// bound and refuses any other, INVALID_QUERY naming that bound, the view's past the route's
	// page, up to the most rows a window holds.
	routeBound, viewBound := strconv.Itoa(projectTimelinePageMax), strconv.Itoa(projectTimelineViewMax)
	for _, limit := range []string{"0", "many", "1", "500", "501", "100000", "100001"} {
		response := dispatchRequest(t, rig.handler, http.MethodGet, projectTimelineTarget("HUB", from, to, url.Values{"limit": {limit}}), nil, "alice")
		_, refusal := parseDeliveryLiveView(projectTimelineRange("HUB", from, to, "limit="+limit))
		n, err := strconv.Atoi(limit)
		routeTakes, viewTakes := err == nil && n >= 1 && n <= projectTimelinePageMax, err == nil && n >= 1 && n <= projectTimelineViewMax
		if took := response.Code == http.StatusOK; took != routeTakes {
			t.Errorf("limit %s: the route answers %d %s, want it taken %t", limit, response.Code, response.Body.String(), routeTakes)
		} else if !took {
			if body := decodeBody[errorBody](t, response); body.Code != "INVALID_QUERY" || !strings.Contains(body.Error, routeBound) {
				t.Errorf("limit %s: the route refuses %s %q, want INVALID_QUERY naming %s", limit, body.Code, body.Error, routeBound)
			}
		}
		if (refusal == nil) != viewTakes {
			t.Errorf("limit %s: the view is refused %+v, want it taken %t", limit, refusal, viewTakes)
		} else if refusal != nil && (refusal.Code != "INVALID_QUERY" || !strings.Contains(refusal.Message, viewBound)) {
			t.Errorf("limit %s: the view is refused %s %q, want INVALID_QUERY naming %s", limit, refusal.Code, refusal.Message, viewBound)
		}
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, projectTimelineTarget("HUB", from, to, url.Values{"offset": {"1"}}), nil, "alice"); response.Code != http.StatusOK {
		t.Errorf("the route refuses an offset: %d %s", response.Code, response.Body.String())
	}
	if _, refusal := parseDeliveryLiveView(projectTimelineRange("HUB", from, to, "offset=1")); refusal == nil || refusal.Code != "INVALID_QUERY" {
		t.Errorf("the view takes an offset (%+v), want INVALID_QUERY: it grows by its limit", refusal)
	}

	conn := newFakeLiveConn()
	missing := dispatchRequest(t, rig.handler, http.MethodGet, projectTimelineTarget("NOPE", from, to, nil), nil, "alice")
	rig.hub.Subscribe(conn, subscribeFrame("missing", projectTimelineRange("NOPE", from, to, "")))
	if refusal, body := conn.refusal(t, "missing", "PROJECT_NOT_FOUND"), decodeBody[errorBody](t, missing); missing.Code != http.StatusNotFound || body.Code != refusal.Code || body.Error != refusal.Message {
		t.Errorf("a project no row holds: the route answers %d %s %q, the view %s %q", missing.Code, body.Code, body.Error, refusal.Code, refusal.Message)
	}

	for _, query := range []string{"limit=3", "author=octocat&repo=acme/widgets&limit=2", "q=change", "issue=__no_issue__"} {
		extra, _ := url.ParseQuery(query)
		route := getBody(t, rig.handler, projectTimelineTarget("HUB", from, to, extra))
		rig.hub.Subscribe(conn, subscribeFrame("q", projectTimelineRange("HUB", from, to, query)))
		var snapshot DeliveryLiveSnapshot
		for {
			frame := conn.next(t, "the snapshot of "+query)
			if got, ok := frame.(DeliveryLiveSnapshot); ok {
				snapshot = got
				break
			}
		}
		if got, want := projectTimelineBody(t, snapshot.Body), projectTimelineBody(t, route); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the view's snapshot differs from the route's body:\nview:  %s\nroute: %s", query, snapshot.Body, route)
		}
		rig.hub.Unsubscribe(conn, "q")
	}
}

// TestTheDefaultsPublishingMarksTheProjectTimeline: a project timeline counts a row no issue
// attributes, and a deploy that ships nothing, under the published default repositories
// (evals.Attribution), so the refresher's commit that publishes them marks its views: the commit's
// change for a project whose default it moved marks that project's timeline views and no other
// project's, and its change for a deploy repository whose judging project it moved marks every
// timeline view. The settings applied, and an eval's day rows, readings or versions, mark none.
func TestTheDefaultsPublishingMarksTheProjectTimeline(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	today := now.Truncate(24 * time.Hour)
	views := map[string]*deliveryView{}
	for _, project := range []string{"DELIV", "OTHER"} {
		views[project] = viewShowingIssues(deliveryLiveSpec{kind: deliveryLiveProjectTimeline, project: project, preset: 7}, nil)
	}
	// published is the change the refresher's commit that publishes the defaults logs for repo,
	// with ref (evals.publishDefaults).
	published := func(repo, ref string) delivery.Change {
		return delivery.Change{Seq: 1, Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenByDefaults, Repo: repo, Ref: ref}
	}
	cases := []struct {
		what   string
		change delivery.Change
		marks  []string
	}{
		{"DELIV's default published", published("DELIV", ""), []string{"DELIV"}},
		{"the judging project of acme/widgets moved", published("acme/widgets", delivery.DefaultsJudgingRef), []string{"DELIV", "OTHER"}},
		{"the settings applied", delivery.Change{Seq: 1, Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenBySettingsRewrite}, nil},
		{"DELIV's merge-to-production day rows", evalsChange("eval_days", "DELIV", evals.SlugMergeToProduction, &today, &today), nil},
		{"a version of DELIV's merge-to-production", evalsChange("eval_versions", "DELIV", evals.SlugMergeToProduction, nil, nil), nil},
		{"a reading of DELIV's pr-ci", evalsChange("eval_readings", "DELIV", evals.SlugPRCI, &today, &today), nil},
	}
	for _, c := range cases {
		for project, view := range views {
			if got, want := view.touchedBy(c.change, now), slices.Contains(c.marks, project); got != want {
				t.Errorf("%s: touchedBy(%s's timeline) = %t, want %t", c.what, project, got, want)
			}
		}
	}
}

// nextProjectTimelineFrame is the next snapshot or delta conn is sent for sub, past the freshness
// verdicts that follow a subscription's first answer.
func nextProjectTimelineFrame(t *testing.T, conn *fakeLiveConn, sub string) any {
	t.Helper()
	for {
		switch frame := conn.next(t, "a frame of "+sub).(type) {
		case DeliveryLiveFreshness:
		case DeliveryLiveSnapshot:
			if frame.Sub == sub {
				return frame
			}
		case DeliveryLiveDelta:
			if frame.Sub == sub {
				return frame
			}
		default:
			raw, _ := json.Marshal(frame)
			t.Fatalf("frame = %s, want a snapshot or a delta of %s", raw, sub)
		}
	}
}

// TestTheProjectTimelineViewGrowsWithItsLimit: over a window of at least 1,300 rows of a
// one-project history, a page subscribes the project-timeline view with limit 200 and grows it 200
// at a time, each time subscribing its sub again with the higher limit and a resume holding the
// rows it has, up to the window's total. Its snapshot is the route's first 200 rows; each growth's
// delta carries exactly the rows the page lacked, and removes none; a pull request merged between
// two growths reaches the page once, by the delta of the view it then holds; and the rows the page
// holds at the end are every row once, equal to the route's union of pages, with the waiting line
// once the limit reached the total.
func TestTheProjectTimelineViewGrowsWithItsLimit(t *testing.T) {
	rig := newHubRig(t, nil)
	fixture := deliverytest.Seed(t, rig.database.Pool, deliverytest.Spec{Seed: 2918, Days: 70, OneProject: true})
	// Every pull request counts for the project of the issue its row names.
	leaveAttributionInputsUnread(t, rig.database.Pool)
	to := fixture.To.AddDate(0, 0, -1)
	from := to.AddDate(0, 0, -56)
	project := fixture.Project
	routeRows := func() []map[string]any {
		return projectTimelineRows(projectTimelinePages(t, rig.handler, project, from, to, nil, projectTimelinePageMax))
	}
	rows := routeRows()
	if len(rows) < 1300 {
		t.Fatalf("the window holds %d rows, want at least 1,300", len(rows))
	}
	view := func(limit int) DeliveryLiveView {
		return projectTimelineRange(project, from, to, "limit="+strconv.Itoa(limit))
	}
	byKey := func(rows []map[string]any) map[string]map[string]any {
		keyed := map[string]map[string]any{}
		for _, row := range rows {
			keyed[liveID(t, row)] = row
		}
		return keyed
	}

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", view(200)))
	snapshot := nextProjectTimelineFrame(t, conn, "t").(DeliveryLiveSnapshot)
	held := mustEncodeLive(t, deliveryLiveProjectTimeline, json.RawMessage(snapshot.Body))
	if got, want := liveComparable(t, held)["items"], liveComparable(t, mustEncodeLive(t, deliveryLiveProjectTimeline, projectTimelineSlice(t, rows[:200])))["items"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("the snapshot's rows are not the route's first 200")
	}
	seq, revs := snapshot.Seq, snapshot.Revs
	apply := func(delta DeliveryLiveDelta) {
		t.Helper()
		applied := liveApply(t, held, delta)
		raw, err := json.Marshal(applied.body)
		if err != nil {
			t.Fatalf("encode the applied body: %v", err)
		}
		held = mustEncodeLive(t, deliveryLiveProjectTimeline, json.RawMessage(raw))
		revs, seq = liveApplyRevs(revs, delta.Revs), delta.Seq
	}

	var written string
	growths := 0
	for limit := 200; limit < len(rows); {
		limit = min(limit+200, len(rows))
		frame := subscribeFrame("t", view(limit))
		frame.Resume = &DeliveryLiveResume{Seq: seq, Revs: revs}
		heldIDs := liveByID(t, liveDecoded(t, held)["items"])
		rig.hub.Subscribe(conn, frame)
		delta := nextProjectTimelineFrame(t, conn, "t").(DeliveryLiveDelta)
		growths++
		var lacking []string
		for _, row := range rows[:limit] {
			if id := liveID(t, row); heldIDs[id] == nil {
				lacking = append(lacking, id)
			}
		}
		changes := delta.Items["items"]
		if got := liveUpsertIDs(t, delta, "items"); !reflect.DeepEqual(sortedCopy(got), sortedCopy(lacking)) || len(changes.Remove) != 0 {
			t.Fatalf("growing to %d rows: the delta upserts %d rows and removes %v, want the %d rows the page lacked and none removed", limit, len(got), changes.Remove, len(lacking))
		}
		apply(delta)

		if written == "" && limit >= 600 {
			// A pull request of the project merges at the window's end, between two growths: the
			// view the page holds now answers it, once.
			rig.settle(t)
			merged := to.Add(-time.Minute)
			opened := merged.Add(-time.Hour)
			pr := delivery.DeliveryPullRequest{
				Repo: fixture.DeployRepo, Number: 99999, Title: "Change between growths", URL: "https://github.com/" + fixture.DeployRepo + "/pull/99999",
				Author: "dev-ana", CreatedAt: &opened, MergedAt: &merged, IssueKey: &fixture.Issues[project][0], Sessions: []string{},
			}
			upsertPR(t, rig.database, pr)
			written = fixture.DeployRepo + "#99999"
			change := nextProjectTimelineFrame(t, conn, "t").(DeliveryLiveDelta)
			if got := liveUpsertIDs(t, change, "items"); !slices.Contains(got, written) || len(change.Items["items"].Remove) == 0 {
				t.Fatalf("the merge between growths sent upserts %v and removed %v, want it upserted and the row it pushed past the limit removed", got, change.Items["items"].Remove)
			}
			apply(change)
			rows = routeRows()
			if got, want := liveComparable(t, held)["items"], liveComparable(t, mustEncodeLive(t, deliveryLiveProjectTimeline, projectTimelineSlice(t, rows[:limit])))["items"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("after the merge between growths, the page's %d rows are not the route's first %d", limit, limit)
			}
		}
	}
	if written == "" || growths < 6 {
		t.Fatalf("the page grew %d times and the merge between growths was written %t", growths, written != "")
	}

	final := liveDecoded(t, held)
	items, _ := final["items"].([]any)
	if len(items) != len(rows) || int(final["total"].(float64)) != len(rows) {
		t.Fatalf("the page holds %d rows of a total %v, want every one of the route's %d once", len(items), final["total"], len(rows))
	}
	got, want := liveByID(t, final["items"]), byKey(rows)
	for id, row := range want {
		if !reflect.DeepEqual(got[id], any(row)) {
			t.Errorf("row %s: the page holds %v, the route answers %v", id, got[id], row)
		}
	}
	if final["waiting"] == nil {
		t.Error("the page's limit reached the window's total, and it holds no waiting line")
	}
	t.Logf("%s..%s: %d rows grown into in %d growths, the merge between growths held once", from.Format(time.DateOnly), to.Format(time.DateOnly), len(rows), growths)
}

// projectTimelineSlice is a project timeline body holding rows as its items, its other members
// empty, for comparing a body's rows.
func projectTimelineSlice(t *testing.T, rows []map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"items": rows, "total": len(rows), "limit": len(rows), "offset": 0, "window": nil, "waiting": nil,
		"facet_counts": map[string]any{}, "color_counts": map[string]any{}, "components": nil, "issue_titles": nil,
		"session_groups": nil, "session_titles": nil, "session_pr_counts": nil, "freshness": nil,
	})
	if err != nil {
		t.Fatalf("encode rows: %v", err)
	}
	return raw
}

// sortedCopy is ids sorted, leaving ids as it is.
func sortedCopy(ids []string) []string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return sorted
}
