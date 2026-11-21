package api

import (
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// TestALegionProjectTheSamplerNeverReadReadsItsWorkEvals: a project that names its Legion daemon,
// which the sampler has never read, holds no observation, and its issues still carry the phase
// work the event log holds. The facts consumer computes it (legionfacts' legionProjects reads the
// setting), and the four Legion evals that read no observation - work time, phase time, rework and
// splitting - read numbers over the days of its history rather than "no data".
func TestALegionProjectTheSamplerNeverReadReadsItsWorkEvals(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	const trees = 4
	fixture := legiontest.Seed(t, database.Pool, legiontest.Spec{Project: "INFRA", Trees: trees, Unobserved: trees})
	if observed := countRows(t, database, `select count(*) from legion_observations where project = 'INFRA'`); observed != 0 {
		t.Fatalf("the history holds %d observations of INFRA, want none, or this test examines a sampled project", observed)
	}
	nameLegionDaemon(t, handler, fixture.Project, "http://legion-infra.internal:13370")
	runLegionFactsConsumer(t, database, fixture.Project, len(fixture.PhaseWork))
	now := time.Now().UTC()
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events), database, now)

	// The window starts on the first day the work-time eval holds a row, so no series of the four
	// reads "no data" for a window that starts before its data.
	first := evalGet[evalListResponse](t, handler, "/api/v1/projects/INFRA/evals?kind=legion&limit=200")
	since := seriesValues(t, findEval(t, first.Items, "INFRA", evals.SlugLegionWorkTime), metrics.SeriesHours).DataSince
	if since == nil {
		t.Fatalf("%s holds no day row of INFRA, which the refresher writes from its stored facts", evals.SlugLegionWorkTime)
	}
	from, err := time.Parse(time.DateOnly, *since)
	if err != nil {
		t.Fatalf("read the first day %q: %v", *since, err)
	}
	window := url.Values{"kind": {evals.KindLegion}, "limit": {"200"}, "from": {from.Format(time.RFC3339)}, "to": {now.Format(time.RFC3339)}}
	list := evalGet[evalListResponse](t, handler, "/api/v1/projects/INFRA/evals?"+window.Encode())
	for _, slug := range []string{evals.SlugLegionWorkTime, evals.SlugLegionPhaseTime, evals.SlugLegionRework, evals.SlugLegionSplitting} {
		item := findEval(t, list.Items, "INFRA", slug)
		read := 0
		for _, series := range item.Series {
			if series.Series.Role != evals.RoleHeadline {
				continue
			}
			if value := series.Window[evals.HeadlineStatistic(item.Definition, series.Series)]; value != nil {
				read++
			}
		}
		if read == 0 {
			t.Errorf("%s reads no headline figure of any of its %d series over %s..%s", slug, len(item.Series), from.Format(time.DateOnly), now.Format(time.RFC3339))
		}
		t.Logf("%s: %d headline series read a figure", slug, read)
	}
}
