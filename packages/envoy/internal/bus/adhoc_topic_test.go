package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

const grantedTopic = "notifications.lxspec.granted"

// compiledTopicKinds is each kind of topic with the transport it takes for a client that has not
// read the deployed stream's subjects, which decides by the subjects this build compiles: the role
// lanes and their exception lanes, and every notification topic the stream does not capture, over
// core NATS; every topic the stream captures, its exception lane included, and every subject
// outside notifications., through JetStream. An exception lane follows the same rule as any other
// topic: the stream captures the lane of an agent subject and of a team topic and no other lane, so
// the lane of any other captured topic takes core NATS.
var compiledTopicKinds = []struct {
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
}

func TestUsesCoreTransportForRoleLanesAndEveryNotificationTheStreamDoesNotCapture(t *testing.T) {
	t.Parallel()
	for _, tc := range compiledTopicKinds {
		if got := (&Client{}).UsesCoreTransport(tc.topic); got != tc.core {
			t.Errorf("UsesCoreTransport(%q) = %t, want %t", tc.topic, got, tc.core)
		}
	}
}

// narrowedAsTheQuiesce is subjects as a hub move's quiesce narrows them, so that the stream
// stores nothing a client publishes: the first wildcard token of each subject becomes
// __envoy_move_frozen, a `>` becoming __envoy_move_frozen.>. A subject with no wildcard is left as
// it is. The tests outside this package take bustest.NarrowedAsTheQuiesce, which this package
// cannot import.
func narrowedAsTheQuiesce(subjects []string) []string {
	narrowed := make([]string, len(subjects))
	for i, subject := range subjects {
		tokens := strings.Split(subject, ".")
		for j, token := range tokens {
			if token == "*" {
				tokens[j] = "__envoy_move_frozen"
				break
			}
			if token == ">" {
				tokens[j] = "__envoy_move_frozen.>"
				break
			}
		}
		narrowed[i] = strings.Join(tokens, ".")
	}
	return narrowed
}

