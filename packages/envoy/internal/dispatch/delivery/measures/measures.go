// Package measures is the delivery measures' pure computation (LEGION-567 slice 2): deploys a day,
// lead time, change failure rate, time to restore, rework share, deploy-run success and their
// per-UTC-day series, judged against Sami's KPI targets. It ports the prototype's
// web/src/lib/dora.ts formula by formula, and each function cites the prototype lines it ports. It
// reads no settings, applies no facet and filters no window: the caller hands it the population
// PRs that survived the facets and the deploy runs its store query selected, and Compute counts
// exactly those. Each measure is a day builder and a fold over day parts (parts.go), which the
// eval routes call over stored parts too.
package measures

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

const day = 24 * time.Hour

// Window is [From, To): half-open, as GET /api/v1/delivery/timeline's window is.
type Window struct{ From, To time.Time }

// PR is what the day builders need of one population pull request, already facet-filtered.
type PR struct {
	ID            string // "owner/repo#N"
	CreatedAt     time.Time
	MergedAt      time.Time
	FirstCommitAt *time.Time
	DeployRunID   *int64
	DeployedAt    *time.Time // the production job's CompletedAt (delivery.Apply.CompletedAt)
	Rework        bool
}

// PROf is pr as the day builders read it, shipped by apply, the production apply that first ships
// it (delivery.ContainingRun), nil where none does yet: what the delivery and CI day rows and the
// eval items route build of one stored pull request. pr carries its opening and its merge; a
// partial row has no lead time to count, and its caller leaves it out.
func PROf(pr delivery.DeliveryPullRequest, apply *delivery.Apply) PR {
	measured := PR{
		ID: pr.Repo + "#" + strconv.Itoa(pr.Number), CreatedAt: *pr.CreatedAt, MergedAt: *pr.MergedAt,
		FirstCommitAt: pr.FirstCommitAt, Rework: pr.Rework,
	}
	if apply != nil {
		runID, completedAt := apply.RunID, apply.CompletedAt
		measured.DeployRunID, measured.DeployedAt = &runID, &completedAt
	}
	return measured
}

// Run is one deploy-workflow run on main started in the window (the store query applies the
// branch and window; the day builders filter nothing).
type Run struct {
	ID                int64
	StartedAt         time.Time
	Conclusion        *delivery.DeliveryRunConclusion
	ReachedProduction bool // the production job concluded success (delivery.ProductionApplies found it)
	// ShippedPRs is every population PR whose first shipping apply is this run, whenever it
	// merged and before facets (delivery.Deploys.ShippedBy).
	ShippedPRs int
}

// Flag is one failed-change flag against a population PR: the PR that broke production, the PR
// that fixed or reverted it, and whether the dora role holder has confirmed it.
type Flag struct {
	Broken, Fix string // PR ids
	Kind        string // "revert" | "fix"
	State       string // "pending" | "confirmed" | "rejected"
}

// Spread is a set of durations' median, 90th percentile and maximum, in minutes; all three are
// nil for an empty set.
type Spread struct {
	MedianMinutes *float64 `json:"median_minutes"`
	P90Minutes    *float64 `json:"p90_minutes"`
	MaxMinutes    *float64 `json:"max_minutes"`
}

// DailyPoint is one UTC day of the window; Partial when the window covers only part of it.
type DailyPoint struct {
	Day               string   `json:"day"` // YYYY-MM-DD, UTC
	Partial           bool     `json:"partial"`
	Deploys           int      `json:"deploys"`
	Concluded         int      `json:"concluded"`
	ReachedProduction int      `json:"reached_production"`
	Cancelled         int      `json:"cancelled"`
	RunSuccessRate    *float64 `json:"run_success_rate"`
}

// FlagCounts is the change failure rate's flags by state, counted against PRs or against deploys.
type FlagCounts struct {
	Confirmed int     `json:"confirmed"`
	Pending   int     `json:"pending"`
	Rejected  int     `json:"rejected"`
	Reverts   int     `json:"reverts"`
	Total     int     `json:"total"`
	Rate      float64 `json:"rate"`
}

