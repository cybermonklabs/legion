package main

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
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
// exception lane, a wildcard reaching a private stream subject, and a topic that does not begin
// with notifications., which no row's rule drops and the filter cannot take. Each was counted, one
// recipient whose consumer delivered nothing; now each is counted as none. Two rows the judgment
// keeps are the controls, each counted once: the lane of the role the session holds, which shows
// the rule reads the role's claim rather than dropping every lane, and a team topic.
//
// Two rows hold a topic the deployed stream does not capture, the stream narrowed as a hub
// move's quiesce narrows it and read by the listener's client: the filter keeps such a topic
// whatever the stream captures now, so the count counts it, once as the subject itself (the count's
// shortcut for a literal) and once through a wildcard. And one row holds a pair of wildcards each
// of which reaches no other session's subjects alone, but which the filter's reduction merges into
// one that reaches every session's failed deliveries: the filter leaves both out (the fifth kind),
// so the count counts the session for neither. Each row first checks its own want against the
// filter, so a row whose want the filter contradicts tests nothing and fails as UNKNOWN, and a row
// first checks what makes it the case it names: that the stream it narrowed is read as not
// capturing the subject, that the stream it left as deployed captures it, and that the pair merges
// into a subject reaching another session.
func TestTheCountAndTheFilterJudgeAStoredTopicByOneRule(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_judged_topics"
	victimOld := contracts.LegacyAgentSubject("ses_judged_victim")
	const victimNew = "notifications.agent.PRIV.ses_judged_victim"
	const team = "notifications.team.judged-squad"
	exception := bus.ExceptionTopicPrefix + team
	const (
		pairFoo     = "notifications.envoy.exceptions.foo.*"
		pairBar     = "notifications.envoy.exceptions.*.bar"
		pairSubject = "notifications.envoy.exceptions.foo.bar"
		pairMerge   = "notifications.envoy.exceptions.>"
	)
	for _, row := range []struct {
		name     string
		topics   []string
		subject  string
		held     bool
		narrowed bool
		// merge, when set, is the subject the filter's reduction merges topics into.
		merge string
		want  int
	}{
		{"a controller lane whose role the session does not hold", []string{lostLane}, lostLane, false, false, "", 0},
		{"another session's inbox in the old form", []string{victimOld}, victimOld, false, false, "", 0},
		{"another session's inbox in the new form", []string{victimNew}, victimNew, false, false, "", 0},
		{"a failed-delivery exception lane", []string{exception}, exception, false, false, "", 0},
		{"a wildcard reaching a private stream subject", []string{"notifications.>"}, team, false, false, "", 0},
		{"a topic that does not begin with notifications.", []string{"*.team.>"}, team, false, false, "", 0},
		{"a topic the deployed stream does not capture", []string{team}, team, false, true, "", 1},
		{"a wildcard the deployed stream does not capture", []string{"notifications.team.*"}, team, false, true, "", 1},
		{"a pair of wildcards the filter merges into another session's subjects", []string{pairFoo, pairBar}, pairSubject, false, false, pairMerge, 0},
		{"control: the lane of the role the session holds", []string{lostLane}, lostLane, true, false, "", 1},
		{"control: a team topic", []string{team}, team, false, false, "", 1},
	} {
		for _, store := range []string{"interest row", "known-session record"} {
			t.Run(row.name+", in its "+store, func(t *testing.T) {
				url := testnats.URL(t)
				client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithStreamSubjectsRefresh(100*time.Millisecond))
				if err != nil {
					t.Fatalf("connect the listener: %v", err)
				}
				t.Cleanup(client.Close)
				rig := newKnownSessionRigOn(t, client, url)
				waitForRegistry(t, rig.harness.registry)
				if row.held {
					claimRoles(t, rig.harness.registry, sessionID, lostLaneRole)
				}
				if store == "interest row" {
					storeRawRow(t, rig.harness.registry, rig.harness.client, sessionID, row.topics...)
				} else {
					if err := rig.known.RecordWithin(t.Context(), session.KnownSession{SessionID: sessionID, Topics: row.topics}); err != nil {
						t.Fatalf("write %s's known-session record naming %v: %v", sessionID, row.topics, err)
					}
					waitFor(t, "the known-session store to hold "+sessionID+"'s record", func() bool {
						record, ok := rig.known.Get(sessionID)
						return ok && !slices.ContainsFunc(row.topics, func(topic string) bool { return !slices.Contains(record.Topics, topic) })
					})
				}
				if row.narrowed {
					conn := testnats.Connect(t, url)
					t.Cleanup(conn.Close)
					js, err := conn.JetStream()
					if err != nil {
						t.Fatalf("open JetStream: %v", err)
					}
					bustest.NarrowStream(t, js, func(subject string) bool { return subject != "notifications.team.>" })
					bustest.AwaitDeployedCapture(t, client, row.subject, false, 15*time.Second)
					for _, topic := range row.topics {
						if client.DeployedStreamCaptures(topic) {
							t.Fatalf("UNKNOWN: the narrowed stream still captures %s as the listener's client read it", topic)
						}
					}
				} else if row.want == 1 && !client.DeployedStreamCaptures(row.subject) {
					t.Fatalf("UNKNOWN: the deployed stream does not capture %s in a row that does not narrow it", row.subject)
				}
				if row.merge != "" {
					for _, topic := range row.topics {
						if bus.ReachesPrivateStreamSubject(sessionID, topic, nil) {
							t.Fatalf("UNKNOWN: %s reaches a private stream subject by itself, so the row is no merge", topic)
						}
					}
					if got := bus.SessionConsumerFilters(row.topics); !slices.Equal(got, []string{row.merge}) || !bus.ReachesPrivateStreamSubject(sessionID, row.merge, nil) {
						t.Fatalf("UNKNOWN: %v reduce to %v, want %s alone, which reaches another session's subjects", row.topics, got, row.merge)
					}
				}
				filter, err := rig.deps.consumers.storedRowFilter(t.Context(), rig.harness.registry, sessionID, row.topics)
				if err != nil {
					t.Fatalf("build the filter of %s from %v: %v", sessionID, row.topics, err)
				}
				if delivers := slices.ContainsFunc(filter, func(subject string) bool { return bus.SubjectContains(subject, row.subject) }); delivers != (row.want == 1) {
					t.Fatalf("UNKNOWN: the filter built from %v is %v, which delivers %s: %t, against the row's want of %d recipients", row.topics, filter, row.subject, delivers, row.want)
				}
				if recipients := countRecipientsOf(t, rig.deps, row.subject); recipients == nil || *recipients != row.want {
					t.Fatalf("a publish on %s counts %v recipients with %s's %s naming %v, want %d: the filter built from those topics is %v", row.subject, countOrNone(recipients), sessionID, store, row.topics, row.want, filter)
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
