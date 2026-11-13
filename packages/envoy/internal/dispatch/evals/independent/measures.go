package independent

import (
	"slices"
	"time"
)

// measures.go is the six measures over the issues that finished in a window, in each of the three
// cuts, stated by the statistic names the eval list route answers (GET
// /api/v1/projects/{key}/evals), so a comparison of the two reads value by value.
//
// "Each measure counts the project's issues that finished in the window. Each measure is three
// series, cut by the stored root flag: all issues, roots alone and children alone." An issue
// finished in the window "when its last move to `done` (its close) falls in the window, it is
// still `done`, and it has Legion phase work". "Medians and p90s use linear interpolation between
// the closest ranks", a share is the summed numerator over the summed denominator, and a count or
// an hours total is a sum.
//
// A measure reads "no data", every statistic null, for a window that starts before its first
// input day (dispatch://INFRA-2916, R12): the UTC day of the first finished issue, and for
// splitting of the first filing where that is earlier. Idle time by role and slot wait read "no
// data" for a window that starts before the project's first claim and first slot observation (R6,
// D3): their input begins on the first whole UTC day after that observation, so their first input
// day is the first day from then on that an issue finished.

// The six measures, by the slugs the eval routes name them by.
const (
	SlugWorkTime  = "legion-work-time"
	SlugPhaseTime = "legion-phase-time"
	SlugIdleTime  = "legion-idle-time"
	SlugSlotWait  = "legion-slot-wait"
	SlugRework    = "legion-rework"
	SlugSplitting = "legion-splitting"
)

// Slugs is the six measures in the order a reading states them.
var Slugs = []string{SlugWorkTime, SlugPhaseTime, SlugIdleTime, SlugSlotWait, SlugRework, SlugSplitting}

// The three cuts of every measure: every finished issue, the issues that are their own tree's
// root, and the rest.
const (
	CutAll      = "all"
	CutRoots    = "roots"
	CutChildren = "children"
)

// Cuts is the three cuts in the order a reading states them.
var Cuts = []string{CutAll, CutRoots, CutChildren}

// SeriesName is a series of a measure in one cut, as the eval routes name it: the series itself
// for the all cut, `<series>:roots` and `<series>:children` for the other two.
func SeriesName(series, cut string) string {
	if cut == CutAll {
		return series
	}
	return series + ":" + cut
}

// inCut is whether an issue (or a filing's filed issue) that is or is not its tree's root counts
// in cut.
func inCut(cut string, root bool) bool {
	switch cut {
	case CutRoots:
		return root
	case CutChildren:
		return !root
	}
	return true
}

// Statistics is one series' figures over the window, by the names the eval routes give them; a
// figure with no value is nil, which the routes answer as null.
type Statistics map[string]*float64

// CutValues is one measure in one cut: how many finished issues of the window the cut holds, and
// each series of the measure in the cut by its name (SeriesName).
type CutValues struct {
	Cut    string                `json:"cut"`
	Issues int                   `json:"issues"`
	Series map[string]Statistics `json:"series"`
}

// Measure is one measure over the window: its first input day (DataSince, a UTC date, nil where it
// has none), whether it reads "no data" over the window, and its three cuts.
type Measure struct {
	Slug      string      `json:"slug"`
	DataSince *string     `json:"data_since"`
	NoData    bool        `json:"no_data"`
	Cuts      []CutValues `json:"cuts"`
}

// sampleStatistics is a series of one value per issue, by the names the eval routes state it
// with: n, median and p90, and its sum under sumName where the series states one ("" for none).
func sampleStatistics(samples []float64, sumName string) Statistics {
	stats := Statistics{"n": figure(float64(len(samples))), "median": nil, "p90": nil}
	if sumName != "" {
		sum := 0.0
		for _, value := range samples {
			sum += value
		}
		stats[sumName] = figure(sum)
	}
	if len(samples) > 0 {
		sorted := slices.Sorted(slices.Values(samples))
		stats["median"], stats["p90"] = figure(quantile(sorted, 0.5)), figure(quantile(sorted, 0.9))
	}
	return stats
}

