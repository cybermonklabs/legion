package measures

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// fixtureFile is testdata/dora-fixture.json: the prototype's web/fixtures/dataset.json with its
// names replaced, carrying two expectation sets per PR and run. expected.prototype is the deploy
// the prototype's fixture states (its own dora.test.ts inputs); expected.stored is the deploy the
// prototype's containment derives from the fixture's runs, which is what Dispatch derives from the
// stored rows (containment.go FirstShippingApply).
type fixtureFile struct {
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	PRs []struct {
		Repo          string     `json:"repo"`
		Number        int        `json:"number"`
		CreatedAt     time.Time  `json:"created_at"`
		MergedAt      time.Time  `json:"merged_at"`
		FirstCommitAt *time.Time `json:"first_commit_at"`
		Rework        bool       `json:"rework"`
		Expected      struct {
			Prototype fixtureDeploy `json:"prototype"`
			Stored    fixtureDeploy `json:"stored"`
		} `json:"expected"`
	} `json:"prs"`
	Runs []struct {
		RunID      int64                           `json:"run_id"`
		StartedAt  time.Time                       `json:"started_at"`
		Conclusion *delivery.DeliveryRunConclusion `json:"conclusion"`
		Production *struct {
			Conclusion string `json:"conclusion"`
		} `json:"production"`
		ExpectedPRs       []string `json:"expected_prs"`
		ExpectedPRsStored []string `json:"expected_prs_stored"`
	} `json:"runs"`
}

type fixtureDeploy struct {
	DeployRun  *int64     `json:"deploy_run"`
	DeployedAt *time.Time `json:"deployed_at"`
}

