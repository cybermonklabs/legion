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

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testwait"
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

	days, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, r.entries(t, evalID), nil, nil, window, &compare)
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

	repoDays, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, r.entries(t, evalID), nil, []string{"acme/web"}, window, &compare)
	if err != nil {
		t.Fatalf("read acme/web's window and compare range: %v", err)
	}
	got = dayLabels(repoDays)
	t.Logf("acme/web rows: %v", got)
	if want := []string{"compare 2026-11-02 20 no samples", "window 2026-11-04 40 no samples", "window 2026-11-06 60 no samples"}; !slices.Equal(got, want) {
		t.Errorf("the repository's read answers %v, want %v", got, want)
	}

	windowOnly, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, []Entry{{Definition: Definition{EvalID: "EVAL/no-such-eval"}}}, nil, nil, window, nil)
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
	quantiles, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, r.entries(t, evalID), nil, []string{"acme/web"}, window, &compare)
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
	first, err := ItemsIn(t.Context(), r.database.Pool, evalID, "lines", "pull_request", "", nil, window, 2, 0)
	if err != nil {
		t.Fatalf("read the first page: %v", err)
	}
	second, err := ItemsIn(t.Context(), r.database.Pool, evalID, "lines", "pull_request", "", nil, window, 2, 2)
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
		days, err := DayWindowIn(t.Context(), r.database.Pool, []string{r.project}, []Entry{test.entry}, nil, nil, window, nil)
		if err != nil {
			t.Fatalf("%s: read the window: %v", test.as, err)
		}
		firsts, err := FirstDaysIn(t.Context(), r.database.Pool, []Entry{test.entry}, nil)
		if err != nil {
			t.Fatalf("%s: read the first days: %v", test.as, err)
		}
		items, err := EvalDaysIn(t.Context(), r.database.Pool, test.entry.Definition, nil, "", nil, window)
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