// DeployFlagCounts is the per-deploy change failure rate, with the rate it would be if every
// pending flag were confirmed: ConfirmedOrPending counts each deploy once.
type DeployFlagCounts struct {
	FlagCounts
	ConfirmedOrPending int     `json:"confirmed_or_pending"`
	UpperBoundRate     float64 `json:"upper_bound_rate"`
}

// DeployFrequency counts the window's successful production deploys, and those that shipped at
// least one population PR, in total and per day.
type DeployFrequency struct {
	SuccessfulDeploys int     `json:"successful_deploys"`
	DeploysWithPRs    int     `json:"deploys_with_prs"`
	PerDay            float64 `json:"per_day"`
	WithPRsPerDay     float64 `json:"with_prs_per_day"`
}

// LeadTime is the four lead-time spreads: merge, first commit and open to production over the
// deployed PRs, and open to merge over every PR.
type LeadTime struct {
	MergeToProduction       Spread `json:"merge_to_production"`
	FirstCommitToProduction Spread `json:"first_commit_to_production"`
	OpenedToProduction      Spread `json:"opened_to_production"`
	OpenedToMerge           Spread `json:"opened_to_merge"`
}

// ChangeFailureRate is the flags counted against PRs and against deploys.
type ChangeFailureRate struct {
	PerPR     FlagCounts       `json:"per_pr"`
	PerDeploy DeployFlagCounts `json:"per_deploy"`
}

// TimeToRestore is the median minutes from a broken PR's deploy to its fix's, over confirmed
// flags whose two PRs both deployed; nil when there is none.
type TimeToRestore struct {
	MedianMinutes *float64 `json:"median_minutes"`
}

// DeployRunSuccess is the deploy runs that concluded success or failure, how many of those reached
// production, the cancelled runs beside them, and the rate; Rate is nil when none concluded.
type DeployRunSuccess struct {
	Concluded         int      `json:"concluded"`
	ReachedProduction int      `json:"reached_production"`
	Cancelled         int      `json:"cancelled"`
	Rate              *float64 `json:"rate"`
}

// Result is every number the measures panel and GET /api/v1/delivery/measures show for one window
// and population. ChangeFailureRate is nil, and TimeToRestore holds no median, while no failure
// source exists (Compute's flags nil): 0 is a measured value and never a stand-in for one, so a
// rate nothing measured reads no value rather than a 0 a target would judge met.
type Result struct {
	DeployFrequency   DeployFrequency    `json:"deploy_frequency"`
	LeadTime          LeadTime           `json:"lead_time"`
	ChangeFailureRate *ChangeFailureRate `json:"change_failure_rate"`
	TimeToRestore     TimeToRestore      `json:"time_to_restore"`
	ReworkShare       float64            `json:"rework_share"`
	DeployRunSuccess  DeployRunSuccess   `json:"deploy_run_success"`
	Daily             []DailyPoint       `json:"daily"`
}

// Targets are Sami's engineering targets (the prototype's KPI_TARGETS, CONTRACT.md "KPI targets",
// set 2026-09-28): "under" is strictly below, "at least" is >=.
type Targets struct {
	DeploysPerDay              float64 `json:"deploys_per_day"`                // at least
	ChangeFailureRate          float64 `json:"change_failure_rate"`            // under, per deploy
	MergeToProductionMinutes   float64 `json:"merge_to_production_minutes"`    // under, for every change (the max)
	OpenedToMergeMedianMinutes float64 `json:"opened_to_merge_median_minutes"` // under, the median
	DeployRunSuccessRate       float64 `json:"deploy_run_success_rate"`        // at least
	UnownedP0                  int     `json:"unowned_p0"`                     // exactly
}

// DefaultTargets are the targets every measures response is judged against (the prototype's
// web/src/lib/dora.ts:11-18).
var DefaultTargets = Targets{
	DeploysPerDay:              20,
	ChangeFailureRate:          0.05,
	MergeToProductionMinutes:   45,
	OpenedToMergeMedianMinutes: 60,
	DeployRunSuccessRate:       0.9,
	UnownedP0:                  0,
}

