package measures

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// The Pipeline page's numbers (LEGION-567 slice 3): how long PR checks, the deploy workflow's
// staging end-to-end gate and its staging and production deploys take, and how often each ends in
// success, per UTC day over the days its caller asks for, beside the merge-to-production and
// open-to-merge lead times per merge day. Ported from the prototype's
// collector/src/collector/pipeline.py (the run and job timings) and web/src/lib/pipeline.ts (the
// lead times), formula by formula. Every timing buckets by the UTC day its run started (a job by
// its run's start). Only results that ended in success or failure are timed; a cancelled one is
// counted beside them, and anything else (skipped, still running) is not counted at all.
//
// How many days are shown is the caller's (INFRA-1077): GET /api/v1/delivery/pipeline reads a
// window, and ShownDayCount is what it answers a caller that names none. How far the
// failing-jobs table looks back is not: it is always FailingWindow back from the window's end.
//
// Each card is a day builder and a fold (parts.go): PRChecks, DeployJobs and LeadTimes put their
// rows on the shown UTC days, build each day's parts (PRChecksDay, DeployJobsDay and the lead-time
// builders of measures.go), and fold each day's parts alone and the shown days' together
// (FoldTiming, FoldSlowestJobs, FoldLeadTime), the folds the CI and delivery evals read stored
// parts with. The failing-jobs table is no day part: FailingJobs reads the run jobs of the
// FailingWindow before the window's end.

// ShownDayCount is how many UTC days the Pipeline page shows when its caller names no window,
// ending on the day its clock is on (pipeline.py:19).
const ShownDayCount = 7

// SlowestJobs is how many of the PR-checks workflow's jobs the slowest-jobs table ranks.
const SlowestJobs = 5

// SlowestMinResults is how many timed results a job needs in the shown days to be ranked, so a
// one-task matrix entry with a handful of results is not (pipeline.py:27-29).
const SlowestMinResults = 50

// FailingWindow is how far back the failing-jobs table looks from the window's end: its last day,
// whatever the window's length (pipeline.py:30).
const FailingWindow = 24 * time.Hour

// Outcome is a run or job conclusion as the Pipeline counts it.
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeFailure   Outcome = "failure"
	OutcomeCancelled Outcome = "cancelled"
)

// OutcomeOf is a conclusion as the Pipeline counts it (pipeline.py:38-47): success; failure for
// failure or timed_out; cancelled; anything else (skipped, startup_failure, still running) is not
// counted, and the second result is false.
func OutcomeOf[C ~string](conclusion *C) (Outcome, bool) {
	if conclusion == nil {
		return "", false
	}
	switch string(*conclusion) {
	case "success":
		return OutcomeSuccess, true
	case "failure", "timed_out":
		return OutcomeFailure, true
	case "cancelled":
		return OutcomeCancelled, true
	}
	return "", false
}

// timed is an outcome the Pipeline times: success or failure.
func (o Outcome) timed() bool { return o == OutcomeSuccess || o == OutcomeFailure }

// Cell is one day's, or the shown days', results of one stage.
type Cell struct {
	N             int      `json:"n"`              // timed results (success or failure)
	MedianMinutes *float64 `json:"median_minutes"` // one decimal, as the prototype rounds
	P90Minutes    *float64 `json:"p90_minutes"`
	Success       int      `json:"success"`
	Failure       int      `json:"failure"`
	Cancelled     int      `json:"cancelled"`
	SuccessRate   *float64 `json:"success_rate"` // success/(success+failure), four decimals, nil when both are 0
}

// Day is one shown UTC day's Cell; Partial is today's.
type Day struct {
	Day     string `json:"day"` // YYYY-MM-DD, UTC
	Partial bool   `json:"partial"`
	Cell
}

// Timing is one stage per shown day, oldest first, and over all of them.
type Timing struct {
	Days  []Day `json:"days"`
	Total Cell  `json:"total"`
}

// sample is one result of one stage: its outcome, and its minutes when timed (nil otherwise).
type sample struct {
	outcome Outcome
	minutes *float64
}

// JobTiming is one PR-checks job's timing, by its name.
type JobTiming struct {
	Name string `json:"name"`
	Timing
}

