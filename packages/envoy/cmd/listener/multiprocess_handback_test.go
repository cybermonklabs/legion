//go:build multiprocess

// The hand-back rows of the two-pod suite (multiprocess_harness_test.go): what a graceful stop
// after a node loss, one during another pod's settle hold and one with messages on the wire must
// leave each session, and the SIGKILLs beside them. Its helpers set a row's pods up.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// mpTakeoverHolder waits until one of pods logs that it took the machine's expired lease, and
// returns that pod and the other one.
func mpTakeoverHolder(t *testing.T, within time.Duration, first, second *mpPod) (holder, other *mpPod) {
	t.Helper()
	mpEventually(t, within, "a standby to take the expired lease", func() bool {
		for _, pod := range []*mpPod{first, second} {
			if strings.Contains(pod.Output.String(), `"from":"expired"`) {
				holder = pod
			}
		}
		return holder != nil
	})
	if holder == first {
		return first, second
	}
	return second, first
}

// A lost node, then a graceful stop of the pod that took its expired lease, with a second standby
// running (a rolling update, or a rescheduled pod, after a node loss). The server keeps the lost
// pod's connection, and with it the durable's push binding, until its ping timeout, minutes in
// production; the second standby takes the released lease and must deliver all the same: every
// message published since the node was lost reaches its session, and every message published after
// the SIGTERM within 10 s of it. A standby that waited for the lost pod's binding to end instead
// would log "durable is push-bound by a subscriber without the lease; waiting for its binding to
// end" and deliver nothing until that connection goes (3 min 42 s against a real nats-server
// 2.10.29), while the server redelivers each message to that connection every ack wait until
// MaxDeliver is spent, so a message published meanwhile is never delivered at all.
func TestTwoPodsGracefulStopAfterANodeLossHandsOverWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_lossstop_one", "ses_mp_lossstop_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	serving.awaitAll(t, publisher.since(frozen), mpHardBound+5*time.Second)

	signalled := holder.Terminate(t)
	holder.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	serving.awaitAll(t, publisher.since(frozen), mpGracefulBound+5*time.Second)
	serving.requireWithin(t, publisher.since(signalled), mpGracefulBound, signalled)
	mpWaitActive(t, other, time.Second)
}

// A lost node, then, while the pod that took its expired lease still holds what it received for
// one ack wait, a graceful stop of that pod, with a second standby running. The lost pod is gone
// for good by then (killed, its connection closed), so no push binding outlives it. The messages
// the server pushed to the lost pod come back over one ack wait, after newer ones, some to the
// stopping holder and the rest to the second standby, which takes the released lease: each
// session must still take its messages in the order they were published, as after a hard
// take-over alone. A stopping holder that NAKed what it holds in the order it received it, with
// the second standby delivering at once and without the hold, would let messages published
// before the take-over reach their session after newer ones.
func TestTwoPodsGracefulStopDuringAHardTakeoversHoldKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_holdstop_one", "ses_mp_holdstop_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	holder, other := mpTakeoverHolder(t, mpHardBound, b, c)
	a.kill(t)
	a.relay.close()             // the lost pod's connection goes, so the second standby finds no binding
	time.Sleep(2 * time.Second) // inside the new holder's hold of one ack wait and a second
	holder.Terminate(t)
	holder.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireKnown(t, messages)
	serving.requireOrder(t, messages)
	mpWaitActive(t, other, time.Second)
}

// A graceful stop of the active pod while messages are on the wire to it: its link delivers
// 300 ms late, so when it unsubscribes the server has already pushed it the messages of the last
// 300 ms. Per-session order must still hold across the hand-over (README "Take-over": the stopping
// pod NAKs what it has not handled, and the durable redelivers those before newer messages). A
// stopping pod that dropped what reaches it after its unsubscribe without a NAK would let the
// standby deliver newer messages at once and the dropped ones come back one ack wait (6 s)
// later, behind them. Without the delay the same happens under load: at a message every 2 ms one
// message in about three stops arrived 6 s late and out of order.
func TestTwoPodsGracefulStopWithMessagesOnTheWireKeepsOrder(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_wirestop_one", "ses_mp_wirestop_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	a.relay.delay(300 * time.Millisecond)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(50*time.Millisecond, sessions...)
	time.Sleep(3 * time.Second)
	a.Terminate(t)
	a.WaitExit(t, "SIGTERM")
	time.Sleep(4 * time.Second) // publish on past the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpGracefulBound+5*time.Second)
	serving.requireKnown(t, messages)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOrder(t, messages)
	mpWaitActive(t, b, time.Second)
}

