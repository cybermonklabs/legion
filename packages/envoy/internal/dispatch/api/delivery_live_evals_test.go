package api

import (
	"bytes"
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// evalsChange is one delivery_changes row of an eval kind: the eval's project in Repo and the eval
// in Ref, with the span of moments the write put values in (migrations 20261117100012 to
// 20261117100014; a version's change carries none).
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
// kind of change marks an evals view but a change of the delivery settings row, which marks every
// evals view: the deploy repository it can move chooses the rows an evals view reads of the evals
// that read it, and a move writes no day row. A filing marks no view: the refresher's rewrite of
// the day's rows is what wakes the evals view. None of the four kinds reaches the delivery switch's
// closing `return true`, which would mark every timeline, measures and pipeline view. The versions
// that mark a delivery view, those of the evals it judges or ranks by, are
// TestAVersionOfAnEvalADeliveryViewReadsMarksIt's; rework-share is none of them.
func TestAnEvalChangeTouchesTheEvalsViewsOfItsProjectAlone(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	today := measures.Midnight(now)
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
		evalsChange("eval_versions", "INFRA", evals.SlugReworkShare, nil, nil),
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
	settingsRow := delivery.Change{Seq: 2, Kind: "settings"}
	for _, view := range views {
		if view.view.spec.kind == evalsView && !view.view.touchedBy(settingsRow, now) {
			t.Errorf("a change of the delivery settings row leaves %s unmarked", view.what)
		}
	}
	t.Logf("issue_filings marks %d of %d views; %d changes of every other kind make %d marks of an evals view; the settings row's marks every evals view",
		filingMarks, len(views), len(others), evalsMarks)
}

// TestAVersionOfAnEvalADeliveryViewReadsMarksIt: the timeline's and the measures' KPIs and the
// Pipeline's lead-time cards are judged by the targets of the delivery evals, and the Pipeline
// ranks its slowest jobs by the min_n of pr-ci's job series, each of the project the deploy
// repository is mapped to, which no view holds. So a new version of a delivery target's eval (a
// register write of its target, a project's seed) in any project marks every timeline, measures
// and pipeline view, and one of pr-ci every pipeline view alone; their readings and day rows mark
// none, and neither does a version of an eval no delivery view reads or of a slug that only ends
// like one.
func TestAVersionOfAnEvalADeliveryViewReadsMarksIt(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	today := measures.Midnight(now)
	views := map[string]*deliveryView{
		deliveryLiveTimeline: deliveryViewShowing(deliveryLiveTimeline), deliveryLiveMeasures: deliveryViewShowing(deliveryLiveMeasures),
		deliveryLivePipeline: deliveryViewShowing(deliveryLivePipeline),
	}
	every := []string{deliveryLiveTimeline, deliveryLiveMeasures, deliveryLivePipeline}
	cases := []struct {
		change delivery.Change
		marks  []string
	}{
		{evalsChange("eval_versions", "INFRA", evals.SlugPRCI, nil, nil), []string{deliveryLivePipeline}},
		{evalsChange("eval_versions", "OTHER", evals.SlugPRCI, nil, nil), []string{deliveryLivePipeline}},
		{evalsChange("eval_versions", "INFRA", evals.SlugReworkShare, nil, nil), nil},
		{evalsChange("eval_versions", "INFRA", "x-"+evals.SlugPRCI, nil, nil), nil},
		{evalsChange("eval_versions", "INFRA", "x-"+evals.SlugMergeToProduction, nil, nil), nil},
		{evalsChange("eval_readings", "INFRA", evals.SlugPRCI, &today, &today), nil},
		{evalsChange("eval_days", "INFRA", evals.SlugPRCI, &today, &today), nil},
		{evalsChange("eval_readings", "INFRA", evals.SlugMergeToProduction, &today, &today), nil},
		{evalsChange("eval_days", "INFRA", evals.SlugMergeToProduction, &today, &today), nil},
	}
	for _, slug := range evals.DeliveryTargetSlugs {
		cases = append(cases, struct {
			change delivery.Change
			marks  []string
		}{evalsChange("eval_versions", "OTHER", slug, nil, nil), every})
	}
	for _, c := range cases {
		for _, kind := range every {
			if got, want := views[kind].touchedBy(c.change, now), slices.Contains(c.marks, kind); got != want {
				t.Errorf("%s of %s: touchedBy(a %s view) = %t, want %t", c.change.Kind, c.change.Ref, kind, got, want)
			}
		}
	}
	t.Logf("%d changes checked against the timeline, measures and pipeline views", len(cases))
}

// TestAPRCIMinNWriteMovesTheLivePipelinesSlowestJobs: the live Pipeline view ranks its slowest jobs
// through the GET route's compute with the GET route's min_n, the one of pr-ci's job series in the
// project the deploy repository is mapped to, and a register write of that min_n reaches it: the
// write's version of pr-ci marks the view, and its delta lands on the GET route's body. Of six
// PR-checks runs, all with a quick job and three with a slow one, no job ranks at the seeded 50,
// both rank at 3, the slower first, and the quick one alone at 6, each body stating its min_n.
// The view is pinned at the moment the runs are dated from, and each GET reads that at: the
// Pipeline route reads at alone, and the hub's clock runs on with the wall clock, so a view of the
// present would answer a later moment than any at the test could send.
func TestAPRCIMinNWriteMovesTheLivePipelinesSlowestJobs(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	ctx := t.Context()
	createTestProject(t, rig.handler, "HUB")
	if _, err := rig.database.Pool.Exec(ctx, `insert into repo_projects (repo, project, created_by) values ('acme/widgets', 'HUB', '{"kind":"user","id":"alice"}'::jsonb)`); err != nil {
		t.Fatalf("map acme/widgets to HUB: %v", err)
	}
	now := rig.clock.Now()
	success, jobSuccess := delivery.DeliveryRunConclusionSuccess, delivery.DeliveryJobConclusionSuccess
	for i := range 6 {
		id := int64(500 + i)
		started := now.Add(-time.Duration(i+1) * time.Hour)
		done := started.Add(40 * time.Minute)
		if err := delivery.UpsertRun(ctx, rig.database.Pool, delivery.DeliveryRun{
			Repo: "acme/widgets", RunID: id, Kind: delivery.DeliveryRunKindPRChecks, HeadSHA: "sha" + strconv.FormatInt(id, 10),
			HeadCommitAt: started, StartedAt: started, CompletedAt: &done, Conclusion: &success, PRNumber: new(7),
			URL: "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(id, 10), HeadBranch: new("feature"), Event: new("pull_request"),
		}); err != nil {
			t.Fatalf("upsert PR-checks run %d: %v", id, err)
		}
		jobs := []delivery.DeliveryRunJob{{Repo: "acme/widgets", RunID: id, Name: "quick", StartedAt: &started, CompletedAt: new(started.Add(5 * time.Minute)), Conclusion: &jobSuccess}}
		if i < 3 {
			jobs = append(jobs, delivery.DeliveryRunJob{Repo: "acme/widgets", RunID: id, Name: "slow", StartedAt: &started, CompletedAt: new(started.Add(30 * time.Minute)), Conclusion: &jobSuccess})
		}
		if err := delivery.UpsertRunJobs(ctx, rig.database.Pool, "acme/widgets", id, jobs, true); err != nil {
			t.Fatalf("upsert PR-checks run %d's jobs: %v", id, err)
		}
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(&now)))
	pinned := "at=" + now.Format(time.RFC3339Nano)
	body := conn.snapshot(t, 5*time.Second, "p").Body
	if names, minN := pipelineSlowestJobs(t, body); len(names) != 0 || minN != measures.SlowestMinResults {
		t.Fatalf("the snapshot ranks %v by %d, want no job by the seeded %d", names, minN, measures.SlowestMinResults)
	}
	rig.settle(t)
	for _, step := range []struct {
		minN int
		want []string
	}{{3, []string{"slow", "quick"}}, {6, []string{"quick"}}} {
		registered := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/HUB/evals", map[string]any{
			"slug": evals.SlugPRCI, "series": []any{map[string]any{"series": evals.SeriesJobs, "min_n": step.minN}},
		}, "pr-ci-min-n-"+strconv.Itoa(step.minN))
		if registered.Code != http.StatusCreated && registered.Code != http.StatusOK {
			t.Fatalf("set HUB's pr-ci min_n to %d: status %d, body %s", step.minN, registered.Code, registered.Body.String())
		}
		body = requireLandsOnGET(t, rig.handler, deliveryLivePipeline, body, conn.delta(t, 5*time.Second, "p"), pinned)
		if names, minN := pipelineSlowestJobs(t, body); !slices.Equal(names, step.want) || minN != step.minN {
			t.Fatalf("with HUB's pr-ci min_n at %d the live Pipeline ranks %v by %d, want %v by %d", step.minN, names, minN, step.want, step.minN)
		}
	}
}