// DeployJobTiming is one deploy-workflow job's timing, by the job names it follows, joined by
// " or ".
type DeployJobTiming struct {
	Job string `json:"job"`
	Timing
}

// FailingJob is one PR-checks job that failed in the last FailingWindow: how many of its results
// there failed, out of how many, and the run of its latest failure.
type FailingJob struct {
	Name         string    `json:"name"`
	Failures     int       `json:"failures"`
	Results      int       `json:"results"`
	LastFailedAt time.Time `json:"last_failed_at"`
	LastRunURL   string    `json:"last_run_url"`
}

// LeadTimeCell is one day's, or the shown days', lead times in minutes, unrounded as the
// prototype's web side leaves them.
type LeadTimeCell struct {
	N             int      `json:"n"`
	MedianMinutes *float64 `json:"median_minutes"`
	P90Minutes    *float64 `json:"p90_minutes"`
	MaxMinutes    *float64 `json:"max_minutes"`
}

// LeadTimeDay is one shown UTC merge day's LeadTimeCell; Partial is today's.
type LeadTimeDay struct {
	Day     string `json:"day"`
	Partial bool   `json:"partial"`
	LeadTimeCell
}

// LeadTimeTiming is one lead time per shown merge day, oldest first, and over all of them.
type LeadTimeTiming struct {
	Days  []LeadTimeDay `json:"days"`
	Total LeadTimeCell  `json:"total"`
}

// ShownDays is the dayCount UTC days ending on the day end is on, oldest first (pipeline.py:54-57).
func ShownDays(end time.Time, dayCount int) []string {
	first := WindowStart(end, dayCount)
	days := make([]string, dayCount)
	for i := range days {
		days[i] = first.AddDate(0, 0, i).Format(time.DateOnly)
	}
	return days
}

// WindowStart is midnight UTC of the first of the dayCount days ending on end's (pipeline.py:60-63).
func WindowStart(end time.Time, dayCount int) time.Time {
	return midnight(end).AddDate(0, 0, -(dayCount - 1))
}

// roundTo is Python's round(x, places), which the prototype's collector applies: it rounds the
// decimal value x holds, half to even, as strconv's 'f' formatting does. A float multiply would
// not (189 s is 3.15 minutes, 3.149999… as a float, which Python rounds to 3.1 and
// math.RoundToEven(x*10)/10 to 3.2).
func roundTo(x float64, places int) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', places, 64), 64)
	if err != nil {
		panic("measures: a formatted float does not parse: " + err.Error())
	}
	return rounded
}

// timingPart is a timing series' part of one day, from its results that day: the minutes of the
// timed ones (success or failure, with a known duration), and how many ended in each outcome.
func timingPart(day time.Time, series string, samples []sample) DayPart {
	var minutes []float64
	counts := map[string]float64{CountSuccess: 0, CountFailure: 0, CountCancelled: 0}
	for _, result := range samples {
		switch result.outcome {
		case OutcomeSuccess:
			counts[CountSuccess]++
		case OutcomeFailure:
			counts[CountFailure]++
		case OutcomeCancelled:
			counts[CountCancelled]++
		}
		if result.outcome.timed() && result.minutes != nil {
			minutes = append(minutes, *result.minutes)
		}
	}
	return samplesPart(day, series, minutes, counts)
}

// cellOf is series' parts folded into one Cell (pipeline.py:80-92): the median and p90 of the
// merge of their minutes to one decimal, each outcome summed, and the success rate to four.
func cellOf(parts []DayPart, series string) Cell {
	sorted := mergedSamples(parts, series)
	counts := foldSeries(parts, series).counts
	result := Cell{
		N:         len(sorted),
		Success:   int(counts[CountSuccess]),
		Failure:   int(counts[CountFailure]),
		Cancelled: int(counts[CountCancelled]),
	}
	if len(sorted) > 0 {
		result.MedianMinutes = new(roundTo(Quantile(sorted, 0.5), 1))
		result.P90Minutes = new(roundTo(Quantile(sorted, 0.9), 1))
	}
	if decided := result.Success + result.Failure; decided > 0 {
		result.SuccessRate = new(roundTo(float64(result.Success)/float64(decided), 4))
	}
	return result
}

