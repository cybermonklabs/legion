package evals

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"
)

// TestDayWindowAnswersTheWindowsDaysAndTheCompareRangesDays: one read answers the day rows of
// the window and of the compare range, each row tagged with its range and holding the days of
// its range alone; with no repository named it answers the project's rows, and with one named
// that repository's rows.
func TestDayWindowAnswersTheWindowsDaysAndTheCompareRangesDays(t *testing.T) {
	r := newRig(t)
	var rows []json.RawMessage
	for day := 1; day <= 6; day++ {
		attributes := map[string]any{}
		if day%2 == 0 {
			attributes["repo"] = "acme/web"
		}
		rows = append(rows, row(t, map[string]any{
			"series": "visits", "row_kind": "day", "row_key": fmt.Sprintf("2026-11-%02d", day), "value": float64(day * 10),
			"measured_at": at(day, 12, 0), "attributes": attributes,
		}))
	}
	written := r.write(t, "visits", alice, rows...)
	evalID := written.Definition.EvalID
	window := Range{From: at(4, 0, 0), To: at(7, 0, 0)}
	compare := Range{From: at(1, 0, 0), To: at(4, 0, 0)}

	days, err := r.registry.DayWindow(t.Context(), r.project, nil, nil, window, &compare)
	if err != nil {
		t.Fatalf("read the window and the compare range: %v", err)
	}
	got := dayLabels(days)
	t.Logf("project rows: %v", got)
	want := []string{"compare 2026-11-01 '' 10", "compare 2026-11-02 '' 20", "compare 2026-11-03 '' 30",
		"window 2026-11-04 '' 40", "window 2026-11-05 '' 50", "window 2026-11-06 '' 60"}
	if !slices.Equal(got, want) {
		t.Errorf("the read answers %v, want %v", got, want)
	}

	repoDays, err := r.registry.DayWindow(t.Context(), r.project, []string{evalID}, []string{"acme/web"}, window, &compare)
	if err != nil {
		t.Fatalf("read acme/web's window and compare range: %v", err)
	}
	got = dayLabels(repoDays)
	t.Logf("acme/web rows: %v", got)
	if want := []string{"compare 2026-11-02 acme/web 20", "window 2026-11-04 acme/web 40", "window 2026-11-06 acme/web 60"}; !slices.Equal(got, want) {
		t.Errorf("the repository's read answers %v, want %v", got, want)
	}

	windowOnly, err := r.registry.DayWindow(t.Context(), r.project, []string{"EVAL/no-such-eval"}, nil, window, nil)
	if err != nil {
		t.Fatalf("read another eval's window: %v", err)
	}
	if len(windowOnly) != 0 {
		t.Errorf("a read of an eval with no day rows answers %v, want none", dayLabels(windowOnly))
	}
}

// TestItemsAnswersTheStandingReadingsOfOneRowKindNewestFirst: the items read answers a logged
// eval's readings of one row kind in the window, newest first, pages them by limit and offset
// with the total, and leaves a replaced reading out.
func TestItemsAnswersTheStandingReadingsOfOneRowKindNewestFirst(t *testing.T) {
	r := newRig(t)
	written := r.write(t, "pr-size", alice,
		row(t, map[string]any{"series": "lines", "row_kind": "pull_request", "row_key": "acme/web#1", "value": 100, "measured_at": at(8, 9, 0)}),
		row(t, map[string]any{"series": "lines", "row_kind": "pull_request", "row_key": "acme/web#2", "value": 200, "measured_at": at(8, 10, 0)}),
		row(t, map[string]any{"series": "lines", "row_kind": "pull_request", "row_key": "acme/web#3", "value": 300, "measured_at": at(8, 11, 0)}),
		row(t, map[string]any{"series": "lines", "row_kind": "day", "row_key": "2026-11-08", "value": 600, "measured_at": at(8, 23, 0)}),
	)
	evalID := written.Definition.EvalID
	r.write(t, "pr-size", alice, row(t, map[string]any{
		"series": "lines", "row_kind": "pull_request", "row_key": "acme/web#2", "value": 250, "measured_at": at(8, 10, 0), "replaces": written.Readings[1].ID,
	}))
	window := Range{From: at(8, 0, 0), To: at(9, 0, 0)}
	first, err := r.registry.Items(t.Context(), evalID, "lines", "pull_request", "", window, 2, 0)
	if err != nil {
		t.Fatalf("read the first page: %v", err)
	}
	second, err := r.registry.Items(t.Context(), evalID, "lines", "pull_request", "", window, 2, 2)
	if err != nil {
		t.Fatalf("read the second page: %v", err)
	}
	values := func(page Page[Reading]) []float64 {
		var all []float64
		for _, reading := range page.Items {
			all = append(all, *reading.Value)
		}
		return all
	}
	t.Logf("total %d; page 1 %v; page 2 %v", first.Total, values(first), values(second))
	if first.Total != 3 || !slices.Equal(values(first), []float64{300, 250}) || !slices.Equal(values(second), []float64{100}) {
		t.Errorf("the items read answers %d in all, %v then %v, want 3, [300 250] then [100]", first.Total, values(first), values(second))
	}
}

// dayLabels names each day row by its range, day, repository and sum, sorted.
func dayLabels(days []DayRow) []string {
	labels := make([]string, len(days))
	for index, day := range days {
		repo := day.Repo
		if repo == "" {
			repo = "''"
		}
		labels[index] = fmt.Sprintf("%s %s %s %v", day.Range, day.Day.Format(time.DateOnly), repo, *day.Sum)
	}
	sort.Strings(labels)
	return labels
}