// pipelineSlowestJobs is the names a pipeline body's slowest-jobs table holds, in its own order, and
// the min_n it ranked them by.
func pipelineSlowestJobs(t *testing.T, body json.RawMessage) ([]string, int) {
	t.Helper()
	var shape struct {
		PRChecks struct {
			SlowestJobs []struct {
				Name string `json:"name"`
			} `json:"slowest_jobs"`
			SlowestMinResults int `json:"slowest_min_results"`
		} `json:"pr_checks"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("read the pipeline body's slowest jobs: %v", err)
	}
	names := make([]string, len(shape.PRChecks.SlowestJobs))
	for i, job := range shape.PRChecks.SlowestJobs {
		names[i] = job.Name
	}
	return names, shape.PRChecks.SlowestMinResults
}

// TestAP0WriteAndTheSamplerMarkNoEvalsView: an evals view shows the unowned-p0 eval, whose count
// it reads from the project's stored row, so an event of a P0 issue, which moves no stored row,
// marks the timeline, which lists the open P0 issues, and no evals view; an event of an issue that
// is no P0 marks neither. The sampler's read of the project's daemon, once a minute, marks the
// project's Legion board and leaves its evals view.
func TestAP0WriteAndTheSamplerMarkNoEvalsView(t *testing.T) {
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

	if got, want := apply(issueEvent("INFRA-9", 0)), map[string]bool{"board": false, "evals": false, "timeline": true}; !reflect.DeepEqual(got, want) {
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
// request's day is what recomputes them, and sends them the eval it moved. So it is for a P0
// issue: its event recomputes the timeline alone, and the unowned-p0 row of today the refresher
// writes for it (evals.Store.WriteUnownedP0Day) recomputes INFRA's evals views and sends them the
// new count.
//
// Each step reads the kinds its write logged, so the step exercises the kind it names and no
// other, and counts every recompute that began after it, of any view.
func TestAnEvalWriteRecomputesTheEvalsViewsOfItsProjectAlone(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "INFRA")
	createTestProject(t, rig.handler, "OTHER")
	now := rig.clock.Now()
	today := measures.Midnight(now)

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
		// A write made by SQL records its eval changes in the outbox and drains nothing; the hub
		// drains before it reads, and so does this read.
		if _, err := delivery.DrainPending(t.Context(), rig.database.Pool); err != nil {
			t.Fatalf("%s: drain the change log's outbox: %v", what, err)
		}
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
		// deploy-frequency reads the deploy repository, so its rows are kept by it (eval_days.deploy_repo)
		// and the view reads those of acme/widgets, the one the settings name.
		if _, err := rig.database.Pool.Exec(t.Context(), `
			insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, counts, built_by)
			values ('INFRA', 'INFRA/deploy-frequency', 'acme/widgets', 'deploys', '', $1, true, 1, 1, '{"with_prs": 1}', 'test')
		`, measures.Midnight(merged)); err != nil {
			t.Fatalf("write deploy-frequency's day row: %v", err)
		}
	}, []string{"eval_days"}, []string{"a", "b"})
	sent("the day row", map[string][]string{"a": {"INFRA/deploy-frequency"}, "b": {"INFRA/deploy-frequency"}})

	step("a P0 issue of INFRA opened", func() {
		response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues",
			map[string]any{"project": "INFRA", "title": "Production is down", "priority": 0}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("open the P0 issue: status %d, body %s", response.Code, response.Body.String())
		}
	}, nil, []string{"t"})
	conns["t"].delta(t, 5*time.Second, "t")
	sent("the P0 issue's event", nil)

	step("the unowned-p0 row the refresher writes for the P0 issue", func() {
		if err := rig.hub.server.registry().WriteUnownedP0Day(t.Context(), "INFRA", today, testServerBuild); err != nil {
			t.Fatalf("write INFRA's unowned-p0 row of today: %v", err)
		}
	}, []string{"eval_days"}, []string{"a", "b"})
	sent("the unowned-p0 row", map[string][]string{"a": {"INFRA/unowned-p0"}, "b": {"INFRA/unowned-p0"}})
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
	today := measures.Midnight(now)
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
	conn.refusal(t, 5*time.Second, "new", errProjectNotFound.code)
	conn.none(t, time.Second, "the refusal of a project no row holds")
	createTestProject(t, rig.handler, "NEWP")
	snapshot := conn.snapshot(t, 5*time.Second, "new")
	requireTheListRead(t, rig.handler, "NEWP", snapshot.Body, "")
}

// TestAReadingReachesAnEvalsPageOverTheLiveSocket: a page subscribed to its project's evals
// over GET /ws/live is sent the list read as its snapshot, and, after one readings write and with
// no second subscription, the delta that lands its body on the route's answer.
func TestAReadingReachesAnEvalsPageOverTheLiveSocket(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	createTestProject(t, rig.handler, "INFRA")
	now := time.Now().UTC()
	today := measures.Midnight(now)
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

// evalsMarkStep is one thing an evals view of INFRA meets, after the moment of the first step: a
// change of kind, of the eval slug names (deploy-frequency where it names none), heard by the
// intake, an eval_days change the settings rewrite wrote (evalsRewrite), or one of the moments of
// a recompute below.
type evalsMarkStep struct {
	after time.Duration
	kind  string
	slug  string
}

// The moments of a recompute a step can be, as recompute and its read make them: its read holds
// the newest change position and takes every mark so far over (deliveryHub.read); it installs its
// answer as of that position and ends; the two at once, nothing marking the view between them;
// and the retry a failed read holds the view back to (notBefore).
const (
	evalsReadHeld   = "a recompute's read holds its position"
	evalsRecomputed = "the recompute ends"
	evalsAnswered   = "a recompute answers every mark"
	evalsRetryHeld  = "a failed read's retry"
)

// evalsRewrite is a step's kind for an eval_days change the settings rewrite wrote, which carries
// the rewrite's mark (delivery.EvalDaysWrittenBySettingsRewrite); a step of kind eval_days is a
// write of day rows that is not the rewrite's.
const evalsRewrite = "the settings rewrite's eval_days change"

// markEvalsView is an evals view of INFRA after steps, from start: each change marked as the
// intake marks a page of the change log (markChangesLocked), each recompute's moment as
// recompute makes it. The view is in a generation of its own, which reads nothing, so what each
// step makes due is read from the view (dueAt) without a database.
func markEvalsView(start time.Time, steps []evalsMarkStep) *deliveryView {
	view := evalsViewOf("INFRA", 28, time.Time{}, time.Time{})
	hub := &deliveryHub{}
	g := &hubGeneration{wakeWork: make(chan struct{}, 1), views: map[string]*deliveryView{"INFRA": view}}
	today := measures.Midnight(start)
	var seq int64
	for _, step := range steps {
		now := start.Add(step.after)
		switch step.kind {
		case evalsReadHeld:
			view.computing, view.readingSeq, view.dirty, view.immediate = true, seq, false, false
		case evalsRecomputed:
			view.seq, view.computing, view.readingSeq = view.readingSeq, false, 0
		case evalsAnswered:
			view.seq, view.dirty, view.immediate = seq, false, false
		case evalsRetryHeld:
			view.notBefore = now.Add(deliveryLiveRetry)
		default:
			seq++
			slug := cmp.Or(step.slug, evals.SlugDeployFrequency)
			var change delivery.Change
			switch step.kind {
			case delivery.ChangeKindSettings:
				change = delivery.Change{Kind: delivery.ChangeKindSettings}
			case delivery.ChangeKindEvalVersions:
				change = evalsChange(step.kind, "INFRA", slug, nil, nil)
			case delivery.ChangeKindEvalReadings, delivery.ChangeKindEvalDays:
				change = evalsChange(step.kind, "INFRA", slug, &today, &today)
			case evalsRewrite:
				change = evalsChange(delivery.ChangeKindEvalDays, "INFRA", slug, &today, &today)
				change.RunKind = delivery.EvalDaysWrittenBySettingsRewrite
			default:
				panic("markEvalsView: a step of unknown kind " + strconv.Quote(step.kind))
			}
			change.Seq = seq
			hub.mu.Lock()
			hub.markChangesLocked(g, []delivery.Change{change}, now)
			hub.mu.Unlock()
		}
	}
	return view
}

// TestASettingsRewriteIsRecomputedAtOnce: an eval_days change the settings rewrite wrote, which
// carries the rewrite's mark, is due at its own mark rather than viewQuiet after it (dueAt), with
// its own cause: after the settings change's own recompute, which reads the figures before the
// rewrite; while that recompute still waits for its quiet, whose mark the rewrite's then is, so
// the one recompute is due at once; 45 s after the settings change, long after its own recompute;
// and with no settings change heard before it, as by a page that subscribed between the settings
// write and the rewrite's commit. A failed read's retry still holds the rewrite back, and a view
// being computed is due nowhere until that recompute ends, and the rewrite then at once.
func TestASettingsRewriteIsRecomputedAtOnce(t *testing.T) {
	start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	settings, rewrite := delivery.ChangeKindSettings, evalsRewrite
	// The refresher's pass commits 1.2-1.7 s after the settings write at ten times INFRA's size;
	// a larger history's, or one a loaded database holds back, can take far longer.
	pass, slow := 1500*time.Millisecond, 45*time.Second
	cases := []struct {
		name  string
		steps []evalsMarkStep
		// due is when the view is due after start; computing is a view being computed, which is due
		// nowhere.
		due       time.Duration
		computing bool
	}{
		{"the rewrite of two evals after the settings change's own recompute", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""},
			{pass, rewrite, ""}, {pass, rewrite, evals.SlugMergeToProduction},
		}, pass, false},
		{"the rewrite inside the settings change's quiet, the one recompute due at the settings change's mark", []evalsMarkStep{
			{0, settings, ""}, {100 * time.Millisecond, rewrite, ""},
		}, 0, false},
		{"the rewrite 45 s after the settings change, long after its own recompute", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {slow, rewrite, ""},
		}, slow, false},
		{"the rewrite with no settings change heard before it", []evalsMarkStep{{0, rewrite, ""}}, 0, false},
		{"the rewrite behind a failed read's retry", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {time.Second, evalsRetryHeld, ""}, {pass, rewrite, ""},
		}, time.Second + deliveryLiveRetry, false},
		{"the rewrite while the settings change's own recompute is computing", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsReadHeld, ""}, {pass, rewrite, ""},
		}, 0, true},
		{"the rewrite once that recompute has ended", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsReadHeld, ""}, {pass, rewrite, ""}, {pass + 40*time.Millisecond, evalsRecomputed, ""},
		}, pass, false},
	}
	for _, c := range cases {
		at, cause, ok := markEvalsView(start, c.steps).dueAt()
		switch {
		case c.computing:
			if ok {
				t.Errorf("%s: due %s after the first step (%s), want nowhere while the view is computed", c.name, at.Sub(start), cause)
			}
		case !ok || !at.Equal(start.Add(c.due)) || cause != deliveryCauseSettingsRewrite:
			t.Errorf("%s: due %t, %s after the first step (%s), want %s after it (%s)", c.name, ok, at.Sub(start), cause, c.due, deliveryCauseSettingsRewrite)
		default:
			last := c.steps[len(c.steps)-1].after
			t.Logf("%s: due %s after the first step, its last step %s after it, cause %s", c.name, at.Sub(start), last, cause)
		}
	}
}

// TestEveryOtherEvalsViewMarkWaitsTheQuiet: every other mark of an evals view coalesces, due
// viewQuiet after the last one, cause change: two of each kind that marks it (readings, versions,
// day writes that are not the rewrite's) and two settings changes; a reading or a version after a
// settings change, which is no rewrite of day rows; a day write that is not the rewrite's, 1.5 s
// after a settings change that moved no day row, whose rewrite therefore committed none; and a day
// write after the rewrite's.
func TestEveryOtherEvalsViewMarkWaitsTheQuiet(t *testing.T) {
	start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	settings, days, rewrite := delivery.ChangeKindSettings, delivery.ChangeKindEvalDays, evalsRewrite
	readings, versions := delivery.ChangeKindEvalReadings, delivery.ChangeKindEvalVersions
	gap, pass := 100*time.Millisecond, 1500*time.Millisecond
	cases := []struct {
		name  string
		steps []evalsMarkStep
		last  time.Duration
	}{
		{"two readings", []evalsMarkStep{{0, readings, ""}, {gap, readings, "pr-size"}}, gap},
		{"two versions", []evalsMarkStep{{0, versions, ""}, {gap, versions, "review-time"}}, gap},
		{"two day writes", []evalsMarkStep{{0, days, ""}, {gap, days, evals.SlugMergeToProduction}}, gap},
		{"two settings changes", []evalsMarkStep{{0, settings, ""}, {gap, settings, ""}}, gap},
		{"a reading after a settings change", []evalsMarkStep{{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {pass, readings, ""}}, pass},
		{"a version after a settings change", []evalsMarkStep{{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {pass, versions, ""}}, pass},
		{"a day write 1.5 s after a settings change that moved no day row", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {pass, days, ""},
		}, pass},
		{"a day write after the rewrite's", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {pass, rewrite, ""},
			{pass + 40*time.Millisecond, evalsAnswered, ""}, {pass + time.Second, days, ""},
		}, pass + time.Second},
	}
	for _, c := range cases {
		at, cause, ok := markEvalsView(start, c.steps).dueAt()
		if want := start.Add(c.last + viewQuiet); !ok || !at.Equal(want) || cause != deliveryCauseChange {
			t.Errorf("%s: due %t, %s after the first mark (%s), want %s after it, viewQuiet after the last (%s)",
				c.name, ok, at.Sub(start), cause, want.Sub(start), deliveryCauseChange)
			continue
		}
		t.Logf("%s: due %s after the first mark, viewQuiet after the last, cause %s", c.name, at.Sub(start), cause)
	}
}

// settingsRewritePage is a page subscribed to INFRA's evals over the 28 days ending today, on a
// hub whose clock stands still: a recompute due after a quiet waits for the test to move the
// clock, and one due at once runs without it. held is the body the page holds.
type settingsRewritePage struct {
	rig   *hubRig
	conn  *fakeLiveConn
	frame DeliveryLiveView
	held  json.RawMessage
	today time.Time
}

// newSettingsRewritePage subscribes the page once the hub's delivery settings and INFRA's evals
// are seeded; configure, when given, sets the hub's seams before it starts.
func newSettingsRewritePage(t *testing.T, configure func(*deliveryHub)) *settingsRewritePage {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	rig := hubOn(t, handler, database, deps, newStillHubClock(), configure)
	seedHubSettings(t, rig.database)
	createTestProject(t, rig.handler, "INFRA")
	today := measures.Midnight(rig.clock.Now())
	page := &settingsRewritePage{
		rig: rig, conn: newFakeLiveConn(), today: today,
		frame: evalsLiveView("INFRA", evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), ""),
	}
	rig.hub.Subscribe(page.conn, subscribeFrame("e", page.frame))
	page.held = page.conn.snapshot(t, 5*time.Second, "e").Body
	rig.settle(t)
	return page
}

// saveSettings saves the delivery settings with one more production job named, which logs one
// change of the settings row, and moves the clock past its quiet: its own recompute reads the
// figures before the refresher's rewrite and sends nothing. No run holds the name, so the change
// moves no eval and the refresher's rewrite of it commits no day row (evals'
// TestASettingsChangeThatMovesNoEvalWritesNothing); a test writes the day rows that follow it as
// the rewrite's or as another writer's (writeDays).
func (p *settingsRewritePage) saveSettings(t *testing.T) {
	t.Helper()
	before, mark := latestChange(t, p.rig.database), p.rig.mark()
	settings, err := delivery.GetSettings(t.Context(), p.rig.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	settings.ProductionJobNames = append(settings.ProductionJobNames, "release / release-canary")
	if _, err := delivery.PutSettings(t.Context(), p.rig.database.Pool, settings, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("save the delivery settings: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), p.rig.database.Pool, before, 100)
	if err != nil {
		t.Fatalf("read the changes the save logged: %v", err)
	}
	if len(changes) != 1 || !settingsRowChange(changes[0]) {
		t.Fatalf("the settings save logged %+v, want one change of the settings row", changes)
	}
	p.rig.hub.catchUpNow()
	p.conn.none(t, 2*viewQuiet, "a settings change the clock has not moved past")
	p.rig.clock.Advance(viewQuiet)
	p.rig.waitFor(t, mark, 5*time.Second, "the settings change's own recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseChange
	})
	p.conn.none(t, 200*time.Millisecond, "the settings change's own recompute, which reads the figures before the rewrite")
}

// dayRowEvals is the two evals of INFRA whose day rows writeDays writes, by id, sorted.
var dayRowEvals = []string{"INFRA/deploy-frequency", "INFRA/merge-to-production"}

// writeDays writes today's day rows of deploy-frequency and merge-to-production, two evals of INFRA
// that read the deploy repository, in one transaction, after moving the hub's clock on by after.
// Where byRewrite, the transaction is the settings rewrite's, marked so before its write
// (evals.MarkSettingsRewrite) as the refresher marks its rewrite of what a settings change moved,
// which writes every eval's rows in one transaction; otherwise it is a write of day rows that is
// not the rewrite's. It requires the one eval_days change of each eval the write logged, carrying
// the rewrite's mark where byRewrite and none otherwise, catches the hub up, and answers the count
// of the hub's notes before the write and the newest change.
func (p *settingsRewritePage) writeDays(t *testing.T, after time.Duration, byRewrite bool) (int, int64) {
	t.Helper()
	p.rig.clock.Advance(after)
	before, mark := latestChange(t, p.rig.database), p.rig.mark()
	tx, err := p.rig.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the write of the day rows: %v", err)
	}
	defer tx.Rollback(t.Context())
	runKind := ""
	if byRewrite {
		if err := evals.MarkSettingsRewrite(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
		runKind = delivery.EvalDaysWrittenBySettingsRewrite
	}
	if _, err := tx.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, samples, counts, built_by)
		values ('INFRA', 'INFRA/deploy-frequency', 'acme/widgets', 'deploys', '', $1, true, 2, 2, null, '{"with_prs": 2}', 'test'),
		       ('INFRA', 'INFRA/merge-to-production', 'acme/widgets', 'minutes', '', $1, true, 1, null, '{42}', '{}', 'test')
	`, p.today); err != nil {
		t.Fatalf("write the day rows: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the day rows: %v", err)
	}
	latest := latestChange(t, p.rig.database)
	changes, err := delivery.ChangesSince(t.Context(), p.rig.database.Pool, before, 100)
	if err != nil {
		t.Fatalf("read the changes the write logged: %v", err)
	}
	var logged []string
	for _, change := range changes {
		logged = append(logged, change.Kind+" "+change.Ref+" run_kind "+strconv.Quote(change.RunKind))
	}
	slices.Sort(logged)
	var want []string
	for _, id := range dayRowEvals {
		want = append(want, "eval_days "+id+" run_kind "+strconv.Quote(runKind))
	}
	if !slices.Equal(logged, want) {
		t.Fatalf("the write logged %v, want %v", logged, want)
	}
	p.rig.hub.catchUpNow()
	return mark, latest
}

