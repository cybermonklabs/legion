package main

import (
	"errors"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// countRecipientsOf is the publish route's count of a message on subject from lostLaneSender, on
// the transport the route's send decides for it (bus.Client.PublishTakesJetStream).
func countRecipientsOf(t *testing.T, d *listenerDeps, subject string) *int {
	t.Helper()
	recipients, _ := recipientCounts(t.Context(), d, subject, lostLaneSender, d.client.PublishTakesJetStream(t.Context(), subject))
	return recipients
}

// The count and the filter judge a stored topic by one rule. A publish counts a session only when
// the consumer filter built from the same topics, as a relay restart builds it from a stored row
// (storedRowFilter), holds a subject the message matches, whichever store names the topics: the
// session's interest row or its known-session record. Each kind of topic that judgment drops
// (storedTopicFiltered) is a row, in each store: a controller notice lane whose role the session
// does not hold, another session's inbox in either form, a failed-delivery exception lane, a
// wildcard reaching a private stream subject, and a topic outside bus.NotificationTopicPrefix,
// which no row's rule drops and the filter cannot take (filterTakes, bus.CanFilterOn). Each was
// counted, one recipient whose consumer delivered nothing; now each is counted as none. Two rows
// the judgment keeps are the controls, each counted once: the lane of the role the session holds,
// which shows the rule reads the role's claim rather than dropping every lane, and a team topic.
// Each row first checks its own want against the filter, so a row whose want the filter
// contradicts tests nothing and fails as UNKNOWN.
func TestTheCountAndTheFilterJudgeAStoredTopicByOneRule(t *testing.T) {
	t.Parallel()
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
		{"a non-notification topic, which the filter cannot take", "*.team.>", team, false, 0},
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
				filter, err := storedRowFilter(t.Context(), rig.harness.registry, sessionID, []string{row.topic})
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

// A publish on a topic no stream subject captures, which the send takes over core NATS, leaves
// both counts out. The consumer filter built from a stored row holds a topic no stream subject
// captures yet (filterTakes), so a session whose interest row and known-session record follow
// such a topic has a consumer filtering on it, and a count that judged the session's topics by
// that filter alone counted it: recipients 1 for a message the send takes over core NATS, which no
// stream the client has read stores. The publish route refuses such a topic once a read of the
// deployed subjects that started after the request asked agrees (bus.SubjectsAsk.Uncaptured), and
// counts it only where no such read could be made, so whether a stream stores the message is
// unknown; the row asks the count itself (recipientCounts). The row first checks that the
// listener's client judges the topic uncaptured, that the filter built from the row holds it and
// that the send takes core NATS for it, and fails as UNKNOWN otherwise.
func TestAPublishOnATopicNoStreamSubjectCapturesLeavesBothCountsOut(t *testing.T) {
	t.Parallel()
	const (
		sessionID = "ses_uncaptured_follower"
		topic     = "notifications.lxspec.probe.counted"
	)
	rig := newKnownSessionRig(t)
	waitForRegistry(t, rig.harness.registry)
	if rig.deps.client.DeployedStreamCaptures(topic) {
		t.Fatalf("UNKNOWN: the deployed stream captures %s; the row needs a topic no stream subject captures", topic)
	}
	storeRawRow(t, rig.harness.registry, rig.harness.client, sessionID, topic)
	if err := rig.known.RecordWithin(t.Context(), session.KnownSession{SessionID: sessionID, Topics: []string{topic}}); err != nil {
		t.Fatalf("write %s's known-session record naming %s: %v", sessionID, topic, err)
	}
	waitFor(t, 10*time.Second, "the known-session store to hold "+sessionID+"'s record", func() bool {
		record, ok := rig.known.Get(sessionID)
		return ok && slices.Contains(record.Topics, topic)
	})
	filter, err := storedRowFilter(t.Context(), rig.harness.registry, sessionID, []string{topic})
	if err != nil {
		t.Fatalf("build the filter of %s from %s: %v", sessionID, topic, err)
	}
	if !slices.Contains(filter, topic) {
		t.Fatalf("UNKNOWN: the filter built from %s is %v, which does not hold it; the row needs a consumer filter that follows a topic no stream captures", topic, filter)
	}
	jetStream := rig.deps.client.PublishTakesJetStream(t.Context(), topic)
	if jetStream {
		t.Fatalf("UNKNOWN: the send's transport for %s is JetStream; the row needs a decision of core NATS", topic)
	}
	if recipients, connected := recipientCounts(t.Context(), rig.deps, topic, lostLaneSender, jetStream); recipients != nil || connected != nil {
		t.Fatalf("a publish on %s, which the send takes over core NATS, counts %v recipients and %v connected with %s's interest row and known-session record naming it, want both left out: whether a stream stores the message is unknown, and the filter built from that row is %v", topic, countOrNone(recipients), countOrNone(connected), sessionID, filter)
	}
}

// countOrNone is *p, or -1 for a count left out, for a failure message.
func countOrNone(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// retireStreamSubject plays an operator retiring subject from the stream: it removes it.
func retireStreamSubject(t *testing.T, js natsgo.JetStreamContext, subject string) {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read %s: %v", bus.Stream, err)
	}
	config := info.Config
	config.Subjects = slices.DeleteFunc(slices.Clone(config.Subjects), func(kept string) bool { return kept == subject })
	if _, err := js.UpdateStream(&config); err != nil {
		t.Fatalf("retire %s: %v", subject, err)
	}
}

// A publish counts its recipients by the transport its send takes, which the publish route decides
// once (bus.Client.PublishTakesJetStream) and hands to both the count (recipientCounts) and the
// send (bus.Client.PublishAckOn), even where the subjects the client holds change between that
// decision and the count. Here the decision reads the subjects again while no stream subject
// captures the topic, so the send goes over core NATS; a newer build's start then adds a subject
// that captures it, and a read of the subjects takes that in before the count; and an operator
// retires the subject again before the send, so no stream stores the message and no consumer
// delivers it, as the decision said. Both counts are left out, the send has no acknowledgement,
// and the stream holds nothing on the topic. The row drives the route's helpers in the route's
// order with the subjects changed between them, since no seam of the route lets a test land a read
// there; the client holds off its periodic reads of the subjects (bus.WithStreamSubjectsRefresh,
// bus.WithKeepInterval), so the row's own reads are the only ones.
//
// A count that judged the transport again on the subjects the client last held
// (bus.Client.UsesCoreTransport) counted the session that follows the topic, recipients 1 for a
// message no consumer receives; a send that judged it again would as well have read the subjects
// apart from the count. Each premise is checked as it is set up, and a row whose premise does not
// hold fails as UNKNOWN.
func TestAPublishCountsItsRecipientsByTheTransportItsSendTakes(t *testing.T) {
	t.Parallel()
	const (
		sessionID = "ses_transport_follower"
		topic     = "notifications.x.counted"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("UNKNOWN: this build compiles a subject capturing %s; the row needs a topic only %s captures", topic, rollNewerSubject)
	}
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1),
		bus.WithKeepInterval(backgroundReadsHeld), bus.WithStreamSubjectsRefresh(backgroundReadsHeld))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newKnownSessionRigOn(t, client, url)
	waitForRegistry(t, rig.harness.registry)
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open the row's JetStream: %v", err)
	}
	storeRawRow(t, rig.harness.registry, client, sessionID, topic)
	if err := rig.known.RecordWithin(t.Context(), session.KnownSession{SessionID: sessionID, Topics: []string{topic}}); err != nil {
		t.Fatalf("write %s's known-session record naming %s: %v", sessionID, topic, err)
	}
	waitFor(t, 10*time.Second, "the known-session store to hold "+sessionID+"'s record", func() bool {
		record, ok := rig.known.Get(sessionID)
		return ok && slices.Contains(record.Topics, topic)
	})
	filter, err := storedRowFilter(t.Context(), rig.harness.registry, sessionID, []string{topic})
	if err != nil {
		t.Fatalf("build the filter of %s from %s: %v", sessionID, topic, err)
	}
	if !slices.Contains(filter, topic) {
		t.Fatalf("UNKNOWN: the filter built from %s is %v, which does not hold it; the row needs a session whose consumer filters on the topic", topic, filter)
	}

	ctx := t.Context()
	jetStream := client.PublishTakesJetStream(ctx, topic)
	if jetStream {
		t.Fatalf("UNKNOWN: the send's transport for %s is JetStream before any stream subject captures it; the row needs a decision of core NATS", topic)
	}
	addStreamSubject(t, js, rollNewerSubject)
	if client.AskDeployedSubjects().Uncaptured(ctx, topic) || client.UsesCoreTransport(topic) {
		t.Fatalf("UNKNOWN: a read of the subjects after %s was added leaves %s uncaptured; the row needs subjects that capture it at the count", rollNewerSubject, topic)
	}
	retireStreamSubject(t, js, rollNewerSubject)

	if recipients, connected := recipientCounts(ctx, rig.deps, topic, lostLaneSender, jetStream); recipients != nil || connected != nil {
		t.Fatalf("a publish on %s whose send takes core NATS, decided before a read of the subjects captured the topic, counts %v recipients and %v connected with %s following it, want both left out: the count did not take the send's decision",
			topic, countOrNone(recipients), countOrNone(connected), sessionID)
	}
	ack, err := client.PublishAckOn(ctx, listenerTestEnvelope(topic, "transport-decided-once"), jetStream)
	if err != nil || ack != (bus.StoredAck{}) {
		t.Fatalf("the send on the transport decided, core NATS, answered %+v, %v; want no acknowledgement and no error", ack, err)
	}
	if _, err := js.GetLastMsg(bus.Stream, topic); !errors.Is(err, natsgo.ErrMsgNotFound) {
		t.Fatalf("the stream's last message on %s after the send = %v, want none: the row needs a message no stream stores", topic, err)
	}
}
