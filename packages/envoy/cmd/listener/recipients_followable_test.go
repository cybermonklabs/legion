package main

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// countRecipientsOf is the publish route's count of a message on subject from lostLaneSender.
func countRecipientsOf(t *testing.T, d *listenerDeps, subject string) *int {
	t.Helper()
	recipients, _ := recipientCounts(t.Context(), d, subject, lostLaneSender)
	return recipients
}

// The count and the filter judge a stored topic by one rule. A publish counts a session only when
// the consumer filter built from the same topics, as a relay restart builds it from a stored row
// (sessionDelivery.storedRowFilter), holds a subject the message matches, whichever store names
// the topics: the session's interest row or its known-session record. Each kind of topic that
// judgment drops (storedTopicFiltered) is a row, in each store: a controller notice lane whose
// role the session does not hold, another session's inbox in either form, a failed-delivery
// exception lane, a wildcard reaching a private stream subject, and a topic the deployed stream
// does not store, which no row's rule drops and the filter cannot take. Each was counted, one
// recipient whose consumer delivered nothing; now each is counted as none. Two rows the judgment
// keeps are the controls, each counted once: the lane of the role the session holds, which shows
// the rule reads the role's claim rather than dropping every lane, and a team topic. Each row
// first checks its own want against the filter, so a row whose want the filter contradicts tests
// nothing and fails as UNKNOWN.
func TestTheCountAndTheFilterJudgeAStoredTopicByOneRule(t *testing.T) {
	const sessionID = "ses_judged_topics"
	victimOld := contracts.LegacyAgentSubject("ses_judged_victim")
	const victimNew = "notifications.agent.PRIV.ses_judged_victim"
	const team = "notifications.team.judged-squad"
	exception := bus.ExceptionTopicPrefix + team
	for _, row := range []struct {
		name    string
		topic   string
		subject string
		held    bool
		want    int
	}{
		{"a controller lane whose role the session does not hold", lostLane, lostLane, false, 0},
		{"another session's inbox in the old form", victimOld, victimOld, false, 0},
		{"another session's inbox in the new form", victimNew, victimNew, false, 0},
		{"a failed-delivery exception lane", exception, exception, false, 0},
		{"a wildcard reaching a private stream subject", "notifications.>", team, false, 0},
		{"a topic the deployed stream does not store", "*.team.>", team, false, 0},
		{"control: the lane of the role the session holds", lostLane, lostLane, true, 1},
		{"control: a team topic", team, team, false, 1},
	} {
		for _, store := range []string{"interest row", "known-session record"} {
			t.Run(row.name+", in its "+store, func(t *testing.T) {
				rig := newKnownSessionRig(t)
				waitForRegistry(t, rig.harness.registry)
				if row.held {
					claimRoles(t, rig.harness.registry, sessionID, lostLaneRole)
				}
				if store == "interest row" {
					storeRawRow(t, rig.harness.registry, rig.harness.client, sessionID, row.topic)
				} else {
					if err := rig.known.RecordWithin(t.Context(), session.KnownSession{SessionID: sessionID, Topics: []string{row.topic}}); err != nil {
						t.Fatalf("write %s's known-session record naming %s: %v", sessionID, row.topic, err)
					}
					waitFor(t, 10*time.Second, "the known-session store to hold "+sessionID+"'s record", func() bool {
						record, ok := rig.known.Get(sessionID)
						return ok && slices.Contains(record.Topics, row.topic)
					})
				}
				filter, err := rig.deps.consumers.storedRowFilter(t.Context(), rig.harness.registry, sessionID, []string{row.topic})
				if err != nil {
					t.Fatalf("build the filter of %s from %s: %v", sessionID, row.topic, err)
				}
				if delivers := slices.ContainsFunc(filter, func(subject string) bool { return bus.SubjectContains(subject, row.subject) }); delivers != (row.want == 1) {
					t.Fatalf("UNKNOWN: the filter built from %s is %v, which delivers %s: %t, against the row's want of %d recipients", row.topic, filter, row.subject, delivers, row.want)
				}
				if recipients := countRecipientsOf(t, rig.deps, row.subject); recipients == nil || *recipients != row.want {
					t.Fatalf("a publish on %s counts %v recipients with %s's %s naming %s, want %d: the filter built from that topic is %v", row.subject, countOrNone(recipients), sessionID, store, row.topic, row.want, filter)
				}
			})
		}
	}
}

// countOrNone is *p, or -1 for a count left out, for a failure message.
func countOrNone(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