// route is the list route's body for the page's window, as of now.
func (p *settingsRewritePage) route(t *testing.T) json.RawMessage {
	t.Helper()
	return getBody(t, p.rig.handler, evalsTarget(t, "INFRA", p.held, ""))
}

// apply applies delta to the body the page holds, as a page applies it, requires that it lands on
// route, and holds the result.
func (p *settingsRewritePage) apply(t *testing.T, delta DeliveryLiveDelta, route json.RawMessage) {
	t.Helper()
	applied := applyLiveDelta(t, evalsView, p.held, delta)
	if got, want := liveComparable(t, mustEncodeLive(t, evalsView, applied)), liveComparable(t, mustEncodeLive(t, evalsView, route)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the delta does not land on the route's body:\napplied: %v\nGET:     %v", got, want)
	}
	p.held = applied
}

// dayRowsDelta is the page's next delta, which it requires to upsert the evals writeDays writes
// (dayRowEvals) and nothing else; what names the write the delta answers.
func (p *settingsRewritePage) dayRowsDelta(t *testing.T, what string) DeliveryLiveDelta {
	t.Helper()
	delta := p.conn.delta(t, 5*time.Second, "e")
	if moved := slices.Sorted(slices.Values(liveUpsertIDs(t, delta, "items"))); !slices.Equal(moved, dayRowEvals) {
		t.Fatalf("%s's delta upserts %v, want every eval it wrote and nothing else, %v", what, moved, dayRowEvals)
	}
	return delta
}

