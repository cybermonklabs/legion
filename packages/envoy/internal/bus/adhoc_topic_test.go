package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// adhocFake is the clock an ad-hoc publish below runs on, standing still: a publish that took the
// JetStream transport would wait on the acknowledgement bound it arms there and never answer,
// where the core publish it must take is answered by the server's confirmation.
func adhocFake() *clocktest.Fake {
	return clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
}

const grantedTopic = "notifications.lxspec.granted"

// Which transport each kind of topic takes for a client that has not read the deployed stream's
// subjects, which decides by the subjects this build compiles: the role lanes and their exception
// lanes, and every notification topic the stream does not capture, over core NATS; every topic the
// stream captures, its exception lane included, and every subject outside notifications., through
// JetStream. An exception lane follows the same rule as any other topic: the stream captures the
// lane of an agent subject and of a team topic and no other lane, so the lane of any other
// captured topic takes core NATS.
func TestUsesCoreTransportForRoleLanesAndEveryNotificationTheStreamDoesNotCapture(t *testing.T) {
	for _, tc := range []struct {
		topic string
		core  bool
	}{
		{contracts.RoleTopicPrefix + "reviewer", true},
		{ExceptionTopicPrefix + contracts.RoleTopicPrefix + "reviewer", true},
		{"notifications.team.deploy-squad", false},
		{"notifications.team.deploy-squad.all", false},
		{ExceptionTopicPrefix + "notifications.team.deploy-squad", false},
		{"notifications.lxspec.probe.all", true},
		// A `*` or `>` in a published topic is a character of it, not a wildcard: the stream's
		// literal-prefix subjects capture none of these, so they take core NATS.
		{"notifications.>", true},
		{"notifications.*.deploy", true},
		{ExceptionTopicPrefix + ">", true},
		{contracts.AgentTopicPrefix + "ses_a", false},
		{ExceptionTopicPrefix + contracts.AgentTopicPrefix + "ses_a", false},
		{"notifications.dispatch.issue.ACME-1.ask.opened", false},
		{"notifications.github.acme.widgets.pr.1", false},
		{"notifications.slack.T1.C1.message", false},
		{"notifications.legion.ACME.controller", false},
		{"notifications.ghostwispr.s1.summary.ready", false},
		{"notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message", false},
		{"team.deploy-squad.all", false},
		{"$KV.envoy_roles.reviewer", false},
	} {
		if got := (&Client{}).UsesCoreTransport(tc.topic); got != tc.core {
			t.Errorf("UsesCoreTransport(%q) = %t, want %t", tc.topic, got, tc.core)
		}
	}
}

// A client that has read the deployed stream's subjects decides by them, not by the subjects this
// build compiles: a topic only the deployed stream captures takes JetStream, one only this build
// captures takes core NATS. A role lane and its exception lane take core NATS even from a stream
// that still captures them.
func TestUsesCoreTransportDecidesByTheDeployedSubjectsOnceItHasReadThem(t *testing.T) {
	for _, tc := range []struct {
		deployed []string
		topic    string
		core     bool
	}{
		{[]string{"notifications.github.>", "notifications.x.*"}, "notifications.x.y", false},
		{[]string{"notifications.github.>", "notifications.x.*"}, "notifications.x.y.z", true},
		{[]string{"notifications.github.>", "notifications.x.*"}, "notifications.x", true},
		{[]string{"notifications.github.>", "notifications.x.*"}, "notifications.github.acme.widgets.pr.1", false},
		{[]string{"notifications.github.>", "notifications.x.*"}, contracts.AgentTopicPrefix + "ses_a", true},
		{[]string{"notifications.>"}, "notifications.team.deploy-squad", false},
		{[]string{"notifications.>"}, contracts.RoleTopicPrefix + "reviewer", true},
		{[]string{"notifications.>"}, ExceptionTopicPrefix + contracts.RoleTopicPrefix + "reviewer", true},
		{[]string{"notifications.>"}, "team.deploy-squad.all", false},
	} {
		client := &Client{}
		client.deployedSubjects.Store(&tc.deployed)
		if got := client.UsesCoreTransport(tc.topic); got != tc.core {
			t.Errorf("deployed %v: UsesCoreTransport(%q) = %t, want %t", tc.deployed, tc.topic, got, tc.core)
		}
	}
}

