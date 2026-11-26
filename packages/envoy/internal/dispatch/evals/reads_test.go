package evals

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestDayWindowAnswersTheWindowsDaysAndTheCompareRangesDays: one read answers the day rows of
// the window and of the compare range, each row tagged with its range and holding the days of
// its range alone; with no repository named it answers the project's rows, and with one named
// that repository's rows; and a row sends its sample list only where its series' fold reads one:
// none while the series is folded by its last value (the fold a reading's eval registers it
// with), every value of its day once a register write folds it as a quantile.
func TestDayWindowAnswersTheWindowsDaysAndTheCompareRangesDays(t *testing.T) {
	t.Parallel()
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

	days, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, r.entries(t, evalID), "", nil, window, &compare)
	if err != nil {
		t.Fatalf("read the window and the compare range: %v", err)
	}
	got := dayLabels(days)
	t.Logf("project rows: %v", got)
	want := []string{"compare 2026-11-01 10 no samples", "compare 2026-11-02 20 no samples", "compare 2026-11-03 30 no samples",
		"window 2026-11-04 40 no samples", "window 2026-11-05 50 no samples", "window 2026-11-06 60 no samples"}
	if !slices.Equal(got, want) {
		t.Errorf("the read answers %v, want %v", got, want)
	}

	repoDays, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, r.entries(t, evalID), "", []string{"acme/web"}, window, &compare)
	if err != nil {
		t.Fatalf("read acme/web's window and compare range: %v", err)
	}
	got = dayLabels(repoDays)
	t.Logf("acme/web rows: %v", got)
	if want := []string{"compare 2026-11-02 20 no samples", "window 2026-11-04 40 no samples", "window 2026-11-06 60 no samples"}; !slices.Equal(got, want) {
		t.Errorf("the repository's read answers %v, want %v", got, want)
	}

	windowOnly, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, []Entry{{Definition: Definition{EvalID: "EVAL/no-such-eval"}}}, "", nil, window, nil)
	if err != nil {
		t.Fatalf("read another eval's window: %v", err)
	}
	if len(windowOnly) != 0 {
		t.Errorf("a read of an eval with no day rows answers %v, want none", dayLabels(windowOnly))
	}

	if _, err := r.registry.Register(t.Context(), Registration{
		Project: r.project, Slug: "visits", Caller: alice, Series: []SeriesChange{{Series: "visits", Agg: Set(AggQuantile)}},
	}); err != nil {
		t.Fatalf("fold visits as a quantile: %v", err)
	}
	quantiles, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, r.entries(t, evalID), "", []string{"acme/web"}, window, &compare)
	if err != nil {
		t.Fatalf("read acme/web's rows of a quantile: %v", err)
	}
	got = dayLabels(quantiles)
	t.Logf("acme/web rows of a quantile: %v", got)
	if want := []string{"compare 2026-11-02 20 [20]", "window 2026-11-04 40 [40]", "window 2026-11-06 60 [60]"}; !slices.Equal(got, want) {
		t.Errorf("the rows of a series folded as a quantile answer %v, want %v", got, want)
	}
}

