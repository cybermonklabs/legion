package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// sameVerdict reports whether two verdicts are both null or both the same.
func sameVerdict(a, b *bool) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// showVerdict is a verdict as a test message states it.
func showVerdict(met *bool) string {
	if met == nil {
		return "null"
	}
	if *met {
		return "met"
	}
	return "missed"
}

// catalogueTargets is the targets a delivery answer states where the catalogue's evals judge it,
// as with the deploy repository mapped to no project: main's values and inclusiveness
// (measures.DefaultTargets), each in its eval's direction and judging the statistics the
// catalogue's target_judges name, in that order.
func catalogueTargets() measures.Targets {
	main := measures.DefaultTargets
	of := func(target *measures.Target, direction string, judges ...string) *measures.Target {
		return &measures.Target{Value: target.Value, Inclusive: target.Inclusive, Direction: direction, Judges: judges}
	}
	return measures.Targets{
		DeploysPerDay:              of(main.DeploysPerDay, evals.DirectionHigherBetter, measures.SeriesDeploys+".per_day"),
		ChangeFailureRate:          of(main.ChangeFailureRate, evals.DirectionLowerBetter, measures.SeriesPerDeploy+".rate"),
		MergeToProductionMinutes:   of(main.MergeToProductionMinutes, evals.DirectionLowerBetter, measures.SeriesMinutes+".max", measures.SeriesMinutes+".median"),
		OpenedToMergeMedianMinutes: of(main.OpenedToMergeMedianMinutes, evals.DirectionLowerBetter, measures.SeriesMinutes+".median"),
		DeployRunSuccessRate:       of(main.DeployRunSuccessRate, evals.DirectionHigherBetter, measures.SeriesRuns+".rate"),
		UnownedP0:                  of(main.UnownedP0, evals.DirectionLowerBetter, evals.SeriesUnownedP0+".value"),
	}
}

// showTargets is a delivery answer's targets as a test message states them: as the answer's JSON
// writes them, each target's fields rather than its pointer.
func showTargets(targets measures.Targets) string {
	shown, _ := json.Marshal(targets)
	return string(shown)
}