// StreamCaptures reads each stream subject as a literal prefix and `.>`; a subject of any other
// shape needs a matcher that splits tokens.
func TestEveryStreamSubjectIsALiteralPrefixAndWildcard(t *testing.T) {
	for _, subject := range streamCarriedSubjects {
		prefix, ok := strings.CutSuffix(subject, ".>")
		if !ok || prefix == "" || strings.ContainsAny(prefix, "*>") {
			t.Errorf("stream subject %q is not a literal prefix followed by .>", subject)
		}
	}
}

// An ad-hoc topic, one no stream subject captures, is published over core NATS: the publish
// answers at once, a subscriber receives the envelope, and the stream retains nothing for it.
// Before the fix the publish went to JetStream, which never answers it while a core subscriber
// holds the subject, so it waited out its acknowledgement bound - which here is on a clock that
// stands still, so such a publish never answers at all.
func TestAnAdHocTopicIsPublishedOverCoreNATSAndReachesItsSubscriber(t *testing.T) {
	uri := testnats.URL(t)
	fake := adhocFake()
	client, err := ConnectOwningStream([]string{uri}, WithClock(fake), WithPublishAcknowledgementClock(fake))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	subscriber := testnats.Connect(t, uri)
	t.Cleanup(subscriber.Close)

	const topic = "notifications.lxspec.probe.all"
	received, err := subscriber.SubscribeSync(topic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	envelope := contracts.Envelope{
		EventID:        "evt-adhoc",
		Source:         "agent",
		SourceEventID:  "source-adhoc",
		Topic:          topic,
		DedupeKey:      "publish.adhoc",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "deploy complete",
		TraceID:        "trace-adhoc",
	}
	published := make(chan error, 1)
	var duplicate bool
	go func() {
		stored, err := client.PublishReportingDuplicate(envelope)
		duplicate = stored
		published <- err
	}()
	if err := testwait.Receive(t, "the ad-hoc publish to be answered with the clock standing still", published); err != nil {
		t.Fatalf("publish to an ad-hoc topic: %v", err)
	}
	if duplicate {
		t.Fatal("a core publish has no stream verdict and reports no duplicate")
	}
	msg, err := received.NextMsgWithContext(testwait.Context(t))
	if err != nil {
		t.Fatalf("the subscriber received nothing: %v", err)
	}
	if msg.Reply != "" {
		t.Fatalf("the envelope arrived as a request with reply %q; a JetStream publish would be one", msg.Reply)
	}

	if _, err := client.JS().GetLastMsg(Stream, topic); !errors.Is(err, nats.ErrMsgNotFound) {
		t.Fatalf("the stream's last message on %s: %v, want none", topic, err)
	}

	// The control: a topic the stream captures still goes to JetStream and is retained.
	captured := envelope
	captured.EventID = "evt-captured"
	captured.Topic = "notifications.github.acme.widgets.pr.1"
	if err := client.Publish(captured); err != nil {
		t.Fatalf("publish to a captured topic: %v", err)
	}
	if _, err := client.JS().GetLastMsg(Stream, captured.Topic); err != nil {
		t.Fatalf("the stream did not retain the captured topic: %v", err)
	}
}

// A publish to an ad-hoc topic the server denies the publisher is a refusal naming the subject,
// not a wait: a listener whose grant leaves the topic out reports that at once. The publish's
// clock stands still, so a publish that waited on the acknowledgement bound a JetStream send arms
// would never answer.
func TestAnAdHocPublishNATSDeniesReturnsTheDenial(t *testing.T) {
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, grantedTopic, "_INBOX.>").URL
	t.Setenv("NATS_NKEY_SEED", seed)
	fake := adhocFake()
	client, err := Connect([]string{uri}, WithClock(fake), WithPublishAcknowledgementClock(fake))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	envelope := contracts.Envelope{
		EventID:        "evt-denied",
		Source:         "agent",
		SourceEventID:  "source-denied",
		Topic:          "notifications.lxspec.denied",
		DedupeKey:      "publish.denied",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "denied",
		TraceID:        "trace-denied",
	}
	denied := make(chan error, 1)
	go func() { denied <- client.Publish(envelope) }()
	if err := testwait.Receive(t, "the denied ad-hoc publish to be answered with the clock standing still", denied); !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("denied ad-hoc publish returned %v, want ErrPublishDenied", err)
	}
	envelope.Topic = grantedTopic
	if err := client.Publish(envelope); err != nil {
		t.Fatalf("granted ad-hoc publish after a denial: %v", err)
	}
}
