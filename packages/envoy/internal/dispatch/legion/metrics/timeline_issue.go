package metrics

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// One issue's Legion timeline (dispatch://INFRA-979's per-issue dig-in): its phases, the
// segments its bars' hours went to, its role turns, CI waits, asks to a person, parked spans and
// its children's phases, each stretch with its hours and where it came from. It is computed by the
// same computation the metrics answer is (Compute), from the same population, so a number on the
// timeline is the number the metrics count for the issue.

// Source names where a stretch came from: the daemon's observed events, or the event log's
// fallback (the status history, handoffs and role events) for an issue the daemon did not observe
// from its first worker event on (Population.Phases).
const (
	SourceObserved      = "observed"
	SourceStatusHistory = "status_history"
	SourceAsks          = "asks"
)

// TimelineSpan is one stretch of the timeline: what it is (a phase, a segment, a role), on which
// bar for a segment, its two ends, its hours and its source.
type TimelineSpan struct {
	Name   string    `json:"name"`
	Bar    string    `json:"bar,omitempty"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Hours  float64   `json:"hours"`
	Source string    `json:"source"`
}

// ChildTimeline is one child of the issue's tree, with its phases.
type ChildTimeline struct {
	Key    string         `json:"key"`
	Title  string         `json:"title"`
	Phases []TimelineSpan `json:"phases"`
	Source string         `json:"source"`
}

// IssueTimeline is GET /api/v1/legion/issues/{key}/timeline's body.
type IssueTimeline struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Root  string `json:"root"`
	// Source is where the phases came from.
	Source    string     `json:"source"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   time.Time  `json:"ended_at"`
	Closed    bool       `json:"closed"`
	// WorkHours, ParkedHours and QueuedHours are the issue's work-time row, as metric 1 reports
	// it; the segments' hours add up to WorkHours less QueuedHours, the bars' share.
	WorkHours    float64         `json:"work_hours"`
	ParkedHours  float64         `json:"parked_hours"`
	QueuedHours  float64         `json:"queued_hours"`
	SegmentHours Segments        `json:"segment_hours"`
	Phases       []TimelineSpan  `json:"phases"`
	Segments     []TimelineSpan  `json:"segments"`
	Turns        []TimelineSpan  `json:"turns"`
	CIWaits      []TimelineSpan  `json:"ci_waits"`
	Asks         []TimelineSpan  `json:"asks"`
	Parked       []TimelineSpan  `json:"parked"`
	Children     []ChildTimeline `json:"children"`
	ReviewRounds int             `json:"review_rounds"`
	SentBack     bool            `json:"sent_back"`
}

// TimelineLookback is how far back the timeline's read reaches for filings and windowed reads;
// the issue's own tree is read whole whatever its age (Read's include).
const TimelineLookback = 84 * 24 * time.Hour

// ReadIssue is the population one issue's timeline reads: the project's population with the
// issue's tree in scope, ending at now.
func ReadIssue(ctx context.Context, pool *store.Pool, github *githubapp.Client, project, key string, now time.Time) (*Population, error) {
	return Read(ctx, pool, github, project, now.Add(-TimelineLookback), now, key)
}

