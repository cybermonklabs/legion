package measures

import (
	"time"
)

// The three failure measures (dispatch://INFRA-978, D7): change-fail-rate, failed-deploy-recovery
// and deployment-rework-rate, built from the record of deploys and failed changes
// (production_events) for one project and one UTC day. A failure is a fact about its deploy: it
// counts on the day its deploy finished, for each project its deploy counts for whose failure
// sources include its signal, and not at all once rejected. The builders take the rows their caller
// already counted for the project, so they read no project and no source.

// SeriesReworkDeploys is deployment-rework-rate's: the deploys that restored a counted failure over
// every deploy of the day (a share). It is spelled as deploy-frequency's sum is, since each eval
// names its own series.
const SeriesReworkDeploys = "deploys"

// CountNotRestored is failed-deploy-recovery's count of the counted failures of the day's deploys
// that no restore has closed yet.
const CountNotRestored = "not_restored"

// FailureDeploy is one deploy row that finished on the day and counts for the project: how many
// counted failures blame it, and whether it restored a counted failure (it is the
// restored_by_deploy of one: a revert's own deploy, or the one a person names with the drill-down's
// Mark restored).
type FailureDeploy struct {
	Failures int
	Restores bool
}

// RestoredFailure is one counted failure restored on the day: the moment its deploy finished and
// the moment it was restored.
type RestoredFailure struct {
	DeployFinishedAt time.Time
	RestoredAt       time.Time
}

// ChangeFailRateDay is change-fail-rate's part of one UTC day: the deploys that finished on it with
// at least one counted failure, over every deploy that finished on it.
func ChangeFailRateDay(day time.Time, deploys []FailureDeploy) []DayPart {
	failed := 0
	for _, deploy := range deploys {
		if deploy.Failures > 0 {
			failed++
		}
	}
	return []DayPart{RatioPart(day, SeriesPerDeploy, failed, len(deploys), nil)}
}

// RecoveryDay is failed-deploy-recovery's part of one UTC day: the minutes from each restored
// failure's deploy to its restore (RestoreMinutes), for the failures restored on the day, beside
// the count of the counted failures of the day's deploys that are not restored yet, which is
// rewritten when one is.
func RecoveryDay(day time.Time, restored []RestoredFailure, notRestored int) []DayPart {
	minutes := make([]float64, 0, len(restored))
	for _, failure := range restored {
		minutes = append(minutes, RestoreMinutes(failure))
	}
	return []DayPart{SamplesPart(day, SeriesMinutes, minutes, map[string]float64{CountNotRestored: float64(notRestored)})}
}

// RestoreMinutes is the minutes from failure's deploy's finish to its restore: the sample
// RecoveryDay counts for it.
func RestoreMinutes(failure RestoredFailure) float64 {
	return minutesBetween(failure.DeployFinishedAt, failure.RestoredAt)
}

// ReworkRateDay is deployment-rework-rate's part of one UTC day: the deploys that finished on it
// and restored a counted failure, over every deploy that finished on it.
func ReworkRateDay(day time.Time, deploys []FailureDeploy) []DayPart {
	restoring := 0
	for _, deploy := range deploys {
		if deploy.Restores {
			restoring++
		}
	}
	return []DayPart{RatioPart(day, SeriesReworkDeploys, restoring, len(deploys), nil)}
}

// Recovery is failed-deploy-recovery over a window: how many restores it times, their median
// minutes (nil when none was restored, "no data", never 0), and the counted failures not restored.
type Recovery struct {
	N             int      `json:"n"`
	MedianMinutes *float64 `json:"median_minutes"`
	NotRestored   int      `json:"not_restored"`
}

// ChangeFailRate is change-fail-rate over a window: the deploys with at least one counted failure,
// every deploy, and the first over the second, 0 where the window counts no deploy, so a measured
// 0 stays 0.
type ChangeFailRate struct {
	Failed int      `json:"failed"`
	Total  int      `json:"total"`
	Rate   *float64 `json:"rate"`
}

// ReworkRate is deployment-rework-rate over a window: the deploys that restored a counted failure,
// every deploy, and the first over the second, 0 where the window counts no deploy.
type ReworkRate struct {
	Restoring int      `json:"restoring"`
	Total     int      `json:"total"`
	Rate      *float64 `json:"rate"`
}

// FoldChangeFailRate is change-fail-rate over parts: the summed failed deploys over the summed
// deploys; nil when parts hold no part of the series (no row: "no data").
func FoldChangeFailRate(parts []DayPart) *ChangeFailRate {
	folded := FoldSeries(parts, SeriesPerDeploy)
	if folded.Parts == 0 {
		return nil
	}
	return &ChangeFailRate{Failed: int(folded.Num), Total: int(folded.Den), Rate: new(ValueOrZero(folded.share()))}
}

// FoldReworkRate is deployment-rework-rate over parts, as FoldChangeFailRate folds its series.
func FoldReworkRate(parts []DayPart) *ReworkRate {
	folded := FoldSeries(parts, SeriesReworkDeploys)
	if folded.Parts == 0 {
		return nil
	}
	return &ReworkRate{Restoring: int(folded.Num), Total: int(folded.Den), Rate: new(ValueOrZero(folded.share()))}
}

// FoldRecovery is failed-deploy-recovery over parts: the median of every part's samples and the
// summed failures not restored; nil when parts hold no part of the series.
func FoldRecovery(parts []DayPart) *Recovery {
	folded := FoldSeries(parts, SeriesMinutes)
	if folded.Parts == 0 {
		return nil
	}
	sorted := MergedSamples(parts, SeriesMinutes)
	recovery := &Recovery{N: sorted.Len(), NotRestored: int(folded.Counts[CountNotRestored])}
	if sorted.Len() > 0 {
		recovery.MedianMinutes = new(sorted.Quantile(0.5))
	}
	return recovery
}
