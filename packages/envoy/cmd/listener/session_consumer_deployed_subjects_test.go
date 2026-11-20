package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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
	want := []string{"notifications.agent.*." + sessionID, contracts.LegacyAgentSubject(sessionID), captured}
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
//
// The row hands the message to the fan-out through the core handler, which no production path
// does for such a topic today: the listener's own durable filters on the subjects this build
// compiles (durable.go, startListenerSubscription, bus.StreamSubjects), so on an older build a
// topic only a newer build's subjects capture never reaches the fan-out, and the session's
// consumer filter is what delivers it during a roll. The row stands for a fan-out that does see
// such a message, and holds that it judges it as the consumer filter does.
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

// A delivery exception on a lane this build stores is stored under its one-copy message id also
// while the listener's last read of the deployed stream's subjects left that lane out: the stream
// may have captured it again since that read, and the publish takes JetStream all the same, since
// this build stores the lane (bus.Client.UsesCoreTransport). Judged on the deployed subjects
// alone, the lane would be published under each exception's own dedupe key, and every listener's
// report of the one stored message would be kept.
//
// The test decides when the listener reads the deployed subjects again
// (bus.WithStreamSubjectsReads): not before both exceptions are published, so a scheduling delay
// cannot let the listener's own minute-long refresh read the lane back and leave the case
// untested. Once the copies are counted it drives one read, and the listener then sees the lane,
// so the read it held back is the one the listener makes.
func TestADeliveryExceptionOnALaneThisBuildStoresIsStoredOnceWhateverTheListenerLastRead(t *testing.T) {
	const (
		reason = "delivery_failed"
		origin = "origin-1"
	)
	topic := contracts.LegacyAgentSubject("ses_stale_exception")
	lane := bus.ExceptionTopicPrefix + topic
	if !bus.StreamCaptures(lane) {
		t.Fatalf("this build compiles no subject capturing %s; the test needs one it does", lane)
	}
	var dropped, restored []string
	for _, carried := range bus.StreamCarriedSubjects() {
		if bus.SubjectContains(carried, lane) {
			restored = append(restored, carried)
			continue
		}
		dropped = append(dropped, carried)
	}
	url := testnats.URL(t)
	deployNewerBuildsStreamOf(t, url, dropped)
	reads := make(chan time.Time)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1), bus.WithStreamSubjectsReads(reads))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	// The newer build puts the lane back after the listener read the subjects.
	js := client.JS()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read %s: %v", bus.Stream, err)
	}
	config := info.Config
	config.Subjects = append(config.Subjects, restored...)
	if _, err := js.UpdateStream(&config); err != nil {
		t.Fatalf("the newer build puts %v back on %s: %v", restored, bus.Stream, err)
	}
	item := listenerTestEnvelope(topic, "exception-stale-lane")
	// Two listeners raise the exception for the one stored message, each from its own durable.
	for range 2 {
		if err := publishDeliveryException(client, item, reason, "ses_stale_exception", origin); err != nil {
			t.Fatalf("publish the delivery exception: %v", err)
		}
	}
	if client.DeployedStreamCaptures(lane) {
		t.Fatalf("the listener read the subjects again before the test sent a read, so it no longer covers a read that left %s out", lane)
	}
	info, err = js.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: lane})
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
	select {
	case reads <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the listener took no read of the deployed subjects in 10 s")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !client.DeployedStreamCaptures(lane) {
		if time.Now().After(deadline) {
			t.Fatalf("the listener still reads the deployed stream without %s 10 s after the read the test sent", lane)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A send is answered 200 only once the notifications stream, the one stream the session consumers
// read, holds the message, whatever state the recipient is in and whatever the deployed stream
// captures. Each recipient state the route answers - stopped with an entry whose relay subject
// nothing holds, stopped with only a known-session record, connected through an entry that records
// no consumer delivery, and connected through a probe the session's relay subscriber answers - is
// sent to over three deployed streams, each a newer build's, which this build leaves as it is:
//
//   - the negative control, which captures the session's subject: the 200 with the recipient's
//     state and one copy stored, so a refusal in the rows after it is the stream's doing and not
//     the rig's;
//   - one that captures no subject of the session's: the send is refused with the bus's 503 and
//     nothing is stored, so the sender keeps the message to send again;
//   - the same, beside another stream that captures the session's subject: that stream's
//     acknowledgement is of a copy no consumer reads, so the send is refused all the same and
//     neither stream holds a copy.
//
// A stopped answer tells the sender the message is kept for the session and a connected one that
// the session's consumer has it, so a 200 over a publish the notifications stream did not store
// would lose the message under an answer that says it was kept.
func TestASendIsAnsweredOnlyOnceTheStreamHoldsIt(t *testing.T) {
	const target = "ses_deployed_stream_drops_agent"
	subject := contracts.LegacyAgentSubject(target)
	if !bus.StreamCaptures(subject) {
		t.Fatalf("this build compiles no subject capturing %s; the test needs one it does", subject)
	}
	var dropped []string
	for _, carried := range bus.StreamCarriedSubjects() {
		if !bus.SubjectContains(carried, subject) {
			dropped = append(dropped, carried)
		}
	}
	if len(dropped) == len(bus.StreamCarriedSubjects()) {
		t.Fatalf("no subject this build carries captures %s, so the stream that drops it drops nothing", subject)
	}
	recipients := []struct {
		name  string
		state string
		setup func(t *testing.T, rig *knownSessionRig)
	}{
		{"stopped with an entry", sessionStateStopped, func(t *testing.T, rig *knownSessionRig) {
			// An entry recording consumer delivery whose relay subject nothing holds, so the probe
			// reads the session stopped.
			if err := rig.deps.sessions.Put(target, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Delivery: session.DeliveryConsumer}); err != nil {
				t.Fatalf("write the session entry of %s: %v", target, err)
			}
		}},
		{"stopped with only a known-session record", sessionStateStopped, func(t *testing.T, rig *knownSessionRig) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := rig.known.RecordWithin(ctx, session.KnownSession{SessionID: target, Title: "a stopped session"}); err != nil {
				t.Fatalf("write the known-session record of %s: %v", target, err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, ok := rig.known.Get(target); ok {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("the known-session store never read the record of %s", target)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}},
		{"connected through an entry without consumer delivery", sessionStateConnected, func(t *testing.T, rig *knownSessionRig) {
			storeSessionHeartbeat(t, rig.harness.client, target, time.Now())
		}},
		{"connected through a probe its relay subscriber answers", sessionStateConnected, func(t *testing.T, rig *knownSessionRig) {
			if err := rig.deps.sessions.Put(target, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Delivery: session.DeliveryConsumer}); err != nil {
				t.Fatalf("write the session entry of %s: %v", target, err)
			}
			startSessionStandIn(t, rig.url, target, answerDone)
		}},
	}
	streams := []struct {
		name     string
		subjects []string
		// another names the subjects a stream other than the notifications stream captures.
		another []string
		status  int
		stored  uint64
	}{
		{"the deployed stream captures the subject", bus.StreamCarriedSubjects(), nil, http.StatusOK, 1},
		{"the deployed stream captures no subject of it", dropped, nil, http.StatusServiceUnavailable, 0},
		{"another stream captures the subject the deployed stream dropped", dropped, []string{contracts.AgentTopicPrefix + ">"}, http.StatusServiceUnavailable, 0},
	}
	for _, who := range recipients {
		for _, stream := range streams {
			t.Run(who.name+"/"+stream.name, func(t *testing.T) {
				url := testnats.URL(t)
				deployNewerBuildsStreamOf(t, url, stream.subjects)
				if stream.another != nil {
					deployAnotherStream(t, url, stream.another)
				}
				client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
				if err != nil {
					t.Fatalf("connect the listener: %v", err)
				}
				t.Cleanup(client.Close)
				if got, want := client.DeployedStreamCaptures(subject), stream.stored == 1; got != want {
					t.Fatalf("the listener reads the deployed stream as capturing %s: %t, want %t", subject, got, want)
				}
				rig := newKnownSessionRigOn(t, client, url)
				who.setup(t, rig)

				answer := rig.send(t, target, nil)

				stored := rig.storedOn(t, subject)
				elsewhere := uint64(0)
				if stream.another != nil {
					elsewhere = storedOnStream(t, client, anotherStream, subject)
				}
				t.Logf("status %d, %d copies on %s, %d in %s, after %s, body %s", answer.status, stored, subject, elsewhere, anotherStream, answer.elapsed, answer.raw)
				if answer.status == http.StatusOK && stored == 0 {
					t.Fatalf("the send was answered 200 (%s) and the notifications stream holds no copy on %s (%d in %s)", answer.raw, subject, elsewhere, anotherStream)
				}
				if answer.status != stream.status || stored != stream.stored || elsewhere != 0 {
					t.Fatalf("status %d, body %s, %d copies on %s and %d in %s; want %d, %d and 0", answer.status, answer.raw, stored, subject, elsewhere, anotherStream, stream.status, stream.stored)
				}
				if answer.elapsed > sendRequestBudget+500*time.Millisecond {
					t.Fatalf("the send was answered after %s, past its %s budget", answer.elapsed, sendRequestBudget)
				}
				if answer.status == http.StatusOK && answer.State != who.state {
					t.Fatalf("state %q, want %s", answer.State, who.state)
				}
				// The bus knows the deployed stream leaves the subject out, so the refusal says
				// so, naming the subject and the stream's generation, and does not read as a bus
				// that did not answer, which a monitor counts as an outage.
				if answer.status != http.StatusOK && (!strings.HasPrefix(answer.Error, streamLacksSubjectPrefix) ||
					!strings.Contains(answer.Error, subject) || !strings.Contains(answer.Error, "generation 1000000")) {
					t.Fatalf("the refusal reads %q, want it to open with %q and name %s and generation 1000000", answer.Error, streamLacksSubjectPrefix, subject)
				}
			})
		}
	}
}

// anotherStream is the stream deployAnotherStream creates: one other program of the deployment
// keeps, which no session consumer reads.
const anotherStream = "ANOTHER_PROGRAMS_STREAM"

// deployAnotherStream creates anotherStream on the server url names, capturing subjects, which the
// notifications stream must leave out: two streams of one account never capture one subject.
func deployAnotherStream(t *testing.T, url string, subjects []string) {
	t.Helper()
	conn := testnats.Connect(t, url)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream to create %s: %v", anotherStream, err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: anotherStream, Subjects: subjects, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create %s capturing %v: %v", anotherStream, subjects, err)
	}
}