// TestASettingsChangeAndItsRewriteSendTheEvalsPageOneDeltaAtOnce: a settings change marks an
// evals page, whose own recompute, viewQuiet after it, reads the figures before the rewrite and
// sends nothing. The refresher's rewrite of what the change moved, one transaction holding two
// evals' rows and marked the rewrite's, arrives on the hub's clock 1.25 s later, as a pass at ten
// times INFRA's size commits 1.2-1.7 s after the settings write, or 45.25 s later, as a pass on a
// larger history or a loaded database can, and the clock then stands still: the rewrite is
// recomputed at its own mark, and the page gets one delta holding both evals' new numbers, which
// lands on the route's body. Nothing follows it, however far the clock moves past every quiet.
func TestASettingsChangeAndItsRewriteSendTheEvalsPageOneDeltaAtOnce(t *testing.T) {
	for _, c := range []struct {
		name  string
		after time.Duration
	}{
		{"the rewrite 1.25 s after the settings change", time.Second},
		{"the rewrite 45.25 s after the settings change", 45 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			page := newSettingsRewritePage(t, nil)
			page.saveSettings(t)
			mark, _ := page.writeDays(t, c.after, true)
			delta := page.dayRowsDelta(t, "the rewrite")
			page.apply(t, delta, page.route(t))
			page.rig.clock.Advance(viewMaxWait)
			page.conn.none(t, time.Second, "the rewrite's one delta, past every quiet")
			begun, rewrites := page.rig.count(mark, recomputeBegin("")), page.rig.count(mark, recomputeBegin(deliveryCauseSettingsRewrite))
			if begun != 1 || rewrites != 1 {
				t.Fatalf("the rewrite began %d recomputes, %d of them %s, want the one", begun, rewrites, deliveryCauseSettingsRewrite)
			}
			t.Logf("the rewrite's eval_days changes were recomputed once (%s) on a clock standing at their mark, and its one delta upserts %v and lands on the route's body",
				deliveryCauseSettingsRewrite, dayRowEvals)
		})
	}
}

