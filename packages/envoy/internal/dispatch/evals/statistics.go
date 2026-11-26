package evals

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// An eval read states each listed series' statistics for a range of days: the figures a card
// shows and a target judges, a judge naming one as `<series>.<stat>`. They are folded from the
// stored day rows of the range (DayWindowIn), never from the population again (R12):
//
//   - a computed eval through the one fold its source names (computedSources), which is the fold
//     the delivery and Legion routes answer through too (R7), so one rule has one statement;
//   - a logged eval through each series' own fold, its agg (aggStatistics), since no function
//     computes it.
//
// computedSources is the one table keyed by a computed source: its fold and its headline. Every
// eval of the seeded catalogue names a source it holds, with both, which init checks when the
// package starts, so a computed eval whose fold or headline is missing fails every binary and
// every test that links this package rather than reading "no data" on a page.

// Statistics is one series' figures over one range, by name. A figure the range holds no value of
// is nil: "no data", never 0.
type Statistics map[string]*float64

// seriesFold folds an eval's day parts of one range into the statistics of each of series, by
// series name; days is the range, whole UTC days, which a per-day figure divides by. A series the
// fold has no figure of answers an empty Statistics.
type seriesFold func(parts []measures.DayPart, days Range, series []string) map[string]Statistics

// computedSource is what a computed eval's source names in this package: the fold of its day
// parts, the exported function the source names read out as statistics; and its headline, the
// statistic it states a series by, given the series' name before its cut, which is one the fold
// states of that series (TestEveryCatalogueSeriesIsStatedByItsHeadline holds each to it).
type computedSource struct {
	fold     seriesFold
	headline func(series string) string
}

// computedSources is each computed eval's fold and headline, by its source.
var computedSources = map[string]computedSource{
	SourceFoldDeployFrequency:  {oneSeries(measures.SeriesDeploys, deployFrequencyStatistics), stated("per_day")},
	SourceFoldDeployRunSuccess: {oneSeries(measures.SeriesRuns, deployRunSuccessStatistics), stated("rate")},
	SourceFoldLeadTime:         {oneSeries(measures.SeriesMinutes, leadTimeStatistics), stated("median")},
	SourceFoldReworkShare:      {oneSeries(measures.SeriesPRs, reworkShareStatistics), stated("rate")},
	SourceFoldChangeFailRate:   {oneSeries(measures.SeriesPerDeploy, changeFailRateStatistics), stated("rate")},
	SourceFoldRecovery:         {oneSeries(measures.SeriesMinutes, recoveryStatistics), stated("median")},
	SourceFoldReworkRate:       {oneSeries(measures.SeriesReworkDeploys, reworkRateStatistics), stated("rate")},
	SourceFoldTiming:           {oneSeries(measures.SeriesMinutes, timingStatistics), stated("median")},
	// unowned-p0's day row is the count at the day's last refresh, which delivery.CountUnownedP0
	// counts by delivery.ListUnownedP0's rule; a window reads its newest day's, the `last` fold.
	SourceListUnownedP0: {oneSeries(SeriesUnownedP0, func(parts []measures.DayPart, _ Range) Statistics {
		return aggStatistics(AggLast, parts, SeriesUnownedP0)
	}), stated("value")},
	SourceFoldWorkTime:  {byCut(metrics.FoldWorkTime, workTimeStatistics), stated("median")},
	SourceFoldPhaseTime: {byCut(metrics.FoldPhaseTime, phaseTimeStatistics), stated("median_per_issue")},
	SourceFoldIdleTime:  {byCut(metrics.FoldIdleTime, idleTimeStatistics), stated("median")},
	SourceFoldSlotWait:  {byCut(metrics.FoldSlotWait, slotWaitStatistics), stated("median")},
	SourceFoldRework: {byCut(metrics.FoldRework, reworkStatistics), func(series string) string {
		if series == metrics.SeriesSentBack {
			return "share"
		}
		return "median"
	}},
	SourceFoldSplitting: {byCut(metrics.FoldSplitting, splittingStatistics), func(series string) string {
		if series == metrics.SeriesHoursWithChildren || series == metrics.SeriesHoursWithoutChildren {
			return "median"
		}
		return "issues"
	}},
}

