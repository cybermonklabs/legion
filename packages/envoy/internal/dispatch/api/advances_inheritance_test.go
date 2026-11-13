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
//
// The listing also draws the two fault lists of GET /api/v1/priorities, which the page's one
// Issues table renders: `advances=any` beside `none`, a repeated `status`, and the move into
// `backlog` or `icebox` under `?moved=true`. Each is read here on its own, and then the two
// listings that ask for the faults are held to the issues that route's own two lists answer.
package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
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
	// Every row of the tree, and the root's own read, answers no relation to it either: the
	// listing row's `advances` and the issue read say what the filter says. GAMMA's other tree
	// is under the live priority, so the chain is the whole of GAMMA's work under none.
	deepRows := map[string][]string{}
	for _, key := range chain {
		deepRows[key] = []string{}
	}
	assertAdvancesRead(t, handler, "/api/v1/issues?project=GAMMA&advances=none", deepRows)
	if got := advanceEntries(readTestIssue(t, handler, chain[0]).Advances); len(got) != 0 {
		t.Fatalf("%s advances %v after its priority was deleted, want nothing", chain[0], got)
	}
}

// TestAdvancesToAGonePriorityReadAsNone reads an issue's relations once one of its priorities is
// deleted. A priority whose state is `gone` is no priority, so the issue read and the listing row
// answer neither the issue's own relation to it nor the one an issue below inherits, as the
// `advances` filter already does. The revision still counts every tuple of the issue's own set,
// the relation to the gone priority among them, so it does not move when the priority is deleted
// and does move when a write retracts a live one. A whole-set write neither states nor retracts
// the relation to the gone priority: the read never offers it, a write naming it is refused, and
// the log keeps the tuple that stated it.
func TestAdvancesToAGonePriorityReadAsNone(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	createTestProject(t, handler, "CORE")
	kept := createTestPriority(t, handler, map[string]any{"id": "kept-goal", "title": "Kept goal"}, "alice").ID
	dropped := createTestPriority(t, handler, map[string]any{"id": "dropped-goal", "title": "Dropped goal"}, "alice").ID
	root := createAdvancingIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Root under both", "force": true, "advances": []string{kept, dropped},
	}, "alice")
	child := createAdvancingIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Child under both", "force": true, "parent": root.Key,
	}, "alice")
	only := createAdvancingIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Under the dropped goal alone", "force": true, "advances": []string{dropped},
	}, "alice")
	rootRevision := readTestIssue(t, handler, root.Key).AdvancesRevision
	onlyRevision := readTestIssue(t, handler, only.Key).AdvancesRevision
	if onlyRevision == 0 {
		t.Fatalf("%s answers no advances revision before the delete, want the log position of its relation", only.Key)
	}

	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/"+dropped,
		nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete %s: status=%d body=%s", dropped, deleted.Code, deleted.Body.String())
	}

	// The issue read answers the live priority alone, own and inherited, and the revision of
	// each issue is the one it had: deleting a priority writes no tuple of any issue's set.
	want := map[string][]string{
		root.Key:  {kept + " own"},
		child.Key: {kept + " inherited"},
		only.Key:  {},
	}
	for key, entries := range want {
		if got := advanceEntries(readTestIssue(t, handler, key).Advances); !slices.Equal(got, entries) {
			t.Fatalf("%s advances %v when read after %s was deleted, want %v", key, got, dropped, entries)
		}
	}
	if got := readTestIssue(t, handler, root.Key).AdvancesRevision; got != rootRevision {
		t.Fatalf("%s's revision is %d after the delete, want the %d it had", root.Key, got, rootRevision)
	}
	if got := readTestIssue(t, handler, only.Key).AdvancesRevision; got != onlyRevision {
		t.Fatalf("%s's revision is %d after the delete, want the %d its relation to the gone priority holds",
			only.Key, got, onlyRevision)
	}
	// Every listing row says the same, and the filter agrees with the rows.
	assertAdvancesRead(t, handler, "/api/v1/issues?project=CORE", want)
	assertAdvancesRead(t, handler, "/api/v1/issues?project=CORE&advances=none", map[string][]string{only.Key: {}})
	if keys := listedIssueKeys(t, handler, "/api/v1/issues?advances="+dropped); len(keys) != 0 {
		t.Fatalf("the deleted priority answers %v, want no issue", keys)
	}

	// A write naming the gone priority again is refused, as a new relation to it is.
	restated := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+root.Key, map[string]any{
		"advances": []string{kept, dropped}, "advances_revision": rootRevision,
	}, "alice")
	if code, text := refusalOf(t, restated); restated.Code != http.StatusBadRequest || code != codeAdvancesInput {
		t.Fatalf("a write naming the gone priority: status=%d code=%s error=%q, want 400 %s",
			restated.Code, code, text, codeAdvancesInput)
	}

	// A whole-set write of nothing retracts the live relation, one act, and leaves the tuple that
	// stated the gone one as the newest of its pair; the revision moves past that retraction.
	retracted := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+root.Key, map[string]any{
		"advances": []string{}, "advances_revision": rootRevision,
	}, "alice")
	if retracted.Code != http.StatusOK {
		t.Fatalf("retract %s's relations: status=%d body=%s", root.Key, retracted.Code, retracted.Body.String())
	}
	after := readTestIssue(t, handler, root.Key)
	if len(after.Advances) != 0 {
		t.Fatalf("%s advances %v after the whole-set retraction, want nothing", root.Key, advanceEntries(after.Advances))
	}
	if after.AdvancesRevision <= rootRevision {
		t.Fatalf("%s's revision is %d after the retraction, want it past the %d it had", root.Key, after.AdvancesRevision, rootRevision)
	}
	history, err := prioritystore.History(ctx, database.Pool, prioritystore.KindIssue, root.Key, prioritystore.RelationAdvances)
	if err != nil {
		t.Fatalf("read %s's advances history: %v", root.Key, err)
	}
	acts := map[string][]bool{}
	for _, act := range history {
		acts[act.ToID] = append(acts[act.ToID], act.Asserted)
	}
	if !slices.Equal(acts[kept], []bool{true, false}) || !slices.Equal(acts[dropped], []bool{true}) {
		t.Fatalf("%s's advances history = %v, want %s stated then retracted and %s stated alone", root.Key, acts, kept, dropped)
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

// TestListIssuesFiltersByRepeatedStatus reads `status` as a list: the issues standing in any
// status named, one value meaning what it meant when the key took one, a repeat of one status the
// same as naming it once, and no value or a blank one every status. A status the lifecycle does
// not hold is refused 400 INVALID_STATUS, naming the value, rather than answering a list nothing
// stands in.
func TestListIssuesFiltersByRepeatedStatus(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createTestProject(t, handler, "STAT")
	keys := map[string]string{}
	for _, status := range []string{model.IssueStatusTriage, "todo", "in_progress", "backlog", "done"} {
		moved := status
		if status == model.IssueStatusTriage {
			// Every issue is created in triage, so that one is written with no move.
			moved = ""
		}
		keys[status] = seedPriorityIssue(t, handler, "STAT", "Work in "+status, moved, "", nil)
	}
	every := slices.Sorted(maps.Values(keys))
	for query, statuses := range map[string][]string{
		"project=STAT":                         nil,
		"project=STAT&status=":                 nil,
		"project=STAT&status=todo":             {"todo"},
		"project=STAT&status=todo&status=todo": {"todo"},
		"project=STAT&status=todo&status=backlog&status=in_progress": {"todo", "backlog", "in_progress"},
		"project=STAT&status=done&status=":                           {"done"},
	} {
		want := every
		if statuses != nil {
			want = []string{}
			for _, status := range statuses {
				want = append(want, keys[status])
			}
			slices.Sort(want)
		}
		if got := slices.Sorted(slices.Values(listedIssueKeys(t, handler, "/api/v1/issues?"+query))); !slices.Equal(got, want) {
			t.Fatalf("?%s answers %v, want %v", query, got, want)
		}
	}
	for query, named := range map[string]string{
		"status=almost":             "almost",
		"status=todo&status=closed": "closed",
		"project=STAT&status=Todo":  "Todo",
	} {
		refused := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
		code, text := refusalOf(t, refused)
		if refused.Code != http.StatusBadRequest || code != "INVALID_STATUS" || !strings.Contains(text, fmt.Sprintf("%q", named)) {
			t.Fatalf("?%s: status=%d code=%s error=%q, want 400 INVALID_STATUS naming %q", query, refused.Code, code, text, named)
		}
	}
}

// TestAdvancesAnyIsTheComplementOfNone reads `advances=any` as the issues `advances=none` leaves
// out, over the same listing: an issue that states a live priority, one that inherits it from an
// issue above it, and none else. An issue above the one that states it inherits nothing, and an
// issue under a priority that is gone advances none, as `none` already reads both. The two
// keywords together are the whole listing, a priority id beside `any` adds nothing to it, and
// the complement holds inside a narrower listing too.
func TestAdvancesAnyIsTheComplementOfNone(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	for _, project := range []string{"LEFT", "RIGHT"} {
		createTestProject(t, handler, project)
	}
	kept := createTestPriority(t, handler, map[string]any{"id": "kept-goal", "title": "Kept goal"}, "alice").ID
	dropped := createTestPriority(t, handler, map[string]any{"id": "dropped-goal", "title": "Dropped goal"}, "alice").ID

	root := seedPriorityIssue(t, handler, "LEFT", "Root under the kept goal", "", "", []string{kept})
	child := seedPriorityIssue(t, handler, "LEFT", "Child inheriting it", "in_progress", root, nil)
	grandchild := seedPriorityIssue(t, handler, "LEFT", "Grandchild inheriting it", "", child, nil)
	above := seedPriorityIssue(t, handler, "RIGHT", "Above the issue that states it", "in_progress", "", nil)
	stated := seedPriorityIssue(t, handler, "RIGHT", "Stating it below a root that does not", "", above, []string{kept})
	underDropped := seedPriorityIssue(t, handler, "RIGHT", "Under the dropped goal", "", "", []string{dropped})
	belowDropped := seedPriorityIssue(t, handler, "RIGHT", "Below the dropped goal", "", underDropped, nil)
	unrelated := seedPriorityIssue(t, handler, "LEFT", "Under nothing", "", "", nil)
	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/"+dropped,
		nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete %s: status=%d body=%s", dropped, deleted.Code, deleted.Body.String())
	}

	wantAny := slices.Sorted(slices.Values([]string{root, child, grandchild, stated}))
	wantNone := slices.Sorted(slices.Values([]string{above, underDropped, belowDropped, unrelated}))
	sorted := func(target string) []string {
		return slices.Sorted(slices.Values(listedIssueKeys(t, handler, target)))
	}
	if got := sorted("/api/v1/issues?advances=" + anyAdvancesFilter); !slices.Equal(got, wantAny) {
		t.Fatalf("advances=any answers %v, want the issues that state the kept goal or sit below one that does: %v", got, wantAny)
	}
	if got := sorted("/api/v1/issues?advances=" + noAdvancesFilter); !slices.Equal(got, wantNone) {
		t.Fatalf("advances=none answers %v, want %v", got, wantNone)
	}
	every := listedIssueKeys(t, handler, "/api/v1/issues")
	if got := listedIssueKeys(t, handler, "/api/v1/issues?advances=any&advances=none"); !slices.Equal(got, every) {
		t.Fatalf("any and none together answer %v, want the whole listing %v", got, every)
	}
	if got := sorted("/api/v1/issues?advances=any&advances=" + kept); !slices.Equal(got, wantAny) {
		t.Fatalf("any beside %s answers %v, want any's own %v", kept, got, wantAny)
	}
	// Each row of either listing says what the filter says of it.
	assertAdvancesRead(t, handler, "/api/v1/issues?advances=any", map[string][]string{
		root: {kept + " own"}, child: {kept + " inherited"}, grandchild: {kept + " inherited"}, stated: {kept + " own"},
	})

	// Inside a narrower listing the two keywords still split it in two.
	for _, narrowed := range []string{"project=LEFT", "project=RIGHT", "status=in_progress", "status=triage&status=todo"} {
		whole := sorted("/api/v1/issues?" + narrowed)
		some := sorted("/api/v1/issues?" + narrowed + "&advances=any")
		none := sorted("/api/v1/issues?" + narrowed + "&advances=none")
		if len(some) == 0 || len(none) == 0 {
			t.Fatalf("?%s splits into %v and %v, want both arms holding an issue so the split decides something", narrowed, some, none)
		}
		for _, key := range some {
			if slices.Contains(none, key) {
				t.Fatalf("?%s answers %s under both any and none", narrowed, key)
			}
		}
		if union := slices.Sorted(slices.Values(slices.Concat(some, none))); !slices.Equal(union, whole) {
			t.Fatalf("?%s's any %v and none %v together are %v, want the listing %v", narrowed, some, none, union, whole)
		}
	}
}

