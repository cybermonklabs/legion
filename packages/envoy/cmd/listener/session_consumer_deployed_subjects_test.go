package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// Judged on the subjects the client held, the first exception went over core NATS
// (bus.Client.PublishAckOn, which reads no subjects), and the stream stored it under no message
// id; no read was made, so the second was judged on the same subjects and stored the same way:
// two copies.
func TestADeliveryExceptionOnALaneAddedSinceTheLastReadIsStoredOnce(t *testing.T) {
	t.Parallel()
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

// A delivery exception about a stored message is published on the one judgement of its lane that
// publishDeliveryException makes (exceptionTakesJetStream): a judgement that answers core NATS has
// it sent over core NATS (bus.Client.PublishAckOn), even where the client's subjects come to
// capture the lane between that judgement and the publish. Here a newer build's subject that
// captures the lane is added after the client read the stream's subjects, and the client's read of
// them again is in flight past 2 s: another publish of the lane started it while the client's own
// lock was held (its CoreSubOK waits for nats.go's connection lock, which
// testnats.HandshakeHold.HoldWriteLock holds), and the read waits for that lock before its request
// goes out, so its own 2 s bound has not begun. The exception's judgement joins that read and,
// when exceptionLaneJudgement (2 s) ends, answers core on the subjects the client holds. The locks
// are let go after that, and the read lands and is kept: the client's subjects capture the lane. The
// stream stores the exception, which it captures, under no Nats-Msg-Id, as it stores every core
// publish.
//
// Control: with client.Publish(exception) in place of the core publish, Publish judges the lane
// again (bus.Client.PublishTakesJetStream), joins the same read, takes JetStream once it lands, and
// the stream stores the exception under a message id of its own dedupe key
// (bus.DeliveryExceptionDedupePrefix): neither the core publish the one judgement chose nor the
// one-copy exception.<reason>.<origin> another listener's JetStream judgement stores it under.
// It must not run in parallel: its 2 s judgement must end before the locks go, 3 s after it began.
func TestADeliveryExceptionIsPublishedOnTheOneJudgementOfItsLane(t *testing.T) {
	const (
		topic  = "notifications.x.judged"
		lane   = bus.ExceptionTopicPrefix + topic
		added  = bus.ExceptionTopicPrefix + "notifications.x.>"
		reason = "delivery_failed"
		origin = "origin-judged"
	)
	if bus.StreamCaptures(lane) {
		t.Fatalf("this build compiles a subject capturing %s; the test needs one it does not", lane)
	}
	url := testnats.URL(t)
	deployNewerBuildsStream(t, url)
	hold := testnats.StartHandshakeHold(t, url)
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open the row's JetStream: %v", err)
	}
	addStreamSubject(t, js, added)
	if client.DeployedStreamCaptures(lane) {
		t.Fatalf("the listener judged %s captured before reading the stream's subjects again; the row needs a stale read", lane)
	}

	release := hold.HoldWriteLock(t, client.Connection())
	go client.CoreSubOK()
	awaitClientLockHeld(t, client)
	// waits is the transport of a publish of the lane under a 50 ms deadline
	// (bus.Client.PublishTakesJetStream), and whether it waited that deadline out: it reads the
	// subjects again when no read is in flight, joins a read in flight once the read has outlasted
	// 2 s, and answers at once on the subjects held while the read is younger.
	waits := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		client.PublishTakesJetStream(ctx, lane)
		return ctx.Err() != nil
	}
	// Another publish of the lane reads the subjects again, and that read waits for the client's
	// lock; the transports after it answer at once until the read has outlasted 2 s.
	read := time.Now()
	if !waits() {
		t.Fatal("the first publish's transport answered within its 50 ms deadline: it started no read of the subjects, or the read it started was answered under the held locks")
	}
	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the read of the stream's subjects in flight to outlast 2 s", func() bool {
		return time.Since(read) > exceptionLaneJudgement && waits()
	})

	item := listenerTestEnvelope(topic, "exception-judged-lane")
	published := make(chan error, 1)
	began := time.Now()
	go func() { published <- publishDeliveryException(client, item, reason, "ses_judged_exception", origin) }()
	// The judgement's 2 s end first; then the locks are let go, and the read lands.
	time.Sleep(time.Until(began.Add(exceptionLaneJudgement + time.Second)))
	release()
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("publish the delivery exception: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the delivery exception was not published within 10 s of the locks' release")
	}
	testwait.Eventually(t, "the listener to keep the read of the subjects that landed after the judgement, capturing "+lane, func() bool {
		return client.DeployedStreamCaptures(lane)
	})
	requireStored(t, js, lane)
	stored, err := js.GetLastMsg(bus.Stream, lane)
	if err != nil {
		t.Fatalf("read the exception on %s: %v", lane, err)
	}
	if got := stored.Header.Get(natsgo.MsgIdHdr); got != "" {
		t.Fatalf("the stream stored the exception on %s under Nats-Msg-Id %q, want none: the publish took JetStream, not the core NATS its lane's one judgement answered (exception.%s.%s is that judgement's JetStream id, %s<id>:%s a second judgement's in Publish)",
			lane, got, reason, origin, bus.DeliveryExceptionDedupePrefix, lane)
	}
}

// awaitClientLockHeld waits until client's own lock stays held: a read of its connection
// (bus.Client.Connection, which takes that lock) still waits 200 ms after it began, where every
// other hold of the lock lasts microseconds. A read left waiting ends once the lock is let go.
func awaitClientLockHeld(t *testing.T, client *bus.Client) {
	t.Helper()
	testwait.WithinPolling(t, 30*time.Second, testwait.Poll, "the client's lock to stay held", func() bool {
		read := make(chan struct{})
		go func() {
			client.Connection()
			close(read)
		}()
		select {
		case <-read:
			return false
		case <-time.After(200 * time.Millisecond):
			return true
		}
	})
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
