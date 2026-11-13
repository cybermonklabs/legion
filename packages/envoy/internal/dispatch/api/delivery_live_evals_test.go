package api

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// evalsChange is one delivery_changes row of an eval kind: the eval's project in Repo and the eval
// in Ref, with the span of moments the write put values in (migrations 20261113082200 to
// 20261113082202; a version's change carries none).
func evalsChange(kind, project, slug string, first, last *time.Time) delivery.Change {
	return delivery.Change{Seq: 1, Kind: kind, Repo: project, Ref: project + "/" + slug, TOld: first, TNew: last}
}

// evalsViewOf is an evals view of project, a preset of days or, with days 0, the custom range
// [from, to).
func evalsViewOf(project string, days int, from, to time.Time) *deliveryView {
	spec := deliveryLiveSpec{kind: evalsView, project: project, preset: days}
	if days == 0 {
		spec.from, spec.to = from, to
	}
	return &deliveryView{spec: spec}
}

// evalsLiveView is a subscribe frame's view of project's evals over window, with query the rest of
// what the list route would be sent.
func evalsLiveView(project string, window DeliveryLiveWindow, query string) DeliveryLiveView {
	return DeliveryLiveView{Kind: evalsView, Project: &project, Window: &window, Query: &query}
}

// evalsDays is the window of the UTC days [from, to), as a page names a custom range.
func evalsDays(from, to time.Time) DeliveryLiveWindow {
	f, tt := from.Format(time.RFC3339), to.Format(time.RFC3339)
	return DeliveryLiveWindow{From: &f, To: &tt}
}

