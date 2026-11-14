package metrics

import (
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// The six Legion evals (dispatch://INFRA-2916, R6 and R12) are each one rule split in two, as
// every delivery and CI number is (measures/parts.go): a day builder (the functions named
// <Measure>Day) turns the facts of the issues that finished on one UTC day, and the filings of
// that day, into the day's parts, and one exported fold (the functions named Fold<Measure>) turns
// any set of those parts into the answer, reading no database. The refresher stores the parts as
// eval_days rows (evals/legion_days.go) and the eval routes fold a window's rows, which is what
// the Delivery page's Legion section reads.
//
// A part folds the per-issue values IssueFacts holds (factsOf), which issue_facts stores: each
// finished issue's work time, its part of each of the seven bars, its idle hours in each role,
// its slot wait, its rework row and its children. A filing is an IssueFiling, which issue_filings
// stores.
//
// Every series is stored in three cuts (R6, D5), which CutSeries names: every issue
// (`<series>`), the roots of their trees (`<series>:roots`, an issue whose tree root is itself)
// and the rest (`<series>:children`). A filing is cut by the filed issue's tree. The splitting
// eval's root figures (RootFigures) count roots alone whatever the cut, as metric 4 counts them:
// their `:roots` cut is their all cut, and their `:children` cut holds no issue, so SplittingDay
// builds no part of it and a fold of it reads no data.
//
// A sample series holds one value per finished issue of the day: its work time, its hours on a
// bar (zero on a bar no hour of it fell on), its idle hours in a role, its slot wait, its review
// rounds, its hours after its first review, or a finished root's work time. Its fold merges the
// parts' sample lists and reads measures.Quantile over the merge, never the mean of the days'
// quantiles. A count is a sum, and a share is the summed numerator over the summed denominator.
// Every number a fold answers is rounded to four decimals (round). A set that holds no part of a
// fold's series reads no value (nil), never 0; a part built for a cut no issue of the day is in
// is a zero part, and reads as zero counts and no quantile.

// The series of the six Legion evals before their cut (CutSeries). legion-phase-time's series
// are its seven bars' roles (architect, planner, implementer, tester, reviewer, merger and
// no_role), legion-idle-time's the six roles, and legion-splitting's filings FilingSeries.
const (
	// SeriesHours is legion-work-time's (each finished issue's work time) and legion-slot-wait's
	// (each finished issue's slot wait): a sample series.
	SeriesHours = "hours"
	// SeriesReviewRounds is legion-rework's review rounds of each finished issue (samples).
	SeriesReviewRounds = "review_rounds"
	// SeriesSentBack is legion-rework's share of finished issues sent back to implementing: the
	// issues sent back over the issues finished.
	SeriesSentBack = "sent_back"
	// SeriesHoursAfterFirstReview is legion-rework's implementing and testing hours after the
	// first review of each finished issue (samples).
	SeriesHoursAfterFirstReview = "hours_after_first_review"
	// SeriesFinished is legion-splitting's count of the issues that finished, by close day.
	SeriesFinished = "finished"
	// SeriesRoots is legion-splitting's count of the roots that finished, with their children in
	// CountChildren.
	SeriesRoots = "roots"
	// SeriesHoursWithChildren and SeriesHoursWithoutChildren are legion-splitting's work time of
	// each finished root with children and of each with none (samples).
	SeriesHoursWithChildren    = "hours_with_children"
	SeriesHoursWithoutChildren = "hours_without_children"
)

// RootFigures are legion-splitting's series that count the finished roots alone whatever the cut
// (metric 4): SplittingDay builds them in the all and the roots cut, the same in both, and never
// in the children cut, which holds no root. The eval still has that cut of each, as every Legion
// series has its three, and it reads no data.
var RootFigures = []string{SeriesRoots, SeriesHoursWithChildren, SeriesHoursWithoutChildren}

// The named counts a Legion part carries beside its value: a phase-time bar's five segments, each
// under its segment's name (Segment), and CountIssues, how many finished issues hold hours on the
// bar; a roots part's CountChildren, the children of its roots.
const (
	CountIssues   = "issues"
	CountChildren = "children"
)

// cuts are the three cuts every Legion series is stored in.
var cuts = []Scope{ScopeAll, ScopeRoots, ScopeChildren}

// CutSeries is series as cut stores it: series itself for the all cut, `<series>:roots` and
// `<series>:children` for the other two.
func CutSeries(series string, cut Scope) string {
	if cut == ScopeAll {
		return series
	}
	return series + ":" + string(cut)
}

// FilingSeries is legion-splitting's series of one class of filing, by filing day:
// `filed:<class>`.
func FilingSeries(class string) string {
	return "filed:" + class
}

// InCut is whether an item counts in cut: every item in the all cut, a root in the roots cut and
// any other item in the children cut. It is the one statement of the eval's cuts, which the day
// builders cut their parts by and the eval items route answers the issues of a cut by.
func InCut(cut Scope, root bool) bool {
	switch cut {
	case ScopeRoots:
		return root
	case ScopeChildren:
		return !root
	}
	return true
}

// countPart is a count's part: N and Sum the items counted, as the delivery builders build one.
func countPart(day time.Time, series string, count int, counts map[string]float64) measures.DayPart {
	sum := float64(count)
	return measures.DayPart{Series: series, Day: day, N: count, Sum: &sum, Counts: counts}
}

// sharePart is a share's part: numerator over denominator, N the items the denominator counts.
func sharePart(day time.Time, series string, numerator, denominator int) measures.DayPart {
	num, den := float64(numerator), float64(denominator)
	return measures.DayPart{Series: series, Day: day, N: denominator, Num: &num, Den: &den}
}

// samplesPart is a sample series' part: every value, their count, their sum and the largest.
// The sum is taken in the order of values, which is the order the issues were given in.
func samplesPart(day time.Time, series string, values []float64, counts map[string]float64) measures.DayPart {
	part := measures.DayPart{Series: series, Day: day, N: len(values), Samples: values, Counts: counts}
	if part.Samples == nil {
		part.Samples = []float64{}
	}
	sum := 0.0
	for _, value := range values {
		sum += value
		if part.Max == nil || value > *part.Max {
			part.Max = new(value)
		}
	}
	part.Sum = &sum
	return part
}

// perIssue is one sample part of series per cut: value of every finished issue in the cut.
func perIssue(day time.Time, series string, finished []IssueFacts, value func(IssueFacts) float64) []measures.DayPart {
	parts := make([]measures.DayPart, 0, len(cuts))
	for _, cut := range cuts {
		values := make([]float64, 0, len(finished))
		for _, facts := range finished {
			if InCut(cut, facts.Root) {
				values = append(values, value(facts))
			}
		}
		parts = append(parts, samplesPart(day, CutSeries(series, cut), values, nil))
	}
	return parts
}

// WorkTimeDay is legion-work-time's parts of one UTC day from the facts of the issues that
// finished on it: in each cut, the work time of each issue that has one (an issue with no first
// worker event has none, and metric 1 leaves it out).
func WorkTimeDay(day time.Time, finished []IssueFacts) []measures.DayPart {
	parts := make([]measures.DayPart, 0, len(cuts))
	for _, cut := range cuts {
		hours := make([]float64, 0, len(finished))
		for _, facts := range finished {
			if InCut(cut, facts.Root) && facts.StartedAt != nil {
				hours = append(hours, facts.WorkHours)
			}
		}
		parts = append(parts, samplesPart(day, CutSeries(SeriesHours, cut), hours, nil))
	}
	return parts
}

// PhaseTimeDay is legion-phase-time's parts of one UTC day from the facts of the issues that
// finished on it: in each cut, one part per bar holding every issue's hours on the bar as a
// sample, zero where no hour of the issue fell on it, so each bar's median is over the same
// issues (metric 2); the bar's five segments, summed over the issues that hold hours on it, and
// how many those are (CountIssues) are its counts.
func PhaseTimeDay(day time.Time, finished []IssueFacts) []measures.DayPart {
	parts := make([]measures.DayPart, 0, len(cuts)*len(bars))
	for _, cut := range cuts {
		for _, bar := range bars {
			values := make([]float64, 0, len(finished))
			var segments Segments
			holding := 0
			for _, facts := range finished {
				if !InCut(cut, facts.Root) {
					continue
				}
				part := facts.Bars[bar.Role]
				values = append(values, part.Segments.total())
				if part.HoldsHours {
					segments.addSegments(part.Segments)
					holding++
				}
			}
			parts = append(parts, samplesPart(day, CutSeries(bar.Role, cut), values, barCounts(segments, holding)))
		}
	}
	return parts
}

// barCounts is a bar's segments and the issues holding hours on it, as its part's counts.
func barCounts(segments Segments, issues int) map[string]float64 {
	return map[string]float64{
		string(SegmentWaitingOnPerson):   segments.WaitingOnPerson,
		string(SegmentInATurn):           segments.InATurn,
		string(SegmentCIWait):            segments.CIWait,
		string(SegmentWaitingOnChildren): segments.WaitingOnChildren,
		string(SegmentIdle):              segments.Idle,
		CountIssues:                      float64(issues),
	}
}

// segmentsOf is the five segments a bar's part counts.
func segmentsOf(counts map[string]float64) Segments {
	return Segments{
		WaitingOnPerson:   counts[string(SegmentWaitingOnPerson)],
		InATurn:           counts[string(SegmentInATurn)],
		CIWait:            counts[string(SegmentCIWait)],
		WaitingOnChildren: counts[string(SegmentWaitingOnChildren)],
		Idle:              counts[string(SegmentIdle)],
	}
}

// IdleTimeDay is legion-idle-time's parts of one UTC day from the facts of the issues that
// finished on it: in each cut, one part per role holding every issue's idle hours in that role
// (IssueFacts.IdleByRole, idleByRole), zero where it had none.
func IdleTimeDay(day time.Time, finished []IssueFacts) []measures.DayPart {
	parts := make([]measures.DayPart, 0, len(cuts)*(len(bars)-1))
	for _, bar := range bars {
		if bar.Role == barNoRole {
			continue
		}
		parts = append(parts, perIssue(day, bar.Role, finished, func(facts IssueFacts) float64 { return facts.IdleByRole[bar.Role] })...)
	}
	return parts
}

// SlotWaitDay is legion-slot-wait's parts of one UTC day from the facts of the issues that
// finished on it: in each cut, every issue's slot wait (IssueFacts.SlotWaitHours, slotWaitHours).
func SlotWaitDay(day time.Time, finished []IssueFacts) []measures.DayPart {
	return perIssue(day, SeriesHours, finished, func(facts IssueFacts) float64 { return facts.SlotWaitHours })
}

// ReworkDay is legion-rework's parts of one UTC day from the facts of the issues that finished on
// it: in each cut, every issue's review rounds, the issues sent back over the issues finished,
// and every issue's implementing and testing hours after its first review (reworkOf).
func ReworkDay(day time.Time, finished []IssueFacts) []measures.DayPart {
	parts := perIssue(day, SeriesReviewRounds, finished, func(facts IssueFacts) float64 { return float64(facts.ReviewRounds) })
	parts = append(parts, perIssue(day, SeriesHoursAfterFirstReview, finished, func(facts IssueFacts) float64 {
		return facts.ImplementingHoursAfterFirstReview + facts.TestingHoursAfterFirstReview
	})...)
	for _, cut := range cuts {
		sentBack, issues := 0, 0
		for _, facts := range finished {
			if !InCut(cut, facts.Root) {
				continue
			}
			issues++
			if facts.SentBack {
				sentBack++
			}
		}
		parts = append(parts, sharePart(day, CutSeries(SeriesSentBack, cut), sentBack, issues))
	}
	return parts
}

// SplittingDay is legion-splitting's parts of one UTC day from the facts of the issues that
// finished on it and the filings made on it: in each cut, each class's filings (every class
// present) and the issues that finished; and in the all and roots cuts alike, the roots that
// finished with their children, and the work time of each of those roots that has one, those
// with children apart from those with none (metric 4).
func SplittingDay(day time.Time, finished []IssueFacts, filed []IssueFiling) []measures.DayPart {
	parts := make([]measures.DayPart, 0, len(cuts)*(len(classes)+1)+3*2)
	for _, cut := range cuts {
		byClass := make(map[string]int, len(classes))
		for _, filing := range filed {
			if InCut(cut, filing.Root == filing.Key) {
				byClass[filing.Class]++
			}
		}
		for _, class := range classes {
			parts = append(parts, countPart(day, CutSeries(FilingSeries(class), cut), byClass[class], nil))
		}
		count := 0
		for _, facts := range finished {
			if InCut(cut, facts.Root) {
				count++
			}
		}
		parts = append(parts, countPart(day, CutSeries(SeriesFinished, cut), count, nil))
	}
	roots, children := 0, 0
	withChildren, withoutChildren := []float64{}, []float64{}
	for _, facts := range finished {
		if !facts.Root {
			continue
		}
		roots++
		children += facts.Children
		if facts.StartedAt == nil {
			continue
		}
		if facts.Children > 0 {
			withChildren = append(withChildren, facts.WorkHours)
		} else {
			withoutChildren = append(withoutChildren, facts.WorkHours)
		}
	}
	for _, cut := range []Scope{ScopeAll, ScopeRoots} {
		parts = append(parts,
			countPart(day, CutSeries(SeriesRoots, cut), roots, map[string]float64{CountChildren: float64(children)}),
			samplesPart(day, CutSeries(SeriesHoursWithChildren, cut), withChildren, nil),
			samplesPart(day, CutSeries(SeriesHoursWithoutChildren, cut), withoutChildren, nil),
		)
	}
	return parts
}

// SampleFold is one sample series folded: how many values its parts hold, their sum, and their
// median and 90th percentile (measures.Quantile over the merge of every part's samples). Median
// and P90 are nil when the parts hold no value. The statistics a series of it answers are `n`,
// `sum` (an hours series' `hours`), `median` and `p90`.
type SampleFold struct {
	N      int      `json:"n"`
	Sum    float64  `json:"sum"`
	Median *float64 `json:"median"`
	P90    *float64 `json:"p90"`
}

// foldSamples folds the parts of series among parts, nil when they hold none.
func foldSamples(parts []measures.DayPart, series string) *SampleFold {
	var merged []float64
	sum, held := 0.0, false
	for _, part := range parts {
		if part.Series != series {
			continue
		}
		held = true
		merged = append(merged, part.Samples...)
		sum += valueOrZero(part.Sum)
	}
	if !held {
		return nil
	}
	fold := &SampleFold{N: len(merged), Sum: round(sum)}
	if len(merged) > 0 {
		slices.Sort(merged)
		fold.Median, fold.P90 = new(quantile(merged, 0.5)), new(quantile(merged, 0.9))
	}
	return fold
}

// counted is the parts of series among parts added up: whether any was of it, their summed
// value (Sum), numerator and denominator, and each named count summed.
type counted struct {
	held          bool
	sum, num, den float64
	counts        map[string]float64
}

func countSeries(parts []measures.DayPart, series string) counted {
	var c counted
	for _, part := range parts {
		if part.Series != series {
			continue
		}
		c.held = true
		c.sum += valueOrZero(part.Sum)
		c.num += valueOrZero(part.Num)
		c.den += valueOrZero(part.Den)
		for name, count := range part.Counts {
			if c.counts == nil {
				c.counts = map[string]float64{}
			}
			c.counts[name] += count
		}
	}
	return c
}

func valueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

// FoldWorkTime is legion-work-time over parts, cut by cut (computed:legion/metrics.FoldWorkTime):
// the work time of every finished issue of the cut that has one, as series `hours` (statistics
// `n`, `median`, `p90`); nil when parts hold no part of the cut's series. parts are the eval's
// own: legion-slot-wait's series has the same name.
func FoldWorkTime(parts []measures.DayPart, cut Scope) *SampleFold {
	return foldSamples(parts, CutSeries(SeriesHours, cut))
}

// FoldSlotWait is legion-slot-wait over parts, cut by cut (computed:legion/metrics.FoldSlotWait):
// every finished issue's hours admitted with no slot of its own over its observed life, parked
// hours left out (slotWaitHours), as series `hours` (statistics `hours` the sum, `n`, `median`,
// `p90`); nil when parts hold no part of the cut's series. parts are the eval's own:
// legion-work-time's series has the same name.
func FoldSlotWait(parts []measures.DayPart, cut Scope) *SampleFold {
	return foldSamples(parts, CutSeries(SeriesHours, cut))
}

// FoldIdleTime is legion-idle-time over parts, cut by cut (computed:legion/metrics.FoldIdleTime):
// for each of the six roles, every finished issue's hours in which it held its own slot in that
// role's bar with the role out of a turn (idleByRole), as series `<role>` (statistics `hours` the
// sum, `n`, `median`, `p90`). A role whose series parts hold no part of has no entry, and the
// answer is nil when no role has one.
func FoldIdleTime(parts []measures.DayPart, cut Scope) map[string]SampleFold {
	var roles map[string]SampleFold
	for _, bar := range bars {
		if bar.Role == barNoRole {
			continue
		}
		fold := foldSamples(parts, CutSeries(bar.Role, cut))
		if fold == nil {
			continue
		}
		if roles == nil {
			roles = map[string]SampleFold{}
		}
		roles[bar.Role] = *fold
	}
	return roles
}

// BarFold is one bar of legion-phase-time folded, its series `<role>`: Hours, the bar's whole
// time (statistic `hours`), the total of its five Segments (statistics `waiting_on_person`,
// `in_a_turn`, `ci_wait`, `waiting_on_children`, `idle`); Issues, how many finished issues hold
// hours on it (`issues`); N, how many finished issues its median is over (`n`); and
// MedianPerIssue, the median of every finished issue's hours on it, zero for an issue no hour of
// which fell on it (`median_per_issue`), nil when the parts hold no issue.
type BarFold struct {
	N              int      `json:"n"`
	Issues         int      `json:"issues"`
	Hours          float64  `json:"hours"`
	MedianPerIssue *float64 `json:"median_per_issue"`
	Segments       Segments `json:"segments"`
}

// PhaseTimeFold is legion-phase-time folded: each bar by its role, a bar whose series the parts
// hold no part of left out, and IdleShare (statistic `idle_share`), the idle hours of every bar
// over their whole time, nil when the bars hold no hour.
type PhaseTimeFold struct {
	Bars      map[string]BarFold `json:"bars"`
	IdleShare *float64           `json:"idle_share"`
}

// FoldPhaseTime is legion-phase-time over parts, cut by cut
// (computed:legion/metrics.FoldPhaseTime): the seven bars of metric 2 (bars), each with its
// segments summed, and the idle share, the bars' rounded idle hours over their rounded hours;
// nil when parts hold no part of any bar's series in the cut.
func FoldPhaseTime(parts []measures.DayPart, cut Scope) *PhaseTimeFold {
	fold := &PhaseTimeFold{Bars: map[string]BarFold{}}
	var whole, idle float64
	for _, bar := range bars {
		series := CutSeries(bar.Role, cut)
		samples := foldSamples(parts, series)
		if samples == nil {
			continue
		}
		var segments Segments
		issues := 0.0
		for _, part := range parts {
			if part.Series == series {
				segments.addSegments(segmentsOf(part.Counts))
				issues += part.Counts[CountIssues]
			}
		}
		folded := BarFold{
			N: samples.N, Issues: int(issues), Hours: round(segments.total()),
			MedianPerIssue: samples.Median, Segments: rounded(segments),
		}
		whole += folded.Hours
		idle += folded.Segments.Idle
		fold.Bars[bar.Role] = folded
	}
	if len(fold.Bars) == 0 {
		return nil
	}
	if whole > 0 {
		fold.IdleShare = new(round(idle / whole))
	}
	return fold
}

// ShareFold is a share folded: the summed numerator over the summed denominator, Share nil when
// the denominator is 0.
type ShareFold struct {
	Num   int      `json:"num"`
	Den   int      `json:"den"`
	Share *float64 `json:"share"`
}

// ReworkFold is legion-rework folded: ReviewRounds, every finished issue's review rounds (series
// `review_rounds`: `n`, `median`, `p90`); SentBack, the issues sent back over the issues finished
// (series `sent_back`: `sent_back` the numerator, `n` the denominator, `share`); and
// HoursAfterFirstReview, every finished issue's implementing and testing hours after its first
// review (series `hours_after_first_review`: `hours` the sum, `n`, `median`, `p90`). A series the
// parts hold no part of is nil.
type ReworkFold struct {
	ReviewRounds          *SampleFold `json:"review_rounds"`
	SentBack              *ShareFold  `json:"sent_back"`
	HoursAfterFirstReview *SampleFold `json:"hours_after_first_review"`
}

// FoldRework is legion-rework over parts, cut by cut (computed:legion/metrics.FoldRework): metric
// 3's review rounds, share sent back and hours after the first review (reworkOf); nil when parts
// hold no part of any of its three series in the cut.
func FoldRework(parts []measures.DayPart, cut Scope) *ReworkFold {
	fold := ReworkFold{
		ReviewRounds:          foldSamples(parts, CutSeries(SeriesReviewRounds, cut)),
		HoursAfterFirstReview: foldSamples(parts, CutSeries(SeriesHoursAfterFirstReview, cut)),
	}
	if sent := countSeries(parts, CutSeries(SeriesSentBack, cut)); sent.held {
		fold.SentBack = &ShareFold{Num: int(sent.num), Den: int(sent.den)}
		if sent.den > 0 {
			fold.SentBack.Share = new(round(sent.num / sent.den))
		}
	}
	if fold.ReviewRounds == nil && fold.SentBack == nil && fold.HoursAfterFirstReview == nil {
		return nil
	}
	return &fold
}

// WeekFold is one UTC week of legion-splitting, starting on its Monday: each class's filings and
// the issues that finished.
type WeekFold struct {
	Week     time.Time      `json:"week"`
	Filed    map[string]int `json:"filed"`
	Finished int            `json:"finished"`
}

// RootsFold is legion-splitting's root figures folded, which count roots alone whatever the cut:
// FinishedRoots, the roots that finished (series `roots`, statistic `issues`); Children, their
// children (`children`); ChildrenPerRoot, the second over the first (`children_per_root`), nil
// with no root; and the work time of those with children and of those with none (series
// `hours_with_children` and `hours_without_children`: `n`, `median`, `p90`), each nil when the
// parts hold no part of its series.
type RootsFold struct {
	FinishedRoots        int         `json:"finished_roots"`
	Children             int         `json:"children"`
	ChildrenPerRoot      *float64    `json:"children_per_root"`
	HoursWithChildren    *SampleFold `json:"hours_with_children"`
	HoursWithoutChildren *SampleFold `json:"hours_without_children"`
}

// SplittingFold is legion-splitting folded: Filed, each class's filings (series `filed:<class>`,
// statistic `issues`), every class present; Finished, the issues that finished (series
// `finished`, `issues`); Weeks, the UTC weeks any of those fall in, oldest first, each class
// present in each; and Roots, the root figures, nil where the parts hold no part of series
// `roots` (its children cut always).
type SplittingFold struct {
	Filed    map[string]int `json:"filed"`
	Finished int            `json:"finished"`
	Weeks    []WeekFold     `json:"weeks"`
	Roots    *RootsFold     `json:"roots"`
}

// FoldSplitting is legion-splitting over parts, cut by cut
// (computed:legion/metrics.FoldSplitting): metric 4's filings by class and the issues that
// finished, in total and by the week (weekStart) of each part's day, a week counted where it
// holds a filing or a finished issue; and the root figures; nil when parts hold no
// part of any of its series in the cut.
func FoldSplitting(parts []measures.DayPart, cut Scope) *SplittingFold {
	fold := &SplittingFold{Filed: make(map[string]int, len(classes))}
	held := false
	weeks := map[time.Time]*WeekFold{}
	week := func(day time.Time) *WeekFold {
		start := weekStart(day)
		if existing, ok := weeks[start]; ok {
			return existing
		}
		fresh := &WeekFold{Week: start, Filed: make(map[string]int, len(classes))}
		for _, class := range classes {
			fresh.Filed[class] = 0
		}
		weeks[start] = fresh
		return fresh
	}
	for _, class := range classes {
		fold.Filed[class] = 0
		series := CutSeries(FilingSeries(class), cut)
		for _, part := range parts {
			if part.Series != series {
				continue
			}
			held = true
			if count := int(valueOrZero(part.Sum)); count > 0 {
				fold.Filed[class] += count
				week(part.Day).Filed[class] += count
			}
		}
	}
	finished := CutSeries(SeriesFinished, cut)
	for _, part := range parts {
		if part.Series != finished {
			continue
		}
		held = true
		if count := int(valueOrZero(part.Sum)); count > 0 {
			fold.Finished += count
			week(part.Day).Finished += count
		}
	}
	if roots := countSeries(parts, CutSeries(SeriesRoots, cut)); roots.held {
		held = true
		fold.Roots = &RootsFold{
			FinishedRoots:        int(roots.sum),
			Children:             int(roots.counts[CountChildren]),
			HoursWithChildren:    foldSamples(parts, CutSeries(SeriesHoursWithChildren, cut)),
			HoursWithoutChildren: foldSamples(parts, CutSeries(SeriesHoursWithoutChildren, cut)),
		}
		if roots.sum > 0 {
			fold.Roots.ChildrenPerRoot = new(round(roots.counts[CountChildren] / roots.sum))
		}
	}
	if !held {
		return nil
	}
	fold.Weeks = make([]WeekFold, 0, len(weeks))
	for _, one := range weeks {
		fold.Weeks = append(fold.Weeks, *one)
	}
	slices.SortFunc(fold.Weeks, func(a, b WeekFold) int { return a.Week.Compare(b.Week) })
	return fold
}