// SIGKILL of the active pod while a frame to session T is in flight, then more messages to T. The
// standby takes over: T must take every message, the one in flight at the kill included, in the
// order they were published and within the hard bound (architect Q2: in order within 15 s; a
// repeat with the same id is allowed). TestTwoPodsSIGKILLHandsOverInOrderWithin15s leaves the
// message in flight out of what it checks. The new holder must take the killed pod's ledger
// attempt for that message over at once: deferred as another pod's live attempt, it would reach
// T 35 s after the kill, after every newer message. The killed pod is the lease holder whose
// expired record the new holder took, so its attempt is no live pod's.
func TestTwoPodsSIGKILLMidFrameKeepsTheSessionsOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const blocked = "ses_mp_midframe"
	a.registerNATS(t, serving, blocked)
	publisher := mpConnectPublisher(t, uri)

	inHand := serving.hold(blocked)
	first := publisher.mustPublish(t, blocked)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never sent %s a frame", a.instance, blocked)
	}
	killed := a.kill(t)
	// The standby's restart at the ack floor after the kill recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, blocked)
	stop := publisher.every(200*time.Millisecond, blocked)
	time.Sleep(3 * time.Second) // publish for a while after the kill
	stop()

	messages := append([]mpMessage{first}, publisher.since(killed)...)
	serving.awaitAll(t, messages, mpHardBound+30*time.Second)
	serving.requireOrder(t, messages)
	serving.requireWithin(t, messages, mpHardBound, killed)
	mpWaitActive(t, b, time.Second)
}

// mpParkedMessages is how many messages mpRetryOutstanding leaves outstanding on the machine's
// durable, each NAKed for durableRetryDelay (30 s), far past every hold these rows measure.
const mpParkedMessages = 4

// mpParkedQuiet is how long mpRetryOutstanding reads the durable after the NAKs and requires it to
// deliver nothing: the 30 s of durableRetryDelay measured on the server rather than quoted.
const mpParkedQuiet = 5 * time.Second