// quantile is the q-th quantile of a sorted list by "linear interpolation between the closest
// ranks": the rank (n−1)·q, read between the two values either side of it.
func quantile(sorted []float64, q float64) float64 {
	rank := float64(len(sorted)-1) * q
	below := int(rank)
	if below+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[below] + (sorted[below+1]-sorted[below])*(rank-float64(below))
}

// figure is one value as a reading states it, rounded as every figure is.
func figure(value float64) *float64 {
	held := rounded(value)
	return &held
}

// share is a numerator over a denominator, nil over none.
func share(numerator, denominator float64) *float64 {
	if denominator == 0 {
		return nil
	}
	return figure(numerator / denominator)
}

// measuring is one window's measures being worked out: the finished issues' values and the
// window's filings, each with what it is cut by, and what each measure's first input day reads.
type measuring struct {
	window   Window
	finished []values
	filed    []classified
	// closes is the close of every issue of the project that finished, in any window, oldest
	// first; firstFiling is the project's first filing, and firstClaim and firstSlot its first
	// claim and first slot observation, each nil where there is none.
	closes      []time.Time
	firstFiling *time.Time
	firstClaim  *time.Time
	firstSlot   *time.Time
}

// classified is one filing of the window: its class, and whether its filed issue is its tree's
// root now ("the toggle reads each filed issue's place in the tree as it stands now, while its
// class reads the parent it had at its issue.created event").
type classified struct {
	class string
	root  bool
}

// measures is the six measures over the window.
func (m measuring) measures() []Measure {
	answer := make([]Measure, 0, len(Slugs))
	for _, slug := range Slugs {
		measure := Measure{Slug: slug}
		since := m.dataSince(slug)
		if since != nil {
			measure.DataSince = new(since.Format(time.DateOnly))
		}
		measure.NoData = since == nil || m.window.From.Before(*since)
		for _, cut := range Cuts {
			stated := CutValues{Cut: cut, Series: map[string]Statistics{}}
			var finished []values
			for _, one := range m.finished {
				if inCut(cut, one.root) {
					finished = append(finished, one)
				}
			}
			stated.Issues = len(finished)
			for series, stats := range m.series(slug, cut, finished) {
				if measure.NoData {
					for name := range stats {
						stats[name] = nil
					}
				}
				stated.Series[SeriesName(series, cut)] = stats
			}
			measure.Cuts = append(measure.Cuts, stated)
		}
		answer = append(answer, measure)
	}
	return answer
}

// dataSince is a measure's first input day: the UTC day of the first issue that finished from the
// day its input begins, and for splitting of the first filing where that is earlier; nil for a
// measure with no input at all.
func (m measuring) dataSince(slug string) *time.Time {
	var begins *time.Time
	switch slug {
	case SlugIdleTime:
		if m.firstClaim == nil {
			return nil
		}
		begins = new(nextMidnight(*m.firstClaim))
	case SlugSlotWait:
		if m.firstSlot == nil {
			return nil
		}
		begins = new(nextMidnight(*m.firstSlot))
	}
	var first *time.Time
	for _, closed := range m.closes {
		if begins == nil || !closed.Before(*begins) {
			first = new(dayOf(closed))
			break
		}
	}
	if slug == SlugSplitting && m.firstFiling != nil {
		if day := dayOf(*m.firstFiling); first == nil || day.Before(*first) {
			first = &day
		}
	}
	return first
}

// series is each series of slug in cut over finished, the cut's finished issues of the window,
// by its name before the cut.
func (m measuring) series(slug, cut string, finished []values) map[string]Statistics {
	switch slug {
	case SlugWorkTime:
		// "Hours from the issue's first worker event to its close": an issue with no first worker
		// event has no work time to count.
		var hours []float64
		for _, one := range finished {
			if one.started {
				hours = append(hours, one.workHours)
			}
		}
		return map[string]Statistics{"hours": sampleStatistics(hours, "")}
	case SlugPhaseTime:
		return phaseTime(finished)
	case SlugIdleTime:
		answer := map[string]Statistics{}
		for _, one := range bars {
			if one.role == noRole {
				continue
			}
			hours := make([]float64, 0, len(finished))
			for _, issue := range finished {
				hours = append(hours, issue.idle[one.role])
			}
			answer[one.role] = sampleStatistics(hours, "hours")
		}
		return answer
	case SlugSlotWait:
		hours := make([]float64, 0, len(finished))
		for _, one := range finished {
			hours = append(hours, one.slotWait)
		}
		return map[string]Statistics{"hours": sampleStatistics(hours, "hours")}
	case SlugRework:
		rounds := make([]float64, 0, len(finished))
		after := make([]float64, 0, len(finished))
		sent := 0.0
		for _, one := range finished {
			rounds = append(rounds, float64(one.reviewRounds))
			after = append(after, one.afterFirstReview)
			if one.sentBack {
				sent++
			}
		}
		issues := float64(len(finished))
		return map[string]Statistics{
			"review_rounds":            sampleStatistics(rounds, ""),
			"sent_back":                {"sent_back": figure(sent), "n": figure(issues), "share": share(sent, issues)},
			"hours_after_first_review": sampleStatistics(after, "hours"),
		}
	case SlugSplitting:
		return m.splitting(cut, finished)
	}
	return nil
}