// TestListIssuesAnswersTheMoveOnlyWhenAsked reads moved_at and moved_by on the listing. Without
// `?moved=true` no row carries either field, paged or not. With it, an issue standing in
// `backlog` or `icebox` carries the time and the actor of its newest move into either, a later
// write that moves no status leaving them where they are; an issue whose status was written with
// no event, one moved there and out again, and one never moved carry neither field.
func TestListIssuesAnswersTheMoveOnlyWhenAsked(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	createTestProject(t, handler, "MOVE")
	twice := seedPriorityIssue(t, handler, "MOVE", "Moved twice", "", "", nil)
	moveTestIssue(t, handler, twice, "backlog", "bob")
	moveTestIssue(t, handler, twice, "icebox", "alice")
	if retitled := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+twice,
		map[string]any{"title": "Moved twice, then retitled"}, "bob"); retitled.Code != http.StatusOK {
		t.Fatalf("retitle %s: status=%d body=%s", twice, retitled.Code, retitled.Body.String())
	}
	unrecorded := seedPriorityIssue(t, handler, "MOVE", "Parked before moves were recorded", "", "", nil)
	if _, err := database.Pool.Exec(ctx, `update issues set status = 'backlog' where key = $1`, unrecorded); err != nil {
		t.Fatalf("park %s with no event: %v", unrecorded, err)
	}
	returned := seedPriorityIssue(t, handler, "MOVE", "Parked and taken back", "backlog", "", nil)
	moveTestIssue(t, handler, returned, "todo", "bob")
	never := seedPriorityIssue(t, handler, "MOVE", "Never parked", "", "", nil)

	// The move into icebox, and the retitle after it, which is the issue's newest event and moves
	// no status: the listing answers the first.
	var wantAt, retitledAt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select created_at from events
		where issue_key = $1 and type = 'issue.updated' and payload->'changed' @> '"status"'::jsonb
		order by seq desc limit 1`, twice).Scan(&wantAt); err != nil {
		t.Fatalf("read the time %s was moved into icebox: %v", twice, err)
	}
	if err := database.Pool.QueryRow(ctx, `
		select created_at from events where issue_key = $1 and type = 'issue.updated'
		order by seq desc limit 1`, twice).Scan(&retitledAt); err != nil {
		t.Fatalf("read the time %s was retitled: %v", twice, err)
	}
	if !retitledAt.After(wantAt) {
		t.Fatalf("%s's newest event is at %v, want the retitle after its move at %v", twice, retitledAt, wantAt)
	}

	for _, query := range []string{"project=MOVE", "project=MOVE&limit=10", "project=MOVE&moved=false"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("list ?%s: status=%d body=%s", query, response.Code, response.Body.String())
		}
		if body := response.Body.String(); strings.Contains(body, `"moved_at"`) || strings.Contains(body, `"moved_by"`) {
			t.Fatalf("?%s, which asks for no move, answers a row carrying one: %s", query, body)
		}
	}

	for _, query := range []string{"project=MOVE&moved=true", "project=MOVE&moved=true&limit=10"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("list ?%s: status=%d body=%s", query, response.Code, response.Body.String())
		}
		var rows []model.IssueSummary
		if strings.Contains(query, "limit=") {
			rows = decodeBody[model.IssueSummaryPage](t, response).Issues
		} else {
			rows = decodeBody[[]model.IssueSummary](t, response)
		}
		byKey := map[string]model.IssueSummary{}
		for _, row := range rows {
			byKey[row.Key] = row
		}
		if len(byKey) != 4 {
			t.Fatalf("?%s answers %d issues, want the 4 of the project", query, len(byKey))
		}
		moved := byKey[twice]
		if moved.MovedAt == nil || !moved.MovedAt.Equal(wantAt) {
			t.Fatalf("?%s says %s was moved at %v, want %v, its move into icebox", query, twice, moved.MovedAt, wantAt)
		}
		if moved.MovedBy == nil || *moved.MovedBy != (model.Actor{Kind: "user", ID: "alice"}) {
			t.Fatalf("?%s says %s was moved by %v, want alice, who moved it into icebox", query, twice, moved.MovedBy)
		}
		for _, key := range []string{unrecorded, returned, never} {
			if row := byKey[key]; row.MovedAt != nil || row.MovedBy != nil {
				t.Fatalf("?%s says %s (%s) was moved at %v by %v, want neither", query, key, row.Status, row.MovedAt, row.MovedBy)
			}
		}
	}
}

// faultListingStatuses are the statuses each fault reads, as the listing's repeated `status`.
func faultListingStatuses(statuses []string) string {
	var query strings.Builder
	for _, status := range statuses {
		query.WriteString("&status=" + status)
	}
	return query.String()
}

// TestTheListingAnswersTheFaultLists holds the two listings the priorities page draws its fault
// lists with to the two lists GET /api/v1/priorities answers for the same deployment, which is
// seeded under the fifty rows each list there is cut at: work in flight under no priority is
// `advances=none` over the four statuses work runs in, and priority work parked is
// `advances=any` over `backlog` and `icebox` with `?moved=true`, whose rows carry the same time
// and actor of the move as the parked list's. The seed holds each case either list turns on: own
// and inherited relations, a relation on a child that places nothing above it, a priority that
// is gone, work parked under no priority, work parked with no recorded move, and finished work.
func TestTheListingAnswersTheFaultLists(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	for _, project := range []string{"CORE", "OPS"} {
		createTestProject(t, handler, project)
	}
	for _, id := range []string{"ship-the-daemon", "park-the-bus", "gone-goal"} {
		createTestPriority(t, handler, map[string]any{"id": id, "title": id}, "alice")
	}

	// In flight under no priority, a root whose child carries the relation among them, and in
	// flight under a priority, own and inherited.
	seedPriorityIssue(t, handler, "CORE", "Unplaced in progress", "in_progress", "", nil)
	seedPriorityIssue(t, handler, "OPS", "Unplaced in testing", "testing", "", nil)
	seedPriorityIssue(t, handler, "CORE", "Unplaced in review", "needs_review", "", nil)
	retroRoot := seedPriorityIssue(t, handler, "CORE", "Unplaced root in retro", "retro", "", nil)
	seedPriorityIssue(t, handler, "CORE", "The child that carries the relation", "todo", retroRoot, []string{"ship-the-daemon"})
	placed := seedPriorityIssue(t, handler, "CORE", "Placed in progress", "in_progress", "", []string{"ship-the-daemon"})
	seedPriorityIssue(t, handler, "CORE", "Placed by inheritance in testing", "testing", placed, nil)
	seedPriorityIssue(t, handler, "OPS", "In review under the gone goal", "needs_review", "", []string{"gone-goal"})

	// Parked under a priority, through a parent, on its own, and with no recorded move; parked
	// under none and under the gone goal; and finished work under a priority.
	parkedParent := seedPriorityIssue(t, handler, "CORE", "Parked work's parent", "todo", "", []string{"park-the-bus"})
	parkedChild := seedPriorityIssue(t, handler, "CORE", "Parked work", "todo", parkedParent, nil)
	moveTestIssue(t, handler, parkedChild, "backlog", "bob")
	seedPriorityIssue(t, handler, "OPS", "Iced on its own", "icebox", "", []string{"ship-the-daemon"})
	unrecorded := seedPriorityIssue(t, handler, "OPS", "Parked with no recorded move", "todo", "", []string{"park-the-bus"})
	if _, err := database.Pool.Exec(context.Background(), `update issues set status = 'backlog' where key = $1`, unrecorded); err != nil {
		t.Fatalf("park %s with no event: %v", unrecorded, err)
	}
	seedPriorityIssue(t, handler, "OPS", "Parked under nothing", "backlog", "", nil)
	seedPriorityIssue(t, handler, "OPS", "Parked under the gone goal", "icebox", "", []string{"gone-goal"})
	seedPriorityIssue(t, handler, "CORE", "Finished under a priority", "done", "", []string{"ship-the-daemon"})
	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/priorities/gone-goal",
		nil, "alice"); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete gone-goal: status=%d body=%s", deleted.Code, deleted.Body.String())
	}

	body := readPriorities(t, handler, "")
	unplaced, parked := body.WorkUnderNoPriority, body.PriorityWorkParked
	if unplaced.Count != 5 || len(unplaced.Issues) != unplaced.Count {
		t.Fatalf("the priorities read counts %d issues in flight under no priority and draws %d, want all 5 drawn",
			unplaced.Count, len(unplaced.Issues))
	}
	if parked.Count != 3 || len(parked.Issues) != parked.Count {
		t.Fatalf("the priorities read counts %d parked priority issues and draws %d, want all 3 drawn",
			parked.Count, len(parked.Issues))
	}

	unplacedListing := "/api/v1/issues?advances=none" + faultListingStatuses(model.IssueStatusesInFlight)
	if got, want := slices.Sorted(slices.Values(listedIssueKeys(t, handler, unplacedListing))),
		slices.Sorted(slices.Values(faultKeys(unplaced.Issues))); !slices.Equal(got, want) {
		t.Fatalf("%s answers %v, want the work in flight under no priority %v", unplacedListing, got, want)
	}

	parkedListing := "/api/v1/issues?advances=any&moved=true" + faultListingStatuses(model.IssueStatusesParked)
	response := dispatchRequest(t, handler, http.MethodGet, parkedListing, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list %s: status=%d body=%s", parkedListing, response.Code, response.Body.String())
	}
	rows := map[string]model.IssueSummary{}
	for _, row := range decodeBody[[]model.IssueSummary](t, response) {
		rows[row.Key] = row
	}
	if got, want := slices.Sorted(maps.Keys(rows)), slices.Sorted(slices.Values(faultKeys(parked.Issues))); !slices.Equal(got, want) {
		t.Fatalf("%s answers %v, want the parked priority work %v", parkedListing, got, want)
	}
	for _, fault := range parked.Issues {
		row := rows[fault.Key]
		sameAt := (row.MovedAt == nil) == (fault.MovedAt == nil) && (row.MovedAt == nil || row.MovedAt.Equal(*fault.MovedAt))
		sameBy := (row.MovedBy == nil) == (fault.MovedBy == nil) && (row.MovedBy == nil || *row.MovedBy == *fault.MovedBy)
		if !sameAt || !sameBy {
			t.Fatalf("%s says %s was moved at %v by %v, where the parked list says %v by %v",
				parkedListing, fault.Key, row.MovedAt, row.MovedBy, fault.MovedAt, fault.MovedBy)
		}
	}
	if row := rows[parkedChild]; row.MovedBy == nil || *row.MovedBy != (model.Actor{Kind: "user", ID: "bob"}) {
		t.Fatalf("%s says %s was moved by %v, want bob, so the comparison above is of a recorded move", parkedListing, parkedChild, row.MovedBy)
	}
	if row := rows[unrecorded]; row.MovedAt != nil || row.MovedBy != nil {
		t.Fatalf("%s says %s was moved at %v by %v, want neither: no event records its move", parkedListing, unrecorded, row.MovedAt, row.MovedBy)
	}
}
