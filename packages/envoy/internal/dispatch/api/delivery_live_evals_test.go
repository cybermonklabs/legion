package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
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
// kind of change marks an evals view but the refresher's word that the rows stand at the delivery
// settings under source repositories that moved (delivery.SettingsApplied), which marks every
// evals view: those repositories choose the rows an evals view reads of the evals that read them,
// and their move writes no day row. A change of the settings itself marks no evals view, and marks
// every timeline, measures and pipeline view, whose freshness the settings hold. The refresher's
// commit that brings the rows to the settings marks every timeline, measures and pipeline view
// too, which answer under the rules recorded with the rows (evals.BuiltSettingsIn): its word
// that the rows stand at them, and each eval_days change of its rewrite, which marks the evals
// views of its eval's project as well. A filing marks no view: the refresher's rewrite of the
// day's rows is what wakes the evals view. None of the four kinds reaches the delivery switch's
// closing `return true`, which would mark every timeline, measures and pipeline view. The versions
// that mark a delivery view, those of the evals it judges or ranks by, are
// TestAVersionOfAnEvalADeliveryViewReadsMarksIt's; rework-share is none of them.
func TestAnEvalChangeTouchesTheEvalsViewsOfItsProjectAlone(t *testing.T) {
	t.Parallel()
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

	// A filing and a write of the record of deploys and failed changes: no view reads either, the
	// refresher does, and its eval_days change marks the evals view.
	filingMarks := 0
	for _, change := range []delivery.Change{
		{Seq: 1, Kind: "issue_filings", Repo: "INFRA", Ref: "INFRA-7", TNew: &hourAgo},
		{Seq: 1, Kind: delivery.ChangeKindProductionEvents, Repo: "INFRA", Ref: "0193a1b2-0000-7000-8000-000000000001", TNew: &hourAgo},
		{Seq: 1, Kind: delivery.ChangeKindProductionEvents, Repo: "INFRA", Ref: "0193a1b2-0000-7000-8000-000000000002", TOld: &dayAgo, TNew: &hourAgo},
	} {
		for _, view := range views {
			if view.view.touchedBy(change, now) {
				filingMarks++
				t.Errorf("%s of %s marks %s, want no view", change.Kind, change.Ref, view.what)
			}
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
	// A change of the delivery settings marks no evals view: such a view reads the source
	// repositories recorded with the day rows. It marks every view that reads the delivery rows,
	// whose freshness the settings hold. The refresher's commit that brings the rows to the
	// settings marks every view that reads the delivery rows too, since each answers under the
	// rules recorded with the rows (evals.BuiltSettingsIn): its word that the rows stand at the
	// settings under source repositories that moved (delivery.SettingsApplied), an eval_days change
	// of no eval, which marks every evals view as well, and each eval_days change of its rewrite,
	// which marks the evals views of its eval's project as well.
	settingsRow := delivery.Change{Seq: 2, Kind: "settings"}
	applied := delivery.Change{Seq: 3, Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenBySettingsRewrite}
	rewritten := evalsChange(delivery.ChangeKindEvalDays, "INFRA", evals.SlugDeployFrequency, &today, &today)
	rewritten.RunKind = delivery.EvalDaysWrittenBySettingsRewrite
	if !delivery.SettingsApplied(applied) || !delivery.EvalDaysImmediate(applied) {
		t.Errorf("an eval_days change of no eval with the settings rewrite's mark is the settings applied %t and immediate %t, want both",
			delivery.SettingsApplied(applied), delivery.EvalDaysImmediate(applied))
	}
	for _, view := range views {
		evalsPage := view.view.spec.kind == evalsView
		readsDelivery := view.view.spec.classes().reads == viewReadsDelivery
		if got := view.view.touchedBy(settingsRow, now); got != readsDelivery {
			t.Errorf("a change of the delivery settings row: touchedBy(%s) = %t, want %t: it marks the views that read the delivery rows and no evals view, whose rows the refresher has not brought to the settings yet",
				view.what, got, readsDelivery)
		}
		if got := view.view.touchedBy(applied, now); got != (evalsPage || readsDelivery) {
			t.Errorf("the settings applied: touchedBy(%s) = %t, want %t: it marks every evals view and every view that reads the delivery rows",
				view.what, got, evalsPage || readsDelivery)
		}
		if got := view.view.touchedBy(rewritten, now); got != (view.infra || readsDelivery) {
			t.Errorf("the rewrite's eval_days change of %s: touchedBy(%s) = %t, want %t: it marks the evals views of INFRA and every view that reads the delivery rows",
				rewritten.Ref, view.what, got, view.infra || readsDelivery)
		}
	}
	t.Logf("issue_filings and production_events mark %d of %d views; %d changes of every other kind make %d marks of an evals view; the settings row's marks every delivery view and no evals view, the settings applied every evals view and every delivery view, and the rewrite's change of an eval the evals views of its project and every delivery view",
		filingMarks, len(views), len(others), evalsMarks)
}

// TestAVersionOfAnEvalADeliveryViewReadsMarksIt: the timeline's and the measures' KPIs and the
// Pipeline's lead-time cards are judged by the targets of the delivery evals, and the Pipeline
// ranks its slowest jobs by the min_n of pr-ci's job series, each of the project the deploy
// repository is mapped to, which no view holds. So a new version of a delivery target's eval (a
// register write of its target, a project's seed) in any project marks every timeline, measures
// and pipeline view, and one of pr-ci every pipeline view alone; their readings and day rows mark
// none (but the failure measures' day rows, which the timeline and the measures fold:
// TestAFailureMeasuresDayRowsMarkTheTimelineAndTheMeasures), and neither does a version of an eval
// no delivery view reads or of a slug that only ends like one. The refresher's commit that moves
// which project that is (delivery.JudgingMoved: the change of no eval whose Ref is
// delivery.DefaultsJudgingRef and Repo the deploy repository) marks every one of them, and its
// change for a project whose own published default it moves none.
func TestAVersionOfAnEvalADeliveryViewReadsMarksIt(t *testing.T) {
	t.Parallel()
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
		{delivery.Change{Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenByDefaults, Repo: "acme/widgets", Ref: delivery.DefaultsJudgingRef}, every},
		{delivery.Change{Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenByDefaults, Repo: "INFRA"}, nil},
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

// TestAFailureMeasuresDayRowsMarkTheTimelineAndTheMeasures: the timeline's and the measures'
// failure measures are the folds of the three failure evals' day rows of the delivery-on project
// (evals.Store.DeploymentFailures), which no view holds. So a write of one's day rows in any
// project, as the refresher writes them after a failure, a reject or a restore, marks every
// timeline and measures view and no pipeline view, which folds none; their readings mark none,
// and neither do the day rows of a slug that only ends like one.
func TestAFailureMeasuresDayRowsMarkTheTimelineAndTheMeasures(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	today := measures.Midnight(now)
	views := map[string]*deliveryView{
		deliveryLiveTimeline: deliveryViewShowing(deliveryLiveTimeline), deliveryLiveMeasures: deliveryViewShowing(deliveryLiveMeasures),
		deliveryLivePipeline: deliveryViewShowing(deliveryLivePipeline),
	}
	every := []string{deliveryLiveTimeline, deliveryLiveMeasures, deliveryLivePipeline}
	folding := []string{deliveryLiveTimeline, deliveryLiveMeasures}
	type markCase struct {
		change delivery.Change
		marks  []string
	}
	var cases []markCase
	for _, slug := range []string{evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery, evals.SlugDeploymentReworkRate} {
		cases = append(cases,
			markCase{evalsChange("eval_days", "INFRA", slug, &today, &today), folding},
			markCase{evalsChange("eval_days", "OTHER", slug, &today, &today), folding},
			markCase{evalsChange("eval_readings", "INFRA", slug, &today, &today), nil},
			markCase{evalsChange("eval_days", "INFRA", "x-"+slug, &today, &today), nil})
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
// project whose deploy source reads the deploy repository, here through the project's published
// default repository (evals.Store.DeployRepoEvals reads eval_days_defaults, which the evals
// refresher's page commit moves, and which the test writes as that commit does), and a register
// write of that min_n reaches it: the write's version of pr-ci marks the view, and its delta lands
// on the GET route's body. Of six PR-checks runs, all with a quick job and three with a slow one,
// no job ranks at the seeded 50, both rank at 3, the slower first, and the quick one alone at 6,
// each body stating its min_n. The view is pinned at the moment the runs are dated from, and each
// GET reads that at: the
// Pipeline route reads at alone, and the hub's clock runs on with the wall clock, so a view of the
// present would answer a later moment than any at the test could send.
func TestAPRCIMinNWriteMovesTheLivePipelinesSlowestJobs(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	ctx := t.Context()
	createTestProject(t, rig.handler, "HUB")
	if _, err := rig.database.Pool.Exec(ctx, `update projects set default_repo = 'acme/widgets' where key = 'HUB'`); err != nil {
		t.Fatalf("make acme/widgets HUB's default repository: %v", err)
	}
	// HUB's one source names no repository, so it reads the default repository, as published.
	saveProjectRules(t, rig.database, "HUB", deployRule("acme/widgets", "release / release"))
	publishDefaultRepos(t, rig.database.Pool)
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
	body := conn.snapshot(t, "p").Body
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
		body = requireLandsOnGET(t, rig.handler, deliveryLivePipeline, body, conn.delta(t, "p"), pinned)
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
	t.Parallel()
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
	hub.dirtyLegionProject(g, "INFRA")
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
// other, and counts every recompute that began after it, of any view, once the hub owes nothing.
func TestAnEvalWriteRecomputesTheEvalsViewsOfItsProjectAlone(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "INFRA")
	saveProjectRules(t, rig.database, "INFRA", deployRule("acme/widgets", "release / release"))
	createTestProject(t, rig.handler, "OTHER")
	now := rig.clock.Now()
	today := measures.Midnight(now)
	publishDeployRepo(t, rig.database, "INFRA", deployRule("acme/widgets", "release / release"), today.AddDate(0, 0, -27))

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
		conns[sub].snapshot(t, sub)
		if sub != "t" {
			conns[sub].freshness(t, "the verdict after the snapshot of "+sub)
		}
	}
	rig.settle(t)

	// step writes, and requires the change kinds the write logged and the views recomputed after
	// it: each named one once its recompute has begun, and every one once the hub owes nothing
	// (settle), when every recompute the write made due has ended.
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
			rig.waitFor(t, mark, "the recompute of "+sub+" after "+what, recomputeOfView(t, frames[sub]))
		}
		rig.settle(t)
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
	// sent reads the one delta each sub of want was sent, requiring the ids it upserts, and then,
	// once the hub owes nothing (settle), requires that no sub holds a frame: every frame the hub
	// owes is queued by then, so a second delta of a sub of want, or one of another sub, is one
	// nobody should be sent.
	sent := func(what string, want map[string][]string) {
		t.Helper()
		for _, sub := range []string{"a", "b", "o", "t"} {
			ids, ok := want[sub]
			if !ok {
				continue
			}
			delta := conns[sub].delta(t, sub)
			list := "items"
			if sub == "t" {
				list = "prs"
			}
			if got := liveUpsertIDs(t, delta, list); !slices.Equal(got, ids) {
				t.Errorf("%s sent %s the %s %v, want %v", what, sub, list, got, ids)
			}
		}
		rig.settle(t)
		for _, sub := range []string{"a", "b", "o", "t"} {
			conns[sub].empty(t, what)
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
	conns["t"].delta(t, "t")
	sent("the P0 issue's event", nil)

	step("the unowned-p0 row the refresher writes for the P0 issue", func() {
		if err := rig.hub.server.registry().WriteUnownedP0Day(t.Context(), "INFRA", today, testServerBuild); err != nil {
			t.Fatalf("write INFRA's unowned-p0 row of today: %v", err)
		}
	}, []string{"eval_days"}, []string{"a", "b"})
	sent("the unowned-p0 row", map[string][]string{"a": {"INFRA/unowned-p0"}, "b": {"INFRA/unowned-p0"}})
}

// TestASettingsSaveRecomputesTheEvalsViewsItMoves: over the refreshed history whose second
// repository holds deploy runs of its own, with four evals views open on a clock that stands still
// (two of the main project's, a window and the same window with a compare range, and the same two
// of the second project's, whose evals no pull request of the history counts for), three saves of
// the main project's delivery settings, each followed by the refresher's passes:
//
//   - a production job name no run holds, beside the one that is, moves no row and no repository
//     a read of the evals takes: the passes log no eval_days change but the word that the rules
//     are recorded (delivery.SettingsRecorded), which names no project and marks no evals view, so
//     no evals view is recomputed, and none is sent anything;
//   - the production rule moved to a job no run holds: the rewrite moves the main project's rows,
//     its eval_days changes name no view of the second project and none is the settings applied,
//     so each view of the main project is recomputed once, at once, and sent one delta that lands
//     on the route's body, and no view of the second project is recomputed;
//   - the source's repository moved to the second repository: the page's commit records the new
//     repository in the old one's place, which moves the repositories a read of the evals takes
//     (evals.DeployReposIn), and says that the rows stand at the settings
//     (delivery.SettingsApplied), the passes' one eval_days change of no eval beside those of the
//     rows the move builds and deletes, which recomputes every evals view of both projects once,
//     at once, and what it sends lands on the route's body.
//
// Before, every settings save the refresher answered marked every evals view of every project.
func TestASettingsSaveRecomputesTheEvalsViewsItMoves(t *testing.T) {
	t.Parallel()
	var other string
	page := newRefreshedEvalsPage(t, nil, deployRunsInTheSecondRepository(t, &other))
	rig, main, second := page.rig, page.fixture.Project, page.fixture.SecondProject
	today := measures.Midnight(page.now)
	window := evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1))
	compare := url.Values{"compare_from": {today.AddDate(0, 0, -55).Format(time.RFC3339)}, "compare_to": {today.AddDate(0, 0, -27).Format(time.RFC3339)}}.Encode()
	type openView struct {
		project, query string
		frame          DeliveryLiveView
		conn           *fakeLiveConn
		held           json.RawMessage
	}
	views := map[string]*openView{
		"e": {project: main, frame: page.frame, conn: page.conn, held: page.held},
		"c": {project: main, query: compare, frame: evalsLiveView(main, window, compare)},
		"x": {project: second, frame: evalsLiveView(second, window, "")},
		"y": {project: second, query: compare, frame: evalsLiveView(second, window, compare)},
	}
	subs := slices.Sorted(maps.Keys(views))
	subOf := map[string]string{}
	for _, sub := range subs {
		view := views[sub]
		spec, refusal := parseDeliveryLiveView(view.frame)
		if refusal != nil {
			t.Fatalf("read the view of %s: %+v", sub, refusal)
		}
		subOf[spec.key] = sub
		if view.conn == nil {
			view.conn = newFakeLiveConn()
			rig.hub.Subscribe(view.conn, subscribeFrame(sub, view.frame))
			view.held = view.conn.snapshot(t, sub).Body
		}
	}
	for _, sub := range subs {
		rig.quiesce(t, views[sub].frame)
	}
	// sent applies each delta sub's page was sent to the body it holds, requires that the body then
	// is the route's, and answers how many there were. The hub queues every frame it owes before it
	// owes nothing (settle), so what is not queued now is not owed.
	sent := func(what, sub string) int {
		t.Helper()
		view := views[sub]
		for deltas := 0; ; deltas++ {
			select {
			case frame := <-view.conn.frames:
				delta, ok := frame.frame.(DeliveryLiveDelta)
				if !ok || delta.Sub != sub {
					raw, _ := json.Marshal(frame.frame)
					t.Fatalf("%s: %s was sent %s, want a delta of its own", what, sub, raw)
				}
				view.held = applyLiveDelta(t, evalsView, view.held, delta)
			default:
				route := getBody(t, rig.handler, evalsTarget(t, view.project, view.held, view.query))
				if got, want := liveComparable(t, mustEncodeLive(t, evalsView, view.held)), liveComparable(t, mustEncodeLive(t, evalsView, route)); !reflect.DeepEqual(got, want) {
					t.Errorf("%s: %s does not hold the route's body:\nheld: %v\nGET:  %v", what, sub, got, want)
				}
				return deltas
			}
		}
	}
	for _, sub := range subs {
		if n := sent("the views' first answers", sub); n != 0 {
			t.Fatalf("%s was sent %d deltas after its snapshot, before any save", sub, n)
		}
	}
	// save saves the main project's delivery settings as change makes them, runs the refresher's
	// passes, and waits until the hub owes nothing for what they logged; it answers the eval_days
	// changes the passes logged and how many times each view was recomputed, failing the test where
	// one was recomputed for another cause than the refresher's settings rewrite. Every save records
	// the new rules in the commit that builds under them, which logs one change that they are
	// recorded (delivery.SettingsRecorded): that change marks the delivery views and no evals view,
	// so save requires exactly one of it and answers the other eval_days changes alone.
	settings := page.fixture.Settings
	save := func(what string, change func(*delivery.DeliverySettings)) ([]delivery.Change, map[string]int) {
		t.Helper()
		change(&settings)
		saveProjectRules(t, rig.database, main, settings)
		before, mark := latestChange(t, rig.database), rig.mark()
		page.refresh(t)
		if _, err := delivery.DrainPending(t.Context(), rig.database.Pool); err != nil {
			t.Fatalf("%s: drain the change log's outbox: %v", what, err)
		}
		logged, err := delivery.ChangesSince(t.Context(), rig.database.Pool, before, 100000)
		if err != nil {
			t.Fatalf("%s: read the changes the passes logged: %v", what, err)
		}
		var changes []delivery.Change
		recorded := 0
		for _, change := range logged {
			switch {
			case delivery.SettingsRecorded(change):
				recorded++
			case change.Kind == delivery.ChangeKindEvalDays:
				changes = append(changes, change)
			}
		}
		if recorded != 1 {
			t.Errorf("%s: the passes logged %d changes that the settings are recorded, want 1", what, recorded)
		}
		rig.hub.catchUpNow()
		rig.clock.Advance(viewMaxWait)
		rig.settle(t)
		recomputed := map[string]int{}
		rig.mu.Lock()
		defer rig.mu.Unlock()
		for _, note := range rig.notes[mark:] {
			sub, ok := subOf[note.view]
			if note.what != "recompute begin" || !ok {
				continue
			}
			recomputed[sub]++
			if note.cause != deliveryCauseSettingsRewrite {
				t.Errorf("%s: %s was recomputed for %s, want %s", what, sub, note.cause, deliveryCauseSettingsRewrite)
			}
		}
		return changes, recomputed
	}

	what := "a production job name no run holds"
	changes, recomputed := save(what, func(s *delivery.DeliverySettings) {
		s.ProductionJobNames = append(slices.Clone(s.ProductionJobNames), "release / a name no run holds")
	})
	if len(changes) != 0 {
		t.Errorf("%s: the passes logged the eval_days changes %+v, want none: it moves no row and no repository a read takes", what, changes)
	}
	if len(recomputed) != 0 {
		t.Errorf("%s recomputed the evals views %v, want none", what, recomputed)
	}
	for _, sub := range subs {
		if n := sent(what, sub); n != 0 {
			t.Errorf("%s sent %s %d deltas, want none", what, sub, n)
		}
	}
	t.Logf("%s logged no eval_days change and recomputed no evals view", what)

	what = "the production rule moved to a job no run holds"
	changes, recomputed = save(what, func(s *delivery.DeliverySettings) { s.ProductionJobNames = []string{"release / a job no run holds"} })
	if !slices.ContainsFunc(changes, func(change delivery.Change) bool { return change.Repo == main }) {
		t.Fatalf("%s: the passes logged the eval_days changes %+v, and the check needs a rewrite that moves %s's rows", what, changes, main)
	}
	for _, change := range changes {
		if delivery.SettingsApplied(change) || change.Repo == second {
			t.Errorf("%s: the passes logged the eval_days change %+v, want none of no eval and none of %s", what, change, second)
		}
	}
	for sub, want := range map[string]int{"e": 1, "c": 1, "x": 0, "y": 0} {
		if recomputed[sub] != want {
			t.Errorf("%s recomputed %s %d times, want %d", what, sub, recomputed[sub], want)
		}
		if n := sent(what, sub); n != want {
			t.Errorf("%s sent %s %d deltas, want %d", what, sub, n, want)
		}
	}
	t.Logf("%s logged %d eval_days changes and recomputed the evals views %v", what, len(changes), recomputed)

	what = "the source's repository moved to " + other
	changes, recomputed = save(what, func(s *delivery.DeliverySettings) { s.DeployRepo = other })
	if applied := slices.IndexFunc(changes, delivery.SettingsApplied); applied < 0 ||
		slices.ContainsFunc(changes[applied+1:], delivery.SettingsApplied) {
		t.Errorf("%s: the passes logged the eval_days changes %+v, want the settings applied once", what, changes)
	}
	for _, sub := range subs {
		if recomputed[sub] != 1 {
			t.Errorf("%s recomputed %s %d times, want once", what, sub, recomputed[sub])
		}
		if n := sent(what, sub); n > 1 {
			t.Errorf("%s sent %s %d deltas, want at most one", what, sub, n)
		}
	}
	t.Logf("%s logged the settings applied and recomputed the evals views %v", what, recomputed)
}

// TestAnEvalsViewsSnapshotIsTheListRead: an evals view's snapshot is, byte for byte, the body
// GET /api/v1/projects/{key}/evals answers for its project, the days its window covers and the
// rest of its query: a custom range with a compare range, and a preset with a kind, a repository
// and a page. Its list is sent by eval id. No refresher pass has run here, so the body states no
// refresher age and the view sets no expiry: nothing but a read moves it
// (TestAnEvalsPageIsSentTheRefreshersAgeAsItGrows has the age's).
// A view of a project no row holds is told PROJECT_NOT_FOUND once, as the route answers, and is
// answered once the project's creation seeds its evals.
func TestAnEvalsViewsSnapshotIsTheListRead(t *testing.T) {
	t.Parallel()
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
		snapshot := conn.snapshot(t, sub)
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
	conn.refusal(t, "new", errProjectNotFound.code)
	rig.requireNothingSent(t, conn, "the refusal of a project no row holds")
	createTestProject(t, rig.handler, "NEWP")
	snapshot := conn.snapshot(t, "new")
	requireTheListRead(t, rig.handler, "NEWP", snapshot.Body, "")
}

// TestAnEvalsPageIsSentTheRefreshersAgeAsItGrows: an evals body states the refresher's age at its
// read (evals.RefresherAgeIn), which its view's revision keeps only coarsely (refresherAgeStep: a
// step per doubling of the age from a minute on), so a read whose age moved within one step sends
// nothing; and the view expires when the step would next move, and its read then sends the
// freshness with the age of that moment. On a clock that stands still, with the refresher's last
// pass 30 s before it and nothing written after it, the page holds an age of 30 s; 20 s on nothing
// is sent; once the age passes a minute the page is sent the freshness alone, with that moment's
// age, which a page that subscribes then is answered too; and once it passes two minutes, again.
// Before, the revision left the age out and the view had no expiry, so a page held the age of its
// last read for as long as no write moved the view: hours, where the refresher had stopped.
func TestAnEvalsPageIsSentTheRefreshersAgeAsItGrows(t *testing.T) {
	t.Parallel()
	clock := newStillHubClock()
	rig := newClockedHubRig(t, clock)
	createTestProject(t, rig.handler, "INFRA")
	ran := clock.Now().Add(-30 * time.Second)
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into eval_days_cursor (name, updated_at) values ('delivery_changes', $1), ('events', $1)
		on conflict (name) do update set updated_at = excluded.updated_at
	`, ran); err != nil {
		t.Fatalf("stamp the refresher's cursors as its last pass does: %v", err)
	}
	ageOf := func(what string, raw json.RawMessage) int {
		t.Helper()
		var freshness evalFreshness
		if err := json.Unmarshal(raw, &freshness); err != nil || freshness.RefresherAgeSeconds == nil {
			t.Fatalf("%s: freshness %s (%v), want a refresher age", what, raw, err)
		}
		return *freshness.RefresherAgeSeconds
	}
	bodyAge := func(what string, body json.RawMessage) int {
		t.Helper()
		var shape struct {
			Freshness json.RawMessage `json:"freshness"`
		}
		if err := json.Unmarshal(body, &shape); err != nil {
			t.Fatalf("%s: read the body's freshness: %v", what, err)
		}
		return ageOf(what, shape.Freshness)
	}
	frame := evalsLiveView("INFRA", DeliveryLiveWindow{Preset: new(28)}, "")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("e", frame))
	held := conn.snapshot(t, "e").Body
	if age := bodyAge("the snapshot", held); age != 30 {
		t.Fatalf("the snapshot states the refresher's age %d s, want 30", age)
	}
	clock.Advance(20 * time.Second)
	rig.requireNothingSent(t, conn, "20 s more of the refresher's age, within its step")

	// grown moves the clock by d, and answers the one frame the hub then owes the page, which must
	// be a delta setting the freshness alone, applied to the body the page holds.
	grown := func(what string, d time.Duration) int {
		t.Helper()
		clock.Advance(d)
		rig.settle(t)
		var sent sentFrame
		select {
		case sent = <-conn.frames:
		default:
			t.Fatalf("%s: the page was sent nothing once the hub owed nothing", what)
		}
		delta, ok := sent.frame.(DeliveryLiveDelta)
		if set, items, mapNames, counts := liveSections(delta); !ok || !slices.Equal(set, []string{"freshness"}) || len(items)+len(mapNames)+len(counts) != 0 {
			raw, _ := json.Marshal(sent.frame)
			t.Fatalf("%s: the page was sent %s, want a delta setting the freshness alone", what, raw)
		}
		held = applyLiveDelta(t, evalsView, held, delta)
		rig.requireNothingSent(t, conn, what)
		return ageOf(what, delta.Set["freshness"])
	}
	if age := grown("the age past a minute", 11*time.Second); age != 61 {
		t.Errorf("past a minute the page was sent the refresher's age %d s, want 61", age)
	}
	late := newFakeLiveConn()
	rig.hub.Subscribe(late, subscribeFrame("l", frame))
	if age := bodyAge("a later page's snapshot", late.snapshot(t, "l").Body); age != 61 {
		t.Errorf("a page that subscribed then was answered the refresher's age %d s, want 61", age)
	}
	if age := grown("the age past two minutes", time.Minute); age != 121 {
		t.Errorf("past two minutes the page was sent the refresher's age %d s, want 121", age)
	}
	if age := bodyAge("the body the page holds", held); age != 121 {
		t.Errorf("the page holds the refresher's age %d s, want 121", age)
	}
}

// TestAnEvalsViewOfNoProjectKeyIsRefusedAsTheRouteRefusesIt: GET /api/v1/projects/{key}/evals
// answers a key that is no project key 404 PROJECT_NOT_FOUND, as it answers every key no project
// row holds (loadProject), so an evals view of such a key is refused with that code, before it is
// held, as parseDeliveryLiveView refuses every view the matching route refuses.
func TestAnEvalsViewOfNoProjectKeyIsRefusedAsTheRouteRefusesIt(t *testing.T) {
	t.Parallel()
	handler, _ := newTestHandlerWithStore(t)
	const notAKey = "not-a-key"
	routed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+notAKey+"/evals", nil, "alice")
	var answer evalRefusal
	if routed.Code != http.StatusNotFound || json.Unmarshal(routed.Body.Bytes(), &answer) != nil || answer.Code != errProjectNotFound.code {
		t.Fatalf("GET the evals of %q: status=%d body=%s, want 404 %s", notAKey, routed.Code, routed.Body.String(), errProjectNotFound.code)
	}
	if _, refusal := parseDeliveryLiveView(evalsLiveView(notAKey, DeliveryLiveWindow{Preset: new(28)}, "")); refusal == nil || refusal.Code != answer.Code {
		t.Errorf("an evals view of %q: refusal = %+v, want the route's %s", notAKey, refusal, answer.Code)
	}
}

// TestAReadingReachesAnEvalsPageOverTheLiveSocket: a page subscribed to its project's evals
// over GET /ws/live is sent the list read as its snapshot, and, after one readings write and with
// no second subscription, the delta that lands its body on the route's answer.
func TestAReadingReachesAnEvalsPageOverTheLiveSocket(t *testing.T) {
	t.Parallel()
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	createTestProject(t, rig.handler, "INFRA")
	now := time.Now().UTC()
	today := measures.Midnight(now)
	window := evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1))

	client := rig.dial(t, asUser("alice"))
	client.send(t, deliverySubscribe("evals", evalsLiveView("INFRA", window, "")))
	snapshot := client.snapshot(t, "evals")
	requireTheListRead(t, rig.handler, "INFRA", snapshot.Body, "")

	// One reading of an eval no row holds: the write registers it, logs the reading and writes its
	// day row, in one transaction.
	written := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/INFRA/evals/pr-size/readings",
		map[string]any{"readings": []any{reading("lines", 120, now.Add(-time.Hour))}}, "socket-reading")
	if written.Code != http.StatusCreated {
		t.Fatalf("log the reading: status %d, body %s", written.Code, written.Body.String())
	}
	delta := client.delta(t, "evals")
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

// TestAReadingDaysBackSendsTheEvalsPageOneDelta (k10): a reading of an eval no row holds, measured
// three days before today on a project whose day rows the refresher has walked through today,
// leaves owed the zero row of each later day through today, which the readings write writes in its
// own transaction beside the reading's day row (evals' writeLoggedZeros). A page subscribed to the
// project's evals over the 28 days through today, on a hub whose clock stands still, is sent one
// delta for the write. The refresher's pass of the next day then fills that eval's new day
// (fillLoggedZeros), which lies past the page's window, and the page is sent nothing more, however
// far the clock moves past every quiet, and holds the route's body. A write that left those zero
// rows to the refresher's fill sent the page a second delta, for the days the fill wrote. Both
// passes run as of a moment after the hub's clock, so the refresher's age each body states reads
// one coarse step (refresherAgeStep) before and after them.
func TestAReadingDaysBackSendsTheEvalsPageOneDelta(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "INFRA")
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	rig := hubOn(t, handler, database, deps, newStillHubClock(), nil)
	today := measures.Midnight(time.Now())
	pass := today.Add(13 * time.Hour)
	refreshEvalDays(t, registry, database, pass)
	frame := evalsLiveView("INFRA", evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), "")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("e", frame))
	held := conn.snapshot(t, "e").Body
	rig.quiesce(t, frame)
	rig.requireNothingSent(t, conn, "the snapshot, before anything was written")

	written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/INFRA/evals/pr-size/readings",
		map[string]any{"readings": []any{reading("lines", 120, today.AddDate(0, 0, -3).Add(9*time.Hour))}}, "days-back-reading")
	if written.Code != http.StatusCreated {
		t.Fatalf("log the reading: status %d, body %s", written.Code, written.Body.String())
	}
	rig.quiesce(t, frame)
	delta := conn.delta(t, "e")
	if got := liveUpsertIDs(t, delta, "items"); !slices.Equal(got, []string{"INFRA/pr-size"}) {
		t.Errorf("the reading's delta upserts %v, want INFRA/pr-size", got)
	}
	held = applyLiveDelta(t, evalsView, held, delta)

	refreshEvalDays(t, registry, database, pass.AddDate(0, 0, 1))
	rig.quiesce(t, frame)
	rig.requireNothingSent(t, conn, "the reading's one delta and the refresher's passes after it")
	route := getBody(t, handler, evalsTarget(t, "INFRA", held, ""))
	if got, want := liveComparable(t, mustEncodeLive(t, evalsView, held)), liveComparable(t, mustEncodeLive(t, evalsView, route)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the page does not hold the route's body after the reading and the refresher's passes:\nheld: %v\nGET:  %v", got, want)
	}
	t.Logf("the reading of %s's day sent one delta of seq %s, which lands on the route's body; the next day's pass after it sent nothing",
		today.AddDate(0, 0, -3).Format(time.DateOnly), delta.Seq)
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

// evalsRebuild is a step's kind for an eval_days change a rebuild of a span wrote, which carries
// the rebuild's mark (delivery.EvalDaysWrittenByRebuild); evalsApplied for the refresher's word
// that the day rows stand at the delivery settings (delivery.SettingsApplied), an eval_days change
// of no eval that carries the settings rewrite's mark; and evalsDefaults for the eval_days change
// of no eval the refresher's page logs for INFRA where its commit moves the default repository
// INFRA publishes (delivery.EvalDaysWrittenByDefaults).
const (
	evalsRebuild  = "the rebuild's eval_days change"
	evalsApplied  = "the settings applied"
	evalsDefaults = "the defaults published"
)

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
			case evalsRebuild:
				change = evalsChange(delivery.ChangeKindEvalDays, "INFRA", slug, &today, &today)
				change.RunKind = delivery.EvalDaysWrittenByRebuild
			case evalsApplied:
				change = delivery.Change{Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenBySettingsRewrite}
			case evalsDefaults:
				change = delivery.Change{Kind: delivery.ChangeKindEvalDays, RunKind: delivery.EvalDaysWrittenByDefaults, Repo: "INFRA"}
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
// its own cause, whatever came before it. A settings change marks no evals view (touchedBy), so the
// rewrite is due at once 100 ms after it as 45 s after it, after a recompute of the view since the
// settings change as with none, where the change moved the deploy repository and no day row (the
// settings applied), and with no settings change heard before it, as by a page that subscribed
// between the settings write and the rewrite's commit. A failed read's retry still holds the
// rewrite back, and a view being computed is due nowhere until that recompute ends, and the
// rewrite then at once.
func TestASettingsRewriteIsRecomputedAtOnce(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	settings, rewrite := delivery.ChangeKindSettings, evalsRewrite
	// pass is a refresher's pass that commits 1.5 s after the settings write, and slow one 45 s
	// after it, as a pass over a larger history, or one a loaded database holds back, can.
	pass, slow := 1500*time.Millisecond, 45*time.Second
	cases := []struct {
		name  string
		steps []evalsMarkStep
		// due is when the view is due after start; computing is a view being computed, which is due
		// nowhere.
		due       time.Duration
		computing bool
	}{
		{"the rewrite of two evals after a recompute since the settings change", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""},
			{pass, rewrite, ""}, {pass, rewrite, evals.SlugMergeToProduction},
		}, pass, false},
		{"the rewrite 100 ms after the settings change, which marks no evals view: due at its own mark", []evalsMarkStep{
			{0, settings, ""}, {100 * time.Millisecond, rewrite, ""},
		}, 100 * time.Millisecond, false},
		{"the settings applied where the change moved no day row, 1.5 s after the settings change", []evalsMarkStep{
			{0, settings, ""}, {pass, evalsApplied, ""},
		}, pass, false},
		{"the rewrite 45 s after the settings change, long after a recompute since it", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {slow, rewrite, ""},
		}, slow, false},
		{"the rewrite with no settings change heard before it", []evalsMarkStep{{0, rewrite, ""}}, 0, false},
		{"the rewrite behind a failed read's retry", []evalsMarkStep{
			{0, settings, ""}, {viewQuiet, evalsAnswered, ""}, {time.Second, evalsRetryHeld, ""}, {pass, rewrite, ""},
		}, time.Second + deliveryLiveRetry, false},
		{"the rewrite while a recompute since the settings change is computing", []evalsMarkStep{
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
// day writes that are not the rewrite's); a reading or a version after a
// settings change, which is no rewrite of day rows; a day write that is not the rewrite's, 1.5 s
// after a settings change that moved no day row, whose rewrite therefore committed none; and a day
// write after the rewrite's.
func TestEveryOtherEvalsViewMarkWaitsTheQuiet(t *testing.T) {
	t.Parallel()
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

// TestAViewUnderMarksLessThanTheQuietApartIsDueASecondAfterItsFirst: marks that keep coming less
// than viewQuiet apart, as the refresher's passes and their echoes make under a stream of changes,
// never let the view's quiet begin, and hold it no longer than viewMaxWait: it is due one second
// after its first mark, whatever marks come after that second. Day writes 200 ms apart through
// 0.8 s and through 3 s, and readings 100 ms apart through 0.9 s.
func TestAViewUnderMarksLessThanTheQuietApartIsDueASecondAfterItsFirst(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	every := func(kind string, apart, through time.Duration) []evalsMarkStep {
		var steps []evalsMarkStep
		for at := time.Duration(0); at <= through; at += apart {
			steps = append(steps, evalsMarkStep{at, kind, ""})
		}
		return steps
	}
	days, readings := delivery.ChangeKindEvalDays, delivery.ChangeKindEvalReadings
	for _, c := range []struct {
		name  string
		steps []evalsMarkStep
	}{
		{"day writes 200 ms apart through 0.8 s", every(days, 200*time.Millisecond, 800*time.Millisecond)},
		{"day writes 200 ms apart through 3 s", every(days, 200*time.Millisecond, 3*time.Second)},
		{"readings 100 ms apart through 0.9 s", every(readings, 100*time.Millisecond, 900*time.Millisecond)},
	} {
		at, cause, ok := markEvalsView(start, c.steps).dueAt()
		if want := start.Add(time.Second); !ok || !at.Equal(want) || cause != deliveryCauseChange {
			t.Errorf("%s: due %t, %s after the first mark (%s), want 1s after it (%s)", c.name, ok, at.Sub(start), cause, deliveryCauseChange)
			continue
		}
		t.Logf("%s: %d marks, due %s after the first, its last %s after it", c.name, len(c.steps), at.Sub(start), c.steps[len(c.steps)-1].after)
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
	createTestProject(t, rig.handler, "INFRA")
	saveProjectRules(t, rig.database, "INFRA", deployRule("acme/widgets", "release / release"))
	today := measures.Midnight(rig.clock.Now())
	publishDeployRepo(t, rig.database, "INFRA", deployRule("acme/widgets", "release / release"), today.AddDate(0, 0, -27))
	page := &settingsRewritePage{
		rig: rig, conn: newFakeLiveConn(), today: today,
		frame: evalsLiveView("INFRA", evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), ""),
	}
	rig.hub.Subscribe(page.conn, subscribeFrame("e", page.frame))
	page.held = page.conn.snapshot(t, "e").Body
	rig.settle(t)
	return page
}

// saveSettings saves INFRA's delivery settings with one more production job named, which logs the
// settings changes of INFRA's rows alone, catches the hub up and moves its clock viewMaxWait on,
// past every quiet, and requires that no recompute of the evals view began and nothing was sent: a
// settings change marks no evals view (touchedBy). No run holds the name, so the change moves no
// eval and the refresher's rewrite of it commits no day row (evals'
// TestASettingsChangeThatMovesNoEvalWritesNothing); a test writes the day rows that follow it as
// the rewrite's or as another writer's (writeDays).
func (p *settingsRewritePage) saveSettings(t *testing.T) {
	t.Helper()
	// Every mark the page's setup left (the feed's seeding marks every view) is answered first, so
	// a recompute after the save is the save's.
	p.rig.quiesce(t, p.frame)
	before, mark := latestChange(t, p.rig.database), p.rig.mark()
	settings, read := seededDeliveryRules(t, p.rig.database.Pool).For("acme/widgets")
	if !read {
		t.Fatal("no deploy source reads acme/widgets")
	}
	settings.ProductionJobNames = append(slices.Clone(settings.ProductionJobNames), "release / release-canary")
	saveProjectRules(t, p.rig.database, "INFRA", settings)
	changes, err := delivery.ChangesSince(t.Context(), p.rig.database.Pool, before, 100)
	if err != nil {
		t.Fatalf("read the changes the save logged: %v", err)
	}
	if len(changes) == 0 || slices.ContainsFunc(changes, func(change delivery.Change) bool {
		return change.Kind != delivery.ChangeKindSettings || change.Repo != "INFRA"
	}) {
		t.Fatalf("the settings save logged %+v, want changes of INFRA's settings alone", changes)
	}
	p.rig.hub.catchUpNow()
	p.rig.clock.Advance(viewMaxWait)
	p.rig.requireNothingSent(t, p.conn, "a settings change, which marks no evals view: the refresher's rewrite does")
	if n := p.rig.count(mark, recomputeBegin("")); n != 0 {
		t.Fatalf("the settings change began %d recomputes of the evals view, want none before the refresher has brought the rows to it", n)
	}
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
	if byRewrite {
		return p.writeDaysAs(t, after, delivery.EvalDaysWrittenBySettingsRewrite)
	}
	return p.writeDaysAs(t, after, "")
}

// writeDaysAs is writeDays' write, its transaction marked with the cause runKind: the settings
// rewrite's (evals.MarkSettingsRewrite), a rebuild's (evals.MarkRebuild), or none for "".
func (p *settingsRewritePage) writeDaysAs(t *testing.T, after time.Duration, runKind string) (int, int64) {
	t.Helper()
	p.rig.clock.Advance(after)
	before, mark := latestChange(t, p.rig.database), p.rig.mark()
	tx, err := p.rig.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the write of the day rows: %v", err)
	}
	defer tx.Rollback(t.Context())
	switch runKind {
	case delivery.EvalDaysWrittenBySettingsRewrite:
		if err := evals.MarkSettingsRewrite(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
	case delivery.EvalDaysWrittenByRebuild:
		if err := evals.MarkRebuild(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
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
	delta := p.conn.delta(t, "e")
	if moved := slices.Sorted(slices.Values(liveUpsertIDs(t, delta, "items"))); !slices.Equal(moved, dayRowEvals) {
		t.Fatalf("%s's delta upserts %v, want every eval it wrote and nothing else, %v", what, moved, dayRowEvals)
	}
	return delta
}

// TestASettingsChangeAndItsRewriteSendTheEvalsPageOneDeltaAtOnce: a settings change marks no evals
// page, and no recompute of it begins (saveSettings). The refresher's rewrite of what the change
// moved, one transaction holding two evals' rows and marked the rewrite's, arrives on the hub's
// clock 2 s after the settings change (saveSettings moves the clock viewMaxWait, the write a second
// more), or 46 s after it, as a pass on a larger history or a loaded database can, and the clock
// then stands still: the rewrite is recomputed at its own mark, and the page gets one delta holding
// both evals' new numbers, which lands on the route's body. Nothing follows it, however far the
// clock moves past every quiet.
func TestASettingsChangeAndItsRewriteSendTheEvalsPageOneDeltaAtOnce(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		after time.Duration
	}{
		{"the rewrite 2 s after the settings change", time.Second},
		{"the rewrite 46 s after the settings change", 45 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			page := newSettingsRewritePage(t, nil)
			page.saveSettings(t)
			mark, _ := page.writeDays(t, c.after, true)
			delta := page.dayRowsDelta(t, "the rewrite")
			page.apply(t, delta, page.route(t))
			page.rig.clock.Advance(viewMaxWait)
			page.rig.requireNothingSent(t, page.conn, "the rewrite's one delta, past every quiet")
			begun, rewrites := page.rig.count(mark, recomputeBegin("")), page.rig.count(mark, recomputeBegin(deliveryCauseSettingsRewrite))
			if begun != 1 || rewrites != 1 {
				t.Fatalf("the rewrite began %d recomputes, %d of them %s, want the one", begun, rewrites, deliveryCauseSettingsRewrite)
			}
			t.Logf("the rewrite's eval_days changes were recomputed once (%s) on a clock standing at their mark, and its one delta upserts %v and lands on the route's body",
				deliveryCauseSettingsRewrite, dayRowEvals)
		})
	}
}

// TestADayWriteAfterASettingsChangeThatMovedNoDayRowWaitsItsQuiet: a settings change marks no evals
// page (saveSettings), and the change moves no eval, so the refresher's rewrite of it commits no day
// row. A write of two evals' day rows that is not the rewrite's then lands 2 s after the settings
// change (saveSettings moves the clock viewMaxWait, the write a second more), on a clock that then
// stands still: no recompute begins and nothing is sent until the clock passes the write's quiet,
// and then the page gets one delta holding both evals' new numbers, from a recompute of cause
// change, which lands on the route's body.
func TestADayWriteAfterASettingsChangeThatMovedNoDayRowWaitsItsQuiet(t *testing.T) {
	t.Parallel()
	page := newSettingsRewritePage(t, nil)
	page.saveSettings(t)
	mark, _ := page.writeDays(t, time.Second, false)
	page.rig.requireNothingSent(t, page.conn, "a day write that is not the rewrite's, on a clock that has not moved past its quiet")
	if n := page.rig.count(mark, recomputeBegin("")); n != 0 {
		t.Fatalf("the day write began %d recomputes on a clock that has not moved past its quiet, want 0", n)
	}
	page.rig.clock.Advance(viewQuiet)
	delta := page.dayRowsDelta(t, "the day write")
	page.apply(t, delta, page.route(t))
	page.rig.requireNothingSent(t, page.conn, "the day write's one delta")
	changes, rewrites := page.rig.count(mark, recomputeBegin(deliveryCauseChange)), page.rig.count(mark, recomputeBegin(deliveryCauseSettingsRewrite))
	if changes != 1 || rewrites != 0 {
		t.Fatalf("the day write began %d %s recomputes and %d %s recomputes, want one %s alone",
			changes, deliveryCauseChange, rewrites, deliveryCauseSettingsRewrite, deliveryCauseChange)
	}
	t.Logf("the day write, 2 s after a settings change that moved no day row, sent nothing until its quiet passed, and then one delta upserting %v from one %s recompute",
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
	t.Parallel()
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
	page.rig.requireNothingSent(t, page.conn, "the rewrite's delta, before the reading's quiet has passed")
	if n := page.rig.count(resumed, recomputeBegin("")); n != 0 {
		t.Fatalf("recomputes after the rewrite's, before the reading's quiet passed = %d, want 0", n)
	}

	page.rig.clock.Advance(viewQuiet)
	second := page.conn.delta(t, "e")
	if got := liveUpsertIDs(t, second, "items"); !slices.Equal(got, []string{"INFRA/review-time"}) {
		t.Fatalf("the delta after the reading's quiet upserts %v, want INFRA/review-time", got)
	}
	page.apply(t, second, page.route(t))
	page.rig.requireNothingSent(t, page.conn, "the reading's delta")
	rewrites, changes := page.rig.count(mark, recomputeBegin(deliveryCauseSettingsRewrite)), page.rig.count(mark, recomputeBegin(deliveryCauseChange))
	if rewrites != 1 || changes != 1 {
		t.Fatalf("since the rewrite: %d %s recomputes and %d %s recomputes, want one of each",
			rewrites, deliveryCauseSettingsRewrite, changes, deliveryCauseChange)
	}
	t.Logf("the rewrite's read held position %d; its one delta upserts %v and lands on the route's body before the reading; the reading, marked while the view was computed, waited its quiet and its delta upserts %v",
		note.through, dayRowEvals, liveUpsertIDs(t, second, "items"))
}

// TestARebuildIsRecomputedAtOnce: an eval_days change a rebuild of a span wrote, which carries the
// rebuild's mark, is due at its own mark rather than viewQuiet after it (dueAt), with its own
// cause, as the settings rewrite's is: alone; with a second eval's of the same commit; inside the
// quiet of a day write that is not a rebuild's, whose mark the rebuild's then is, so the one
// recompute is due at that first mark; and never before a failed read's retry. A change of the
// settings row alone leaves an evals view not due at all.
func TestARebuildIsRecomputedAtOnce(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	days, rebuild := delivery.ChangeKindEvalDays, evalsRebuild
	cases := []struct {
		name  string
		steps []evalsMarkStep
		due   time.Duration
	}{
		{"a rebuild's change of one eval", []evalsMarkStep{{time.Second, rebuild, ""}}, time.Second},
		{"a rebuild's changes of two evals", []evalsMarkStep{{time.Second, rebuild, ""}, {time.Second, rebuild, evals.SlugMergeToProduction}}, time.Second},
		{"a rebuild inside the quiet of another day write", []evalsMarkStep{{0, days, ""}, {100 * time.Millisecond, rebuild, ""}}, 0},
		{"a rebuild behind a failed read's retry", []evalsMarkStep{{0, evalsRetryHeld, ""}, {time.Second, rebuild, ""}}, deliveryLiveRetry},
	}
	for _, c := range cases {
		at, cause, ok := markEvalsView(start, c.steps).dueAt()
		if !ok || !at.Equal(start.Add(c.due)) || cause != deliveryCauseRebuild {
			t.Errorf("%s: due %t, %s after the start (%s), want %s after it (%s)", c.name, ok, at.Sub(start), cause, c.due, deliveryCauseRebuild)
			continue
		}
		t.Logf("%s: due %s after the start, cause %s", c.name, at.Sub(start), cause)
	}
	if at, cause, ok := markEvalsView(start, []evalsMarkStep{{0, delivery.ChangeKindSettings, ""}, {time.Second, delivery.ChangeKindSettings, ""}}).dueAt(); ok {
		t.Errorf("two changes of the settings row leave an evals view due %s after the first (%s), want not due: the refresher's rewrite marks it", at.Sub(start), cause)
	}
}

// TestADefaultsPublishIsRecomputedAtOnce: the eval_days change the refresher's page logs for a
// project whose published default repository its commit moves, which carries the publishing's
// mark, is due at its own mark rather than viewQuiet after it (dueAt), with its own cause, as a
// rebuild's is: alone; inside the quiet of a day write that is not its own, whose mark it then is,
// so the one recompute is due at that first mark; and never before a failed read's retry.
func TestADefaultsPublishIsRecomputedAtOnce(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		steps []evalsMarkStep
		due   time.Duration
	}{
		{"a publishing of the defaults", []evalsMarkStep{{time.Second, evalsDefaults, ""}}, time.Second},
		{"a publishing inside the quiet of a day write", []evalsMarkStep{{0, delivery.ChangeKindEvalDays, ""}, {100 * time.Millisecond, evalsDefaults, ""}}, 0},
		{"a publishing behind a failed read's retry", []evalsMarkStep{{0, evalsRetryHeld, ""}, {time.Second, evalsDefaults, ""}}, deliveryLiveRetry},
	} {
		at, cause, ok := markEvalsView(start, c.steps).dueAt()
		if !ok || !at.Equal(start.Add(c.due)) || cause != deliveryCauseDefaults {
			t.Errorf("%s: due %t, %s after the start (%s), want %s after it (%s)", c.name, ok, at.Sub(start), cause, c.due, deliveryCauseDefaults)
			continue
		}
		t.Logf("%s: due %s after the start, cause %s", c.name, at.Sub(start), cause)
	}
}

// TestARebuildsCommitSendsTheEvalsPageOneDeltaAtOnce: one transaction that holds two evals' day
// rows and carries the rebuild's mark, as the refresher's rebuild of a span commits them, reaches
// a page on a hub whose clock stands still: it is recomputed at its own mark, once, and the page
// gets one delta holding both evals' new numbers, which lands on the route's body. Nothing follows
// it, however far the clock moves past every quiet.
func TestARebuildsCommitSendsTheEvalsPageOneDeltaAtOnce(t *testing.T) {
	t.Parallel()
	page := newSettingsRewritePage(t, nil)
	mark, _ := page.writeDaysAs(t, time.Second, delivery.EvalDaysWrittenByRebuild)
	delta := page.dayRowsDelta(t, "the rebuild")
	page.apply(t, delta, page.route(t))
	page.rig.clock.Advance(viewMaxWait)
	page.rig.requireNothingSent(t, page.conn, "the rebuild's one delta, past every quiet")
	begun, rebuilds := page.rig.count(mark, recomputeBegin("")), page.rig.count(mark, recomputeBegin(deliveryCauseRebuild))
	if begun != 1 || rebuilds != 1 {
		t.Fatalf("the rebuild began %d recomputes, %d of them %s, want the one", begun, rebuilds, deliveryCauseRebuild)
	}
	t.Logf("the rebuild's eval_days changes were recomputed once (%s) on a clock standing at their mark, and its one delta upserts %v and lands on the route's body",
		deliveryCauseRebuild, dayRowEvals)
}

// refreshedEvalsPage is a page subscribed to the main project's evals over the 28 days ending on
// the last day of the delivery history of 30 days (seedEvalHistories), whose day rows and stored
// figures the refresher has written (refreshEvalDays), on a hub whose clock stands still. held is
// the body the page holds.
type refreshedEvalsPage struct {
	rig      *hubRig
	conn     *fakeLiveConn
	frame    DeliveryLiveView
	held     json.RawMessage
	registry *evals.Store
	fixture  deliverytest.Fixture
	now      time.Time
}

// newRefreshedEvalsPage seeds the history, runs prepare (where it is not nil) on it, refreshes the
// day rows with a registry over database, or over traced where that is given a tracer, and
// subscribes the page.
func newRefreshedEvalsPage(t *testing.T, tracer pgx.QueryTracer, prepare func(database *store.Store, fixture deliverytest.Fixture)) *refreshedEvalsPage {
	t.Helper()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	fixture, windows := seedEvalHistories(t, database, deps, 30)
	end := windows[0].Spec.Now
	if fixture.To.After(end) {
		end = fixture.To
	}
	page := &refreshedEvalsPage{conn: newFakeLiveConn(), fixture: fixture, now: end.Add(-time.Minute), registry: evals.New(database, deps.Docs, deps.Events, deps.Attributor)}
	if tracer != nil {
		page.registry = evals.New(storetest.TracedStore(t, database, tracer), deps.Docs, deps.Events, deps.Attributor)
	}
	if prepare != nil {
		prepare(database, fixture)
	}
	page.rig = hubOn(t, handler, database, deps, newStillHubClock(), nil)
	page.refresh(t)
	today := measures.Midnight(page.now)
	page.frame = evalsLiveView(fixture.Project, evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), "")
	page.rig.hub.Subscribe(page.conn, subscribeFrame("e", page.frame))
	page.held = page.conn.snapshot(t, "e").Body
	page.rig.settle(t)
	page.rig.quiesce(t, page.frame)
	if sent := page.take(t); sent > 0 {
		t.Fatalf("the page was sent %d deltas after its snapshot, before anything was written", sent)
	}
	return page
}

// refresh runs the refresher's passes as of the page's moment until no walk has days left.
func (p *refreshedEvalsPage) refresh(t *testing.T) {
	t.Helper()
	refreshEvalDays(t, p.registry, p.rig.database, p.now)
}

// route is the list route's body for the page's window, as of now.
func (p *refreshedEvalsPage) route(t *testing.T) json.RawMessage {
	t.Helper()
	return getBody(t, p.rig.handler, evalsTarget(t, p.fixture.Project, p.held, ""))
}

// take waits until the hub owes nothing (settle), when every frame it owes the page is queued on
// it, applies every delta queued to the body the page holds, and answers how many there were.
func (p *refreshedEvalsPage) take(t *testing.T) int {
	t.Helper()
	p.rig.settle(t)
	for taken := 0; ; taken++ {
		select {
		case sent := <-p.conn.frames:
			delta, ok := sent.frame.(DeliveryLiveDelta)
			if !ok || delta.Sub != "e" {
				raw, _ := json.Marshal(sent.frame)
				t.Fatalf("frame = %s, want a delta of the page's sub", raw)
			}
			p.held = applyLiveDelta(t, evalsView, p.held, delta)
		default:
			return taken
		}
	}
}

// requireAtRoute fails unless the body the page holds is the list route's.
func (p *refreshedEvalsPage) requireAtRoute(t *testing.T, what string) {
	t.Helper()
	if got, want := liveComparable(t, mustEncodeLive(t, evalsView, p.held)), liveComparable(t, mustEncodeLive(t, evalsView, p.route(t))); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: the page does not hold the route's body:\nheld: %v\nGET:  %v", what, got, want)
	}
}

// idle waits until the hub holds no mark of the page's view and computes it no more.
func (p *refreshedEvalsPage) idle(t *testing.T) {
	t.Helper()
	p.rig.idle(t, p.frame)
}

// idle waits until the hub holds no mark of frame's view and computes it no more; a view the hub
// does not hold is idle. Its one bound is the test binary's deadline (testwait.Until).
func (r *hubRig) idle(t *testing.T, frame DeliveryLiveView) {
	t.Helper()
	spec, _ := parseDeliveryLiveView(frame)
	testwait.Until(t, "the hub to hold no mark of the view "+spec.key+" and compute it no more (what it held)", func() (string, bool) {
		r.hub.mu.Lock()
		defer r.hub.mu.Unlock()
		var view *deliveryView
		if generation := r.hub.generation; generation != nil {
			view = generation.views[spec.key]
		}
		if view == nil {
			return "no such view", true
		}
		return fmt.Sprintf("dirty %t, computing %t", view.dirty, view.computing), !view.dirty && !view.computing
	})
}

// quiesce catches the hub up, moves its clock past every quiet and waits until frame's view is
// answered (idle): on a clock that stands still, a mark that waits its quiet is due only once the
// test moves the clock, and would otherwise be answered by the next recompute a test counts.
func (r *hubRig) quiesce(t *testing.T, frame DeliveryLiveView) {
	t.Helper()
	r.hub.catchUpNow()
	r.clock.Advance(viewMaxWait)
	r.idle(t, frame)
}

// TestAPutThatMovesTheDeployRepositoryAndAJudgedJobSendsTheEvalsPageOneDelta: the history's second
// repository holds deploy runs of its own, and one save of the delivery settings moves the deploy
// repository to it and names a production job no run holds. The change of the settings row marks
// no evals view, so on a clock moved past every quiet the page is sent nothing: a recompute then
// would answer the new repository's rows as the old production rule built them, a state between
// the old and the new. The refresher's pass rewrites the days under the new rule, and the page
// gets one delta, at once on a clock that stands still, which lands on the route's body after the
// rewrite; nothing follows it.
func TestAPutThatMovesTheDeployRepositoryAndAJudgedJobSendsTheEvalsPageOneDelta(t *testing.T) {
	t.Parallel()
	var other string
	page := newRefreshedEvalsPage(t, nil, deployRunsInTheSecondRepository(t, &other))
	before := page.route(t)
	page.moveDeployRepoAndRule(t, other)
	page.rig.hub.catchUpNow()
	page.rig.clock.Advance(viewMaxWait)
	page.rig.requireNothingSent(t, page.conn, "the settings save alone, before the refresher has rewritten the days it moves")

	mark := page.rig.mark()
	page.refresh(t)
	page.rig.hub.catchUpNow()
	delta := page.conn.delta(t, "e")
	page.held = applyLiveDelta(t, evalsView, page.held, delta)
	page.requireAtRoute(t, "the one delta after the rewrite")
	if reflect.DeepEqual(liveComparable(t, mustEncodeLive(t, evalsView, before)), liveComparable(t, mustEncodeLive(t, evalsView, page.held))) {
		t.Fatal("the route answers after the save what it answered before, and the check needs a save that moves it")
	}
	page.rig.clock.Advance(viewMaxWait)
	page.rig.requireNothingSent(t, page.conn, "the rewrite's one delta, past every quiet")
	if begun := page.rig.count(mark, recomputeBegin("")); begun != 1 {
		t.Fatalf("the rewrite began %d recomputes of the page's view, want one", begun)
	}
	t.Logf("one save that moved the deploy repository to %s and the production rule sent the page one delta, after the rewrite, which upserts %v and lands on the route's body",
		other, liveUpsertIDs(t, delta, "items"))
}

// deployRunsInTheSecondRepository is a prepare of newRefreshedEvalsPage: it copies the deploy
// repository's deploy runs started from the history's thirteenth day on, with their jobs, into the
// history's second repository, which so holds deploy runs of its own, and sets other to it.
func deployRunsInTheSecondRepository(t *testing.T, other *string) func(*store.Store, deliverytest.Fixture) {
	return func(database *store.Store, fixture deliverytest.Fixture) {
		*other = fixture.Repos[1]
		for _, statement := range []string{`
			insert into delivery_runs
			select (jsonb_populate_record(null::delivery_runs, to_jsonb(run) || jsonb_build_object('repo', $2::text))).*
			from delivery_runs run where run.repo = $1 and run.kind = 'deploy' and run.started_at >= $3`, `
			insert into delivery_run_jobs
			select (jsonb_populate_record(null::delivery_run_jobs, to_jsonb(job) || jsonb_build_object('repo', $2::text))).*
			from delivery_run_jobs job join delivery_runs run on run.repo = job.repo and run.run_id = job.run_id
			where job.repo = $1 and run.kind = 'deploy' and run.started_at >= $3`,
		} {
			if _, err := database.Pool.Exec(t.Context(), statement, fixture.DeployRepo, *other, fixture.From.AddDate(0, 0, 12)); err != nil {
				t.Fatalf("copy the deploy repository's deploy runs to %s: %v", *other, err)
			}
		}
	}
}

// moveDeployRepoAndRule saves, in one save of the delivery settings of the history's project, other
// as the repository its source reads and a production job name no run holds as the production
// rule's only one.
func (p *refreshedEvalsPage) moveDeployRepoAndRule(t *testing.T, other string) {
	t.Helper()
	settings := p.fixture.Settings
	settings.DeployRepo, settings.ProductionJobNames = other, []string{"release / a job no run holds"}
	saveProjectRules(t, p.rig.database, p.fixture.Project, settings)
}

// withoutFreshness is a JSON body decoded, without its top-level freshness, which states the
// refresher's age at the moment of the read.
func withoutFreshness(t *testing.T, body json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	delete(decoded, "freshness")
	return decoded
}

// listItemsByID is each item of an evals list body by its id.
func listItemsByID(t *testing.T, body json.RawMessage) map[string]any {
	t.Helper()
	items := map[string]any{}
	for _, item := range withoutFreshness(t, body)["items"].([]any) {
		items[item.(map[string]any)["id"].(string)] = item
	}
	return items
}

// TestAMarkBetweenASettingsSaveAndItsRewriteSendsTheOldStateWhole: as in
// TestAPutThatMovesTheDeployRepositoryAndAJudgedJobSendsTheEvalsPageOneDelta, one save moves the
// deploy repository to the history's second repository and the production rule to a job no run
// holds. Before the refresher's pass rewrites the days under the new rule, every read of the evals
// answers the old settings whole: the list route, an eval's detail and its items answer what they
// answered before the save, since each reads the deploy repository and the rule recorded with the
// stored rows (evals.DeployReposIn, evals.BuiltSettingsIn) and not those the settings name from the
// save on, which would answer the new repository's rows as the old rule built them. A reading of a
// logged eval, which is no change of the settings, then marks the page's view: its recompute sends
// a delta that upserts the logged eval alone, every other eval as the page held it before the
// save. The refresher's pass then rewrites the days, and the next delta lands the page on the
// route's body after the rewrite, which states the new settings whole; nothing follows it.
func TestAMarkBetweenASettingsSaveAndItsRewriteSendsTheOldStateWhole(t *testing.T) {
	t.Parallel()
	var other string
	page := newRefreshedEvalsPage(t, nil, deployRunsInTheSecondRepository(t, &other))
	project := page.fixture.Project
	list := evalsTarget(t, project, page.held, "")
	query := list[strings.Index(list, "?"):]
	detail := "/api/v1/projects/" + project + "/evals/" + evals.SlugDeployFrequency + query
	items := "/api/v1/projects/" + project + "/evals/" + evals.SlugDeployFrequency + "/items" + query + "&limit=200"
	before := map[string]json.RawMessage{"list": page.route(t), "detail": getBody(t, page.rig.handler, detail), "items": getBody(t, page.rig.handler, items)}
	page.moveDeployRepoAndRule(t, other)
	for what, target := range map[string]string{"list": list, "detail": detail, "items": items} {
		if got, want := withoutFreshness(t, getBody(t, page.rig.handler, target)), withoutFreshness(t, before[what]); !reflect.DeepEqual(got, want) {
			t.Errorf("after the save, before the rewrite, GET %s answers otherwise than before the save:\ngot:  %v\nwant: %v", target, got, want)
		}
	}
	page.rig.hub.catchUpNow()
	page.rig.clock.Advance(viewMaxWait)
	page.rig.requireNothingSent(t, page.conn, "the settings save alone, before the refresher has rewritten the days it moves")

	logged := evals.EvalID(project, "pr-size")
	written := evalRequest(t, page.rig.handler, http.MethodPost, "/api/v1/projects/"+project+"/evals/pr-size/readings",
		map[string]any{"readings": []any{reading("lines", 120, page.now.Add(-time.Hour))}}, "between-save-and-rewrite")
	if written.Code != http.StatusCreated {
		t.Fatalf("log the reading: status %d, body %s", written.Code, written.Body.String())
	}
	page.rig.hub.catchUpNow()
	page.rig.clock.Advance(viewMaxWait)
	between := page.conn.delta(t, "e")
	if got := liveUpsertIDs(t, between, "items"); !slices.Equal(got, []string{logged}) {
		t.Errorf("the reading's delta, before the rewrite, upserts %v, want %s alone", got, logged)
	}
	page.held = applyLiveDelta(t, evalsView, page.held, between)
	held, was := listItemsByID(t, page.held), listItemsByID(t, before["list"])
	if _, found := held[logged]; !found {
		t.Errorf("the page holds no item %s after the reading's delta", logged)
	}
	delete(held, logged)
	if !reflect.DeepEqual(held, was) {
		t.Errorf("after the reading's delta, before the rewrite, the page's evals other than %s are not those it held before the save:\nheld:   %v\nbefore: %v", logged, held, was)
	}
	page.requireAtRoute(t, "the reading's delta before the rewrite")

	page.refresh(t)
	page.rig.hub.catchUpNow()
	after := page.conn.delta(t, "e")
	page.held = applyLiveDelta(t, evalsView, page.held, after)
	page.requireAtRoute(t, "the rewrite's delta")
	moved := listItemsByID(t, page.held)
	delete(moved, logged)
	if reflect.DeepEqual(moved, was) {
		t.Fatal("the route answers after the rewrite what it answered before the save, and the check needs a save that moves it")
	}
	for what, target := range map[string]string{"detail": detail, "items": items} {
		if reflect.DeepEqual(withoutFreshness(t, getBody(t, page.rig.handler, target)), withoutFreshness(t, before[what])) {
			t.Errorf("after the rewrite GET %s answers what it answered before the save, and the check needs a save that moves it", target)
		}
	}
	page.rig.clock.Advance(viewMaxWait)
	page.rig.requireNothingSent(t, page.conn, "the rewrite's one delta, past every quiet")
	t.Logf("the reading's delta before the rewrite upserted %v alone; the rewrite's delta upserted %v and landed on the route's body",
		liveUpsertIDs(t, between, "items"), liveUpsertIDs(t, after, "items"))
}

// TestTheDeliveryPagesAnswerTheRecordedSettingsUntilTheRefresherCommits: the timeline, the measures
// and the Pipeline answer under the rule recorded with each repository's stored day rows
// (readDeliveryRules, evals.BuiltSettingsIn), as the Evals tab does, so one page shows one state.
// Over the refreshed history, with a timeline and a measures view of the last fourteen days and a
// Pipeline view at the page's moment open, one save of the main project's settings moves its
// source's production rule to a job no run holds. Until the refresher's pass commits the rewrite
// of what the save moved, each GET route answers what it answered before the save, and the open
// views, which the change of the settings marks, are sent nothing. The rewrite's commit marks each
// view at once (its eval_days changes carry the rewrite's mark), which is recomputed once and sent
// one delta that lands on its GET route's body: the new rule's, under which no run is a deploy.
// Nothing follows it. Before, the routes and the views read the saved settings, and answered the
// new rule from the save on, beside an Evals tab that still answered the old one.
func TestTheDeliveryPagesAnswerTheRecordedSettingsUntilTheRefresherCommits(t *testing.T) {
	t.Parallel()
	page := newRefreshedEvalsPage(t, nil, nil)
	rig := page.rig
	today := measures.Midnight(page.now)
	from, to := today.AddDate(0, 0, -13), today.AddDate(0, 0, 1)
	frames := map[string]DeliveryLiveView{
		"t": timelineRange(from, to, ""), "m": measuresRange(from, to, ""), "p": livePipelineView(&page.now),
	}
	kinds := map[string]string{"t": deliveryLiveTimeline, "m": deliveryLiveMeasures, "p": deliveryLivePipeline}
	queries := map[string]string{"p": "at=" + page.now.Format(time.RFC3339Nano)}
	subs := []string{"t", "m", "p"}
	conn := newFakeLiveConn()
	for _, sub := range subs {
		rig.hub.Subscribe(conn, subscribeFrame(sub, frames[sub]))
	}
	held := map[string]json.RawMessage{}
	for sub, snapshot := range conn.firstSnapshots(t, subs...) {
		held[sub] = snapshot.Body
	}
	rig.hub.catchUpNow()
	rig.clock.Advance(viewMaxWait)
	rig.requireNothingSent(t, conn, "the views' first answers")
	route := func(sub string) json.RawMessage {
		t.Helper()
		return getBody(t, rig.handler, windowTarget(t, kinds[sub], held[sub], queries[sub]))
	}
	before := map[string]json.RawMessage{}
	for _, sub := range subs {
		before[sub] = route(sub)
		requireSameBody(t, "the "+kinds[sub]+" view's first answer", held[sub], before[sub])
	}

	settings := page.fixture.Settings
	settings.ProductionJobNames = []string{"release / a job no run holds"}
	saveProjectRules(t, rig.database, page.fixture.Project, settings)
	for _, sub := range subs {
		if got, want := withoutComputedAt(t, route(sub)), withoutComputedAt(t, before[sub]); !reflect.DeepEqual(got, want) {
			t.Errorf("after the save, before the refresher's commit, GET %s answers otherwise than before the save:\ngot:  %v\nwant: %v", kinds[sub], got, want)
		}
	}
	rig.hub.catchUpNow()
	rig.clock.Advance(viewMaxWait)
	rig.requireNothingSent(t, conn, "the settings save alone, before the refresher has rewritten the days it moves")

	mark := rig.mark()
	page.refresh(t)
	rig.hub.catchUpNow()
	for owed := maps.Clone(kinds); len(owed) > 0; {
		frame := conn.next(t, "a delta of each delivery view")
		delta, ok := frame.(DeliveryLiveDelta)
		if !ok || owed[delta.Sub] == "" {
			raw, _ := json.Marshal(frame)
			t.Fatalf("frame = %s, want a delta of one of the views still owed one, %v", raw, owed)
		}
		delete(owed, delta.Sub)
		held[delta.Sub] = requireLandsOnGET(t, rig.handler, kinds[delta.Sub], held[delta.Sub], delta, queries[delta.Sub])
		if reflect.DeepEqual(withoutComputedAt(t, held[delta.Sub]), withoutComputedAt(t, before[delta.Sub])) {
			t.Errorf("the %s view answers after the rewrite what it answered before the save, and the check needs a save that moves it", kinds[delta.Sub])
		}
	}
	rig.clock.Advance(viewMaxWait)
	rig.requireNothingSent(t, conn, "the rewrite's one delta of each view, past every quiet")
	for _, sub := range subs {
		spec, _ := parseDeliveryLiveView(frames[sub])
		recomputes := rig.count(mark, func(note deliveryHubNote) bool { return note.what == "recompute begin" && note.view == spec.key })
		rewrites := rig.count(mark, func(note deliveryHubNote) bool {
			return note.what == "recompute begin" && note.view == spec.key && note.cause == deliveryCauseSettingsRewrite
		})
		if recomputes != 1 || rewrites != 1 {
			t.Errorf("the rewrite began %d recomputes of the %s view, %d of them %s, want the one", recomputes, kinds[sub], rewrites, deliveryCauseSettingsRewrite)
		}
	}
}

// TestALiveDeliveryPageFollowsASaveOfARecordedFieldThatMovesNoDayRow: a save of a field of a
// source's rule the refresher records with its repository's day rows (eval_days_rules,
// evals.BuiltSettingsIn) that moves no day row and not the source's repository is recorded by the
// refresher's commit with no change of its own, while the GET routes read the new record from that
// commit on. Over the refreshed history, with both staging cards of the main project's source
// first moved to job names no run holds, and a timeline, a measures and a Pipeline view open, each
// save below of a recorded field (a production job name no run holds beside the one that is, each
// staging card renamed between two names no run holds, and the production apply step moved
// between two steps no production job runs) is first answered by the hub, whose recompute for the
// change of the settings reads the old record, as where the refresher's pass takes longer than
// the quiet, and then followed by the refresher's passes; once the hub owes nothing each view's
// body, with every delta it was sent applied, must be its GET route's: a field whose record a live
// view never hears of fails here, naming what differs. Each save logs how many eval_days changes
// its passes logged.
func TestALiveDeliveryPageFollowsASaveOfARecordedFieldThatMovesNoDayRow(t *testing.T) {
	t.Parallel()
	page := newRefreshedEvalsPage(t, nil, nil)
	rig := page.rig
	// current is the rule of the main project's one source, which each save changes and saves
	// whole (saveProjectRules).
	current := page.fixture.Settings
	// save saves the change, lets the hub answer the change of the settings first, as it does
	// where the refresher's pass takes longer than the quiet, and then runs the refresher's passes.
	save := func(what string, change func(*delivery.DeliverySettings)) {
		t.Helper()
		change(&current)
		saveProjectRules(t, rig.database, page.fixture.Project, current)
		rig.hub.catchUpNow()
		rig.clock.Advance(viewMaxWait)
		rig.settle(t)
		page.refresh(t)
		rig.hub.catchUpNow()
		rig.clock.Advance(viewMaxWait)
	}
	save("both staging cards on names no run holds", func(s *delivery.DeliverySettings) {
		s.StagingE2EJobName, s.StagingApplyJobName = new("staging-e2e / a job no run holds"), new("staging-release / a job no run holds")
	})

	today := measures.Midnight(page.now)
	from, to := today.AddDate(0, 0, -13), today.AddDate(0, 0, 1)
	frames := map[string]DeliveryLiveView{
		"t": timelineRange(from, to, ""), "m": measuresRange(from, to, ""), "p": livePipelineView(&page.now),
	}
	kinds := map[string]string{"t": deliveryLiveTimeline, "m": deliveryLiveMeasures, "p": deliveryLivePipeline}
	queries := map[string]string{"p": "at=" + page.now.Format(time.RFC3339Nano)}
	subs := []string{"t", "m", "p"}
	conn := newFakeLiveConn()
	for _, sub := range subs {
		rig.hub.Subscribe(conn, subscribeFrame(sub, frames[sub]))
	}
	held := map[string]json.RawMessage{}
	for sub, snapshot := range conn.firstSnapshots(t, subs...) {
		held[sub] = snapshot.Body
	}
	rig.hub.catchUpNow()
	rig.clock.Advance(viewMaxWait)
	rig.requireNothingSent(t, conn, "the views' first answers")

	// differing names each field of want that got holds otherwise, with both values: a field that
	// is an object on both sides by each of its own fields that differs.
	var differing func(prefix string, got, want map[string]any) string
	differing = func(prefix string, got, want map[string]any) string {
		var parts []string
		for _, key := range slices.Sorted(maps.Keys(want)) {
			if reflect.DeepEqual(got[key], want[key]) {
				continue
			}
			gotObject, gotIsObject := got[key].(map[string]any)
			wantObject, wantIsObject := want[key].(map[string]any)
			if gotIsObject && wantIsObject {
				parts = append(parts, differing(prefix+key+".", gotObject, wantObject))
				continue
			}
			live, _ := json.Marshal(got[key])
			route, _ := json.Marshal(want[key])
			parts = append(parts, fmt.Sprintf("%s%s: live %.300s, GET %.300s", prefix, key, live, route))
		}
		return strings.Join(parts, "; ")
	}
	// take applies every delta the hub queued, once it owes nothing, to the bodies the page holds.
	take := func() {
		rig.settle(t)
		for {
			select {
			case sent := <-conn.frames:
				if delta, ok := sent.frame.(DeliveryLiveDelta); ok {
					held[delta.Sub] = applyLiveDelta(t, kinds[delta.Sub], held[delta.Sub], delta)
				}
			default:
				return
			}
		}
	}
	// The apply step's case first moves it to a step no production job runs, which judges every
	// run alike, before its save between two such steps.
	for _, c := range []struct {
		field         string
		setup, change func(*delivery.DeliverySettings)
	}{
		{"production_job_names", nil, func(s *delivery.DeliverySettings) {
			s.ProductionJobNames = append(slices.Clone(s.ProductionJobNames), "release / a name no run holds")
		}},
		{"staging_e2e_job_name", nil, func(s *delivery.DeliverySettings) {
			s.StagingE2EJobName = new("staging-e2e / another job no run holds")
		}},
		{"staging_apply_job_name", nil, func(s *delivery.DeliverySettings) {
			s.StagingApplyJobName = new("staging-release / another job no run holds")
		}},
		{"production_apply_step_name", func(s *delivery.DeliverySettings) {
			s.ProductionApplyStepName = new("a step no production job runs")
		}, func(s *delivery.DeliverySettings) {
			s.ProductionApplyStepName = new("another step no production job runs")
		}},
	} {
		if c.setup != nil {
			save(c.field+"'s first value", c.setup)
			take()
		}
		before := latestChange(t, rig.database)
		save(c.field, c.change)
		rig.settle(t)
		logged, err := delivery.ChangesSince(t.Context(), rig.database.Pool, before, 100000)
		if err != nil {
			t.Fatalf("%s: read the changes the save and the passes logged: %v", c.field, err)
		}
		days := 0
		for _, change := range logged {
			if change.Kind == delivery.ChangeKindEvalDays {
				days++
			}
		}
		take()
		for _, sub := range subs {
			route := getBody(t, rig.handler, windowTarget(t, kinds[sub], held[sub], queries[sub]))
			got, want := liveComparable(t, mustEncodeLive(t, kinds[sub], held[sub])), liveComparable(t, mustEncodeLive(t, kinds[sub], route))
			if diff := differing("", got, want); diff != "" {
				t.Errorf("after a save of %s and the refresher's passes (%d eval_days changes logged), the live %s view does not hold its GET route's body: %s",
					c.field, days, kinds[sub], diff)
			}
		}
		t.Logf("a save of %s: the passes logged %d eval_days changes", c.field, days)
	}
}

// figureScopeReads is a tracer that calls before ahead of each read of how far a project's stored
// figures are folded (readFigureScope's statement), the first statement of every fold of them, and
// of no other statement: the read of how far they are built (figuresThrough) reads the same table
// by a list of projects.
type figureScopeReads struct {
	before func()
}

func (f *figureScopeReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if f.before != nil && strings.Contains(data.SQL, "from eval_figures_built where project_key = $2") {
		f.before()
	}
	return ctx
}

func (f *figureScopeReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestAWalkBatchsFoldAfterARecomputeLeavesTheEvalsPageAtTheRoute: over the history with its pull
// requests' attribution inputs left unread, so the issue each row names is its attribution
// (leaveAttributionInputsUnread), the delivery walk begins again over inputs no change names (the
// main project's pull requests moved to the second project, their changes taken out of the log,
// and the walk's record set to begin again, as a page's commit sets it), and the walk rewrites the
// main project's days a batch a pass, each day a commit of its own,
// and folds the stored figures of a batch's days in one more commit, after the last of them. Ahead
// of every fold the hub catches up with the day commits, its clock is moved past their quiet, and
// the page's view is recomputed: it reads the days as the batch left them and the stored figures
// as they stood before it. The fold's commit logs a change of each eval whose figures it moved, so
// the view is marked again, and once the clock has passed that mark's quiet the page holds the
// route's body. Before, the fold logged nothing, and the page kept the figures of before the last
// batch.
func TestAWalkBatchsFoldAfterARecomputeLeavesTheEvalsPageAtTheRoute(t *testing.T) {
	t.Parallel()
	reads := &figureScopeReads{}
	page := newRefreshedEvalsPage(t, reads, func(database *store.Store, _ deliverytest.Fixture) {
		leaveAttributionInputsUnread(t, database.Pool)
	})
	before := page.route(t)
	ctx := t.Context()
	pool := page.rig.database.Pool
	second := page.fixture.SecondProject
	var head int64
	if err := pool.QueryRow(ctx, `select coalesce(max(seq), 0) from delivery_changes`).Scan(&head); err != nil {
		t.Fatalf("read the change log's head: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, rank, created_by)
		values ($1, $2, 900, 'Work moved here', 'in_progress', '00000900', '{"kind":"user","id":"alice"}')
	`, second+"-900", second); err != nil {
		t.Fatalf("make an issue of %s: %v", second, err)
	}
	if _, err := pool.Exec(ctx, `
		update delivery_pull_requests set issue_key = $2 where repo = $1 and issue_key like $3
	`, page.fixture.DeployRepo, second+"-900", page.fixture.Project+"-%"); err != nil {
		t.Fatalf("move %s's pull requests to %s: %v", page.fixture.Project, second, err)
	}
	if _, err := pool.Exec(ctx, `delete from delivery_changes where seq > $1`, head); err != nil {
		t.Fatalf("take the move's changes out of the log: %v", err)
	}
	if _, err := pool.Exec(ctx, `update eval_days_walks set next_day = null where family = 'delivery'`); err != nil {
		t.Fatalf("begin the delivery walk again: %v", err)
	}
	folds := 0
	reads.before = func() {
		folds++
		page.rig.hub.catchUpNow()
		page.rig.clock.Advance(viewMaxWait)
		page.idle(t)
	}
	page.refresh(t)
	reads.before = nil
	if folds == 0 {
		t.Fatal("the walk traced no fold of the stored figures, so no recompute ran ahead of one, and the check needs the tracer to meet each fold")
	}
	page.rig.hub.catchUpNow()
	page.rig.clock.Advance(viewMaxWait)
	page.idle(t)
	deltas := page.take(t)
	if reflect.DeepEqual(liveComparable(t, mustEncodeLive(t, evalsView, before)), liveComparable(t, mustEncodeLive(t, evalsView, page.route(t)))) {
		t.Fatal("the route answers after the pull requests moved what it answered before, and the check needs a walk that moves it")
	}
	page.requireAtRoute(t, "after the walk's last fold")
	t.Logf("the walk begun again folded its batches' figures behind %d recomputes of the page's view; the page took %d deltas and holds the route's body", folds, deltas)
}

// publishDeployRepo writes the record a refresher's page leaves of a repository whose rows it
// brought to its source's rule (eval_days_rules, evals.recordRules): rule.DeployRepo's rows of the
// evals that read the deploy repository stand built under rule for project, kept from since. A
// read of those evals folds the rows of the repositories that record names alone
// (evals.DeployReposIn), the one state the refresher published, so a test that writes such rows by
// hand, with no refresher's pass, writes the record they stand under too. It writes no change row:
// the record moves in the commit of the rows it describes, which logs theirs.
func publishDeployRepo(t testing.TB, database *store.Store, project string, rule delivery.DeliverySettings, since time.Time) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
		insert into eval_days_rules (repo, project_key, production_job_names, production_apply_step_name,
			staging_e2e_job_name, staging_apply_job_name, kept_since)
		values ($1, $2, $3, $4, $5, $6, $7::date)
		on conflict (repo) do update set project_key = excluded.project_key, production_job_names = excluded.production_job_names,
			production_apply_step_name = excluded.production_apply_step_name, staging_e2e_job_name = excluded.staging_e2e_job_name,
			staging_apply_job_name = excluded.staging_apply_job_name, kept_since = excluded.kept_since, updated_at = now()
	`, rule.DeployRepo, project, rule.ProductionJobNames, rule.ProductionApplyStepName, rule.StagingE2EJobName,
		rule.StagingApplyJobName, since.Format(time.DateOnly)); err != nil {
		t.Fatalf("publish the rule %s's rows stand built under: %v", rule.DeployRepo, err)
	}
}
