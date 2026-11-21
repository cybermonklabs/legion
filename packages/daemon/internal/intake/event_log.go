package intake

import (
	"encoding/json"
	"fmt"
	"iter"
)

// issueEvent is what walkIssueEvents gives of one issue event of an issue's Dispatch event log:
// its sequence, its type, the kind of its actor (`user`, `session` or `system`), and the issue's
// status and labels as the event leaves them.
type issueEvent struct {
	Seq       int64
	Type      string
	ActorKind string
	Status    string
	Labels    []string
}

// walkIssueEvents gives each issue event of events, an issue's Dispatch event log as
// dispatch.Client.EventsAfter serves it from sequence 0, oldest first, in the log's order, and
// passes over every other event (a comment, a claim, a review). The issue events are the types
// the decoder reads an issue from (isIssueEvent), so a type the decoder starts to read an issue
// from is given here too. An event that does not decode as a Dispatch event, or an issue event
// with no payload field or with a payload that does not decode, is an error naming its position,
// and the walk ends there: the live decoder refuses such an event too. A loop that stops early
// leaves the later events undecoded.
func walkIssueEvents(events []json.RawMessage) iter.Seq2[issueEvent, error] {
	return func(yield func(issueEvent, error) bool) {
		for i, raw := range events {
			var event dispatchEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				yield(issueEvent{}, fmt.Errorf("decode Dispatch event %d of %d: %w", i+1, len(events), err))
				return
			}
			if !isIssueEvent(event.Type) {
				continue
			}
			var issue struct {
				Status string   `json:"status"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(event.Payload, &issue); err != nil {
				yield(issueEvent{}, fmt.Errorf("decode Dispatch issue payload of event %d of %d: %w", i+1, len(events), err))
				return
			}
			if !yield(issueEvent{Seq: event.Seq, Type: event.Type, ActorKind: event.Actor.Kind, Status: issue.Status, Labels: issue.Labels}, nil) {
				return
			}
		}
	}
}
