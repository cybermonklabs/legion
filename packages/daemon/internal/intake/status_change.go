package intake

import "encoding/json"

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
// itself. A log walkIssueEvents cannot read is an error, since a log the daemon cannot read cannot
// say the status stayed.
//
// The walk reads the log from sequence 0 because an event carries the status it leaves, not the
// one it found: no event after seq says what the status was at seq, so only the events at or
// before it tell a rename, which carries the same status, from a change.
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
