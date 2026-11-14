package metrics

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// sampleDay is a hand-built sample part of series on day 2026-11-<day>: values, and their sum.
func sampleDay(day int, series string, values ...float64) measures.DayPart {
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	if values == nil {
		values = []float64{}
	}
	return measures.DayPart{
		Series: series, Day: time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC), N: len(values), Sum: &sum, Samples: values,
	}
}

// showFold is a sample fold as `n=… sum=… median=… p90=…`, or `no value`.
func showFold(fold *SampleFold) string {
	if fold == nil {
		return "no value"
	}
	show := func(value *float64) string {
		if value == nil {
			return "none"
		}
		return fmt.Sprint(*value)
	}
	return fmt.Sprintf("n=%d sum=%g median=%s p90=%s", fold.N, fold.Sum, show(fold.Median), show(fold.P90))
}

// TestFoldSlotWaitMergesTheDaysSamplesOfItsCut: FoldSlotWait over hand-built parts of two days
// sums the hours and reads the median and p90 off the merge of both days' samples (4 and 8.8
// here, where the mean of the two days' medians would be 6), reads each cut's own series alone,
// passes over a series of another name, and answers no value for a cut no part holds and for no
// part at all; a zero part (a cut with no issue on its day) answers zero issues and no quantile.
func TestFoldSlotWaitMergesTheDaysSamplesOfItsCut(t *testing.T) {
	parts := []measures.DayPart{
		sampleDay(3, SeriesHours, 4, 0),
		sampleDay(4, SeriesHours, 10),
		sampleDay(3, CutSeries(SeriesHours, ScopeRoots), 4),
		sampleDay(4, CutSeries(SeriesHours, ScopeRoots)),
		// Another eval's series, which a caller that hands over a mixed set must not see counted.
		sampleDay(3, "architect", 100),
	}
	for _, one := range []struct {
		name  string
		parts []measures.DayPart
		cut   Scope
		want  string
	}{
		{"every issue of both days", parts, ScopeAll, "n=3 sum=14 median=4 p90=8.8"},
		{"the roots, one of them on a zero day", parts, ScopeRoots, "n=1 sum=4 median=4 p90=4"},
		{"the children, whom no part holds", parts, ScopeChildren, "no value"},
		{"a zero part alone", []measures.DayPart{sampleDay(5, SeriesHours)}, ScopeAll, "n=0 sum=0 median=none p90=none"},
		{"no part at all", nil, ScopeAll, "no value"},
	} {
		if got := showFold(FoldSlotWait(one.parts, one.cut)); got != one.want {
			t.Errorf("%s: FoldSlotWait = %s, want %s", one.name, got, one.want)
		}
	}
}

// TestFoldIdleTimeAnswersEachRoleItsPartsHold: FoldIdleTime over hand-built parts answers each
// role whose series the parts hold, its hours summed across the days and its median and p90 read
// off the merged samples; a role no part holds (the tester's here) has no entry rather than a
// zero; the no-role bar, which has no idle time, is never read; and a set holding no role of the
// cut answers no value at all.
func TestFoldIdleTimeAnswersEachRoleItsPartsHold(t *testing.T) {
	parts := []measures.DayPart{
		sampleDay(3, "architect", 2, 0, 5),
		sampleDay(4, "architect", 1),
		sampleDay(3, "implementer", 0, 3),
		sampleDay(3, CutSeries("implementer", ScopeChildren), 3),
		sampleDay(3, barNoRole, 7),
	}
	idle := FoldIdleTime(parts, ScopeAll)
	want := map[string]string{
		"architect":   "n=4 sum=8 median=1.5 p90=4.1",
		"implementer": "n=2 sum=3 median=1.5 p90=2.7",
	}
	if len(idle) != len(want) {
		t.Errorf("FoldIdleTime answers %d roles, want %d: %v", len(idle), len(want), idle)
	}
	for role, wanted := range want {
		fold, held := idle[role]
		if !held {
			t.Errorf("FoldIdleTime answers no %s, whose parts hold %s", role, wanted)
			continue
		}
		if got := showFold(&fold); got != wanted {
			t.Errorf("the %s's idle time = %s, want %s", role, got, wanted)
		}
	}
	for _, absent := range []string{"tester", barNoRole} {
		if fold, held := idle[absent]; held {
			t.Errorf("the %s's idle time = %s, want no entry: no part of its series is in the set", absent, showFold(&fold))
		}
	}
	children := FoldIdleTime(parts, ScopeChildren)
	if got := showFold(new(children["implementer"])); len(children) != 1 || got != "n=1 sum=3 median=3 p90=3" {
		t.Errorf("the children cut = %v, want the implementer alone, n=1 sum=3", children)
	}
	if got := FoldIdleTime(parts, ScopeRoots); got != nil {
		t.Errorf("the roots cut, which no part holds, = %v, want no value", got)
	}
	if got := FoldIdleTime(nil, ScopeAll); got != nil {
		t.Errorf("FoldIdleTime of no part = %v, want no value", got)
	}
}

