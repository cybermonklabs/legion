package evals

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// The failure measures' histories (failure_days.go): failureDay is their first day, three days
// before refresherToday, and deltaApp the default repository of DELTA, a third project.
var failureDay = refresherToday.AddDate(0, 0, -3)

const deltaApp = "acme/delta-app"

// failureAt is the moment hour:minute of the day day days after failureDay.
func failureAt(day, hour, minute int) time.Time {
	return failureDay.AddDate(0, 0, day).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// seedFailureProjects writes ALPHA, OMEGA and DELTA, with the default repositories acme/alpha-app,
// acme/omega-app and acme/delta-app, the issues ALPHA-1 and OMEGA-1, and the pull requests
// acme/alpha-app#1 (ALPHA-1) and acme/alpha-app#2 (OMEGA-1).
func (r *refresherRig) seedFailureProjects(t *testing.T) {
	t.Helper()
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('ALPHA', 'Alpha', 'alice', 2, $1), ('OMEGA', 'Omega', 'alice', 2, $2), ('DELTA', 'Delta', 'alice', 1, $3)`, alphaApp, omegaApp, deltaApp)
	r.exec(t, "the issues", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', $1, '{"kind":"system","id":"failures-test"}', $2),
		       ('OMEGA-1', 'OMEGA', 1, 'Omega work', 'in_progress', '00000001', $3, '{"kind":"system","id":"failures-test"}', $2)`,
		alphaApp, failureDay.AddDate(0, 0, -10), omegaApp)
	for number, issue := range map[int]string{1: "ALPHA-1", 2: "OMEGA-1"} {
		merged := failureDay.AddDate(0, 0, -1)
		if err := delivery.UpsertPullRequest(t.Context(), r.database.Pool, delivery.DeliveryPullRequest{
			Repo: alphaApp, Number: number, Title: fmt.Sprintf("%s: change %d", issue, number),
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", alphaApp, number), Author: "dev-ana",
			CreatedAt: new(merged.Add(-time.Hour)), MergedAt: new(merged), IssueKey: new(issue), Sessions: []string{},
		}); err != nil {
			t.Fatalf("write %s#%d: %v", alphaApp, number, err)
		}
	}
}

// putFailures saves project's delivery settings: delivery on, one GitHub source reading its
// default repository under job, any extra sources, and the failure sources counted from since
// (none where sources is empty).
func (r *refresherRig) putFailures(t *testing.T, project, job string, sources []string, since time.Time, extra ...delivery.SourceInput) {
	t.Helper()
	input := delivery.ProjectSettingsInput{
		Project: project, Delivery: true, Sources: append([]delivery.SourceInput{deliverytest.SourceOf(ruleOfJob(job), nil)}, extra...),
		PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{}, FailureSources: sources,
	}
	if len(sources) > 0 {
		input.FailuresMeasuredSince = &since
	}
	putProjectSettings(t, r.database.Pool, input)
}

// githubDeploy writes the GitHub adapter's deploy row ref of project's repo, finished at finished,
// shipping prs, with status ("" for none).
func (r *refresherRig) githubDeploy(t *testing.T, project, repo, ref string, finished time.Time, status string, prs ...string) prodevents.Event {
	t.Helper()
	event, _, err := prodevents.Write(t.Context(), r.database.Pool, prodevents.Event{
		ProjectKey: project, Kind: prodevents.KindDeploy, Source: prodevents.SourceGitHub, SourceRef: ref, Status: status,
		StartedAt: new(finished.Add(-20 * time.Minute)), FinishedAt: &finished, Repo: repo, PullRequests: prs, Actor: alice,
	})
	if err != nil {
		t.Fatalf("write the deploy %s of %s: %v", ref, project, err)
	}
	return event
}

// failure writes a failure row of project under ref, of signal (from source), blaming deploy,
// detected at detected; restored at restored by restoring where restored is set.
func (r *refresherRig) failure(t *testing.T, project, ref, source, signal string, deploy prodevents.Event, detected time.Time,
	restored *time.Time, restoring *prodevents.Event) prodevents.Event {
	t.Helper()
	event := prodevents.Event{
		ProjectKey: project, Kind: prodevents.KindFailure, Source: source, SourceRef: ref, DeployID: &deploy.ID,
		Signal: &signal, DetectedAt: &detected, Actor: alice, RestoredAt: restored,
	}
	if restored != nil {
		event.RestoredBy = &alice
		if restoring != nil {
			event.RestoredByDeploy = &restoring.ID
		}
	}
	written, _, err := prodevents.Write(t.Context(), r.database.Pool, event)
	if err != nil {
		t.Fatalf("write the failure %s of %s: %v", ref, project, err)
	}
	return written
}

// failureValues is project's three failure measures over window as an eval read states them: the
// stored rows folded with each series' first day (FirstDaysIn, DayWindowIn, SeriesValuesOf), each
// eval's one series by its slug.
func (r *refresherRig) failureValues(t *testing.T, project string, window Range) map[string]SeriesValues {
	t.Helper()
	ctx := t.Context()
	page, err := r.registry.Current(ctx, []string{project}, "", 100, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", project, err)
	}
	entries := slices.DeleteFunc(page.Items, func(entry Entry) bool { return !isFailureMeasure(entry.Definition) })
	if len(entries) != len(failureDaySlugs) {
		t.Fatalf("%s holds %d failure measures, want %d", project, len(entries), len(failureDaySlugs))
	}
	firsts, err := FirstDaysIn(ctx, r.database.Pool, entries, nil)
	if err != nil {
		t.Fatalf("read the first days of %s's failure measures: %v", project, err)
	}
	rows, err := DayWindowIn(ctx, r.database.Pool, []string{project}, entries, nil, nil, window, nil)
	if err != nil {
		t.Fatalf("read %s's failure measures: %v", project, err)
	}
	values := map[string]SeriesValues{}
	for _, entry := range entries {
		own := slices.DeleteFunc(slices.Clone(rows), func(row WindowRow) bool { return row.EvalID != entry.Definition.EvalID })
		values[entry.Definition.Slug] = SeriesValuesOf(entry, own, firsts[entry.Definition.EvalID], window, nil)[0]
	}
	return values
}