// FoldTiming is a CI eval over parts (pr-ci's runs, staging-e2e, staging-deploy and
// production-deploy): its SeriesMinutes parts folded into one Cell. Its median, p90 and success
// rate are nil when no part holds a result.
func FoldTiming(parts []DayPart) Cell {
	return cellOf(parts, SeriesMinutes)
}

// timingByDay is parts on each shown day folded alone by fold, and the shown days' parts folded
// together (pipeline.py:95-106): a part of a day not shown is dropped, and the last day is
// partial.
func timingByDay(parts []DayPart, days []string, fold func([]DayPart) Cell) Timing {
	byDay := partsByDay(parts)
	timing := Timing{Days: make([]Day, len(days))}
	var shown []DayPart
	for i, d := range days {
		timing.Days[i] = Day{Day: d, Partial: i == len(days)-1, Cell: fold(byDay[d])}
		shown = append(shown, byDay[d]...)
	}
	timing.Total = fold(shown)
	return timing
}

// onShownDays is each shown day's UTC midnight and the rows whose time is on it, oldest first; a
// row on no shown day is in none, since the Pipeline counts only the days it shows.
func onShownDays[T any](rows []T, timeOf func(T) time.Time, days []string) ([]time.Time, [][]T) {
	midnights := make([]time.Time, len(days))
	index := make(map[string]int, len(days))
	for i, d := range days {
		parsed, err := time.Parse(time.DateOnly, d)
		if err != nil {
			panic("measures: a shown day is not a date: " + err.Error())
		}
		midnights[i], index[d] = parsed, i
	}
	byDay := make([][]T, len(days))
	for _, row := range rows {
		if i, shown := index[utcDay(timeOf(row))]; shown {
			byDay[i] = append(byDay[i], row)
		}
	}
	return midnights, byDay
}

// utcDay is t's UTC day, YYYY-MM-DD.
func utcDay(t time.Time) string {
	return t.UTC().Format(time.DateOnly)
}

// elapsedMinutes is end - start in minutes, or nil when either is unknown.
func elapsedMinutes(start, end *time.Time) *float64 {
	if start == nil || end == nil {
		return nil
	}
	return new(minutesBetween(*start, *end))
}

// PRChecksDay is pr-ci's parts of one UTC day, from the PR-checks workflow's pull_request runs
// started that day and their jobs (pipeline.py:113-135): SeriesMinutes, each run's wall time from
// its start to its completion and its outcome; and one series per job with a result that day
// (JobSeriesPrefix and its name), whose results come only from runs that ended in success or
// failure, whose jobs the prototype fetches, and only from jobs that did, the only ones it stores
// (github_source.py:287-299), each job's minutes its duration rounded to the whole second, as the
// prototype stores its seconds. A run still running adds nothing until it concludes, when its day
// is built again. The failing-jobs table is no part of a day: it is FailingJobs' read of the 24
// hours before a window's end, over the same runs and jobs.
func PRChecksDay(day time.Time, runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob) []DayPart {
	runSamples := make([]sample, 0, len(runs))
	jobSamples := map[string][]sample{}
	for _, run := range runs {
		outcome, ok := OutcomeOf(run.Conclusion)
		if !ok {
			continue
		}
		result := sample{outcome: outcome}
		if outcome.timed() {
			result.minutes = elapsedMinutes(&run.StartedAt, run.CompletedAt)
		}
		runSamples = append(runSamples, result)
		if !outcome.timed() {
			continue
		}
		for _, job := range jobsByRun[run.RunID] {
			jobOutcome, ok := OutcomeOf(job.Conclusion)
			if !ok || !jobOutcome.timed() {
				continue
			}
			var minutes *float64
			if job.StartedAt != nil && job.CompletedAt != nil {
				minutes = new(math.Round(job.CompletedAt.Sub(*job.StartedAt).Seconds()) / 60)
			}
			jobSamples[job.Name] = append(jobSamples[job.Name], sample{outcome: jobOutcome, minutes: minutes})
		}
	}
	parts := []DayPart{timingPart(day, SeriesMinutes, runSamples)}
	for _, name := range slices.Sorted(maps.Keys(jobSamples)) {
		parts = append(parts, timingPart(day, JobSeriesPrefix+name, jobSamples[name]))
	}
	return parts
}

