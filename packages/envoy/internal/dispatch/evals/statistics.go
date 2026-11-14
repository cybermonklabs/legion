package evals

import (
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// An eval read states each listed series' statistics for a range of days: the figures a card
// shows and a target judges, a judge naming one as `<series>.<stat>`. They are folded from the
// stored day rows of the range (DayWindow), never from the population again (R12):
//
//   - a computed eval through the one fold its source names (computedFolds), which is the fold the
//     delivery and Legion routes answer through too (R7), so one rule has one statement;
//   - a logged eval through each series' own fold, its agg (aggStatistics), since no function
//     computes it.
//
// computedFolds is the one table from a source to its fold. Every eval of the seeded catalogue
// names a source it holds, which init checks when the package starts, so a computed eval whose
// fold is missing fails every binary and every test that links this package rather than reading
// "no data" on a page.

// Statistics is one series' figures over one range, by name. A figure the range holds no value of
// is nil: "no data", never 0.
type Statistics map[string]*float64

// seriesFold folds an eval's day parts of one range into the statistics of each of series, by
// series name; days is the range, whole UTC days, which a per-day figure divides by. A series the
// fold has no figure of answers an empty Statistics.
type seriesFold func(parts []measures.DayPart, days Range, series []string) map[string]Statistics

// computedFolds is each computed eval's fold, by its source: the exported function the source
// names, read out as statistics.
var computedFolds = map[string]seriesFold{
	SourceFoldDeployFrequency:   oneSeries(measures.SeriesDeploys, deployFrequencyStatistics),
	SourceFoldDeployRunSuccess:  oneSeries(measures.SeriesRuns, deployRunSuccessStatistics),
	SourceFoldLeadTime:          oneSeries(measures.SeriesMinutes, leadTimeStatistics),
	SourceFoldReworkShare:       oneSeries(measures.SeriesPRs, reworkShareStatistics),
	SourceFoldChangeFailureRate: changeFailureRateStatistics,
	SourceFoldTimeToRestore:     oneSeries(measures.SeriesMinutes, timeToRestoreStatistics),
	SourceFoldTiming:            oneSeries(measures.SeriesMinutes, timingStatistics),
	// unowned-p0's day row is the count at the day's last refresh, which delivery.ListUnownedP0
	// counts; a window reads its newest day's, the `last` fold.
	SourceListUnownedP0: oneSeries(SeriesUnownedP0, func(parts []measures.DayPart, _ Range) Statistics {
		return aggStatistics(AggLast, parts, SeriesUnownedP0)
	}),
	SourceFoldWorkTime:  byCut(metrics.FoldWorkTime, workTimeStatistics),
	SourceFoldPhaseTime: byCut(metrics.FoldPhaseTime, phaseTimeStatistics),
	SourceFoldIdleTime:  byCut(metrics.FoldIdleTime, idleTimeStatistics),
	SourceFoldSlotWait:  byCut(metrics.FoldSlotWait, slotWaitStatistics),
	SourceFoldRework:    byCut(metrics.FoldRework, reworkStatistics),
	SourceFoldSplitting: byCut(metrics.FoldSplitting, splittingStatistics),
}

// Every catalogue eval's source has its fold: checked once, when the package starts.
func init() {
	for _, entry := range catalogue {
		if _, held := computedFolds[entry.definition.Source]; !held {
			panic("evals: the catalogue eval " + entry.definition.Slug + " names the source " +
				entry.definition.Source + ", which no fold of computedFolds answers")
		}
	}
}

// FoldStatistics is the statistics of each of series over parts, the day parts of one range of
// days of definition's eval (those of other evals are passed over): by the fold definition's
// source names for a computed eval, by each series' agg for a logged one. A computed eval whose
// source names no fold of this package (one a register write named) answers no figure.
func FoldStatistics(definition Definition, series []Series, parts []measures.DayPart, days Range) map[string]Statistics {
	names := make([]string, len(series))
	for index, one := range series {
		names[index] = one.Series
	}
	if definition.Computed() {
		fold, held := computedFolds[definition.Source]
		if !held {
			folded := make(map[string]Statistics, len(names))
			for _, name := range names {
				folded[name] = Statistics{}
			}
			return folded
		}
		return fold(parts, days, names)
	}
	folded := make(map[string]Statistics, len(series))
	for _, one := range series {
		folded[one.Series] = aggStatistics(one.Agg, parts, one.Series)
	}
	return folded
}

// HeadlineStatistic is the statistic series, one of definition's, is stated by where nothing
// names one: the figure its fold leads with. A computed eval's fold names its own
// (computedHeadlines), since each fold states statistics of its own, which its series' agg need
// not name (deploy-frequency's `deploys` sums its days and states per_day, deploys, with_prs and
// with_prs_per_day, no `sum`); a logged series' agg names it. An eval's target with no judges
// judges its primary series' headline statistic.
func HeadlineStatistic(definition Definition, series Series) string {
	if definition.Computed() {
		if headline, held := computedHeadlines[definition.Source]; held {
			base, _ := metrics.SplitCutSeries(series.Series)
			return headline(base)
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

// computedHeadlines is each computed fold's headline statistic of a series, by the fold's source
// and the series' name before its cut: a statistic the fold states of that series
// (TestEveryCatalogueSeriesIsStatedByItsHeadline holds each to it).
var computedHeadlines = map[string]func(series string) string{
	SourceFoldDeployFrequency:   stated("per_day"),
	SourceFoldDeployRunSuccess:  stated("rate"),
	SourceFoldLeadTime:          stated("median"),
	SourceFoldReworkShare:       stated("rate"),
	SourceFoldChangeFailureRate: stated("rate"),
	SourceFoldTimeToRestore:     stated("median"),
	SourceFoldTiming:            stated("median"),
	SourceListUnownedP0:         stated("value"),
	SourceFoldWorkTime:          stated("median"),
	SourceFoldPhaseTime:         stated("median_per_issue"),
	SourceFoldIdleTime:          stated("median"),
	SourceFoldSlotWait:          stated("median"),
	SourceFoldRework: func(series string) string {
		if series == metrics.SeriesSentBack {
			return "share"
		}
		return "median"
	},
	SourceFoldSplitting: func(series string) string {
		if series == metrics.SeriesHoursWithChildren || series == metrics.SeriesHoursWithoutChildren {
			return "median"
		}
		return "issues"
	},
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
	stats := blank("per_day", "deploys", "with_prs", "with_prs_per_day")
	if folded := measures.FoldDeployFrequency(parts, measures.Window{From: days.From, To: days.To}); folded != nil {
		stats["per_day"], stats["deploys"] = new(folded.PerDay), new(float64(folded.SuccessfulDeploys))
		stats["with_prs"], stats["with_prs_per_day"] = new(float64(folded.DeploysWithPRs)), new(folded.WithPRsPerDay)
	}
	return stats
}

// deploy-run-success's `runs` (measures.FoldDeployRunSuccess): rate, concluded,
// reached_production, cancelled.
func deployRunSuccessStatistics(parts []measures.DayPart, _ Range) Statistics {
	stats := blank("rate", "concluded", "reached_production", "cancelled")
	if folded := measures.FoldDeployRunSuccess(parts); folded != nil {
		stats["rate"], stats["concluded"] = folded.Rate, new(float64(folded.Concluded))
		stats["reached_production"], stats["cancelled"] = new(float64(folded.ReachedProduction)), new(float64(folded.Cancelled))
	}
	return stats
}

// A lead time's `minutes` (measures.FoldLeadTime): n, median, p90, max.
func leadTimeStatistics(parts []measures.DayPart, _ Range) Statistics {
	stats := blank("n", "median", "p90", "max")
	if holds(parts, measures.SeriesMinutes) {
		folded := measures.FoldLeadTime(parts)
		stats["n"], stats["median"], stats["p90"], stats["max"] = new(float64(folded.N)), folded.MedianMinutes, folded.P90Minutes, folded.MaxMinutes
	}
	return stats
}

// rework-share's `prs` (measures.FoldReworkShare): rate.
func reworkShareStatistics(parts []measures.DayPart, _ Range) Statistics {
	return Statistics{"rate": measures.FoldReworkShare(parts)}
}

// change-fail-rate's three shares (measures.FoldChangeFailureRate): `per_deploy` and `per_pr`,
// each its rate, confirmed, pending, rejected and total (and per_pr its reverts), and
// `upper_bound`, its rate and confirmed_or_pending.
func changeFailureRateStatistics(parts []measures.DayPart, _ Range, names []string) map[string]Statistics {
	perDeploy := blank("rate", "confirmed", "pending", "rejected", "total")
	perPR := blank("rate", "confirmed", "pending", "rejected", "reverts", "total")
	upperBound := blank("rate", "confirmed_or_pending")
	if folded := measures.FoldChangeFailureRate(parts); folded != nil {
		flagged := func(stats Statistics, flags measures.FlagCounts) {
			stats["rate"], stats["confirmed"], stats["pending"] = new(flags.Rate), new(float64(flags.Confirmed)), new(float64(flags.Pending))
			stats["rejected"], stats["total"] = new(float64(flags.Rejected)), new(float64(flags.Total))
		}
		flagged(perDeploy, folded.PerDeploy.FlagCounts)
		flagged(perPR, folded.PerPR)
		perPR["reverts"] = new(float64(folded.PerPR.Reverts))
		upperBound["rate"], upperBound["confirmed_or_pending"] = new(folded.PerDeploy.UpperBoundRate), new(float64(folded.PerDeploy.ConfirmedOrPending))
	}
	shares := map[string]Statistics{measures.SeriesPerDeploy: perDeploy, measures.SeriesPerPR: perPR, measures.SeriesUpperBound: upperBound}
	folded := make(map[string]Statistics, len(names))
	for _, name := range names {
		folded[name] = shares[name]
		if folded[name] == nil {
			folded[name] = Statistics{}
		}
	}
	return folded
}

// failed-deploy-recovery's `minutes` (measures.FoldTimeToRestore): median.
func timeToRestoreStatistics(parts []measures.DayPart, _ Range) Statistics {
	return Statistics{"median": measures.FoldTimeToRestore(parts).MedianMinutes}
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
// sample lists, so a window read sends them only where it does (DayWindow). A computed eval's fold
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
//   - last: value, the last value of the part measured last, n.
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
	var largest, last *float64
	var lastAt *time.Time
	for _, part := range parts {
		if part.Series != series {
			continue
		}
		n += part.N
		if part.Max != nil && (largest == nil || *part.Max > *largest) {
			largest = part.Max
		}
		if part.LastValue != nil && part.LastAt != nil && (lastAt == nil || !part.LastAt.Before(*lastAt)) {
			last, lastAt = part.LastValue, part.LastAt
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
		if samples := measures.MergedSamples(parts, series); len(samples) > 0 {
			stats["median"], stats["p90"] = new(measures.Quantile(samples, 0.5)), new(measures.Quantile(samples, 0.9))
		}
	case AggMax:
		stats["max"] = largest
	default:
		stats["value"] = last
	}
	return stats
}
