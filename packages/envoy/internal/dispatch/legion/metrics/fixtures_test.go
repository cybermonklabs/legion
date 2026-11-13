package metrics

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// base is the moment every fixture's hour is counted from, and `hour` is a whole number of hours
// after it: a fixture reads as the timeline it describes.
var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func hour(after float64) time.Time {
	return base.Add(time.Duration(after * float64(time.Hour)))
}

// newIssue is an issue of a fixture, with the maps the builders below write into.
func newIssue(key string) *Issue {
	return &Issue{Key: key, Title: key + " title", FirstRoleEvent: map[string]time.Time{}, Observed: map[string][]Mark{}}
}

// under names the issue's parent.
func (i *Issue) under(parent string) *Issue {
	i.Parent = parent
	return i
}

// openedAt is when the issue was created, which its own row carries.
func (i *Issue) openedAt(at time.Time) *Issue {
	i.CreatedAt = at
	return i
}

// closedAt closes the issue.
func (i *Issue) closedAt(at time.Time) *Issue {
	i.ClosedAt = &at
	return i
}

// worked records the first event of a session titled `Legion <role> · <this issue>`.
func (i *Issue) worked(role string, at time.Time) *Issue {
	i.FirstRoleEvent[role] = at
	return i
}

// handedOff records a handoff of the phase word.
func (i *Issue) handedOff(word string, at time.Time) *Issue {
	i.Handoffs = append(i.Handoffs, Handoff{Phase: word, Version: len(i.Handoffs) + 1, At: at})
	return i
}

// statusAt records a status snapshot.
func (i *Issue) statusAt(at time.Time, status string) *Issue {
	i.StatusMarks = append(i.StatusMarks, Mark{At: at, Value: status})
	return i
}

// observe records one value a read found changed, the JSON text as legion_observations holds it.
func (i *Issue) observe(field string, at time.Time, value string) *Issue {
	i.Observed[field] = append(i.Observed[field], Mark{At: at, Value: value})
	return i
}

// observeString records an observation whose value is a JSON string.
func (i *Issue) observeString(field string, at time.Time, value string) *Issue {
	return i.observe(field, at, fmt.Sprintf("%q", value))
}

// asked records a span an ask of this issue stood on a person's turn.
func (i *Issue) asked(from, to time.Time) *Issue {
	i.AskTurns = append(i.AskTurns, Span{From: from, To: to})
	return i
}

// fixture is a population of hand-built issues over [from, to), with its trees linked as a read
// links them.
//
// It fills the two sets the tree rule reads (Population.isLegionIssue) as a read fills them: an
// issue the fixture gave an observation of is one the daemon reports, with the `tree` it last
// named, and an issue the fixture gave a role event or a handoff carries Legion phase work.
// They are the project's own facts, which no fixture states twice.
func fixture(from, to time.Time, issues ...*Issue) *Population {
	population := &Population{
		Project: "INFRA", From: from, To: to, Issues: map[string]*Issue{},
		reported: map[string]string{}, phaseWork: map[string]struct{}{},
	}
	for _, issue := range issues {
		population.Issues[issue.Key] = issue
		population.Keys = append(population.Keys, issue.Key)
		if len(issue.Observed) > 0 {
			population.reported[issue.Key] = issue.Tree
		}
		if len(issue.FirstRoleEvent) > 0 || len(issue.Handoffs) > 0 {
			population.phaseWork[issue.Key] = struct{}{}
		}
	}
	slices.Sort(population.Keys)
	population.linkTrees()
	return population
}

// window is what the Legion evals answer of a population's window in one cut, built the way the
// refresher builds the day rows and the eval routes fold them (evals/legion_days.go): the facts of
// every issue that finished in the window (ProjectFacts' entries that closed in [From, To)) and
// every filing the population read (ProjectFilings), turned into each eval's day parts by the
// issue's close day and the filing's day, and folded in the cut. finished and filed are the
// issues and the filings the cut keeps, in the population's order, which the eval items route
// lists beside the numbers.
type window struct {
	cut      Scope
	finished []string
	facts    map[string]IssueFacts
	filed    []IssueFiling

	workTimeParts, phaseTimeParts, reworkParts, splittingParts []measures.DayPart
}

// windowOf is population's window in cut.
func windowOf(population *Population, cut Scope) window {
	w := window{cut: cut, facts: map[string]IssueFacts{}}
	dayOf := func(at time.Time) time.Time { return at.UTC().Truncate(24 * time.Hour) }
	closedOn := map[time.Time][]IssueFacts{}
	filedOn := map[time.Time][]IssueFiling{}
	facts := ProjectFacts(population)
	for _, key := range population.Keys {
		issue, held := facts[key]
		if !held || issue.ClosedAt == nil || issue.ClosedAt.Before(population.From) || !issue.ClosedAt.Before(population.To) {
			continue
		}
		w.facts[key] = issue
		closedOn[dayOf(*issue.ClosedAt)] = append(closedOn[dayOf(*issue.ClosedAt)], issue)
		if InCut(cut, issue.Root) {
			w.finished = append(w.finished, key)
		}
	}
	for _, filing := range ProjectFilings(population) {
		filedOn[dayOf(filing.At)] = append(filedOn[dayOf(filing.At)], filing)
		if InCut(cut, filing.Root == filing.Key) {
			w.filed = append(w.filed, filing)
		}
	}
	days := slices.Collect(maps.Keys(closedOn))
	for day := range filedOn {
		if _, closed := closedOn[day]; !closed {
			days = append(days, day)
		}
	}
	slices.SortFunc(days, func(a, b time.Time) int { return a.Compare(b) })
	for _, day := range days {
		// Five of the evals have input on a day with a closed issue, and legion-splitting on a
		// day with a closed issue or a filing, as LegionDay.build writes them.
		if finished := closedOn[day]; len(finished) > 0 {
			w.workTimeParts = append(w.workTimeParts, WorkTimeDay(day, finished)...)
			w.phaseTimeParts = append(w.phaseTimeParts, PhaseTimeDay(day, finished)...)
			w.reworkParts = append(w.reworkParts, ReworkDay(day, finished)...)
		}
		w.splittingParts = append(w.splittingParts, SplittingDay(day, closedOn[day], filedOn[day])...)
	}
	return w
}

