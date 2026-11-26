// Package measures is the delivery measures' pure computation (LEGION-567 slice 2): deploys a day,
// lead time, change failure rate, time to restore, rework share, deploy-run success and their
// per-UTC-day series. It ports the prototype's web/src/lib/dora.ts formula by formula, and each
// function cites the prototype lines it ports. It judges nothing: the targets a delivery answer
// states (Targets) are those of the evals of the deploy repository's project
// (evals.Store.DeployRepoEvals), its verdicts (Status) are evals.VerdictOf's, and DefaultTargets
// is what the eval catalogue seeds those targets from. It reads no database, applies no facet and
// filters no window: the caller hands it the population PRs that survived the facets, the deploy
// runs its store query selected and the delivery settings the deploy cards follow (DeployCards),
// and Compute counts exactly those. Each measure is a day builder and a fold over day parts
// (parts.go), which the eval routes call over stored parts too.
package measures

import (
	"maps"
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

// Spread is a set of durations' median, 90th percentile and maximum, in minutes, all three nil for
// an empty set, and N, how many durations the set holds. N is no figure the measures panel shows,
// so the answer does not send it; it is the lead-time evals' `n`, which a person may name as a
// judge of their target, so the measures answer judges it as the eval does (evals.ResultStatistics).
type Spread struct {
	N             int      `json:"-"`
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
// Rate is Confirmed over Total, nil where Total is 0, as it is, with every count 0, while no
// failure source exists.
type FlagCounts struct {
	Confirmed int      `json:"confirmed"`
	Pending   int      `json:"pending"`
	Rejected  int      `json:"rejected"`
	Reverts   int      `json:"reverts"`
	Total     int      `json:"total"`
	Rate      *float64 `json:"rate"`
}

// DeployFlagCounts is the per-deploy change failure rate, with the rate it would be if every
// pending flag were confirmed: ConfirmedOrPending counts each deploy once. UpperBoundRate is nil
// where Rate is.
type DeployFlagCounts struct {
	FlagCounts
	ConfirmedOrPending int      `json:"confirmed_or_pending"`
	UpperBoundRate     *float64 `json:"upper_bound_rate"`
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

// ChangeFailureRate is the flags counted against PRs and against deploys. Its zero value is the
// rate while no failure source exists: every count 0 and every rate nil.
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
// and population. While no failure source exists (Compute's flags nil), ChangeFailureRate's rates
// are nil and its counts 0, and TimeToRestore holds no median: 0 is a measured value and never a
// stand-in for one, so a rate nothing measured reads no value rather than a 0 a target would judge
// met. ChangeFailureRate itself is never null: a Delivery page of an earlier build reads its
// fields with no null guard.
type Result struct {
	DeployFrequency   DeployFrequency   `json:"deploy_frequency"`
	LeadTime          LeadTime          `json:"lead_time"`
	ChangeFailureRate ChangeFailureRate `json:"change_failure_rate"`
	TimeToRestore     TimeToRestore     `json:"time_to_restore"`
	ReworkShare       float64           `json:"rework_share"`
	DeployRunSuccess  DeployRunSuccess  `json:"deploy_run_success"`
	Daily             []DailyPoint      `json:"daily"`
}

// Target is one measure's target as the eval that states it holds it: its value, whether the value
// itself meets it (inclusive: at least or at most, rather than above or under), which way is better
// (the eval's direction, fixed by its code: more deploys a day and more run success, fewer
// failures, minutes and unowned P0 issues), and the statistics it judges, each `<series>.<stat>`,
// in the order the eval's verdict judges them (evals.JudgesOf: its target_judges, or its primary
// series' headline statistic where it names none), so a reader names the figure a verdict is of.
// The answer's status is the window's verdict (evals.VerdictOf); a page tones each other figure by
// its own value of a statistic the target judges, compared with the value the same way, and a
// day's bar or cell only for a rate or a quantile of the day's samples. The direction's enum
// is the two directions the evals name (evals.DirectionHigherBetter, evals.DirectionLowerBetter),
// and the judges' is every judge a register write takes of the delivery evals (evals'
// deliveryJudges), which this package cannot import; evals'
// TestTheTargetsDirectionEnumIsTheEvalsDirections and TestTheTargetsJudgesEnumIsEveryDeliveryJudge
// hold each pair together.
type Target struct {
	Value     float64  `json:"value"`
	Inclusive bool     `json:"inclusive"`
	Direction string   `json:"direction" enum:"higher_better,lower_better"`
	Judges    []string `json:"judges" enum:"deploys.deploys,deploys.per_day,deploys.with_prs,deploys.with_prs_per_day,issues.value,minutes.max,minutes.median,minutes.n,minutes.p90,per_deploy.confirmed,per_deploy.pending,per_deploy.rate,per_deploy.rejected,per_deploy.total,per_pr.confirmed,per_pr.pending,per_pr.rate,per_pr.rejected,per_pr.reverts,per_pr.total,runs.cancelled,runs.concluded,runs.rate,runs.reached_production,upper_bound.confirmed_or_pending,upper_bound.rate"`
}

// Targets are the targets a delivery answer judges its measures by: each the target of the eval
// that states the measure in the deploy repository's project (evals.Store.DeployRepoEvals), which
// a person may set (R5), null where that eval's target is off.
type Targets struct {
	DeploysPerDay              *Target `json:"deploys_per_day"`                // deploy-frequency's
	ChangeFailureRate          *Target `json:"change_failure_rate"`            // change-fail-rate's
	MergeToProductionMinutes   *Target `json:"merge_to_production_minutes"`    // merge-to-production's
	OpenedToMergeMedianMinutes *Target `json:"opened_to_merge_median_minutes"` // pr-open-to-merge's
	DeployRunSuccessRate       *Target `json:"deploy_run_success_rate"`        // deploy-run-success's
	UnownedP0                  *Target `json:"unowned_p0"`                     // unowned-p0's
}

// DefaultTargets are the engineering targets the prototype states (its KPI_TARGETS,
// web/src/lib/dora.ts 11-18, CONTRACT.md "KPI targets", set 2026-09-28), which the catalogue seeds
// each delivery eval's target from: at least 20 deploys a day and 90% run success, under 5%
// failures per deploy, 45 minutes for every change to reach production and a 60-minute median from
// open to merge, and no more than 0 unowned P0 issues. Each holds its value and inclusiveness
// alone: the catalogue states each eval's direction and judges once, beside the target it seeds
// from here, and a delivery answer's target takes them from the eval.
var DefaultTargets = Targets{
	DeploysPerDay:              &Target{Value: 20, Inclusive: true},
	ChangeFailureRate:          &Target{Value: 0.05},
	MergeToProductionMinutes:   &Target{Value: 45},
	OpenedToMergeMedianMinutes: &Target{Value: 60},
	DeployRunSuccessRate:       &Target{Value: 0.9, Inclusive: true},
	UnownedP0:                  &Target{Value: 0, Inclusive: true},
}

// Status is whether each KPI meets its target, its eval's verdict of the answer's figures
// (evals.VerdictOf), null where the eval's target is off, and where no figure it judges misses the
// target and one of them has no value. Each is its eval's verdict over every statistic its target
// judges (MergeToProduction merge-to-production's, of the longest change and the median as the
// catalogue judges it), but MergeToProductionMedian and ChangeFailureRateUpperBound, each the
// verdict of one statistic of an eval (the median merge to production, the failure rate's upper
// bound) against that eval's target, and null as well where that eval's target judges no such
// statistic: the catalogue's judges give the median a verdict and the upper bound none.
type Status struct {
	DeploysPerDay               *bool `json:"deploys_per_day"`
	ChangeFailureRate           *bool `json:"change_failure_rate"`
	ChangeFailureRateUpperBound *bool `json:"change_failure_rate_upper_bound"`
	MergeToProductionMedian     *bool `json:"merge_to_production_median"`
	MergeToProduction           *bool `json:"merge_to_production"`
	OpenedToMerge               *bool `json:"opened_to_merge"`
	DeployRunSuccess            *bool `json:"deploy_run_success"`
	UnownedP0                   *bool `json:"unowned_p0"`
}

// Quantile is p's quantile of ascending, non-empty sorted, linear between the two nearest ranks:
// d3's quantileSorted, as the prototype's collector/src/collector/pipeline.py:69-77 writes it.
// Each product is converted to float64, which rounds it before the subtraction or sum it feeds,
// as the prototype's arithmetic rounds every step: the Go spec lets a build fuse a product with
// that subtraction or sum into one instruction that rounds once, an arm64 build does, and the
// last bit that moves can move a figure rounded to one decimal (a Pipeline day's p90 of 453 s,
// 7.55 minutes, read 7.6 on arm64 and 7.5 on amd64), so the quantile would differ by machine.
// The arithmetic is SortedSamples' Quantile's.
func Quantile(sorted []float64, p float64) float64 {
	return SortedSamples{merged: sorted, n: len(sorted)}.Quantile(p)
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
	cell := LeadTimeCell{N: sorted.Len()}
	if sorted.Len() == 0 {
		return cell
	}
	cell.MedianMinutes = new(sorted.Quantile(0.5))
	cell.P90Minutes = new(sorted.Quantile(0.9))
	cell.MaxMinutes = new(sorted.Max())
	return cell
}

// FoldReworkShare is rework-share over parts: the summed rework pull requests over the summed
// pull requests; nil when parts count none.
func FoldReworkShare(parts []DayPart) *float64 {
	return FoldSeries(parts, SeriesPRs).share()
}

// FoldChangeFailureRate is change-fail-rate over parts: the flags counted against pull requests
// and against deploys, each rate its summed numerator over its summed denominator, nil where that
// denominator is 0, since a window with no pull request or no deploy measured no rate of them and
// a 0 there would be judged met; the zero ChangeFailureRate, every rate nil, when parts hold none
// of its three series.
func FoldChangeFailureRate(parts []DayPart) ChangeFailureRate {
	perDeploy := FoldSeries(parts, SeriesPerDeploy)
	upperBound := FoldSeries(parts, SeriesUpperBound)
	perPR := FoldSeries(parts, SeriesPerPR)
	if perDeploy.Parts+upperBound.Parts+perPR.Parts == 0 {
		return ChangeFailureRate{}
	}
	reverts := int(perPR.Counts[CountReverts])
	return ChangeFailureRate{
		PerPR: FlagCounts{
			Confirmed: int(perPR.Num),
			Pending:   int(perPR.Counts[CountPending]),
			Rejected:  int(perPR.Counts[CountRejected]),
			Reverts:   reverts,
			Total:     int(perPR.Den),
			Rate:      perPR.share(),
		},
		PerDeploy: DeployFlagCounts{
			FlagCounts: FlagCounts{
				Confirmed: int(perDeploy.Num),
				Pending:   int(perDeploy.Counts[CountPending]),
				Rejected:  int(perDeploy.Counts[CountRejected]),
				Reverts:   reverts,
				Total:     int(perDeploy.Den),
				Rate:      perDeploy.share(),
			},
			ConfirmedOrPending: int(upperBound.Num),
			UpperBoundRate:     upperBound.share(),
		},
	}
}

// FoldTimeToRestore is failed-deploy-recovery over parts: the median of every part's minutes
// (dora.ts:206-213, 251), whose MedianMinutes is nil when no part holds a sample.
func FoldTimeToRestore(parts []DayPart) TimeToRestore {
	sorted := MergedSamples(parts, SeriesMinutes)
	if sorted.Len() == 0 {
		return TimeToRestore{}
	}
	return TimeToRestore{MedianMinutes: new(sorted.Quantile(0.5))}
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
// two read no value through their folds (every failure rate nil and no median to restore),
// exactly as their evals read them; a source that holds no flag (an empty, non-nil list) measures
// a rate of 0 over a window with pull requests and deploys, and no value of a denominator the
// window holds none of (FoldChangeFailureRate).
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
		return Spread{N: cell.N, MedianMinutes: cell.MedianMinutes, P90Minutes: cell.P90Minutes, MaxMinutes: cell.MaxMinutes}
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