// Every catalogue eval's source has its fold and its headline: checked once, when the package
// starts.
func init() {
	for _, entry := range catalogue {
		if source, held := computedSources[entry.definition.Source]; !held || source.fold == nil || source.headline == nil {
			panic("evals: the catalogue eval " + entry.definition.Slug + " names the source " +
				entry.definition.Source + ", for which computedSources holds no fold and headline")
		}
	}
}

// FoldStatistics is the statistics of each of series over parts, the day parts of one range of
// days of definition's eval (those of other evals are passed over): by the fold definition's
// source names for a computed eval, by each series' agg for a logged one. A computed eval whose
// source names no fold of this package (one an older build's catalogue seeded, under a source this
// build no longer holds) answers no figure. A figure the fold leaves outside what a float holds
// has no value: each day row holds finite parts (the readings write refuses a row that would not,
// CodeReadingDayOverflow), but the parts of a range of days can still sum past the largest float,
// and no read could answer that figure.
func FoldStatistics(definition Definition, series []Series, parts []measures.DayPart, days Range) map[string]Statistics {
	var folded map[string]Statistics
	source, held := computedSources[definition.Source]
	switch {
	case definition.Computed() && held:
		names := make([]string, len(series))
		for index, one := range series {
			names[index] = one.Series
		}
		folded = source.fold(parts, days, names)
	case definition.Computed():
		folded = make(map[string]Statistics, len(series))
		for _, one := range series {
			folded[one.Series] = Statistics{}
		}
	default:
		folded = make(map[string]Statistics, len(series))
		for _, one := range series {
			folded[one.Series] = aggStatistics(one.Agg, parts, one.Series)
		}
	}
	return writable(folded)
}

// writable is folded with every figure outside what a float holds made nil, in place: no answer
// can write one.
func writable(folded map[string]Statistics) map[string]Statistics {
	for _, stats := range folded {
		for name, value := range stats {
			if value != nil && (math.IsInf(*value, 0) || math.IsNaN(*value)) {
				stats[name] = nil
			}
		}
	}
	return folded
}

// SeriesValues is one listed series of an eval as a read states it: its current definition, its
// statistics over the window, over the compare range (null with none) and the change from the
// compare range to the window (each statistic the window's less the compare range's, null where
// either has no value; null with no compare range); data_since, the first day the series holds a
// day row (YYYY-MM-DD, null for a series with none), before which a range reads "no data", every
// statistic null; via_repository, how many of the rows the window's day rows count reached the
// project through a repository rather than an issue; and days, the window's per-day array of a
// headline series, one point per day that holds a row of it ([] for a control series).
type SeriesValues struct {
	Series
	Window        Statistics `json:"window"`
	Compare       Statistics `json:"compare"`
	Change        Statistics `json:"change"`
	DataSince     *string    `json:"data_since"`
	ViaRepository int        `json:"via_repository"`
	Days          []DayPoint `json:"days"`
}

// DayPoint is one day of a headline series' per-day array: the day (YYYY-MM-DD, UTC) and the
// series' statistics over that day's rows alone, through the same fold as the window's.
type DayPoint struct {
	Day        string     `json:"day"`
	Statistics Statistics `json:"statistics"`
}

// SeriesValuesOf is each listed series of entry with its values over rows, the eval's day rows of
// window and of compare, the compare range, nil for none (DayWindowIn), and firsts, each series'
// first day (FirstDaysIn). A range that starts before a series' first day, or a series with no first
// day at all, reads "no data": every statistic of that range null, whatever rows the range holds;
// the per-day array still answers each day that holds a row (R12).
//
// It folds the rows in the one order every fold of them takes (sortWindowRows), which it sorts rows
// into in place, as the stored figures are folded (figures.go, newFigureGroup), so a fold whose
// sums depend on the order of its parts answers the stored figures' bits whatever order the read's
// plan answered its rows in, and a read of the stored figures (StoredFigures.SeriesValues) answers
// what this does, bit for bit.
func SeriesValuesOf(entry Entry, rows []WindowRow, firsts map[string]time.Time, window Range, compare *Range) []SeriesValues {
	listed, headline := listedAndHeadline(entry)
	sortWindowRows(rows)
	var windowParts, compareParts []measures.DayPart
	byDay := map[time.Time][]measures.DayPart{}
	via := map[string]int{}
	for _, row := range rows {
		part := row.Part
		if row.Range == RangeCompare {
			compareParts = append(compareParts, part)
			continue
		}
		windowParts = append(windowParts, part)
		byDay[part.Day] = append(byDay[part.Day], part)
		via[part.Series] += part.ViaRepository
	}
	definition := entry.Definition
	windowed := FoldStatistics(definition, listed, windowParts, window)
	var compared map[string]Statistics
	if compare != nil {
		compared = FoldStatistics(definition, listed, compareParts, *compare)
	}
	// Each day of the window folded alone, by the same fold, for the headline series' arrays.
	days := slices.SortedFunc(maps.Keys(byDay), func(a, b time.Time) int { return a.Compare(b) })
	daily := make([]dayFold, len(days))
	for index, day := range days {
		daily[index] = dayFold{day: day, folded: foldDay(definition, headline, byDay[day], day)}
	}
	return valuesOf(listed, windowed, via, compared, daily, firsts, window, compare)
}