// evalsTarget is the list route of project for the days an evals body folded, whose window names
// the midnight of its first day and the one after its last, with the rest of the view's query.
func evalsTarget(t *testing.T, project string, body json.RawMessage, query string) string {
	t.Helper()
	var shape struct {
		Window evalRange `json:"window"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("read the evals body's window: %v", err)
	}
	window := url.Values{"from": {shape.Window.From.Format(time.RFC3339)}, "to": {shape.Window.To.Format(time.RFC3339)}}.Encode()
	if query != "" {
		window += "&" + query
	}
	return "/api/v1/projects/" + project + "/evals?" + window
}

// requireTheListRead fails unless body is, byte for byte, what the list route answers for the days
// it folded and query; it answers the route's body.
func requireTheListRead(t *testing.T, handler http.Handler, project string, body json.RawMessage, query string) json.RawMessage {
	t.Helper()
	target := evalsTarget(t, project, body, query)
	route := bytes.TrimSuffix(getBody(t, handler, target), []byte("\n"))
	if !bytes.Equal(body, route) {
		t.Fatalf("the live body differs from GET %s:\nlive: %s\nGET:  %s", target, body, route)
	}
	return route
}

// TestAnEvalChangeTouchesTheEvalsViewsOfItsProjectAlone: a reading, a version of a definition and a
// day row each mark the evals views of their eval's project and no other view at all, and no other
// kind of change marks an evals view. A filing marks no view: the refresher's rewrite of the day's
// rows is what wakes the evals view. None of the four kinds reaches the delivery switch's closing
// `return true`, which would mark every timeline, measures and pipeline view.
func TestAnEvalChangeTouchesTheEvalsViewsOfItsProjectAlone(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	today := utcDayOf(now)
	hourAgo, dayAgo := now.Add(-time.Hour), now.Add(-24*time.Hour)

	views := []struct {
		what string
		view *deliveryView
		// infra is whether the view is an evals view of INFRA, which every eval change of INFRA marks.
		infra bool
	}{
		{"INFRA's evals over 28 days", evalsViewOf("INFRA", 28, time.Time{}, time.Time{}), true},
		{"INFRA's evals over a custom range", evalsViewOf("INFRA", 0, today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), true},
		{"OTHER's evals over 28 days", evalsViewOf("OTHER", 28, time.Time{}, time.Time{}), false},
		{"INFRA's Legion board", boardViewShowing("INFRA", "INFRA-7"), false},
		{"a timeline", deliveryViewShowing(deliveryLiveTimeline, "INFRA-7"), false},
		{"a measures view", deliveryViewShowing(deliveryLiveMeasures, "INFRA-7"), false},
		{"a pipeline view", deliveryViewShowing(deliveryLivePipeline, "INFRA-7"), false},
	}
	evalChanges := []delivery.Change{
		evalsChange("eval_readings", "INFRA", "pr-size", &dayAgo, &hourAgo),
		evalsChange("eval_versions", "INFRA", "deploy-frequency", nil, nil),
		evalsChange("eval_days", "INFRA", "deploy-frequency", &today, &today),
	}
	for _, change := range evalChanges {
		marked := []string{}
		for _, view := range views {
			got := view.view.touchedBy(change, now)
			if got != view.infra {
				t.Errorf("%s of %s: touchedBy(%s) = %t, want %t", change.Kind, change.Ref, view.what, got, view.infra)
			}
			if got {
				marked = append(marked, view.what)
			}
		}
		t.Logf("%s of %s marks %d of %d views: %v", change.Kind, change.Ref, len(marked), len(views), marked)
	}

	filing := delivery.Change{Seq: 1, Kind: "issue_filings", Repo: "INFRA", Ref: "INFRA-7", TNew: &hourAgo}
	filingMarks := 0
	for _, view := range views {
		if view.view.touchedBy(filing, now) {
			filingMarks++
			t.Errorf("issue_filings of INFRA-7 marks %s, want no view", view.what)
		}
	}

	// Every other kind, with and without the times it can carry: none reaches an evals view.
	others := []delivery.Change{
		{Kind: "pull_request", Repo: "acme/widgets", Ref: "1", TNew: &hourAgo},
		{Kind: "pull_request", Repo: "acme/widgets", Ref: "2"},
		{Kind: "run", RunKind: string(delivery.DeliveryRunKindDeploy), Repo: "acme/widgets", Ref: "7", TNew: &hourAgo},
		{Kind: "run", RunKind: string(delivery.DeliveryRunKindPRChecks), Repo: "acme/widgets", Ref: "8"},
		{Kind: "run_job", RunKind: string(delivery.DeliveryRunKindDeploy), Repo: "acme/widgets", Ref: "7", TNew: &hourAgo},
		{Kind: "session_group", Ref: "g-1"},
		{Kind: "settings"},
		{Kind: "settings", Repo: "acme/widgets"},
		issueFactsChange("INFRA", "INFRA-7", dayAgo, hourAgo),
	}
	evalsMarks := 0
	for _, change := range others {
		for _, view := range views {
			if view.view.spec.kind == evalsView && view.view.touchedBy(change, now) {
				evalsMarks++
				t.Errorf("%s %s/%s marks %s, want no evals view", change.Kind, change.Repo, change.Ref, view.what)
			}
		}
	}
	t.Logf("issue_filings marks %d of %d views; %d changes of every other kind make %d marks of an evals view",
		filingMarks, len(views), len(others), evalsMarks)
}

// TestAnEvalsViewIsMarkedByAP0WriteAndNotByTheSampler: an evals view shows the unowned-p0 eval,
// so an event of a P0 issue marks it, as it marks the timeline; an event of an issue that is no
// P0 marks neither. The sampler's read of the project's daemon, once a minute, marks the
// project's Legion board and leaves its evals view.
func TestAnEvalsViewIsMarkedByAP0WriteAndNotByTheSampler(t *testing.T) {
	hub := &deliveryHub{clock: newStillHubClock()}
	board, list, timeline := boardViewShowing("INFRA", "INFRA-7"), evalsViewOf("INFRA", 28, time.Time{}, time.Time{}),
		deliveryViewShowing(deliveryLiveTimeline)
	g := &hubGeneration{
		views:    map[string]*deliveryView{"board": board, "evals": list, "timeline": timeline},
		wakeWork: make(chan struct{}, 1),
		p0:       map[string]struct{}{},
	}
	clean := func() {
		for _, view := range g.views {
			view.dirty = false
		}
	}
	dirty := func() map[string]bool {
		marked := map[string]bool{}
		for key, view := range g.views {
			marked[key] = view.dirty
		}
		return marked
	}
	issueEvent := func(key string, priority int) model.Event {
		payload, err := json.Marshal(map[string]any{"key": key, "status": "todo", "priority": priority})
		if err != nil {
			t.Fatal(err)
		}
		return model.Event{Type: "issue.updated", IssueKey: &key, Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: json.RawMessage(payload)}
	}
	apply := func(event model.Event) map[string]bool {
		clean()
		hub.mu.Lock()
		hub.applyEventLocked(g, event, hub.clock.Now())
		hub.mu.Unlock()
		return dirty()
	}

	if got, want := apply(issueEvent("INFRA-9", 0)), map[string]bool{"board": false, "evals": true, "timeline": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("a P0's write marks %v, want %v", got, want)
	}
	if got, want := apply(issueEvent("INFRA-10", 2)), map[string]bool{"board": false, "evals": false, "timeline": false}; !reflect.DeepEqual(got, want) {
		t.Errorf("a P2's write marks %v, want %v", got, want)
	}
	clean()
	hub.dirtyLegionBoard(g, "INFRA")
	if got, want := dirty(), map[string]bool{"board": true, "evals": false, "timeline": false}; !reflect.DeepEqual(got, want) {
		t.Errorf("the sampler's read of INFRA's daemon marks %v, want %v", got, want)
	}
}

// TestAnEvalWriteRecomputesTheEvalsViewsOfItsProjectAlone: through the change log, each eval write
// recomputes the evals views of its project and no other view: a reading (of a computed eval, so
// the write logs its reading alone), a register write (a version), and a day row. A pull request
// recomputes the timeline and no evals view; the day row the refresher then writes for the pull
// request's day is what recomputes them, and sends them the eval it moved.
//
// Each step reads the kinds its write logged, so the step exercises the kind it names and no
// other, and counts every recompute that began after it, of any view.
func TestAnEvalWriteRecomputesTheEvalsViewsOfItsProjectAlone(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "INFRA")
	createTestProject(t, rig.handler, "OTHER")
	now := rig.clock.Now()
	today := utcDayOf(now)

	// The timeline is read first, so its read sets the hub's verdict and every later first answer
	// is followed by it.
	frames := map[string]DeliveryLiveView{
		"t": presetView(deliveryLiveTimeline, 7, ""),
		"a": evalsLiveView("INFRA", DeliveryLiveWindow{Preset: new(28)}, ""),
		"b": evalsLiveView("INFRA", evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)),
			url.Values{"compare_from": {today.AddDate(0, 0, -55).Format(time.RFC3339)}, "compare_to": {today.AddDate(0, 0, -27).Format(time.RFC3339)}}.Encode()),
		"o": evalsLiveView("OTHER", DeliveryLiveWindow{Preset: new(28)}, ""),
	}
	conns := map[string]*fakeLiveConn{}
	subOf := map[string]string{}
	for _, sub := range []string{"t", "a", "b", "o"} {
		spec, refusal := parseDeliveryLiveView(frames[sub])
		if refusal != nil {
			t.Fatalf("read the view of %s: %+v", sub, refusal)
		}
		subOf[spec.key] = sub
		conns[sub] = newFakeLiveConn()
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, frames[sub]))
		conns[sub].snapshot(t, 5*time.Second, sub)
		if sub != "t" {
			conns[sub].freshness(t, 5*time.Second, "the verdict after the snapshot of "+sub)
		}
	}
	rig.settle(t)

	// step writes, and requires the change kinds the write logged and the views recomputed after
	// it: each named one once its recompute has begun, and every one once the hub has gone quiet.
	step := func(what string, write func(), wantKinds, wantRecomputed []string) {
		t.Helper()
		before, mark := latestChange(t, rig.database), rig.mark()
		write()
		changes, err := delivery.ChangesSince(t.Context(), rig.database.Pool, before, 100)
		if err != nil {
			t.Fatalf("%s: read the changes it logged: %v", what, err)
		}
		var kinds []string
		for _, change := range changes {
			if !slices.Contains(kinds, change.Kind) {
				kinds = append(kinds, change.Kind)
			}
		}
		if !slices.Equal(kinds, wantKinds) {
			t.Fatalf("%s logged the kinds %v, want %v", what, kinds, wantKinds)
		}
		rig.hub.catchUpNow()
		for _, sub := range wantRecomputed {
			rig.waitFor(t, mark, 5*time.Second, "the recompute of "+sub+" after "+what, recomputeOfView(t, frames[sub]))
		}
		rig.waitNoRecompute(t, viewMaxWait)
		recomputed := []string{}
		rig.mu.Lock()
		for _, note := range rig.notes[mark:] {
			if note.what == "recompute begin" && !slices.Contains(recomputed, subOf[note.view]) {
				recomputed = append(recomputed, subOf[note.view])
			}
		}
		rig.mu.Unlock()
		slices.Sort(recomputed)
		if !slices.Equal(recomputed, wantRecomputed) {
			t.Errorf("%s recomputed the views of %v, want %v", what, recomputed, wantRecomputed)
		}
		t.Logf("%s logged %v and recomputed the views of %v", what, kinds, recomputed)
	}
	// sent reads the one delta each sub of want was sent, requiring the ids it upserts, and
	// requires that every other sub was sent nothing.
	sent := func(what string, want map[string][]string) {
		t.Helper()
		for _, sub := range []string{"a", "b", "o", "t"} {
			ids, ok := want[sub]
			if !ok {
				conns[sub].none(t, 100*time.Millisecond, what)
				continue
			}
			delta := conns[sub].delta(t, 5*time.Second, sub)
			list := "items"
			if sub == "t" {
				list = "prs"
			}
			if got := liveUpsertIDs(t, delta, list); !slices.Equal(got, ids) {
				t.Errorf("%s sent %s the %s %v, want %v", what, sub, list, got, ids)
			}
		}
	}

	step("a reading of deploy-frequency, a computed eval", func() {
		response := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/deploy-frequency/readings",
			map[string]any{"readings": []any{reading("deploys", 500, now.Add(-time.Minute))}}, "hub-computed-reading")
		if response.Code != http.StatusCreated {
			t.Fatalf("log the reading: status %d, body %s", response.Code, response.Body.String())
		}
	}, []string{"eval_readings"}, []string{"a", "b"})
	// The computed value reads its day rows alone, so the views' answers stand as they were.
	sent("the reading of a computed eval", nil)

	step("the register write of review-time", func() {
		response := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals",
			map[string]any{"slug": "review-time", "name": "Review time", "unit": "minutes"}, "hub-register")
		if response.Code != http.StatusCreated {
			t.Fatalf("register review-time: status %d, body %s", response.Code, response.Body.String())
		}
	}, []string{"eval_versions"}, []string{"a", "b"})
	sent("the register write", map[string][]string{"a": {"INFRA/review-time"}, "b": {"INFRA/review-time"}})

	merged := now.Add(-time.Hour)
	step("a pull request merged today", func() {
		upsertPR(t, rig.database, hubPR(1, merged))
	}, []string{"pull_request"}, []string{"t"})
	sent("the pull request", map[string][]string{"t": {"acme/widgets#1"}})

	step("the day row the refresher writes for the pull request's day", func() {
		if _, err := rig.database.Pool.Exec(t.Context(), `
			insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, counts, built_by)
			values ('INFRA', 'INFRA/deploy-frequency', 'deploys', '', $1, true, 1, 1, '{"with_prs": 1}', 'test')
		`, utcDayOf(merged)); err != nil {
			t.Fatalf("write deploy-frequency's day row: %v", err)
		}
	}, []string{"eval_days"}, []string{"a", "b"})
	sent("the day row", map[string][]string{"a": {"INFRA/deploy-frequency"}, "b": {"INFRA/deploy-frequency"}})
}

// TestAnEvalsViewsSnapshotIsTheListRead: an evals view's snapshot is, byte for byte, the body
// GET /api/v1/projects/{key}/evals answers for its project, the days its window covers and the
// rest of its query: a custom range with a compare range, and a preset with a kind, a repository
// and a page. Its list is sent by eval id. It sets no expiry, so nothing narrows it but a read.
// A view of a project no row holds is told PROJECT_NOT_FOUND once, as the route answers, and is
// answered once the project's creation seeds its evals.
func TestAnEvalsViewsSnapshotIsTheListRead(t *testing.T) {
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "INFRA")
	now := rig.clock.Now()
	today := utcDayOf(now)
	// Readings of a logged eval on two days, so the window folds day rows of its own.
	logged := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/review-time/readings",
		map[string]any{"readings": []any{reading("minutes", 10, now.Add(-time.Hour)), reading("minutes", 25, now.Add(-49*time.Hour))}}, "snapshot-readings")
	if logged.Code != http.StatusCreated {
		t.Fatalf("log the readings: status %d, body %s", logged.Code, logged.Body.String())
	}

	custom := url.Values{"compare_from": {today.AddDate(0, 0, -55).Format(time.RFC3339)}, "compare_to": {today.AddDate(0, 0, -27).Format(time.RFC3339)}}.Encode()
	filtered := url.Values{"kind": {"experiment"}, "repo": {"acme/widgets"}, "limit": {"5"}}.Encode()
	for sub, view := range map[string]DeliveryLiveView{
		"custom":   evalsLiveView("INFRA", evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), custom),
		"filtered": evalsLiveView("INFRA", DeliveryLiveWindow{Preset: new(7)}, filtered),
	} {
		conn := newFakeLiveConn()
		rig.hub.Subscribe(conn, subscribeFrame(sub, view))
		snapshot := conn.snapshot(t, 5*time.Second, sub)
		requireTheListRead(t, rig.handler, "INFRA", snapshot.Body, *view.Query)

		var body evalListResponse
		if err := json.Unmarshal(snapshot.Body, &body); err != nil {
			t.Fatalf("%s: decode the snapshot: %v", sub, err)
		}
		ids := []string{}
		for _, item := range body.Items {
			if item.ID != item.Definition.EvalID {
				t.Errorf("%s: an item's id is %q, its eval's %q", sub, item.ID, item.Definition.EvalID)
			}
			ids = append(ids, item.ID)
		}
		slices.Sort(ids)
		if held := slices.Sorted(maps.Keys(snapshot.Revs.Items["items"])); len(ids) == 0 || !slices.Equal(held, ids) {
			t.Errorf("%s: the revisions hold the items %v, want one per eval of %v", sub, held, ids)
		}
		spec, _ := parseDeliveryLiveView(view)
		rig.hub.mu.Lock()
		expiry := rig.hub.generation.views[spec.key].expiry
		rig.hub.mu.Unlock()
		if !expiry.IsZero() {
			t.Errorf("%s: the view expires at %s, want never", sub, expiry)
		}
		t.Logf("%s: the snapshot is the route's %d bytes, %d of %d evals by id", sub, len(snapshot.Body), len(body.Items), body.Total)
	}

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("new", evalsLiveView("NEWP", DeliveryLiveWindow{Preset: new(7)}, "")))
	conn.refusal(t, 5*time.Second, "new", evalsNoProjectCode)
	conn.none(t, time.Second, "the refusal of a project no row holds")
	createTestProject(t, rig.handler, "NEWP")
	snapshot := conn.snapshot(t, 5*time.Second, "new")
	requireTheListRead(t, rig.handler, "NEWP", snapshot.Body, "")
}

// TestAReadingReachesAnEvalsPageOverTheLiveSocket (k10): a page subscribed to its project's evals
// over GET /ws/live is sent the list read as its snapshot, and, after one readings write and with
// no second subscription, the delta that lands its body on the route's answer.
func TestAReadingReachesAnEvalsPageOverTheLiveSocket(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	createTestProject(t, rig.handler, "INFRA")
	now := time.Now().UTC()
	today := utcDayOf(now)
	window := evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1))

	client := rig.dial(t, asUser("alice"))
	client.send(t, deliverySubscribe("evals", evalsLiveView("INFRA", window, "")))
	snapshot := client.snapshot(t, 10*time.Second, "evals")
	requireTheListRead(t, rig.handler, "INFRA", snapshot.Body, "")

	// One reading of an eval no row holds: the write registers it, logs the reading and writes its
	// day row, in one transaction.
	written := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/pr-size/readings",
		map[string]any{"readings": []any{reading("lines", 120, now.Add(-time.Hour))}}, "socket-reading")
	if written.Code != http.StatusCreated {
		t.Fatalf("log the reading: status %d, body %s", written.Code, written.Body.String())
	}
	delta := client.delta(t, 10*time.Second, "evals")
	if got := liveUpsertIDs(t, delta, "items"); !slices.Equal(got, []string{"INFRA/pr-size"}) {
		t.Errorf("the reading's delta upserts %v, want INFRA/pr-size", got)
	}
	applied := applyLiveDelta(t, evalsView, snapshot.Body, delta)
	route := mustEncodeLive(t, evalsView, getBody(t, rig.handler, evalsTarget(t, "INFRA", applied, "")))
	if got, want := liveComparable(t, mustEncodeLive(t, evalsView, applied)), liveComparable(t, route); !reflect.DeepEqual(got, want) {
		t.Fatalf("the delta does not land on the route's body:\napplied: %v\nGET:     %v", got, want)
	}
	if held := hubSubs(rig.served.deliveryHub); held != 1 {
		t.Errorf("the hub holds %d subscriptions, want the page's one", held)
	}
	t.Logf("snapshot of %d bytes; the reading's delta of seq %s upserts %v and sets %v, and lands on the route's body",
		len(snapshot.Body), delta.Seq, liveUpsertIDs(t, delta, "items"), slices.Sorted(maps.Keys(delta.Set)))
}
