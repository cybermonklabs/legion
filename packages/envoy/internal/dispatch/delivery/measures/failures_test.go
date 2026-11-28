package measures

import (
	"testing"
	"time"
)

var failureDay = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

func failureAt(day time.Time, hours int) time.Time { return day.Add(time.Duration(hours) * time.Hour) }

// A hand-built week folds to the DORA values: four deploys over two days, two of them with a
// counted failure (one with two failures, which counts once), one restored after 90 minutes and one
// after 30, and one deploy that restored a failure. Change fail rate is 2/4, recovery's median the
// middle of 30 and 90 minutes with nothing left unrestored, and rework rate 1/4.
func TestAWindowOfDeploysAndFailuresFoldsToTheDORAValues(t *testing.T) {
	second := failureDay.AddDate(0, 0, 1)
	dayOne := []FailureDeploy{{Failures: 2}, {}}
	dayTwo := []FailureDeploy{{Failures: 1}, {Restores: true}}
	var parts []DayPart
	parts = append(parts, ChangeFailRateDay(failureDay, dayOne)...)
	parts = append(parts, ChangeFailRateDay(second, dayTwo)...)
	parts = append(parts, ReworkRateDay(failureDay, dayOne)...)
	parts = append(parts, ReworkRateDay(second, dayTwo)...)
	parts = append(parts, RecoveryDay(failureDay, []RestoredFailure{{DeployFinishedAt: failureAt(failureDay, 9), RestoredAt: failureAt(failureDay, 9).Add(90 * time.Minute)}}, 0)...)
	parts = append(parts, RecoveryDay(second, []RestoredFailure{{DeployFinishedAt: failureAt(second, 8), RestoredAt: failureAt(second, 8).Add(30 * time.Minute)}}, 0)...)

	if rate := FoldChangeFailRate(parts); rate == nil || rate.Failed != 2 || rate.Total != 4 || rate.Rate == nil || *rate.Rate != 0.5 {
		t.Errorf("change fail rate = %+v, want 0.5 (2 of 4 deploys)", rate)
	}
	if rate := FoldReworkRate(parts); rate == nil || rate.Restoring != 1 || rate.Total != 4 || rate.Rate == nil || *rate.Rate != 0.25 {
		t.Errorf("rework rate = %+v, want 0.25 (1 of 4 deploys)", rate)
	}
	recovery := FoldRecovery(parts)
	if recovery == nil || recovery.N != 2 || recovery.MedianMinutes == nil || *recovery.MedianMinutes != 60 || recovery.NotRestored != 0 {
		t.Errorf("recovery = %+v, want 2 restores, a 60-minute median and none left", recovery)
	}
}

// A rejected failure is not counted, so its deploy leaves the numerator: change fail rate moves by
// exactly that deploy's share of the window's deploys.
func TestARejectMovesChangeFailRateByThatDeploysShare(t *testing.T) {
	counted := []FailureDeploy{{Failures: 1}, {Failures: 1}, {}, {}, {}}
	before := FoldChangeFailRate(ChangeFailRateDay(failureDay, counted))
	counted[1].Failures = 0 // the second deploy's one failure rejected
	after := FoldChangeFailRate(ChangeFailRateDay(failureDay, counted))
	if before == nil || after == nil || before.Failed-after.Failed != 1 || before.Total != after.Total || *before.Rate-*after.Rate != 1.0/5 {
		t.Errorf("change fail rate moved from %+v to %+v, want down by 1/5", before, after)
	}
}

// A day with deploys and no failure is a measured 0 in change fail rate and rework rate, and
// recovery has no median ("no data", never 0) while it keeps the count of failures not restored; a
// window with no part of a series reads no value at all.
func TestNoFailureIsZeroAndNothingRestoredIsNoMedian(t *testing.T) {
	deploys := []FailureDeploy{{}, {}}
	if rate := FoldChangeFailRate(ChangeFailRateDay(failureDay, deploys)); rate == nil || rate.Failed != 0 || rate.Total != 2 || rate.Rate == nil || *rate.Rate != 0 {
		t.Errorf("change fail rate with no failure = %+v, want 0 of 2", rate)
	}
	if rate := FoldReworkRate(ReworkRateDay(failureDay, deploys)); rate == nil || rate.Restoring != 0 || rate.Total != 2 || rate.Rate == nil || *rate.Rate != 0 {
		t.Errorf("rework rate with no restore = %+v, want 0 of 2", rate)
	}
	recovery := FoldRecovery(RecoveryDay(failureDay, nil, 3))
	if recovery == nil || recovery.MedianMinutes != nil || recovery.N != 0 || recovery.NotRestored != 3 {
		t.Errorf("recovery with three open failures = %+v, want no median and 3 not restored", recovery)
	}
	if FoldChangeFailRate(nil) != nil || FoldReworkRate(nil) != nil || FoldRecovery(nil) != nil {
		t.Errorf("a window with no row read a value")
	}
	if rate := FoldChangeFailRate(ChangeFailRateDay(failureDay, nil)); rate == nil || rate.Total != 0 || rate.Rate == nil || *rate.Rate != 0 {
		t.Errorf("a day row with no deploy = %+v, want a rate of 0, a measured value", rate)
	}
}
