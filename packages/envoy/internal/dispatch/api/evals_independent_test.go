package api

import (
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/evals/independent"
)

// legionIssueFloor is the least number of issues k6's comparison proves anything over, across its
// two windows: below it the history is too small to reach the shapes the rules differ on.
const legionIssueFloor = 200

// TestTheLegionEvalsEqualTheIndependentReading is k6 over the Go API (R6, D3, A4): over the
// history `evalfixture -legion` loads, whose observations start partway through, the six Legion
// evals of GET /api/v1/projects/{key}/evals?kind=legion, each series in its three cuts and each
// statistic, equal the independent reading (package independent, written from the words of R6, D3
// and INFRA-1077's rules) for a window that starts before the project's first observation and one
// that starts after it: hours to 0.1, counts exactly, shares to 0.001, and "no data" on both sides
// alike. Idle time and slot wait read "no data" over the first window on both sides and hold
// values over the second. The negative control is the reading's window shifted 7 days, which the
// comparison must find differing.
func TestTheLegionEvalsEqualTheIndependentReading(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := t.Context()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	loaded, err := evalfixture.WriteLegion(ctx, database.Pool, "LEGK", evalfixture.DefaultIssues, now)
	if err != nil {
		t.Fatalf("load the Legion history: %v", err)
	}
	registry := evals.New(database, deps.Docs, deps.Events)
	writeLegionEvals(t, database, registry, loaded.Project, loaded.From, now)
	if _, err := registry.Ensure(ctx, loaded.Project); err != nil {
		t.Fatalf("seed the evals of %s: %v", loaded.Project, err)
	}
	t.Logf("%s: %d issues, %d finished, %d trees (%d unobserved), first observation %s, first finished %s",
		loaded.Project, loaded.Issues, loaded.Finished, loaded.Trees, loaded.Unobserved,
		loaded.FirstObserved.Format(time.RFC3339), loaded.FirstFinished.Format(time.RFC3339))

	read := func(window evalfixture.Window) (evalListResponse, independent.Reading) {
		t.Helper()
		query := url.Values{"kind": {evals.KindLegion}, "limit": {"200"},
			"from": {window.From.Format(time.RFC3339)}, "to": {window.To.Format(time.RFC3339)}}
		route := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+loaded.Project+"/evals?"+query.Encode())
		reading, err := independent.Read(ctx, database.Pool, loaded.Project, independent.Window{From: window.From, To: window.To})
		if err != nil {
			t.Fatalf("read %s independently over %s..%s: %v", loaded.Project, window.From, window.To, err)
		}
		return route, reading
	}

	issues, values := 0, 0
	parked, classes := map[string]float64{}, map[string]int{}
	for _, side := range []struct {
		name     string
		window   evalfixture.Window
		observed bool
	}{
		{"before the first observation", loaded.Windows.Before, false},
		{"after the first observation", loaded.Windows.After, true},
	} {
		route, reading := read(side.window)
		where := fmt.Sprintf("%s..%s (%s)", side.window.From.Format(time.DateOnly), side.window.To.Format(time.DateOnly), side.name)
		compared, measures, differences := compareLegionEvals(route, reading)
		for _, difference := range differences {
			t.Errorf("%s: %s", where, difference)
		}
		if measures != len(independent.Slugs) {
			t.Errorf("%s: the route answers %d Legion evals, want the %d measures", where, measures, len(independent.Slugs))
		}
		for _, slug := range []string{independent.SlugIdleTime, independent.SlugSlotWait} {
			held := heldFigures(t, route, slug)
			measure, _ := reading.Measure(slug)
			switch {
			case !side.observed && (held > 0 || !measure.NoData):
				t.Errorf("%s: %s holds %d figures on the route and reads no data %v independently, want no data on both sides", where, slug, held, measure.NoData)
			case side.observed && (held == 0 || measure.NoData):
				t.Errorf("%s: %s holds %d figures on the route and reads no data %v independently, want figures on both sides", where, slug, held, measure.NoData)
			}
		}
		issues += reading.Issues
		values += compared
		for status, hours := range reading.ParkedHours {
			parked[status] += hours
		}
		for class, filed := range reading.FilingsByClass {
			classes[class] += filed
		}
		t.Logf("%s: %d finished issues, %d filings %v, parked hours %v, %d values of %d measures compared, %d differences",
			where, reading.Issues, reading.Filings, reading.FilingsByClass, reading.ParkedHours, compared, measures, len(differences))
	}
	if issues < legionIssueFloor {
		t.Errorf("the two windows hold %d finished issues, want at least %d", issues, legionIssueFloor)
	}
	// The history holds what the rules differ on beyond phases: hours parked in each parked status
	// (which the work time leaves out), stored merge times, and filings of every class.
	var merged int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from issue_facts f join issues i on i.key = f.issue_key
		where i.project_key = $1 and f.merged_at is not null`, loaded.Project).Scan(&merged); err != nil {
		t.Fatalf("count the stored merge times: %v", err)
	}
	if parked["backlog"] == 0 || parked["icebox"] == 0 || merged == 0 || len(classes) != 4 || slices.Contains(slices.Collect(maps.Values(classes)), 0) {
		t.Errorf("the windows' finished issues sat %v hours parked, %d hold a stored merge time, and their filings by class are %v: want hours in both parked statuses, merge times and every class",
			parked, merged, classes)
	}
	t.Logf("compared %d values over %d finished issues in two windows; parked hours %.1f (backlog %.1f, icebox %.1f), %d stored merge times, filings by class %v",
		values, issues, parked["backlog"]+parked["icebox"], parked["backlog"], parked["icebox"], merged, classes)

	// The negative control: the reading's window shifted by 7 days and the route's not. A
	// comparison that still finds every value equal compares nothing that moves with the window.
	for _, window := range []evalfixture.Window{loaded.Windows.Before, loaded.Windows.After} {
		route, _ := read(window)
		shifted := evalfixture.Window{From: window.From.AddDate(0, 0, 7), To: window.To.AddDate(0, 0, 7)}
		_, reading := read(shifted)
		compared, _, differences := compareLegionEvals(route, reading)
		if len(differences) == 0 {
			t.Errorf("the reading over %s..%s equals the route over %s..%s in all %d values, want the shift to move them",
				shifted.From.Format(time.DateOnly), shifted.To.Format(time.DateOnly), window.From.Format(time.DateOnly), window.To.Format(time.DateOnly), compared)
			continue
		}
		t.Logf("negative control: the reading shifted 7 days from %s differs from the route in %d of %d values, the first: %s",
			window.From.Format(time.DateOnly), len(differences), compared, differences[0])
	}
}

// compareLegionEvals holds every Legion eval of route (a list read of one project) to reading,
// series by series and statistic by statistic, and answers how many values it compared, how many
// Legion evals the route answered, and each difference. A series or a statistic one side states
// and the other does not is a difference, and so is a first day the two sides name differently
// (independent.Measure.SeriesSince: a series no day holds input of names none).
func compareLegionEvals(route evalListResponse, reading independent.Reading) (int, int, []string) {
	compared, measures := 0, 0
	var differences []string
	differ := func(format string, args ...any) { differences = append(differences, fmt.Sprintf(format, args...)) }
	for _, item := range route.Items {
		if item.Definition.Kind != evals.KindLegion {
			continue
		}
		measures++
		slug := item.Definition.Slug
		measure, held := reading.Measure(slug)
		if !held {
			differ("%s: the reading states no such measure", slug)
			continue
		}
		stated := 0
		for _, cut := range measure.Cuts {
			stated += len(cut.Series)
		}
		if stated != len(item.Series) {
			differ("%s: the route states %d series and the reading %d", slug, len(item.Series), stated)
		}
		for _, series := range item.Series {
			name := series.Series.Series
			mine, held := measure.Series(name)
			if !held {
				differ("%s %s: the reading states no such series", slug, name)
				continue
			}
			compared++
			if since := measure.SeriesSince(name); showDay(series.DataSince) != showDay(since) {
				differ("%s %s: data since %s on the route, %s in the reading", slug, name, showDay(series.DataSince), showDay(since))
			}
			names := statNames(series.Window)
			if mineNames := statNames(mine); strings.Join(names, ",") != strings.Join(mineNames, ",") {
				differ("%s %s: the route states %v and the reading %v", slug, name, names, mineNames)
			}
			for _, stat := range names {
				compared++
				got, want := series.Window[stat], mine[stat]
				if (got == nil) != (want == nil) || (got != nil && math.Abs(*got-*want) > legionTolerance(series.Series.Unit, stat)) {
					differ("%s %s.%s: %s on the route, %s in the reading", slug, name, stat, showFigure(got), showFigure(want))
				}
			}
		}
	}
	return compared, measures, differences
}

// legionTolerance is how far two readings of one statistic may stand apart: a count exactly, a
// share to a thousandth, an hour figure to the tenth of an hour R6's check holds the two sides
// to, and a figure of rounds (a median of counts) to the four decimals both sides round to.
func legionTolerance(unit, stat string) float64 {
	switch {
	case stat == "n" || stat == "issues" || stat == "sent_back" || stat == "children":
		return 0
	case strings.HasSuffix(stat, "share") || stat == "children_per_root":
		return 1e-3
	case unit == "hours":
		return 0.1
	}
	return 1e-4
}

// heldFigures is how many statistics of slug's series the route answers a value of.
func heldFigures(t *testing.T, route evalListResponse, slug string) int {
	t.Helper()
	held := 0
	for _, item := range route.Items {
		if item.Definition.Slug != slug {
			continue
		}
		for _, series := range item.Series {
			for _, value := range series.Window {
				if value != nil {
					held++
				}
			}
		}
		return held
	}
	t.Fatalf("the route answers no %s", slug)
	return 0
}

// statNames is the statistic names of stats, in order: of the route's (evals.Statistics) or of
// the reading's (independent.Statistics).
func statNames[S ~map[string]*float64](stats S) []string {
	return slices.Sorted(maps.Keys(stats))
}

// showDay is a first day for a failure message.
func showDay(day *string) string {
	if day == nil {
		return "none"
	}
	return *day
}