// sortWindowRows sorts rows in place into the one order a read of the day rows (SeriesValuesOf),
// the stored figures (newFigureGroup) and a rebuild's overlay of staged rows (readOverlayGroups)
// all fold them in: by day, and within a day by the key each row is stored under (WindowRow.owner),
// then by its repository (WindowRow.repo) and then by its series. A day of one series can hold
// several rows: a project's own row and an owner key's (owners.go), and in the cut of the project's
// default repository its own row of the repository and its own row stored under
// legionfacts.DefaultRepo (repoCut). No two rows of one eval and range share all four, so the order
// is total, and a sum of floats, whose bits depend on the order of its parts (1e16, -1e16 and 1 sum
// to 1 in that order and to 0 in another), answers the same bits whatever order the database
// answered the rows in.
func sortWindowRows(rows []WindowRow) {
	slices.SortFunc(rows, func(a, b WindowRow) int {
		if order := a.Part.Day.Compare(b.Part.Day); order != 0 {
			return order
		}
		return cmp.Or(strings.Compare(a.owner, b.owner), strings.Compare(a.repo, b.repo), strings.Compare(a.Part.Series, b.Part.Series))
	})
}

// foldDay is the fold of parts, one day's parts of definition's eval, of each of headline, its
// headline series, that holds a part that day, over that day alone: a day of a headline series'
// per-day array, which a read of the day rows (SeriesValuesOf) and the stored figures
// (figureGroup.dayFigure) both answer through it. With no headline series it is empty.
func foldDay(definition Definition, headline []Series, parts []measures.DayPart, day time.Time) map[string]Statistics {
	kept := map[string]Statistics{}
	if len(headline) == 0 {
		return kept
	}
	folded := FoldStatistics(definition, headline, parts, Range{From: day, To: day.AddDate(0, 0, 1)})
	for _, series := range headline {
		if holds(parts, series.Series) {
			kept[series.Series] = folded[series.Series]
		}
	}
	return kept
}

// dayFold is one day of a window that holds a row of the eval: the fold of that day's rows alone of
// each headline series that holds a row that day (foldDay).
type dayFold struct {
	day    time.Time
	folded map[string]Statistics
}

// valuesOf is each of listed, an eval's listed series, with its values: windowed and compared, the
// folds of the window's rows and the compare range's (nil with no compare range), via, the
// via-repository counts of the window's rows, and daily, the window's days that hold a row of the
// eval, in day order; read against firsts, each series' first day, as SeriesValuesOf says. It is
// the statement both reads make of their folds, SeriesValuesOf's of the day rows and
// StoredFigures.SeriesValues' of the stored figures.
func valuesOf(listed []Series, windowed map[string]Statistics, via map[string]int, compared map[string]Statistics, daily []dayFold,
	firsts map[string]time.Time, window Range, compare *Range) []SeriesValues {
	values := make([]SeriesValues, len(listed))
	for index, series := range listed {
		first, held := firsts[series.Series]
		value := SeriesValues{Series: series, Window: windowed[series.Series], ViaRepository: via[series.Series], Days: []DayPoint{}}
		if held {
			value.DataSince = new(first.Format(time.DateOnly))
		}
		if !held || window.From.Before(first) {
			value.Window = noData(value.Window)
		}
		if compare != nil {
			value.Compare = compared[series.Series]
			if !held || compare.From.Before(first) {
				value.Compare = noData(value.Compare)
			}
			value.Change = changeOf(value.Window, value.Compare)
		}
		if series.Role == RoleHeadline {
			for _, day := range daily {
				if stats, found := day.folded[series.Series]; found {
					value.Days = append(value.Days, DayPoint{Day: day.day.Format(time.DateOnly), Statistics: stats})
				}
			}
		}
		values[index] = value
	}
	return values
}

