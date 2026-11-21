package intake

import (
	"encoding/json"
	"errors"
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

// UnreadableEventLog is the error walkIssueEvents gives for an event of an issue's Dispatch event
// log that it cannot read. Event is the event's position in the log, counted from 1, of Events in
// all. Payload is set when the event decoded as a Dispatch event and its issue payload did not;
// Seq is the event's sequence then, and 0 when the event itself did not decode. Err is the cause.
type UnreadableEventLog struct {
	Event   int
	Events  int
	Seq     int64
	Payload bool
	Err     error
}

func (e *UnreadableEventLog) Error() string {
	if e.Payload {
		return fmt.Sprintf("decode Dispatch issue payload of event %d of %d: %v", e.Event, e.Events, e.Err)
	}
	return fmt.Sprintf("decode Dispatch event %d of %d: %v", e.Event, e.Events, e.Err)
}

func (e *UnreadableEventLog) Unwrap() error { return e.Err }

// errPayloadNotObject is the cause of an UnreadableEventLog for an issue event whose payload is no
// JSON object.
var errPayloadNotObject = errors.New("the payload is not a JSON object")

// walkIssueEvents gives each issue event of events, an issue's Dispatch event log as
// dispatch.Client.EventsAfter serves it from sequence 0, oldest first, in the log's order, and
// passes over every other event (a comment, a claim, a review). The issue events are the types
// the decoder reads an issue from (isIssueEvent), so a type the decoder starts to read an issue
// from is given here too. An event that does not decode as a Dispatch event, or an issue event
// whose payload is no JSON object (missing, `null`, or not an object) or whose payload does not
// decode, is an *UnreadableEventLog naming its position, and the walk ends there: the live decoder
// refuses such an event too. A loop that stops early leaves the later events undecoded.
func walkIssueEvents(events []json.RawMessage) iter.Seq2[issueEvent, error] {
	return func(yield func(issueEvent, error) bool) {
		for i, raw := range events {
			var event dispatchEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				yield(issueEvent{}, &UnreadableEventLog{Event: i + 1, Events: len(events), Err: err})
				return
			}
			if !isIssueEvent(event.Type) {
				continue
			}
			if !isJSONObject(event.Payload) {
				yield(issueEvent{}, &UnreadableEventLog{Event: i + 1, Events: len(events), Seq: event.Seq, Payload: true, Err: errPayloadNotObject})
				return
			}
			var issue struct {
				Status string   `json:"status"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(event.Payload, &issue); err != nil {
				yield(issueEvent{}, &UnreadableEventLog{Event: i + 1, Events: len(events), Seq: event.Seq, Payload: true, Err: err})
				return
			}
			if !yield(issueEvent{Seq: event.Seq, Type: event.Type, ActorKind: event.Actor.Kind, Status: issue.Status, Labels: issue.Labels}, nil) {
				return
			}
		}
	}
}