// Status is whether each KPI meets its target; nil when the window has nothing to measure.
type Status struct {
	DeploysPerDay               bool  `json:"deploys_per_day"`
	ChangeFailureRate           *bool `json:"change_failure_rate"`
	ChangeFailureRateUpperBound *bool `json:"change_failure_rate_upper_bound"`
	MergeToProductionMedian     *bool `json:"merge_to_production_median"`
	MergeToProduction           *bool `json:"merge_to_production"`
	OpenedToMerge               *bool `json:"opened_to_merge"`
	DeployRunSuccess            *bool `json:"deploy_run_success"`
	UnownedP0                   bool  `json:"unowned_p0"`
}

// Quantile is p's quantile of ascending, non-empty sorted, linear between the two nearest ranks:
// d3's quantileSorted, as the prototype's collector/src/collector/pipeline.py:69-77 writes it.
func Quantile(sorted []float64, p float64) float64 {
	i := float64(len(sorted)-1) * p
	lo := int(math.Floor(i))
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (sorted[lo+1]-sorted[lo])*(i-float64(lo))
}

// minutesBetween is b - a in minutes, one division of whole nanoseconds, so a duration the
// prototype computes from whole milliseconds (dora.ts:25-27) comes out to the same float.
func minutesBetween(a, b time.Time) float64 {
	return float64(b.Sub(a)) / float64(time.Minute)
}

// concluded is a run that concluded success or failure: the run-success denominator. A cancelled
// run is not a deploy attempt (dora.ts:61-63).
func concluded(run Run) bool {
	return run.Conclusion != nil &&
		(*run.Conclusion == delivery.DeliveryRunConclusionSuccess || *run.Conclusion == delivery.DeliveryRunConclusionFailure)
}

func cancelled(run Run) bool {
	return run.Conclusion != nil && *run.Conclusion == delivery.DeliveryRunConclusionCancelled
}

// Midnight is the UTC midnight of the UTC day t is on: the day a day part, a day row and an eval
// window's day are of.
func Midnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// windowDays is the UTC midnight of every day window covers, oldest first, and always at least
// one: a window ending exactly at midnight covers none of the day that starts then.
func windowDays(window Window) []time.Time {
	first := Midnight(window.From)
	last := Midnight(window.To)
	if last.Equal(window.To) {
		last = last.Add(-day)
	}
	if last.Before(first) {
		last = first
	}
	var days []time.Time
	for d := first; !d.After(last); d = d.Add(day) {
		days = append(days, d)
	}
	return days
}

// The delivery measures' day builders. Each turns one UTC day's rows into its eval's parts of that
// day: the population pull requests merged that day, the deploy runs on main started that day, and
// the failed-change flags that count on that day (JudgedFlag.Day). Two kinds of row are not
// settled when their day is first built, and the day is built again (by the eval_days refresher,
// for the stored parts) when the run that settles them lands: a pull request with no deploy yet
// adds nothing to the lead times that end in production, and a deploy run still running neither
// reached production nor concluded, and the pull requests it will ship count on no run
// (Run.ShippedPRs) until it does.

// DeployFrequencyDay is deploy-frequency's part of one UTC day, from the deploy runs started that
// day (dora.ts:169-174): the runs that reached production, a cancelled one included, with how many
// of those shipped a pull request in CountWithPRs. A deploy shipped a pull request when it is the
// first shipping apply of any population pull request, whenever that pull request merged
// (Run.ShippedPRs): with_prs is the deploy's own, so the delivery routes and the day rows count
// one deploy alike in every window that holds it. Counting only the pull requests merged inside
// the window would count a deploy that shipped only pull requests merged before a window as one
// with none in that window.
func DeployFrequencyDay(day time.Time, runs []Run) []DayPart {
	successful, withPRs := 0, 0
	for _, run := range runs {
		if run.ReachedProduction {
			successful++
			if run.ShippedPRs > 0 {
				withPRs++
			}
		}
	}
	return []DayPart{CountPart(day, SeriesDeploys, successful, map[string]float64{CountWithPRs: float64(withPRs)})}
}

