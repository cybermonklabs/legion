package main

import (
	"encoding/json"
	"slices"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// A topic under a subject only a newer build's stream carries (notifications.x.>, which this build
// does not compile) is registered through this build's listener with no warning, and the session's
// consumer filters on it, so a message stored on it while the two builds run is the consumer's to
// deliver. A topic no stream captures is registered and filtered on too, with the warning that
// nothing is delivered on it until a stream subject captures it: neither the registration route
// nor the consumer filter asks the stream's subjects, so the filter carries both topics the route
// stored, and the consumer delivers what the stream stores on a topic once a subject captures it.
func TestSessionConsumerFiltersOnATopicOnlyTheDeployedStreamCaptures(t *testing.T) {
	const (
		sessionID  = "ses_deployed_subject"
		newer      = "notifications.x.>"
		captured   = "notifications.x.y"
		uncaptured = "notifications.lxspec.probe.all"
	)
	for _, topic := range []string{captured, uncaptured} {
		if bus.StreamCaptures(topic) {
			t.Fatalf("this build compiles a subject capturing %s; the test needs one it does not", topic)
		}
	}
	url := testnats.URL(t)
	deployNewerBuildsStream(t, url, newer)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	if client.ReachesNoSession(captured) {
		t.Fatalf("the listener's client judges %s on the subjects it compiles, not on the deployed stream's", captured)
	}
	rig := newSessionConsumerRigOn(t, client, url)

	raw := rig.post(t, "/v1/interests/subscribe", `{"session_id":"`+sessionID+`","self_subscribed":true,
		"topics":["`+captured+`","`+uncaptured+`"],"delivery":{"consumer":true,"epoch":"e"}}`)
	var answer interestSubscribeAnswer
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		t.Fatalf("decode the registration's answer %s: %v", raw, err)
	}
	if want := []string{listenerwire.UncapturedTopicWarning(uncaptured, sessionID)}; !slices.Equal(answer.Warnings, want) {
		t.Fatalf("warnings = %q, want only %q: %s is captured by the deployed stream and %s by no stream", answer.Warnings, want, captured, uncaptured)
	}
	if !slices.Contains(answer.Topics, captured) || !slices.Contains(answer.Topics, uncaptured) {
		t.Fatalf("answered topics = %v, want %s and %s among them", answer.Topics, captured, uncaptured)
	}
	want := []string{"notifications.agent.*." + sessionID, contracts.LegacyAgentSubject(sessionID), uncaptured, captured}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v: the filter must carry every topic the registration stored", got, want)
	}

	rig.store(t, captured)
	rig.awaitPending(t, sessionID, 1)
}

// A fan-out of a message on a topic only the deployed stream captures, to a session on consumer
// delivery that follows it, is that session's own consumer's to relay: the consumer filters on the
// topic (sessionConsumerFilter), so the fan-out counts the session skipped, records nothing for
// it, and publishes no delivery exception. A fan-out that judged the topic on the subjects this
// build compiles would read the session as one nothing reaches and report a failure for a message
// its consumer delivers.
//
// The row hands the message to the fan-out through the core handler. In production the listener's
// own durable hands it over: that durable filters on every subject (durable.go, consumerFilterAll,
// set by ensureListenerDurable), so on either build a topic only a newer build's subjects capture
// reaches the fan-out, and the session's consumer filter is what delivers it during a roll. The row
// holds that the fan-out judges such a message as the consumer filter does.
func TestFanoutLeavesATopicOnlyTheDeployedStreamCapturesToTheSessionsConsumer(t *testing.T) {
	const (
		sessionID = "ses_deployed_fanout"
		topic     = "notifications.x.y"
	)
	if bus.StreamCaptures(topic) {
		t.Fatalf("this build compiles a subject capturing %s; the test needs one it does not", topic)
	}
	harness := newListenerDeliveryHarnessOn(t, connectToNewerBuildsStream(t, "notifications.x.>"))
	harness.registerConsumerTarget(t, sessionID, topic, nil)
	if _, state := harness.sessions.Lookup(sessionID); state != session.Live {
		t.Fatalf("session %s reads %s before the delivery, want live", sessionID, state)
	}
	item := listenerTestEnvelope(topic, "fanout-deployed-subject")
	probe := exceptionProbe(t, harness.client)

	harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush what the fan-out published: %v", err)
	}

	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d, want 1 - the session's own consumer holds %s:\n%s", deliveredSeries("skipped"), got, topic, body)
	}
	if got := counterValue(t, body, deliveredSeries("failed")); got != -1 {
		t.Fatalf("%s = %d, want no series at all - the session's consumer relays the message:\n%s", deliveredSeries("failed"), got, body)
	}
	noException(t, probe)
	if ledgerDelivered(t, harness.config, item.DedupeKey, sessionID) || ledgerAttempted(t, harness.config, item.DedupeKey, sessionID) {
		t.Fatalf("the fan-out recorded %s, which it handed the message to nobody for", sessionID)
	}
}

