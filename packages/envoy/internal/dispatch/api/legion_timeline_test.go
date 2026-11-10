package api

import (
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// TestAnIssuesTimelineAddsUpToWhatTheMetricsCountForIt: the timeline route and the metrics route
// read one population and one computation, so an issue's per-segment hours on its timeline are
// the hours the metrics' seven bars hold for it, and they add up to its work time less its queued
// time.
func TestAnIssuesTimelineAddsUpToWhatTheMetricsCountForIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	now := time.Now().UTC().Truncate(time.Minute)
	at := func(hoursAgo float64) time.Time { return now.Add(-time.Duration(hoursAgo * float64(time.Hour))) }
	root := seedLegionIssue(t, database, "INFRA", 1, "A root", "a", nil, "")
	child := seedLegionIssue(t, database, "INFRA", 2, "Its child", "b", nil, root)

	seedLegionRoleEvent(t, database, root, "s-impl", "implementer", root, at(10))
	for _, mark := range []struct {
		field, value string
		hoursAgo     float64
	}{
		{legion.FieldPhase, `"implementing"`, 10},
		{legion.ClaimFieldPrefix + "implementer", `"working"`, 10},
		{legion.ClaimFieldPrefix + "implementer", `"idle"`, 8},
		{legion.FieldPhase, `"reviewing"`, 6},
		{legion.FieldPhase, `"implementing"`, 5},
		{legion.FieldPhase, `"reviewing"`, 3},
		{legion.FieldPhase, `"done"`, 1},
	} {
		seedLegionObservation(t, database, "INFRA", root, mark.field, mark.value, at(mark.hoursAgo))
	}
	closeLegionIssue(t, database, root, at(1))
	seedLegionRoleEvent(t, database, child, "s-child", "implementer", child, at(9))
	seedLegionObservation(t, database, "INFRA", child, legion.FieldPhase, `"implementing"`, at(9))
	seedLegionObservation(t, database, "INFRA", child, legion.FieldPhase, `"done"`, at(7))
	closeLegionIssue(t, database, child, at(7))

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/legion/issues/"+root+"/timeline", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET the timeline: status=%d body=%s", response.Code, response.Body.String())
	}
	timeline := decodeBody[legionTimelineResponse](t, response)

	// The window holds the root's close and not the child's, so the bars are the root's alone.
	query := url.Values{"project": {"INFRA"}, "from": {at(2).Format(time.RFC3339)}, "to": {now.Format(time.RFC3339)}}
	measured := dispatchRequest(t, handler, http.MethodGet, "/api/v1/legion/metrics?"+query.Encode(), nil, "alice")
	if measured.Code != http.StatusOK {
		t.Fatalf("GET the metrics: status=%d body=%s", measured.Code, measured.Body.String())
	}
	report := decodeBody[metrics.Report](t, measured)
	if report.Finished != 1 || report.WorkTime.Issues[0].Key != root {
		t.Fatalf("the metrics window holds %d finished issues, want the root alone", report.Finished)
	}
	var bars metrics.Segments
	for _, bar := range report.PhaseTime.Bars {
		bars.WaitingOnPerson += bar.Segments.WaitingOnPerson
		bars.InATurn += bar.Segments.InATurn
		bars.CIWait += bar.Segments.CIWait
		bars.WaitingOnChildren += bar.Segments.WaitingOnChildren
		bars.Idle += bar.Segments.Idle
		bars.Unknown += bar.Segments.Unknown
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.001 }
	got := timeline.SegmentHours
	for _, pair := range []struct {
		name               string
		timeline, measured float64
	}{
		{"waiting on a person", got.WaitingOnPerson, bars.WaitingOnPerson},
		{"in a turn", got.InATurn, bars.InATurn},
		{"CI wait", got.CIWait, bars.CIWait},
		{"waiting on children", got.WaitingOnChildren, bars.WaitingOnChildren},
		{"idle", got.Idle, bars.Idle},
		{"unknown", got.Unknown, bars.Unknown},
	} {
		if !near(pair.timeline, pair.measured) {
			t.Errorf("%s: the timeline holds %g h, the metrics' bars %g h", pair.name, pair.timeline, pair.measured)
		}
	}
	row := report.WorkTime.Issues[0]
	total := got.WaitingOnPerson + got.InATurn + got.CIWait + got.WaitingOnChildren + got.Idle + got.Unknown
	if !near(total, row.Hours-row.QueuedHours) || !near(timeline.WorkHours, row.Hours) {
		t.Errorf("the timeline's segments add up to %g h and its work time is %g h; the metrics' work time is %g h less %g queued",
			total, timeline.WorkHours, row.Hours, row.QueuedHours)
	}
	var spans float64
	for _, span := range timeline.Segments {
		spans += span.Hours
	}
	if !near(spans, total) {
		t.Errorf("the segment spans add up to %g h, their totals to %g h", spans, total)
	}
	// The same timeline the metrics read: observed, sent back once, two review rounds, a turn,
	// and the child under it with its phases.
	if timeline.Source != metrics.SourceObserved || !timeline.SentBack || timeline.ReviewRounds != 2 {
		t.Errorf("source %s, sent back %v, %d rounds; want observed, true, 2", timeline.Source, timeline.SentBack, timeline.ReviewRounds)
	}
	if len(timeline.Turns) != 1 || timeline.Turns[0].Name != "implementer" || !near(timeline.Turns[0].Hours, 2) {
		t.Errorf("turns %+v, want the implementer's two hours", timeline.Turns)
	}
	if len(timeline.Children) != 1 || timeline.Children[0].Key != child || len(timeline.Children[0].Phases) == 0 {
		t.Errorf("children %+v, want %s with its phases", timeline.Children, child)
	}
	if column := timeline.Columns["reviewing"]; column.ID != "reviewing" || column.Label != "Reviewing" {
		t.Errorf("the reviewing phase is drawn as %+v, want the board's Reviewing column", column)
	}
}