// DeployRunSuccessDay is deploy-run-success's part of one UTC day, from the deploy runs started
// that day (dora.ts:218-223, 253-258): the runs that concluded success or failure and reached
// production, over those that concluded, with the cancelled runs, which are no deploy attempt, in
// CountCancelled.
func DeployRunSuccessDay(day time.Time, runs []Run) []DayPart {
	concludedRuns, reached, cancelledRuns := 0, 0, 0
	for _, run := range runs {
		if concluded(run) {
			concludedRuns++
			if run.ReachedProduction {
				reached++
			}
		}
		if cancelled(run) {
			cancelledRuns++
		}
	}
	return []DayPart{RatioPart(day, SeriesRuns, reached, concludedRuns, map[string]float64{CountCancelled: float64(cancelledRuns)})}
}

// MergeToProductionDay is merge-to-production's part of one UTC day, from the population pull
// requests merged that day: the minutes from each deployed one's merge to its deploy
// (dora.ts:176-184).
func MergeToProductionDay(day time.Time, prs []PR) []DayPart {
	return leadTimeDay(day, prs, func(pr PR) (float64, bool) {
		if pr.DeployedAt == nil {
			return 0, false
		}
		return minutesBetween(pr.MergedAt, *pr.DeployedAt), true
	})
}

// FirstCommitToProductionDay is first-commit-to-production's part of one UTC day, from the
// population pull requests merged that day: the minutes from each deployed one's first commit to
// its deploy, where its first commit is known (dora.ts:176-184).
func FirstCommitToProductionDay(day time.Time, prs []PR) []DayPart {
	return leadTimeDay(day, prs, func(pr PR) (float64, bool) {
		if pr.DeployedAt == nil || pr.FirstCommitAt == nil {
			return 0, false
		}
		return minutesBetween(*pr.FirstCommitAt, *pr.DeployedAt), true
	})
}

// OpenToProductionDay is open-to-production's part of one UTC day, from the population pull
// requests merged that day: the minutes from each deployed one's opening to its deploy
// (dora.ts:176-184).
func OpenToProductionDay(day time.Time, prs []PR) []DayPart {
	return leadTimeDay(day, prs, func(pr PR) (float64, bool) {
		if pr.DeployedAt == nil {
			return 0, false
		}
		return minutesBetween(pr.CreatedAt, *pr.DeployedAt), true
	})
}

// OpenToMergeDay is pr-open-to-merge's part of one UTC day, from the population pull requests
// merged that day: the minutes from each one's opening to its merge, deployed or not
// (dora.ts:176-184).
func OpenToMergeDay(day time.Time, prs []PR) []DayPart {
	return leadTimeDay(day, prs, func(pr PR) (float64, bool) {
		return minutesBetween(pr.CreatedAt, pr.MergedAt), true
	})
}

// leadTimeDay is a lead time's part of day: the minutes of every pull request minutesOf times.
func leadTimeDay(day time.Time, prs []PR, minutesOf func(PR) (float64, bool)) []DayPart {
	var minutes []float64
	for _, pr := range prs {
		if value, timed := minutesOf(pr); timed {
			minutes = append(minutes, value)
		}
	}
	return []DayPart{SamplesPart(day, SeriesMinutes, minutes, nil)}
}

// ReworkShareDay is rework-share's part of one UTC day, from the population pull requests merged
// that day (dora.ts:215, 252): the rework ones over every one.
func ReworkShareDay(day time.Time, prs []PR) []DayPart {
	rework := 0
	for _, pr := range prs {
		if pr.Rework {
			rework++
		}
	}
	return []DayPart{RatioPart(day, SeriesPRs, rework, len(prs), nil)}
}

// JudgedFlag is a Flag whose broken PR is a population PR, with the two PRs it names as the
// change-failure parts read them; Fix is nil when the fix is not a population PR.
type JudgedFlag struct {
	Flag
	Broken PR
	Fix    *PR
}

// Day is the UTC day a flag counts on: the day its broken PR deployed, so every flag of one deploy
// counts on one day and the deploy counts once however many flags name it, or, while it has not
// deployed, the day it merged.
func (f JudgedFlag) Day() time.Time {
	if f.Broken.DeployedAt != nil {
		return Midnight(*f.Broken.DeployedAt)
	}
	return Midnight(f.Broken.MergedAt)
}

