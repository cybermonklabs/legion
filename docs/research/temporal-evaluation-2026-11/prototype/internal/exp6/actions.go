package exp6

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	historypb "go.temporal.io/api/history/v1"

	"github.com/cybermonklabs/legion/docs/research/temporal-evaluation-2026-11/prototype/internal/harness"
)

// metricName is the server's own counter of billable Actions.
const metricName = "action"

// localActivityMarker is the marker name the Go SDK records a local activity under. The
// published rule charges the local activities of one workflow task as a single Action,
// so they are counted by the workflow task that recorded them, not one by one. Markers
// of any other kind -- a version marker, a side effect -- are not Actions.
const localActivityMarker = "LocalActivity"

// Mapped is the Action count read off a set of histories by the published rule, with the
// working shown: what each event type contributed, and how many of each there were.
type Mapped struct {
	// Actions is the total.
	Actions int
	// ByEventType is the Actions each event type contributed, keyed by the event type's
	// EVENT_TYPE_... name.
	ByEventType map[string]int
	// EventsByType is how many events of each type the histories held.
	EventsByType map[string]int
	// Events is every event of every history counted.
	Events int
}

// MapActions counts the Actions a history costs under Temporal Cloud's published list,
// adding to what is already in m.
func (m *Mapped) MapActions(events []*historypb.HistoryEvent) {
	if m.ByEventType == nil {
		m.ByEventType, m.EventsByType = map[string]int{}, map[string]int{}
	}
	// Local activities are charged by the workflow task that recorded them, so the
	// workflow tasks already charged for are remembered rather than each marker.
	chargedTasks := map[int64]bool{}
	for _, event := range events {
		name := harness.EventName(event)
		m.Events++
		m.EventsByType[name]++
		actions := 0
		switch name {
		case "EVENT_TYPE_WORKFLOW_EXECUTION_STARTED":
			actions = 1 // start_workflow
		case "EVENT_TYPE_ACTIVITY_TASK_SCHEDULED":
			actions = 1 // schedule_activity
		case "EVENT_TYPE_ACTIVITY_TASK_STARTED":
			// One retry_activity per attempt beyond the first.
			if attempt := event.GetActivityTaskStartedEventAttributes().GetAttempt(); attempt > 1 {
				actions = int(attempt) - 1
			}
		case "EVENT_TYPE_TIMER_STARTED":
			actions = 1 // start_timer
		case "EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED":
			actions = 1 // signal_workflow
		case "EVENT_TYPE_START_CHILD_WORKFLOW_EXECUTION_INITIATED":
			actions = 2 // the child's start costs two
		case "EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW":
			actions = 1 // one start
		case "EVENT_TYPE_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES":
			actions = 1 // upsert
		case "EVENT_TYPE_MARKER_RECORDED":
			attrs := event.GetMarkerRecordedEventAttributes()
			task := attrs.GetWorkflowTaskCompletedEventId()
			if attrs.GetMarkerName() == localActivityMarker && !chargedTasks[task] {
				chargedTasks[task] = true
				actions = 1 // every local activity of one workflow task, together
			}
		}
		m.ByEventType[name] += actions
		m.Actions += actions
	}
}

// Breakdown is the per-event-type working, as lines a reader can check: the event type,
// how many of them there were, and the Actions they contributed.
func (m *Mapped) Breakdown() []string {
	names := make([]string, 0, len(m.EventsByType))
	for name := range m.EventsByType {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("%s events=%d actions=%d", name, m.EventsByType[name], m.ByEventType[name]))
	}
	return lines
}

// MetricRow is one row of the server's action metric family: the line as the endpoint
// printed it, its labels, and its value.
type MetricRow struct {
	Line   string
	Labels map[string]string
	Value  float64
}

// FetchActionFamily reads the whole action metric family from a Prometheus endpoint.
func FetchActionFamily(ctx context.Context, metricsURL string) ([]MetricRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("metrics url %q: %w", metricsURL, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", metricsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read %s: %s", metricsURL, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", metricsURL, err)
	}

	var rows []MetricRow
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, found := strings.Cut(line, "{")
		if !found {
			// A row of a family with no labels at all.
			name, rest, found = strings.Cut(line, " ")
			if !found || name != metricName {
				continue
			}
			value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return nil, fmt.Errorf("parse %q: %w", line, err)
			}
			rows = append(rows, MetricRow{Line: line, Labels: map[string]string{}, Value: value})
			continue
		}
		if name != metricName {
			continue
		}
		labelText, valueText, found := strings.Cut(rest, "}")
		if !found {
			return nil, fmt.Errorf("parse %q: no closing brace", line)
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(valueText), 64)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", line, err)
		}
		rows = append(rows, MetricRow{Line: line, Labels: parseLabels(labelText), Value: value})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Line < rows[j].Line })
	return rows, nil
}

// parseLabels reads the label list of a Prometheus row: name="value", comma separated.
func parseLabels(text string) map[string]string {
	labels := map[string]string{}
	for _, pair := range strings.Split(text, ",") {
		name, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		labels[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return labels
}

// SumFor is the total of every row of the family whose namespace label is the one given,
// over every action_type, operation and service_name.
func SumFor(rows []MetricRow, namespace string) float64 {
	total := 0.0
	for _, row := range rows {
		if row.Labels["namespace"] == namespace {
			total += row.Value
		}
	}
	return total
}

// AwaitStableActionFamily polls the endpoint until the namespace's total is the same on
// two consecutive reads, because the server emits its metrics asynchronously and a read
// taken too early is short. It returns the family as it stood when the total settled.
func AwaitStableActionFamily(ctx context.Context, metricsURL, namespace string, interval, timeout time.Duration) ([]MetricRow, float64, int, error) {
	deadline := time.Now().Add(timeout)
	previous, reads := -1.0, 0
	for {
		rows, err := FetchActionFamily(ctx, metricsURL)
		if err != nil {
			return nil, 0, reads, err
		}
		reads++
		total := SumFor(rows, namespace)
		if total > 0 && total == previous {
			return rows, total, reads, nil
		}
		previous = total
		if !time.Now().Before(deadline) {
			return rows, total, reads, fmt.Errorf("the action metric for namespace %s never held still over two reads within %s (last: %.0f)", namespace, timeout, total)
		}
		select {
		case <-ctx.Done():
			return nil, 0, reads, ctx.Err()
		case <-time.After(interval):
		}
	}
}