// TestTheDeliveryVerdictsAreTheirEvalsVerdicts (R5): the measures answer and the Pipeline judge
// their figures by the targets of the delivery evals of the project whose published default
// repository is the deploy repository (which a person sets and the evals refresher's page commit
// publishes), so after register writes that move three targets onto figures of a day-aligned
// window of seven days, inclusive or not, judge pr-open-to-merge by its p90 alone,
// and turn deploy-frequency's off, each verdict of the measures answer and of the Pipeline's
// lead-time cards is the verdict the eval route states for the same window, and each target the
// answer states is the eval's: value, inclusiveness, direction and the statistics its verdict
// judges, so the page names the p90 under the card whose figure is the median by default. The
// target turned off is null and judges nothing. The Pipeline reads at alone, so it is read at the
// window's last second, whose seven days are the window's. unowned-p0 is left out of the
// comparison: its eval reads the project's stored count of each day, where the measures answer
// counts every project's open P0 issues now. With the deploy repository no project's default, both
// answers are judged by the catalogue's targets, main's (catalogueTargets).
func TestTheDeliveryVerdictsAreTheirEvalsVerdicts(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture, _ := seedEvalHistories(t, database, deps, 14)
	project := fixture.Project
	from, to := fixture.From.AddDate(0, 0, 4), fixture.From.AddDate(0, 0, 11)
	window := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
	pipelineAt := url.Values{"at": {to.Add(-time.Second).Format(time.RFC3339)}}.Encode()
	list := "/api/v1/projects/" + project + "/evals?limit=200&" + window
	before := evalGet[evalListResponse](t, handler, list)
	figure := func(slug, series, name string) float64 {
		t.Helper()
		value := seriesValues(t, findEval(t, before.Items, project, slug), series).Window[name]
		if value == nil {
			t.Fatalf("%s's %s.%s has no value over %s, so no target can be set on it", slug, series, name, window)
		}
		return *value
	}
	median, p90 := figure(evals.SlugPROpenToMerge, measures.SeriesMinutes, "median"), figure(evals.SlugPROpenToMerge, measures.SeriesMinutes, "p90")
	if median >= p90 {
		t.Fatalf("pr-open-to-merge's median %v is not under its p90 %v over %s, so no target tells a judge of the p90 from one of the median", median, p90, window)
	}
	for index, write := range []map[string]any{
		{"slug": evals.SlugMergeToProduction, "target": figure(evals.SlugMergeToProduction, measures.SeriesMinutes, "max"), "target_inclusive": true},
		// Judged by its p90 alone, between the median and the p90: the p90 misses it where the
		// median, the statistic the catalogue judges, would meet it.
		{"slug": evals.SlugPROpenToMerge, "target": (median + p90) / 2, "target_inclusive": true,
			"target_judges": []string{measures.SeriesMinutes + ".p90"}},
		{"slug": evals.SlugDeployRunSuccess, "target": figure(evals.SlugDeployRunSuccess, measures.SeriesRuns, "rate"), "target_inclusive": false},
		{"slug": evals.SlugDeployFrequency, "target": nil},
	} {
		written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/evals", write, "target-"+string(rune('a'+index)))
		if written.Code != http.StatusCreated {
			t.Fatalf("register %v: status %d, body %s", write, written.Code, written.Body.String())
		}
	}

	after := evalGet[evalListResponse](t, handler, list)
	answer := getDeliveryMeasures(t, handler, window)
	pipeline := evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?"+pipelineAt)
	if want := measures.ShownDays(to.Add(-time.Second)); !slices.Equal(pipeline.Days, want) || want[0] != from.Format(time.DateOnly) {
		t.Fatalf("the Pipeline at %s shows the days %v, want the window's seven, %v from %s", pipelineAt, pipeline.Days, want, from.Format(time.DateOnly))
	}
	eval := func(slug string) (*bool, *measures.Target) {
		item := findEval(t, after.Items, project, slug)
		if item.Definition.Target == nil || item.Verdict == nil {
			return nil, nil
		}
		judges := []string{}
		for _, judged := range item.Verdict.Judges {
			judges = append(judges, judged.Judge)
		}
		return item.Verdict.Met, &measures.Target{
			Value: *item.Definition.Target, Inclusive: item.Definition.TargetInclusive, Direction: item.Definition.Direction, Judges: judges,
		}
	}
	compared := 0
	for _, check := range []struct {
		what   string
		slug   string
		status *bool
		target *measures.Target
	}{
		{"measures deploys_per_day", evals.SlugDeployFrequency, answer.Status.DeploysPerDay, answer.Targets.DeploysPerDay},
		{"measures change_failure_rate", evals.SlugChangeFailRate, answer.Status.ChangeFailureRate, answer.Targets.ChangeFailureRate},
		{"measures merge_to_production", evals.SlugMergeToProduction, answer.Status.MergeToProduction, answer.Targets.MergeToProductionMinutes},
		{"measures opened_to_merge", evals.SlugPROpenToMerge, answer.Status.OpenedToMerge, answer.Targets.OpenedToMergeMedianMinutes},
		{"measures deploy_run_success", evals.SlugDeployRunSuccess, answer.Status.DeployRunSuccess, answer.Targets.DeployRunSuccessRate},
		{"pipeline merge_to_production", evals.SlugMergeToProduction, pipeline.Status.MergeToProduction, pipeline.Targets.MergeToProductionMinutes},
		{"pipeline opened_to_merge", evals.SlugPROpenToMerge, pipeline.Status.OpenedToMerge, pipeline.Targets.OpenedToMergeMedianMinutes},
	} {
		met, target := eval(check.slug)
		if !sameVerdict(check.status, met) {
			t.Errorf("%s is %s, and the eval route's verdict of %s over the same window is %s", check.what, showVerdict(check.status), check.slug, showVerdict(met))
		}
		if !reflect.DeepEqual(check.target, target) {
			t.Errorf("%s's target is %+v, and %s's is %+v", check.what, check.target, check.slug, target)
		}
		compared++
		t.Logf("%s: %s, target %+v, as %s", check.what, showVerdict(check.status), check.target, check.slug)
	}
	if met, _ := eval(evals.SlugMergeToProduction); met == nil || !*met {
		t.Errorf("merge-to-production's verdict at an inclusive target equal to the window's longest change is %s, want met", showVerdict(met))
	}
	wantOpened := &measures.Target{Value: (median + p90) / 2, Inclusive: true, Direction: evals.DirectionLowerBetter, Judges: []string{measures.SeriesMinutes + ".p90"}}
	for _, opened := range []struct {
		what    string
		target  *measures.Target
		verdict *bool
	}{
		{"the measures answer", answer.Targets.OpenedToMergeMedianMinutes, answer.Status.OpenedToMerge},
		{"the Pipeline", pipeline.Targets.OpenedToMergeMedianMinutes, pipeline.Status.OpenedToMerge},
	} {
		if !reflect.DeepEqual(opened.target, wantOpened) {
			t.Errorf("%s states pr-open-to-merge's target %+v, want %+v: the p90 the write named, lower better", opened.what, opened.target, wantOpened)
		}
		if opened.verdict == nil || *opened.verdict {
			t.Errorf("%s judges pr-open-to-merge %s at a target between the median %v and the p90 %v, want missed, the p90's verdict",
				opened.what, showVerdict(opened.verdict), median, p90)
		}
	}
	if answer.Targets.DeploysPerDay != nil || answer.Status.DeploysPerDay != nil {
		t.Errorf("deploy-frequency's target is off, and the measures answer states the target %+v and the verdict %s, want null and null",
			answer.Targets.DeploysPerDay, showVerdict(answer.Status.DeploysPerDay))
	}

	// With the deploy repository no project's default, once the refresher's page commit publishes
	// that (publishDefaultRepos), the catalogue's targets judge both answers.
	if _, err := database.Pool.Exec(t.Context(), `update projects set default_repo = null where key = $1`, project); err != nil {
		t.Fatalf("clear %s's default repository: %v", project, err)
	}
	publishDefaultRepos(t, database.Pool)
	unmapped := getDeliveryMeasures(t, handler, window)
	unmappedPipeline := evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?"+pipelineAt)
	if want := catalogueTargets(); !reflect.DeepEqual(unmapped.Targets, want) || !reflect.DeepEqual(unmappedPipeline.Targets, want) {
		t.Errorf("with the deploy repository mapped to no project the targets are %s and %s, want the catalogue's %s",
			showTargets(unmapped.Targets), showTargets(unmappedPipeline.Targets), showTargets(want))
	}
	if unmapped.Status.DeploysPerDay == nil {
		t.Error("with the deploy repository mapped to no project deploys_per_day has no verdict, want the catalogue's target to judge it")
	}
	t.Logf("%d verdicts and targets compared with the eval route's; unmapped, the targets are the catalogue's", compared)
}