// wantFigures fails unless each statistic of want (`<slug>.<statistic>`) holds that value in
// values, nil meaning "no data".
func wantFigures(t *testing.T, what string, values map[string]SeriesValues, want map[string]*float64) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(want)) {
		slug, name, _ := strings.Cut(key, ".")
		value, stated := values[slug].Window[name]
		expected := want[key]
		switch {
		case !stated:
			t.Errorf("%s: %s states no %s", what, slug, name)
		case (value == nil) != (expected == nil):
			t.Errorf("%s: %s %s is %s, want %s", what, slug, name, showValue(value), showValue(expected))
		case value != nil && math.Abs(*value-*expected) > 1e-9:
			t.Errorf("%s: %s %s is %g, want %g", what, slug, name, *value, *expected)
		}
	}
}

// failureWindow is the record of TestAWindowOfDeploysAndFailuresFoldsToTheDORAValuesThroughTheStoredRows,
// ALPHA's from failureDay on, every source on: GitHub deploys of acme/alpha-app shipping no pull
// request, D1 and D2 on the first day, D3 and D4 on the second, D5 on the third, and D6 on the
// second, which the adapter dropped; F1 (a failing check) on D1, restored at 12:30 by D2; F2 (a
// post) and F3 (a person's flag) on D3, F3 restored at 09:00 of the third day by D5; and F4, a
// counted failing check on the dropped D6, restored by D4.
type failureWindow struct {
	deploys  map[string]prodevents.Event
	failures map[string]prodevents.Event
}

func (r *refresherRig) seedFailureWindow(t *testing.T) failureWindow {
	t.Helper()
	r.seedFailureProjects(t)
	r.putFailures(t, "ALPHA", alphaJob, delivery.FailureSources, failureDay)
	deploy := func(ref string, at time.Time, status string) prodevents.Event {
		return r.githubDeploy(t, "ALPHA", alphaApp, ref, at, status)
	}
	w := failureWindow{deploys: map[string]prodevents.Event{
		"D1": deploy("github:1", failureAt(0, 10, 0), ""), "D2": deploy("github:2", failureAt(0, 14, 0), ""),
		"D3": deploy("github:3", failureAt(1, 9, 0), ""), "D4": deploy("github:4", failureAt(1, 16, 0), ""),
		"D5": deploy("github:5", failureAt(2, 8, 30), ""), "D6": deploy("github:6", failureAt(1, 12, 0), prodevents.StatusDropped),
	}}
	d := w.deploys
	w.failures = map[string]prodevents.Event{
		"F1": r.failure(t, "ALPHA", "check:1", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d["D1"], failureAt(0, 11, 0), new(failureAt(0, 12, 30)), new(d["D2"])),
		"F2": r.failure(t, "ALPHA", "post:2", prodevents.SourceAPI, prodevents.SignalAPI, d["D3"], failureAt(1, 10, 0), nil, nil),
		"F3": r.failure(t, "ALPHA", "person:3", prodevents.SourcePerson, prodevents.SignalPerson, d["D3"], failureAt(1, 11, 0), new(failureAt(2, 9, 0)), new(d["D5"])),
		"F4": r.failure(t, "ALPHA", "check:4", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d["D6"], failureAt(1, 13, 0), new(failureAt(1, 15, 0)), new(d["D4"])),
	}
	return w
}

// threeDays is the window of failureWindow's three days.
var threeDays = Range{From: failureDay, To: failureDay.AddDate(0, 0, 3)}

// TestAWindowOfDeploysAndFailuresFoldsToTheDORAValuesThroughTheStoredRows: over failureWindow's
// three days, ALPHA's stored rows fold to R11's values: change fail rate 2 of 5 deploys (D1, D3),
// recovery the median of F1's 150 minutes and F3's 1,440 (795) with F2 not restored, and rework
// rate 2 of 5 (D2 and D5 restored a failure). The dropped D6, with its counted F4, counts in no
// numerator and no denominator, so D4, which restored F4, is no rework. No row stands before
// failureDay, and every day from it through today holds the all-repositories row of each measure.
func TestAWindowOfDeploysAndFailuresFoldsToTheDORAValuesThroughTheStoredRows(t *testing.T) {
	r := newRefresherRig(t)
	r.seedFailureWindow(t)
	r.settle(t)
	wantFigures(t, "the three days", r.failureValues(t, "ALPHA", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(2.0), "change-fail-rate.total": new(5.0), "change-fail-rate.rate": new(0.4),
		"failed-deploy-recovery.n": new(2.0), "failed-deploy-recovery.median": new(795.0), "failed-deploy-recovery.not_restored": new(1.0),
		"deployment-rework-rate.restoring": new(2.0), "deployment-rework-rate.total": new(5.0), "deployment-rework-rate.rate": new(0.4),
	})
	ids := []string{EvalID("ALPHA", SlugChangeFailRate), EvalID("ALPHA", SlugFailedDeployRecovery), EvalID("ALPHA", SlugDeploymentReworkRate)}
	if n := r.count(t, `select count(*) from eval_days where eval_id = any($1) and day < $2`, ids, failureDay); n != 0 {
		t.Errorf("ALPHA holds %d failure rows before its failures_measured_since %s, want none", n, failureDay.Format(time.DateOnly))
	}
	days := int(refresherToday.Sub(failureDay)/(24*time.Hour)) + 1
	if n := r.count(t, `select count(*) from eval_days where eval_id = any($1) and repo = '' and day >= $2`, ids, failureDay); n != 3*days {
		t.Errorf("ALPHA holds %d all-repositories failure rows from %s through today, want %d, each measure's of each day", n, failureDay.Format(time.DateOnly), 3*days)
	}
	if n := r.count(t, `select count(*) from eval_days where eval_id = $1 and repo = $2 and day = $3 and num = 1 and den = 2`,
		EvalID("ALPHA", SlugChangeFailRate), alphaApp, failureDay); n != 1 {
		t.Errorf("acme/alpha-app's change-fail-rate row of %s does not count 1 failed deploy of 2", failureDay.Format(time.DateOnly))
	}
	expectTruth(t, "after settling the failure window", r.storedFigures(t), r.truthFigures(t))
}