func TestTheItemsDayOfTheDefaultRepositoryCombinesItsParts(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const repo = "acme/web"
	day := at(4, 0, 0)
	definition := Definition{ProjectKey: r.project, EvalID: EvalID(r.project, SlugLegionWorkTime), Source: SourceFoldWorkTime}
	if _, err := pool.Exec(ctx, `insert into eval_days_defaults (project_key, repo) values ($1, $2)`, r.project, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
		values ($1, $2, 'hours', $3, $5, true, 1, 4, 'test'),
		       ($1, $2, 'hours', $4, $5, true, 2, 7, 'test'),
		       ($1, $2, 'hours', 'acme/api', $5, true, 1, 20, 'test')
	`, r.project, definition.EvalID, repo, legionfacts.DefaultRepo, day); err != nil {
		t.Fatal(err)
	}
	days, err := EvalDaysIn(ctx, pool, definition, nil, "", []string{repo, "acme/api"}, Range{From: day, To: day.AddDate(0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range days {
		got = append(got, fmt.Sprintf("%s: n=%d sum=%s", row.Repo, row.N, showValue(row.Sum)))
	}
	if want := []string{"acme/api: n=1 sum=20", "acme/web: n=3 sum=11"}; !slices.Equal(got, want) {
		t.Errorf("items day rows = %v, want %v", got, want)
	}
}

func TestItemsDayRowsSendSamplesOnlyForPRCIJobs(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const repo = "acme/web"
	day := at(4, 0, 0)
	definition := Definition{ProjectKey: r.project, EvalID: EvalID(r.project, SlugPRCI), Slug: SlugPRCI, Source: SourceFoldTiming}
	const job = measures.JobSeriesPrefix + "lint"
	if _, err := pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, samples, built_by)
		values ($1, $2, $3, 'minutes', '', $4, true, 1, '{9}', 'test'),
		       ($1, $2, $3, $5, '', $4, false, 2, '{2,6}', 'test')
	`, r.project, definition.EvalID, repo, day, job); err != nil {
		t.Fatal(err)
	}
	days, err := EvalDaysIn(ctx, pool, definition, DeployReposOf(r.project, repo), "", nil, Range{From: day, To: day.AddDate(0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("items read %d day rows, want both headline and job", len(days))
	}
	for _, row := range days {
		if row.Series == job {
			row.DayPart.Series = measures.SeriesMinutes
			cell := measures.FoldTiming([]measures.DayPart{row.DayPart})
			if cell.MedianMinutes == nil || *cell.MedianMinutes != 4 {
				t.Errorf("job median = %s, want 4", showValue(cell.MedianMinutes))
			}
		} else if row.Samples != nil {
			t.Errorf("%s day sent its samples %v, want none", row.Series, row.Samples)
		}
	}
}

func TestALargeRegistryReadBoundsItsSeriesStatements(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	definitions := make([]Definition, 600)
	for i := range definitions {
		slug := fmt.Sprintf("eval-%03d-", i) + strings.Repeat("x", 500)
		definitions[i] = Definition{EvalID: EvalID(r.project, slug), Slug: slug}
	}
	for _, i := range []int{0, len(definitions) - 1} {
		r.write(t, definitions[i].Slug, alice, row(t, map[string]any{
			"series": "minutes", "row_kind": "run", "row_key": "one", "value": 1, "measured_at": at(3, 9, 0),
		}))
	}
	trace := &sentStatements{}
	pool := storetest.TracedStore(t, r.database, trace).Pool
	entries, err := entriesOf(t.Context(), pool, definitions, r.project)
	if err != nil {
		t.Fatal(err)
	}
	for i, definition := range definitions {
		if entries[i].Definition.EvalID != definition.EvalID {
			t.Errorf("entry %d = %q, want %q", i, entries[i].Definition.EvalID, definition.EvalID)
		}
	}
	for _, i := range []int{0, len(definitions) - 1} {
		if series := entries[i].Series; len(series) != 1 || series[0].Series != "minutes" {
			t.Errorf("entry %d has series %v, want minutes", i, series)
		}
	}
	sent := r.measured(t, trace.take())
	largest, _ := largestStatement(sent)
	if len(sent) < 2 || largest > 256<<10 {
		t.Errorf("series read sent %d statements, largest %d bytes; want several within 256 KiB", len(sent), largest)
	}
}

func TestARequestForAnotherSpellingReadsTheOneStoredCutAndItsMark(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const storedRepo, requestedRepo = "Acme/Web", "acme/web"
	window := Range{From: at(4, 0, 0), To: at(11, 0, 0)}
	catalogued := entryOf(t, SlugPROpenToMerge)
	entry := Entry{Definition: catalogued.definition, Series: catalogued.series}
	entry.Definition.ProjectKey = r.project
	entry.Definition.EvalID = EvalID(r.project, SlugPROpenToMerge)
	if _, err := pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, samples, built_by)
		values ($1, $2, 'minutes', $3, $4, true, 1, '{12}', 'test')`,
		r.project, entry.Definition.EvalID, storedRepo, window.From); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into eval_figures_built (project_key, through, built_by) values ($1, $2, 'test')`, r.project, window.To); err != nil {
		t.Fatal(err)
	}
	figures := encodeFigures(map[string]Statistics{"minutes": {"median": new(12.0)}}, nil)
	if _, err := pool.Exec(ctx, `
		insert into eval_range_figures (project_key, repo, days, end_day, eval_id, fold_key, figures, oldest_updated_at, built_by)
		values ($1, $2, 7, $3, $4, $5, $6, now(), 'test')`,
		r.project, storedRepo, window.To, entry.Definition.EvalID, foldKeyOf(entry), figures); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into eval_day_figures (project_key, repo, day, eval_id, fold_key, figures, rows_stamp, built_by)
		values ($1, $2, $3, $4, $5, $6, 0, 'test')`,
		r.project, storedRepo, window.From, entry.Definition.EvalID, foldKeyOf(entry), figures); err != nil {
		t.Fatal(err)
	}
	read := func(repo string) ([]SeriesValues, bool) {
		t.Helper()
		repos, err := StoredRepos(ctx, pool, []string{r.project}, []string{repo})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(repos, []string{storedRepo}) || !FiguresOffered(repos, window, nil) {
			t.Fatalf("%q resolved to %v, want the one stored cut %q", repo, repos, storedRepo)
		}
		got, err := ReadStoredFiguresIn(ctx, pool, []string{r.project}, []Entry{entry}, repos, window, nil)
		if err != nil {
			t.Fatal(err)
		}
		values, _, served := got.SeriesValues(entry, map[string]time.Time{"minutes": window.From}, window, nil)
		return values, served
	}
	for _, repo := range []string{storedRepo, requestedRepo} {
		values, served := read(repo)
		if !served || len(values) != 1 || showValue(values[0].Window["median"]) != "12" || len(values[0].Days) != 1 {
			t.Errorf("%q: cached read served=%t with %+v, want median 12 and one day", repo, served, values)
		}
	}
	if _, err := pool.Exec(ctx, `
		insert into eval_figure_cuts_stale (project_key, repo, generation) values ($1, $2, 1)`, r.project, storedRepo); err != nil {
		t.Fatal(err)
	}
	if _, served := read(requestedRepo); served {
		t.Errorf("%q was served despite the stale mark on %q", requestedRepo, storedRepo)
	}
}

func TestARepositoryReadFoldsBothStoredSpellingsAndThePublishedDefault(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const published, delivered = "acme/web", "Acme/Web"
	owner := OwnerKey(delivered, nil, nil)
	day := at(4, 0, 0)
	window := Range{From: day, To: day.AddDate(0, 0, 7)}
	if _, err := pool.Exec(ctx, `insert into eval_days_defaults (project_key, repo) values ($1, $2)`, r.project, published); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
		values ($1, $2, 'hours', $3, $7, true, 1, 4, 'test'),
		       ($4, $5, 'minutes', $6, $7, true, 2, 7, 'test')
	`, r.project, EvalID(r.project, SlugLegionWorkTime), legionfacts.DefaultRepo,
		owner, EvalID(owner, SlugPROpenToMerge), delivered, day); err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{Definition: Definition{ProjectKey: r.project, EvalID: EvalID(r.project, SlugLegionWorkTime), Source: SourceFoldWorkTime}},
		{Definition: Definition{ProjectKey: r.project, EvalID: EvalID(r.project, SlugPROpenToMerge), Source: SourceFoldLeadTime}},
	}
	projects := []string{r.project}
	for _, repo := range []string{published, delivered, "ACME/WEB"} {
		repos, err := StoredRepos(ctx, pool, projects, []string{repo})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(repos, []string{delivered, published}) || FiguresOffered(repos, window, nil) {
			t.Errorf("%q resolved to %v, want both stored spellings and a day-row fold", repo, repos)
		}
		rows, err := DayWindowIn(ctx, pool, projects, entries, nil, repos, window, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, row := range rows {
			got[row.EvalID] += row.Part.N
		}
		if len(got) != 2 || got[entries[0].Definition.EvalID] != 1 || got[entries[1].Definition.EvalID] != 2 {
			t.Errorf("window for %q counted %v, want one Legion and two delivery samples", repo, got)
		}
	}
	missing := []string{"No/Such-Repository"}
	repos, err := StoredRepos(ctx, pool, projects, missing)
	if err != nil || !slices.Equal(repos, missing) {
		t.Errorf("resolve an unknown name: %v, %v; want the name as given", repos, err)
	}
}