// noData is stats with every statistic null: a range the series has no data for.
func noData(stats Statistics) Statistics {
	return blank(slices.Collect(maps.Keys(stats))...)
}

// changeOf is each statistic of window less compare's, null where either has no value, or where
// the difference of two finite figures is past what a float holds, which no answer can write.
func changeOf(window, compare Statistics) Statistics {
	change := make(Statistics, len(window))
	for name, value := range window {
		change[name] = nil
		if against := compare[name]; value != nil && against != nil {
			if difference := *value - *against; !math.IsInf(difference, 0) {
				change[name] = &difference
			}
		}
	}
	return change
}

// WindowStatistics is each of series' statistics over the window, by series name: what the eval's
// verdict judges (VerdictOf).
func WindowStatistics(series []SeriesValues) map[string]Statistics {
	window := make(map[string]Statistics, len(series))
	for _, one := range series {
		window[one.Series.Series] = one.Window
	}
	return window
}

// HeadlineStatistic is the statistic series, one of definition's, is stated by where nothing
// names one: the figure its fold leads with. A computed eval's source names its own
// (computedSources), since each fold states statistics of its own, which its series' agg need
// not name (deploy-frequency's `deploys` sums its days and states per_day, deploys, with_prs and
// with_prs_per_day, no `sum`); a logged series' agg names it. An eval's target with no judges
// judges its primary series' headline statistic.
func HeadlineStatistic(definition Definition, series Series) string {
	if definition.Computed() {
		if source, held := computedSources[definition.Source]; held {
			base, _ := metrics.SplitCutSeries(series.Series)
			return source.headline(base)
		}
	}
	switch series.Agg {
	case AggSum:
		return "sum"
	case AggRatio:
		return "rate"
	case AggQuantile:
		return "median"
	case AggMax:
		return "max"
	case AggWeightedMean:
		return "mean"
	}
	return "value"
}

// stated is the headline of a fold that leads every series with one statistic.
func stated(statistic string) func(string) string {
	return func(string) string { return statistic }
}

// oneSeries is the fold of an eval stated by one series alone: each other name answers no figure.
func oneSeries(series string, fold func(parts []measures.DayPart, days Range) Statistics) seriesFold {
	return func(parts []measures.DayPart, days Range, names []string) map[string]Statistics {
		folded := make(map[string]Statistics, len(names))
		for _, name := range names {
			folded[name] = Statistics{}
			if name == series {
				folded[name] = fold(parts, days)
			}
		}
		return folded
	}
}

// byCut is the fold of a Legion eval, whose every series is stored in its cuts (metrics.CutSeries):
// fold runs once per cut the names hold, and read states one series, by its name before the cut,
// from that cut's fold.
func byCut[F any](fold func([]measures.DayPart, metrics.Scope) F, read func(F, string) Statistics) seriesFold {
	return func(parts []measures.DayPart, _ Range, names []string) map[string]Statistics {
		cuts := map[metrics.Scope]F{}
		folded := make(map[string]Statistics, len(names))
		for _, name := range names {
			base, cut := metrics.SplitCutSeries(name)
			answer, done := cuts[cut]
			if !done {
				answer = fold(parts, cut)
				cuts[cut] = answer
			}
			folded[name] = read(answer, base)
		}
		return folded
	}
}

// blank is a Statistics naming each of names with no figure.
func blank(names ...string) Statistics {
	stats := make(Statistics, len(names))
	for _, name := range names {
		stats[name] = nil
	}
	return stats
}

// holds reports whether parts hold a part of series: a fold over none reads "no data".
func holds(parts []measures.DayPart, series string) bool {
	return slices.ContainsFunc(parts, func(part measures.DayPart) bool { return part.Series == series })
}

// deploy-frequency's `deploys` (measures.FoldDeployFrequency): per_day, deploys, with_prs,
// with_prs_per_day.
func deployFrequencyStatistics(parts []measures.DayPart, days Range) Statistics {
	return deployFrequencyOf(measures.FoldDeployFrequency(parts, measures.Window{From: days.From, To: days.To}))
}

