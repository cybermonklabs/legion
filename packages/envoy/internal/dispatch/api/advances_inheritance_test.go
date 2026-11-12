// advances_inheritance_test.go reads the `advances` relation as an issue below the one that
// states it answers it: the inheritance walk every issue read climbs (issueAdvancesTerms) and
// the descent the listing's filter makes of the same rule (issueAdvancesFilterTerms), on the
// database store/storetest's Open clones from this package's migrated template, through the
// handler the neighbouring tests drive (newTestHandlerWithStore, dispatchRequest).
//
// A relation is stated on one issue and read on every issue below it, in any project, so the
// trees here stand in three of them and each read is held to say, row by row, whether the issue
// states the relation itself or inherits it. The filter is read the same way: by the keys it
// answers for one priority, for two named together, for none, for the two together, paged, and
// after the priority is deleted — a priority whose state is `gone` is no priority, so the work
// under it advances none.
package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

func TestAdvancesInheritance(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	for _, project := range []string{"ALPHA", "BETA", "GAMMA"} {
		createTestProject(t, handler, project)
	}
	shipTheDaemon := createTestPriority(t, handler, map[string]any{
		"id": "ship-the-daemon", "title": "Ship the daemon", "owner": "alice",
	}, "alice").ID
	cutTheQueue := createTestPriority(t, handler, map[string]any{
		"id": "cut-the-queue", "title": "Cut the queue", "owner": "bob",
	}, "alice").ID
	holdTheLine := createTestPriority(t, handler, map[string]any{
		"id": "hold-the-line", "title": "Hold the line", "owner": "alice",
	}, "alice").ID

	// A tree in each of three projects, each root stating the one relation. ALPHA's is the
	// deeper one: a second child, which states a second priority, with a child of its own.
	trees := map[string]string{}
	for _, project := range []string{"ALPHA", "BETA", "GAMMA"} {
		root := createAdvancingIssue(t, handler, map[string]any{
			"project": project, "title": project + " root", "force": true,
			"advances": []string{shipTheDaemon},
		}, "alice")
		child := createAdvancingIssue(t, handler, map[string]any{
			"project": project, "title": project + " child", "force": true, "parent": root.Key,
		}, "alice")
		grandchild := createAdvancingIssue(t, handler, map[string]any{
			"project": project, "title": project + " grandchild", "force": true, "parent": child.Key,
		}, "alice")
		trees[project+" root"], trees[project+" child"], trees[project+" grandchild"] = root.Key, child.Key, grandchild.Key
	}
	served := createAdvancingIssue(t, handler, map[string]any{
		"project": "ALPHA", "title": "ALPHA second child", "force": true,
		"parent": trees["ALPHA root"], "advances": []string{cutTheQueue},
	}, "alice")
	servedChild := createAdvancingIssue(t, handler, map[string]any{
		"project": "ALPHA", "title": "ALPHA second grandchild", "force": true, "parent": served.Key,
	}, "alice")
	trees["ALPHA second child"], trees["ALPHA second grandchild"] = served.Key, servedChild.Key

	// Work that advances nothing, which is what `advances=none` answers and what the two keys
	// together answer beside a tree.
	unrelated := createAdvancingIssue(t, handler, map[string]any{
		"project": "BETA", "title": "Unrelated work", "force": true,
	}, "alice")

	// The whole tree of the related root in each project, each row saying own or inherited.
	shipTree := map[string][]string{}
	for _, project := range []string{"ALPHA", "BETA", "GAMMA"} {
		shipTree[trees[project+" root"]] = []string{shipTheDaemon + " own"}
		shipTree[trees[project+" child"]] = []string{shipTheDaemon + " inherited"}
		shipTree[trees[project+" grandchild"]] = []string{shipTheDaemon + " inherited"}
	}
	// The child that states the second priority serves both, and so does its own child; the
	// issues above it serve the first alone, which is already written above: a relation on a
	// child places nothing above it.
	shipTree[served.Key] = []string{cutTheQueue + " own", shipTheDaemon + " inherited"}
	shipTree[servedChild.Key] = []string{cutTheQueue + " inherited", shipTheDaemon + " inherited"}
	assertAdvancesRead(t, handler, "/api/v1/issues?advances="+shipTheDaemon, shipTree)

	// The second priority answers that child and its children alone.
	assertAdvancesRead(t, handler, "/api/v1/issues?advances="+cutTheQueue, map[string][]string{
		served.Key:      {cutTheQueue + " own", shipTheDaemon + " inherited"},
		servedChild.Key: {cutTheQueue + " inherited", shipTheDaemon + " inherited"},
	})

	// A tree as deep as the parent-depth cap: refs.IssueDepthCap links below its root, which is
	// the deepest chain a write allows (refuseParentTooDeep), under a priority of its own.
	chain := make([]string, 0, refs.IssueDepthCap+1)
	for depth := range refs.IssueDepthCap + 1 {
		body := map[string]any{
			"project": "GAMMA", "title": fmt.Sprintf("Deep %02d", depth), "force": true,
		}
		if depth == 0 {
			body["advances"] = []string{holdTheLine}
		} else {
			body["parent"] = chain[depth-1]
		}
		chain = append(chain, createAdvancingIssue(t, handler, body, "alice").Key)
	}
	deepTree := map[string][]string{chain[0]: {holdTheLine + " own"}}
	for _, key := range chain[1:] {
		deepTree[key] = []string{holdTheLine + " inherited"}
	}
	assertAdvancesRead(t, handler, "/api/v1/issues?advances="+holdTheLine, deepTree)
	// The deepest issue of that chain reads its inherited relation on its own read too, which
	// is the walk climbing the whole cap rather than the listing's descent.
	deepest := readTestIssue(t, handler, chain[len(chain)-1])
	if got := advanceEntries(deepest.Advances); !slices.Equal(got, []string{holdTheLine + " inherited"}) {
		t.Fatalf("the issue %d links below its root advances %v, want %s inherited",
			refs.IssueDepthCap, got, holdTheLine)
	}

	// `advances=none` finds neither tree, and finds the work that advances nothing.
	none := listedIssueKeys(t, handler, "/api/v1/issues?advances=none")
	for key := range shipTree {
		if slices.Contains(none, key) {
			t.Fatalf("the work advancing no priority names %s, which is under %s", key, shipTheDaemon)
		}
	}
	for _, key := range chain {
		if slices.Contains(none, key) {
			t.Fatalf("the work advancing no priority names %s, which is under %s", key, holdTheLine)
		}
	}
	if !slices.Contains(none, unrelated.Key) {
		t.Fatalf("the work advancing no priority = %v, want %s among it", none, unrelated.Key)
	}

	// Two `advances` keys are one union: the second priority's two issues and the deep chain,
	// and nothing else.
	union := listedIssueKeys(t, handler, "/api/v1/issues?advances="+cutTheQueue+"&advances="+holdTheLine)
	wantUnion := append([]string{served.Key, servedChild.Key}, chain...)
	slices.Sort(wantUnion)
	if got := slices.Sorted(slices.Values(union)); !slices.Equal(got, wantUnion) {
		t.Fatalf("the two priorities together answer %d issues (%v), want the %d of both trees",
			len(got), got, len(wantUnion))
	}

	// A priority and `none` together, with `open` both ways. One issue of the tree and one that
	// advances nothing are closed, so the open read answers neither and the read that does not
	// ask for open work answers both.
	closedUnder := trees["BETA grandchild"]
	closedUnplaced := createAdvancingIssue(t, handler, map[string]any{
		"project": "BETA", "title": "Finished and under nothing", "force": true,
	}, "alice").Key
	for _, key := range []string{closedUnder, closedUnplaced} {
		if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
			map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
			t.Fatalf("close %s: status=%d body=%s", key, closed.Code, closed.Body.String())
		}
	}
	both := "/api/v1/issues?advances=" + shipTheDaemon + "&advances=none"
	everything := listedIssueKeys(t, handler, both)
	for _, key := range []string{closedUnder, closedUnplaced, unrelated.Key, trees["ALPHA root"]} {
		if !slices.Contains(everything, key) {
			t.Fatalf("the priority and `none` together answer %v, want %s among them", everything, key)
		}
	}
	for _, key := range chain[1:] {
		if slices.Contains(everything, key) {
			t.Fatalf("the priority and `none` together name %s, which advances %s", key, holdTheLine)
		}
	}
	open := listedIssueKeys(t, handler, both+"&open=true")
	for _, key := range []string{closedUnder, closedUnplaced} {
		if slices.Contains(open, key) {
			t.Fatalf("the open work under the priority or under none names %s, which is closed", key)
		}
	}
	if !slices.Contains(open, trees["ALPHA root"]) || !slices.Contains(open, unrelated.Key) {
		t.Fatalf("the open work under the priority or under none = %v, want the open issues of both arms", open)
	}
	if len(open) != len(everything)-2 {
		t.Fatalf("the open read answers %d issues against the whole read's %d, want the two closed ones gone",
			len(open), len(everything))
	}

	// The paged answer and its total: consecutive pages cover the tree once, and the total is
	// every issue the filter matches however small the page.
	target := "/api/v1/issues?advances=" + shipTheDaemon
	whole := listedIssueKeys(t, handler, target)
	paged := []string{}
	for offset := 0; offset < len(whole); offset += 5 {
		page := listedIssuePage(t, handler, fmt.Sprintf("%s&limit=5&offset=%d", target, offset))
		if page.Total != len(whole) {
			t.Fatalf("the page at offset %d answers total %d, want the %d issues the filter matches",
				offset, page.Total, len(whole))
		}
		if want := min(5, len(whole)-offset); len(page.Issues) != want {
			t.Fatalf("the page at offset %d holds %d issues, want %d", offset, len(page.Issues), want)
		}
		for _, row := range page.Issues {
			paged = append(paged, row.Key)
		}
	}
	if !slices.Equal(paged, whole) {
		t.Fatalf("the pages together answer %v, want the unpaged listing's %v", paged, whole)
	}

	// A priority whose state is `gone` is no priority: its tree advances none.
	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/"+holdTheLine,
		nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete %s: status=%d body=%s", holdTheLine, deleted.Code, deleted.Body.String())
	}
	if keys := listedIssueKeys(t, handler, "/api/v1/issues?advances="+holdTheLine); len(keys) != 0 {
		t.Fatalf("the deleted priority answers %v, want no issue", keys)
	}
	// The whole tree answers `advances=none` now, and no other `advances` key draws it: a
	// relation to a priority that is gone is no relation to a priority at all.
	nowNone := listedIssueKeys(t, handler, "/api/v1/issues?advances=none")
	stillUnderShip := listedIssueKeys(t, handler, "/api/v1/issues?advances="+shipTheDaemon)
	for _, key := range chain {
		if !slices.Contains(nowNone, key) {
			t.Fatalf("the work advancing no priority does not name %s, whose priority is gone", key)
		}
		if slices.Contains(stillUnderShip, key) {
			t.Fatalf("%s is drawn under %s after its own priority was deleted", key, shipTheDaemon)
		}
	}
}

// assertAdvancesRead holds one `advances` listing to the issues it answers and to what each row
// says of every relation it carries: "<priority> own" for the issue's own relation and
// "<priority> inherited" for one an issue above it states.
func assertAdvancesRead(t *testing.T, handler http.Handler, target string, want map[string][]string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	rows := decodeBody[[]model.IssueSummary](t, response)
	got := make(map[string][]string, len(rows))
	for _, row := range rows {
		got[row.Key] = advanceEntries(row.Advances)
	}
	if len(got) != len(want) {
		t.Fatalf("%s answers %d issues (%v), want %d", target, len(got), slices.Sorted(maps.Keys(got)), len(want))
	}
	for key, entries := range want {
		answered, listed := got[key]
		if !listed {
			t.Fatalf("%s answers no row for %s", target, key)
		}
		if !slices.Equal(answered, entries) {
			t.Fatalf("%s says %s advances %v, want %v", target, key, answered, entries)
		}
	}
}

// listedIssuePage reads one paged listing: the page's rows and the count of every issue the
// filters match, which the same statement made of the same snapshot.
func listedIssuePage(t *testing.T, handler http.Handler, target string) model.IssueSummaryPage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[model.IssueSummaryPage](t, response)
}
