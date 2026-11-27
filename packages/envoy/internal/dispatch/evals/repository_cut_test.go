package evals

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legionseed"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// TestARepositorysCutReadsEachSeriesOnEveryDayItsEvalHoldsARowOf: R12 counts a cut's days by the
// eval, "one row per measure per day"
// (dispatch://INFRA-2916/comment/354d442b-ee5c-4d48-8c6a-d876d23be7d7). On a day a repository's cut
// holds a row of an eval, a series of it the day holds no row of, because the repository's part of
// it was empty and is stored as no row (repositoryRows), counts 0 that day, as the project's own
// zero row of it does.
//
// The history is eight Legion trees, one a day, every root of which owns acme/legion, so every
// issue and filing resolves to acme/legion and its cut holds the project's issues on every day.
// legion-splitting's cut of acme/legion then holds, on each of its days, rows of some series and
// none of others: of no filing class but the architect's at design, of no filing in the roots cut,
// of no root without children, and of no filing on the last two days. Once the refresher has
// settled, the eval list's read of acme/legion over the last 7 days and over the live 8 days, which
// the stored figures answer (listValues), answers what the fold of its day rows answers, and of
// legion-splitting:
//
//   - each series the eval's zero day builds (ZeroLegionDay) answers every day of the window the cut
//     holds a row of the eval on, with its zero part's statistics on a day it holds no row of;
//   - hours_without_children, which the cut holds no row of on any day, reads n 0 over the window,
//     not null;
//   - each series the zero day builds no part of (the children cut of the root figures) answers no
//     day and no value.
//
// Each Legion eval's values there are the read of the project's own rows, which hold the same
// issues: the window, and each day of the per-day arrays but today, of which the project holds
// zero rows and the cut no row of the eval, so the cut's arrays leave today out. Red before a read
// of a repository's cut folded those zero parts (cutZeros): each such series answered only the days
// it holds a row of, and hours_without_children read null.
func TestARepositorysCutReadsEachSeriesOnEveryDayItsEvalHoldsARowOf(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx, pool := t.Context(), r.database.Pool
	const repo = "acme/legion"
	history := legiontest.Seed(t, pool, legionseed.Spec{Trees: 8, Now: deliveryseed.DefaultNow, Spacing: 24 * time.Hour})
	project := history.Project
	r.exec(t, "give every Legion root a repository", `update issues set repo = $2 where project_key = $1 and parent_key is null`, project, repo)
	facts, filings, err := legionfacts.ComputeProject(ctx, pool, project, deliveryseed.DefaultNow)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", project, err)
	}
	if _, err := legionfacts.WriteProject(ctx, pool, legionfacts.ProjectWrite{Project: project, Rows: facts, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", project, err)
	}
	if err := legionfacts.WriteFilings(ctx, pool, legionfacts.FilingsWrite{Project: project, Filings: filings, Whole: true}); err != nil {
		t.Fatalf("write the filings of %s: %v", project, err)
	}
	r.settle(t)
	r.expectBuilt(t, "the first whole build", refresherToday)

	// held is the project's listed rows of each Legion eval, by eval, cut ("" or repo) and day
	// (YYYY-MM-DD), and then by series.
	type cutDay struct{ eval, repo, day string }
	held := map[cutDay]map[string]DayRow{}
	for _, row := range r.dayRows(t) {
		if row.ProjectKey != project || !row.Listed || !slices.Contains(legionDaySlugs, strings.TrimPrefix(row.EvalID, project+"/")) {
			continue
		}
		if row.Repo != "" && row.Repo != repo {
			t.Fatalf("%s holds the row %s, and every issue of it resolves to %s", project, rowKey(row), repo)
		}
		at := cutDay{eval: row.EvalID, repo: row.Repo, day: row.Day.Format(time.DateOnly)}
		if held[at] == nil {
			held[at] = map[string]DayRow{}
		}
		held[at][row.Series] = row
	}
	// The cut holds the project's issues: its rows of a day are the project's own rows of the day
	// that count something, value for value.
	for at, rows := range held {
		other := cutDay{eval: at.eval, repo: repo, day: at.day}
		if at.repo == repo {
			other.repo = ""
		}
		for series, row := range rows {
			stored, found := held[other][series]
			if at.repo == repo && !found || at.repo == "" && found == emptyPart(row.DayPart) || found && !sameDayValues(&row, &stored) {
				t.Fatalf("on %s the row %s stands, and the cut %q holds a row of its series %t, of the same values %t: the cut of %s does not hold the project's issues",
					at.day, rowFacts(row), other.repo, found, found && sameDayValues(&row, &stored), repo)
			}
		}
	}

	splitting := EvalID(project, SlugLegionSplitting)
	page, err := r.registry.Current(ctx, []string{project}, "", 200, 0)
	if err != nil {
		t.Fatalf("read %s's evals: %v", project, err)
	}
	index := slices.IndexFunc(page.Items, func(entry Entry) bool { return entry.Definition.EvalID == splitting })
	if index < 0 {
		t.Fatalf("%s holds no eval %s", project, splitting)
	}
	entry := page.Items[index]
	_, headline := listedAndHeadline(entry)
	// zeroOf is each series' statistics on day folded from its zero part alone, the part the
	// project's own zero row of it holds (ZeroLegionDay), as a day of its per-day array folds it;
	// a series of no zero part has none.
	zeroOf := func(day time.Time) map[string]Statistics {
		var parts []measures.DayPart
		for _, row := range ZeroLegionDay(project, day, SlugLegionSplitting).Rows {
			parts = append(parts, row.DayPart)
		}
		return foldDay(entry.Definition, headline, parts, day)
	}
	zeroed := zeroOf(refresherToday)
	var unzeroed []string
	for _, series := range headline {
		if _, found := zeroed[series.Series]; !found {
			unzeroed = append(unzeroed, series.Series)
		}
	}
	first, partial := "", 0
	for at, rows := range held {
		if at.eval != splitting {
			continue
		}
		if at.repo == "" && (first == "" || at.day < first) {
			first = at.day
		}
		for series := range zeroed {
			if _, found := rows[series]; at.repo == repo && !found {
				partial++
			}
		}
	}
	if len(unzeroed) == 0 || partial == 0 {
		t.Fatalf("%s's series of no zero part are %v, and its cut of %s lacks a row of a series of a zero part %d times on the days it holds a row of the eval on: the check needs some of both",
			splitting, unzeroed, repo, partial)
	}
	text := func(value any) string {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %v: %v", value, err)
		}
		return string(encoded)
	}
	decode := func(what, encoded string) []SeriesValues {
		t.Helper()
		var values []SeriesValues
		if err := json.Unmarshal([]byte(encoded), &values); err != nil {
			t.Fatalf("decode the values of %s: %v", what, err)
		}
		return values
	}

	today := refresherToday.Format(time.DateOnly)
	for _, shape := range []struct {
		name   string
		window Range
	}{
		{"the last 7 days", rangeOf(7, refresherToday)},
		{"the live 8 days", rangeOf(8, refresherToday.AddDate(0, 0, 1))},
	} {
		if from := shape.window.From.Format(time.DateOnly); first == "" || first > from {
			t.Fatalf("%s: %s's first day is %q, and the check needs a window that starts on or after it, from %s", shape.name, splitting, first, from)
		}
		if dayIn(refresherToday, shape.window) && (len(held[cutDay{eval: splitting, repo: repo, day: today}]) > 0 || len(held[cutDay{eval: splitting, day: today}]) == 0) {
			t.Fatalf("%s: today the project holds %d rows of %s and the cut of %s %d, and the check needs the project's zero rows and no row of the cut",
				shape.name, len(held[cutDay{eval: splitting, day: today}]), splitting, repo, len(held[cutDay{eval: splitting, repo: repo, day: today}]))
		}
		stated, folded, fromFigures := r.listValues(t, project, []string{repo}, shape.window, nil)
		ownStated, ownFolded, ownFromFigures := r.listValues(t, project, nil, shape.window, nil)
		for _, slug := range legionDaySlugs {
			id := EvalID(project, slug)
			if !fromFigures[id] || !ownFromFigures[id] {
				t.Errorf("%s: the stored figures answer %s of %s %t and of the project's own rows %t, want both", shape.name, id, repo, fromFigures[id], ownFromFigures[id])
			}
			if stated[id] != folded[id] {
				t.Errorf("%s: the read of %s states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
					shape.name, repo, id, len(stated[id]), len(folded[id]), firstDiffering(stated[id], folded[id]))
			}
			if ownStated[id] != ownFolded[id] {
				t.Errorf("%s: the read of the project's own rows states %s's values in %d bytes that differ from the fold of its day rows' %d at byte %d",
					shape.name, id, len(ownStated[id]), len(ownFolded[id]), firstDiffering(ownStated[id], ownFolded[id]))
			}
			cut, own := decode(id+" of "+repo, stated[id]), decode(id, ownStated[id])
			if len(cut) != len(own) {
				t.Fatalf("%s: the read of %s states %d series of %s and the read of the project's own rows %d", shape.name, repo, len(cut), id, len(own))
			}
			for index, values := range cut {
				// The project's own values, of the days the cut holds a row of the eval on alone.
				ownValues := own[index]
				days := []DayPoint{}
				for _, point := range ownValues.Days {
					if len(held[cutDay{eval: id, repo: repo, day: point.Day}]) > 0 {
						days = append(days, point)
					}
				}
				ownValues.Days = days
				if got, want := text(values), text(ownValues); got != want {
					at := firstDiffering(got, want)
					t.Errorf("%s: the read of %s states %s's %s otherwise than the read of the project's own rows over the days the cut holds a row of the eval on: they differ at byte %d, %q, want %q",
						shape.name, repo, id, values.Series.Series, at, got[at:min(at+80, len(got))], want[at:min(at+80, len(want))])
				}
			}
		}

		var measured []string
		for day := shape.window.From; day.Before(shape.window.To); day = day.AddDate(0, 0, 1) {
			if len(held[cutDay{eval: splitting, repo: repo, day: day.Format(time.DateOnly)}]) > 0 {
				measured = append(measured, day.Format(time.DateOnly))
			}
		}
		if len(measured) == 0 {
			t.Fatalf("%s: the cut of %s holds no row of %s, and the check needs days it holds one on", shape.name, repo, splitting)
		}
		zeroDays := 0
		values := decode(splitting+" of "+repo, stated[splitting])
		for _, value := range values {
			series := value.Series.Series
			days := []string{}
			for _, point := range value.Days {
				days = append(days, point.Day)
			}
			if _, zero := zeroed[series]; !zero {
				for statistic, figure := range value.Window {
					if figure != nil {
						t.Errorf("%s: the cut of %s reads %s's %s, which no day builds a part of, as %s %s over the window, want no value", shape.name, repo, splitting, series, statistic, showValue(figure))
					}
				}
				if len(days) != 0 {
					t.Errorf("%s: the cut of %s answers %s's %s, which no day builds a part of, on the days %v, want none", shape.name, repo, splitting, series, days)
				}
				continue
			}
			if !slices.Equal(days, measured) {
				t.Errorf("%s: the cut of %s answers %s's %s on %d days, %v, want each of the %d days it holds a row of the eval on, %v: %v missing",
					shape.name, repo, splitting, series, len(days), days, len(measured), measured, difference(measured, days))
			}
			for _, point := range value.Days {
				if _, found := held[cutDay{eval: splitting, repo: repo, day: point.Day}][series]; found {
					continue
				}
				day, err := time.Parse(time.DateOnly, point.Day)
				if err != nil {
					t.Fatalf("%s: the day %q of %s's %s: %v", shape.name, point.Day, splitting, series, err)
				}
				zeroDays++
				if got, want := text(point.Statistics), text(zeroOf(day)[series]); got != want {
					t.Errorf("%s: on %s, when the cut of %s holds no row of %s's %s, it answers %s, want %s, its zero part's", shape.name, point.Day, repo, splitting, series, got, want)
				}
			}
		}
		at := slices.IndexFunc(values, func(value SeriesValues) bool { return value.Series.Series == metrics.SeriesHoursWithoutChildren })
		if at < 0 {
			t.Fatalf("%s: the read of %s states no %s of %s", shape.name, repo, metrics.SeriesHoursWithoutChildren, splitting)
		}
		if n := values[at].Window["n"]; n == nil || *n != 0 {
			t.Errorf("%s: the cut of %s reads %s's %s n as %s over the window, want 0: it holds rows of the eval on %d of its days and of no root without children",
				shape.name, repo, splitting, metrics.SeriesHoursWithoutChildren, showValue(n), len(measured))
		}
		t.Logf("%s: the cut of %s holds rows of %s on %v, and its read answered %d days of a series it holds no row of as that series' zero part",
			shape.name, repo, splitting, measured, zeroDays)
	}
}