// TestARejectMovesChangeFailRateByThatDeploysShare: rejecting F1, the one failure that blames D1,
// moves ALPHA's change fail rate over failureWindow's three days by exactly D1's share, one deploy
// of five, through the record's change (no walk): 2 of 5 to 1 of 5.
func TestARejectMovesChangeFailRateByThatDeploysShare(t *testing.T) {
	r := newRefresherRig(t)
	w := r.seedFailureWindow(t)
	r.settle(t)
	before := r.failureValues(t, "ALPHA", threeDays)[SlugChangeFailRate].Window
	if _, err := prodevents.SetState(t.Context(), r.database.Pool, "ALPHA", w.failures["F1"].ID, prodevents.StateChange{
		State: prodevents.StateRejected, Reason: "the check was flaky", Actor: alice, At: refresherNow,
	}); err != nil {
		t.Fatalf("reject F1: %v", err)
	}
	report, _ := r.pass(t)
	if !report.days[builtDay{family: failureFamily, project: "ALPHA", day: failureDay}] {
		t.Errorf("the pass after the reject wrote %v, want ALPHA's failure day %s among them", report.sorted(), failureDay.Format(time.DateOnly))
	}
	after := r.failureValues(t, "ALPHA", threeDays)[SlugChangeFailRate].Window
	if before["rate"] == nil || after["rate"] == nil {
		t.Fatalf("change fail rate reads %s before the reject and %s after, want a value each", showValue(before["rate"]), showValue(after["rate"]))
	}
	if *before["failed"]-*after["failed"] != 1 || *before["total"] != *after["total"] {
		t.Errorf("the reject moved failed %g to %g and total %g to %g, want one failed deploy fewer of the same total",
			*before["failed"], *after["failed"], *before["total"], *after["total"])
	}
	if share := 1 / *after["total"]; math.Abs(*before["rate"]-*after["rate"]-share) > 1e-12 {
		t.Errorf("the reject moved change fail rate from %g to %g, want by exactly D1's share %g", *before["rate"], *after["rate"], share)
	}
	expectTruth(t, "after rejecting F1", r.storedFigures(t), r.truthFigures(t))

	// F3's restore and restoring deploy fall on a different day from the deploy it blames.
	// Neither day may be left stale when a reject or re-count changes only the failure's state.
	for _, step := range []struct {
		state             string
		restored, reworks float64
	}{{prodevents.StateRejected, 0, 0}, {prodevents.StateCounted, 1, 1}} {
		if _, err := prodevents.SetState(t.Context(), r.database.Pool, "ALPHA", w.failures["F3"].ID, prodevents.StateChange{
			State: step.state, Reason: "reviewed the flag", Actor: alice, At: refresherNow,
		}); err != nil {
			t.Fatalf("set F3 to %s: %v", step.state, err)
		}
		r.refreshIn(t.Context(), t, nil)
		wantFigures(t, "F3 "+step.state, r.failureValues(t, "ALPHA", threeDays), map[string]*float64{
			"failed-deploy-recovery.n": &step.restored, "deployment-rework-rate.restoring": &step.reworks,
		})
		expectTruth(t, "F3 "+step.state, r.storedFigures(t), r.truthFigures(t))
	}
	restorer := r.githubDeploy(t, "ALPHA", alphaApp, "github:7", failureAt(2, 15, 0), "")
	throughToday := Range{From: failureDay, To: refresherToday.AddDate(0, 0, 1)}
	if _, err := prodevents.SetState(t.Context(), r.database.Pool, "ALPHA", w.failures["F2"].ID, prodevents.StateChange{
		State: prodevents.StateRestored, RestoredByDeploy: new(restorer.ID), Actor: alice, At: failureAt(3, 10, 0),
	}); err != nil {
		t.Fatalf("restore F2: %v", err)
	}
	r.refreshIn(t.Context(), t, nil)
	wantFigures(t, "F2 restored", r.failureValues(t, "ALPHA", throughToday), map[string]*float64{
		"failed-deploy-recovery.n": new(2.0), "deployment-rework-rate.restoring": new(2.0),
	})
	expectTruth(t, "F2 restored", r.storedFigures(t), r.truthFigures(t))
	// Removing the restoring deploy still changes its finish day, even when neither the
	// blamed deploy nor the old and new restore times fall on it.
	if _, err := prodevents.SetState(t.Context(), r.database.Pool, "ALPHA", w.failures["F2"].ID, prodevents.StateChange{
		State: prodevents.StateRestored, Actor: alice, At: failureAt(3, 11, 0),
	}); err != nil {
		t.Fatalf("clear F2's restoring deploy: %v", err)
	}
	r.refreshIn(t.Context(), t, nil)
	wantFigures(t, "F2 restored without a deploy", r.failureValues(t, "ALPHA", throughToday), map[string]*float64{
		"failed-deploy-recovery.n": new(2.0), "deployment-rework-rate.restoring": new(1.0),
	})
	expectTruth(t, "F2 restored without a deploy", r.storedFigures(t), r.truthFigures(t))
}

