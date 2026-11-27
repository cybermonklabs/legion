package intake

import (
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// LabelSetter is the kind of the Dispatch actor (`user`, `session` or `system`) that added
// dispatch.LegionLabel to an issue, read from its Dispatch event log as dispatch.Client.EventsAfter
// serves it from sequence 0, oldest first. The setter is the actor of the newest issue.created or
// issue.updated event whose labels carry the label (dispatch.CarriesLegionLabel, in any case) while
// the previous such event's did not; an issue.created that carries it counts as adding it. Only
// those two event types move the label state: every other event (a comment, a claim, a review,
// an issue.closed) is passed over and counts for nothing, so a person's comment between a
// session's label and a person's later unrelated edit leaves the session the setter, and a label
// removed and set again names whoever set it last. It is "" when no event added the label. The
// log is read by walkIssueEvents, an issue.closed's payload too: for a log it cannot read,
// LabelSetter returns the walk's *dispatch.EventLogError, whose cause is fixed text, since a log
// the daemon cannot read names no setter it can act on.
//
// The daemon reads the log for this because an unrecorded issue has no earlier state in the
// daemon: the event that reaches it says the label is there, not who put it there (DispatchIssue.
// LabelSetter).
func LabelSetter(events []dispatch.Event) (string, error) {
	setter, labelled := "", false
	for event, err := range walkIssueEvents(events) {
		if err != nil {
			return "", err
		}
		if event.Type != "issue.created" && event.Type != "issue.updated" {
			continue
		}
		now := dispatch.CarriesLegionLabel(event.Labels)
		if now && !labelled {
			setter = event.ActorKind
		}
		labelled = now
	}
	return setter, nil
}
