package evals

import (
	"fmt"
	"testing"
	"time"
)

// planShape is the moved days of one group a write hands figurePlan.add, in day order, and the end
// day its ranges end by.
type planShape struct {
	name    string
	days    []time.Time
	through time.Time
}

// planDays is count days from first, every step days.
func planDays(first time.Time, count, step int) []time.Time {
	days := make([]time.Time, count)
	for index := range days {
		days[index] = first.AddDate(0, 0, index*step)
	}
	return days
}

// planShapes is the days a rewrite moves of one group, each ending its ranges by the day the
// figures stand built through when the last day is yesterday (FigureLead days after today): no
// day; one day; a run of 200 days to yesterday, as a production rule's rewrite moves; runs whose
// gaps are shorter and longer than the longest range (figureSpan); days every 90 days, whose spans
// never meet, and every third day, whose spans all do; and days around through, whose ranges it cuts
// short, up to days after it, which no range holds, alone and after a day it cuts short.
func planShapes() []planShape {
	yesterday := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	through := yesterday.AddDate(0, 0, 1+FigureLead)
	var gaps []time.Time
	for _, run := range []struct{ back, count int }{{400, 10}, {385, 10}, {200, 20}, {100, 3}} {
		gaps = append(gaps, planDays(yesterday.AddDate(0, 0, -run.back), run.count, 1)...)
	}
	return []planShape{
		{"no day", nil, through},
		{"one day", planDays(yesterday.AddDate(0, 0, -100), 1, 1), through},
		{"200 days to yesterday", planDays(yesterday.AddDate(0, 0, -199), 200, 1), through},
		{"runs with gaps shorter and longer than the longest range", gaps, through},
		{"every 90 days", planDays(yesterday.AddDate(0, 0, -450), 6, 90), through},
		{"every third day", planDays(yesterday.AddDate(0, 0, -177), 60, 3), through},
		{"the days around through", planDays(through.AddDate(0, 0, -5), 8, 1), through},
		{"a day after through", planDays(through.AddDate(0, 0, 1), 1, 1), through},
	}
}

// rangesOfEveryPair is the set of the ranges of key's rows whose span holds one of days and that
// end by through, each day's every (length, end day) pair written into it: what figurePlan.add
// added before it merged each day's run of end days into those of the days before it.
func rangesOfEveryPair(key figureGroupKey, days []time.Time, through time.Time) map[rangeFigureKey]bool {
	ranges := map[rangeFigureKey]bool{}
	for _, day := range days {
		for _, n := range FigureLengths {
			for end := day.AddDate(0, 0, 1); !end.After(day.AddDate(0, 0, n)) && !end.After(through); end = end.AddDate(0, 0, 1) {
				ranges[rangeFigureKey{repo: key.repo, days: n, end: end, eval: key.eval, deployRepo: key.deployRepo}] = true
			}
		}
	}
	return ranges
}

