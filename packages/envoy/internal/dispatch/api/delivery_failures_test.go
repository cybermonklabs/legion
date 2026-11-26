package api

import (
	"encoding/json"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// failuresDay is the first day of the API's failure measures history, and failuresNow the moment
// its rows are built as of, two days later.
var (
	failuresDay = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	failuresNow = failuresDay.AddDate(0, 0, 2).Add(12 * time.Hour)
)

// failuresAt is hour of the day day days after failuresDay.
func failuresAt(day, hour int) time.Time {
	return failuresDay.AddDate(0, 0, day).Add(time.Duration(hour) * time.Hour)
}

// saveProjectDelivery saves input through delivery.PutProjectSettings, in a transaction of its own
// (deliverytest.SaveSettings).
func saveProjectDelivery(t *testing.T, pool *store.Pool, input delivery.ProjectSettingsInput) {
	t.Helper()
	if _, err := deliverytest.SaveSettings(t.Context(), pool, input, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("save the delivery settings of %s: %v", input.Project, err)
	}
}

// writeProductionEvent writes event, as the record's routes write one.
func writeProductionEvent(t *testing.T, pool *store.Pool, event prodevents.Event) prodevents.Event {
	t.Helper()
	event.Actor = model.Actor{Kind: "agent", ID: "release-bot"}
	written, _, err := prodevents.Write(t.Context(), pool, event)
	if err != nil {
		t.Fatalf("write production event %s: %v", event.SourceRef, err)
	}
	return written
}

// seedFailureHistory writes FAIL, the one delivery-on project, whose delivery settings the
// deployment-wide measures route reads too (readDeliveryRules: a project with delivery on is all it
// needs), whose events source reads the deploys posted to it and which counts posted failures from
// failuresDay: P1 and P2 on the first day, P3 on the second; FA, a posted failure of P1, restored
// at 13:00 by P2, and FB of P3, not restored. It answers the rows by name. No other project has
// delivery on, since the route answers the failure measures of the one delivery-on project alone.
func seedFailureHistory(t *testing.T, database *store.Store) map[string]prodevents.Event {
	t.Helper()
	pool := database.Pool
	if _, err := pool.Exec(t.Context(), `insert into projects (key, name, default_assignee, default_repo)
		values ('FAIL', 'Failures', 'alice', 'acme/fail-app'), ('MORE', 'More', 'alice', 'acme/more-app')`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	since := failuresDay
	saveProjectDelivery(t, pool, delivery.ProjectSettingsInput{
		Project: "FAIL", Delivery: true, Sources: []delivery.SourceInput{{Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
		FailureSources: []string{delivery.FailureSourceEvents}, FailuresMeasuredSince: &since,
	})
	rows := map[string]prodevents.Event{}
	deploy := func(name string, at time.Time) {
		rows[name] = writeProductionEvent(t, pool, prodevents.Event{
			ProjectKey: "FAIL", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: name, FinishedAt: &at,
			URL: "https://deploys.example/" + name,
		})
	}
	deploy("P1", failuresAt(0, 10))
	deploy("P2", failuresAt(0, 15))
	deploy("P3", failuresAt(1, 10))
	failure := func(name, blamed string, detected time.Time, restored *time.Time, by string) {
		event := prodevents.Event{
			ProjectKey: "FAIL", Kind: prodevents.KindFailure, Source: prodevents.SourceAPI, SourceRef: name,
			DeployID: new(rows[blamed].ID), Signal: new(prodevents.SignalAPI), DetectedAt: &detected,
		}
		if restored != nil {
			event.RestoredAt, event.RestoredBy, event.RestoredByDeploy = restored, &model.Actor{Kind: "agent", ID: "release-bot"}, new(rows[by].ID)
		}
		rows[name] = writeProductionEvent(t, pool, event)
	}
	failure("FA", "P1", failuresAt(0, 11), new(failuresAt(0, 13)), "P2")
	failure("FB", "P3", failuresAt(1, 11), nil, "")
	return rows
}

// failureWindowQuery is the query of the window from day from to day to of failuresDay's history.
func failureWindowQuery(from, to int) string {
	return url.Values{"from": {failuresDay.AddDate(0, 0, from).Format(time.RFC3339)}, "to": {failuresDay.AddDate(0, 0, to).Format(time.RFC3339)}}.Encode()
}

// evalStatistics is the window statistics of each eval of FAIL's list read over query, by slug.
func evalStatistics(t *testing.T, handler http.Handler, query string) map[string]evals.SeriesValues {
	t.Helper()
	list := evalGet[evalListResponse](t, handler, "/api/v1/projects/FAIL/evals?limit=200&"+query)
	values := map[string]evals.SeriesValues{}
	for _, item := range list.Items {
		if len(item.Series) > 0 {
			values[item.Definition.Slug] = item.Series[0]
		}
	}
	return values
}

// sameFigure fails unless got, the measures route's figure, is want, the eval route's.
func sameFigure(t *testing.T, name string, got, want *float64) {
	t.Helper()
	switch {
	case (got == nil) != (want == nil):
		t.Errorf("%s: the measures route answers %v, the eval route %v", name, got, want)
	case got != nil && math.Float64bits(*got) != math.Float64bits(*want):
		t.Errorf("%s: the measures route answers %g, the eval route %g", name, *got, *want)
	}
}

// TestTheMeasuresRoutesFailureValuesEqualTheEvalRoutes (k11, decision 3): over the history's two
// days, the old delivery routes answer the three failure measures FAIL's eval routes answer, from
// the same stored rows through the same folds: change fail rate 2 of 3, recovery 180 minutes with
// one failure not restored, rework 1 of 3. Each of the three surfaces that still answer them is
// held to it: the measures route, the `measures` of the timeline route, and the live hub's
// `measures` view, which a subscriber's first answer carries. A window that starts before
// failures_measured_since reads null in all of them and in the evals; and with a second
// delivery-on project the deployment-wide routes and the hub answer null, while FAIL's evals still
// answer.
func TestTheMeasuresRoutesFailureValuesEqualTheEvalRoutes(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedFailureHistory(t, database)
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	refreshEvalDays(t, registry, database, failuresNow)

	// The three surfaces' measures: the measures route's, the timeline route's measures object and
	// the first snapshot a subscriber of the hub's measures view is sent, of one window.
	measuresRoute := func(from, to int) measures.Result {
		t.Helper()
		return getDeliveryMeasures(t, handler, failureWindowQuery(from, to)).Measures
	}
	timelineRoute := func(from, to int) measures.Result {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?"+failureWindowQuery(from, to), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET timeline?%s: status=%d body=%s", failureWindowQuery(from, to), response.Code, response.Body.String())
		}
		return decodeBody[deliveryTimelineWithMeasures](t, response).Measures.Measures
	}
	hubView := func(hub *hubRig, from, to int) measures.Result {
		t.Helper()
		conn := newFakeLiveConn()
		hub.hub.Subscribe(conn, subscribeFrame("m", measuresRange(failuresDay.AddDate(0, 0, from), failuresDay.AddDate(0, 0, to), "")))
		var body deliveryMeasuresResponse
		if err := json.Unmarshal(conn.snapshot(t, "m").Body, &body); err != nil {
			t.Fatalf("decode the hub's measures snapshot: %v", err)
		}
		return body.Measures
	}

	query := failureWindowQuery(0, 2)
	want := evalStatistics(t, handler, query)
	cfr, recovery, rework := want[evals.SlugChangeFailRate].Window, want[evals.SlugFailedDeployRecovery].Window, want[evals.SlugDeploymentReworkRate].Window
	oneProject := hubOn(t, handler, database, deps, newNoonHubClock(), nil)
	for surface, got := range map[string]measures.Result{
		"the measures route": measuresRoute(0, 2), "the timeline route's measures": timelineRoute(0, 2), "the hub's measures view": hubView(oneProject, 0, 2),
	} {
		if got.ChangeFailureRate == nil || got.TimeToRestore == nil || got.DeploymentReworkRate == nil {
			t.Fatalf("%s answers change fail rate %v, recovery %v and rework rate %v, want a value each",
				surface, got.ChangeFailureRate, got.TimeToRestore, got.DeploymentReworkRate)
		}
		sameFigure(t, surface+": change fail rate", got.ChangeFailureRate.Rate, cfr["rate"])
		sameFigure(t, surface+": failed deploys", new(float64(got.ChangeFailureRate.Failed)), cfr["failed"])
		sameFigure(t, surface+": deploys", new(float64(got.ChangeFailureRate.Total)), cfr["total"])
		sameFigure(t, surface+": recovery median", got.TimeToRestore.MedianMinutes, recovery["median"])
		sameFigure(t, surface+": restores timed", new(float64(got.TimeToRestore.N)), recovery["n"])
		sameFigure(t, surface+": failures not restored", new(float64(got.TimeToRestore.NotRestored)), recovery["not_restored"])
		sameFigure(t, surface+": rework rate", got.DeploymentReworkRate.Rate, rework["rate"])
		sameFigure(t, surface+": restoring deploys", new(float64(got.DeploymentReworkRate.Restoring)), rework["restoring"])
		if got.ChangeFailureRate.Failed != 2 || got.ChangeFailureRate.Total != 3 || got.TimeToRestore.MedianMinutes == nil ||
			*got.TimeToRestore.MedianMinutes != 180 || got.TimeToRestore.NotRestored != 1 || got.DeploymentReworkRate.Restoring != 1 {
			t.Errorf("%s answers change fail rate %+v, recovery %+v and rework rate %+v; want 2 of 3, 180 minutes with 1 not restored, 1 of 3",
				surface, *got.ChangeFailureRate, *got.TimeToRestore, *got.DeploymentReworkRate)
		}
	}

	early := failureWindowQuery(-1, 2)
	for surface, before := range map[string]measures.Result{
		"the measures route": measuresRoute(-1, 2), "the timeline route's measures": timelineRoute(-1, 2), "the hub's measures view": hubView(oneProject, -1, 2),
	} {
		if before.ChangeFailureRate != nil || before.TimeToRestore != nil || before.DeploymentReworkRate != nil {
			t.Errorf("a window from the day before failures_measured_since answers %v, %v and %v on %s, want null each",
				before.ChangeFailureRate, before.TimeToRestore, before.DeploymentReworkRate, surface)
		}
	}
	if rate := evalStatistics(t, handler, early)[evals.SlugChangeFailRate].Window["rate"]; rate != nil {
		t.Errorf("a window from the day before failures_measured_since reads change fail rate %g on the eval route, want no data", *rate)
	}

	saveProjectDelivery(t, database.Pool, delivery.ProjectSettingsInput{
		Project: "MORE", Delivery: true, Sources: []delivery.SourceInput{{Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
	})
	// The hub that answers the two-project window is one that starts after the second project's
	// settings are saved, so its first read is of the settings as they stand.
	twoProjects := hubOn(t, handler, database, deps, newNoonHubClock(), nil)
	for surface, two := range map[string]measures.Result{
		"the measures route": measuresRoute(0, 2), "the timeline route's measures": timelineRoute(0, 2), "the hub's measures view": hubView(twoProjects, 0, 2),
	} {
		if two.ChangeFailureRate != nil || two.TimeToRestore != nil || two.DeploymentReworkRate != nil {
			t.Errorf("with two delivery-on projects %s answers %v, %v and %v, want null each",
				surface, two.ChangeFailureRate, two.TimeToRestore, two.DeploymentReworkRate)
		}
	}
	if rate := evalStatistics(t, handler, query)[evals.SlugChangeFailRate].Window["rate"]; rate == nil {
		t.Error("with two delivery-on projects FAIL's change fail rate reads no data, want its value")
	}
}

// TestTheFailureMeasuresItemsAnswerTheirDeploysAndFailures: the items route of each failure
// measure answers the deploys and the failures of the record behind its numbers over the window,
// each with its relation: change fail rate's three deploys (P1 and P3 failed) and the two failures
// that blame them; recovery's FA restored after 180 minutes, FB not restored, and the deploys they
// blame; rework rate's P2 restoring, with FA, the failure it restored.
func TestTheFailureMeasuresItemsAnswerTheirDeploysAndFailures(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	rows := seedFailureHistory(t, database)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, failuresNow)
	type item struct {
		kind, relation string
		minutes        *float64
	}
	read := func(slug string) map[string]item {
		t.Helper()
		answer := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/FAIL/evals/"+slug+"/items?"+failureWindowQuery(0, 2))
		held := map[string]item{}
		for _, row := range answer.Items {
			if row.RowKind != evalRowDeploy && row.RowKind != evalRowFailure {
				continue
			}
			relation, _ := row.Cols["relation"].(string)
			one := item{kind: row.RowKind, relation: relation}
			if minutes, ok := row.Cols["minutes"].(float64); ok {
				one.minutes = &minutes
			}
			for name, event := range rows {
				if event.ID == row.RowKey {
					held[name] = one
				}
			}
		}
		return held
	}
	for slug, want := range map[string]map[string]item{
		evals.SlugChangeFailRate: {
			"P1": {kind: evalRowDeploy, relation: evals.RelationFailed}, "P2": {kind: evalRowDeploy, relation: evals.RelationDeployed},
			"P3": {kind: evalRowDeploy, relation: evals.RelationFailed},
			"FA": {kind: evalRowFailure, relation: evals.RelationCounted}, "FB": {kind: evalRowFailure, relation: evals.RelationCounted},
		},
		evals.SlugFailedDeployRecovery: {
			"P1": {kind: evalRowDeploy, relation: evals.RelationBlamed}, "P3": {kind: evalRowDeploy, relation: evals.RelationBlamed},
			"FA": {kind: evalRowFailure, relation: evals.RelationRestored, minutes: new(180.0)},
			"FB": {kind: evalRowFailure, relation: evals.RelationNotRestored},
		},
		evals.SlugDeploymentReworkRate: {
			"P1": {kind: evalRowDeploy, relation: evals.RelationDeployed}, "P2": {kind: evalRowDeploy, relation: evals.RelationRestoring},
			"P3": {kind: evalRowDeploy, relation: evals.RelationDeployed}, "FA": {kind: evalRowFailure, relation: evals.RelationRestored},
		},
	} {
		got := read(slug)
		if names, wanted := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(names, wanted) {
			t.Errorf("%s's items are %v, want %v", slug, names, wanted)
			continue
		}
		for name, one := range want {
			held := got[name]
			if held.kind != one.kind || held.relation != one.relation || (held.minutes == nil) != (one.minutes == nil) ||
				held.minutes != nil && *held.minutes != *one.minutes {
				t.Errorf("%s's item %s is %+v, want %+v", slug, name, held, one)
			}
		}
	}
}

// TestAFailureMeasuresItemsOfTwoRangesAreEachRangesOwn: the items route of each failure measure,
// read with a compare range, reads the record behind its numbers once for both ranges, and
// answers for each range the deploys and failures, with their relations, that a read of that
// range alone answers: the window's of the second day, and the compare range's of the first.
func TestAFailureMeasuresItemsOfTwoRangesAreEachRangesOwn(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedFailureHistory(t, database)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, failuresNow)
	type item struct {
		key, kind, relation, at string
	}
	read := func(slug, query, label string) []item {
		t.Helper()
		answer := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/FAIL/evals/"+slug+"/items?"+query)
		var items []item
		for _, row := range answer.Items {
			if row.Range == label && (row.RowKind == evalRowDeploy || row.RowKind == evalRowFailure) {
				relation, _ := row.Cols["relation"].(string)
				items = append(items, item{key: row.RowKey, kind: row.RowKind, relation: relation, at: row.MeasuredAt.Format(time.RFC3339Nano)})
			}
		}
		return items
	}
	both := failureWindowQuery(1, 2) + "&" + url.Values{
		"compare_from": {failuresDay.Format(time.RFC3339)}, "compare_to": {failuresDay.AddDate(0, 0, 1).Format(time.RFC3339)},
	}.Encode()
	for _, slug := range []string{evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery, evals.SlugDeploymentReworkRate} {
		for _, one := range []struct{ label, alone string }{{evals.RangeWindow, failureWindowQuery(1, 2)}, {evals.RangeCompare, failureWindowQuery(0, 1)}} {
			got, want := read(slug, both, one.label), read(slug, one.alone, evals.RangeWindow)
			if len(want) == 0 || !slices.Equal(got, want) {
				t.Errorf("%s's %s items read with the other range are %+v, want %+v, those of a read of it alone", slug, one.label, got, want)
			}
		}
	}
}