// judgeFlags is each of flags whose broken PR is among prs, with its two PRs: a flag counts
// against the population only when its broken PR is in it (dora.ts:186-204).
func judgeFlags(prs []PR, flags []Flag) []JudgedFlag {
	if len(flags) == 0 {
		return nil
	}
	byID := make(map[string]PR, len(prs))
	for _, pr := range prs {
		byID[pr.ID] = pr
	}
	var judged []JudgedFlag
	for _, flag := range flags {
		broken, ok := byID[flag.Broken]
		if !ok {
			continue
		}
		flagged := JudgedFlag{Flag: flag, Broken: broken}
		if fix, ok := byID[flag.Fix]; ok {
			flagged.Fix = &fix
		}
		judged = append(judged, flagged)
	}
	return judged
}

// ChangeFailureRateDay is change-fail-rate's three parts of one UTC day (dora.ts:186-204,
// 230-250), from the population pull requests merged that day, the deploy runs started that day
// and the flags that count on it: per deploy, the deploys that shipped a confirmed flag's broken
// PR, each once, over the successful deploys, with the deploys of pending and of rejected flags in
// counts; the upper bound, the deploys of a confirmed or a pending flag, each once, over the same
// deploys; and per PR, the confirmed flags over the pull requests, with the pending, rejected and
// confirmed revert flags in counts.
func ChangeFailureRateDay(day time.Time, prs []PR, runs []Run, flags []JudgedFlag) []DayPart {
	successful := 0
	for _, run := range runs {
		if run.ReachedProduction {
			successful++
		}
	}
	var confirmed, pending, rejected, reverts int
	confirmedDeploys, pendingDeploys, rejectedDeploys := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, flag := range flags {
		var deploys map[int64]bool
		switch flag.State {
		case "confirmed":
			confirmed++
			if flag.Kind == "revert" {
				reverts++
			}
			deploys = confirmedDeploys
		case "pending":
			pending++
			deploys = pendingDeploys
		case "rejected":
			rejected++
			deploys = rejectedDeploys
		default:
			continue
		}
		if flag.Broken.DeployRunID != nil {
			deploys[*flag.Broken.DeployRunID] = true
		}
	}
	confirmedOrPending := len(confirmedDeploys)
	for run := range pendingDeploys {
		if !confirmedDeploys[run] {
			confirmedOrPending++
		}
	}
	return []DayPart{
		RatioPart(day, SeriesPerDeploy, len(confirmedDeploys), successful, map[string]float64{
			CountPending: float64(len(pendingDeploys)), CountRejected: float64(len(rejectedDeploys)),
		}),
		RatioPart(day, SeriesUpperBound, confirmedOrPending, successful, nil),
		RatioPart(day, SeriesPerPR, confirmed, len(prs), map[string]float64{
			CountPending: float64(pending), CountRejected: float64(rejected), CountReverts: float64(reverts),
		}),
	}
}

// TimeToRestoreDay is failed-deploy-recovery's part of one UTC day (dora.ts:206-213), from the
// flags that count on it: the minutes from each confirmed flag's broken PR's deploy to its fix's,
// where the fix is a population PR and both deployed.
func TimeToRestoreDay(day time.Time, flags []JudgedFlag) []DayPart {
	var minutes []float64
	for _, flag := range flags {
		if flag.State == "confirmed" && flag.Fix != nil && flag.Broken.DeployedAt != nil && flag.Fix.DeployedAt != nil {
			minutes = append(minutes, minutesBetween(*flag.Broken.DeployedAt, *flag.Fix.DeployedAt))
		}
	}
	return []DayPart{SamplesPart(day, SeriesMinutes, minutes, nil)}
}

// The delivery measures' folds: each answers its eval's number from any set of its parts.