// TestTheFigurePlanAddsEachRangeOfTheMovedDaysOnce: for each shape of moved days (planShapes),
// figurePlan.add adds a group's range of each key of rangesOfEveryPair, the set a write of every
// day's every (length, end day) pair makes, once and no other, each to fold over the group; adds
// a group the read found no row of the same keys as ranges to delete; and adds each moved day's
// own row, to fold or to delete, and nothing else.
func TestTheFigurePlanAddsEachRangeOfTheMovedDaysOnce(t *testing.T) {
	t.Parallel()
	key := figureGroupKey{eval: "ACME/deploy-frequency", figureCut: figureCut{repo: "acme/widgets", deployRepo: "acme/deploy"}}
	group := &figureGroup{key: 2916}
	for _, shape := range planShapes() {
		want := rangesOfEveryPair(key, shape.days, shape.through)
		var held, gone figurePlan
		held.add(group, key, shape.days, shape.through)
		gone.add(nil, key, shape.days, shape.through)
		if len(held.goneRanges) != 0 || len(held.goneDays) != 0 || len(gone.ranges) != 0 || len(gone.days) != 0 {
			t.Errorf("%s: the plan of a held group deletes %d ranges and %d days, and that of a group with no row folds %d ranges and %d days; want none",
				shape.name, len(held.goneRanges), len(held.goneDays), len(gone.ranges), len(gone.days))
		}
		folded := make([]rangeFigureKey, len(held.ranges))
		for index, target := range held.ranges {
			if target.group != group || target.key != group.key || target.held || target.figures != nil {
				t.Errorf("%s: the range %s is added to fold over group %p under fold key %d, held %t; want group %p under %d, not yet folded",
					shape.name, rangeKeyText(target.rangeFigureKey), target.group, target.key, target.held, group, group.key)
			}
			folded[index] = target.rangeFigureKey
		}
		for what, added := range map[string][]rangeFigureKey{"to fold": folded, "to delete": gone.goneRanges} {
			times := map[rangeFigureKey]int{}
			for _, one := range added {
				times[one]++
			}
			for one, n := range times {
				if n != 1 {
					t.Errorf("%s: the range %s is added %d times %s; want once", shape.name, rangeKeyText(one), n, what)
				}
				if !want[one] {
					t.Errorf("%s: the range %s is added %s, which holds no moved day or ends after %s", shape.name, rangeKeyText(one), what, shape.through.Format(time.DateOnly))
				}
			}
			for one := range want {
				if times[one] == 0 {
					t.Errorf("%s: the range %s, which holds a moved day, is not added %s", shape.name, rangeKeyText(one), what)
				}
			}
		}
		if len(held.days) != len(shape.days) || len(gone.goneDays) != len(shape.days) {
			t.Fatalf("%s: %d days are added to fold and %d to delete; want the %d moved", shape.name, len(held.days), len(gone.goneDays), len(shape.days))
		}
		for index, day := range shape.days {
			own := dayFigureKey{repo: key.repo, day: day, eval: key.eval, deployRepo: key.deployRepo}
			if held.days[index].dayFigureKey != own || held.days[index].group != group || gone.goneDays[index] != own {
				t.Errorf("%s: the %d-th day added is %+v to fold and %+v to delete; want %+v", shape.name, index, held.days[index].dayFigureKey, gone.goneDays[index], own)
			}
		}
		t.Logf("%s: %d moved days, %d ranges", shape.name, len(shape.days), len(want))
	}
}

// rewritePlanInput is what a settings rewrite's figure plan adds (readFigurePlan): the 200 days to
// 2026-11-01 of each of 40 groups, eight evals of each of five cuts, every day moved, and the end
// day the ranges end by once the figures stand built through FigureLead days after the last.
func rewritePlanInput() ([]figureGroupKey, []time.Time, time.Time) {
	yesterday := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var keys []figureGroupKey
	for cut := range 5 {
		for eval := range 8 {
			keys = append(keys, figureGroupKey{eval: fmt.Sprintf("BENCH/eval-%d", eval), figureCut: figureCut{repo: fmt.Sprintf("acme/repo-%d", cut), deployRepo: "acme/deploy"}})
		}
	}
	return keys, planDays(yesterday.AddDate(0, 0, -199), 200, 1), yesterday.AddDate(0, 0, 1+FigureLead)
}

// BenchmarkTheFigurePlanOfASettingsRewrite times the plan a settings rewrite's write of the stored
// figures makes of its moved days, past the read of their rows (readFigurePlan): each group's
// ranges and days to fold added (figurePlan.add) and sorted by key, over rewritePlanInput:
// `go test -run '^$' -bench TheFigurePlanOfASettingsRewrite -benchmem ./internal/dispatch/evals`.
func BenchmarkTheFigurePlanOfASettingsRewrite(b *testing.B) {
	keys, days, through := rewritePlanInput()
	group := &figureGroup{}
	b.ReportAllocs()
	ranges := 0
	for b.Loop() {
		var plan figurePlan
		for _, key := range keys {
			plan.add(group, key, days, through)
		}
		plan.sortByKey()
		ranges = len(plan.ranges)
	}
	b.ReportMetric(float64(ranges), "ranges/op")
}
