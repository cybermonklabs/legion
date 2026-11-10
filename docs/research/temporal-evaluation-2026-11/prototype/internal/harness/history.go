package harness

import (
	"context"
	"fmt"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
)

// History reads a run's whole history from the server, every event of it. The
// experiments count their own cost in history events, so they read the history the
// server kept rather than what they believe they did.
func History(ctx context.Context, c client.Client, workflowID, runID string) ([]*historypb.HistoryEvent, error) {
	var events []*historypb.HistoryEvent
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("read history of %s: %w", workflowID, err)
		}
		events = append(events, event)
	}
	return events, nil
}

// EventName is a history event's type under the name the protobuf enum gives it, the
// EVENT_TYPE_... form Temporal's published list of billable Actions is written in. The
// enum's own String method prints a shorthand instead, which that list does not use.
func EventName(event *historypb.HistoryEvent) string {
	kind := event.GetEventType()
	if name, ok := enumspb.EventType_name[int32(kind)]; ok {
		return name
	}
	return fmt.Sprintf("EVENT_TYPE_UNKNOWN(%d)", int32(kind))
}