// FoldDeployFrequency is deploy-frequency over parts: the successful deploys and those that
// shipped a pull request, in total and per day of window (dora.ts:169-174, 253); nil when parts
// hold no SeriesDeploys part.
func FoldDeployFrequency(parts []DayPart, window Window) *DeployFrequency {
	deploys := FoldSeries(parts, SeriesDeploys)
	if deploys.Parts == 0 {
		return nil
	}
	windowDays := float64(window.To.Sub(window.From)) / float64(day)
	withPRs := deploys.Counts[CountWithPRs]
	return &DeployFrequency{
		SuccessfulDeploys: int(deploys.Sum),
		DeploysWithPRs:    int(withPRs),
		PerDay:            deploys.Sum / windowDays,
		WithPRsPerDay:     withPRs / windowDays,
	}
}

// FoldDeployRunSuccess is deploy-run-success over parts: the runs that concluded, how many of
// those reached production, the cancelled runs, and the summed reached over the summed concluded,
// nil when none concluded (dora.ts:258); nil when parts hold no SeriesRuns part.
func FoldDeployRunSuccess(parts []DayPart) *DeployRunSuccess {
	runs := FoldSeries(parts, SeriesRuns)
	if runs.Parts == 0 {
		return nil
	}
	return &DeployRunSuccess{
		Concluded:         int(runs.Den),
		ReachedProduction: int(runs.Num),
		Cancelled:         int(runs.Counts[CountCancelled]),
		Rate:              runs.share(),
	}
}

// FoldLeadTime is a lead time over parts (merge-to-production, first-commit-to-production,
// open-to-production and pr-open-to-merge): how many pull requests it times, and the median, 90th
// percentile and maximum of their minutes, the merge of every part's samples (dora.ts:48-56,
// pipeline.ts:21-29), unrounded; the three are nil when no part holds a sample.
func FoldLeadTime(parts []DayPart) LeadTimeCell {
	sorted := MergedSamples(parts, SeriesMinutes)
	cell := LeadTimeCell{N: len(sorted)}
	if len(sorted) == 0 {
		return cell
	}
	cell.MedianMinutes = new(Quantile(sorted, 0.5))
	cell.P90Minutes = new(Quantile(sorted, 0.9))
	cell.MaxMinutes = new(sorted[len(sorted)-1])
	return cell
}

// FoldReworkShare is rework-share over parts: the summed rework pull requests over the summed
// pull requests; nil when parts count none.
func FoldReworkShare(parts []DayPart) *float64 {
	return FoldSeries(parts, SeriesPRs).share()
}

// FoldChangeFailureRate is change-fail-rate over parts: the flags counted against pull requests
// and against deploys, each rate its summed numerator over its summed denominator, 0 when that
// denominator is (the measures panel's rates are never null); nil when parts hold none of its
// three series.
func FoldChangeFailureRate(parts []DayPart) *ChangeFailureRate {
	perDeploy := FoldSeries(parts, SeriesPerDeploy)
	upperBound := FoldSeries(parts, SeriesUpperBound)
	perPR := FoldSeries(parts, SeriesPerPR)
	if perDeploy.Parts+upperBound.Parts+perPR.Parts == 0 {
		return nil
	}
	reverts := int(perPR.Counts[CountReverts])
	return &ChangeFailureRate{
		PerPR: FlagCounts{
			Confirmed: int(perPR.Num),
			Pending:   int(perPR.Counts[CountPending]),
			Rejected:  int(perPR.Counts[CountRejected]),
			Reverts:   reverts,
			Total:     int(perPR.Den),
			Rate:      ValueOrZero(perPR.share()),
		},
		PerDeploy: DeployFlagCounts{
			FlagCounts: FlagCounts{
				Confirmed: int(perDeploy.Num),
				Pending:   int(perDeploy.Counts[CountPending]),
				Rejected:  int(perDeploy.Counts[CountRejected]),
				Reverts:   reverts,
				Total:     int(perDeploy.Den),
				Rate:      ValueOrZero(perDeploy.share()),
			},
			ConfirmedOrPending: int(upperBound.Num),
			UpperBoundRate:     ValueOrZero(upperBound.share()),
		},
	}
}

