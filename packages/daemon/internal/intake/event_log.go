package intake

import (
	"encoding/json"
	"fmt"
	"iter"

	"github.com/sjawhar/envoy/oauth2call"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
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
// from is given here too. An event that does not decode as a Dispatch event is refused by the read
// itself (EventsAfter's *dispatch.EventLogError). An issue event whose payload is no JSON object
// (missing, `null`, or not an object) or whose payload does not decode is a
// *dispatch.EventLogError naming its place in the log and its cause in fixed text (a decode's
// through oauth2call.DecodeError, which quotes no value), and the walk ends there: the live decoder
// refuses such an event too. A loop that stops early leaves the later events undecoded.
func walkIssueEvents(events []dispatch.Event) iter.Seq2[issueEvent, error] {
	return func(yield func(issueEvent, error) bool) {
		for i, event := range events {
			if !isIssueEvent(event.Type) {
				continue
			}
			if !isJSONObject(event.Payload) {
				yield(issueEvent{}, &dispatch.EventLogError{Event: i + 1, Seq: event.Seq,
					Err: fmt.Errorf("decode Dispatch issue payload of event %d of %d: the payload is not a JSON object", i+1, len(events))})
				return
			}
			var issue struct {
				Status string   `json:"status"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(event.Payload, &issue); err != nil {
				yield(issueEvent{}, &dispatch.EventLogError{Event: i + 1, Seq: event.Seq,
					Err: fmt.Errorf("decode Dispatch issue payload of event %d of %d: %w", i+1, len(events), oauth2call.DecodeError(err, event.Payload, &issue))})
				return
			}
			if !yield(issueEvent{Seq: event.Seq, Type: event.Type, ActorKind: event.Actor.Kind, Status: issue.Status, Labels: issue.Labels}, nil) {
				return
			}
		}
	}
}