// jobTimings is each PR-checks job's timing by name over the shown days: each job series of
// pr-ci's parts, folded per day and over the shown days.
func jobTimings(parts []DayPart, days []string) map[string]Timing {
	bySeries := map[string][]DayPart{}
	for _, part := range parts {
		if strings.HasPrefix(part.Series, JobSeriesPrefix) {
			bySeries[part.Series] = append(bySeries[part.Series], part)
		}
	}
	timings := make(map[string]Timing, len(bySeries))
	for series, jobParts := range bySeries {
		timings[strings.TrimPrefix(series, JobSeriesPrefix)] = timingByDay(jobParts, days, func(parts []DayPart) Cell { return cellOf(parts, series) })
	}
	return timings
}

// FoldSlowestJobs is pr-ci's slowest-jobs table over parts (pipeline.py:137-142): the SlowestJobs
// jobs with SlowestMinResults timed results over the shown days (the job series' min_n), by
// their median over those days, slowest first, then by name, each with its cell of every shown
// day; never nil.
func FoldSlowestJobs(parts []DayPart, days []string) []JobTiming {
	slowest := []JobTiming{}
	for name, timing := range jobTimings(parts, days) {
		if timing.Total.N >= SlowestMinResults {
			slowest = append(slowest, JobTiming{Name: name, Timing: timing})
		}
	}
	slices.SortFunc(slowest, func(a, b JobTiming) int {
		// A ranked job has SlowestMinResults timed results, so its median is set.
		if c := cmp.Compare(*b.Total.MedianMinutes, *a.Total.MedianMinutes); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return slowest[:min(len(slowest), SlowestJobs)]
}

// prChecksParts is pr-ci's parts of each shown day, built from the runs started on it.
func prChecksParts(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, days []string) []DayPart {
	midnights, byDay := onShownDays(runs, func(run delivery.DeliveryRun) time.Time { return run.StartedAt }, days)
	var parts []DayPart
	for i, d := range midnights {
		parts = append(parts, PRChecksDay(d, byDay[i], jobsByRun)...)
	}
	return parts
}

// PRChecks is the PR-checks workflow's Pipeline numbers (pipeline.py:113-167), from the parts
// PRChecksDay builds of each shown day's runs: run, every run's wall time from its start to its
// completion and its outcome, each day folded alone and the shown days together by FoldTiming;
// slowest, FoldSlowestJobs; and failing, FailingJobs up to end, where the window stops. A run on
// no shown day adds to no cell. The caller passes the shown days' runs, each with its event and
// branch already selected.
func PRChecks(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, days []string, end time.Time, rollupJob string) (run Timing, slowest []JobTiming, failing []FailingJob) {
	parts := prChecksParts(runs, jobsByRun, days)
	return timingByDay(parts, days, FoldTiming), FoldSlowestJobs(parts, days), FailingJobs(runs, jobsByRun, end, rollupJob)
}

// FailingJobs is PRChecks' failing tally (pipeline.py:144-157): every job that failed in a run
// completed in the FailingWindow up to end (a run that completed after end, which a window pinned
// in the past can read, is not counted), most failures first, then by name. It reads only runs
// that ended in success or failure (a cancelled run's jobs are not the prototype's to count) and
// only their jobs that did, and leaves out rollupJob (none when empty) and every summary or guard
// job (delivery.IsSummaryOrGuardJob). It is no day part: the 24 hours it looks back over end at
// any instant, not at a UTC day's end, so it is read from the run jobs themselves, the rows the
// PR-checks parts are built from, as an items read is.
func FailingJobs(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, end time.Time, rollupJob string) []FailingJob {
	horizon := end.Add(-FailingWindow)
	var recent []delivery.DeliveryRun
	for _, run := range runs {
		outcome, ok := OutcomeOf(run.Conclusion)
		if ok && outcome.timed() && run.CompletedAt != nil && !run.CompletedAt.Before(horizon) && !run.CompletedAt.After(end) {
			recent = append(recent, run)
		}
	}
	// In completion order, runs completing together by run id, so each job's last failure is its
	// latest whatever order runs holds them in.
	slices.SortFunc(recent, func(a, b delivery.DeliveryRun) int {
		return cmp.Or(a.CompletedAt.Compare(*b.CompletedAt), cmp.Compare(a.RunID, b.RunID))
	})
	tallies := map[string]*FailingJob{}
	for _, run := range recent {
		for _, job := range jobsByRun[run.RunID] {
			outcome, ok := OutcomeOf(job.Conclusion)
			if !ok || !outcome.timed() || (rollupJob != "" && job.Name == rollupJob) || delivery.IsSummaryOrGuardJob(job.Name) {
				continue
			}
			tally, seen := tallies[job.Name]
			if !seen {
				tally = &FailingJob{Name: job.Name}
				tallies[job.Name] = tally
			}
			tally.Results++
			if outcome == OutcomeFailure {
				tally.Failures++
				tally.LastFailedAt = *run.CompletedAt
				tally.LastRunURL = run.URL
			}
		}
	}
	failing := []FailingJob{}
	for _, tally := range tallies {
		if tally.Failures > 0 {
			failing = append(failing, *tally)
		}
	}
	slices.SortFunc(failing, func(a, b FailingJob) int {
		if c := cmp.Compare(b.Failures, a.Failures); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return failing
}

// DeployJobCardID identifies one Pipeline deploy card: the key DeployJobsDay answers a card's
// parts under, and the card a deploy card's eval is of (evals.CardOf).
type DeployJobCardID string

// DeployJobCard is one Pipeline card, identified by CardID. Its embedded production rule gives every
// name it follows (a renamed job's old and new names, so history and new runs both count) and, for
// the production card, the apply step whose success makes a successful job a deploy.
type DeployJobCard struct {
	CardID DeployJobCardID
	delivery.Production
}

// The Pipeline's deploy cards, by the key GET /api/v1/delivery/pipeline answers each under: the
// production deploy, the staging end-to-end gate and the staging deploy (the CI evals
// production-deploy, staging-e2e and staging-deploy).
const (
	CardProductionApply DeployJobCardID = "production_apply"
	CardStagingE2E      DeployJobCardID = "staging_e2e"
	CardStagingApply    DeployJobCardID = "staging_apply"
)

// DeployCards is the deploy cards under settings, in the order DeployJobsDay gives a name two cards
// follow to the first: the production deploy, by the settings' production rule, and the staging
// end-to-end and staging deploy jobs where the settings name them. The Pipeline route and the CI
// evals' day rows both build their cards here, and read the deploy runs with the jobs these
// cards follow.
func DeployCards(settings delivery.DeliverySettings) []DeployJobCard {
	cards := []DeployJobCard{{CardID: CardProductionApply, Production: settings.Production()}}
	for _, optional := range []struct {
		cardID DeployJobCardID
		name   *string
	}{
		{CardStagingE2E, settings.StagingE2EJobName},
		{CardStagingApply, settings.StagingApplyJobName},
	} {
		if optional.name != nil {
			cards = append(cards, DeployJobCard{CardID: optional.cardID, Production: delivery.Production{JobNames: []string{*optional.name}}})
		}
	}
	return cards
}

// DeployCardJobNames is every job name cards follow: the jobs a deploy run is read with for them.
func DeployCardJobNames(cards []DeployJobCard) []string {
	var names []string
	for _, card := range cards {
		names = append(names, card.JobNames...)
	}
	return names
}

// DeployJobsDay is each card's part of one UTC day, keyed by its CardID (staging-e2e,
// staging-deploy and production-deploy; pipeline.py:170-185), read in one pass from the deploy
// workflow's push runs on main started that day: each followed job's outcome (its conclusion as
// the card's delivery.Production reads it, so a job that applied nothing is not counted) and its
// minutes from its start to its completion when timed. A run without a followed job, or whose job
// is not counted, adds nothing; every card gets a part. A run adds no more than one result to a
// card, from its Production.Job. A name two cards follow counts for the first. A run whose job is
// still running adds nothing until the job concludes, when its day is built again.
func DeployJobsDay(day time.Time, runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, cards []DeployJobCard) map[DeployJobCardID][]DayPart {
	samples := make([][]sample, len(cards))
	cardOf := map[string]int{}
	for i, card := range cards {
		for _, name := range card.JobNames {
			if _, taken := cardOf[name]; !taken {
				cardOf[name] = i
			}
		}
	}
	cardJobs := make([][]delivery.DeliveryRunJob, len(cards))
	for _, run := range runs {
		for i := range cardJobs {
			cardJobs[i] = cardJobs[i][:0]
		}
		for _, job := range jobsByRun[run.RunID] {
			if i, followed := cardOf[job.Name]; followed {
				cardJobs[i] = append(cardJobs[i], job)
			}
		}
		for i, jobs := range cardJobs {
			job := cards[i].Job(jobs)
			if job == nil {
				continue
			}
			outcome, ok := OutcomeOf(cards[i].Conclusion(*job))
			if !ok {
				continue
			}
			result := sample{outcome: outcome}
			if outcome.timed() {
				result.minutes = elapsedMinutes(job.StartedAt, job.CompletedAt)
			}
			samples[i] = append(samples[i], result)
		}
	}
	parts := make(map[DeployJobCardID][]DayPart, len(cards))
	for i, card := range cards {
		parts[card.CardID] = []DayPart{timingPart(day, SeriesMinutes, samples[i])}
	}
	return parts
}

// DeployJobs is each card's timing keyed by its CardID, from the parts DeployJobsDay builds of each
// shown day's runs, each day folded alone and the shown days together by FoldTiming. The caller
// passes the deploy workflow's push runs on main started in the shown days; a run on no shown day
// adds nothing. Every card gets a timing, all of its days empty when no run lists its jobs, and
// its job field is its names joined by " or ".
func DeployJobs(runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, cards []DeployJobCard, days []string) map[DeployJobCardID]DeployJobTiming {
	midnights, byDay := onShownDays(runs, func(run delivery.DeliveryRun) time.Time { return run.StartedAt }, days)
	parts := make(map[DeployJobCardID][]DayPart, len(cards))
	for i, d := range midnights {
		for card, cardParts := range DeployJobsDay(d, byDay[i], jobsByRun, cards) {
			parts[card] = append(parts[card], cardParts...)
		}
	}
	timings := make(map[DeployJobCardID]DeployJobTiming, len(cards))
	for _, card := range cards {
		timings[card.CardID] = DeployJobTiming{Job: strings.Join(card.JobNames, " or "), Timing: timingByDay(parts[card.CardID], days, FoldTiming)}
	}
	return timings
}

// leadTimeByDay is parts on each shown day folded alone by FoldLeadTime, and the shown days' parts
// folded together: a part of a day not shown is dropped, and the last day is partial.
func leadTimeByDay(parts []DayPart, days []string) LeadTimeTiming {
	byDay := partsByDay(parts)
	timing := LeadTimeTiming{Days: make([]LeadTimeDay, len(days))}
	var shown []DayPart
	for i, d := range days {
		timing.Days[i] = LeadTimeDay{Day: d, Partial: i == len(days)-1, LeadTimeCell: FoldLeadTime(byDay[d])}
		shown = append(shown, byDay[d]...)
	}
	timing.Total = FoldLeadTime(shown)
	return timing
}

// LeadTimes is the two lead-time cards (pipeline.ts:31-58), each PR on its UTC merge day:
// mergeToProduction, the DORA lead time, over the PRs that deployed (one still waiting has none
// yet), and openedToMerge over every PR, deployed or not; each shown day's PRs built into that
// day's parts (MergeToProductionDay, OpenToMergeDay) and folded by FoldLeadTime. A PR merged on no
// shown day adds nothing.
func LeadTimes(prs []PR, days []string) (mergeToProduction, openedToMerge LeadTimeTiming) {
	midnights, byDay := onShownDays(prs, func(pr PR) time.Time { return pr.MergedAt }, days)
	var merged, opened []DayPart
	for i, d := range midnights {
		merged = append(merged, MergeToProductionDay(d, byDay[i])...)
		opened = append(opened, OpenToMergeDay(d, byDay[i])...)
	}
	return leadTimeByDay(merged, days), leadTimeByDay(opened, days)
}