// FoldTimeToRestore is failed-deploy-recovery over parts: the median of every part's minutes
// (dora.ts:206-213, 251), whose MedianMinutes is nil when no part holds a sample.
func FoldTimeToRestore(parts []DayPart) TimeToRestore {
	sorted := MergedSamples(parts, SeriesMinutes)
	if len(sorted) == 0 {
		return TimeToRestore{}
	}
	return TimeToRestore{MedianMinutes: new(Quantile(sorted, 0.5))}
}

// DailySeries is the window's per-UTC-day deploys and run success, oldest first, every day of the
// window present (the prototype's dora.ts:83-117): each day's deploy-frequency and
// deploy-run-success parts folded alone, by FoldDeployFrequency and FoldDeployRunSuccess. A window
// ending exactly at midnight covers none of the day that starts then; a part of a day the window
// does not cover is in no point, and a day with no part reads zero (Compute builds a part for
// every day of its window).
func DailySeries(deploys, runs []DayPart, window Window) []DailyPoint {
	deploysByDay, runsByDay := partsByDay(deploys), partsByDay(runs)
	var points []DailyPoint
	for _, d := range windowDays(window) {
		point := DailyPoint{
			Day:     utcDay(d),
			Partial: d.Before(window.From) || d.Add(day-time.Millisecond).After(window.To),
		}
		if frequency := FoldDeployFrequency(deploysByDay[point.Day], Window{From: d, To: d.Add(day)}); frequency != nil {
			point.Deploys = frequency.SuccessfulDeploys
		}
		if success := FoldDeployRunSuccess(runsByDay[point.Day]); success != nil {
			point.Concluded, point.ReachedProduction, point.Cancelled = success.Concluded, success.ReachedProduction, success.Cancelled
			point.RunSuccessRate = success.Rate
		}
		points = append(points, point)
	}
	return points
}

// deliveryRows is one UTC day's rows of the delivery measures, as the day builders read them.
type deliveryRows struct {
	day   time.Time
	prs   []PR
	runs  []Run
	flags []JudgedFlag
}

// deliveryDays is the rows of every UTC day window covers, each day present whether it holds a
// row or not, and of any other day a row is on, oldest first: each pull request on its merge day,
// each run on its start day, the field its store query filters on, and each flag on its
// JudgedFlag.Day.
func deliveryDays(prs []PR, runs []Run, flags []Flag, window Window) []*deliveryRows {
	byDay := map[time.Time]*deliveryRows{}
	rowsOn := func(t time.Time) *deliveryRows {
		d := Midnight(t)
		if byDay[d] == nil {
			byDay[d] = &deliveryRows{day: d}
		}
		return byDay[d]
	}
	for _, d := range windowDays(window) {
		rowsOn(d)
	}
	for _, pr := range prs {
		rows := rowsOn(pr.MergedAt)
		rows.prs = append(rows.prs, pr)
	}
	for _, run := range runs {
		rows := rowsOn(run.StartedAt)
		rows.runs = append(rows.runs, run)
	}
	for _, flag := range judgeFlags(prs, flags) {
		rows := rowsOn(flag.Day())
		rows.flags = append(rows.flags, flag)
	}
	return slices.SortedFunc(maps.Values(byDay), func(a, b *deliveryRows) int { return a.day.Compare(b.day) })
}