// TestAWindowBeforeFailuresMeasuredSinceReadsNoDataAndNamesTheDay (R9): with ALPHA's
// failures_measured_since tomorrow, a window of the last week reads "no data" in each measure and
// names tomorrow, and no row stands; moved to 30 days ago, a window from then with deploys and no
// failure reads 0 (and one starting a day earlier still "no data", naming that day); and with one
// failure restored by a later deploy, each of the three reads a value.
func TestAWindowBeforeFailuresMeasuredSinceReadsNoDataAndNamesTheDay(t *testing.T) {
	r := newRefresherRig(t)
	r.seedFailureProjects(t)
	tomorrow := refresherToday.AddDate(0, 0, 1)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults}, tomorrow)
	d1 := r.githubDeploy(t, "ALPHA", alphaApp, "github:1", failureAt(0, 10, 0), "")
	d2 := r.githubDeploy(t, "ALPHA", alphaApp, "github:2", failureAt(1, 10, 0), "")
	r.settle(t)
	week := Range{From: refresherToday.AddDate(0, 0, -6), To: tomorrow}
	noData := map[string]*float64{
		"change-fail-rate.rate": nil, "failed-deploy-recovery.n": nil, "failed-deploy-recovery.not_restored": nil, "deployment-rework-rate.rate": nil,
	}
	named := func(what string, values map[string]SeriesValues, day time.Time) {
		t.Helper()
		for _, slug := range failureDaySlugs {
			if since := values[slug].DataSince; since == nil || *since != day.Format(time.DateOnly) {
				t.Errorf("%s: %s's data_since is %v, want %s", what, slug, since, day.Format(time.DateOnly))
			}
		}
	}
	values := r.failureValues(t, "ALPHA", week)
	wantFigures(t, "a week before failures_measured_since", values, noData)
	named("a week before failures_measured_since", values, tomorrow)
	if n := r.count(t, `select count(*) from eval_days where project_key = 'ALPHA' and eval_id = any($1)`,
		[]string{EvalID("ALPHA", SlugChangeFailRate), EvalID("ALPHA", SlugFailedDeployRecovery), EvalID("ALPHA", SlugDeploymentReworkRate)}); n != 0 {
		t.Errorf("ALPHA holds %d failure rows before its failures_measured_since, want none", n)
	}

	since := refresherToday.AddDate(0, 0, -30)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults}, since)
	r.settle(t)
	measured := Range{From: since, To: tomorrow}
	values = r.failureValues(t, "ALPHA", measured)
	wantFigures(t, "30 days with no failure", values, map[string]*float64{
		"change-fail-rate.rate": new(0.0), "change-fail-rate.total": new(2.0), "failed-deploy-recovery.n": new(0.0),
		"failed-deploy-recovery.median": nil, "failed-deploy-recovery.not_restored": new(0.0), "deployment-rework-rate.rate": new(0.0),
	})
	named("30 days with no failure", values, since)
	earlier := r.failureValues(t, "ALPHA", Range{From: since.AddDate(0, 0, -1), To: tomorrow})
	wantFigures(t, "a window from the day before failures_measured_since", earlier, noData)
	named("a window from the day before failures_measured_since", earlier, since)

	r.failure(t, "ALPHA", "check:1", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d1, failureAt(0, 11, 0), new(failureAt(1, 12, 0)), &d2)
	r.settle(t)
	wantFigures(t, "one failure restored", r.failureValues(t, "ALPHA", measured), map[string]*float64{
		"change-fail-rate.rate": new(0.5), "failed-deploy-recovery.median": new(26 * 60.0),
		"failed-deploy-recovery.not_restored": new(0.0), "deployment-rework-rate.rate": new(0.5),
	})
	expectTruth(t, "after one failure restored", r.storedFigures(t), r.truthFigures(t))
}

// TestAFailureCountsForEachProjectItsDeployCountsForWhoseSourcesIncludeItsSignal (D7, decision 7):
// ALPHA's GitHub deploy D ships ALPHA-1's and OMEGA-1's pull requests, so it counts for ALPHA and
// OMEGA, and DELTA's E ships none, so it counts for DELTA alone. A failing check on D counts for
// ALPHA, whose sources hold check_results, and not for OMEGA, whose hold reverts alone, nor for
// DELTA, whose deploy it is not; a revert of D then counts for OMEGA and not for ALPHA.
func TestAFailureCountsForEachProjectItsDeployCountsForWhoseSourcesIncludeItsSignal(t *testing.T) {
	r := newRefresherRig(t)
	r.seedFailureProjects(t)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults}, failureDay)
	r.putFailures(t, "OMEGA", omegaJob, []string{delivery.FailureSourceReverts}, failureDay)
	r.putFailures(t, "DELTA", "delta / apply", []string{delivery.FailureSourceCheckResults}, failureDay)
	d := r.githubDeploy(t, "ALPHA", alphaApp, "github:1", failureAt(1, 10, 0), "", alphaApp+"#1", alphaApp+"#2")
	r.githubDeploy(t, "DELTA", deltaApp, "github:2", failureAt(1, 11, 0), "")
	r.failure(t, "ALPHA", "check:1", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d, failureAt(1, 12, 0), nil, nil)
	r.settle(t)
	for project, want := range map[string][2]float64{"ALPHA": {1, 1}, "OMEGA": {0, 1}, "DELTA": {0, 1}} {
		wantFigures(t, project+" with a failing check", r.failureValues(t, project, threeDays), map[string]*float64{
			"change-fail-rate.failed": new(want[0]), "change-fail-rate.total": new(want[1]),
		})
	}
	r.failure(t, "OMEGA", "revert:1", prodevents.SourceReverts, prodevents.SignalRevert, d, failureAt(1, 13, 0), nil, nil)
	r.pass(t)
	for project, want := range map[string][2]float64{"ALPHA": {1, 1}, "OMEGA": {1, 1}, "DELTA": {0, 1}} {
		wantFigures(t, project+" with a revert besides", r.failureValues(t, project, threeDays), map[string]*float64{
			"change-fail-rate.failed": new(want[0]), "change-fail-rate.total": new(want[1]),
			"failed-deploy-recovery.not_restored": new(want[0]),
		})
	}
	expectTruth(t, "after the revert on D", r.storedFigures(t), r.truthFigures(t))
}