// storedOnStream is how many messages stream holds on subject, read through client.
func storedOnStream(t *testing.T, client *bus.Client, stream, subject string) uint64 {
	t.Helper()
	info, err := client.JS().StreamInfo(stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read what %s holds on %s: %v", stream, subject, err)
	}
	return info.State.Subjects[subject]
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
// build leaves it: every subject this build carries and extra (deployNewerBuildsStreamOf).
func deployNewerBuildsStream(t *testing.T, url string, extra ...string) {
	t.Helper()
	deployNewerBuildsStreamOf(t, url, append(bus.StreamCarriedSubjects(), extra...))
}

// deployNewerBuildsStreamOf creates the notifications stream on the server url names as a newer
// build carrying subjects leaves it, under a generation above any this build stamps, so a start of
// this build that owns the stream leaves it as it is, a subject of this build's the newer one left
// out included. It carries the stream's identity, which a consumer registration answers with.
func deployNewerBuildsStreamOf(t *testing.T, url string, subjects []string) {
	t.Helper()
	conn := testnats.Connect(t, url)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream to create the newer build's stream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{
		Name:      bus.Stream,
		Subjects:  subjects,
		Retention: natsgo.LimitsPolicy,
		Discard:   natsgo.DiscardNew,
		Storage:   natsgo.FileStorage,
		// The generation key is spelled out, as every build reads it: bus keeps its own unexported.
		Metadata: map[string]string{bus.StreamIDMetadataKey: id.New(), "dispatch.stream-generation": "1000000"},
	}); err != nil {
		t.Fatalf("the newer build creates %s: %v", bus.Stream, err)
	}
}