// TestAFigureItsJudgesDoNotNameHasNoVerdict (R5): the measures answer's verdict of the median merge
// to production is that one statistic's verdict against its eval's target, answered only where
// the eval's judges name it and null otherwise, since a target no judge holds the figure to says
// nothing of it; and each eval's verdict is over every statistic its judges name. Over figures
// where the median merge to production meets the catalogue's 45 minutes and the longest change
// misses it, and the per-deploy failure rate meets the catalogue's 5% over 10 deploys: merge to
// production judged by its longest change alone answers no median verdict; the change failure rate
// judged by its rate and its count of deploys answers missed, the count of 10 missing 5%; and the
// catalogue's judges (merge to production's longest change and median, the per-deploy rate)
// answer the median's verdict, met, and the change failure rate's, met.
func TestAFigureItsJudgesDoNotNameHasNoVerdict(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	catalogue, err := evals.New(database, nil, nil, nil).DeployRepoEvals(t.Context(), "acme/widgets", evals.DeliveryTargetSlugs)
	if err != nil {
		t.Fatalf("read the catalogue's delivery evals: %v", err)
	}
	figures := evals.ResultStatistics(measures.Result{
		LeadTime:          measures.LeadTime{MergeToProduction: measures.Spread{N: 3, MedianMinutes: new(30.0), P90Minutes: new(80.0), MaxMinutes: new(90.0)}},
		ChangeFailureRate: &measures.ChangeFailRate{Failed: 0, Total: 10, Rate: new(0.0)},
	}, 0)
	judged := func(slug string, judges ...string) map[string]evals.Entry {
		entries := maps.Clone(catalogue)
		entry := entries[slug]
		entry.Definition.TargetJudges = judges
		entries[slug] = entry
		return entries
	}
	missed, met := new(false), new(true)
	for _, check := range []struct {
		what    string
		entries map[string]evals.Entry
		field   func(measures.Status) *bool
		name    string
		want    *bool
	}{
		{"the catalogue's judges", catalogue, func(status measures.Status) *bool { return status.MergeToProduction }, "merge_to_production", missed},
		{"the catalogue's judges", catalogue, func(status measures.Status) *bool { return status.MergeToProductionMedian }, "merge_to_production_median", met},
		{"the catalogue's judges", catalogue, func(status measures.Status) *bool { return status.ChangeFailureRate }, "change_failure_rate", met},
		{"merge to production judged by minutes.max", judged(evals.SlugMergeToProduction, measures.SeriesMinutes+".max"),
			func(status measures.Status) *bool { return status.MergeToProduction }, "merge_to_production", missed},
		{"merge to production judged by minutes.max", judged(evals.SlugMergeToProduction, measures.SeriesMinutes+".max"),
			func(status measures.Status) *bool { return status.MergeToProductionMedian }, "merge_to_production_median", nil},
		{"the change failure rate judged by per_deploy.rate and per_deploy.total", judged(evals.SlugChangeFailRate, measures.SeriesPerDeploy+".rate", measures.SeriesPerDeploy+".total"),
			func(status measures.Status) *bool { return status.ChangeFailureRate }, "change_failure_rate", missed},
	} {
		if got := check.field(measuresStatus(check.entries, figures)); !sameVerdict(got, check.want) {
			t.Errorf("with %s, status.%s is %s, want %s", check.what, check.name, showVerdict(got), showVerdict(check.want))
		}
	}
}