// TestAPostedDeployThatShipsNoPullRequestCountsForItsRoutesProjectAlone (R12, k12): a deploy posted
// to OMEGA's route, which OMEGA's events source reads, with a posted failure on it, counts for
// OMEGA alone, though ALPHA counts posted failures too; and one posted to ALPHA, which holds no
// events source, counts nowhere.
func TestAPostedDeployThatShipsNoPullRequestCountsForItsRoutesProjectAlone(t *testing.T) {
	r := newRefresherRig(t)
	r.seedFailureProjects(t)
	events := []string{delivery.FailureSourceEvents}
	r.putFailures(t, "ALPHA", alphaJob, events, failureDay)
	r.putFailures(t, "OMEGA", omegaJob, events, failureDay, eventsSource)
	for _, project := range []string{"OMEGA", "ALPHA"} {
		posted := r.post(t, project, "release-1", failureAt(1, 10, 0))
		r.failure(t, project, "page-1", prodevents.SourceAPI, prodevents.SignalAPI, posted, failureAt(1, 11, 0), nil, nil)
	}
	r.settle(t)
	for project, want := range map[string][2]float64{"OMEGA": {1, 1}, "ALPHA": {0, 0}} {
		wantFigures(t, project, r.failureValues(t, project, threeDays), map[string]*float64{
			"change-fail-rate.failed": new(want[0]), "change-fail-rate.total": new(want[1]), "deployment-rework-rate.total": new(want[1]),
		})
	}
	expectTruth(t, "after the posted deploys and failures", r.storedFigures(t), r.truthFigures(t))
}

// TestAPersonsFlagCountsUnderAnySource: ALPHA counts reverts alone, and a person's flag of D1
// counts all the same, where a posted failure of D2 does not: change fail rate 1 of 2.
func TestAPersonsFlagCountsUnderAnySource(t *testing.T) {
	r := newRefresherRig(t)
	r.seedFailureProjects(t)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceReverts}, failureDay)
	d1 := r.githubDeploy(t, "ALPHA", alphaApp, "github:1", failureAt(1, 10, 0), "")
	d2 := r.githubDeploy(t, "ALPHA", alphaApp, "github:2", failureAt(1, 14, 0), "")
	r.failure(t, "ALPHA", "person:1", prodevents.SourcePerson, prodevents.SignalPerson, d1, failureAt(1, 11, 0), nil, nil)
	r.failure(t, "ALPHA", "post:2", prodevents.SourceAPI, prodevents.SignalAPI, d2, failureAt(1, 15, 0), nil, nil)
	r.settle(t)
	wantFigures(t, "a person's flag under reverts alone", r.failureValues(t, "ALPHA", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(1.0), "change-fail-rate.total": new(2.0), "failed-deploy-recovery.not_restored": new(1.0),
	})
	expectTruth(t, "after the person's flag", r.storedFigures(t), r.truthFigures(t))
}

// TestASettingsChangeOfTheFailureSourcesRebuildsTheRows: ALPHA's deploy D1 carries a posted
// failure, which its check_results source does not count; a save that turns on events rewrites
// ALPHA's failure rows on the pass that reads the settings change, which then count it; and a save
// that turns every failure source off deletes them, and the read names no first day.
func TestASettingsChangeOfTheFailureSourcesRebuildsTheRows(t *testing.T) {
	r := newRefresherRig(t)
	r.seedFailureProjects(t)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults}, failureDay)
	d1 := r.githubDeploy(t, "ALPHA", alphaApp, "github:1", failureAt(1, 10, 0), "")
	r.failure(t, "ALPHA", "post:1", prodevents.SourceAPI, prodevents.SignalAPI, d1, failureAt(1, 11, 0), nil, nil)
	r.settle(t)
	wantFigures(t, "check_results alone", r.failureValues(t, "ALPHA", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(0.0), "change-fail-rate.total": new(1.0),
	})

	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults, delivery.FailureSourceEvents}, failureDay)
	report, _ := r.pass(t)
	if !report.days[builtDay{family: failureFamily, project: "ALPHA", day: failureDay.AddDate(0, 0, 1)}] {
		t.Errorf("the pass after the save wrote %v, want ALPHA's failure day %s among them", report.sorted(), failureDay.AddDate(0, 0, 1).Format(time.DateOnly))
	}
	wantFigures(t, "events turned on", r.failureValues(t, "ALPHA", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(1.0), "change-fail-rate.total": new(1.0),
	})
	expectTruth(t, "after turning events on", r.storedFigures(t), r.truthFigures(t))

	r.putFailures(t, "ALPHA", alphaJob, nil, time.Time{})
	r.pass(t)
	ids := []string{EvalID("ALPHA", SlugChangeFailRate), EvalID("ALPHA", SlugFailedDeployRecovery), EvalID("ALPHA", SlugDeploymentReworkRate)}
	if n := r.count(t, `select count(*) from eval_days where eval_id = any($1)`, ids); n != 0 {
		t.Errorf("ALPHA holds %d failure rows once its failure sources are off, want none", n)
	}
	off := r.failureValues(t, "ALPHA", threeDays)
	wantFigures(t, "failure sources off", off, map[string]*float64{"change-fail-rate.rate": nil, "deployment-rework-rate.rate": nil})
	for _, slug := range failureDaySlugs {
		if since := off[slug].DataSince; since != nil {
			t.Errorf("with failure sources off %s's data_since is %s, want none", slug, *since)
		}
	}
	expectTruth(t, "after turning failure sources off", r.storedFigures(t), r.truthFigures(t))
}

