package measures

import (
	"testing"
	"time"
)

// foldDays are the two UTC days the hand-built parts below are of.
var foldDays = []time.Time{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}

// minutesParts are two days' minutes, the first day's few and short and the second's many and
// long, so the quantile of their union and the mean of their two daily quantiles part ways: the
// union [1 2 3 10 20 30 40 100] has median 15 and p90 58, where the days' medians are 2 and 30.
func minutesParts(counts ...map[string]float64) []DayPart {
	count := func(day int) map[string]float64 {
		if day < len(counts) {
			return counts[day]
		}
		return nil
	}
	return []DayPart{
		{Series: SeriesMinutes, Day: foldDays[0], N: 3, Sum: new(6.0), Max: new(3.0), Samples: []float64{3, 1, 2}, Counts: count(0)},
		{Series: SeriesMinutes, Day: foldDays[1], N: 5, Sum: new(200.0), Max: new(100.0), Samples: []float64{100, 10, 40, 20, 30}, Counts: count(1)},
	}
}

// TestAQuantileFoldsTheMergeOfTheDaysSamples: FoldLeadTime, FoldTimeToRestore and FoldTiming read
// the quantile of every sample of the parts, not the mean of each day's quantile.
func TestAQuantileFoldsTheMergeOfTheDaysSamples(t *testing.T) {
	parts := minutesParts(
		map[string]float64{CountSuccess: 3, CountFailure: 0, CountCancelled: 1},
		map[string]float64{CountSuccess: 4, CountFailure: 1, CountCancelled: 0},
	)
	meanOfDailyMedians := (*FoldLeadTime(parts[:1]).MedianMinutes + *FoldLeadTime(parts[1:]).MedianMinutes) / 2
	if meanOfDailyMedians == 15 {
		t.Fatalf("the mean of the daily medians is %v, the union's median, so this test cannot tell them apart", meanOfDailyMedians)
	}

	lead := FoldLeadTime(parts)
	assertLeadTimeCell(t, "FoldLeadTime", lead, 8, new(15.0), new(58.0), new(100.0))
	assertOptional(t, "FoldTimeToRestore.median_minutes", FoldTimeToRestore(parts).MedianMinutes, new(15.0))
	assertCell(t, "FoldTiming", FoldTiming(parts), 8, new(15.0), new(58.0), 7, 1, 1, new(0.875))
	t.Logf("median of the merged samples %v, p90 %v; the mean of the two daily medians would read %v",
		*lead.MedianMinutes, *lead.P90Minutes, meanOfDailyMedians)
}

// TestAShareFoldsTheSummedNumeratorOverTheSummedDenominator: FoldDeployRunSuccess,
// FoldReworkShare and FoldChangeFailureRate read Σnum/Σden over the parts, not the mean of each
// day's share; a count beside a share is summed.
func TestAShareFoldsTheSummedNumeratorOverTheSummedDenominator(t *testing.T) {
	// One run of one concluded on the first day, one of nine on the second: 2/10 in all, where the
	// days' shares, 1 and 1/9, average 5/9.
	share := func(series string, counts ...map[string]float64) []DayPart {
		first, second := map[string]float64(nil), map[string]float64(nil)
		if len(counts) == 2 {
			first, second = counts[0], counts[1]
		}
		return []DayPart{
			{Series: series, Day: foldDays[0], N: 1, Num: new(1.0), Den: new(1.0), Counts: first},
			{Series: series, Day: foldDays[1], N: 9, Num: new(1.0), Den: new(9.0), Counts: second},
		}
	}
	const meanOfDailyShares = (1 + 1.0/9) / 2

	success := FoldDeployRunSuccess(share(SeriesRuns, map[string]float64{CountCancelled: 2}, map[string]float64{CountCancelled: 3}))
	if success == nil || success.Concluded != 10 || success.ReachedProduction != 2 || success.Cancelled != 5 {
		t.Fatalf("FoldDeployRunSuccess = %+v, want 2 of 10 concluded and 5 cancelled", success)
	}
	assertOptional(t, "FoldDeployRunSuccess.rate", success.Rate, new(0.2))
	assertOptional(t, "FoldReworkShare", FoldReworkShare(share(SeriesPRs)), new(0.2))

	failures := FoldChangeFailureRate(append(append(
		share(SeriesPerDeploy, map[string]float64{CountPending: 1}, map[string]float64{CountRejected: 2}),
		share(SeriesUpperBound)...),
		share(SeriesPerPR, map[string]float64{CountReverts: 1}, map[string]float64{CountPending: 4})...))
	if failures == nil {
		t.Fatal("FoldChangeFailureRate = nil, want the three shares")
	}
	perDeploy, perPR := failures.PerDeploy, failures.PerPR
	if perDeploy.Confirmed != 2 || perDeploy.Total != 10 || perDeploy.Pending != 1 || perDeploy.Rejected != 2 || perDeploy.ConfirmedOrPending != 2 || perDeploy.Reverts != 1 {
		t.Errorf("per_deploy = %+v, want 2 confirmed of 10 deploys, 1 pending, 2 rejected, 2 confirmed or pending, 1 revert", perDeploy)
	}
	if perPR.Confirmed != 2 || perPR.Total != 10 || perPR.Pending != 4 || perPR.Reverts != 1 {
		t.Errorf("per_pr = %+v, want 2 confirmed of 10 pull requests, 4 pending, 1 revert", perPR)
	}
	assertFloat(t, "per_deploy.rate", perDeploy.Rate, 0.2)
	assertFloat(t, "per_deploy.upper_bound_rate", perDeploy.UpperBoundRate, 0.2)
	assertFloat(t, "per_pr.rate", perPR.Rate, 0.2)
	t.Logf("summed share %v; the mean of the two daily shares would read %v", *success.Rate, meanOfDailyShares)
}

