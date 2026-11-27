package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// The tests of the live hub's items view (dispatch://INFRA-2918, D9): one eval's rows, the body of
// GET /api/v1/projects/{key}/evals/{slug}/items, read through the route's own function and moved
// by its eval's writes and, for pr-ci's run and job rows, by a change of a pull-request checks run.

// itemsLiveView is a subscribe frame's view of the rows of project's eval slug over window, with
// query the rest of what the items route would be sent.
func itemsLiveView(project, slug string, window DeliveryLiveWindow, query string) DeliveryLiveView {
	return DeliveryLiveView{Kind: evalItemsView, Project: &project, Slug: &slug, Window: &window, Query: &query}
}

// itemsViewOf is the held view of itemsLiveView's frame, as the hub resolves it, with no answer yet.
func itemsViewOf(t *testing.T, project, slug string, window DeliveryLiveWindow, query string) *deliveryView {
	t.Helper()
	spec, refusal := parseDeliveryLiveView(itemsLiveView(project, slug, window, query))
	if refusal != nil {
		t.Fatalf("read the items view of %s/%s with %q: %+v", project, slug, query, refusal)
	}
	return &deliveryView{spec: spec}
}

// itemsTarget is the items route of project's eval slug for the days an items body was read over,
// whose window names the midnight of its first day and the one after its last, with the rest of
// the view's query.
func itemsTarget(t *testing.T, project, slug string, body json.RawMessage, query string) string {
	t.Helper()
	var shape struct {
		Window evalRange `json:"window"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("read the items body's window: %v", err)
	}
	target := evalItemsPath(project, slug) + "?" + url.Values{
		"from": {shape.Window.From.Format(time.RFC3339)}, "to": {shape.Window.To.Format(time.RFC3339)},
	}.Encode()
	if query != "" {
		target += "&" + query
	}
	return target
}

// requireTheItemsRead fails unless body is, byte for byte, what the items route answers for the
// days it was read over and query.
func requireTheItemsRead(t *testing.T, handler http.Handler, project, slug string, body json.RawMessage, query string) {
	t.Helper()
	target := itemsTarget(t, project, slug, body, query)
	route := []byte(strings.TrimSuffix(string(getBody(t, handler, target)), "\n"))
	if string(body) != string(route) {
		t.Fatalf("the live body differs from GET %s:\nlive: %s\nGET:  %s", target, body, route)
	}
}

// requireTheDeltaLandsOnTheItemsRead fails unless delta, applied to held as a page applies it,
// gives the items route's body for query; it answers the body the page then holds.
func requireTheDeltaLandsOnTheItemsRead(t *testing.T, handler http.Handler, project, slug string, held json.RawMessage, delta DeliveryLiveDelta, query string) json.RawMessage {
	t.Helper()
	applied := applyLiveDelta(t, evalItemsView, held, delta)
	route := mustEncodeLive(t, evalItemsView, getBody(t, handler, itemsTarget(t, project, slug, applied, query)))
	if got, want := liveComparable(t, mustEncodeLive(t, evalItemsView, applied)), liveComparable(t, route); !reflect.DeepEqual(got, want) {
		t.Fatalf("the delta does not land on the route's body:\napplied: %v\nGET:     %v", got, want)
	}
	return applied
}

// itemsRowKeys is the row keys an items body holds, in its order.
func itemsRowKeys(t *testing.T, body json.RawMessage) []string {
	t.Helper()
	var answer evalItemsResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode an items body: %v", err)
	}
	keys := make([]string, len(answer.Items))
	for index, row := range answer.Items {
		keys[index] = row.RowKey
	}
	return keys
}

// TestTheItemsViewAnswersTheItemsRoutesBody: an items view's snapshot is, byte for byte, the body
// GET /api/v1/projects/{key}/evals/{slug}/items answers for its eval, the days its window covers
// and the rest of its query: a custom range with a compare range, and a preset with a row kind and
// a page. Every field of it is sent whole, so its revisions hold fields alone. A reading of the eval
// sends each view the delta that lands its body on the route's. A view of an eval its project does
// not hold is told EVAL_NOT_FOUND once, as the route answers, and is answered once a reading
// registers the eval.
func TestTheItemsViewAnswersTheItemsRoutesBody(t *testing.T) {
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "INFRA")
	now := rig.clock.Now()
	today := measures.Midnight(now)
	logged := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/review-time/readings",
		map[string]any{"readings": []any{
			reading("minutes", 10, now.Add(-time.Hour)), reading("minutes", 25, now.Add(-49*time.Hour)),
			reading("minutes", 40, now.Add(-30*24*time.Hour)),
		}}, "items-view-readings")
	if logged.Code != http.StatusCreated {
		t.Fatalf("log the readings: status %d, body %s", logged.Code, logged.Body.String())
	}

	compare := url.Values{"compare_from": {today.AddDate(0, 0, -55).Format(time.RFC3339)}, "compare_to": {today.AddDate(0, 0, -27).Format(time.RFC3339)}}.Encode()
	filtered := url.Values{"row_kind": {evalRowDay}, "limit": {"5"}}.Encode()
	frames := map[string]DeliveryLiveView{
		"custom":   itemsLiveView("INFRA", "review-time", evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), compare),
		"filtered": itemsLiveView("INFRA", "review-time", DeliveryLiveWindow{Preset: new(7)}, filtered),
	}
	conns, held := map[string]*fakeLiveConn{}, map[string]json.RawMessage{}
	for _, sub := range []string{"custom", "filtered"} {
		view := frames[sub]
		conns[sub] = newFakeLiveConn()
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, view))
		snapshot := conns[sub].snapshot(t, sub)
		requireTheItemsRead(t, rig.handler, "INFRA", "review-time", snapshot.Body, *view.Query)
		if len(snapshot.Revs.Items)+len(snapshot.Revs.Maps)+len(snapshot.Revs.Counts) != 0 {
			t.Errorf("%s: the snapshot's revisions hold items %v, maps %v and counts %v, want its fields alone", sub,
				snapshot.Revs.Items, snapshot.Revs.Maps, snapshot.Revs.Counts)
		}
		if fields := slices.Sorted(maps.Keys(snapshot.Revs.Fields)); !slices.Equal(fields, []string{"compare", "items", "limit", "offset", "total", "window"}) {
			t.Errorf("%s: the snapshot's revisions hold the fields %v, want every field of the body", sub, fields)
		}
		if len(itemsRowKeys(t, snapshot.Body)) == 0 {
			t.Errorf("%s: the snapshot holds no row, so the check compares nothing", sub)
		}
		held[sub] = snapshot.Body
		t.Logf("%s: the snapshot is the route's %d bytes, rows %v", sub, len(snapshot.Body), itemsRowKeys(t, snapshot.Body))
	}
	rig.settle(t)

	written := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/review-time/readings",
		map[string]any{"readings": []any{reading("minutes", 55, now.Add(-2*time.Hour))}}, "items-view-reading")
	if written.Code != http.StatusCreated {
		t.Fatalf("log the reading: status %d, body %s", written.Code, written.Body.String())
	}
	for _, sub := range []string{"custom", "filtered"} {
		delta := conns[sub].delta(t, sub)
		if set, _, _, _ := liveSections(delta); !slices.Contains(set, "items") {
			t.Errorf("%s: the reading's delta sets %v, want the rows", sub, set)
		}
		applied := requireTheDeltaLandsOnTheItemsRead(t, rig.handler, "INFRA", "review-time", held[sub], delta, *frames[sub].Query)
		if slices.Equal(itemsRowKeys(t, applied), itemsRowKeys(t, held[sub])) && sub == "custom" {
			t.Errorf("%s: the reading's delta leaves the rows %v as they were", sub, itemsRowKeys(t, applied))
		}
	}

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("new", itemsLiveView("INFRA", "pr-size", DeliveryLiveWindow{Preset: new(7)}, "")))
	routed := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/INFRA/evals/pr-size/items", nil, "alice")
	answer := decodeBody[refusalAnswer](t, routed)
	if routed.Code != http.StatusNotFound || answer.Code != codeEvalNotFound {
		t.Fatalf("GET the items of pr-size: status %d, body %s; want 404 %s", routed.Code, routed.Body.String(), codeEvalNotFound)
	}
	if refusal := conn.refusal(t, "new", answer.Code); refusal.Message != answer.Error {
		t.Errorf("the refusal's message = %q, want the route's %q", refusal.Message, answer.Error)
	}
	conn.none(t, time.Second, "the refusal of an eval no row holds")
	registered := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/pr-size/readings",
		map[string]any{"readings": []any{reading("lines", 120, now.Add(-time.Hour))}}, "items-view-register")
	if registered.Code != http.StatusCreated {
		t.Fatalf("log pr-size's reading: status %d, body %s", registered.Code, registered.Body.String())
	}
	snapshot := conn.snapshot(t, "new")
	requireTheItemsRead(t, rig.handler, "INFRA", "pr-size", snapshot.Body, "")
	if len(itemsRowKeys(t, snapshot.Body)) == 0 {
		t.Error("the snapshot after the registering reading holds no row")
	}
}

// TestAReadingMarksTheItemsViewOfItsEvalAlone: a reading, a version of a definition and a day row
// of an eval each mark the items views of that eval, whatever their window, row kind or compare
// range, and no items view of another eval, of the project's or of another project's; the
// project's evals view is marked too, and no delivery view. The refresher's word that the rows
// stand at the delivery settings, an eval_days change of no eval, marks every items view, and the
// settings row itself none. No other kind of change marks an items view of a logged eval. Then,
// through the change log, one readings write recomputes the one items view of its eval among
// three, whose delta lands on the route's body, and sends the other two nothing.
func TestAReadingMarksTheItemsViewOfItsEvalAlone(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	today := measures.Midnight(now)
	hourAgo, dayAgo := now.Add(-time.Hour), now.Add(-24*time.Hour)
	compare := url.Values{"compare_from": {today.AddDate(0, 0, -55).Format(time.RFC3339)}, "compare_to": {today.AddDate(0, 0, -27).Format(time.RFC3339)}}.Encode()
	views := []struct {
		what string
		view *deliveryView
		// marked is whether a change of INFRA/review-time marks the view.
		marked bool
	}{
		{"INFRA's review-time over 28 days", itemsViewOf(t, "INFRA", "review-time", DeliveryLiveWindow{Preset: new(28)}, ""), true},
		{"INFRA's review-time over a custom range with a compare range", itemsViewOf(t, "INFRA", "review-time",
			evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), compare), true},
		{"INFRA's review-time's day rows", itemsViewOf(t, "INFRA", "review-time", DeliveryLiveWindow{Preset: new(7)}, "row_kind=day"), true},
		{"INFRA's pr-size", itemsViewOf(t, "INFRA", "pr-size", DeliveryLiveWindow{Preset: new(28)}, ""), false},
		{"INFRA's review-time-2", itemsViewOf(t, "INFRA", "review-time-2", DeliveryLiveWindow{Preset: new(28)}, ""), false},
		{"OTHER's review-time", itemsViewOf(t, "OTHER", "review-time", DeliveryLiveWindow{Preset: new(28)}, ""), false},
		{"INFRA's evals", evalsViewOf("INFRA", 28, time.Time{}, time.Time{}), true},
		{"a timeline", deliveryViewShowing(deliveryLiveTimeline, "INFRA-7"), false},
		{"a pipeline view", deliveryViewShowing(deliveryLivePipeline, "INFRA-7"), false},
	}
	for _, change := range []delivery.Change{
		evalsChange(delivery.ChangeKindEvalReadings, "INFRA", "review-time", &dayAgo, &hourAgo),
		evalsChange(delivery.ChangeKindEvalVersions, "INFRA", "review-time", nil, nil),
		evalsChange(delivery.ChangeKindEvalDays, "INFRA", "review-time", &today, &today),
	} {
		marked := []string{}
		for _, view := range views {
			got := view.view.touchedBy(change, now)
			if got != view.marked {
				t.Errorf("%s of %s: touchedBy(%s) = %t, want %t", change.Kind, change.Ref, view.what, got, view.marked)
			}
			if got {
				marked = append(marked, view.what)
			}
		}
		t.Logf("%s of %s marks %d of %d views: %v", change.Kind, change.Ref, len(marked), len(views), marked)
	}

	settingsRow := delivery.Change{Seq: 2, Kind: delivery.ChangeKindSettings}
	applied := delivery.Change{Seq: 3, Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenBySettingsRewrite}
	others := []delivery.Change{
		{Seq: 1, Kind: delivery.ChangeKindPullRequest, Repo: "acme/widgets", Ref: "1", TNew: &hourAgo},
		{Seq: 1, Kind: delivery.ChangeKindRun, RunKind: string(delivery.DeliveryRunKindDeploy), Repo: "acme/widgets", Ref: "7", TNew: &hourAgo},
		{Seq: 1, Kind: delivery.ChangeKindRunJob, RunKind: string(delivery.DeliveryRunKindPRChecks), Repo: "acme/widgets", Ref: "8", TNew: &hourAgo},
		{Seq: 1, Kind: delivery.ChangeKindSessionGroup, Ref: "g-1"},
		{Seq: 1, Kind: delivery.ChangeKindIssueFilings, Repo: "INFRA", Ref: "INFRA-7", TNew: &hourAgo},
		{Seq: 1, Kind: delivery.ChangeKindProductionEvents, Repo: "INFRA", Ref: "0193a1b2-0000-7000-8000-000000000001", TNew: &hourAgo},
		issueFactsChange("INFRA", "INFRA-7", dayAgo, hourAgo),
		settingsRow,
	}
	for _, view := range views {
		if view.view.spec.kind != evalItemsView {
			continue
		}
		if !view.view.touchedBy(applied, now) {
			t.Errorf("the settings applied: touchedBy(%s) = false, want true: an items view reads the settings its rows stand built under", view.what)
		}
		for _, change := range others {
			if view.view.touchedBy(change, now) {
				t.Errorf("%s %s/%s marks %s, want no items view of a logged eval", change.Kind, change.Repo, change.Ref, view.what)
			}
		}
	}

	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "INFRA", "OTHER")
	at := rig.clock.Now()
	for _, logged := range []struct{ project, slug, series, key string }{
		{"INFRA", "review-time", "minutes", "alone-1"}, {"INFRA", "pr-size", "lines", "alone-2"}, {"OTHER", "review-time", "minutes", "alone-3"},
	} {
		response := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/"+logged.project+"/evals/"+logged.slug+"/readings",
			map[string]any{"readings": []any{reading(logged.series, 10, at.Add(-3*time.Hour))}}, logged.key)
		if response.Code != http.StatusCreated {
			t.Fatalf("log a reading of %s/%s: status %d, body %s", logged.project, logged.slug, response.Code, response.Body.String())
		}
	}
	frames := map[string]DeliveryLiveView{
		"r": itemsLiveView("INFRA", "review-time", DeliveryLiveWindow{Preset: new(7)}, ""),
		"p": itemsLiveView("INFRA", "pr-size", DeliveryLiveWindow{Preset: new(7)}, ""),
		"o": itemsLiveView("OTHER", "review-time", DeliveryLiveWindow{Preset: new(7)}, ""),
	}
	conns, held, subOf := map[string]*fakeLiveConn{}, map[string]json.RawMessage{}, map[string]string{}
	for _, sub := range []string{"r", "p", "o"} {
		spec, refusal := parseDeliveryLiveView(frames[sub])
		if refusal != nil {
			t.Fatalf("read the view of %s: %+v", sub, refusal)
		}
		subOf[spec.key] = sub
		conns[sub] = newFakeLiveConn()
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, frames[sub]))
		held[sub] = conns[sub].snapshot(t, sub).Body
	}
	rig.settle(t)
	mark := rig.mark()
	written := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/review-time/readings",
		map[string]any{"readings": []any{reading("minutes", 30, at.Add(-time.Hour))}}, "alone-reading")
	if written.Code != http.StatusCreated {
		t.Fatalf("log the reading: status %d, body %s", written.Code, written.Body.String())
	}
	rig.hub.catchUpNow()
	rig.waitFor(t, mark, "the recompute of INFRA's review-time items after its reading", recomputeOfView(t, frames["r"]))
	rig.waitNoRecompute(t, viewMaxWait)
	recomputed := []string{}
	rig.mu.Lock()
	for _, note := range rig.notes[mark:] {
		if note.what == "recompute begin" && !slices.Contains(recomputed, subOf[note.view]) {
			recomputed = append(recomputed, subOf[note.view])
		}
	}
	rig.mu.Unlock()
	if !slices.Equal(recomputed, []string{"r"}) {
		t.Errorf("the reading of INFRA's review-time recomputed the items views of %v, want r alone", recomputed)
	}
	delta := conns["r"].delta(t, "r")
	requireTheDeltaLandsOnTheItemsRead(t, rig.handler, "INFRA", "review-time", held["r"], delta, "")
	conns["p"].none(t, 100*time.Millisecond, "the reading of another eval")
	conns["o"].none(t, 100*time.Millisecond, "the reading of another project's eval")
}

// TestARunJobChangeMarksThePrCiJobsView: pr-ci's run and job rows are read from the pull-request
// checks runs and their jobs themselves, so a change of such a run or of one of its jobs on a day of
// one of a pr-ci items view's ranges marks the view when it keeps run or job rows (a sort, which
// keeps the job rows alone, row_kind job or run, or no row kind), and one that carries no time
// marks it too; a change of a run on no day of its ranges, a deploy run's, and every such change
// for a view of pr-ci's day rows or readings alone, or of another eval, marks nothing. Then,
// through the change log, a job of a counted run that fails moves the failures list a live page
// holds: the job change recomputes the failures view and not the day rows' view of the same eval,
// and its delta lands on the route's body, now naming the job.
func TestARunJobChangeMarksThePrCiJobsView(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	today := measures.Midnight(now)
	hourAgo := now.Add(-time.Hour)
	inCompare, outside := today.AddDate(0, 0, -40).Add(9*time.Hour), today.AddDate(0, 0, -100)
	compare := url.Values{"compare_from": {today.AddDate(0, 0, -55).Format(time.RFC3339)}, "compare_to": {today.AddDate(0, 0, -27).Format(time.RFC3339)}}.Encode()
	custom := evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1))
	views := []struct {
		what string
		view *deliveryView
		// window and compared are whether a pull-request checks change in the window, and in the
		// compare range, marks the view.
		window, compared bool
	}{
		{"pr-ci's failing jobs", itemsViewOf(t, "INFRA", evals.SlugPRCI, DeliveryLiveWindow{Preset: new(7)}, "sort=failures"), true, false},
		{"pr-ci's slowest jobs", itemsViewOf(t, "INFRA", evals.SlugPRCI, DeliveryLiveWindow{Preset: new(28)}, "sort=median"), true, false},
		{"pr-ci's job rows with a compare range", itemsViewOf(t, "INFRA", evals.SlugPRCI, custom, "row_kind=job&"+compare), true, true},
		{"pr-ci's run rows", itemsViewOf(t, "INFRA", evals.SlugPRCI, DeliveryLiveWindow{Preset: new(7)}, "row_kind=run"), true, false},
		{"pr-ci's every row", itemsViewOf(t, "INFRA", evals.SlugPRCI, DeliveryLiveWindow{Preset: new(7)}, ""), true, false},
		{"pr-ci's day rows", itemsViewOf(t, "INFRA", evals.SlugPRCI, DeliveryLiveWindow{Preset: new(7)}, "row_kind=day"), false, false},
		{"pr-ci's readings", itemsViewOf(t, "INFRA", evals.SlugPRCI, DeliveryLiveWindow{Preset: new(7)}, "row_kind=sample"), false, false},
		{"staging-e2e's run rows", itemsViewOf(t, "INFRA", evals.SlugStagingE2E, DeliveryLiveWindow{Preset: new(7)}, ""), false, false},
		{"review-time's rows", itemsViewOf(t, "INFRA", "review-time", DeliveryLiveWindow{Preset: new(7)}, ""), false, false},
	}
	checks := string(delivery.DeliveryRunKindPRChecks)
	cases := []struct {
		what   string
		change delivery.Change
		marks  func(window, compared bool) bool
	}{
		{"a job of a run of the window", delivery.Change{Seq: 1, Kind: delivery.ChangeKindRunJob, RunKind: checks, Repo: "acme/widgets", Ref: "8", TOld: &hourAgo, TNew: &hourAgo},
			func(window, _ bool) bool { return window }},
		{"a run of the window", delivery.Change{Seq: 1, Kind: delivery.ChangeKindRun, RunKind: checks, Repo: "acme/widgets", Ref: "8", TNew: &hourAgo},
			func(window, _ bool) bool { return window }},
		{"a run with no time", delivery.Change{Seq: 1, Kind: delivery.ChangeKindRun, RunKind: checks, Repo: "acme/widgets", Ref: "9"},
			func(window, _ bool) bool { return window }},
		{"a job of a run of the compare range", delivery.Change{Seq: 1, Kind: delivery.ChangeKindRunJob, RunKind: checks, Repo: "acme/widgets", Ref: "5", TOld: &inCompare, TNew: &inCompare},
			func(_, compared bool) bool { return compared }},
		{"a run of no range", delivery.Change{Seq: 1, Kind: delivery.ChangeKindRun, RunKind: checks, Repo: "acme/widgets", Ref: "4", TNew: &outside},
			func(bool, bool) bool { return false }},
		{"a job of a deploy run of the window", delivery.Change{Seq: 1, Kind: delivery.ChangeKindRunJob, RunKind: string(delivery.DeliveryRunKindDeploy), Repo: "acme/widgets", Ref: "7", TOld: &hourAgo, TNew: &hourAgo},
			func(bool, bool) bool { return false }},
		{"a pull request of the window", delivery.Change{Seq: 1, Kind: delivery.ChangeKindPullRequest, Repo: "acme/widgets", Ref: "1", TNew: &hourAgo},
			func(bool, bool) bool { return false }},
	}
	for _, c := range cases {
		marked := []string{}
		for _, view := range views {
			got, want := view.view.touchedBy(c.change, now), c.marks(view.window, view.compared)
			if got != want {
				t.Errorf("%s: touchedBy(%s) = %t, want %t", c.what, view.what, got, want)
			}
			if got {
				marked = append(marked, view.what)
			}
		}
		t.Logf("%s marks %v", c.what, marked)
	}

	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "HUB")
	ctx := t.Context()
	at := rig.clock.Now()
	day := measures.Midnight(at)
	issue := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "HUB", "title": "Checks"}, "alice")
	if issue.Code != http.StatusCreated {
		t.Fatalf("open HUB's issue: status %d, body %s", issue.Code, issue.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key
	pr := hubPR(7, at.Add(-5*time.Hour))
	pr.IssueKey = &key
	upsertPR(t, rig.database, pr)
	failure := delivery.DeliveryRunConclusionFailure
	jobFailure, jobSuccess := delivery.DeliveryJobConclusionFailure, delivery.DeliveryJobConclusionSuccess
	const runID = 700
	started, done := at.Add(-3*time.Hour), at.Add(-2*time.Hour)
	if err := delivery.UpsertRun(ctx, rig.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: runID, Kind: delivery.DeliveryRunKindPRChecks, HeadSHA: "sha700", HeadCommitAt: started.Add(-time.Minute),
		StartedAt: started, CompletedAt: &done, Conclusion: &failure, PRNumber: new(7),
		URL: "https://github.com/acme/widgets/actions/runs/700", HeadBranch: new("feature"), Event: new("pull_request"),
	}); err != nil {
		t.Fatalf("upsert PR-checks run %d: %v", runID, err)
	}
	jobs := func(steady *delivery.DeliveryJobConclusion) []delivery.DeliveryRunJob {
		return []delivery.DeliveryRunJob{
			{Repo: "acme/widgets", RunID: runID, Name: "checks / flaky", StartedAt: &started, CompletedAt: &done, Conclusion: &jobFailure},
			{Repo: "acme/widgets", RunID: runID, Name: "checks / steady", StartedAt: &started, CompletedAt: &done, Conclusion: steady},
		}
	}
	if err := delivery.UpsertRunJobs(ctx, rig.database.Pool, "acme/widgets", runID, jobs(&jobSuccess), true); err != nil {
		t.Fatalf("upsert PR-checks run %d's jobs: %v", runID, err)
	}
	refreshEvalDays(t, rig.hub.server.registry(), rig.database, at)

	window := evalsDays(day.AddDate(0, 0, -6), day.AddDate(0, 0, 1))
	frames := map[string]DeliveryLiveView{
		"f": itemsLiveView("HUB", evals.SlugPRCI, window, "sort=failures&limit=200"),
		"d": itemsLiveView("HUB", evals.SlugPRCI, window, "row_kind=day"),
	}
	conns, held, subOf := map[string]*fakeLiveConn{}, map[string]json.RawMessage{}, map[string]string{}
	for _, sub := range []string{"f", "d"} {
		spec, refusal := parseDeliveryLiveView(frames[sub])
		if refusal != nil {
			t.Fatalf("read the view of %s: %+v", sub, refusal)
		}
		subOf[spec.key] = sub
		conns[sub] = newFakeLiveConn()
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, frames[sub]))
		held[sub] = conns[sub].snapshot(t, sub).Body
		requireTheItemsRead(t, rig.handler, "HUB", evals.SlugPRCI, held[sub], *frames[sub].Query)
	}
	if got := itemsRowKeys(t, held["f"]); !slices.Equal(got, []string{failingJobKey("acme/widgets", "checks / flaky")}) {
		t.Fatalf("the failures view holds %v, want the one job that failed", got)
	}
	rig.settle(t)

	mark := rig.mark()
	if err := delivery.UpsertRunJobs(ctx, rig.database.Pool, "acme/widgets", runID, jobs(&jobFailure), true); err != nil {
		t.Fatalf("fail run %d's steady job: %v", runID, err)
	}
	rig.hub.catchUpNow()
	rig.waitFor(t, mark, "the recompute of pr-ci's failures after a job failed", recomputeOfView(t, frames["f"]))
	rig.waitNoRecompute(t, viewMaxWait)
	recomputed := []string{}
	rig.mu.Lock()
	for _, note := range rig.notes[mark:] {
		if note.what == "recompute begin" && !slices.Contains(recomputed, subOf[note.view]) {
			recomputed = append(recomputed, subOf[note.view])
		}
	}
	rig.mu.Unlock()
	if !slices.Equal(recomputed, []string{"f"}) {
		t.Errorf("the job's change recomputed the items views of %v, want the failures view alone", recomputed)
	}
	delta := conns["f"].delta(t, "f")
	applied := requireTheDeltaLandsOnTheItemsRead(t, rig.handler, "HUB", evals.SlugPRCI, held["f"], delta, *frames["f"].Query)
	if got, want := itemsRowKeys(t, applied), []string{failingJobKey("acme/widgets", "checks / flaky"), failingJobKey("acme/widgets", "checks / steady")}; !slices.Equal(got, want) {
		t.Errorf("after the steady job failed the failures view holds %v, want both jobs", got)
	}
	conns["d"].none(t, 100*time.Millisecond, "the job's change")
}

// TestTheFailuresItemsViewDropsAJobWhenItLeavesThe24Hours: pr-ci's failures list (sort=failures)
// counts the jobs that failed in the 24 hours before its range's end, and a preset's range ends at
// now, so the clock alone moves it. A job that failed in a run completed 23 h 59 min before now is
// listed, and the preset view expires just past that completion + measures.FailingWindow, by the
// Pipeline's failing-jobs rule (failingJobsNextExpiry) over the runs the list counts; a custom
// range listing the same job, whose end the clock does not move, sets no expiry. When the clock
// reaches the preset view's expiry the view reads its list again, and the frame it sends lists the
// job no more, while the custom range is sent nothing.
func TestTheFailuresItemsViewDropsAJobWhenItLeavesThe24Hours(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Checks"})
	ctx := t.Context()
	// Whole seconds, so the times Postgres stores are the ones the test compares with.
	at := rig.clock.Now().Truncate(time.Second)
	completed := at.Add(-23*time.Hour - 59*time.Minute)
	started := completed.Add(-30 * time.Minute)
	upsertPR(t, rig.database, prNaming(7, completed.Add(time.Hour), key))
	const runID = 710
	failure, jobFailure := delivery.DeliveryRunConclusionFailure, delivery.DeliveryJobConclusionFailure
	if err := delivery.UpsertRun(ctx, rig.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: runID, Kind: delivery.DeliveryRunKindPRChecks, HeadSHA: "sha710", HeadCommitAt: started.Add(-time.Minute),
		StartedAt: started, CompletedAt: &completed, Conclusion: &failure, PRNumber: new(7),
		URL: "https://github.com/acme/widgets/actions/runs/710", HeadBranch: new("feature"), Event: new("pull_request"),
	}); err != nil {
		t.Fatalf("upsert PR-checks run %d: %v", runID, err)
	}
	if err := delivery.UpsertRunJobs(ctx, rig.database.Pool, "acme/widgets", runID, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: runID, Name: "checks / flaky", StartedAt: &started, CompletedAt: &completed, Conclusion: &jobFailure},
	}, true); err != nil {
		t.Fatalf("upsert PR-checks run %d's job: %v", runID, err)
	}
	refreshEvalDays(t, rig.hub.server.registry(), rig.database, at)

	frames := map[string]DeliveryLiveView{
		"preset": itemsLiveView("HUB", evals.SlugPRCI, DeliveryLiveWindow{Preset: new(7)}, "sort=failures"),
		"custom": itemsLiveView("HUB", evals.SlugPRCI, evalsDays(at.AddDate(0, 0, -7), at), "sort=failures"),
	}
	keys, conns, held := map[string]string{}, map[string]*fakeLiveConn{}, map[string]json.RawMessage{}
	for _, sub := range []string{"preset", "custom"} {
		spec, refusal := parseDeliveryLiveView(frames[sub])
		if refusal != nil {
			t.Fatalf("read the view of %s: %+v", sub, refusal)
		}
		keys[sub] = spec.key
		conns[sub] = newFakeLiveConn()
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, frames[sub]))
		held[sub] = conns[sub].snapshot(t, sub).Body
		if got := itemsRowKeys(t, held[sub]); !slices.Equal(got, []string{failingJobKey("acme/widgets", "checks / flaky")}) {
			t.Fatalf("%s: the failures view holds %v, want the job that failed 23 h 59 min ago", sub, got)
		}
	}
	rig.settle(t)
	expiryOf := func(sub string) time.Time {
		rig.hub.mu.Lock()
		defer rig.hub.mu.Unlock()
		return rig.hub.generation.views[keys[sub]].expiry
	}
	leaves := completed.Add(measures.FailingWindow).Add(time.Nanosecond)
	if got := expiryOf("preset"); !got.Equal(leaves) {
		t.Fatalf("the preset failures view expires at %s, want %s, just past the run's completion + 24 h", got.Format(time.RFC3339Nano), leaves.Format(time.RFC3339Nano))
	}
	if got := expiryOf("custom"); !got.IsZero() {
		t.Errorf("the custom range's failures view expires at %s, want never: the clock does not move its end", got.Format(time.RFC3339Nano))
	}

	mark := rig.mark()
	ahead := leaves.Sub(rig.clock.Now())
	if ahead <= 0 {
		t.Fatalf("the clock already reads %s, past the expiry %s the test moves it to", rig.clock.Now().Format(time.RFC3339Nano), leaves.Format(time.RFC3339Nano))
	}
	rig.clock.Advance(ahead)
	rig.waitFor(t, mark, "the preset failures view's read at its expiry", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.view == keys["preset"] && note.cause == deliveryCauseExpiry
	})

	delta := conns["preset"].delta(t, "preset")
	if set, _, _, _ := liveSections(delta); !slices.Contains(set, "items") {
		t.Errorf("the expiry's delta sets %v, want the rows", set)
	}
	if got := itemsRowKeys(t, applyLiveDelta(t, evalItemsView, held["preset"], delta)); len(got) != 0 {
		t.Errorf("past the run's completion + 24 h the failures view holds %v, want no job", got)
	}
	conns["custom"].none(t, 100*time.Millisecond, "the preset view's expiry")
}

// TestTheItemsViewParsesAsTheItemsRoute: an items view is read as the items route reads its query,
// so a query the route takes is held, with the project and the eval it names and a key of its own,
// and a query the route refuses is refused with the route's code and words. The view's own shape
// is held where every kind's is: an items view names its project, its eval's slug and its window,
// and no other kind names a slug.
func TestTheItemsViewParsesAsTheItemsRoute(t *testing.T) {
	handler := newTestHandler(t)
	createTestProject(t, handler, "EVL")
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	window := evalsDays(from, to)
	routeQuery := func(query string) string {
		target := evalItemsPath("EVL", evals.SlugPRCI) + "?" + url.Values{"from": {*window.From}, "to": {*window.To}}.Encode()
		if query != "" {
			target += "&" + query
		}
		return target
	}
	keys := map[string]string{}
	for _, query := range []string{
		"",
		"sort=failures&series=job:build",
		"sort=median&limit=200",
		"row_kind=job&row=build&repo=acme/one&repo=acme/two&limit=5&offset=1",
		url.Values{"compare_from": {from.AddDate(0, 0, -7).Format(time.RFC3339)}, "compare_to": {from.Format(time.RFC3339)}}.Encode(),
	} {
		if routed := dispatchRequest(t, handler, http.MethodGet, routeQuery(query), nil, "alice"); routed.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s; want it taken", routeQuery(query), routed.Code, routed.Body.String())
		}
		spec, refusal := parseDeliveryLiveView(itemsLiveView("EVL", evals.SlugPRCI, window, query))
		if refusal != nil {
			t.Errorf("an items view of %q: refusal = %+v, want it held as the route takes it", query, refusal)
			continue
		}
		if spec.kind != evalItemsView || spec.project != "EVL" || spec.slug != evals.SlugPRCI || !strings.HasPrefix(spec.key, evalItemsView+"|EVL|"+evals.SlugPRCI+"|") {
			t.Errorf("an items view of %q resolves to %+v, want EVL's pr-ci under a key of its own", query, spec)
		}
		if other, taken := keys[spec.key]; taken {
			t.Errorf("the items views of %q and %q share the key %s", query, other, spec.key)
		}
		keys[spec.key] = query
	}
	other, refusal := parseDeliveryLiveView(itemsLiveView("EVL", evals.SlugStagingE2E, window, ""))
	if refusal != nil {
		t.Fatalf("an items view of staging-e2e: %+v", refusal)
	}
	if _, taken := keys[other.key]; taken {
		t.Errorf("the items views of two evals share the key %s", other.key)
	}

	for _, query := range []string{
		"kind=ci",
		"sort=median&sort=failures",
		"sort=fastest",
		"series=a&series=b",
		"row=a&row=b",
		"compare_from=" + url.QueryEscape(from.AddDate(0, 0, -7).Format(time.RFC3339)),
		"limit=0",
		"limit=201",
		"repo=",
	} {
		routed := dispatchRequest(t, handler, http.MethodGet, routeQuery(query), nil, "alice")
		answer := decodeBody[refusalAnswer](t, routed)
		if routed.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status %d, body %s; want it refused", routeQuery(query), routed.Code, routed.Body.String())
		}
		_, refusal := parseDeliveryLiveView(itemsLiveView("EVL", evals.SlugPRCI, window, query))
		if refusal == nil || refusal.Code != answer.Code || refusal.Message != answer.Error {
			t.Errorf("an items view of %q: refusal = %+v, want the route's %s %q", query, refusal, answer.Code, answer.Error)
		}
	}

	project, slug, empty, at := "EVL", evals.SlugPRCI, "", "2026-11-08T00:00:00Z"
	query := ""
	for _, refused := range []struct {
		what string
		view DeliveryLiveView
		code string
	}{
		{"an items view with no project", DeliveryLiveView{Kind: evalItemsView, Slug: &slug, Window: &window, Query: &query}, "INVALID_FRAME"},
		{"an items view with no slug", DeliveryLiveView{Kind: evalItemsView, Project: &project, Window: &window, Query: &query}, "INVALID_FRAME"},
		{"an items view with an empty slug", DeliveryLiveView{Kind: evalItemsView, Project: &project, Slug: &empty, Window: &window, Query: &query}, "INVALID_FRAME"},
		{"an items view with no window", DeliveryLiveView{Kind: evalItemsView, Project: &project, Slug: &slug, Query: &query}, "INVALID_FRAME"},
		{"an items view with an at", DeliveryLiveView{Kind: evalItemsView, Project: &project, Slug: &slug, Window: &window, At: &at}, "INVALID_FRAME"},
		{"an evals view with a slug", DeliveryLiveView{Kind: evalsView, Project: &project, Slug: &slug, Window: &window, Query: &query}, "INVALID_FRAME"},
		{"a timeline with a slug", DeliveryLiveView{Kind: deliveryLiveTimeline, Slug: &slug, Window: &window, Query: &query}, "INVALID_FRAME"},
		{"an items view of no project key", itemsLiveView("evl", slug, window, ""), "INVALID_QUERY"},
	} {
		if _, refusal := parseDeliveryLiveView(refused.view); refusal == nil || refusal.Code != refused.code {
			t.Errorf("%s: refusal = %+v, want %s", refused.what, refusal, refused.code)
		}
	}
}
