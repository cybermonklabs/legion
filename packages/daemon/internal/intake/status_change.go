package intake

import (
	"encoding/json"
	"fmt"
)

// StatusChangedAfter is the sequence of the first issue event after seq, in an issue's Dispatch
// event log as dispatch.Client.EventsAfter serves it from sequence 0, oldest first, that changed
// the issue's status; 0 when no event after seq changed it. The issue events are the three the
// decoder reads an issue from (issue.created, issue.updated and issue.closed), and each carries
// the issue's status as the event leaves it; every other event (a comment, a claim, a review) is
// passed over. The status at seq is the status of the last issue event at or before it, and an
// issue event after seq changes the status when it carries another status than the issue event
// before it. When no issue event stands at or before seq, the first one after it sets the status
// the later ones are compared with, and is no change itself. A seq of 0 or less, a review from a
// read that carries no sequence, cannot be placed, and is 0. An event that does not decode as a
// Dispatch event, or an issue event whose payload does not decode, is an error naming its
// position, since a log the daemon cannot read cannot say the status stayed.
//
// The walk reads the log from sequence 0 because an event carries the status it leaves, not the
// one it found: no event after seq says what the status was at seq, so only the events at or
// before it tell a rename, which carries the same status, from a change.
func StatusChangedAfter(events []json.RawMessage, seq int64) (int64, error) {
	if seq <= 0 {
		return 0, nil
	}
	status, known := "", false
	for i, raw := range events {
		var event dispatchEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return 0, fmt.Errorf("decode Dispatch event %d of %d: %w", i+1, len(events), err)
		}
		if event.Type != "issue.created" && event.Type != "issue.updated" && event.Type != "issue.closed" {
			continue
		}
		var issue struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(event.Payload, &issue); err != nil {
			return 0, fmt.Errorf("decode Dispatch issue payload of event %d of %d: %w", i+1, len(events), err)
		}
		if event.Seq > seq && known && issue.Status != status {
			return event.Seq, nil
		}
		status, known = issue.Status, true
	}
	return 0, nil
}