// A delivery exception on a lane only the deployed stream captures is stored under its one-copy
// message id (exception.<reason>.<origin>), so the stream keeps one copy however many listeners
// raise it for the same stored message. Judged on the subjects this build compiles, the lane would
// be published under each exception's own dedupe key, and every listener's report would be kept.
func TestADeliveryExceptionOnALaneOnlyTheDeployedStreamCapturesIsStoredOnce(t *testing.T) {
	const (
		topic  = "notifications.x.y"
		lane   = bus.ExceptionTopicPrefix + topic
		reason = "delivery_failed"
		origin = "origin-1"
	)
	if bus.StreamCaptures(lane) {
		t.Fatalf("this build compiles a subject capturing %s; the test needs one it does not", lane)
	}
	client := connectToNewerBuildsStream(t, bus.ExceptionTopicPrefix+"notifications.x.>")
	item := listenerTestEnvelope(topic, "exception-deployed-lane")
	// Two listeners raise the exception for the one stored message, each from its own durable.
	for range 2 {
		if err := publishDeliveryException(client, item, reason, "ses_deployed_exception", origin); err != nil {
			t.Fatalf("publish the delivery exception: %v", err)
		}
	}
	js := client.JS()
	info, err := js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: lane})
	if err != nil {
		t.Fatalf("read %s: %v", bus.Stream, err)
	}
	if got := info.State.Subjects[lane]; got != 1 {
		t.Fatalf("the stream holds %d exceptions on %s, want 1", got, lane)
	}
	stored, err := js.GetLastMsg(bus.Stream, lane)
	if err != nil {
		t.Fatalf("read the exception on %s: %v", lane, err)
	}
	if got, want := stored.Header.Get(natsgo.MsgIdHdr), "exception."+reason+"."+origin; got != want {
		t.Fatalf("stored message id = %q, want %q", got, want)
	}
}

// A delivery exception on a lane only a subject added since the listener last read the deployed
// stream's subjects captures - a newer build's start adding its own during a roll - is stored under
// its one-copy message id too: publishDeliveryException judges the lane as its publish's transport
// does (bus.Client.PublishTakesJetStream): on a read of the subjects again where the client's own
// leave the lane uncaptured, made here because no read of them has ended in the last 250 ms and
// none is in flight, and not on the subjects its client holds, which the row proves stale before
// the exception. Inside that gap, or while a read younger than 2 s is in flight, the transport
// reads nothing and answers on the subjects held. The same exception handed twice, as two
// listeners each raise it for one stored message from their own durables, is stored once, under
// exception.<reason>.<origin>.
//
// Judged on the subjects the client held, the first exception went to Publish, whose transport read
// the subjects again and stored it under the exception's own dedupe key; that read was kept, so the
// second was judged captured and stored under exception.<reason>.<origin>: two copies.
func TestADeliveryExceptionOnALaneAddedSinceTheLastReadIsStoredOnce(t *testing.T) {
	const (
		topic  = "notifications.x.z"
		lane   = bus.ExceptionTopicPrefix + topic
		added  = bus.ExceptionTopicPrefix + "notifications.x.>"
		reason = "delivery_failed"
		origin = "origin-added"
	)
	if bus.StreamCaptures(lane) {
		t.Fatalf("this build compiles a subject capturing %s; the test needs one it does not", lane)
	}
	client := connectToNewerBuildsStream(t)
	js := client.JS()
	addStreamSubject(t, js, added)
	if client.DeployedStreamCaptures(lane) {
		t.Fatalf("the listener judged %s captured before reading the stream's subjects again; the row needs a stale read", lane)
	}

	item := listenerTestEnvelope(topic, "exception-added-lane")
	for range 2 {
		if err := publishDeliveryException(client, item, reason, "ses_added_exception", origin); err != nil {
			t.Fatalf("publish the delivery exception: %v", err)
		}
	}
	info, err := js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: lane})
	if err != nil {
		t.Fatalf("read %s: %v", bus.Stream, err)
	}
	if got := info.State.Subjects[lane]; got != 1 {
		t.Fatalf("the stream holds %d exceptions on %s after the same exception was raised twice, want 1: they were stored under two message ids", got, lane)
	}
	stored, err := js.GetLastMsg(bus.Stream, lane)
	if err != nil {
		t.Fatalf("read the exception on %s: %v", lane, err)
	}
	if got, want := stored.Header.Get(natsgo.MsgIdHdr), "exception."+reason+"."+origin; got != want {
		t.Fatalf("stored message id = %q, want %q", got, want)
	}
}

// connectToNewerBuildsStream starts a server, creates on it the stream a newer build carrying
// extra leaves (deployNewerBuildsStream), and connects this build's listener client to it, which
// owns the stream and leaves it as it is.
func connectToNewerBuildsStream(t *testing.T, extra ...string) *bus.Client {
	t.Helper()
	url := testnats.URL(t)
	deployNewerBuildsStream(t, url, extra...)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// deployNewerBuildsStream creates the notifications stream on the server url names as a newer
// build leaves it: every subject this build carries and extra, under a generation above any this
// build stamps, so a start of this build that owns the stream leaves it as it is. It carries the
// stream's identity, which a consumer registration answers with.
func deployNewerBuildsStream(t *testing.T, url string, extra ...string) {
	t.Helper()
	conn := testnats.Connect(t, url)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream to create the newer build's stream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{
		Name:      bus.Stream,
		Subjects:  append(bus.StreamCarriedSubjects(), extra...),
		Retention: natsgo.LimitsPolicy,
		Discard:   natsgo.DiscardNew,
		Storage:   natsgo.FileStorage,
		// The generation key is spelled out, as every build reads it: bus keeps its own unexported.
		Metadata: map[string]string{bus.StreamIDMetadataKey: id.New(), "dispatch.stream-generation": "1000000"},
	}); err != nil {
		t.Fatalf("the newer build creates %s: %v", bus.Stream, err)
	}
}