// phaseTime is phase time by worker type over finished: for each bar, "the median phase hours per
// issue for each role ... over every finished issue of the window, and an issue no worker of that
// role touched counts as zero hours", the issues that hold hours on it, its hours and its five
// segments; and the idle share, "idle hours over all phase hours", which every bar states.
func phaseTime(finished []values) map[string]Statistics {
	answer := map[string]Statistics{}
	whole, idle := 0.0, 0.0
	for _, one := range bars {
		perIssue := make([]float64, 0, len(finished))
		var segments [segmentCount]float64
		holding := 0
		for _, issue := range finished {
			part := issue.bars[one.role]
			perIssue = append(perIssue, part.hours())
			if !part.holds {
				continue
			}
			holding++
			for segment, hours := range part.segments {
				segments[segment] += hours
			}
		}
		stats := sampleStatistics(perIssue, "")
		stats["median_per_issue"] = stats["median"]
		delete(stats, "median")
		delete(stats, "p90")
		stats["issues"] = figure(float64(holding))
		total := 0.0
		for segment, hours := range segments {
			stats[segmentNames[segment]] = figure(hours)
			total += rounded(hours)
		}
		stats["hours"] = figure(total)
		whole += rounded(total)
		idle += rounded(segments[segmentIdle])
		answer[one.role] = stats
	}
	idleShare := share(idle, whole)
	for _, stats := range answer {
		stats["idle_share"] = idleShare
	}
	return answer
}

// splitting is splitting and follow-ups over finished and the window's filings in cut: each
// class's filings and the issues that finished, "the weekly counts follow the toggle"; and, for the
// all and roots cuts alike, the finished roots with their children, children per root and the
// work time of the roots with children and of those without: "These root figures count the
// project's finished roots whatever the all, roots or children toggle says, since they describe
// roots", so the children cut, which holds no root, states none of them.
func (m measuring) splitting(cut string, finished []values) map[string]Statistics {
	answer := map[string]Statistics{}
	for _, class := range classes {
		count := 0.0
		for _, one := range m.filed {
			if one.class == class && inCut(cut, one.root) {
				count++
			}
		}
		answer["filed:"+class] = Statistics{"issues": figure(count)}
	}
	answer["finished"] = Statistics{"issues": figure(float64(len(finished)))}
	if cut == CutChildren {
		return answer
	}
	roots, children := 0.0, 0.0
	var with, without []float64
	for _, one := range m.finished {
		if !one.root {
			continue
		}
		roots++
		children += float64(one.children)
		if !one.started {
			continue
		}
		if one.children > 0 {
			with = append(with, one.workHours)
		} else {
			without = append(without, one.workHours)
		}
	}
	answer["roots"] = Statistics{"issues": figure(roots), "children": figure(children), "children_per_root": share(children, roots)}
	answer["hours_with_children"] = sampleStatistics(with, "")
	answer["hours_without_children"] = sampleStatistics(without, "")
	return answer
}

// dayOf is the UTC midnight of the day at is on.
func dayOf(at time.Time) time.Time {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
}

// nextMidnight is the first UTC midnight at or after at.
func nextMidnight(at time.Time) time.Time {
	day := dayOf(at)
	if day.Before(at) {
		day = day.AddDate(0, 0, 1)
	}
	return day
}