// Compute is every measure of the window over prs and runs (the prototype's dora.ts:163-266
// computeDora), built and folded: the rows are bucketed by UTC day (deliveryDays), each day's
// parts built by the day builders, and the window's parts folded by the same folds an eval read
// folds stored parts with. Every run counts and every PR counts: the caller's query applied the
// window and branch to runs, and the facets to prs.
//
// flags is a failure source's flags, nil where no failure source exists. With none, no part of
// change-fail-rate or failed-deploy-recovery is built, as no day row of either is stored, so the
// two read no value through their folds (a nil ChangeFailureRate and no median to restore),
// exactly as their evals read them; a source that holds no flag (an empty, non-nil list) measures
// a rate of 0.
func Compute(prs []PR, runs []Run, flags []Flag, window Window) Result {
	var frequency, success, mergeToProduction, firstCommitToProduction, openedToProduction, openedToMerge, rework, changeFailure, restore []DayPart
	for _, rows := range deliveryDays(prs, runs, flags, window) {
		frequency = append(frequency, DeployFrequencyDay(rows.day, rows.runs)...)
		success = append(success, DeployRunSuccessDay(rows.day, rows.runs)...)
		mergeToProduction = append(mergeToProduction, MergeToProductionDay(rows.day, rows.prs)...)
		firstCommitToProduction = append(firstCommitToProduction, FirstCommitToProductionDay(rows.day, rows.prs)...)
		openedToProduction = append(openedToProduction, OpenToProductionDay(rows.day, rows.prs)...)
		openedToMerge = append(openedToMerge, OpenToMergeDay(rows.day, rows.prs)...)
		rework = append(rework, ReworkShareDay(rows.day, rows.prs)...)
		if flags != nil {
			changeFailure = append(changeFailure, ChangeFailureRateDay(rows.day, rows.prs, rows.runs, rows.flags)...)
			restore = append(restore, TimeToRestoreDay(rows.day, rows.flags)...)
		}
	}
	spread := func(parts []DayPart) Spread {
		cell := FoldLeadTime(parts)
		return Spread{MedianMinutes: cell.MedianMinutes, P90Minutes: cell.P90Minutes, MaxMinutes: cell.MaxMinutes}
	}
	// deliveryDays answers at least one day, the window's, and each day has a part of every
	// series but the two failure measures', so no other fold below reads "no data".
	return Result{
		DeployFrequency: *FoldDeployFrequency(frequency, window),
		LeadTime: LeadTime{
			MergeToProduction:       spread(mergeToProduction),
			FirstCommitToProduction: spread(firstCommitToProduction),
			OpenedToProduction:      spread(openedToProduction),
			OpenedToMerge:           spread(openedToMerge),
		},
		ChangeFailureRate: FoldChangeFailureRate(changeFailure),
		TimeToRestore:     FoldTimeToRestore(restore),
		// The panel's share reads 0 for a window with no pull request (dora.ts:215).
		ReworkShare:      ValueOrZero(FoldReworkShare(rework)),
		DeployRunSuccess: *FoldDeployRunSuccess(success),
		Daily:            DailySeries(frequency, success, window),
	}
}

// KPIStatus judges result and the count of unowned P0 issues against targets (the prototype's
// dora.ts:280-295): "at least" targets are met at the target, "under" targets are missed at it,
// and a measure with nothing to measure is nil rather than met: a change failure rate with no
// value (no failure source) or over no deploy among them.
func KPIStatus(result Result, unownedP0 int, targets Targets) Status {
	under := func(value *float64, target float64) *bool {
		if value == nil {
			return nil
		}
		met := *value < target
		return &met
	}
	var cfrMet, cfrUpperMet *bool
	if result.ChangeFailureRate != nil && result.ChangeFailureRate.PerDeploy.Total > 0 {
		cfr := result.ChangeFailureRate.PerDeploy
		cfrMet = under(&cfr.Rate, targets.ChangeFailureRate)
		cfrUpperMet = under(&cfr.UpperBoundRate, targets.ChangeFailureRate)
	}
	var runSuccessMet *bool
	if rate := result.DeployRunSuccess.Rate; rate != nil {
		met := *rate >= targets.DeployRunSuccessRate
		runSuccessMet = &met
	}
	merge := result.LeadTime.MergeToProduction
	return Status{
		DeploysPerDay:               result.DeployFrequency.PerDay >= targets.DeploysPerDay,
		ChangeFailureRate:           cfrMet,
		ChangeFailureRateUpperBound: cfrUpperMet,
		MergeToProductionMedian:     under(merge.MedianMinutes, targets.MergeToProductionMinutes),
		MergeToProduction:           under(merge.MaxMinutes, targets.MergeToProductionMinutes),
		OpenedToMerge:               under(result.LeadTime.OpenedToMerge.MedianMinutes, targets.OpenedToMergeMedianMinutes),
		DeployRunSuccess:            runSuccessMet,
		UnownedP0:                   unownedP0 == targets.UnownedP0,
	}
}
