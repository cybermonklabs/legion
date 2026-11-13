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
)

// A topic under a subject only a newer build's stream carries (notifications.x.>, which this build
// does not compile) is registered through this build's listener with no warning, and the session's
// consumer filters on it, so a message stored on it while the two builds run is the consumer's to
// deliver. The registration route and the consumer filter judge the one deployed stream: a route
// that stored the topic and a filter that left it out would answer the session 200 for a topic it
// is never delivered. A topic no stream captures is still left out of the registration with the
// warning, and out of the filter.
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
	if want := []string{contracts.UncapturedTopicRefusal(uncaptured, sessionID)}; !slices.Equal(answer.Warnings, want) {
		t.Fatalf("warnings = %q, want only %q: %s is captured by the deployed stream and %s by no stream", answer.Warnings, want, captured, uncaptured)
	}
	if !slices.Contains(answer.Topics, captured) || slices.Contains(answer.Topics, uncaptured) {
		t.Fatalf("answered topics = %v, want %s among them and %s not", answer.Topics, captured, uncaptured)
	}
	want := []string{contracts.AgentSubject(sessionID), captured}
	if got := rig.filters(t, sessionID); !slices.Equal(got, want) {
		t.Fatalf("consumer filters %v, want %v: the filter must carry the topic the registration stored, and leave out the one it did not", got, want)
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
	if harness.config.dedupeCache.Seen(item.DedupeKey, sessionID) || harness.config.attemptCache.Seen(item.DedupeKey, sessionID) {
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