// TestTheStoredDaysReadAsTheirDayInASessionEastOfUTC: a date the evals read, a repository's
// kept_since in eval_days_rules (builtUnderIn) and a project's failures_measured_since
// (failuresSinceIn, for its project and for every project), reads as its own UTC midnight in a
// session whose time zone is east of UTC (Pacific/Kiritimati, UTC+14), where a cast to timestamptz
// would read the day before.
func TestTheStoredDaysReadAsTheirDayInASessionEastOfUTC(t *testing.T) {
	r := newRefresherRig(t)
	ctx := t.Context()
	r.seedFailureProjects(t)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults}, failureDay)
	r.exec(t, "a repository's rule with its start", `insert into eval_days_rules (repo, project_key, production_job_names, kept_since)
		values ($1, 'ALPHA', '{}', $2::date)`, alphaApp, failureDay)
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local timezone = 'Pacific/Kiritimati'`); err != nil {
		t.Fatalf("set the session's time zone: %v", err)
	}
	built, err := builtUnderIn(ctx, tx)
	if err != nil {
		t.Fatalf("read the rules the rows stand built under: %v", err)
	}
	if since := built[ruleKey(alphaApp)].since; since == nil || !since.Equal(failureDay) {
		t.Errorf("%s's kept_since %s reads as %v, want that day", alphaApp, failureDay.Format(time.DateOnly), since)
	}
	for _, projects := range [][]string{{"ALPHA"}, nil} {
		since, err := failuresSinceIn(ctx, tx, projects)
		if err != nil {
			t.Fatalf("read the failure measures' first days of %v: %v", projects, err)
		}
		if day := since["ALPHA"]; day == nil || !day.Equal(failureDay) {
			t.Errorf("ALPHA's failures_measured_since %s reads as %v (projects %v), want that day", failureDay.Format(time.DateOnly), day, projects)
		}
	}
}

// TestASaveOfTheFailureFieldsRefoldsTheFailureEvalsInEveryCut (dispatch://INFRA-978, plan T19
// path 5): after a save moves ALPHA's failures_measured_since back 30 days, the pass that reads
// the settings change writes ALPHA's failure rows in one transaction marked a settings rewrite
// (delivery.EvalDaysWrittenBySettingsRewrite), refolding every cut of the three failure evals
// before commit so each cut's stored figures equal the fold of their day rows, while every other
// eval's stored figures are unchanged.
func TestASaveOfTheFailureFieldsRefoldsTheFailureEvalsInEveryCut(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	r.seedFailureProjects(t)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults}, failureDay)
	early := failureDay.AddDate(0, 0, -20)
	r.deployRun(t, alphaApp, 1001, failureAt(0, 10, 0), alphaJob)
	dEarly := r.githubDeploy(t, "ALPHA", alphaApp, "github:early", early.Add(10*time.Hour), "")
	dLater := r.githubDeploy(t, "ALPHA", alphaApp, "github:later", failureAt(0, 10, 0), "")
	r.failure(t, "ALPHA", "check:early", prodevents.SourceCheckResults, prodevents.SignalCheckFail, dEarly, early.Add(11*time.Hour), new(early.Add(13*time.Hour)), &dLater)
	r.settle(t)
	expectTruth(t, "before moving failures_measured_since", r.storedFigures(t), r.truthFigures(t))

	before := r.storedFigures(t)
	seq := r.latestSeq(t)
	since := failureDay.AddDate(0, 0, -30)
	r.putFailures(t, "ALPHA", alphaJob, []string{delivery.FailureSourceCheckResults}, since)
	r.pass(t)

	after := r.storedFigures(t)
	expectTruth(t, "after moving failures_measured_since back 30 days", after, r.truthFigures(t))
	failureIDs := map[string]bool{
		EvalID("ALPHA", SlugChangeFailRate):       true,
		EvalID("ALPHA", SlugFailedDeployRecovery): true,
		EvalID("ALPHA", SlugDeploymentReworkRate): true,
	}
	for _, line := range writtenFigures(before, after) {
		parts := strings.Fields(line)
		evalID := parts[len(parts)-1]
		if !failureIDs[evalID] {
			t.Errorf("moving failures_measured_since wrote a non-failure eval's stored figure: %s", line)
		}
	}
	if _, err := delivery.DrainPending(ctx, pool); err != nil {
		t.Fatalf("drain pending changes: %v", err)
	}
	changes, err := delivery.ChangesSince(ctx, pool, seq, 200)
	if err != nil {
		t.Fatalf("read changes after moving failures_measured_since: %v", err)
	}
	var marked []string
	for _, change := range changes {
		if change.Kind != delivery.ChangeKindEvalDays || !failureIDs[change.Ref] {
			continue
		}
		if change.RunKind != delivery.EvalDaysWrittenBySettingsRewrite {
			t.Errorf("the failure settings rewrite logged %s with run_kind %q, want %q",
				change.Ref, change.RunKind, delivery.EvalDaysWrittenBySettingsRewrite)
		}
		marked = append(marked, change.Ref)
	}
	slices.Sort(marked)
	wantMarked := slices.Sorted(maps.Keys(failureIDs))
	if !slices.Equal(marked, wantMarked) {
		t.Errorf("the failure settings rewrite logged %v, want each of %v once", marked, wantMarked)
	}
}

// TestAFailureCountsForEachProjectItsDeployCountsForUnderThePublishedDefaults
// (dispatch://INFRA-978, plan T19 path 6): a failure on a deploy that ships a pull request with no
// issue in a repository that is the default of two projects (ALPHA and BETA) counts for both when
// both have the signal's source on; after BETA's default moves away and the move is published, for
// ALPHA alone; a pull request whose issue (PIER-1, owning no repository) is held only through its
// project's published default counts there and not by the default repository once PIER's default
// matches, and a pull request whose issue owns the repository (OMEGA-1) counts for OMEGA and not by
// the default repository; a deploy that ships no pull request counts for its source key's reader
// (ALPHA, whose deploy source reads the repository) and not for BETA even while BETA shares the
// default repository; and change-fail-rate's deploys equal deploy-frequency's for each project.
func TestAFailureCountsForEachProjectItsDeployCountsForUnderThePublishedDefaults(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	shared, other := alphaApp, "acme/beta-other"
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('ALPHA', 'Alpha', 'alice', 2, $1), ('BETA', 'Beta', 'alice', 2, $1),
		       ('OMEGA', 'Omega', 'alice', 2, $2), ('PIER', 'Pier', 'alice', 2, $3)`,
		shared, omegaApp, pierRepo)
	r.exec(t, "the issues", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('OMEGA-1', 'OMEGA', 1, 'Omega work', 'in_progress', '00000001', $1, '{"kind":"system","id":"failures-test"}', $2),
		       ('PIER-1', 'PIER', 1, 'Pier work', 'in_progress', '00000001', null, '{"kind":"system","id":"failures-test"}', $2)`,
		shared, failureDay.AddDate(0, 0, -10))

	allFailureSources := []string{delivery.FailureSourceCheckResults}
	putProjectSettings(t, pool, delivery.ProjectSettingsInput{
		Project: "ALPHA", Delivery: true, Sources: []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob(alphaJob), new(shared))},
		PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{}, FailureSources: allFailureSources, FailuresMeasuredSince: &failureDay,
	})
	putProjectSettings(t, pool, delivery.ProjectSettingsInput{
		Project: "BETA", Delivery: true, Sources: []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob("beta / apply"), new(other))},
		PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{}, FailureSources: allFailureSources, FailuresMeasuredSince: &failureDay,
	})
	putProjectSettings(t, pool, delivery.ProjectSettingsInput{
		Project: "OMEGA", Delivery: true, Sources: []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob(omegaJob), new(omegaApp))},
		PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{}, FailureSources: allFailureSources, FailuresMeasuredSince: &failureDay,
	})
	putProjectSettings(t, pool, delivery.ProjectSettingsInput{
		Project: "PIER", Delivery: true, Sources: []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob("pier / apply"), new(pierRepo))},
		PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{}, FailureSources: allFailureSources, FailuresMeasuredSince: &failureDay,
	})

	for _, pr := range []struct {
		number int
		merged time.Time
		issue  *string
		attrs  *delivery.AttributionInputs
	}{
		{1, failureAt(0, 9, 0), nil, &delivery.AttributionInputs{TitleKeys: []string{}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}}},
		{2, failureAt(0, 11, 0), new("OMEGA-1"), &delivery.AttributionInputs{TitleKeys: []string{"OMEGA-1"}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}}},
		{3, failureAt(0, 13, 0), nil, &delivery.AttributionInputs{TitleKeys: []string{"PIER-1"}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}}},
	} {
		if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
			Repo: shared, Number: pr.number, Title: fmt.Sprintf("Change %d", pr.number),
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", shared, pr.number), Author: "dev-ana",
			CreatedAt: new(pr.merged.Add(-time.Hour)), MergedAt: new(pr.merged), IssueKey: pr.issue,
			Sessions: []string{}, Attribution: pr.attrs,
		}); err != nil {
			t.Fatalf("write %s#%d: %v", shared, pr.number, err)
		}
	}
	r.deployRun(t, shared, 101, failureAt(0, 10, 0), alphaJob)
	r.deployRun(t, shared, 102, failureAt(0, 12, 0), alphaJob)
	r.deployRun(t, shared, 103, failureAt(0, 14, 0), alphaJob)
	r.deployRun(t, shared, 104, failureAt(0, 16, 0), alphaJob)
	d1 := r.githubDeploy(t, "ALPHA", shared, "github:101", failureAt(0, 10, 20), "", shared+"#1")
	d2 := r.githubDeploy(t, "ALPHA", shared, "github:102", failureAt(0, 12, 20), "", shared+"#2")
	d3 := r.githubDeploy(t, "ALPHA", shared, "github:103", failureAt(0, 14, 20), "", shared+"#3")
	// The production record still names a previous source owner; the published source key
	// reader is ALPHA, not OMEGA and not BETA, which only shares the default.
	d4 := r.githubDeploy(t, "OMEGA", shared, "github:104", failureAt(0, 16, 20), "")
	r.failure(t, "ALPHA", "check:101", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d1, failureAt(0, 10, 30), nil, nil)
	r.failure(t, "ALPHA", "check:102", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d2, failureAt(0, 12, 30), nil, nil)
	r.failure(t, "ALPHA", "check:103", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d3, failureAt(0, 14, 30), nil, nil)
	r.failure(t, "ALPHA", "check:104", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d4, failureAt(0, 16, 30), nil, nil)
	r.settle(t)

	check := func(what string, want map[string][2]float64) {
		t.Helper()
		for _, project := range []string{"ALPHA", "BETA", "OMEGA", "PIER"} {
			values := r.failureValues(t, project, threeDays)
			wantFigures(t, what+" "+project, values, map[string]*float64{
				"change-fail-rate.failed": new(want[project][0]), "change-fail-rate.total": new(want[project][1]),
			})
			deploys := r.deploysOn(t, project, failureDay)
			if float64(deploys.n) != want[project][1] {
				t.Errorf("%s: %s deploy-frequency counts %d deploys, change-fail-rate wants %g", what, project, deploys.n, want[project][1])
			}
			tx, err := r.registry.BeginSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			record, err := FailureRecordIn(ctx, tx, r.registry.attributor, project, SlugChangeFailRate)
			_ = tx.Rollback(ctx)
			if err != nil {
				t.Fatalf("%s: FailureRecordIn(%s): %v", what, project, err)
			}
			items := record.Items(threeDays)
			deployItems := 0
			for _, item := range items {
				if item.Event.Kind == prodevents.KindDeploy {
					deployItems++
				}
			}
			if float64(deployItems) != want[project][1] {
				t.Errorf("%s: %s FailureRecordIn answers %d deploy items, want %g", what, project, deployItems, want[project][1])
			}
		}
		expectTruth(t, what, r.storedFigures(t), r.truthFigures(t))
	}

	check("shared default", map[string][2]float64{
		"ALPHA": {3, 3}, "BETA": {2, 2}, "OMEGA": {1, 1}, "PIER": {0, 0},
	})

	r.exec(t, "move BETA's default away", `update projects set default_repo = $1 where key = 'BETA'`, other)
	r.exec(t, "move PIER's default to shared", `update projects set default_repo = $1 where key = 'PIER'`, shared)
	check("before publishing default moves", map[string][2]float64{
		"ALPHA": {3, 3}, "BETA": {2, 2}, "OMEGA": {1, 1}, "PIER": {0, 0},
	})

	r.settle(t)
	check("after BETA moves away and PIER moves to shared", map[string][2]float64{
		"ALPHA": {2, 2}, "BETA": {0, 0}, "OMEGA": {1, 1}, "PIER": {2, 2},
	})
}

// TestTheFailureRowsOfAMovedProjectAreWrittenUnderThePublishedDefaults (dispatch://INFRA-978, plan
// T19 path 6): moving a project's default writes the moved projects' failure rows after the commit
// that publishes the move, in the same pass, under the published defaults, with each cut's stored
// figures of the three failure evals equal to their day rows (and every eval once the pass finishes
// its stale-cut refold). The test also covers a project whose own default did not move (ALPHA)
// losing a deploy whose pull request's issue is held through another project's default (PIER-1),
// so publishDefaults' full moved set is written under the published defaults.
func TestTheFailureRowsOfAMovedProjectAreWrittenUnderThePublishedDefaults(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	r.seedFailureProjects(t)
	r.exec(t, "add PIER", `insert into projects (key, name, default_assignee, next_number, default_repo) values ('PIER', 'Pier', 'alice', 2, $1)`, pierRepo)
	r.exec(t, "add PIER-1", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('PIER-1', 'PIER', 1, 'Pier work', 'in_progress', '00000001', null, '{"kind":"system","id":"failures-test"}', $1)`, failureDay.AddDate(0, 0, -10))
	putProjectSettings(t, pool, delivery.ProjectSettingsInput{
		Project: "ALPHA", Delivery: true, Sources: []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob(alphaJob), new(alphaApp))},
		PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{}, FailureSources: []string{delivery.FailureSourceCheckResults}, FailuresMeasuredSince: &failureDay,
	})
	putProjectSettings(t, pool, delivery.ProjectSettingsInput{
		Project: "PIER", Delivery: true, Sources: []delivery.SourceInput{deliverytest.SourceOf(ruleOfJob("pier / apply"), new(pierRepo))},
		PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{}, FailureSources: []string{delivery.FailureSourceCheckResults}, FailuresMeasuredSince: &failureDay,
	})
	if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
		Repo: alphaApp, Number: 10, Title: "PIER-1: conditional change", URL: "https://github.com/" + alphaApp + "/pull/10",
		Author: "dev-ana", CreatedAt: new(failureAt(0, 8, 0)), MergedAt: new(failureAt(0, 9, 0)), Sessions: []string{},
		Attribution: &delivery.AttributionInputs{TitleKeys: []string{"PIER-1"}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
	}); err != nil {
		t.Fatalf("write conditional PR: %v", err)
	}
	r.deployRun(t, alphaApp, 110, failureAt(0, 10, 0), alphaJob)
	d1 := r.githubDeploy(t, "ALPHA", alphaApp, "github:110", failureAt(0, 10, 20), "", alphaApp+"#10")
	r.failure(t, "ALPHA", "check:110", prodevents.SourceCheckResults, prodevents.SignalCheckFail, d1, failureAt(0, 11, 0), nil, nil)
	r.settle(t)
	wantFigures(t, "ALPHA before moving PIER's default", r.failureValues(t, "ALPHA", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(1.0), "change-fail-rate.total": new(1.0),
	})
	wantFigures(t, "PIER before moving PIER's default", r.failureValues(t, "PIER", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(0.0), "change-fail-rate.total": new(0.0),
	})

	// Move PIER's default to alphaApp: PIER-1 now resolves to alphaApp, moving d1 from ALPHA to PIER.
	// One woken pass (no walk) publishes the move, writes ALPHA's and PIER's failure rows after the
	// commit under the published defaults, and finishes the stale-cut refold.
	r.exec(t, "move PIER's default to alphaApp", `update projects set default_repo = $1 where key = 'PIER'`, alphaApp)
	report, _ := r.refreshIn(t.Context(), t, nil)
	for _, project := range []string{"ALPHA", "PIER"} {
		if !report.days[builtDay{family: failureFamily, project: project, day: failureDay}] {
			t.Errorf("the publishing pass wrote %v, want %s's failure day %s written in the same pass",
				report.sorted(), project, failureDay.Format(time.DateOnly))
		}
	}
	// The failure rows were written after the publishing commit: their xmin must be strictly later
	// than eval_days_defaults' xmin.
	var defaultsXmin, pierFailureXmin uint32
	if err := pool.QueryRow(ctx, `select xmin::text::bigint from eval_days_defaults where project_key = 'PIER'`).Scan(&defaultsXmin); err != nil {
		t.Fatalf("read eval_days_defaults xmin: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		select xmin::text::bigint from eval_days
		where project_key = 'PIER' and eval_id = $1 and repo = '' and day = $2
	`, EvalID("PIER", SlugChangeFailRate), failureDay).Scan(&pierFailureXmin); err != nil {
		t.Fatalf("read PIER failure row xmin: %v", err)
	}
	if pierFailureXmin <= defaultsXmin {
		t.Errorf("PIER's failure row xmin %d <= eval_days_defaults xmin %d; want the failure rows written after the publishing commit",
			pierFailureXmin, defaultsXmin)
	}
	wantFigures(t, "ALPHA right after publishing pass", r.failureValues(t, "ALPHA", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(0.0), "change-fail-rate.total": new(0.0),
	})
	wantFigures(t, "PIER right after publishing pass", r.failureValues(t, "PIER", threeDays), map[string]*float64{
		"change-fail-rate.failed": new(1.0), "change-fail-rate.total": new(1.0),
	})
	expectTruth(t, "after publishing PIER's default move", r.storedFigures(t), r.truthFigures(t))
}