// timed is the finished issues of the cut that have a work time, the ones legion-work-time
// counts: an issue with no first worker event has none.
func (w window) timed() []string {
	return slices.DeleteFunc(slices.Clone(w.finished), func(key string) bool { return w.facts[key].StartedAt == nil })
}

// fact is one finished issue's facts, failing the test where the window finished no such issue.
func (w window) fact(t *testing.T, key string) IssueFacts {
	t.Helper()
	facts, held := w.facts[key]
	if !held {
		t.Fatalf("no issue %s finished in the window; it finished %v", key, w.finished)
	}
	return facts
}

// workTime is legion-work-time over the window in its cut, a fold of no value where it holds none.
func (w window) workTime() SampleFold {
	if fold := FoldWorkTime(w.workTimeParts, w.cut); fold != nil {
		return *fold
	}
	return SampleFold{}
}

// phaseTime is legion-phase-time over the window in its cut.
func (w window) phaseTime(t *testing.T) PhaseTimeFold {
	t.Helper()
	fold := FoldPhaseTime(w.phaseTimeParts, w.cut)
	if fold == nil {
		t.Fatalf("legion-phase-time over the window, cut %s, holds no bar: %d issues finished", w.cut, len(w.finished))
	}
	return *fold
}

// hoursPerBar is each bar's hours of legion-phase-time over the window in its cut, by role, and
// their sum.
func (w window) hoursPerBar(t *testing.T) (map[string]float64, float64) {
	t.Helper()
	hours, whole := map[string]float64{}, 0.0
	for role, bar := range w.phaseTime(t).Bars {
		hours[role] = bar.Hours
		whole += bar.Hours
	}
	return hours, whole
}

// barOf is one role's bar of legion-phase-time over the window in its cut.
func barOf(t *testing.T, w window, role string) BarFold {
	t.Helper()
	bars := w.phaseTime(t).Bars
	bar, held := bars[role]
	if !held {
		t.Fatalf("legion-phase-time has no %s bar: %+v", role, bars)
	}
	return bar
}

// rework is legion-rework over the window in its cut.
func (w window) rework(t *testing.T) ReworkFold {
	t.Helper()
	fold := FoldRework(w.reworkParts, w.cut)
	if fold == nil || fold.ReviewRounds == nil || fold.SentBack == nil {
		t.Fatalf("legion-rework over the window, cut %s, holds no value: %d issues finished", w.cut, len(w.finished))
	}
	return *fold
}

// splitting is legion-splitting over the window in its cut.
func (w window) splitting(t *testing.T) SplittingFold {
	t.Helper()
	fold := FoldSplitting(w.splittingParts, w.cut)
	if fold == nil {
		t.Fatalf("legion-splitting over the window, cut %s, holds no value", w.cut)
	}
	return *fold
}

// roots is legion-splitting's root figures over the window, which count roots alone whatever the
// cut and which its all cut carries.
func (w window) roots(t *testing.T) RootsFold {
	t.Helper()
	fold := FoldSplitting(w.splittingParts, ScopeAll)
	if fold == nil || fold.Roots == nil {
		t.Fatalf("legion-splitting over the window holds no root figures")
	}
	return *fold.Roots
}

// keysOf is the keys of filings, in their order.
func keysOf(filings []IssueFiling) []string {
	keys := make([]string, 0, len(filings))
	for _, filing := range filings {
		keys = append(keys, filing.Key)
	}
	return keys
}

// valueOf is a fold's value, or -1 for no value, so a comparison with an expected number reads
// the two apart.
func valueOf(value *float64) float64 {
	if value == nil {
		return -1
	}
	return *value
}

// medianOf is a sample fold's median as valueOf reads it, -1 for a fold of no value or none at all.
func medianOf(fold *SampleFold) float64 {
	if fold == nil {
		return -1
	}
	return valueOf(fold.Median)
}

// timelineOf is an issue's phase timeline written as "<phase> <from>-<to>" in hours after base,
// which is what a phase test compares.
func timelineOf(population *Population, issue *Issue) []string {
	spans, _ := population.Phases(issue)
	written := make([]string, 0, len(spans))
	for _, span := range spans {
		written = append(written, fmt.Sprintf("%s %g-%g",
			span.Phase, span.Span.From.Sub(base).Hours(), span.Span.To.Sub(base).Hours()))
	}
	return written
}