func TestStoredReposProbesOneIndexEntryPerRepository(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const repositories, days = 80, 128
	if _, err := pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
		select $1, $2, 'minutes', 'Acme/Repo-' || r, $3::date + d, true, 1, 1, 'test'
		from generate_series(1, $4::int) r cross join generate_series(0, $5::int - 1) d
	`, r.project, EvalID(r.project, SlugPROpenToMerge), at(4, 0, 0), repositories, days); err != nil {
		t.Fatal(err)
	}
	// Index-only probes need the visibility map, not a heap fetch to confirm each next key.
	testwait.UntilPolling(t, 100*time.Millisecond, "all fixture day pages visible", func() (int, bool) {
		if _, err := pool.Exec(ctx, `vacuum (freeze, analyze) eval_days`); err != nil {
			t.Fatal(err)
		}
		var pages, visible int
		if err := pool.QueryRow(ctx, `select relpages, relallvisible from pg_class where oid = 'eval_days'::regclass`).Scan(&pages, &visible); err != nil {
			t.Fatal(err)
		}
		return pages - visible, pages > 0 && pages == visible
	})
	trace := &sentStatements{}
	traced := storetest.TracedStore(t, r.database, trace).Pool
	projects := []string{r.project}
	if got, err := StoredRepos(ctx, traced, projects, nil); got != nil || err != nil || len(trace.take()) != 0 {
		t.Fatalf("resolve an unfiltered read: %v, %v; want no names and no statement", got, err)
	}
	got, err := StoredRepos(ctx, traced, projects, []string{"acme/repo-3"})
	if err != nil || !slices.Equal(got, []string{"Acme/Repo-3"}) {
		t.Fatalf("resolve acme/repo-3: %v, %v; want Acme/Repo-3", got, err)
	}
	sent := trace.take()
	if len(sent) != 1 {
		t.Fatalf("resolving one name among %d sent %d statements, want one", repositories, len(sent))
	}
	plan, _ := explaintest.AnalyzedPlan(t, ctx, pool, sent[0].SQL, sent[0].Args...)
	probes, visited, heap := 0.0, 0.0, 0.0
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation != "eval_days" {
			continue
		}
		if node.Type != "Index Only Scan" || node.Index != "eval_days_window" {
			t.Errorf("resolver reads days outside the window index:\n%s", explaintest.Outline(t, plan))
		}
		probes += node.Loops
		visited += (node.Rows + node.RowsRemovedByFilter) * node.Loops
		heap += node.HeapFetches
	}
	if probes == 0 || probes > repositories+1 || visited > repositories+1 || heap != 0 {
		t.Errorf("resolver used %.0f probes, saw %.0f index entries and fetched %.0f heap rows; want at most one entry per repository and no heap rows\n%s",
			probes, visited, heap, explaintest.Outline(t, plan))
	}
	t.Logf("one statement: %.0f probes, %.0f index entries, %.0f heap fetches for %d repositories and %d day rows", probes, visited, heap, repositories, repositories*days)
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
	days, err := DayWindowIn(t.Context(), traced.database.Pool, []string{r.project}, entries, nil, nil, window, &compare)
	if err != nil {
		t.Fatalf("read the day rows of %d evals: %v", page, err)
	}
	firsts, err := FirstDaysIn(t.Context(), traced.database.Pool, entries, nil)
	if err != nil {
		t.Fatalf("read the first days of %d evals: %v", page, err)
	}
	if _, err := ReadStoredFiguresIn(t.Context(), traced.database.Pool, []string{r.project}, entries, nil, window, &compare); err != nil {
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

// listedValues is each of project's evals' values over window and compare (nil for none) as the
// eval list's read states them from the day rows as of now (api's readEvalValues and
// evalValues.items: FirstDaysIn, BuiltIn, DayWindowIn, SeriesValuesOf and Built.Withhold), and the
// fold of the same rows with no rule of how far they are built (folded), by eval id; and the evals.
func (r *refresherRig) listedValues(t *testing.T, project string, window Range, compare *Range, now time.Time) (stated, folded map[string][]SeriesValues, entries []Entry) {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	page, err := r.registry.Current(ctx, []string{project}, "", 200, 0)
	if err != nil {
		t.Fatalf("read %s's evals: %v", project, err)
	}
	deployRepos, err := DeployReposIn(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	firsts, err := FirstDaysIn(ctx, pool, page.Items, deployRepos)
	if err != nil {
		t.Fatalf("read the first days of %s's evals: %v", project, err)
	}
	built, err := BuiltIn(ctx, pool, page.Items, now)
	if err != nil {
		t.Fatalf("read how far %s's day rows are built: %v", project, err)
	}
	rows, err := DayWindowIn(ctx, pool, []string{project}, page.Items, deployRepos, nil, window, compare)
	if err != nil {
		t.Fatalf("read %s's day rows of %s: %v", project, daysText(window), err)
	}
	byEval := map[string][]WindowRow{}
	for _, row := range rows {
		byEval[row.EvalID] = append(byEval[row.EvalID], row)
	}
	stated, folded = map[string][]SeriesValues{}, map[string][]SeriesValues{}
	for _, entry := range page.Items {
		id := entry.Definition.EvalID
		folded[id] = SeriesValuesOf(entry, slices.Clone(byEval[id]), firsts[id], window, compare)
		stated[id] = built.Withhold(entry.Definition, SeriesValuesOf(entry, slices.Clone(byEval[id]), firsts[id], window, compare), window, compare)
	}
	return stated, folded, page.Items
}

// statedPart is one part of values (window, compare, change, or the days before day of the
// per-day arrays) encoded, series by series, for a comparison of two reads.
func statedPart(t *testing.T, values []SeriesValues, part string, before time.Time) string {
	t.Helper()
	held := map[string]any{}
	for _, one := range values {
		switch part {
		case "window":
			held[one.Series.Series] = one.Window
		case "compare":
			held[one.Series.Series] = one.Compare
		case "change":
			held[one.Series.Series] = one.Change
		case "days":
			var days []DayPoint
			for _, point := range one.Days {
				if point.Day < before.Format(time.DateOnly) {
					days = append(days, point)
				}
			}
			held[one.Series.Series] = days
		}
	}
	encoded, err := json.Marshal(held)
	if err != nil {
		t.Fatalf("encode the %s of the values: %v", part, err)
	}
	return string(encoded)
}

// noValue reports whether every statistic of the part (window, compare or change) of values is
// null: a range that reads "no data".
func noValue(values []SeriesValues, part string) bool {
	for _, one := range values {
		stats := one.Window
		switch part {
		case "compare":
			stats = one.Compare
		case "change":
			stats = one.Change
		}
		for _, value := range stats {
			if value != nil {
				return false
			}
		}
	}
	return true
}

// TestAWindowTheWalkHasNotBuiltReadsNoDataUntilItHas (R12, dispatch://INFRA-2916's k12): over the
// delivery and Legion histories, a first walk of one day a pass stops after its first pass, its
// days built through the day before each family's serving record (eval_days_serving), which is
// days before today. Read as the eval list reads them as of today (listedValues), for every
// computed eval of each family: a window of the built day answers the values it answers once the
// walk has ended; a window of three days that runs past it answers no value, while its built day's
// own values are the settled ones; a compare range does the same against either window, the change
// with no value where either range has none; and once the walk has ended each of those reads
// answers the fold of its rows, a value among them, nothing withheld. Read as of the day of the
// record itself, a window that holds only that day, today, past the built day answers its values.
// Red where a read folds the built days of a window that holds days the walk has not built.
func TestAWindowTheWalkHasNotBuiltReadsNoDataUntilItHas(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, legion := r.seedHistories(t)
	r.leaveInputsUnread(t)
	r.batch = 1
	r.pass(t)
	type read struct {
		name               string
		window             Range
		compare            *Range
		windowed, compared bool
	}
	type family struct {
		walk, project, walked string
		next                  time.Time
		reads                 []read
		before                []map[string][]SeriesValues
		evals                 []Entry
	}
	families := []*family{{walk: walkDelivery, project: deliveries.Project}, {walk: walkLegion, project: legion.Project, walked: legion.Project}}
	for _, one := range families {
		next, _ := r.rebuildNext(t, one.walk, one.walked)
		if next == nil || !next.AddDate(0, 0, 1).Before(refresherToday) {
			t.Fatalf("the %s walk stands at %v after its first pass, and the check needs two days before today past it", one.walk, next)
		}
		if served := r.count(t, `select count(*) from eval_days_serving where family = $1 and project_key = $2 and next_day = $3`, one.walk, one.walked, *next); served != 1 {
			t.Fatalf("the %s rows are served through %d records at their walk's day %s, and the check needs one", one.walk, served, next.Format(time.DateOnly))
		}
		one.next = *next
		inside := Range{From: next.AddDate(0, 0, -1), To: *next}
		past := Range{From: next.AddDate(0, 0, -1), To: next.AddDate(0, 0, 2)}
		one.reads = []read{
			{name: "the built days", window: inside},
			{name: "a window past the built days", window: past, windowed: true},
			{name: "the built days against a compare range past them", window: inside, compare: &past, compared: true},
			{name: "a window past the built days against the built days", window: past, compare: &inside, windowed: true},
		}
		for _, shape := range one.reads {
			stated, _, entries := r.listedValues(t, one.project, shape.window, shape.compare, r.now)
			one.before = append(one.before, stated)
			one.evals = entries
		}
		// Read as of the record's day, the window's last day is today.
		stated, folded, _ := r.listedValues(t, one.project, Range{From: next.AddDate(0, 0, -1), To: next.AddDate(0, 0, 1)}, nil, next.Add(12*time.Hour))
		valued := false
		for _, entry := range one.evals {
			if key, walked := builtWalk(entry.Definition); !walked || key.family != one.walk {
				continue
			}
			id := entry.Definition.EvalID
			if got, want := statedPart(t, stated[id], "window", one.next), statedPart(t, folded[id], "window", one.next); got != want {
				t.Errorf("as of %s, %s's window of the built days and today answers %s, want its rows' fold %s", one.next.Format(time.DateOnly), id, got, want)
			}
			valued = valued || !noValue(stated[id], "window")
		}
		if !valued {
			t.Errorf("as of %s, no %s eval of %s states a value over the built days and today, and the check needs one", one.next.Format(time.DateOnly), one.walk, one.project)
		}
	}

	r.settle(t)
	for _, one := range families {
		checked, valued := 0, false
		for index, shape := range one.reads {
			settled, folded, _ := r.listedValues(t, one.project, shape.window, shape.compare, r.now)
			for _, entry := range one.evals {
				if key, walked := builtWalk(entry.Definition); !walked || key.family != one.walk {
					continue
				}
				checked++
				id := entry.Definition.EvalID
				before, after := one.before[index][id], settled[id]
				what := fmt.Sprintf("%s's %s (%s", id, shape.name, daysText(shape.window))
				if shape.compare != nil {
					what += " against " + daysText(*shape.compare)
				}
				what += ")"
				for _, part := range []string{"window", "compare", "change"} {
					if got, want := statedPart(t, after, part, one.next), statedPart(t, folded[id], part, one.next); got != want {
						t.Errorf("once the walk ended, %s answers the %s %s, want its rows' fold %s", what, part, got, want)
					}
				}
				if !noValue(after, "window") {
					valued = true
				}
				if shape.windowed {
					if !noValue(before, "window") {
						t.Errorf("before the walk built %s, %s answers the window %s, want no value", one.next.Format(time.DateOnly), what, statedPart(t, before, "window", one.next))
					}
				} else if got, want := statedPart(t, before, "window", one.next), statedPart(t, after, "window", one.next); got != want {
					t.Errorf("before the walk built %s, %s answers the window %s, want the settled %s", one.next.Format(time.DateOnly), what, got, want)
				}
				if shape.compare != nil {
					if shape.compared {
						if !noValue(before, "compare") {
							t.Errorf("before the walk built %s, %s answers the compare range %s, want no value", one.next.Format(time.DateOnly), what, statedPart(t, before, "compare", one.next))
						}
					} else if got, want := statedPart(t, before, "compare", one.next), statedPart(t, after, "compare", one.next); got != want {
						t.Errorf("before the walk built %s, %s answers the compare range %s, want the settled %s", one.next.Format(time.DateOnly), what, got, want)
					}
					if (shape.windowed || shape.compared) && !noValue(before, "change") {
						t.Errorf("before the walk built %s, %s answers the change %s, want no value", one.next.Format(time.DateOnly), what, statedPart(t, before, "change", one.next))
					}
				}
				if got, want := statedPart(t, before, "days", one.next), statedPart(t, after, "days", one.next); got != want {
					t.Errorf("before the walk built %s, %s answers the days before it %s, want the settled %s", one.next.Format(time.DateOnly), what, got, want)
				}
			}
		}
		if checked == 0 || !valued {
			t.Fatalf("%d reads of %s's %s evals, a value among them %t, and the check needs a value", checked, one.project, one.walk, valued)
		}
		t.Logf("%s: the first pass built %s's days through %s; %d reads of its %s evals checked", one.walk, one.project,
			one.next.AddDate(0, 0, -1).Format(time.DateOnly), checked, one.walk)
	}
}
