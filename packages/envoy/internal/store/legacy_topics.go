package store

import (
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The registry's side of the rename that put a project into an issue's, an agent's and a role's
// names (contracts.CanonicalTopic, contracts.CanonicalRole): while every reader takes both forms,
// a name a caller gives in the old form is taken, counted (LegacySubjectCounter) and named back to
// the caller with its new form, and nothing is refused.
//
// A record stores each topic as its caller wrote it. Rewriting a topic into its new form would put
// in the record a topic the plugins that wrote it do not recognise: claude-envoy unsubscribes
// every topic of its record it does not follow (syncRegisteredInterests in
// packages/claude-envoy/src/envoy-channel-server.ts), and pi-envoy reads a role off its record's
// role topic by cutting the lane's prefix (desiredRole in packages/pi-envoy/extensions/envoy.ts).
// So the registry's readers take both forms instead: the interest index matches every form of a
// stored topic (subjectIndex), an unsubscribe removes every form (Remove), a role topic names its
// role in either form (roleOfTopic), and a session's consumer filters on every form
// (bus.SessionConsumers).

// LegacyTopicWarnings names each of topics a registration of sessionID gave in the old form, with
// its new form, "<old> is the old form of <new>; register <new>", which the subscribe route passes
// on in its answer's warnings, and counts each (kind="interest"). A role's lane is named with the
// claim that reaches it instead, "<old> is the old form of <new>; a role's lane delivers nothing to
// an interest: claim <project>.<role> through POST /v1/roles/set": a role is held by a claim, and
// an interest in its lane, in either form, delivers nothing. The session's own agent subject in the
// old form is counted and named in no warning: a plugin registers it itself on every heartbeat,
// and the caller named no such topic.
func (r *Registry) LegacyTopicWarnings(sessionID string, topics []string) []string {
	own := contracts.LegacyAgentSubject(sessionID)
	var warnings []string
	for _, topic := range topics {
		canonical, legacy := oldFormOf(topic)
		if !legacy {
			continue
		}
		r.countLegacy("interest")
		if topic == own {
			continue
		}
		advice := "; register " + canonical
		if role, isRole := strings.CutPrefix(canonical, contracts.RoleTopicPrefix); isRole {
			advice = "; a role's lane delivers nothing to an interest: claim " + role + " through POST /v1/roles/set"
		}
		warnings = append(warnings, topic+" is the old form of "+canonical+advice)
	}
	return warnings
}

// CountStoredLegacyTopics counts each of topics, what sessionID's consumer filter carries of its
// stored interest row, that is in the old form of its name (kind="interest"), each time the row is
// read into the filter: the count moves while any record still holds an old form. The session's
// own agent subject in the old form is not counted there: the listener writes it into every row
// itself (the subscribe route), so counting it would move the counter for as long as the listener
// does that, whatever the session's plugin registers.
func (r *Registry) CountStoredLegacyTopics(sessionID string, topics []string) {
	own := contracts.LegacyAgentSubject(sessionID)
	for _, topic := range topics {
		if _, legacy := oldFormOf(topic); legacy && topic != own {
			r.countLegacy("interest")
		}
	}
}

// oldFormOf is a topic's new form, and whether the topic is in the old form, for the counts above:
// the old form contracts.CanonicalTopic reads, and also an issue-event filter only old-form names
// match though it names no key (contracts.LegacyIssueFilter), which CanonicalTopic reads as its own
// new form. A subscriber on such a filter hears nothing once Dispatch publishes the new form, so
// it is a reader the old form's refusal would stop, and the count must move for it.
func oldFormOf(topic string) (canonical string, legacy bool) {
	if canonical, legacy := contracts.CanonicalTopic(topic); legacy {
		return canonical, true
	}
	return contracts.LegacyIssueFilter(topic)
}

// ClaimedRole reads a role a claim names in either form (contracts.CanonicalRole) and answers the
// role's name, which is what its claim record is keyed by, with a warning for the old form,
// "<old> is the old form of <project>.<name>; claim <project>.<name>", counted (kind="role"). A
// role in the new form answers its name and no warning.
func (r *Registry) ClaimedRole(role string) (name, warning string) {
	project, name, legacy := contracts.CanonicalRole(role)
	if !legacy {
		return name, ""
	}
	r.countLegacy("role")
	current := project + "." + name
	return name, role + " is the old form of " + current + "; claim " + current
}

// ReadRole reads a role that a publish to its lane, a message on the role lane or a holder lookup
// (GET /v1/roles/<role>) names, in either form (contracts.CanonicalRole), and answers the role's
// name, which its claim record is keyed by, with a warning for the old form, "<old> is the old form
// of <project>.<name>; name <project>.<name>", counted (kind="role_read"). A role in the new form
// answers its name and no warning, and an empty name, which names no role, is not counted. It is
// ClaimedRole's count for every other reader of a role name: a publisher that only sends to a
// role, never claims one, is still on the old form, and its sends stop resolving once the old form
// is refused.
func (r *Registry) ReadRole(role string) (name, warning string) {
	project, name, legacy := contracts.CanonicalRole(role)
	if !legacy || name == "" {
		return name, ""
	}
	r.countLegacy("role_read")
	current := project + "." + name
	return name, role + " is the old form of " + current + "; name " + current
}

// countLegacy counts one old-form name of kind, when the registry was opened with a counter.
func (r *Registry) countLegacy(kind string) {
	if r.legacySubjects != nil {
		r.legacySubjects.Inc([2]string{"kind", kind})
	}
}
