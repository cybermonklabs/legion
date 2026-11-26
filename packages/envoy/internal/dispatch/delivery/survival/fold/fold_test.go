package fold_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
)

// figures is a group's figures as plain values, nil where a figure has none.
func figures(got fold.GroupFigures) []any {
	var out []any
	for _, figure := range []*float64{got.Share, got.Lines, got.Added, got.Changes, got.ViaRepository} {
		if figure == nil {
			out = append(out, nil)
		} else {
			out = append(out, *figure)
		}
	}
	return out
}

// noData is figures with no value at all: "no data", never 0.
var noData = []any{nil, nil, nil, nil, nil}

// twoMeasuredDays is the project's parts of two measured days: on day 1 a change of PROJ-1 adding
// 40 lines, 35 alive at its mark, 4 removed by a later change of PROJ-2 (rework) and 1 by one of
// PROJ-1 (iteration); on day 2 a change of PROJ-5 adding 60, 57 alive, 3 removed by a change of
// PROJ-3 (rework), and a change with no issue that added no line.
func twoMeasuredDays(extra ...fold.Day) fold.BuildInput {
	first := change(1, 1, "agent/a", "PROJ-1", 40, 35)
	second := change(2, 2, "agent/b", "PROJ-5", 60, 57)
	return fold.BuildInput{Project: project, Groups: groups,
		Days:    append([]fold.Day{measured(1), measured(2)}, extra...),
		Changes: []fold.Change{first, second, change(2, 3, "agent/c", "", 0, 0)},
		Pairs: []fold.Pair{
			pairOf(first, change(5, 4, "agent/d", "PROJ-2", 0, 0), 4),
			pairOf(first, change(6, 5, "agent/e", "PROJ-1", 0, 0), 1),
			pairOf(second, change(7, 6, "agent/f", "PROJ-3", 0, 0), 3),
		},
	}
}

func TestFoldSurvivalIsTheDirectShareOfMeasuredDays(t *testing.T) {
	t.Parallel()
	parts, _ := fold.BuildDays(twoMeasuredDays())
	got := figures(fold.FoldSurvival(parts, fold.GroupSeries(fold.SeriesSurvival, "agents")))
	// 92 of 100 lines, not the mean of the days' 87.5 and 95 per cent.
	if want := []any{0.92, 92.0, 100.0, 3.0, 1.0}; !reflect.DeepEqual(got, want) {
		t.Errorf("the agents' survival folds to %v, want %v (share, lines, added, changes, via repository)", got, want)
	}
	if got := figures(fold.FoldSurvival(parts, fold.GroupSeries(fold.SeriesSurvival, "others"))); !reflect.DeepEqual(got, []any{nil, 0.0, 0.0, 0.0, 0.0}) {
		t.Errorf("the others' survival, with no change, folds to %v: want no share of no line, and zero counts", got)
	}
}

func TestFoldReworkDividesTheSummedLines(t *testing.T) {
	t.Parallel()
	_, parts := fold.BuildDays(twoMeasuredDays())
	// 4 + 3 lines of rework over the 100 the agents added, not the mean of the days' 10 and 5 per
	// cent.
	if got, want := figures(fold.FoldRework(parts, fold.GroupSeries(fold.SeriesRework, "agents"))), []any{0.07, 7.0, 100.0, 3.0, 1.0}; !reflect.DeepEqual(got, want) {
		t.Errorf("the agents' rework folds to %v, want %v", got, want)
	}
	if got, want := figures(fold.FoldRework(parts, fold.GroupSeries(fold.SeriesIteration, "agents"))), []any{0.01, 1.0, 100.0, 3.0, 1.0}; !reflect.DeepEqual(got, want) {
		t.Errorf("the agents' iteration folds to %v, want %v", got, want)
	}
}

func TestFoldReadsNoDataAcrossAMarkedOrBlockedDay(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]fold.BuildInput{
		"a marked day":  twoMeasuredDays(fold.Day{Day: day(3), State: survival.DayMarked}),
		"a blocked day": twoMeasuredDays(fold.Day{Day: day(3), State: survival.DayBlocked, BlockedFiles: 2}),
		"an unreadable repository": func() fold.BuildInput {
			in := twoMeasuredDays()
			in.RepoUnreadable = true
			return in
		}(),
	} {
		survivalParts, reworkParts := fold.BuildDays(in)
		if got := figures(fold.FoldSurvival(survivalParts, fold.GroupSeries(fold.SeriesSurvival, "agents"))); !reflect.DeepEqual(got, noData) {
			t.Errorf("%s: the survival folds to %v, want no data", name, got)
		}
		if got := figures(fold.FoldRework(reworkParts, fold.GroupSeries(fold.SeriesRework, "agents"))); !reflect.DeepEqual(got, noData) {
			t.Errorf("%s: the rework folds to %v, want no data", name, got)
		}
		// The window of the measured days alone still reads its number.
		window := func(parts []measures.DayPart) []measures.DayPart {
			var out []measures.DayPart
			for _, part := range parts {
				if part.Day.Before(day(3)) {
					out = append(out, part)
				}
			}
			return out
		}
		if share := fold.FoldSurvival(window(survivalParts), fold.GroupSeries(fold.SeriesSurvival, "agents")).Share; (share == nil) != in.RepoUnreadable {
			t.Errorf("%s: the window of the measured days alone reads the share %v", name, share)
		}
	}
}