// TestADayWriteAfterASettingsChangeThatMovedNoDayRowWaitsItsQuiet: a settings change marks an evals
// page, whose own recompute reads the figures and sends nothing, and the change moves no eval, so
// the refresher's rewrite of it commits no day row. A write of two evals' day rows that is not the
// rewrite's then lands 1.25 s after the settings change, on a clock that then stands still: no
// recompute begins and nothing is sent until the clock passes the write's quiet, and then the page
// gets one delta holding both evals' new numbers, from a recompute of cause change, which lands on
// the route's body.
func TestADayWriteAfterASettingsChangeThatMovedNoDayRowWaitsItsQuiet(t *testing.T) {
	page := newSettingsRewritePage(t, nil)
	page.saveSettings(t)
	mark, _ := page.writeDays(t, time.Second, false)
	page.conn.none(t, time.Second, "a day write that is not the rewrite's, on a clock that has not moved past its quiet")
	if n := page.rig.count(mark, recomputeBegin("")); n != 0 {
		t.Fatalf("the day write began %d recomputes on a clock that has not moved past its quiet, want 0", n)
	}
	page.rig.clock.Advance(viewQuiet)
	delta := page.dayRowsDelta(t, "the day write")
	page.apply(t, delta, page.route(t))
	page.conn.none(t, time.Second, "the day write's one delta")
	changes, rewrites := page.rig.count(mark, recomputeBegin(deliveryCauseChange)), page.rig.count(mark, recomputeBegin(deliveryCauseSettingsRewrite))
	if changes != 1 || rewrites != 0 {
		t.Fatalf("the day write began %d %s recomputes and %d %s recomputes, want one %s alone",
			changes, deliveryCauseChange, rewrites, deliveryCauseSettingsRewrite, deliveryCauseChange)
	}
	t.Logf("the day write, 1.25 s after a settings change that moved no day row, sent nothing until its quiet passed, and then one delta upserting %v from one %s recompute",
		dayRowEvals, deliveryCauseChange)
}

