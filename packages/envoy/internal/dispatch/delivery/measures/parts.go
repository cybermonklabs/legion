package measures

import (
	"slices"
	"time"
)

// Every delivery and CI number is one rule split in two (dispatch://INFRA-2916, R7 and R12): a day
// builder (the functions named <Measure>Day) turns the rows of one UTC day into that day's parts,
// and one exported fold (the functions named Fold<Measure>) turns any set of parts into the
// answer. The delivery routes build the parts of their window's days from the rows they read and
// fold them (Compute, PRChecks, DeployJobs, LeadTimes); the eval routes fold the parts stored in
// eval_days through the same folds, so each rule has one statement and a read that both families
// serve has one answer.
//
// A fold reads the parts of its own series and passes over the rest, so it can be handed every
// part of its eval. A count or a sum adds Sum; a share is the summed Num over the summed Den, never
// the mean of the days' shares; a quantile merges the days' Samples and reads Quantile over the
// sorted merge, which is the quantile of every value of those days, never the mean of the days'
// quantiles. A set that holds no part of a fold's series reads no value (nil), never 0; a part
// built for a day with no rows is a zero part, and reads as zero counts.

// DayPart is one series' part of one UTC day: what one eval_days row holds except its key's
// project, eval and repository and its bookkeeping (listed, built_by, updated_at), so a fold
// reads a stored row and a part built on request alike, and evals.DayRow converts to and from it
// with nothing lost. Sum, Num, Den, Max and LastValue are nil where the part holds no such value.
// A builder fills the columns its series' fold reads: a sum N and Sum; a share N (the rows its
// denominator counts), Num and Den; a quantile N, Sum, Max and Samples, every value of the day.
// Counts are the named sub-counts a fold sums beside the value (CountWithPRs and the rest).
// ViaRepository is how many of the day's rows reached the part's project through a repository
// rather than an issue (dispatch://INFRA-2916, D1); the builders here read no project, and leave it
// to the caller that attributes their rows.
type DayPart struct {
	Series string
	// Day is the UTC midnight the day starts at.
	Day           time.Time
	N             int
	Sum           *float64
	Num           *float64
	Den           *float64
	Max           *float64
	Samples       []float64
	Counts        map[string]float64
	LastValue     *float64
	LastAt        *time.Time
	ViaRepository int
}

// The series the delivery and CI evals' parts are stored under (the seeded catalogue's).
const (
	// SeriesDeploys is deploy-frequency's: the successful deploys (a sum).
	SeriesDeploys = "deploys"
	// SeriesRuns is deploy-run-success's: the runs that reached production over those that
	// concluded (a share).
	SeriesRuns = "runs"
	// SeriesPRs is rework-share's: the rework pull requests over every one (a share).
	SeriesPRs = "prs"
	// SeriesPerDeploy, SeriesUpperBound and SeriesPerPR are change-fail-rate's three shares.
	SeriesPerDeploy  = "per_deploy"
	SeriesUpperBound = "upper_bound"
	SeriesPerPR      = "per_pr"
	// SeriesMinutes is the minutes of every lead time, of failed-deploy-recovery and of each CI
	// eval (a quantile).
	SeriesMinutes = "minutes"
	// JobSeriesPrefix and a job's name are pr-ci's series of that one job (a quantile): the
	// series the slowest-jobs table ranks.
	JobSeriesPrefix = "job:"
)

// The named counts a part carries beside its value.
const (
	CountWithPRs   = "with_prs"
	CountCancelled = "cancelled"
	CountSuccess   = "success"
	CountFailure   = "failure"
	CountPending   = "pending"
	CountRejected  = "rejected"
	CountReverts   = "reverts"
)

// CountPart is a sum's part: count rows, with counts beside them. The part helpers and the fold
// helpers below are exported because the Legion builders and folds (legion/metrics) and a logged
// eval's fold (evals) build and add up their parts by the same rules.
func CountPart(day time.Time, series string, count int, counts map[string]float64) DayPart {
	sum := float64(count)
	return DayPart{Series: series, Day: day, N: count, Sum: &sum, Counts: counts}
}

// RatioPart is a share's part: numerator over denominator, N the rows the denominator counts.
func RatioPart(day time.Time, series string, numerator, denominator int, counts map[string]float64) DayPart {
	num, den := float64(numerator), float64(denominator)
	return DayPart{Series: series, Day: day, N: denominator, Num: &num, Den: &den, Counts: counts}
}

// SamplesPart is a quantile's part: every value of the day, with their count, sum and largest.
// The sum is taken in the order of samples.
func SamplesPart(day time.Time, series string, samples []float64, counts map[string]float64) DayPart {
	part := DayPart{Series: series, Day: day, N: len(samples), Samples: samples, Counts: counts}
	if part.Samples == nil {
		part.Samples = []float64{}
	}
	sum := 0.0
	for _, value := range samples {
		sum += value
		if part.Max == nil || value > *part.Max {
			part.Max = new(value)
		}
	}
	part.Sum = &sum
	return part
}

// Folded is one series' parts added up: how many parts were of the series (Parts, which is 0
// where none was), and their sums, numerators, denominators and counts.
type Folded struct {
	Parts         int
	Sum, Num, Den float64
	Counts        map[string]float64
}

// FoldSeries adds up the parts of series among parts, in the order parts are given.
func FoldSeries(parts []DayPart, series string) Folded {
	var f Folded
	for _, part := range parts {
		if part.Series != series {
			continue
		}
		f.Parts++
		f.Sum += ValueOrZero(part.Sum)
		f.Num += ValueOrZero(part.Num)
		f.Den += ValueOrZero(part.Den)
		for name, count := range part.Counts {
			if f.Counts == nil {
				f.Counts = map[string]float64{}
			}
			f.Counts[name] += count
		}
	}
	return f
}

// share is the summed numerator over the summed denominator, nil when the denominator is 0.
func (f Folded) share() *float64 {
	if f.Den == 0 {
		return nil
	}
	return new(f.Num / f.Den)
}

// MergedSamples is every sample of series among parts, sorted: the one list a quantile reads.
func MergedSamples(parts []DayPart, series string) []float64 {
	var merged []float64
	for _, part := range parts {
		if part.Series == series {
			merged = append(merged, part.Samples...)
		}
	}
	slices.Sort(merged)
	return merged
}

// partsByDay is parts by the UTC day each is of, YYYY-MM-DD.
func partsByDay(parts []DayPart) map[string][]DayPart {
	byDay := map[string][]DayPart{}
	for _, part := range parts {
		key := utcDay(part.Day)
		byDay[key] = append(byDay[key], part)
	}
	return byDay
}

// ValueOrZero is *value, 0 where value is nil: a part's Sum, Num or Den as a fold adds it.
func ValueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}
