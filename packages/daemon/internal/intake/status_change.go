package intake

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// StatusChange is what one event of an issue's Dispatch event log says about the issue's status:
// the event's sequence, whether the event changed the status, and who made the event.
type StatusChange struct {
	// Seq is the event's place in its issue's event log.
	Seq int64
	// Changed is true for an issue.closed, and for an issue.updated whose payload.changed holds
	// "status". Every other event is false: a comment, a message, a document version, a claim, and
	// an issue.updated that moved other fields. Dispatch leaves changed out of an event whose write
	// moved no field (its IssueEventPayload carries the list with omitzero), so a write of the
	// status the issue already shows is false too.
	Changed bool
	// Actor is who Dispatch names as the author of the event.
	Actor dispatch.Actor
}

// StatusChangeOf reads one event of an issue's Dispatch event log, as dispatch.Client.EventsAfter
// serves it. The outbox reads the events after a set-back's sequence with it, so that the set-back
// never writes over a later status change. Only a status change counts for that check: a comment,
// a message or the daemon's own reason post moves the sequence and changes no status. An event
// that does not decode as a Dispatch event, or an issue.updated whose payload does not decode, is
// an error, because a log the daemon cannot read tells it nothing it can act on.
func StatusChangeOf(raw json.RawMessage) (StatusChange, error) {
	var event dispatchEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return StatusChange{}, fmt.Errorf("decode Dispatch event: %w", err)
	}
	change := StatusChange{Seq: event.Seq, Actor: dispatch.Actor(event.Actor), Changed: event.Type == "issue.closed"}
	if event.Type != "issue.updated" {
		return change, nil
	}
	var issue struct {
		Changed []string `json:"changed"`
	}
	if len(event.Payload) > 0 {
		if err := json.Unmarshal(event.Payload, &issue); err != nil {
			return StatusChange{}, fmt.Errorf("decode the issue payload of Dispatch event %d: %w", event.Seq, err)
		}
	}
	change.Changed = slices.Contains(issue.Changed, "status")
	return change, nil
}