// deployFrequencyOf is deploy-frequency's `deploys` statistics of a folded deploy frequency, every
// figure nil where folded is (no part).
func deployFrequencyOf(folded *measures.DeployFrequency) Statistics {
	stats := blank("per_day", "deploys", "with_prs", "with_prs_per_day")
	if folded != nil {
		stats["per_day"], stats["deploys"] = new(folded.PerDay), new(float64(folded.SuccessfulDeploys))
		stats["with_prs"], stats["with_prs_per_day"] = new(float64(folded.DeploysWithPRs)), new(folded.WithPRsPerDay)
	}
	return stats
}

// deploy-run-success's `runs` (measures.FoldDeployRunSuccess): rate, concluded,
// reached_production, cancelled.
func deployRunSuccessStatistics(parts []measures.DayPart, _ Range) Statistics {
	return deployRunSuccessOf(measures.FoldDeployRunSuccess(parts))
}

// deployRunSuccessOf is deploy-run-success's `runs` statistics of a folded run success, every
// figure nil where folded is (no part).
func deployRunSuccessOf(folded *measures.DeployRunSuccess) Statistics {
	stats := blank("rate", "concluded", "reached_production", "cancelled")
	if folded != nil {
		stats["rate"], stats["concluded"] = folded.Rate, new(float64(folded.Concluded))
		stats["reached_production"], stats["cancelled"] = new(float64(folded.ReachedProduction)), new(float64(folded.Cancelled))
	}
	return stats
}

// A lead time's `minutes` (measures.FoldLeadTime): n, median, p90, max.
func leadTimeStatistics(parts []measures.DayPart, _ Range) Statistics {
	if !holds(parts, measures.SeriesMinutes) {
		return blank("n", "median", "p90", "max")
	}
	return LeadTimeStatistics(measures.FoldLeadTime(parts))
}

// LeadTimeStatistics is a lead time eval's `minutes` statistics of a folded lead time (n, median,
// p90, max): what merge-to-production and pr-open-to-merge state of the cell the Pipeline's two
// lead-time cards fold, which those cards are judged by (VerdictOf).
func LeadTimeStatistics(cell measures.LeadTimeCell) Statistics {
	return Statistics{"n": new(float64(cell.N)), "median": cell.MedianMinutes, "p90": cell.P90Minutes, "max": cell.MaxMinutes}
}

// rework-share's `prs` (measures.FoldReworkShare): rate.
func reworkShareStatistics(parts []measures.DayPart, _ Range) Statistics {
	return Statistics{"rate": measures.FoldReworkShare(parts)}
}

// change-fail-rate's `per_deploy` (measures.FoldChangeFailRate): rate, failed (the deploys with a
// counted failure) and total (every deploy).
func changeFailRateStatistics(parts []measures.DayPart, _ Range) Statistics {
	return changeFailRateOf(measures.FoldChangeFailRate(parts))
}

// changeFailRateOf is change-fail-rate's `per_deploy` statistics of a folded change fail rate,
// every figure nil where folded is (no part was folded: "no data").
func changeFailRateOf(folded *measures.ChangeFailRate) Statistics {
	stats := blank("rate", "failed", "total")
	if folded != nil {
		stats["rate"], stats["failed"], stats["total"] = folded.Rate, new(float64(folded.Failed)), new(float64(folded.Total))
	}
	return stats
}

// failed-deploy-recovery's `minutes` (measures.FoldRecovery): n (the restores timed), median and
// not_restored, every figure nil where no part was folded, and the median nil where no failure was
// restored.
func recoveryStatistics(parts []measures.DayPart, _ Range) Statistics {
	stats := blank("n", "median", "not_restored")
	if folded := measures.FoldRecovery(parts); folded != nil {
		stats["n"], stats["median"], stats["not_restored"] = new(float64(folded.N)), folded.MedianMinutes, new(float64(folded.NotRestored))
	}
	return stats
}

// deployment-rework-rate's `deploys` (measures.FoldReworkRate): rate, restoring (the deploys that
// restored a counted failure) and total (every deploy), every figure nil where no part was folded.
func reworkRateStatistics(parts []measures.DayPart, _ Range) Statistics {
	stats := blank("rate", "restoring", "total")
	if folded := measures.FoldReworkRate(parts); folded != nil {
		stats["rate"], stats["restoring"], stats["total"] = folded.Rate, new(float64(folded.Restoring)), new(float64(folded.Total))
	}
	return stats
}