// TestACountFoldsTheSumOfTheDays: FoldDeployFrequency adds the days' deploys and the deploys that
// shipped a pull request, and divides each by the window's days.
func TestACountFoldsTheSumOfTheDays(t *testing.T) {
	parts := []DayPart{
		countPart(foldDays[0], SeriesDeploys, 3, map[string]float64{CountWithPRs: 1}),
		countPart(foldDays[1], SeriesDeploys, 5, map[string]float64{CountWithPRs: 4}),
	}
	got := FoldDeployFrequency(parts, Window{From: foldDays[0], To: foldDays[1].Add(day)})
	if got == nil || *got != (DeployFrequency{SuccessfulDeploys: 8, DeploysWithPRs: 5, PerDay: 4, WithPRsPerDay: 2.5}) {
		t.Errorf("FoldDeployFrequency = %+v, want 8 deploys, 5 with pull requests, 4 and 2.5 a day", got)
	}
}

// TestNoPartsReadNoValueAndAZeroPartReadsZero: a fold of no part of its series reads no value,
// never 0, whether the set is empty or holds only another series' parts; a zero part, a day built
// with no rows, reads zero counts, and a share over its zero denominator still reads no value.
func TestNoPartsReadNoValueAndAZeroPartReadsZero(t *testing.T) {
	window := Window{From: foldDays[0], To: foldDays[0].Add(day)}
	other := []DayPart{countPart(foldDays[0], "elsewhere", 4, nil), ratioPart(foldDays[0], "elsewhere", 1, 2, nil)}
	for name, parts := range map[string][]DayPart{"no part": nil, "another series' parts": other} {
		if got := FoldDeployFrequency(parts, window); got != nil {
			t.Errorf("%s: FoldDeployFrequency = %+v, want nil", name, got)
		}
		if got := FoldDeployRunSuccess(parts); got != nil {
			t.Errorf("%s: FoldDeployRunSuccess = %+v, want nil", name, got)
		}
		if got := FoldReworkShare(parts); got != nil {
			t.Errorf("%s: FoldReworkShare = %v, want nil", name, *got)
		}
		if got := FoldChangeFailureRate(parts); got != nil {
			t.Errorf("%s: FoldChangeFailureRate = %+v, want nil", name, got)
		}
		assertLeadTimeCell(t, name+": FoldLeadTime", FoldLeadTime(parts), 0, nil, nil, nil)
		assertOptional(t, name+": FoldTimeToRestore.median_minutes", FoldTimeToRestore(parts).MedianMinutes, nil)
		assertCell(t, name+": FoldTiming", FoldTiming(parts), 0, nil, nil, 0, 0, 0, nil)
		if got := FoldSlowestJobs(parts, []string{utcDay(foldDays[0])}); got == nil || len(got) != 0 {
			t.Errorf("%s: FoldSlowestJobs = %#v, want an empty list", name, got)
		}
	}

	if got := FoldDeployFrequency(DeployFrequencyDay(foldDays[0], nil), window); got == nil || *got != (DeployFrequency{}) {
		t.Errorf("FoldDeployFrequency of a zero part = %+v, want zero deploys", got)
	}
	if got := FoldDeployRunSuccess(DeployRunSuccessDay(foldDays[0], nil)); got == nil || *got != (DeployRunSuccess{}) {
		t.Errorf("FoldDeployRunSuccess of a zero part = %+v, want zero runs and no rate", got)
	}
	if got := FoldReworkShare(ReworkShareDay(foldDays[0], nil)); got != nil {
		t.Errorf("FoldReworkShare of a zero part = %v, want nil: no pull request has no share", *got)
	}
	assertLeadTimeCell(t, "FoldLeadTime of a zero part", FoldLeadTime(MergeToProductionDay(foldDays[0], nil)), 0, nil, nil, nil)
}