// TestATargetWriteMovesTheLiveDeliveryVerdicts: a register write of merge-to-production's target
// and judges in the project whose deploy source reads the deploy repository, here through its
// published default repository (which the test publishes as the evals refresher's page commit
// does, publishDefaultRepos), reaches an open measures view and an open Pipeline view: the write's
// version marks both, and each delta lands on its GET route's body, which states the new target,
// its inclusiveness, the eval's direction and the judge the write named. The Pipeline view is
// pinned at the test's start, the at its GET route reads, since that route reads at alone and the
// hub's clock runs on.
func TestATargetWriteMovesTheLiveDeliveryVerdicts(t *testing.T) {
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "HUB")
	if _, err := rig.database.Pool.Exec(t.Context(), `update projects set default_repo = 'acme/widgets' where key = 'HUB'`); err != nil {
		t.Fatalf("make acme/widgets HUB's default repository: %v", err)
	}
	// HUB's one source names no repository, so it reads the default repository, as published.
	saveProjectRules(t, rig.database, "HUB", deployRule("acme/widgets", "release / release"))
	publishDefaultRepos(t, rig.database.Pool)
	now := rig.clock.Now()
	upsertPR(t, rig.database, hubPR(1, now.Add(-3*time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(&now)))
	snapshots := conn.firstSnapshots(t, "m", "p")
	rig.settle(t)

	registered := evalRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/HUB/evals", map[string]any{
		"slug": evals.SlugMergeToProduction, "target": 30, "target_inclusive": true, "target_judges": []string{measures.SeriesMinutes + ".p90"},
	}, "merge-target")
	if registered.Code != http.StatusCreated {
		t.Fatalf("set HUB's merge-to-production target: status %d, body %s", registered.Code, registered.Body.String())
	}
	want := &measures.Target{Value: 30, Inclusive: true, Direction: evals.DirectionLowerBetter, Judges: []string{measures.SeriesMinutes + ".p90"}}
	kinds := map[string]string{"m": deliveryLiveMeasures, "p": deliveryLivePipeline}
	// What each view's GET route reads besides what its body names: nothing for the measures view,
	// whose body names its window, and for the pipeline view, whose body names no at, the at it was
	// pinned at.
	queries := map[string]string{"p": "at=" + now.Format(time.RFC3339Nano)}
	for len(kinds) > 0 {
		frame := conn.next(t, "a delta of each view")
		delta, ok := frame.(DeliveryLiveDelta)
		if !ok || kinds[delta.Sub] == "" {
			raw, _ := json.Marshal(frame)
			t.Fatalf("frame = %s, want a delta of one of the views still owed one, %v", raw, kinds)
		}
		kind := kinds[delta.Sub]
		delete(kinds, delta.Sub)
		body := requireLandsOnGET(t, rig.handler, kind, snapshots[delta.Sub].Body, delta, queries[delta.Sub])
		var shape struct {
			Targets measures.Targets `json:"targets"`
		}
		if err := json.Unmarshal(body, &shape); err != nil {
			t.Fatalf("read the %s body's targets: %v", kind, err)
		}
		if got := shape.Targets.MergeToProductionMinutes; !reflect.DeepEqual(got, want) {
			t.Errorf("the live %s view states merge_to_production_minutes %+v, want %+v", kind, got, want)
		}
	}
}