// A CI eval's `minutes` (measures.FoldTiming): n, median, p90, success, failure, cancelled,
// success_rate.
func timingStatistics(parts []measures.DayPart, _ Range) Statistics {
	stats := blank("n", "median", "p90", "success", "failure", "cancelled", "success_rate")
	if holds(parts, measures.SeriesMinutes) {
		cell := measures.FoldTiming(parts)
		stats["n"], stats["median"], stats["p90"] = new(float64(cell.N)), cell.MedianMinutes, cell.P90Minutes
		stats["success"], stats["failure"], stats["cancelled"] = new(float64(cell.Success)), new(float64(cell.Failure)), new(float64(cell.Cancelled))
		stats["success_rate"] = cell.SuccessRate
	}
	return stats
}

// sampleStatistics is a sample series of a Legion fold (metrics.SampleFold): n, median and p90,
// and sum under sumName where the series states its sum ("" for none).
func sampleStatistics(fold *metrics.SampleFold, sumName string) Statistics {
	stats := blank("n", "median", "p90")
	if sumName != "" {
		stats[sumName] = nil
	}
	if fold != nil {
		stats["n"], stats["median"], stats["p90"] = new(float64(fold.N)), fold.Median, fold.P90
		if sumName != "" {
			stats[sumName] = new(fold.Sum)
		}
	}
	return stats
}

// legion-work-time's `hours` (metrics.FoldWorkTime): n, median, p90.
func workTimeStatistics(fold *metrics.SampleFold, base string) Statistics {
	if base != metrics.SeriesHours {
		return Statistics{}
	}
	return sampleStatistics(fold, "")
}

// legion-slot-wait's `hours` (metrics.FoldSlotWait): hours (the sum), n, median, p90.
func slotWaitStatistics(fold *metrics.SampleFold, base string) Statistics {
	if base != metrics.SeriesHours {
		return Statistics{}
	}
	return sampleStatistics(fold, "hours")
}

// legion-idle-time's `<role>` (metrics.FoldIdleTime): hours (the sum), n, median, p90.
func idleTimeStatistics(fold map[string]metrics.SampleFold, role string) Statistics {
	if held, found := fold[role]; found {
		return sampleStatistics(&held, "hours")
	}
	return sampleStatistics(nil, "hours")
}

// legion-phase-time's `<role>` (metrics.FoldPhaseTime): one bar's n, issues, hours,
// median_per_issue and five segments, and idle_share, its cut's idle hours over its hours, which
// every bar of the cut states so a judge can name it on any.
func phaseTimeStatistics(fold *metrics.PhaseTimeFold, role string) Statistics {
	stats := blank("n", "issues", "hours", "median_per_issue", "idle_share")
	for segment := range (metrics.Segments{}).ByName() {
		stats[string(segment)] = nil
	}
	if fold == nil {
		return stats
	}
	stats["idle_share"] = fold.IdleShare
	bar, found := fold.Bars[role]
	if !found {
		return stats
	}
	stats["n"], stats["issues"], stats["hours"], stats["median_per_issue"] = new(float64(bar.N)), new(float64(bar.Issues)), new(bar.Hours), bar.MedianPerIssue
	for segment, hours := range bar.Segments.ByName() {
		stats[string(segment)] = new(hours)
	}
	return stats
}

// legion-rework's three series (metrics.FoldRework): `review_rounds` (n, median, p90),
// `sent_back` (sent_back the numerator, n the denominator, share) and `hours_after_first_review`
// (hours the sum, n, median, p90).
func reworkStatistics(fold *metrics.ReworkFold, base string) Statistics {
	if fold == nil {
		fold = &metrics.ReworkFold{}
	}
	switch base {
	case metrics.SeriesReviewRounds:
		return sampleStatistics(fold.ReviewRounds, "")
	case metrics.SeriesHoursAfterFirstReview:
		return sampleStatistics(fold.HoursAfterFirstReview, "hours")
	case metrics.SeriesSentBack:
		stats := blank("sent_back", "n", "share")
		if fold.SentBack != nil {
			stats["sent_back"], stats["n"], stats["share"] = new(float64(fold.SentBack.Num)), new(float64(fold.SentBack.Den)), fold.SentBack.Share
		}
		return stats
	}
	return Statistics{}
}