// Timeline is one issue's timeline over population, or false for a key the population does not
// hold.
func Timeline(population *Population, key string) (IssueTimeline, bool) {
	issue := population.Issues[key]
	if issue == nil {
		return IssueTimeline{}, false
	}
	c := &computation{
		population: population, scope: ScopeAll,
		phases: map[string]phaseTimeline{}, views: map[string]*issueView{},
		roots: map[string]string{}, workTimes: map[string]workTimeRow{},
	}
	timeline := c.timelineOf(issue)
	source := SourceStatusHistory
	if timeline.observed {
		source = SourceObserved
	}
	out := IssueTimeline{
		Key: key, Title: issue.Title, Root: c.treeRoot(key), Source: source,
		EndedAt: population.issueEnd(issue), Closed: issue.ClosedAt != nil,
		Phases: phaseSpans(timeline.spans, source), Segments: []TimelineSpan{}, Turns: []TimelineSpan{},
		CIWaits: []TimelineSpan{}, Asks: []TimelineSpan{}, Parked: []TimelineSpan{}, Children: []ChildTimeline{},
		ReviewRounds: reviewRounds(issue, timeline), SentBack: sentBackOnce(issue, timeline),
	}
	row := c.workedOf(issue)
	if row.ok {
		started := row.row.StartedAt
		out.StartedAt = &started
		out.WorkHours, out.ParkedHours, out.QueuedHours = row.row.Hours, row.row.ParkedHours, row.row.QueuedHours
		view := c.viewOf(issue)
		shared := c.barSpansOf(issue)
		for _, bar := range bars {
			spans := intersect(phaseSpansOf(view.timeline, bar.Phases...), shared)
			if len(spans) == 0 {
				continue
			}
			for _, rule := range timelineSegments {
				for _, span := range view.splitSpans(spans)[rule] {
					out.Segments = append(out.Segments, TimelineSpan{
						Name: string(rule), Bar: bar.Role, From: span.From, To: span.To,
						Hours: hours(span.Duration()), Source: segmentSource(rule, source),
					})
				}
			}
			out.SegmentHours.addSegments(view.split(spans))
		}
		out.SegmentHours = rounded(out.SegmentHours)
		slices.SortFunc(out.Segments, func(a, b TimelineSpan) int { return a.From.Compare(b.From) })
		for _, span := range normalize(ciWaitSpans(view)) {
			out.CIWaits = append(out.CIWaits, stretch("ci_wait", span, SourceObserved))
		}
	}
	for _, field := range slices.Sorted(maps.Keys(issue.Observed)) {
		if !strings.HasPrefix(field, legion.ClaimFieldPrefix) {
			continue
		}
		role := strings.TrimPrefix(field, legion.ClaimFieldPrefix)
		working := markSpans(issue.Observed[field], out.EndedAt, func(value string) bool {
			return legion.JSONString([]byte(value)) == "working"
		})
		for _, span := range normalize(working) {
			out.Turns = append(out.Turns, stretch(role, span, SourceObserved))
		}
	}
	for _, span := range issue.AskTurns {
		out.Asks = append(out.Asks, stretch("ask", span, SourceAsks))
	}
	for _, span := range normalize(population.parkedSpans(issue)) {
		out.Parked = append(out.Parked, stretch("parked", span, SourceStatusHistory))
	}
	for _, childKey := range population.Keys {
		child := population.Issues[childKey]
		if child == nil || child.Parent != key {
			continue
		}
		childTimeline := c.timelineOf(child)
		childSource := SourceStatusHistory
		if childTimeline.observed {
			childSource = SourceObserved
		}
		out.Children = append(out.Children, ChildTimeline{
			Key: child.Key, Title: child.Title, Source: childSource,
			Phases: phaseSpans(childTimeline.spans, childSource),
		})
	}
	return out, true
}

// timelineSegments is the precedence's segments and the two that take what no rule claimed.
var timelineSegments = []Segment{
	SegmentWaitingOnPerson, SegmentInATurn, SegmentCIWait, SegmentWaitingOnChildren, SegmentIdle, SegmentUnknown,
}

// segmentSource is where a segment's hours came from: an ask to a person is Dispatch's own ask
// events, an unknown hour is no source at all, and the rest are the daemon's observations laid
// over the issue's phases.
func segmentSource(segment Segment, phases string) string {
	switch segment {
	case SegmentWaitingOnPerson:
		return SourceAsks
	case SegmentUnknown:
		return phases
	default:
		return SourceObserved
	}
}

func phaseSpans(spans []PhaseSpan, source string) []TimelineSpan {
	out := make([]TimelineSpan, 0, len(spans))
	for _, span := range spans {
		out = append(out, stretch(span.Phase, span.Span, source))
	}
	return out
}

func stretch(name string, span Span, source string) TimelineSpan {
	return TimelineSpan{Name: name, From: span.From, To: span.To, Hours: hours(span.Duration()), Source: source}
}
