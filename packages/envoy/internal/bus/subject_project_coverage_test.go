package bus

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The census of the families the bus carries, each read for its project by a row of
// contracts.SubjectProject. Every subject the stream carries (StreamCarriedSubjects), and every
// role lane, which it does not (roleLaneSubjects), is named here with the families its
// publishers put under it, each with the publisher that does. A subject the stream gains fails
// until it is named here, and a family named here fails until contracts.SubjectProject has a row
// for it, so a family added to the bus later cannot reach the listener's scope check
// unclassified: SubjectProject would read it as no project, which a reader of a private project's
// message must never be left to.
//
// Negative control, run when this was written: delete the notifications.dispatch.document.> row
// from subjectProjectRows (internal/contracts/subject_project.go) and this test fails naming
// notifications.dispatch.document.>; add "notifications.newfamily.>" to streamSubjects and it
// fails naming that subject.
var subjectProjectCensus = map[string][]string{
	// Each session's own messages: Dispatch's routes, mentions, follower and author copies, and
	// every send, in either form (contracts.AgentSubject, contracts.LegacyAgentSubject, which
	// cmd/listener sendHandler calls).
	"notifications.agent.>": {"notifications.agent.>"},
	// Dispatch's outbox (internal/dispatch/outbox envelope): an issue's, a document's and a
	// project's events; and Dispatch's start (internal/dispatch/startup Subject).
	"notifications.dispatch.>": {
		"notifications.dispatch.issue.>",
		"notifications.dispatch.document.>",
		"notifications.dispatch.project.>",
		"notifications.dispatch.server.>",
	},
	// A repository's webhook events and the CI settlements derived from them.
	"notifications.github.>": {"notifications.github.>"},
	"notifications.slack.>":  {"notifications.slack.>"},
	// The Legion daemon's controller and issue notices, and the listener's wake lane.
	"notifications.legion.>":     {"notifications.legion.>"},
	"notifications.ghostwispr.>": {"notifications.ghostwispr.>"},
	"notifications.whatsapp.>":   {"notifications.whatsapp.>"},
	"notifications.team.>":       {"notifications.team.>"},
	// A failed delivery's report on an agent subject and on a team topic.
	"notifications.envoy.exceptions.notifications.agent.>": {"notifications.envoy.exceptions.notifications.agent.>"},
	"notifications.envoy.exceptions.notifications.team.>":  {"notifications.envoy.exceptions.notifications.team.>"},
	// The listener's own lanes: kept role messages and their cursors, resume floors, and the roles
	// a session took from another.
	KeptTopicPrefix + ">":            {KeptRoleSubjectPrefix + ">", KeptTopicPrefix + "cursor.>"},
	SessionResumeSubjectPrefix + ">": {SessionResumeSubjectPrefix + ">"},
	TakeoverSubjectPrefix + ">":      {TakeoverSubjectPrefix + ">"},
	// The role lanes and their exception twin, which travel over core NATS (roleLaneSubjects).
	contracts.RoleTopicPrefix + ">":                        {contracts.RoleTopicPrefix + ">"},
	ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">": {ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">"},
}

func TestEveryFamilyTheBusCarriesHasASubjectProjectRow(t *testing.T) {
	carried := slices.Concat(StreamCarriedSubjects(), roleLaneSubjects)
	if len(carried) < 14 {
		t.Fatalf("the bus carries %d subjects, want at least 14: a census over a handful covers nothing", len(carried))
	}
	for _, subject := range carried {
		if _, named := subjectProjectCensus[subject]; !named {
			t.Errorf("the bus carries %q, which the census does not name: name the families its publishers put under it", subject)
		}
	}
	for subject := range subjectProjectCensus {
		if !slices.Contains(carried, subject) {
			t.Errorf("the census names %q, which the bus does not carry", subject)
		}
	}
	rows := contracts.SubjectProjectFamilies()
	for subject, families := range subjectProjectCensus {
		for _, family := range families {
			if !SubjectContains(subject, family) {
				t.Errorf("the census puts family %q under %q, which does not contain it", family, subject)
			}
			if !slices.ContainsFunc(rows, func(row string) bool { return SubjectContains(row, family) }) {
				t.Errorf("family %q has no contracts.SubjectProject row, so every name of it reads as no project", family)
			}
		}
	}
}