// TestASettingsRewritesDeltaIsWholeWhenAChangeLandsDuringItsRecompute: the rewrite's recompute is
// held once its read is done, and a reading of another eval of INFRA, whose write logs its own
// eval_days change, lands there. The rewrite committed whole before its changes reached the log
// and before the read took its position after them, so every statement of the read sees every
// row of it: the page's one delta of the rewrite holds both evals' new numbers and lands on the
// route's body as it stood before the reading. The reading marks the view while it is computed,
// so nothing recomputes it a second time at once; the reading's day write is not the rewrite's,
// so it waits its own quiet, and the recompute after the quiet sends the reading's eval alone.
func TestASettingsRewritesDeltaIsWholeWhenAChangeLandsDuringItsRecompute(t *testing.T) {
	var pause *notePause
	page := newSettingsRewritePage(t, func(hub *deliveryHub) { pause = pauseNotes(hub) })
	page.saveSettings(t)
	point := pause.at(func(note deliveryHubNote) bool {
		return note.what == "recompute read" && note.cause == deliveryCauseSettingsRewrite
	})
	mark, rewritten := page.writeDays(t, time.Second, true)
	note := point.wait(t, "the read of the rewrite's recompute")
	if note.through < rewritten {
		t.Fatalf("the rewrite's read took position %d, before the rewrite's newest change %d", note.through, rewritten)
	}
	rewrite := page.route(t)

	// review-time is a logged eval no row holds yet: the write registers it, logs the reading and
	// writes its day row, in one transaction.
	written := evalRequest(t, page.rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/review-time/readings",
		map[string]any{"readings": []any{reading("minutes", 25, time.Now().UTC().Add(-time.Minute))}}, "rewrite-reading")
	if written.Code != http.StatusCreated {
		t.Fatalf("log the reading: status %d, body %s", written.Code, written.Body.String())
	}
	if landed := latestChange(t, page.rig.database); note.through >= landed {
		t.Fatalf("the rewrite's read took position %d, which holds the reading's change %d: the reading did not land during the recompute", note.through, landed)
	}
	page.rig.hub.catchUpNow()
	spec, _ := parseDeliveryLiveView(page.frame)
	page.rig.hub.mu.Lock()
	view := page.rig.hub.generation.views[spec.key]
	dirty, immediate, computing := view.dirty, view.immediate, view.computing
	page.rig.hub.mu.Unlock()
	if !dirty || immediate || !computing {
		t.Fatalf("the view the reading marked during the rewrite's recompute is dirty %t, immediate %t, computing %t; want dirty, waiting for its quiet, while computed",
			dirty, immediate, computing)
	}
	resumed := page.rig.mark()
	point.proceed()

	first := page.dayRowsDelta(t, "the rewrite")
	page.apply(t, first, rewrite)
	page.conn.none(t, 2*viewQuiet, "the rewrite's delta, before the reading's quiet has passed")
	if n := page.rig.count(resumed, recomputeBegin("")); n != 0 {
		t.Fatalf("recomputes after the rewrite's, before the reading's quiet passed = %d, want 0", n)
	}

	page.rig.clock.Advance(viewQuiet)
	second := page.conn.delta(t, 5*time.Second, "e")
	if got := liveUpsertIDs(t, second, "items"); !slices.Equal(got, []string{"INFRA/review-time"}) {
		t.Fatalf("the delta after the reading's quiet upserts %v, want INFRA/review-time", got)
	}
	page.apply(t, second, page.route(t))
	page.conn.none(t, time.Second, "the reading's delta")
	rewrites, changes := page.rig.count(mark, recomputeBegin(deliveryCauseSettingsRewrite)), page.rig.count(mark, recomputeBegin(deliveryCauseChange))
	if rewrites != 1 || changes != 1 {
		t.Fatalf("since the rewrite: %d %s recomputes and %d %s recomputes, want one of each",
			rewrites, deliveryCauseSettingsRewrite, changes, deliveryCauseChange)
	}
	t.Logf("the rewrite's read held position %d; its one delta upserts %v and lands on the route's body before the reading; the reading, marked while the view was computed, waited its quiet and its delta upserts %v",
		note.through, dayRowEvals, liveUpsertIDs(t, second, "items"))
}
