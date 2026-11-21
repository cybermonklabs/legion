package model

import "github.com/sjawhar/envoy/internal/contracts"

// The keys Dispatch records a destination of an event by: what events.published_destinations,
// events.owed_keys and comment_fallback_copies.destination hold, what the outbox's chains are kept
// on, and what every reader of those columns matches (MentionCopyHolderSQL,
// internal/dispatch/sessionanswers, internal/dispatch/graphcensus). A key names its destination
// and no project: it is the same bytes every release of Dispatch has written, so a release reads
// another's records while the two roll side by side and a reached destination stays reached
// whichever wrote it. They are record keys, not bus subjects: the outbox publishes on the subject
// that names the event's project (outbox.busSubject), which no record holds.

// IssueDestinationKey is the record key of an issue's topic family for an event of type typ:
// notifications.dispatch.issue.<key>.<type>.
func IssueDestinationKey(key, typ string) string {
	return contracts.DispatchIssueTopicPrefix + key + "." + typ
}

// SessionDestinationKey is the record key of one session's own destination:
// notifications.agent.<session>.
func SessionDestinationKey(session string) string {
	return contracts.AgentTopicPrefix + session
}

// RoleDestinationKey is the record key of a role route: notifications.role.<role>.
func RoleDestinationKey(role string) string {
	return contracts.RoleTopicPrefix + role
}