func TestFoldReadsNoDataWithNoMeasuredDay(t *testing.T) {
	t.Parallel()
	series := fold.GroupSeries(fold.SeriesSurvival, "agents")
	for name, parts := range map[string][]measures.DayPart{
		"no part": nil,
		"a marked day alone": func() []measures.DayPart {
			parts, _ := fold.BuildDays(fold.BuildInput{Project: project, Groups: groups, Days: []fold.Day{{Day: day(1), State: survival.DayMarked}}})
			return parts
		}(),
		"a blocked day alone": func() []measures.DayPart {
			parts, _ := fold.BuildDays(fold.BuildInput{Project: project, Groups: groups, Days: []fold.Day{{Day: day(1), State: survival.DayBlocked, BlockedFiles: 1}}})
			return parts
		}(),
		"a change on a day that waits": func() []measures.DayPart {
			parts, _ := fold.BuildDays(fold.BuildInput{Project: project, Groups: groups, Changes: []fold.Change{change(1, 1, "agent/a", "", 10, 10)}})
			return parts
		}(),
	} {
		if got := figures(fold.FoldSurvival(parts, series)); !reflect.DeepEqual(got, noData) {
			t.Errorf("%s: the survival folds to %v, want no data, never 0", name, got)
		}
		if got := figures(fold.FoldRework(parts, fold.GroupSeries(fold.SeriesRework, "agents"))); !reflect.DeepEqual(got, noData) {
			t.Errorf("%s: the rework folds to %v, want no data, never 0", name, got)
		}
	}
	// The same change once its day is measured reads a number: the fold can read one.
	parts, _ := fold.BuildDays(fold.BuildInput{Project: project, Groups: groups, Days: []fold.Day{measured(1)},
		Changes: []fold.Change{change(1, 1, "agent/a", "", 10, 10)}})
	if got, want := figures(fold.FoldSurvival(parts, series)), []any{1.0, 10.0, 10.0, 1.0, 1.0}; !reflect.DeepEqual(got, want) {
		t.Errorf("the change's day measured: the survival folds to %v, want %v", got, want)
	}
}

func TestFoldStatusCountsTheWaitingDaysAfterTheNewestRow(t *testing.T) {
	t.Parallel()
	in := fold.BuildInput{Project: project, Groups: groups, Days: []fold.Day{
		measured(1), {Day: day(2), State: survival.DayMarked}, {Day: day(3), State: survival.DayBlocked, BlockedFiles: 4},
		{Day: day(4), State: survival.DayBlocked, BlockedFiles: 4}, measured(5),
	}, Changes: []fold.Change{change(1, 1, "agent/a", "", 10, 10)}}
	parts, _ := fold.BuildDays(in)
	got := fold.FoldStatus(parts, day(0), day(10))
	marked, two := day(2), 2
	want := fold.Status{MeasuredDays: 2, MarkedDays: 1, BlockedDays: 2, BlockedFiles: 8, FirstMarkedDay: &marked,
		Groups: &two, WaitingDays: 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the status folds to %+v, want %+v: days 6 to 9 after the newest row wait", got, want)
	}
	if got := fold.FoldStatus(parts, day(0), day(3)); got.WaitingDays != 0 || got.MeasuredDays != 1 || got.MarkedDays != 1 {
		t.Errorf("the range of days 0 to 2 folds to %+v, want no waiting day, one measured and one marked", got)
	}
	if got, want := fold.FoldStatus(nil, day(0), day(10)), (fold.Status{WaitingDays: 10}); !reflect.DeepEqual(got, want) {
		t.Errorf("a range with no row folds to %+v, want %+v: every day waits", got, want)
	}
	in.RepoUnreadable = true
	parts, _ = fold.BuildDays(in)
	if got := fold.FoldStatus(parts, day(0), day(10)); got.RepoUnreadable != 5 {
		t.Errorf("with the repository unreadable the status folds to %+v, want each of the 5 rows naming it", got)
	}
	var none []time.Time
	for _, part := range parts {
		if part.Day.After(day(5)) {
			none = append(none, part.Day)
		}
	}
	if len(none) != 0 {
		t.Errorf("parts after the newest decided day: %v", none)
	}
}