// mpRetryOutstanding registers on pod a two healthy sessions, named after row, and leaves the
// machine's durable holding messages no pod of it can settle, by the route production reaches that
// state on: the backlog. An outage of the pod receiving the durable leaves messages unacknowledged
// on it, and any registration of their session after them, at its real resume point or fresh,
// gives it a consumer starting after them. The session follows a stored topic and is live, but its
// consumer cannot carry those messages (session.Resolve's "one whose consumer cannot carry this
// message", internal/session/session.go), so when the durable delivers them again the fan-out's
// resolution of that live session fails, which the durable path NAKs for durableRetryDelay (30 s;
// fanoutDelivery). Those messages are in no pod's hands over a hand-over and come back only after
// those 30 s, so the pod taking delivery over never finds every outstanding message in hand,
// however long the hand-over takes. The old mechanism, a registration that asked for no consumer
// at all, is one the listener refuses since #2603 (2af83d53fc: sessionDelivery.consumerFor
// answers it 409 not_requested), and this is the other state session.Resolve still parks a retry
// for.
//
// The outage is a's link frozen (listenerRelay.freeze) while the parked messages are published
// and the durable pushes them to a, then dropped: what the durable pushed goes with that
// connection, unacknowledged, and comes back only at the durable's ack wait (consumerAckWait,
// 6 s). The session registers through a once a has reconnected, inside that ack wait, with the
// resume point a correct client holds: the stream's identity and the last parked sequence. The
// freeze alone cannot be ordered against the registration, since every connection of a goes
// through the one relay: the registration's own writes would wait for the thaw, behind the
// durable's push, which a would then fan out to no registered interest and ACK.
//
// It requires the retry to be outstanding before it returns: pod a answered the parked
// registration with a consumer starting just after the last parked message, logged a failed
// delivery to the parked session for every parked message, the fan-out's failure that it NAKs for
// durableRetryDelay, and counted at least that many NAKs (envoy_messages_naked_total); and the
// durable, read directly, still holds every parked message and delivers nothing for mpParkedQuiet.
//
// A session with no record at all, which the unplaceable branch defers too
// (unplaceable_delivery.go), is no use here: its NAK comes back after unplaceableRetryDelay
// (5 s), inside the holds these rows measure, and a redelivery that lands during a hold is in
// that pod's hands and ends the hold early.
//
// It returns the row's sessions, the publisher and the healthy session ids.
func mpRetryOutstanding(t *testing.T, uri string, a *mpPod, row string) (serving *mpNATSSession, publisher *mpPublisher, healthy []string) {
	t.Helper()
	serving = mpStartNATSSessions(t, uri)
	healthy = []string{"ses_mp_" + row + "_one", "ses_mp_" + row + "_two"}
	for _, sessionID := range healthy {
		a.registerNATS(t, serving, sessionID)
	}
	publisher = mpConnectPublisher(t, uri)
	js := publisher.client.JS()
	parked := "ses_mp_" + row + "_parked"
	parkedTopic := "notifications.github.acme.mp-parked." + row
	stream, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read %s: %v", bus.Stream, err)
	}
	streamID := stream.Config.Metadata[bus.StreamIDMetadataKey]
	if streamID == "" {
		t.Fatalf("%s carries no %s", bus.Stream, bus.StreamIDMetadataKey)
	}

	// The outage: the parked messages are pushed to a's frozen link, and go with it.
	var thaw func()
	publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	for range mpParkedMessages {
		if _, err := publisher.publishOn(mpTarget{topic: parkedTopic, session: parked}); err != nil {
			t.Fatal(err)
		}
	}
	last, err := js.GetLastMsg(bus.Stream, parkedTopic)
	if err != nil {
		t.Fatalf("read the last parked message on %s: %v", parkedTopic, err)
	}
	lastParked := last.Sequence
	mpEventually(t, 10*time.Second, "the machine's durable to push every parked message to "+a.instance+"'s frozen link", func() bool {
		info, err := js.ConsumerInfo(bus.Stream, publisher.consumer)
		return err == nil && info.Delivered.Stream >= lastParked && info.NumAckPending >= mpParkedMessages
	})
	// a is back once each connection the registration and the fan-out need has reconnected and
	// each cache that drop left stale has been read again: the bus logs a connection's name in
	// Go's text format, quoted when it holds a space.
	named := func(connection string) string {
		if strings.Contains(connection, " ") {
			return "name=" + strconv.Quote(connection) + " "
		}
		return "name=" + connection + " "
	}
	back := [][]string{
		{"envoy nats reconnected", named(mpReceiveConnection(a.instance))},
		{"envoy nats reconnected", named(mpSessionConsumerConnection)},
		{`"msg":"interest registry cache warm-up"`, `"outcome":"completed"`},
		{`"msg":"session registry cache warm-up"`, `"outcome":"completed"`},
	}
	count := func(parts []string) int { return linesWith(a.Output.String(), parts...) }
	before := make([]int, len(back))
	for index, parts := range back {
		before[index] = count(parts)
	}
	dropped := time.Now()
	a.relay.drop()
	thaw()
	mpEventually(t, 10*time.Second, a.instance+" to reconnect and read its interest and session caches again", func() bool {
		for index, parts := range back {
			if count(parts) <= before[index] {
				return false
			}
		}
		return true
	})

	registration := fmt.Sprintf(`{"session_id":%q,"topics":[%q],"host":"127.0.0.1","delivery":{"consumer":true,"epoch":%q,"resume":{"stream_id":%q,"seq":%d}}}`,
		parked, parkedTopic, "mp-"+parked, streamID, lastParked)
	answer := a.call(http.MethodPost, "/v1/interests/subscribe", registration)
	if answer.err != nil || answer.status != http.StatusOK {
		t.Fatalf("subscribe %s through %s: status %d %s %v", registration, a.instance, answer.status, answer.body, answer.err)
	}
	registeredAt := time.Now()
	var registered struct {
		Delivery struct {
			ConsumerStart uint64 `json:"consumer_start"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(answer.body), &registered); err != nil {
		t.Fatalf("decode %s's registration answer %s: %v", parked, answer.body, err)
	}
	if registered.Delivery.ConsumerStart != lastParked+1 {
		t.Fatalf("%s gave %s a consumer starting at %d, want %d, just after the last parked message: %s",
			a.instance, parked, registered.Delivery.ConsumerStart, lastParked+1, answer.body)
	}
	t.Logf("%s registered at its resume point %d, %s after the outage, consumer start %d",
		parked, lastParked, registeredAt.Sub(dropped).Round(time.Millisecond), registered.Delivery.ConsumerStart)
	if line := lineWith(a.Output.String(), "listener no matching interests", parkedTopic); line != "" {
		t.Fatalf("%s took a parked message off the durable before %s registered: %s", a.instance, parked, line)
	}
	mpEventually(t, 30*time.Second, a.instance+" to defer every parked message for its retry delay", func() bool {
		return strings.Count(a.Output.String(), `"session_id":"`+parked+`"`) >= mpParkedMessages
	})
	failed := `"msg":"listener delivery failed"`
	mpEventually(t, 30*time.Second, a.instance+" to NAK every parked message for durableRetryDelay", func() bool {
		failures := linesWith(a.Output.String(), failed, `"session_id":"`+parked+`"`)
		return failures >= mpParkedMessages &&
			metricValue(t, &listenerProcess{Process: a.Process, port: a.port}, "envoy_messages_naked_total") >= mpParkedMessages
	})

	// The direct reading: the durable still holds every parked message, and delivers none again,
	// for mpParkedQuiet after the NAKs. Delivered.Consumer counts every delivery the durable makes,
	// a redelivery included, and nothing else is published meanwhile.
	nakedAt := time.Now()
	var first *natsgo.ConsumerInfo
	for {
		info, err := js.ConsumerInfo(bus.Stream, publisher.consumer)
		if err != nil {
			t.Fatalf("read %s: %v", publisher.consumer, err)
		}
		at := time.Now()
		lastDelivery := "never"
		if info.Delivered.Last != nil {
			lastDelivery = info.Delivered.Last.Format("15:04:05.000")
		}
		t.Logf("%s at %s (+%s after the NAKs): delivered %d (stream %d, last at %s), ack floor %d (stream %d), ack pending %d, redelivered %d",
			publisher.consumer, at.Format("15:04:05.000"), at.Sub(nakedAt).Round(time.Millisecond), info.Delivered.Consumer, info.Delivered.Stream,
			lastDelivery, info.AckFloor.Consumer, info.AckFloor.Stream, info.NumAckPending, info.NumRedelivered)
		if info.NumAckPending < mpParkedMessages {
			t.Fatalf("%s holds %d messages unacknowledged, want at least the %d parked", publisher.consumer, info.NumAckPending, mpParkedMessages)
		}
		if first == nil {
			first = info
		} else if info.Delivered.Consumer != first.Delivered.Consumer || info.NumRedelivered != first.NumRedelivered {
			t.Fatalf("%s delivered again within %s of the NAKs: delivered %d, redelivered %d, from %d, %d",
				publisher.consumer, at.Sub(nakedAt).Round(time.Millisecond), info.Delivered.Consumer, info.NumRedelivered,
				first.Delivered.Consumer, first.NumRedelivered)
		}
		if at.Sub(nakedAt) >= mpParkedQuiet {
			break
		}
		time.Sleep(time.Second)
	}
	return serving, publisher, healthy
}

// mpTo is the messages published to the sessions given.
func mpTo(messages []mpMessage, sessions ...string) []mpMessage {
	var to []mpMessage
	for _, m := range messages {
		if slices.Contains(sessions, m.session) {
			to = append(to, m)
		}
	}
	return to
}

// SIGKILL of the active pod while messages a session refuses are
// outstanding on the machine's durable (mpRetryOutstanding), with a message published every
// 100 ms to two healthy sessions. The standby takes the expired lease and its settle hold never
// finds every outstanding message in hand: it ends at what the killed pod can still have
// outstanding, one 6 s ack wait and a second after the subscribe, and every message to a healthy
// session published after the kill reaches it within 15 s, each session's in order. A hold that
// ran to its 13 s cap instead would let those messages arrive about 19 s after the kill.
func TestTwoPodsSIGKILLWithARetryOutstandingHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving, publisher, healthy := mpRetryOutstanding(t, uri, a, "retrykill")

	stop := publisher.every(100*time.Millisecond, healthy...)
	mpEventually(t, 10*time.Second, "the first healthy messages to arrive", func() bool {
		sent := mpTo(publisher.all(), healthy...)
		return len(sent) >= 10 && serving.tookAll(sent[:10])
	})
	killed := a.kill(t)
	time.Sleep(3 * time.Second) // publish on after the kill
	stop()

	messages := mpTo(publisher.all(), healthy...)
	serving.awaitAll(t, messages, mpHardBound+10*time.Second)
	serving.requireWithin(t, mpTo(publisher.since(killed), healthy...), mpHardBound, killed)
	serving.requireOrder(t, messages)
	serving.requireKnown(t, publisher.all())
	mpWaitActive(t, b, time.Second)
}