// TestItemsAnswersTheStandingReadingsOfOneRowKindNewestFirst: the items read answers a logged
// eval's readings of one row kind in the window, newest first, pages them by limit and offset
// with the total, and leaves a replaced reading out.
func TestItemsAnswersTheStandingReadingsOfOneRowKindNewestFirst(t *testing.T) {
	t.Parallel()
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
	first, err := r.registry.Items(t.Context(), evalID, "lines", "pull_request", "", nil, window, 2, 0)
	if err != nil {
		t.Fatalf("read the first page: %v", err)
	}
	second, err := r.registry.Items(t.Context(), evalID, "lines", "pull_request", "", nil, window, 2, 2)
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

// TestEachEvalReadsTheDayRowsOfItsOwnWriter: a logged eval's readings write and the computation
// each hold a row of one eval, series and day side by side (eval_days.writer, R3), and every read
// of an eval's rows reads its definition's writer's alone: the window read, the first day and the
// items route's day rows. Logged, the eval reads the reading's row and the zero rows its write left
// after it (writeLoggedZeros), and the reading's day as its first; read as computed, the
// computation's two rows, the earlier of them its first day.
func TestEachEvalReadsTheDayRowsOfItsOwnWriter(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	evalID := r.write(t, "load", alice, row(t, map[string]any{
		"series": "load", "row_kind": "run", "row_key": "r", "value": 7, "measured_at": at(4, 9, 0),
	})).Definition.EvalID
	if _, err := r.database.Pool.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, writer, series, repo, day, listed, n, sum, built_by)
		values ($1, $2, 'computation', 'load', '', '2026-11-03', true, 3, 3, 'test'),
		       ($1, $2, 'computation', 'load', '', '2026-11-04', true, 3, 3, 'test')
	`, r.project, evalID); err != nil {
		t.Fatalf("write the computation's rows of %s: %v", evalID, err)
	}
	logged := r.entries(t, evalID)[0]
	computed := logged
	computed.Definition.Source = SourceFoldDeployFrequency
	window := Range{From: at(1, 0, 0), To: at(7, 0, 0)}
	for _, test := range []struct {
		as    string
		entry Entry
		rows  []string
		first string
	}{
		{"logged", logged, []string{"window 2026-11-04 7 no samples", "window 2026-11-05 0 no samples", "window 2026-11-06 0 no samples"}, "2026-11-04"},
		{"computed", computed, []string{"window 2026-11-03 3 no samples", "window 2026-11-04 3 no samples"}, "2026-11-03"},
	} {
		days, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, []Entry{test.entry}, "", nil, window, nil)
		if err != nil {
			t.Fatalf("%s: read the window: %v", test.as, err)
		}
		firsts, err := FirstDaysIn(t.Context(), r.database.Pool, []Entry{test.entry}, "")
		if err != nil {
			t.Fatalf("%s: read the first days: %v", test.as, err)
		}
		items, err := EvalDaysIn(t.Context(), r.database.Pool, test.entry.Definition, "", "", nil, window)
		if err != nil {
			t.Fatalf("%s: read the items' day rows: %v", test.as, err)
		}
		var itemDays []string
		for _, day := range items {
			itemDays = append(itemDays, fmt.Sprintf("window %s %v no samples", day.Day.Format(time.DateOnly), *day.Sum))
		}
		slices.Sort(itemDays)
		first := firsts[evalID]["load"].Format(time.DateOnly)
		t.Logf("%s: the window reads %v, the items %v, the first day %s", test.as, dayLabels(days), itemDays, first)
		if !slices.Equal(dayLabels(days), test.rows) || !slices.Equal(itemDays, test.rows) || first != test.first {
			t.Errorf("%s: the window reads %v, the items %v and the first day is %s; want %v and %s", test.as, dayLabels(days), itemDays, first, test.rows, test.first)
		}
	}
}

// dayLabels names each day row by its range, day, sum and sample list, sorted.
func dayLabels(days []WindowRow) []string {
	labels := make([]string, len(days))
	for index, day := range days {
		samples := "no samples"
		if day.Part.Samples != nil {
			samples = fmt.Sprint(day.Part.Samples)
		}
		labels[index] = fmt.Sprintf("%s %s %v %s", day.Range, day.Part.Day.Format(time.DateOnly), *day.Part.Sum, samples)
	}
	sort.Strings(labels)
	return labels
}

// TestTheListReadsOfAFullPageOfTheLargestEvalsSendBoundedStatements: the reads the list read makes
// of a full page of evals (200, the most a page holds), each a logged eval of MaxEvalSeries series
// folded as quantiles whose names and slug are the largest a key holds, over a window and a compare
// range: the day rows (DayWindowIn), the series' first days (FirstDaysIn) and the stored figures
// of a project whose figures are built through both ranges (ReadStoredFiguresIn, its range and day
// figures reads among them), each on the pool, send statements that each take at most
// statementBytes of parameters as pgx encodes them, the day rows' in several; and the day rows of
// the three evals that hold some come back once each, each with its samples, and their first days
// with them.
func TestTheListReadsOfAFullPageOfTheLargestEvalsSendBoundedStatements(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	const page = 200
	day := at(3, 0, 0)
	largest := func(prefix string, index int) string {
		head := fmt.Sprintf("%s%04d-", prefix, index)
		return head + strings.Repeat("x", maxKeyBytes-len(head))
	}
	entries := make([]Entry, page)
	for index := range entries {
		slug := largest("e", index)
		evalID := EvalID(r.project, slug)
		entry := Entry{Definition: Definition{EvalID: evalID, ProjectKey: r.project, Slug: slug, Source: SourceLogged}}
		for series := range MaxEvalSeries {
			entry.Series = append(entry.Series, Series{EvalID: evalID, Series: largest("s", series),
				SeriesComputation: SeriesComputation{Role: RoleHeadline, Listed: true, Agg: AggQuantile}})
		}
		entries[index] = entry
	}
	// Three of the evals hold day rows on day: two values of their first series and one of their last.
	held := []int{0, page / 2, page - 1}
	for _, index := range held {
		var rows []json.RawMessage
		for at, reading := range []struct {
			series int
			value  float64
		}{{0, 1}, {0, 2}, {MaxEvalSeries - 1, 3}} {
			rows = append(rows, row(t, map[string]any{"series": largest("s", reading.series), "row_kind": "run", "row_key": fmt.Sprint(at),
				"value": reading.value, "measured_at": day.Add(time.Duration(at) * time.Hour)}))
		}
		r.write(t, largest("e", index), alice, rows...)
	}
	if _, err := r.database.Pool.Exec(t.Context(), `insert into eval_figures_built (project_key, through, built_by) values ($1, $2, 'test')`,
		r.project, day.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("mark %s's stored figures built: %v", r.project, err)
	}
	statements := &sentStatements{}
	traced := New(storetest.TracedStore(t, r.database, statements), r.documents, r.registry.events, r.registry.attributor)
	window, compare := Range{From: day, To: day.AddDate(0, 0, 1)}, Range{From: day.AddDate(0, 0, -7), To: day.AddDate(0, 0, -6)}
	days, err := DayWindowIn(t.Context(), traced.database.Pool, []string{r.project}, entries, "", nil, window, &compare)
	if err != nil {
		t.Fatalf("read the day rows of %d evals: %v", page, err)
	}
	firsts, err := FirstDaysIn(t.Context(), traced.database.Pool, entries, "")
	if err != nil {
		t.Fatalf("read the first days of %d evals: %v", page, err)
	}
	if _, err := ReadStoredFiguresIn(t.Context(), traced.database.Pool, []string{r.project}, entries, "", nil, window, &compare); err != nil {
		t.Fatalf("read the stored figures of %d evals: %v", page, err)
	}
	sent := r.measured(t, statements.take())
	windows := 0
	figures := map[string]bool{}
	for _, statement := range sent {
		for _, table := range []string{"eval_range_figures", "eval_day_figures"} {
			if strings.Contains(statement.sql, "from "+table) {
				figures[table] = true
			}
		}
		if statement.bytes > statementBytes {
			t.Errorf("the list's reads sent %.80q with %d bytes of parameters as pgx encodes them, past %d", strings.Join(strings.Fields(statement.sql), " "), statement.bytes, statementBytes)
		}
		if strings.Contains(statement.sql, "then samples end") {
			windows++
		}
	}
	if windows < 2 {
		t.Errorf("the day rows of %d evals of %d series were read in %d statements, and the check needs them cut", page, MaxEvalSeries, windows)
	}
	if len(figures) != 2 {
		t.Errorf("the stored figures of %d evals were read from %v, and the check needs both their range and their day figures read", page, slices.Sorted(maps.Keys(figures)))
	}
	var want, got []string
	for _, index := range held {
		evalID := entries[index].Definition.EvalID
		want = append(want, fmt.Sprintf("%.6s %.6s 2", evalID[len(r.project)+1:], largest("s", 0)), fmt.Sprintf("%.6s %.6s 1", evalID[len(r.project)+1:], largest("s", MaxEvalSeries-1)))
		for _, series := range []string{largest("s", 0), largest("s", MaxEvalSeries-1)} {
			if first, ok := firsts[evalID][series]; !ok || !first.Equal(day) {
				t.Errorf("the first day of %.6s's series %.6s is %v (%t), want %s", evalID[len(r.project)+1:], series, first, ok, day.Format(time.DateOnly))
			}
		}
	}
	for _, one := range days {
		if one.Range != RangeWindow || !one.Part.Day.Equal(day) || one.Part.N != len(one.Part.Samples) {
			t.Errorf("a day row of %.6s's series %.6s answers %s on %s with n %d and samples %v, want the window's day and its samples",
				one.EvalID[len(r.project)+1:], one.Part.Series, one.Range, one.Part.Day.Format(time.DateOnly), one.Part.N, one.Part.Samples)
		}
		got = append(got, fmt.Sprintf("%.6s %.6s %d", one.EvalID[len(r.project)+1:], one.Part.Series, len(one.Part.Samples)))
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the day rows read are %v, want %v", got, want)
	}
	largestBytes, what := largestStatement(sent)
	t.Logf("%d statements, %d of the day rows; the largest %d bytes of parameters (%s), of %d", len(sent), windows, largestBytes, what, statementBytes)
}
