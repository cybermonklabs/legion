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

// UnreadableEventLog is the error walkIssueEvents gives for an event of an issue's Dispatch event
// log that it cannot decode. Event is the event's place in the log, counted from 1. Seq is the
// event's Dispatch sequence when the event decoded and its issue payload did not, and 0 when the
// event itself did not decode. Err names the event's place in the log and the cause, and Error
// returns its text.
type UnreadableEventLog struct {
	Event int
	Seq   int64
	Err   error
}

func (e *UnreadableEventLog) Error() string { return e.Err.Error() }
func (e *UnreadableEventLog) Unwrap() error { return e.Err }

// walkIssueEvents gives each issue event of events, an issue's Dispatch event log as
// dispatch.Client.EventsAfter serves it from sequence 0, oldest first, in the log's order, and
// passes over every other event (a comment, a claim, a review). The issue events are the types
// the decoder reads an issue from (isIssueEvent), so a type the decoder starts to read an issue
// from is given here too. An event that does not decode as a Dispatch event, or an issue event
// whose payload is no JSON object (missing, `null`, or not an object) or whose payload does not
// decode, is an *UnreadableEventLog naming its place in the log, and the walk ends there: the live
// decoder refuses such an event too. A loop that stops early leaves the later events undecoded.
func walkIssueEvents(events []json.RawMessage) iter.Seq2[issueEvent, error] {
	return func(yield func(issueEvent, error) bool) {
		for i, raw := range events {
			var event dispatchEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				yield(issueEvent{}, &UnreadableEventLog{Event: i + 1, Err: fmt.Errorf("decode Dispatch event %d of %d: %w", i+1, len(events), err)})
				return
			}
			if !isIssueEvent(event.Type) {
				continue
			}
			if !isJSONObject(event.Payload) {
				yield(issueEvent{}, &UnreadableEventLog{Event: i + 1, Seq: event.Seq,
					Err: fmt.Errorf("decode Dispatch issue payload of event %d of %d: the payload is not a JSON object", i+1, len(events))})
				return
			}
			var issue struct {
				Status string   `json:"status"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(event.Payload, &issue); err != nil {
				yield(issueEvent{}, &UnreadableEventLog{Event: i + 1, Seq: event.Seq,
					Err: fmt.Errorf("decode Dispatch issue payload of event %d of %d: %w", i+1, len(events), err)})
				return
			}
			if !yield(issueEvent{Seq: event.Seq, Type: event.Type, ActorKind: event.Actor.Kind, Status: issue.Status, Labels: issue.Labels}, nil) {
				return
			}
		}
	}
}