func readFixture(t *testing.T) fixtureFile {
	t.Helper()
	data, err := os.ReadFile("testdata/dora-fixture.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture fixtureFile
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

// loadFixture builds Compute's inputs from the fixture as the prototype's own dora.test.ts reads
// it: each PR's deploy as the fixture states it, and each run's ShippedPRs from expected_prs.
func loadFixture(t *testing.T) ([]PR, []Run, Window) {
	return buildFixture(t, false)
}

// storedFixture builds the same inputs with each PR's deploy as containment derives it from the
// fixture's runs (expected.stored, expected_prs_stored): the numbers Task 3's API test reads back
// from Postgres.
func storedFixture(t *testing.T) ([]PR, []Run, Window) {
	return buildFixture(t, true)
}

func buildFixture(t *testing.T, stored bool) ([]PR, []Run, Window) {
	t.Helper()
	fixture := readFixture(t)
	prs := make([]PR, 0, len(fixture.PRs))
	for _, pr := range fixture.PRs {
		deploy := pr.Expected.Prototype
		if stored {
			deploy = pr.Expected.Stored
		}
		prs = append(prs, PR{
			ID:            pr.Repo + "#" + strconv.Itoa(pr.Number),
			CreatedAt:     pr.CreatedAt,
			MergedAt:      pr.MergedAt,
			FirstCommitAt: pr.FirstCommitAt,
			DeployRunID:   deploy.DeployRun,
			DeployedAt:    deploy.DeployedAt,
			Rework:        pr.Rework,
		})
	}
	runs := make([]Run, 0, len(fixture.Runs))
	for _, run := range fixture.Runs {
		shipped := run.ExpectedPRs
		if stored {
			shipped = run.ExpectedPRsStored
		}
		runs = append(runs, Run{
			ID:                run.RunID,
			StartedAt:         run.StartedAt,
			Conclusion:        run.Conclusion,
			ReachedProduction: run.Production != nil && run.Production.Conclusion == "success",
			ShippedPRs:        len(shipped),
		})
	}
	return prs, runs, Window{From: fixture.Window.From, To: fixture.Window.To}
}

const tolerance = 1e-9

func near(got, want float64) bool { return math.Abs(got-want) <= tolerance }

func assertFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if !near(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func assertOptional(t *testing.T, name string, got *float64, want *float64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", name, show(got), show(want))
	case !near(*got, *want):
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}

func show(v *float64) any {
	if v == nil {
		return "nil"
	}
	return *v
}

func assertSpread(t *testing.T, name string, got Spread, median, p90, max float64) {
	t.Helper()
	assertOptional(t, name+".median_minutes", got.MedianMinutes, &median)
	assertOptional(t, name+".p90_minutes", got.P90Minutes, &p90)
	assertOptional(t, name+".max_minutes", got.MaxMinutes, &max)
}

func dayOf(t *testing.T, daily []DailyPoint, day string) DailyPoint {
	t.Helper()
	for _, point := range daily {
		if point.Day == day {
			return point
		}
	}
	t.Fatalf("daily has no %s", day)
	return DailyPoint{}
}

func assertDay(t *testing.T, point DailyPoint, deploys, concluded, reached, cancelled int, rate *float64) {
	t.Helper()
	if point.Deploys != deploys || point.Concluded != concluded || point.ReachedProduction != reached || point.Cancelled != cancelled {
		t.Errorf("%s = {deploys %d, concluded %d, reached %d, cancelled %d}, want {%d, %d, %d, %d}",
			point.Day, point.Deploys, point.Concluded, point.ReachedProduction, point.Cancelled, deploys, concluded, reached, cancelled)
	}
	assertOptional(t, point.Day+".run_success_rate", point.RunSuccessRate, rate)
}

// Expected values below are the prototype's own computeDora output on the same fixture:
//
//	cd ~/proto/delivery-timeline/web && bun /tmp/plan567s23/dump-dora.ts
//
// and its web/src/lib/dora.test.ts asserts the same numbers (`bun test src/lib/dora.test.ts`).

func TestComputeMatchesThePrototypeOnItsFixture(t *testing.T) {
	prs, runs, window := loadFixture(t)
	result := Compute(prs, runs, FailureParts{}, window)

	frequency := result.DeployFrequency
	if frequency.SuccessfulDeploys != 5 || frequency.DeploysWithPRs != 5 {
		t.Errorf("deploy_frequency = {%d, %d}, want {5, 5}", frequency.SuccessfulDeploys, frequency.DeploysWithPRs)
	}
	assertFloat(t, "deploy_frequency.per_day", frequency.PerDay, 0.17341040462427745)
	assertFloat(t, "deploy_frequency.with_prs_per_day", frequency.WithPRsPerDay, 0.17341040462427745)

	assertSpread(t, "merge_to_production", result.LeadTime.MergeToProduction, 60, 1014, 1140)
	assertSpread(t, "first_commit_to_production", result.LeadTime.FirstCommitToProduction, 255, 1494, 1620)
	assertSpread(t, "opened_to_production", result.LeadTime.OpenedToProduction, 210, 1416, 1500)
	assertSpread(t, "opened_to_merge", result.LeadTime.OpenedToMerge, 330, 462, 1080)

	if result.ChangeFailureRate != nil || result.TimeToRestore != nil || result.DeploymentReworkRate != nil {
		t.Errorf("the failure measures with no stored part = %+v, %+v, %+v, want no value", result.ChangeFailureRate, result.TimeToRestore, result.DeploymentReworkRate)
	}
	assertFloat(t, "rework_share", result.ReworkShare, 4.0/14)

	success := result.DeployRunSuccess
	if success.Concluded != 7 || success.ReachedProduction != 5 || success.Cancelled != 1 {
		t.Errorf("deploy_run_success = %+v, want {7, 5, 1}", success)
	}
	assertOptional(t, "deploy_run_success.rate", success.Rate, new(5.0/7))

	daily := result.Daily
	if len(daily) != 29 {
		t.Fatalf("len(daily) = %d, want 29", len(daily))
	}
	if daily[0].Day != "2026-08-30" || daily[0].Partial {
		t.Errorf("daily[0] = {%s, partial %t}, want {2026-08-30, false}", daily[0].Day, daily[0].Partial)
	}
	if daily[28].Day != "2026-09-27" || !daily[28].Partial {
		t.Errorf("daily[28] = {%s, partial %t}, want {2026-09-27, true}", daily[28].Day, daily[28].Partial)
	}
	deploys, concluded := 0, 0
	for _, point := range daily {
		deploys += point.Deploys
		concluded += point.Concluded
	}
	if deploys != frequency.SuccessfulDeploys || concluded != success.Concluded {
		t.Errorf("daily sums = {deploys %d, concluded %d}, want the totals {%d, %d}", deploys, concluded, frequency.SuccessfulDeploys, success.Concluded)
	}
	assertDay(t, dayOf(t, daily, "2026-09-05"), 1, 1, 1, 0, new(1.0))
	assertDay(t, dayOf(t, daily, "2026-09-11"), 0, 1, 0, 0, new(0.0))
	assertDay(t, dayOf(t, daily, "2026-09-19"), 0, 0, 0, 1, nil)
	assertDay(t, dayOf(t, daily, "2026-09-01"), 0, 0, 0, 0, nil)
}

// TestComputeOnStoredContainment runs the fixture with each PR's deploy derived from the fixture's
// runs, as Dispatch derives it from stored rows; the numbers are the prototype's computeDora over
// the prototype's own containment of the same fixture (`bun /tmp/plan567s23/dump-dora-stored.ts`
// from the prototype's web/), so Task 3's API test and this package agree on one set.
func TestComputeOnStoredContainment(t *testing.T) {
	prs, runs, window := storedFixture(t)
	result := Compute(prs, runs, FailureParts{}, window)

	if result.DeployFrequency.SuccessfulDeploys != 5 || result.DeployFrequency.DeploysWithPRs != 5 {
		t.Errorf("deploy_frequency = %+v, want {5, 5}", result.DeployFrequency)
	}
	assertFloat(t, "deploy_frequency.per_day", result.DeployFrequency.PerDay, 0.17341040462427745)
	assertSpread(t, "merge_to_production", result.LeadTime.MergeToProduction, 510, 2736, 3840)
	assertSpread(t, "first_commit_to_production", result.LeadTime.FirstCommitToProduction, 1380, 2988, 4380)
	assertSpread(t, "opened_to_production", result.LeadTime.OpenedToProduction, 1260, 2874, 4320)
	assertSpread(t, "opened_to_merge", result.LeadTime.OpenedToMerge, 330, 462, 1080)
	assertFloat(t, "rework_share", result.ReworkShare, 4.0/14)
	if s := result.DeployRunSuccess; s.Concluded != 7 || s.ReachedProduction != 5 || s.Cancelled != 1 {
		t.Errorf("deploy_run_success = %+v, want {7, 5, 1}", s)
	}
}

// TestComputeAnswersTheFailureMeasuresByTheirEvalsFolds: the three failure measures are their
// evals' stored parts folded by each eval's one fold, so the measures answer and an eval read of
// the same parts agree. With no part (no row: before failures_measured_since, or a project with no
// failure source) each reads no value, encoded null, which no target can judge; a day row with
// deploys and no failure reads a measured 0, and one with one restored failure a value in all
// three.
func TestComputeAnswersTheFailureMeasuresByTheirEvalsFolds(t *testing.T) {
	prs, runs, window := loadFixture(t)
	unmeasured := Compute(prs, runs, FailureParts{}, window)
	encoded, err := json.Marshal(struct {
		ChangeFail any `json:"change_failure_rate"`
		Recovery   any `json:"time_to_restore"`
		Rework     any `json:"deployment_rework_rate"`
	}{unmeasured.ChangeFailureRate, unmeasured.TimeToRestore, unmeasured.DeploymentReworkRate})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"change_failure_rate":null,"time_to_restore":null,"deployment_rework_rate":null}`; string(encoded) != want {
		t.Errorf("the failure measures with no stored part encode %s, want %s", encoded, want)
	}

	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	quiet := []FailureDeploy{{}, {}}
	measured := Compute(prs, runs, FailureParts{
		ChangeFail: ChangeFailRateDay(day, quiet), Recovery: RecoveryDay(day, nil, 0), Rework: ReworkRateDay(day, quiet),
	}, window)
	if rate := measured.ChangeFailureRate; rate == nil || rate.Failed != 0 || rate.Total != 2 || rate.Rate == nil || *rate.Rate != 0 {
		t.Errorf("change_failure_rate with deploys and no failure = %+v, want a measured 0 of 2", rate)
	}
	if rate := measured.DeploymentReworkRate; rate == nil || rate.Restoring != 0 || rate.Total != 2 || rate.Rate == nil || *rate.Rate != 0 {
		t.Errorf("deployment_rework_rate with deploys and no restore = %+v, want a measured 0 of 2", rate)
	}
	if recovery := measured.TimeToRestore; recovery == nil || recovery.N != 0 || recovery.MedianMinutes != nil || recovery.NotRestored != 0 {
		t.Errorf("time_to_restore with no restore = %+v, want no median and nothing left", recovery)
	}

	broken := []FailureDeploy{{Failures: 1}, {Restores: true}}
	parts := FailureParts{
		ChangeFail: ChangeFailRateDay(day, broken),
		Recovery:   RecoveryDay(day, []RestoredFailure{{DeployFinishedAt: day.Add(9 * time.Hour), RestoredAt: day.Add(11 * time.Hour)}}, 0),
		Rework:     ReworkRateDay(day, broken),
	}
	failed := Compute(prs, runs, parts, window)
	if rate, want := failed.ChangeFailureRate, FoldChangeFailRate(parts.ChangeFail); rate == nil || rate.Failed != want.Failed || rate.Total != want.Total || *rate.Rate != 0.5 {
		t.Errorf("change_failure_rate = %+v, want FoldChangeFailRate's 1 of 2", rate)
	}
	if recovery := failed.TimeToRestore; recovery == nil || recovery.MedianMinutes == nil || *recovery.MedianMinutes != 120 || recovery.N != 1 {
		t.Errorf("time_to_restore = %+v, want FoldRecovery's 120-minute median of one restore", recovery)
	}
	if rate, want := failed.DeploymentReworkRate, FoldReworkRate(parts.Rework); rate == nil || rate.Restoring != want.Restoring || rate.Total != want.Total || *rate.Rate != 0.5 {
		t.Errorf("deployment_rework_rate = %+v, want FoldReworkRate's 1 of 2", rate)
	}
}

// TestACancelledRunWhoseProductionJobSucceededIsADeployButNotARunSuccess is dora.test.ts:122-137.
func TestACancelledRunWhoseProductionJobSucceededIsADeployButNotARunSuccess(t *testing.T) {
	prs, runs, window := loadFixture(t)
	runs = append(runs, Run{
		ID:                509,
		StartedAt:         time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC),
		Conclusion:        new(delivery.DeliveryRunConclusionCancelled),
		ReachedProduction: true,
	})
	result := Compute(prs, runs, FailureParts{}, window)
	if result.DeployFrequency.SuccessfulDeploys != 6 {
		t.Errorf("successful_deploys = %d, want 6", result.DeployFrequency.SuccessfulDeploys)
	}
	if s := result.DeployRunSuccess; s.Concluded != 7 || s.ReachedProduction != 5 || s.Cancelled != 2 {
		t.Errorf("deploy_run_success = %+v, want {7, 5, 2}", s)
	}
	assertOptional(t, "deploy_run_success.rate", result.DeployRunSuccess.Rate, new(5.0/7))
	assertDay(t, dayOf(t, result.Daily, "2026-09-20"), 1, 0, 0, 1, nil)
}

// windowed narrows the fixture to a window as the store's queries do: runs started in [From, To)
// and PRs merged in it. Compute filters nothing itself.
func windowed(prs []PR, runs []Run, window Window) ([]PR, []Run) {
	var keptPRs []PR
	for _, pr := range prs {
		if !pr.MergedAt.Before(window.From) && pr.MergedAt.Before(window.To) {
			keptPRs = append(keptPRs, pr)
		}
	}
	var keptRuns []Run
	for _, run := range runs {
		if !run.StartedAt.Before(window.From) && run.StartedAt.Before(window.To) {
			keptRuns = append(keptRuns, run)
		}
	}
	return keptPRs, keptRuns
}

type dayRow struct {
	day     string
	partial bool
	deploys int
}

func dayRows(daily []DailyPoint) []dayRow {
	rows := make([]dayRow, len(daily))
	for i, point := range daily {
		rows[i] = dayRow{point.Day, point.Partial, point.Deploys}
	}
	return rows
}

func assertDayRows(t *testing.T, got []DailyPoint, want []dayRow) {
	t.Helper()
	rows := dayRows(got)
	if len(rows) != len(want) {
		t.Fatalf("daily = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("daily[%d] = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// TestDailySeriesFollowsABrushedWindow is dora.test.ts:155-166.
func TestDailySeriesFollowsABrushedWindow(t *testing.T) {
	allPRs, allRuns, _ := loadFixture(t)
	window := Window{From: time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC), To: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	prs, runs := windowed(allPRs, allRuns, window)
	result := Compute(prs, runs, FailureParts{}, window)
	assertDayRows(t, result.Daily, []dayRow{
		{"2026-09-05", true, 0},
		{"2026-09-06", false, 1},
		{"2026-09-07", false, 0},
		{"2026-09-08", false, 0},
		{"2026-09-09", true, 1},
	})
	assertFloat(t, "deploy_frequency.per_day", result.DeployFrequency.PerDay, 0.5026178010471204)
}

// TestAWindowEndingAtMidnightHasNoEmptyTrailingDay is dora.test.ts:168-176.
func TestAWindowEndingAtMidnightHasNoEmptyTrailingDay(t *testing.T) {
	allPRs, allRuns, _ := loadFixture(t)
	window := Window{From: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}
	prs, runs := windowed(allPRs, allRuns, window)
	assertDayRows(t, Compute(prs, runs, FailureParts{}, window).Daily, []dayRow{
		{"2026-09-06", false, 1},
		{"2026-09-07", false, 0},
		{"2026-09-08", false, 0},
	})
}

// TestQuantileIsLinearBetweenRanks is the prototype's collector/tests/test_pipeline.py:52-57.
func TestQuantileIsLinearBetweenRanks(t *testing.T) {
	values := []float64{10, 20, 30, 40, 60}
	assertFloat(t, "Quantile(0.9)", Quantile(values, 0.9), 52)
	assertFloat(t, "Quantile(0.5)", Quantile(values, 0.5), 30)
	assertFloat(t, "Quantile of one", Quantile([]float64{7}, 0.9), 7)
}

// TestQuantileRoundsEachStepOnEveryArchitecture: Quantile rounds its rank and the step it adds
// to a float64 each, as the prototype's Python and d3's JavaScript do, so a build that fuses a
// product with the subtraction or the sum it feeds into one instruction that rounds once (arm64's
// FNMSUBD and FMADDD, which the Go spec allows) answers what an amd64 build answers. Each case is
// a day of the Pipeline oracle's history (api/testdata/golden_delivery) whose p90 an arm64 build
// moved across the one-decimal boundary the Pipeline rounds to. Eleven production deploys: the
// rank 10 × 0.9 rounds to 9, so the p90 is the tenth value, 453 s (7.55 minutes, a float just
// below it), where the rank's unrounded fraction 2⁻⁵² lifts a fused p90 to the float above 7.55,
// 7.6. Fifty-four PR-check runs: the rank 53 × 0.9 rounds to 47.700000000000003, and the
// unrounded fraction falls short of it, so a fused p90 falls short of 22.35, 22.3.
func TestQuantileRoundsEachStepOnEveryArchitecture(t *testing.T) {
	minutes := func(seconds ...int) []float64 {
		values := make([]float64, len(seconds))
		for i, s := range seconds {
			values[i] = minutesBetween(time.Time{}, time.Time{}.Add(time.Duration(s)*time.Second))
		}
		return values
	}
	var checks []int
	for i := range 47 {
		checks = append(checks, 600+10*i)
	}
	checks = append(checks, 1334, 1344, 1400, 1450, 1500, 1550, 1600)
	cases := []struct {
		name   string
		sorted []float64
		p90    float64
		cell   float64
	}{
		{"eleven deploys", minutes(300, 310, 320, 330, 340, 350, 360, 370, 380, 453, 590), 7.55, 7.5},
		{"fifty-four runs", minutes(checks...), 22.35, 22.4},
	}
	for _, c := range cases {
		if got := Quantile(c.sorted, 0.9); got != c.p90 {
			t.Errorf("%s: Quantile(0.9) = %.17g, want %.17g", c.name, got, c.p90)
		}
		cell := FoldTiming([]DayPart{SamplesPart(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC), SeriesMinutes, c.sorted, nil)})
		if cell.P90Minutes == nil || *cell.P90Minutes != c.cell {
			t.Errorf("%s: FoldTiming's p90_minutes = %v, want %v", c.name, show(cell.P90Minutes), c.cell)
		}
	}
}