// legion-splitting's series (metrics.FoldSplitting): each `filed:<class>` and `finished`, issues;
// `roots`, issues (the finished roots), children and children_per_root; and
// `hours_with_children` and `hours_without_children`, n, median and p90.
func splittingStatistics(fold *metrics.SplittingFold, base string) Statistics {
	if class, filed := strings.CutPrefix(base, metrics.FilingSeriesPrefix); filed {
		stats := blank("issues")
		if fold != nil {
			stats["issues"] = new(float64(fold.Filed[class]))
		}
		return stats
	}
	var roots *metrics.RootsFold
	if fold != nil {
		roots = fold.Roots
	}
	switch base {
	case metrics.SeriesFinished:
		stats := blank("issues")
		if fold != nil {
			stats["issues"] = new(float64(fold.Finished))
		}
		return stats
	case metrics.SeriesRoots:
		stats := blank("issues", "children", "children_per_root")
		if roots != nil {
			stats["issues"], stats["children"], stats["children_per_root"] = new(float64(roots.FinishedRoots)), new(float64(roots.Children)), roots.ChildrenPerRoot
		}
		return stats
	case metrics.SeriesHoursWithChildren, metrics.SeriesHoursWithoutChildren:
		var samples *metrics.SampleFold
		if roots != nil {
			samples = roots.HoursWithoutChildren
			if base == metrics.SeriesHoursWithChildren {
				samples = roots.HoursWithChildren
			}
		}
		return sampleStatistics(samples, "")
	}
	return Statistics{}
}

// readsSamples reports whether the fold of series, one of definition's eval, reads its day rows'
// sample lists, so a window read sends them only where it does (DayWindowIn). A computed eval's fold
// reads what its builders store, and they store a sample list only for a series whose fold merges
// one; a logged series' agg reads them for a quantile alone (aggStatistics), though the readings
// write keeps every value whatever the agg, so a later change of agg still folds every day.
func readsSamples(definition Definition, series Series) bool {
	return definition.Computed() || series.Agg == AggQuantile
}

// aggStatistics is a logged series folded by its agg over the parts of series among parts, every
// figure nil where they hold none:
//
//   - sum: sum, n;
//   - ratio: rate, the summed weighted values over the summed weights (a reading's value weighted
//     by its n, or 1), n;
//   - weighted_mean: mean, the same quotient, n;
//   - quantile: n, sum, median, p90 and max, over the merge of the days' samples
//     (measures.Quantile);
//   - max: max, n;
//   - last: value, the last value of the part measured last, n. Of parts whose last values were
//     measured at one moment (two repositories' rows of one day, in a read of repositories) it is
//     the one written later (lastBefore: the write that began later, then the later reading of
//     one write), the order a logged eval's row of the whole project breaks the same tie by
//     (addParts), whatever order the parts come in.
func aggStatistics(agg string, parts []measures.DayPart, series string) Statistics {
	var stats Statistics
	switch agg {
	case AggSum:
		stats = blank("sum", "n")
	case AggRatio:
		stats = blank("rate", "n")
	case AggWeightedMean:
		stats = blank("mean", "n")
	case AggQuantile:
		stats = blank("n", "sum", "median", "p90", "max")
	case AggMax:
		stats = blank("max", "n")
	default:
		stats = blank("value", "n")
	}
	folded := measures.FoldSeries(parts, series)
	if folded.Parts == 0 {
		return stats
	}
	var n int
	var largest *float64
	// last is the part of the reading that holds the last value, held once a part holds one.
	var last dayPart
	held := false
	for _, part := range parts {
		if part.Series != series {
			continue
		}
		n += part.N
		if part.Max != nil && (largest == nil || *part.Max > *largest) {
			largest = part.Max
		}
		if candidate, ok := lastOf(part); ok && (!held || last.lastBefore(candidate)) {
			last, held = candidate, true
		}
	}
	stats["n"] = new(float64(n))
	switch agg {
	case AggSum:
		stats["sum"] = new(folded.Sum)
	case AggRatio, AggWeightedMean:
		name := "rate"
		if agg == AggWeightedMean {
			name = "mean"
		}
		if folded.Den != 0 {
			stats[name] = new(folded.Num / folded.Den)
		}
	case AggQuantile:
		stats["sum"], stats["max"] = new(folded.Sum), largest
		if samples := measures.MergedSamples(parts, series); samples.Len() > 0 {
			stats["median"], stats["p90"] = new(samples.Quantile(0.5)), new(samples.Quantile(0.9))
		}
	case AggMax:
		stats["max"] = largest
	default:
		if held {
			stats["value"] = new(last.value)
		}
	}
	return stats
}