// The one verdict the bus gives each topic (publishPathOf), and the two questions read from it
// (UsesCoreTransport, ReachesNoSession), for a client that holds a deployed list. A topic the
// deployed stream captures takes JetStream, also one only a newer build's subject captures; a
// topic this build stores that the deployed stream does not capture is refused, so nothing is
// sent; a role lane and its exception lane take core NATS even from a stream that captures them,
// and so does a notification topic neither list captures. A subject outside notifications. takes
// JetStream.
//
// The row of an agent topic over the first deployed list took core NATS before the refusal: the
// deployed list lacks notifications.agent.>, so an agent topic read as an ad-hoc topic and went
// where nothing stores it.
func TestThePublishPathOfEachTopicKind(t *testing.T) {
	t.Parallel()
	withDeployed := func(deployed []string) *Client {
		client := &Client{}
		client.deployedSubjects.Store(&subjectsRead{subjects: deployed})
		return client
	}
	type row struct {
		topic string
		path  publishPath
	}
	check := func(t *testing.T, client *Client, rows []row) {
		t.Helper()
		for _, tc := range rows {
			if got := client.publishPathOf(tc.topic); got != tc.path {
				t.Errorf("publishPathOf(%q) = %s, want %s", tc.topic, got, tc.path)
			}
			if got, want := client.UsesCoreTransport(tc.topic), tc.path == overCoreNATS; got != want {
				t.Errorf("UsesCoreTransport(%q) = %t, want %t", tc.topic, got, want)
			}
			// A topic reaches no session exactly when it takes core NATS and is no role lane: an
			// ad-hoc topic, or the exception lane of a role lane. A refused topic is one this build
			// stores, which reaches its sessions once the stream captures it again.
			wantNoSession := tc.path == overCoreNATS && !strings.HasPrefix(tc.topic, contracts.RoleTopicPrefix)
			if got := client.ReachesNoSession(tc.topic); got != wantNoSession {
				t.Errorf("ReachesNoSession(%q) = %t, want %t", tc.topic, got, wantNoSession)
			}
		}
	}
	t.Run("a deployed list of github and x.*", func(t *testing.T) {
		check(t, withDeployed([]string{"notifications.github.>", "notifications.x.*"}), []row{
			{contracts.AgentTopicPrefix + "ses_a", refusedUndeployed},
			{"notifications.team.t", refusedUndeployed},
			{ExceptionTopicPrefix + contracts.AgentTopicPrefix + "ses_a", refusedUndeployed},
			{"notifications.x.y", overJetStream},
			{"notifications.x.y.z", overCoreNATS},
			{"notifications.x", overCoreNATS},
			{"notifications.github.acme.widgets.pr.1", overJetStream},
		})
	})
	t.Run("a deployed list of every notification", func(t *testing.T) {
		check(t, withDeployed([]string{"notifications.>"}), []row{
			{contracts.RoleTopicPrefix + "reviewer", overCoreNATS},
			{ExceptionTopicPrefix + contracts.RoleTopicPrefix + "reviewer", overCoreNATS},
			{"notifications.team.t", overJetStream},
			{"notifications.team.deploy-squad", overJetStream},
			{"team.deploy-squad.all", overJetStream},
		})
	})
	t.Run("the compiled list narrowed as the quiesce narrows it", func(t *testing.T) {
		client := withDeployed(narrowedAsTheQuiesce(streamCarriedSubjects))
		var rows []row
		for _, tc := range compiledTopicKinds {
			want := overJetStream
			switch {
			case tc.core:
				want = overCoreNATS
			case strings.HasPrefix(tc.topic, NotificationTopicPrefix):
				want = refusedUndeployed
			}
			rows = append(rows, row{tc.topic, want})
		}
		check(t, client, rows)
	})
	t.Run("a client that has read nothing", func(t *testing.T) {
		var rows []row
		for _, tc := range compiledTopicKinds {
			want := overJetStream
			if tc.core {
				want = overCoreNATS
			}
			rows = append(rows, row{tc.topic, want})
		}
		check(t, &Client{}, rows)
	})
}

// StreamCaptures reads each stream subject as a literal prefix and `.>`; a subject of any other
// shape needs a matcher that splits tokens.
func TestEveryStreamSubjectIsALiteralPrefixAndWildcard(t *testing.T) {
	t.Parallel()
	for _, subject := range streamCarriedSubjects {
		prefix, ok := strings.CutSuffix(subject, ".>")
		if !ok || prefix == "" || strings.ContainsAny(prefix, "*>") {
			t.Errorf("stream subject %q is not a literal prefix followed by .>", subject)
		}
	}
}

// An ad-hoc topic, one no stream subject captures, is published over core NATS: the publish
// answers at once, a subscriber receives the envelope, and the stream retains nothing for it. Before
// the fix the publish went to JetStream, which never answers it while a core subscriber holds the
// subject, so it failed at its five-second acknowledgement deadline.
func TestAnAdHocTopicIsPublishedOverCoreNATSAndReachesItsSubscriber(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri})
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
	started := time.Now()
	duplicate, err := client.PublishReportingDuplicate(envelope)
	if err != nil {
		t.Fatalf("publish to an ad-hoc topic: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("publish to an ad-hoc topic took %s; a core publish answers once the server confirms it", elapsed)
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

// A publish to an ad-hoc topic the server denies the publisher is a refusal naming the subject, not
// a five-second wait: a listener whose grant leaves the topic out reports that at once.
func TestAnAdHocPublishNATSDeniesReturnsTheDenial(t *testing.T) {
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, grantedTopic, "_INBOX.>").URL
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{uri})
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
	started := time.Now()
	err = client.Publish(envelope)
	if !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("denied ad-hoc publish returned %v, want ErrPublishDenied", err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("denied ad-hoc publish waited %s", elapsed)
	}
	envelope.Topic = grantedTopic
	if err := client.Publish(envelope); err != nil {
		t.Fatalf("granted ad-hoc publish after a denial: %v", err)
	}
}
