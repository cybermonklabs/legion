package intake

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

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
	// DaemonActor is true when the daemon made the event, by the rule the decoder applies to the
	// same event (DispatchIssue.DaemonActor): the actor is the session of the event's issue that
	// dispatch.DaemonSession names. The set-back's check passes over the daemon's own changes.
	DaemonActor bool
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
	change := StatusChange{Seq: event.Seq, Actor: dispatch.Actor(event.Actor), DaemonActor: event.daemonActor(), Changed: event.Type == "issue.closed"}
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

// NeedsEventLog says whether the fact's preparation reads the issue's Dispatch event log: a
// person's approval that may queue its parked root (MayQueueHandover) and that carries a sequence
// to place it by in that log (Mark.Seq). A review from a read that carries no sequence cannot be
// placed, and reads no log. The daemon's preparation reads the log for such a fact and sets what it
// shows (WithStatusChange), so this method is the one rule for which approvals read the log.
func (a DispatchArtifact) NeedsEventLog() bool {
	return a.MayQueueHandover() && a.Mark.Seq > 0
}

// WithStatusChange is the fact with StatusChangedSeq set from events, the issue's Dispatch event
// log as dispatch.Client.EventsAfter serves it from sequence 0, oldest first: the sequence of the
// first issue event after the approval (Mark.Seq) that changed the issue's status, 0 when none did
// (statusChangedAfter). It is for a fact that needs the log (NeedsEventLog), whose Mark.Seq places
// the approval in it. A log the daemon cannot decode is an error, with the fact unchanged.
func (a DispatchArtifact) WithStatusChange(events []json.RawMessage) (DispatchArtifact, error) {
	changed, err := statusChangedAfter(events, a.Mark.Seq)
	if err != nil {
		return a, err
	}
	a.StatusChangedSeq = changed
	return a, nil
}

// statusChangedAfter is the sequence of the first issue event after seq, in an issue's Dispatch
// event log as dispatch.Client.EventsAfter serves it from sequence 0, oldest first, that changed
// the issue's status; 0 when no event after seq changed it. The issue events are the ones
// walkIssueEvents gives, each carrying the issue's status as the event leaves it; every other
// event (a comment, a claim, a review) is passed over. The status at seq is the status of the
// last issue event at or before it, and an issue event after seq changes the status when it
// carries another status than the issue event before it. When no issue event stands at or before
// seq, the first one after it sets the status the later ones are compared with, and is no change
// itself. For a log walkIssueEvents cannot read, it returns the walk's *dispatch.EventLogError,
// since a log the daemon cannot read cannot say the status stayed.
//
// The walk reads the log from sequence 0 because an event carries the status it leaves, not the
// one it found: no event after seq says what the status was at seq, and the walk tells a change
// by comparing each issue event's status with the issue event before it.
func statusChangedAfter(events []json.RawMessage, seq int64) (int64, error) {
	status, known := "", false
	for event, err := range walkIssueEvents(events) {
		if err != nil {
			return 0, err
		}
		if event.Seq > seq && known && event.Status != status {
			return event.Seq, nil
		}
		status, known = event.Status, true
	}
	return 0, nil
}

// StatusTransition is the change of an issue's Dispatch status that left the issue at the status a
// view or a listing of it shows (StatusTransitionAt): who wrote that status, and over what.
type StatusTransition struct {
	// Seq is the sequence of the issue event that made the change. It is 0 where Dispatch's log
	// proves no writer of the status (StatusTransitionAt says when), or where nothing read the log.
	Seq int64
	// StatusBefore is the status the issue event before it left, "" where none stands before it,
	// as for an issue created at its status.
	StatusBefore string
	// Session is the session that made the change, by the decoder's rule (dispatchEvent.actorSession):
	// "" for a person, Dispatch itself and the daemon's own session, which writes the todo of
	// release_children and rerun_child.
	Session string
	// At is when Dispatch recorded the change, zero for an event that carries no time.
	At time.Time
}

// StatusTransitionAt is the transition that left an issue at status as Dispatch showed it at seq,
// read from events, the issue's Dispatch event log as dispatch.Client.EventsAfter serves it from
// sequence 0, oldest first: the newest issue event at or below seq whose status differs from the
// issue event before it, or the first issue event, which gives the issue its first status. That
// is the rule statusChangedAfter reads a change by (walkIssueEvents), so every other event (a
// comment, a claim, a document version) and an issue event that leaves the status as it was (a
// rename, a re-rank, a label, a link, a write of the status already shown) names no writer. The
// walk stops at the first issue event past seq, so a later event does not move it.
//
// The log proves no writer, and the transition's Seq is 0, where no issue event stands at or below
// seq, where the newest one leaves the issue at another status than status, or where the event of
// the transition names no actor kind. For a log walkIssueEvents cannot read up to there, it returns
// the walk's *dispatch.EventLogError, since such a log cannot say who wrote the status.
func StatusTransitionAt(events []json.RawMessage, seq int64, status string) (StatusTransition, error) {
	var transition StatusTransition
	var kind, last string
	known := false
	for event, err := range walkIssueEvents(events) {
		if err != nil {
			return StatusTransition{}, err
		}
		if event.Seq > seq {
			break
		}
		if !known || event.Status != last {
			transition = StatusTransition{Seq: event.Seq, StatusBefore: last, Session: event.Session, At: event.At}
			kind = event.ActorKind
		}
		last, known = event.Status, true
	}
	if !known || last != status || kind == "" {
		return StatusTransition{}, nil
	}
	return transition, nil
}
