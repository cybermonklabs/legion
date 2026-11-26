package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
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
// (sessionDelivery.storedRowFilter), holds a subject the message matches, whichever store names
// the topics: the session's interest row or its known-session record. Each kind of topic that
// judgment drops (storedTopicFiltered) is a row, in each store: a controller notice lane whose
// role the session does not hold, another session's inbox in either form, a failed-delivery
// exception lane, a wildcard reaching a private stream subject, and a topic that does not begin
// with notifications., which no row's rule drops and the filter cannot take (filterTakes,
// bus.CanFilterOn). Each was counted, one recipient whose consumer delivered nothing; now each is
// counted as none. Two rows the judgment keeps are the controls, each counted once: the lane of
// the role the session holds, which shows the rule reads the role's claim rather than dropping
// every lane, and a team topic.
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
				recipients := countRecipientsOf(t, rig.deps, row.subject)
				if row.merge != "" {
					// No stream subject captures the pair's subject, so its publish takes core NATS
					// and carries no count (recipientCounts); the count's judgment of the session is
					// read as it judges a publish the stream stores, where main counted it anyway.
					recipients, _ = recipientCounts(t.Context(), rig.deps, row.subject, lostLaneSender, true)
				}
				if recipients == nil || *recipients != row.want {
					t.Fatalf("a publish on %s counts %v recipients with %s's %s naming %v, want %d: the filter built from those topics is %v", row.subject, countOrNone(recipients), sessionID, store, row.topics, row.want, filter)
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
	waitFor(t, "the known-session store to hold "+sessionID+"'s record", func() bool {
		record, ok := rig.known.Get(sessionID)
		return ok && slices.Contains(record.Topics, topic)
	})
	filter, err := rig.deps.consumers.storedRowFilter(t.Context(), rig.harness.registry, sessionID, []string{topic})
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
// and the stream holds nothing on the topic. The row pins the helpers only: it drives them in the
// route's order with one decision handed to both, so a route that decided twice would pass it;
// TestThePublishRouteCountsAndSendsOnItsOneDecisionOfTheTransport drives the route itself. The
// client holds off its periodic reads of the subjects (bus.WithStreamSubjectsRefresh,
// bus.WithKeepInterval), so the row's own reads are the only ones.
//
// A count that judged the transport again on the subjects the client last held
// (bus.Client.UsesCoreTransport) counted the session that follows the topic, recipients 1 for a
// message no consumer receives. Each premise is checked as it is set up, and a row whose premise
// does not hold fails as UNKNOWN.
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
	waitFor(t, "the known-session store to hold "+sessionID+"'s record", func() bool {
		record, ok := rig.known.Get(sessionID)
		return ok && slices.Contains(record.Topics, topic)
	})
	filter, err := rig.deps.consumers.storedRowFilter(t.Context(), rig.harness.registry, sessionID, []string{topic})
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

// The publish route (POST /v1/messages/publish) counts a message's recipients and sends it on one
// decision of its transport (bus.Client.PublishTakesJetStream), whatever read of the deployed
// subjects lands between that decision and the send. Here the route's read of the subjects goes
// unanswered, so it refuses nothing and decides on the subjects its client holds, which leave the
// topic uncaptured: core NATS. Between that decision and the send (bus.WithAfterPublishTransport) a
// newer build's start adds a subject that captures the topic, a read of the subjects takes that in,
// and an operator retires the subject again, so no stream stores the message and none answers a
// JetStream publish of it. The route answers 200 with both counts left out, though a session
// follows the topic, and the send goes over core NATS: the stream holds nothing on the topic.
//
// A send that decided the transport again, on the subjects that read left, took JetStream for a
// topic no stream captures, which no stream answers, and the route answered 503. A count that
// decided again counted the session that follows the topic. Each premise is checked as it is set
// up, and a row whose premise does not hold fails as UNKNOWN.
func TestThePublishRouteCountsAndSendsOnItsOneDecisionOfTheTransport(t *testing.T) {
	t.Parallel()
	const (
		sessionID = "ses_route_transport_follower"
		topic     = "notifications.x.route-counted"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("UNKNOWN: this build compiles a subject capturing %s; the row needs a topic only %s captures", topic, rollNewerSubject)
	}
	url := testnats.URL(t)
	relay := testnats.StartUnansweredRelayOn(t, url, "$JS.API.STREAM.INFO."+bus.Stream)
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open the row's JetStream: %v", err)
	}
	// between is what lands between the route's decision and its send, once, for the row's
	// topic: what it found is read once the route has answered, on this goroutine, which the
	// route's handler runs on.
	var (
		client    *bus.Client
		decided   atomic.Bool
		jetStream bool
		captured  bool
	)
	between := func(decidedTopic string, decision bool) {
		if decidedTopic != topic || !decided.CompareAndSwap(false, true) {
			return
		}
		jetStream = decision
		addStreamSubject(t, js, rollNewerSubject)
		captured = !client.AskDeployedSubjects().Uncaptured(t.Context(), topic) && !client.UsesCoreTransport(topic)
		retireStreamSubject(t, js, rollNewerSubject)
	}
	client, err = bus.ConnectOwningStream([]string{relay.URL()}, bus.WithReplicas(1),
		bus.WithKeepInterval(backgroundReadsHeld), bus.WithStreamSubjectsRefresh(backgroundReadsHeld),
		bus.WithAfterPublishTransport(between))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newKnownSessionRigOn(t, client, url)
	waitForRegistry(t, rig.harness.registry)
	storeRawRow(t, rig.harness.registry, client, sessionID, topic)
	if err := rig.known.RecordWithin(t.Context(), session.KnownSession{SessionID: sessionID, Topics: []string{topic}}); err != nil {
		t.Fatalf("write %s's known-session record naming %s: %v", sessionID, topic, err)
	}
	waitFor(t, "the known-session store to hold "+sessionID+"'s record", func() bool {
		record, ok := rig.known.Get(sessionID)
		return ok && slices.Contains(record.Topics, topic)
	})
	if client.DeployedStreamCaptures(topic) {
		t.Fatalf("UNKNOWN: the listener judges %s captured before the publish; the row needs subjects that leave it uncaptured", topic)
	}

	relay.Arm(1)
	answer := publishOn(rig.mux, topic)
	switch {
	case !decided.Load():
		t.Fatalf("UNKNOWN: the publish route answered %d %s and never decided the transport of %s", answer.Code, answer.Body.String(), topic)
	case jetStream:
		t.Fatalf("UNKNOWN: the route's decision for %s was JetStream; the row needs a decision of core NATS, which its unanswered read of the subjects leaves", topic)
	case !captured:
		t.Fatalf("UNKNOWN: a read of the subjects after %s was added left %s uncaptured; the row needs subjects that capture it between the decision and the send", rollNewerSubject, topic)
	}
	if answer.Code != http.StatusOK {
		t.Fatalf("the publish on %s, decided core NATS before a read of the subjects captured it, was answered %d %s, want 200: the send did not take the route's decision", topic, answer.Code, answer.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(answer.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the publish's answer %s: %v", answer.Body.String(), err)
	}
	if recipients, connected := body["recipients"], body["recipients_connected"]; recipients != nil || connected != nil {
		t.Fatalf("the publish on %s, whose send takes core NATS, answered recipients %s and recipients_connected %s with %s following it, want both left out: the count did not take the route's decision",
			topic, recipients, connected, sessionID)
	}
	if _, err := js.GetLastMsg(bus.Stream, topic); !errors.Is(err, natsgo.ErrMsgNotFound) {
		t.Fatalf("the stream's last message on %s after the publish = %v, want none: the send did not go over core NATS", topic, err)
	}
}