// TestTheIdleAndSlotWaitBuildersCutEachIssueByItsRoot: the day builders put every finished
// issue's value in the all cut and in its own cut, roots or children, a zero where a role had no
// idle hour, so the folds of the built parts read what the issues hold; the builders of a day with
// no issue build zero parts in every cut.
func TestTheIdleAndSlotWaitBuildersCutEachIssueByItsRoot(t *testing.T) {
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	finished := []IssueFacts{
		{Root: true, SlotWaitHours: 6, IdleByRole: map[string]float64{"architect": 2, "planner": 1}},
		{Root: false, SlotWaitHours: 1, IdleByRole: map[string]float64{"implementer": 4}},
		{Root: false, SlotWaitHours: 0, IdleByRole: map[string]float64{"implementer": 2, "tester": 0.5}},
	}
	slot := SlotWaitDay(day, finished)
	for _, one := range []struct {
		cut  Scope
		want string
	}{
		{ScopeAll, "n=3 sum=7 median=1 p90=5"},
		{ScopeRoots, "n=1 sum=6 median=6 p90=6"},
		{ScopeChildren, "n=2 sum=1 median=0.5 p90=0.9"},
	} {
		if got := showFold(FoldSlotWait(slot, one.cut)); got != one.want {
			t.Errorf("slot wait, %s = %s, want %s", one.cut, got, one.want)
		}
	}
	idle := IdleTimeDay(day, finished)
	if len(idle) != 6*3 {
		t.Errorf("IdleTimeDay built %d parts, want one for each of six roles in three cuts", len(idle))
	}
	for _, one := range []struct {
		cut  Scope
		role string
		want string
	}{
		{ScopeAll, "implementer", "n=3 sum=6 median=2 p90=3.6"},
		{ScopeRoots, "architect", "n=1 sum=2 median=2 p90=2"},
		{ScopeChildren, "architect", "n=2 sum=0 median=0 p90=0"},
		{ScopeChildren, "tester", "n=2 sum=0.5 median=0.25 p90=0.45"},
		{ScopeAll, "merger", "n=3 sum=0 median=0 p90=0"},
	} {
		fold := FoldIdleTime(idle, one.cut)[one.role]
		if got := showFold(&fold); got != one.want {
			t.Errorf("idle time, %s, %s = %s, want %s", one.cut, one.role, got, one.want)
		}
	}
	for _, cut := range []Scope{ScopeAll, ScopeRoots, ScopeChildren} {
		if got := showFold(FoldSlotWait(SlotWaitDay(day, nil), cut)); got != "n=0 sum=0 median=none p90=none" {
			t.Errorf("slot wait of a day with no issue, %s = %s, want a zero part", cut, got)
		}
	}
}
