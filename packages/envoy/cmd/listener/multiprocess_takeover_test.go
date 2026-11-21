//go:build multiprocess

// The take-over rows of the two-pod suite (multiprocess_harness_test.go): which of a machine's two
// processes delivers across a reconnect, a short drop, a start together, a SIGKILL, a node loss, a
// graceful stop, a NATS restart and a grant that carries the listener state bucket only after a
// pod started, and what each answers meanwhile.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A reconnect in place: the active pod's link to NATS is cut for 7 s, past the lease's 5 s, then
// it reconnects to the same server, with a message published every 100 ms throughout. The standby
// takes over while the active pod is away, and once it is back only one of them delivers: every
// message reaches its session exactly once. Before INFRA-557 the standby binds the durable once
// the server drops the cut pod's interest, and the cut pod's nats.go sends its subscription again
// on the reconnect, so both deliver and messages arrive twice.
func TestTwoPodsReconnectInPlaceDeliversEachMessageOnce(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_reconnect_one", "ses_mp_reconnect_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	const outage = 7 * time.Second
	reconnects := mainClientReconnects(a.Output.String())
	cut := publisher.quiesce(t, serving, func() { a.relay.cut(outage) })
	mpEventually(t, outage+30*time.Second, a.instance+" to reconnect after the cut", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	time.Sleep(5 * time.Second) // keep publishing past the reconnect
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	actives := 0
	for _, pod := range []*mpPod{a, b} {
		if pod.active() {
			actives++
		}
	}
	if actives != 1 {
		t.Fatalf("%d pods report themselves active after the reconnect, want 1", actives)
	}
	watch.requireServed(t, cut)
	t.Logf("%d messages, each taken once across a %s cut of %s", len(messages), outage, a.instance)
}

// A short drop: the active pod's link is cut for 2 s, under the lease's 5 s. It reconnects and
// renews in time, so it stays the machine's active pod and the standby never delivers: every
// message reaches its session exactly once and within 10 s of its publish (a ledger call that
// failed while the pod's state connection reconnected once cost a message 30 s), and the standby
// never reports itself anything but a standby. Before INFRA-557 the standby may bind in the gap,
// and the reconnected pod's nats.go then subscribes beside it.
func TestTwoPodsShortDropKeepsTheActivePod(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_drop_one", "ses_mp_drop_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	var notStandby []string
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			if !b.standby() {
				_, body := b.health()
				notStandby = append(notStandby, fmt.Sprint(body))
			}
			select {
			case <-done:
				return
			case <-time.After(mpPoll):
			}
		}
	}()
	halt := sync.OnceFunc(func() { close(done); <-watched })
	t.Cleanup(halt)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	const outage = 2 * time.Second
	reconnects := mainClientReconnects(a.Output.String())
	cut := publisher.quiesce(t, serving, func() { a.relay.cut(outage) })
	mpEventually(t, outage+30*time.Second, a.instance+" to reconnect after the cut", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	reconnected := time.Since(cut)
	time.Sleep(5 * time.Second) // keep publishing past the reconnect
	stop()

	messages := publisher.all()
	stopped := time.Now()
	serving.awaitAll(t, messages, 30*time.Second)
	t.Logf("%s reconnected %s after the cut; the last of %d messages arrived %s after the last publish",
		a.instance, reconnected.Round(time.Millisecond), len(messages), time.Since(stopped).Round(time.Millisecond))
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	time.Sleep(mpQuiet)
	halt()
	serving.requireOnce(t, messages)
	if len(notStandby) > 0 {
		t.Fatalf("%s was not a standby in %d polls across %s's short drop: %v", b.instance, len(notStandby), a.instance, notStandby)
	}
	if strings.Contains(b.Output.String(), "durable receiving") {
		t.Fatalf("%s received the durable across %s's short drop", b.instance, a.instance)
	}
	if !a.active() {
		t.Fatalf("%s is not the active pod after its short drop", a.instance)
	}
}

// Two pods of the machine started at the same moment, 20 times, each time on a NATS account of its
// own: once both are ready exactly one reports itself active and the other a standby, and each
// message reaches its session exactly once. Before INFRA-557 nats.go's push-bound check is
// check-then-act, and two pods started together both bind the durable, so both report themselves
// healthy, the active pod's answer then (and both deliver). The row stops at the first start that
// fails.
func TestTwoPodsStartedTogetherElectOneActivePod(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for start := range 20 {
		passed := t.Run(fmt.Sprintf("start %d", start+1), func(t *testing.T) {
			uri := mpNATS(t)
			pods := []*mpPod{mpStartPod(t, binary, uri, "pod-a"), mpStartPod(t, binary, uri, "pod-b")}
			mpEventually(t, 30*time.Second, "both pods to be ready", func() bool {
				return (pods[0].active() || pods[0].standby()) && (pods[1].active() || pods[1].standby())
			})
			// Read again, a few times, so a second pod that took the lease a moment later shows.
			for range 4 {
				var actives []string
				for _, pod := range pods {
					if pod.active() {
						actives = append(actives, pod.instance)
					}
				}
				if len(actives) != 1 {
					t.Fatalf("%d pods report themselves active (%v), want exactly 1", len(actives), actives)
				}
				time.Sleep(mpPoll)
			}
			serving := mpStartNATSSessions(t, uri)
			const sessionID = "ses_mp_together"
			pods[1].registerNATS(t, serving, sessionID)
			publisher := mpConnectPublisher(t, uri)
			for range 5 {
				publisher.mustPublish(t, sessionID)
			}
			messages := publisher.all()
			serving.awaitAll(t, messages, 10*time.Second)
			time.Sleep(time.Second)
			serving.requireOnce(t, messages)
		})
		if !passed {
			break
		}
	}
}

// SIGKILL of the active pod while it has a message in hand, on a port session T whose frame never
// answers, and a second message to T queued behind it on T's own consumer. The standby takes over:
// both of T's messages and every message published after the kill reach their sessions within
// 15 s, each session's in the order they were published, and the standby lists the registered
// sessions throughout. Before INFRA-557 the durable's 60 s ack wait holds the queued message with
// the killed pod (and any message the server pushed to it as it died), so those come after the
// newer ones, and late. What waits behind the frame in hand is T's own consumer's since main's
// per-session consumers: another session's messages no longer queue behind it, and the standby's
// relay restarts that consumer at its ack floor, which is what brings both messages back at once
// rather than at the consumer's own 60 s ack wait.
func TestTwoPodsSIGKILLHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const blocked, queued, other = "ses_mp_kill_t", "ses_mp_kill_s", "ses_mp_kill_u"
	for _, sessionID := range []string{blocked, queued, other} {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, blocked, queued, other)
	publisher := mpConnectPublisher(t, uri)

	// The blocked session answers every frame `nak` until the kill, so the killed pod's relay
	// took nothing to it and the successor must deliver both, in stream order.
	serving.fail(blocked)
	first := publisher.mustPublish(t, blocked)
	mpEventually(t, 10*time.Second, a.instance+" to relay "+first.event+" to "+blocked, func() bool {
		return serving.failures(blocked) >= 1
	})
	behind := publisher.mustPublish(t, blocked)
	mpEventually(t, 10*time.Second, a.instance+" to hold the queued message too", func() bool {
		return publisher.sessionAckPending(blocked) >= 2
	})

	killed := a.kill(t)
	// The standby's restart at the ack floor after the kill recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, blocked)
	stop := publisher.every(100*time.Millisecond, queued, other)
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	serving.accept(blocked)     // the successor's frames are taken; the killed pod's never were
	time.Sleep(3 * time.Second) // publish for a while after the kill
	stop()

	messages := append([]mpMessage{first, behind}, publisher.since(killed)...)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, killed)
	serving.requireOrder(t, messages)
	time.Sleep(time.Second)
	serving.requireOnce(t, messages)
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, killed)
}

// A message stored while no relay of its session runs anywhere reaches that session within 10 s
// of its publish. The active pod is SIGKILLed, and the message is published after the kill and
// before the standby has taken the lease over: the killed pod's relay is gone and the
// successor's has not started, so the stream stores the message with nobody pulling for it. The
// session's consumer outlives both pods on the server, and the successor restarts it at its ack
// floor (bus.SessionConsumers.RestartAtFloor), which is below this message, so the successor
// relays it. This is the root's delete-listener requirement: a message sent while the pod
// serving a session is replaced is delivered, not lost. A successor that created the consumer
// afresh at the stream's end (DeliverNew), or restarted it at a start read before the publish,
// would never send it and the row would wait out its bound.
func TestTwoPodsAMessageStoredWhileNoRelayRunsReachesItsSessionWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_stored_without_relay"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)
	warm := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{warm}, 20*time.Second)

	a.kill(t)
	a.WaitExit(t, "SIGKILL")
	// The standby takes a record it has read unchanged for a full leaseTTL (5 s), so the publish
	// below lands in the gap where neither pod relays this session.
	if b.active() {
		t.Fatalf("%s had taken the lease over before the publish, so no message of this row is stored without a relay", b.instance)
	}
	m := publisher.mustPublish(t, sessionID)
	if b.active() {
		t.Fatalf("%s took the lease over while %s was being published, so the row proves nothing", b.instance, m.event)
	}

	serving.awaitAll(t, []mpMessage{m}, mpHardBound+15*time.Second)
	serving.requireWithin(t, []mpMessage{m}, mpGracefulBound, time.Time{})
	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{warm, m})
	serving.requireKnown(t, []mpMessage{warm, m})
}

// A session that refuses its own consumer's frames: a frame the session answers `nak` is
// delivered again by the server after relayNakDelay, so the relay of that consumer sends it
// again, and the pod that takes delivery over sends it again in its turn, because the message is
// its session's consumer's and no pod's memory. Across a graceful stop and then a SIGKILL of the
// pod holding it, each refused message reaches its session exactly once, within the take-over
// bound of the event that moved delivery: 10 s of the stop, 15 s of the kill. Before main's
// per-session consumers a refused message was the durable's to NAK and came back to the
// machine's durable, which is why the hand-back rows park their outstanding messages elsewhere
// (mpRetryOutstanding).
func TestTwoPodsARefusedFrameReachesItsSessionAcrossAStopAndAKill(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_port_retry"
	a.registerNATS(t, serving, sessionID)
	publisher := mpConnectPublisher(t, uri)

	serving.fail(sessionID)
	overTheStop := publisher.mustPublish(t, sessionID)
	mpEventually(t, 10*time.Second, a.instance+"'s push to be refused", func() bool { return serving.failures(sessionID) > 0 })
	// The standby's restart at the ack floor after the stop recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, sessionID)
	signalled := a.Terminate(t)
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, mpGracefulBound)
	serving.accept(sessionID)
	serving.awaitAll(t, []mpMessage{overTheStop}, mpGracefulBound+5*time.Second)
	serving.requireWithin(t, []mpMessage{overTheStop}, mpGracefulBound, signalled)

	// The next standby starts only now, so each take-over has one pod to go to.
	c := mpStartPod(t, binary, uri, "pod-c")
	mpWaitStandby(t, c, 30*time.Second)
	serving.fail(sessionID)
	refused := serving.failures(sessionID)
	overTheKill := publisher.mustPublish(t, sessionID)
	mpEventually(t, 10*time.Second, b.instance+"'s push to be refused", func() bool { return serving.failures(sessionID) > refused })
	killed := b.kill(t)
	// pod-c's restart at the ack floor after the kill recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, sessionID)
	mpWaitActive(t, c, mpHardBound)
	serving.accept(sessionID)
	serving.awaitAll(t, []mpMessage{overTheKill}, mpHardBound+5*time.Second)
	serving.requireWithin(t, []mpMessage{overTheKill}, mpHardBound, killed)

	time.Sleep(mpQuiet)
	serving.requireOnce(t, []mpMessage{overTheStop, overTheKill})
}

// Node loss: the active pod's link freezes, its sockets left open, so the server keeps its
// subscription as it does for a node that vanished. The messages published during the freeze
// reach their sessions through the standby within 15 s, each session's in order; once the link
// thaws, the old pod delivers none of them again. Before INFRA-557 the server keeps the frozen
// pod's binding for minutes and the standby cannot bind.
func TestTwoPodsNodeLossHandsOverInOrderWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_loss_one", "ses_mp_loss_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	var thaw func()
	frozen := publisher.quiesce(t, serving, func() { thaw = a.relay.freeze() })
	t.Cleanup(thaw)
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, mpHardBound, b.instance+" to take over", b.active)
	time.Sleep(2 * time.Second) // publish on past the take-over
	stop()

	messages := publisher.since(frozen)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, frozen)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)

	thaw()
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	watch.requireServed(t, frozen)
}

// The active pod's receive connection alone is lost: the relay freezes the connection its main
// client holds, the one the machine's durable is received on, in both directions and closes every
// reconnect it makes under that name, while the delivery lease's own connection - a second socket
// of the same pod, `envoy-listener-lease pod-a` - goes on renewing the lease record. A message is
// published every 100 ms from before the cut. The standby must take the lease over and deliver:
// every message reaches its session exactly once, in publish order, the first published after the
// cut within 15 s of it; the cut pod must hold the lease again at no poll of the 20 s that
// follow, while the standby keeps delivering; and once the cut heals the old pod must answer
// /healthz 200 role standby. Before this round the cut pod renews the record on its own
// connection for as long as that one is up - answering /healthz 503 `subscription inactive` and
// delivering nothing meanwhile - so no standby takes over until the pod's self-health monitor
// kills it, ten minutes on.
func TestTwoPodsReceiveConnectionLostHandsOverWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_recvlost_one", "ses_mp_recvlost_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, sessions...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	var heal func()
	cut := publisher.quiesce(t, serving, func() { heal = a.lanes.cut(mpReceiveConnection(a.instance)) })
	t.Cleanup(func() { heal() })

	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	taken := time.Since(cut)
	released := lineWith(a.Output.String(), `"msg":"listener lease released: the receive connection is down"`)
	if released == "" {
		t.Fatalf("%s released the lease without logging why:\n%s", a.instance, mpTail(mpLinesWithout(a.Output.String()), 40))
	}
	if lines := strings.Count(a.Output.String(), "listener lease released: the receive connection is down"); lines != 1 {
		t.Fatalf("%s logged the release of its lost receive connection %d times, want one line: %s", a.instance, lines, released)
	}
	t.Logf("%s logged %s", a.instance, released)
	// The cut pod takes the lease back at no poll while its receive connection is down, and the
	// standby holds the record and goes on delivering throughout.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(mpPoll) {
		record := publisher.stored(t, bus.ListenerStateBucket, bus.ListenerLeaseKey(mpMachine))
		if record["holder"] != b.instance || record["released"] == true {
			t.Fatalf("%s into the cut the lease record names %v (released %v), want %s holding it",
				time.Since(cut).Round(time.Millisecond), record["holder"], record["released"], b.instance)
		}
		if a.active() {
			_, body := a.health()
			t.Fatalf("%s into the cut %s reports itself active again: %v", time.Since(cut).Round(time.Millisecond), a.instance, body)
		}
	}
	stop()

	messages := publisher.since(cut)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, cut)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)
	first, _ := serving.first(sessions[0], messages[0].event)
	t.Logf("%s took the lease %s after the cut of %s's receive connection; the first message published after it reached its session %s after the cut, %d messages in all",
		b.instance, taken.Round(time.Millisecond), a.instance, first.Sub(cut).Round(time.Millisecond), len(messages))

	heal()
	mpWaitStandby(t, a, 30*time.Second)
	time.Sleep(mpQuiet)
	serving.requireOnce(t, messages)
	watch.requireServed(t, cut)
}

// The active pod's receive connection comes back inside the bound: the relay freezes it only
// until the pod's client has noticed the cut, then heals it, so nats.go reconnects on the same
// relay about a second later, inside leaseReceiveLost. The pod keeps the lease in the term it
// held across the cut, the standby never receives, and every message reaches its session once and
// within 10 s of its publish.
func TestTwoPodsReceiveConnectionBackInsideTheBoundKeepsTheLease(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_recvback_one", "ses_mp_recvback_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)
	acquisitions := strings.Count(a.Output.String(), `"msg":"listener lease acquired"`)
	reconnects := mainClientReconnects(a.Output.String())

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	heal := a.lanes.cut(mpReceiveConnection(a.instance))
	t.Cleanup(heal)
	cut := time.Now()
	mpEventually(t, 15*time.Second, a.instance+"'s client to notice the cut", func() bool {
		return strings.Contains(a.Output.String(), `envoy nats disconnected name="`+mpReceiveConnection(a.instance)+`"`)
	})
	noticed := time.Now()
	heal()
	mpEventually(t, 15*time.Second, a.instance+" to reconnect", func() bool {
		return mainClientReconnects(a.Output.String()) > reconnects
	})
	back := time.Since(noticed)
	time.Sleep(5 * time.Second) // publish on past the bound the holder would have released at
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, 30*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireOnce(t, messages)
	t.Logf("%s noticed the cut %s after it and was back %s later, inside the %s bound",
		a.instance, noticed.Sub(cut).Round(time.Millisecond), back.Round(time.Millisecond), leaseReceiveLost)
	if back >= leaseReceiveLost {
		t.Fatalf("%s was back %s after it noticed the cut, past the bound %s: the row tested nothing", a.instance, back, leaseReceiveLost)
	}
	if !a.active() {
		_, body := a.health()
		t.Fatalf("%s is not the active pod after a cut it reconnected inside the bound from: %v", a.instance, body)
	}
	if took := strings.Count(a.Output.String(), `"msg":"listener lease acquired"`); took != acquisitions {
		t.Fatalf("%s acquired the lease %d times across the cut, want the term it held throughout", a.instance, took-acquisitions)
	}
	if strings.Contains(a.Output.String(), "listener lease released: the receive connection is down") {
		t.Fatalf("%s released the lease for a receive connection that was back inside the bound", a.instance)
	}
	if strings.Contains(b.Output.String(), "durable receiving") {
		t.Fatalf("%s received the durable across %s's short receive cut", b.instance, a.instance)
	}
}

// A graceful stop: the active pod is in hand on a frame that never answers when it is sent
// SIGTERM, with a message published every 100 ms before and after. Every message reaches its
// session within 10 s of its publish; a message delivered twice is the same message, with its own
// event id. Before INFRA-557 the stopping pod keeps the durable bound for ~23 s, through its HTTP
// shutdown and its NATS drain, so the messages wait.
func TestTwoPodsGracefulStopDeliversWithin10s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const slow = "ses_mp_graceful_slow"
	sessions := []string{"ses_mp_graceful_one", "ses_mp_graceful_two"}
	for _, sessionID := range append([]string{slow}, sessions...) {
		a.registerNATS(t, serving, sessionID)
	}
	watch := mpWatchSessions(t, b, append([]string{slow}, sessions...)...)
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 10 })
	inHand := serving.hold(slow)
	publisher.mustPublish(t, slow)
	select {
	case <-inHand:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never sent %s a frame", a.instance, slow)
	}
	// The standby's restart at the ack floor after the stop recreates this consumer.
	awaitSessionConsumerStored(t, publisher.client.Conn, slow)
	signalled := a.Terminate(t)
	time.Sleep(6 * time.Second) // publish on through the hand-over
	stop()

	messages := publisher.all()
	serving.awaitAll(t, messages, mpGracefulBound+5*time.Second)
	serving.requireWithin(t, messages, mpGracefulBound, time.Time{})
	serving.requireKnown(t, messages)
	a.WaitExit(t, "SIGTERM")
	mpWaitActive(t, b, time.Second)
	watch.requireServed(t, signalled)
}

// A standby waits as long as the active pod runs: 140 s on, past the 135 s after which a standby
// once gave up and exited, it is still up, /healthz 200 role standby, and it lists the registered
// sessions in every poll; then a SIGTERM of the active pod hands delivery to it. Before INFRA-557
// the standby exits at 135 s.
func TestTwoPodsStandbyWaitsPast140s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	serving := mpStartNATSSessions(t, uri)
	const sessionID = "ses_mp_standby_waits"
	a.registerNATS(t, serving, sessionID)
	watch := mpWatchSessions(t, b, sessionID)
	started := time.Now()

	// The standby is checked by mpPod.standby here, which a build before the lease answers too, so
	// that build fails on its exit rather than on its /healthz; the role is checked after the wait.
	for time.Since(started) < 140*time.Second {
		select {
		case <-b.Exited:
			t.Fatalf("%s exited %s into its standby", b.instance, time.Since(started).Round(time.Second))
		case <-time.After(mpPoll):
		}
		if !b.standby() {
			status, body := b.health()
			t.Fatalf("%s %s into its standby is not a standby: /healthz %d %v", b.instance, time.Since(started).Round(time.Second), status, body)
		}
	}
	if status, body := b.health(); status != http.StatusOK || body["role"] != "standby" {
		t.Fatalf("%s's /healthz after %s as a standby: %d %v, want 200 role standby", b.instance, time.Since(started).Round(time.Second), status, body)
	}

	signalled := a.Terminate(t)
	mpWaitActive(t, b, mpGracefulBound)
	publisher := mpConnectPublisher(t, uri)
	m := publisher.mustPublish(t, sessionID)
	serving.awaitAll(t, []mpMessage{m}, mpGracefulBound)
	watch.requireServed(t, started)
	t.Logf("%s took over %s after %s's SIGTERM", b.instance, time.Since(signalled).Round(time.Millisecond), a.instance)
}

// Readiness: a listener started while its NATS is down answers /healthz 503 "starting" and stays
// up, past the ten one-second connect tries after which it once exited; once NATS is up it is
// 200 active within 10 s, and a second pod started then is 200 standby. Before INFRA-557 the
// listener answers 200 "starting" with nothing ready, then exits.
func TestTwoPodsReadinessWaitsForNATS(t *testing.T) {
	server, uri := mpStartNATS(t, testnats.ListenerGrant)
	server.Stop(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPod(t, binary, uri, "pod-a")
	mpEventually(t, 10*time.Second, a.instance+" to answer /healthz", func() bool { status, _ := a.health(); return status != 0 })
	for started := time.Now(); time.Since(started) < 15*time.Second; time.Sleep(mpPoll) {
		select {
		case <-a.Exited:
			t.Fatalf("%s exited while NATS was down", a.instance)
		default:
		}
		if status, body := a.health(); status != http.StatusServiceUnavailable || body["status"] != "starting" {
			t.Fatalf("%s's /healthz while NATS is down: %d %v, want 503 starting", a.instance, status, body)
		}
	}

	server.Start(t)
	upAt := time.Now()
	mpEventually(t, 10*time.Second, a.instance+" to be 200 active once NATS is up", func() bool {
		status, body := a.health()
		return status == http.StatusOK && body["role"] == "active"
	})
	t.Logf("%s was active %s after NATS came up", a.instance, time.Since(upAt).Round(time.Millisecond))
	b := mpStartPod(t, binary, uri, "pod-b")
	mpEventually(t, 30*time.Second, b.instance+" to be 200 standby", func() bool {
		status, body := b.health()
		return status == http.StatusOK && body["role"] == "standby"
	})
}

// The architect's contract for a message sent while the active pod is deleted, at the listener's
// API: the standby sits behind a 500 ms slow link, so its caches trail the bucket. While the active
// pod is stopped, by SIGTERM and by SIGKILL, sessions keep registering through it every 250 ms for
// as long as it answers, and each registered session is sent to through the standby every 250 ms,
// the first send at once, until the standby has taken over. Every send answers within 10 s, 2xx
// or a retryable 503 with Retry-After, and never 404 "no live session". The end-to-end resend is
// INFRA-562's. Before INFRA-557 the standby's cache has not seen the registration yet and answers
// 404 "no live session".
func TestTwoPodsSendThroughTheStandbyDuringATakeover(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, stopping := range []struct {
		name string
		stop func(t *testing.T, p *mpPod) time.Time
	}{
		{"SIGTERM", func(t *testing.T, p *mpPod) time.Time { return p.Terminate(t) }},
		{"SIGKILL", func(t *testing.T, p *mpPod) time.Time { return p.kill(t) }},
	} {
		t.Run(stopping.name, func(t *testing.T) {
			uri := mpNATS(t)
			a, b := mpStartPair(t, binary, uri)
			b.relay.delay(500 * time.Millisecond)

			type send struct {
				session string
				answer  mpAnswer
			}
			var (
				mu         sync.Mutex
				sends      []send
				registered []string
				senders    sync.WaitGroup
			)
			stopSending := make(chan struct{})
			sendTo := func(sessionID string) {
				defer senders.Done()
				body := fmt.Sprintf(`{"target_session":%q,"message":"send during the take-over","source":"agent","source_session":"ses_mp_sender"}`, sessionID)
				for {
					answer := b.call(http.MethodPost, "/v1/messages/send", body)
					mu.Lock()
					sends = append(sends, send{session: sessionID, answer: answer})
					mu.Unlock()
					select {
					case <-stopSending:
						return
					case <-time.After(mpPoll):
					}
				}
			}
			var refusal string
			register := func(index int) bool {
				sessionID := fmt.Sprintf("ses_mp_takeover_%03d", index)
				answer := a.call(http.MethodPost, "/v1/interests/subscribe", mpOnConsumer(t, fmt.Sprintf(`{"session_id":%q,"topics":[],"self_subscribed":true}`, sessionID)))
				if answer.err != nil || answer.status != http.StatusOK {
					refusal = fmt.Sprintf("%d %s %v", answer.status, strings.TrimSpace(answer.body), answer.err)
					return false
				}
				mu.Lock()
				registered = append(registered, sessionID)
				mu.Unlock()
				senders.Add(1)
				go sendTo(sessionID)
				return true
			}

			index := 0
			for ; index < 6; index++ {
				if !register(index) {
					t.Fatalf("%s refused a registration before it was stopped: %s", a.instance, refusal)
				}
				time.Sleep(mpPoll)
			}
			mu.Lock()
			watch := mpWatchSessions(t, b, slices.Clone(registered)...)
			mu.Unlock()
			stoppedAt := stopping.stop(t, a)
			after := 0
			for ; register(index); index++ {
				after++
				time.Sleep(mpPoll)
			}
			mpEventually(t, mpHardBound, b.instance+" to take over", b.active)
			time.Sleep(time.Second) // send on past the take-over
			close(stopSending)
			senders.Wait()
			watch.requireServed(t, stoppedAt)

			mu.Lock()
			defer mu.Unlock()
			retryable := 0
			for _, s := range sends {
				answer := s.answer
				switch {
				case answer.err != nil:
					t.Fatalf("send to %s through %s: %v after %s, want an answer within 10 s", s.session, b.instance, answer.err, answer.took)
				case answer.status == http.StatusNotFound:
					t.Fatalf("send to %s through %s: 404 %s", s.session, b.instance, answer.body)
				case answer.status == http.StatusServiceUnavailable && answer.header.Get("Retry-After") != "":
					retryable++
				case answer.status < 200 || answer.status > 299:
					t.Fatalf("send to %s through %s: %d %s, want 2xx or a 503 with Retry-After", s.session, b.instance, answer.status, answer.body)
				}
			}
			t.Logf("%d sessions registered through %s, %d of them after its %s, until it answered %s; %d sends through %s, %d answered a retryable 503, %s after the stop",
				len(registered), a.instance, after, stopping.name, refusal, len(sends), b.instance, retryable, time.Since(stoppedAt).Round(time.Millisecond))
		})
	}
}

// A standby across a NATS restart registers no JetStream subscription of the durable, so its
// client's recovery has nothing to resubscribe and logs no "envoy nats recovery resubscribe
// failed"; both pods come back, one active and one a standby. Before INFRA-557 the standby's
// pending bind is a subscription the recovery retries and logs that failure for.
func TestTwoPodsStandbyAcrossANATSRestartLogsNoResubscribeFailure(t *testing.T) {
	server, uri := mpStartNATS(t, testnats.ListenerGrant)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)

	disconnects := strings.Count(b.Output.String(), "envoy nats disconnected")
	server.Stop(t)
	mpEventually(t, 10*time.Second, b.instance+" to see NATS go", func() bool {
		return strings.Count(b.Output.String(), "envoy nats disconnected") > disconnects
	})
	server.Start(t)
	mpEventually(t, 30*time.Second, "one pod active and the other a standby after the restart", func() bool {
		return (a.active() && b.standby()) || (b.active() && a.standby())
	})
	// A recovery's first attempt runs at the reconnect and the next a second later: watch past both.
	const failure = "envoy nats recovery resubscribe failed"
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(mpPoll) {
		if line := lineWith(b.Output.String(), failure); line != "" {
			t.Fatalf("%s, the standby, logged across the NATS restart: %s", b.instance, line)
		}
	}
}

// Two pods of one machine can be told apart in their logs: every JSON record of each carries its
// own instance, and internal/bus's text lines, which keep Go's positional format, name each pod's
// NATS client. Before INFRA-557 the records carry only machine_id.
func TestTwoPodsLogTheirOwnInstance(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a, b := mpStartPair(t, binary, uri)
	// A dropped link has each pod's client log its disconnect and reconnect in text.
	a.relay.drop()
	b.relay.drop()
	for _, pod := range []*mpPod{a, b} {
		mpEventually(t, 10*time.Second, pod.instance+" to log its reconnect", func() bool {
			return mainClientReconnects(pod.Output.String()) > 0
		})
	}
	for _, pod := range []*mpPod{a, b} {
		records := 0
		for line := range strings.SplitSeq(pod.Output.String(), "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) != nil || record["msg"] == nil {
				continue
			}
			records++
			if record["instance"] != pod.instance {
				t.Fatalf("%s's record has instance %v, want %q: %s", pod.instance, record["instance"], pod.instance, line)
			}
		}
		if records == 0 {
			t.Fatalf("%s logged no JSON record", pod.instance)
		}
		textLine := regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} INFO envoy nats disconnected name="envoy-listener ` + regexp.QuoteMeta(pod.instance) + `"`)
		if !textLine.MatchString(pod.Output.String()) {
			t.Fatalf("%s's bus disconnect line is not date, time, message naming the client envoy-listener %s", pod.instance, pod.instance)
		}
	}
}

// The active pod's receive connection alone is lost to a route that drops its packets, so its
// reconnects are dials that hang as well: the relay freezes the main client's connection and holds
// every connection the pod opens from then on before its first byte, while the lease connection
// goes on renewing. nats.go holds the connection's lock across each reconnect dial and its connect
// init, up to its connect timeout every time, so a receive signal read through that connection's
// state waits for it. The holder must still release the lease on its own bound
// (leaseReceiveLost), logging why, and the standby must take it over and deliver every message in
// publish order, the first published after the cut within 15 s of it, as when the reconnects are
// refused (TestTwoPodsReceiveConnectionLostHandsOverWithin15s). At 580089b5 (the INFRA-557
// tester's round-18 finding) the lease loop blocks reading that state, its renewals lapse instead
// of its release landing, the standby takes the expired record about 8 s after the cut, and the
// first message after the cut reaches its session 13.9-16.0 s after it (2 of 7 runs past 15 s).
func TestTwoPodsReceiveConnectionLostToADroppingRouteReleasesTheLeaseWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_recvhole_one", "ses_mp_recvhole_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	var heal, unhole func()
	cut := publisher.quiesce(t, serving, func() {
		unhole = a.lanes.blackholeNew()
		heal = a.lanes.cut(mpReceiveConnection(a.instance))
	})
	t.Cleanup(func() { unhole(); heal() })

	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	time.Sleep(5 * time.Second) // publish on past the take-over
	stop()
	if lineWith(a.Output.String(), `"msg":"listener lease released: the receive connection is down"`) == "" {
		t.Fatalf("%s never released the lease for its lost receive connection; its lease lines:\n%s", a.instance,
			mpLinesMatching(a.Output.String(), "lease"))
	}
	messages := publisher.since(cut)
	serving.awaitAll(t, messages, mpHardBound+5*time.Second)
	serving.requireWithin(t, messages, mpHardBound, cut)
	serving.requireOrder(t, messages)
	serving.requireOnce(t, messages)
}

// mpLinesMatching is the lines of output that contain needle.
func mpLinesMatching(output, needle string) string {
	var kept []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, needle) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// The active pod's receive connection is lost again while its reconnect hooks are still running
// from the reconnect before. The relay drops every JetStream stream-info request of the role claim
// bucket on that connection, which the listener's KV rewatch hook - the first of its reconnect
// hooks - makes first, so the hook waits on that request. The connection is cut
// until the pod notices, then healed, so nats.go reconnects in place and the hooks start; a
// second later, with the rewatch still waiting, the connection is cut again on a route that drops
// its packets, its reconnect dials hanging, while the lease connection goes on renewing. The
// holder must release the lease on its own bound and the standby must take it over: every message
// reaches its session once, and every message published after the second cut within 15 s of it
// and in publish order. At 9c247139 the client ran its reconnect hooks on nats.go's callback
// goroutine, so the disconnect of the second cut waited behind the hanging rewatch, the receive
// signal read up for as long as the hooks took, and the take-over came long past 15 s.
//
// Since main's 2200c28eae (#4134) a run of the hooks is under a context that ends when its
// connection leaves CONNECTED, and a run that ends so is superseded, not failed
// (internal/bus/recovery.go, runReconnectHooks and reportSuperseded): the rewatch no longer waits
// out its 10 s request timeout to fail, its open of the role bucket is cancelled by the second
// cut's loss, and the run's INFO line names that open. That line, which no run has written a
// second after the reconnect, is what proves the rewatch hung through the cut; a build whose
// hooks return at once writes none, and the row fails there.
func TestTwoPodsReceiveConnectionLostWhileItsReconnectHooksHangHandsOverWithin15s(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := mpStartPodBehindLanes(t, binary, uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	b := mpStartPod(t, binary, uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	serving := mpStartNATSSessions(t, uri)
	sessions := []string{"ses_mp_hookhang_one", "ses_mp_hookhang_two"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, uri)
	receive := mpReceiveConnection(a.instance)
	named := `name="` + receive + `"`
	reconnects := func() int {
		return strings.Count(mpLinesMatching(a.Output.String(), named), "envoy nats reconnected")
	}
	// hungRuns counts the runs of the hooks on the receive connection that its loss superseded
	// while the rewatch still waited for the dropped stream info: the open of the role bucket is
	// what the superseded line names as cancelled (2200c28eae).
	hungRuns := func() int {
		superseded := mpLinesMatching(mpLinesMatching(a.Output.String(), named),
			"envoy nats reconnect hooks superseded by a newer connection event")
		return strings.Count(superseded, "rewatch interest cache: open role KV bucket: context canceled")
	}
	hookFailures := func() int {
		return strings.Count(mpLinesMatching(a.Output.String(), named), "envoy nats reconnect hook failed")
	}

	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first messages to arrive", func() bool { return len(publisher.all()) >= 20 })
	undrop := a.lanes.dropRequests(receive, "$JS.API.STREAM.INFO.KV_"+bus.RoleClaimBucket)
	t.Cleanup(undrop)
	before := reconnects()
	var healFirst func()
	publisher.quiesce(t, serving, func() { healFirst = a.lanes.cut(receive) })
	t.Cleanup(func() { healFirst() })
	mpEventually(t, 10*time.Second, a.instance+" to notice the first cut", func() bool {
		return strings.Contains(mpLinesMatching(a.Output.String(), named), "envoy nats disconnected")
	})
	healFirst()
	mpEventually(t, 10*time.Second, a.instance+" to reconnect in place", func() bool { return reconnects() > before })
	hungBefore, failedBefore := hungRuns(), hookFailures()
	time.Sleep(time.Second)
	if hungRuns() != hungBefore || hookFailures() != failedBefore {
		t.Fatalf("%s's reconnect hooks ended within a second of the reconnect, so the row does not cut while they run:\n%s",
			a.instance, mpLinesMatching(a.Output.String(), named))
	}
	if record := publisher.stored(t, bus.ListenerStateBucket, bus.ListenerLeaseKey(mpMachine)); record["holder"] != a.instance || record["released"] == true {
		t.Fatalf("after the reconnect in place the lease record names %v (released %v), want %s holding it",
			record["holder"], record["released"], a.instance)
	}

	var unhole, heal func()
	cut := time.Now()
	unhole = a.lanes.blackholeNew()
	heal = a.lanes.cut(receive)
	t.Cleanup(func() { unhole(); heal() })
	mpEventually(t, mpHardBound, b.instance+" to take the lease over", b.active)
	taken := time.Since(cut)
	time.Sleep(5 * time.Second) // publish on past the take-over
	stop()
	if hungRuns() == hungBefore {
		t.Fatalf("%s's reconnect hooks had not failed by the end of the row: no run the second cut superseded names the rewatch's open of the role bucket as cancelled, so the rewatch did not hang:\n%s",
			a.instance, mpLinesMatching(a.Output.String(), named))
	}

	all := publisher.since(time.Time{})
	serving.awaitAll(t, all, mpHardBound+10*time.Second)
	serving.requireOnce(t, all)
	messages := publisher.since(cut)
	serving.requireWithin(t, messages, mpHardBound, cut)
	serving.requireOrder(t, messages)
	t.Logf("%s took the lease %s after the second cut, with %s's rewatch hanging; %d messages after it", b.instance,
		taken.Round(time.Millisecond), a.instance, len(messages))
}

// mpLateGrant is a row's NATS server where a pod of mpMachine connects as one of two nkey users:
// the late user, whose grant is the listener's (testnats.ListenerGrant) without the listener state
// bucket, as a hub's auth.conf from before that bucket grants it, and the granted user, holding the
// whole grant. grant gives the late user the whole grant by a reload that keeps every connection,
// as an operator applying the auth.conf that carries the bucket does. mpStartPod connects a pod as
// the late user, and one started with grantedEnv as the granted one.
type mpLateGrant struct {
	server      *testnats.Granted
	uri         string
	whole       testnats.Grant
	late        string
	granted     string
	grantedSeed string
}

func mpStartLateGrant(t *testing.T) *mpLateGrant {
	t.Helper()
	whole := testnats.ListenerGrant(t, mpMachine)
	lateSeed, late := testnats.User(t)
	grantedSeed, granted := testnats.User(t)
	server := testnats.StartGranted(t,
		testnats.GrantedUser{Public: late, Grant: mpWithoutListenerState(whole)},
		testnats.GrantedUser{Public: granted, Grant: whole})
	mpSeeds.Store(server.AdminURL, lateSeed)
	t.Cleanup(func() { mpSeeds.Delete(server.AdminURL) })
	return &mpLateGrant{server: server, uri: server.AdminURL, whole: whole, late: late, granted: granted, grantedSeed: grantedSeed}
}

// grant gives the late user the whole grant and returns the instant just before the reload began.
func (g *mpLateGrant) grant(t *testing.T) time.Time {
	t.Helper()
	at := time.Now()
	g.server.Regrant(t,
		testnats.GrantedUser{Public: g.late, Grant: g.whole},
		testnats.GrantedUser{Public: g.granted, Grant: g.whole})
	return at
}

// grantedEnv connects a pod as the granted user: mpStartPodAt appends it after the late user's seed,
// and the last of two entries for one variable is the one a process gets.
func (g *mpLateGrant) grantedEnv() string { return "NATS_NKEY_SEED=" + g.grantedSeed }

// mpWithoutListenerState is grant without a subject of the listener state bucket.
func mpWithoutListenerState(grant testnats.Grant) testnats.Grant {
	grant.Publish.Allow = slices.DeleteFunc(slices.Clone(grant.Publish.Allow), func(subject string) bool {
		return strings.Contains(subject, "KV_"+bus.ListenerStateBucket) || strings.Contains(subject, "$KV."+bus.ListenerStateBucket+".")
	})
	return grant
}

// mpRecords is every record of a pod's output whose msg is msg, decoded.
func mpRecords(output, msg string) []map[string]any {
	var records []map[string]any
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == msg {
			records = append(records, record)
		}
	}
	return records
}

// mpRecordTime is when a pod logged record.
func mpRecordTime(t *testing.T, record map[string]any) time.Time {
	t.Helper()
	text, _ := record["time"].(string)
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("read the time of %v: %v", record, err)
	}
	return at
}

// mpJoinTry bounds a try of the listener state bucket the grant refused that was in flight when the
// grant came: the open waits listenerStateOpenSends of listenerStateRequestWait at most, and a
// second for the rest of it.
const mpJoinTry = listenerStateOpenSends*listenerStateRequestWait + time.Second

// mpJoined waits for pod to log line, one of the lines a pod that delivered without a lease logs as
// it joins the lease (leaseJoinedHolderLine, leaseJoinedStandbyLine), once the grant applied at
// granted lets it open the listener state bucket. It returns when the pod logged it, and the
// retry_in of the last refusal of the bucket the pod logged before it: the wait between the pod's
// tries that the try which opened the bucket came at the end of. The join must come within that
// wait and one try of the grant.
func mpJoined(t *testing.T, pod *mpPod, line string, granted time.Time) time.Time {
	t.Helper()
	var joined time.Time
	mpEventually(t, durableRefusedRetryCap+mpJoinTry, pod.instance+" to join the lease once its grant carried the listener state bucket", func() bool {
		records := mpRecords(pod.Output.String(), line)
		if len(records) == 0 {
			return false
		}
		joined = mpRecordTime(t, records[0])
		return true
	})
	backoff := durableBindPoll
	for _, record := range mpRecords(pod.Output.String(), listenerStateRefusedLine) {
		if mpRecordTime(t, record).After(joined) {
			break
		}
		wait, _ := record["retry_in"].(string)
		parsed, err := time.ParseDuration(wait)
		if err != nil {
			t.Fatalf("read the retry_in of %v: %v", record, err)
		}
		backoff = parsed
	}
	if took := joined.Sub(granted); took > backoff+mpJoinTry {
		t.Fatalf("%s joined the lease %s after its grant carried the bucket, past the %s wait it was in and a try's %s", pod.instance,
			took.Round(time.Millisecond), backoff, mpJoinTry)
	}
	t.Logf("%s joined the lease %s after the grant, out of a %s wait", pod.instance, joined.Sub(granted).Round(time.Millisecond), backoff)
	return joined
}

// mpLateGrantRow is a row whose first pod, pod-a, started while its grant lacked the listener state
// bucket (mpLateGrant): it delivers without a lease, /healthz says so, four sessions are registered
// through it on consumer delivery, and a message is published every 100 ms to their inboxes, in
// turn, until stop.
type mpLateGrantRow struct {
	grant     *mpLateGrant
	binary    string
	a         *mpPod
	serving   *mpNATSSession
	sessions  []string
	publisher *mpPublisher
	stop      func()
}

// mpStartLateGrantRow starts a row's pod-a without the grant through start (mpStartPod, or
// mpStartPodBehindLanes for a row that fails one of its connections), and waits until it has tried
// the bucket again at least once, so the wait between its tries is in its log.
func mpStartLateGrantRow(t *testing.T, name string, start func(t *testing.T, binary, natsURL, instance string, env ...string) *mpPod) *mpLateGrantRow {
	t.Helper()
	grant := mpStartLateGrant(t)
	binary := cmdtest.Build(t, "envoy-listener")
	a := start(t, binary, grant.uri, "pod-a")
	mpWaitActive(t, a, 30*time.Second)
	if _, body := a.health(); body["lease"] != listenerLeaseUnavailable {
		t.Fatalf("pod-a's /healthz without the grant names lease %v, want %q", body["lease"], listenerLeaseUnavailable)
	}
	serving := mpStartNATSSessions(t, grant.uri)
	sessions := []string{"ses_mp_" + name + "_one", "ses_mp_" + name + "_two", "ses_mp_" + name + "_three", "ses_mp_" + name + "_four"}
	for _, sessionID := range sessions {
		a.registerNATS(t, serving, sessionID)
	}
	publisher := mpConnectPublisher(t, grant.uri)
	stop := publisher.every(100*time.Millisecond, sessions...)
	mpEventually(t, 10*time.Second, "the first 20 messages to reach their sessions", func() bool {
		sent := publisher.all()
		return len(sent) >= 20 && serving.tookAll(sent[:20])
	})
	mpEventually(t, durableRefusedRetryCap+mpJoinTry, "pod-a to try the listener state bucket again", func() bool {
		return len(mpRecords(a.Output.String(), listenerStateRefusedLine)) > 0
	})
	return &mpLateGrantRow{grant: grant, binary: binary, a: a, serving: serving, sessions: sessions, publisher: publisher, stop: stop}
}

// mpRelayed fails the row, naming why, when pod logged its session relays changing to relaying
// want: the relays log each change of whether they follow a receive term.
func mpRelayed(t *testing.T, pod *mpPod, want bool, why string) {
	t.Helper()
	for _, record := range mpRecords(pod.Output.String(), "session relays follow the receive term") {
		if record["relaying"] == want {
			t.Fatalf("%s %s: %v", pod.instance, why, record)
		}
	}
}

// mpRelayTermRestartLine is the line a relay logs when the term its pull was held to ended and its
// pod serves a term again, so it restarts its session's consumer at the ack floor
// (sessionRelays.relayNATS).
const mpRelayTermRestartLine = "session relay starts again at the ack floor with a new pull: the term its pull was held to ended, and this task serves a term again"

// A pod started before the hub's grant carried the listener state bucket delivers without a lease,
// tries the bucket again, and once an operator applies the grant joins the lease without a
// restart: within the wait it was in between two tries, and one try, of the grant, it takes the
// absent record and keeps the subscription it received on, its queue and its relays as the
// holder's. /healthz drops its lease field and names it active, it subscribes to the durable once
// in all, its relays never stop nor restart a session's consumer, and every message published
// before, during and after the join
// reaches its session exactly once and within 3 s of its publish. Before, the pod opened the
// bucket once, at its start, and ran without a lease until it was restarted; a pod that let its
// subscription go before it took the lease would leave each session its relays' stop and restart
// as a gap.
func TestTwoPodsLeaseTakenOnALateGrantKeepsTheSubscription(t *testing.T) {
	row := mpStartLateGrantRow(t, "late_take", mpStartPod)
	granted := row.grant.grant(t)
	mpJoined(t, row.a, leaseJoinedHolderLine, granted)
	mpEventually(t, 5*time.Second, "pod-a's /healthz to drop its lease field and name it active", func() bool {
		status, body := row.a.health()
		_, named := body["lease"]
		return status == http.StatusOK && body["status"] == "healthy" && body["role"] == "active" && !named
	})
	time.Sleep(5 * time.Second) // publish on past the join
	row.stop()
	if subscribes := len(mpRecords(row.a.Output.String(), "durable receiving")); subscribes != 1 {
		t.Fatalf("pod-a subscribed to the durable %d times across the join, want once: it let its subscription go", subscribes)
	}
	mpRelayed(t, row.a, false, "stopped its session relays across the join")
	if restarts := len(mpRecords(row.a.Output.String(), mpRelayTermRestartLine)); restarts != 0 {
		t.Fatalf("pod-a's session relays restarted %d consumers at their ack floor across the join, want none: their term ended under them", restarts)
	}

	messages := row.publisher.all()
	row.serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	row.serving.requireOnce(t, messages)
	row.serving.requireWithin(t, messages, 3*time.Second, time.Time{})
}

// Once a pod that started without the grant has joined the lease and holds it, a pod started after
// that stands by: /healthz names it a standby throughout, it never subscribes to the durable nor
// relays a session, and the first pod goes on delivering every message exactly once.
func TestTwoPodsLeaseTakenOnALateGrantLeavesALaterPodStandingBy(t *testing.T) {
	row := mpStartLateGrantRow(t, "late_standby", mpStartPod)
	granted := row.grant.grant(t)
	mpJoined(t, row.a, leaseJoinedHolderLine, granted)
	b := mpStartPod(t, row.binary, row.grant.uri, "pod-b")
	mpWaitStandby(t, b, 30*time.Second)
	for watched := time.Now(); time.Since(watched) < 5*time.Second; time.Sleep(mpPoll) {
		if !b.standby() || !row.a.active() {
			status, body := b.health()
			t.Fatalf("with pod-a holding the lease it joined, pod-b's /healthz is %d %v and pod-a active is %v, want pod-b a standby and pod-a active", status, body, row.a.active())
		}
	}
	row.stop()
	messages := row.publisher.all()
	row.serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	row.serving.requireOnce(t, messages)
	if subscribes := len(mpRecords(b.Output.String(), "durable receiving")); subscribes != 0 {
		t.Fatalf("pod-b subscribed to the durable %d times beside pod-a's lease", subscribes)
	}
	mpRelayed(t, b, true, "relayed its machine's sessions beside pod-a's lease")
}

// A pod started before its grant carried the listener state bucket delivers without a lease, and a
// pod started after, under a grant that carries it, takes the absent lease and waits out the first
// pod's push binding as a pre-lease build's. Once the first pod's grant carries the bucket too, it
// reads that holder's record: it stops delivering at once, its relays stop and settle, its
// subscription goes and it stands by, and the holder's wait for the binding ends at its next poll,
// so delivery passes to the holder within relayFormerSettle and a poll of the join, the join itself
// coming within the first pod's wait and one try of the grant. Every message reaches its session
// exactly once. Before, the first pod ran without a lease for good and the holder never received.
func TestTwoPodsLeaseHeldElsewhereOnALateGrantHandsDeliveryToItsHolder(t *testing.T) {
	row := mpStartLateGrantRow(t, "late_yield", mpStartPod)
	b := mpStartLateGrantHolder(t, row)
	granted := row.grant.grant(t)
	joined := mpJoined(t, row.a, leaseJoinedStandbyLine, granted)
	handOver := relayFormerSettle + durableBindPoll + time.Second
	receiving := mpReceiving(t, b, handOver+5*time.Second)
	if handed := receiving.Sub(joined); handed > handOver {
		t.Fatalf("pod-b received the durable %s after pod-a joined the lease, past relayFormerSettle and a poll (%s)", handed.Round(time.Millisecond), handOver)
	}
	mpWaitActive(t, b, 10*time.Second)
	mpWaitStandby(t, row.a, 10*time.Second)
	time.Sleep(5 * time.Second) // publish on past the hand-over
	row.stop()

	messages := row.publisher.all()
	row.serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	row.serving.requireOnce(t, messages)
	t.Logf("delivery passed to pod-b %s after pod-a joined the lease; %d messages, each taken once", receiving.Sub(joined).Round(time.Millisecond), len(messages))
}

// mpStartLateGrantHolder starts the row's pod-b under the whole grant (grantedEnv) and waits until
// it has taken the absent lease and waits out pod-a's push binding as a pre-lease build's.
func mpStartLateGrantHolder(t *testing.T, row *mpLateGrantRow) *mpPod {
	t.Helper()
	return mpStartLateGrantHolderWith(t, row, mpStartPod)
}

// mpStartLateGrantHolderWith is mpStartLateGrantHolder through start.
func mpStartLateGrantHolderWith(t *testing.T, row *mpLateGrantRow, start func(t *testing.T, binary, natsURL, instance string, env ...string) *mpPod) *mpPod {
	t.Helper()
	b := start(t, row.binary, row.grant.uri, "pod-b", row.grant.grantedEnv())
	mpEventually(t, 60*time.Second, "pod-b to take the lease and wait out pod-a's push binding", func() bool {
		return len(mpRecords(b.Output.String(), "durable is push-bound by a subscriber without the lease; waiting for its binding to end")) > 0
	})
	return b
}

// mpReceiving waits, within, for pod to subscribe to the durable, and returns when it logged that.
func mpReceiving(t *testing.T, pod *mpPod, within time.Duration) time.Time {
	t.Helper()
	var receiving time.Time
	mpEventually(t, within, pod.instance+" to receive the durable", func() bool {
		records := mpRecords(pod.Output.String(), "durable receiving")
		if len(records) == 0 {
			return false
		}
		receiving = mpRecordTime(t, records[0])
		return true
	})
	return receiving
}

// mpDecisionGrace is how long after a pod's join decision - its leaseJoinedStandbyLine - a message
// published may still be one its relays took. Nothing in the code waits there: joinTerm clears the
// relays' term (sessionRelays.setOwned) in its next step after that line, on the receive loop's
// goroutine, and from then on no relay's pull hands out a message (sessionRelays.pullBound,
// bus.PullBound). The durable's subscription, which goes relayFormerSettle (1 s) later, takes no
// part: the durable path skips what a session's consumer holds, and from the decision on its
// handler forwards and acks nothing (durableReceiver.holding). So the grace covers the clear's lag
// behind the line, and a publisher's stamp, taken once the stream has acked the message, coming
// after a relay already took that message. Measured at ef976a9b across 10 runs of the row below:
// the relays stopped 20 µs to 1.03 ms after the line, and the last message pod-a forwarded was
// published 6 to 65 ms before it. 100 ms is that 1.03 ms and a margin of about 99 ms for a
// publisher's stamp taken late under load. A message published inside the grace can come twice:
// pod-a took and forwarded it before its relays stopped, and the holder's restart of the session's
// consumer at the floor the held frame keeps sends it again, the in-doubt repeat
// (sessionRelays.settleStoppedFrames).
const mpDecisionGrace = 100 * time.Millisecond

// mpReceiveHoldBound bounds how long a row may hold a pod's receive connection (mpLaneRelay.cut,
// which stops its bytes both ways and leaves its sockets open) and have the pod take the hold for
// a slow link. That connection pings its server every second and reads itself lost at the ping
// that would put a third unanswered one out (bus.WithSilentCutDetection, listenerStreamOptions):
// two seconds at the soonest after the first ping the hold leaves unanswered, which the pod sent
// at most a round trip through the relay before the hold began. A receive connection read lost
// reconnects, and is lost receive for the pod's lease (leaseReceiveLost), neither of which a row
// that only holds the connection is about. The 100 ms under those two seconds is for that round
// trip and for the answer after the heal.
const mpReceiveHoldBound = 1900 * time.Millisecond

// The rule a pod that delivered without a lease stops forwarding by once its join reads another
// pod's record (durableReceiver.joinTerm): from that decision on, nothing it takes is forwarded or
// acked. One frame to each session is in the first pod's hands across the decision - the session
// answers it received and never done (mpNATSSession.withhold), so it stays unacknowledged
// at its consumer's floor - and the holder therefore restarts each consumer at that floor
// (bus.SessionConsumers.KeepListed keeps none with a message unacknowledged), sending again every
// frame above it. A frame the session answered above a held one before the decision may so come
// twice: the repeat the README's "When a session can see a message twice" states
// (sessionRelays.settleStoppedFrames). One the first pod forwarded after its decision would come
// twice as well. So every message published from mpDecisionGrace (100 ms) after the decision on
// reaches its session exactly once, and none more than twice: the grace is the relays' stop
// behind the decision's line, 1.03 ms at most across 10 runs, and a margin for the publisher's
// stamp (mpDecisionGrace). A message published inside it may be one the first pod took before its
// relays stopped, which comes twice as the repeat above does. The holder's restart deletes each
// consumer and creates it again, which must come after the consumer's state is stored
// (awaitSessionConsumerStored), so the row holds the holder's receive connection until it is: the
// holder cannot read the first pod's binding gone, and restart, before.
func TestTwoPodsLeaseHeldElsewhereOnALateGrantForwardsNothingPastItsDecision(t *testing.T) {
	row := mpStartLateGrantRow(t, "late_fence", mpStartPod)
	b := mpStartLateGrantHolderWith(t, row, mpStartPodBehindLanes)
	var held []<-chan struct{}
	for _, sessionID := range row.sessions {
		held = append(held, row.serving.withhold(sessionID))
	}
	for index, arrived := range held {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("no frame to %s arrived to hold within 10 s", row.sessions[index])
		}
	}
	granted := row.grant.grant(t)
	joined := mpJoined(t, row.a, leaseJoinedStandbyLine, granted)
	// pod-b's restart at the ack floor deletes these consumers and creates them again, so each must
	// be stored before it (awaitSessionConsumerStored), once pod-a's relay of it has handed its
	// frames back, its last act on the consumer (relayNATS). The cut of pod-b's receive connection
	// holds the restart until then. pod-b restarts the consumers as it subscribes
	// (durableReceiver.begin), after the poll of the durable that reads its binding gone
	// (awaitUnbound), and that poll is answered on this connection; the cut holds its bytes both
	// ways, sockets open, until the heal below, while pod-b's lease renews on a connection of its
	// own. The binding goes with pod-a's subscription, relayFormerSettle after the decision
	// (joinTerm) and past the settle of pod-a's relays, so a cut made before then holds every
	// answer that reads it gone.
	heal := b.lanes.cut(mpReceiveConnection(b.instance))
	cut := time.Now()
	t.Cleanup(heal)
	if late := cut.Sub(joined); late >= relayFormerSettle {
		t.Fatalf("pod-b's receive connection was cut %s after pod-a's decision, not within relayFormerSettle (%s), when pod-a's subscription may have gone: pod-b may have read the binding gone before the consumers were stored",
			late.Round(time.Millisecond), relayFormerSettle)
	}
	whole, failed := relayHandOverLines(relayCancelTermEnded)
	for _, sessionID := range row.sessions {
		mpEventually(t, 10*time.Second, row.a.instance+"'s relay of "+sessionID+" to hand its frames back", func() bool {
			output := row.a.Output.String()
			for _, record := range append(mpRecords(output, whole), mpRecords(output, failed)...) {
				if record["session_id"] == sessionID && record["reason"] == relayCancelTermEnded.String() {
					return true
				}
			}
			return false
		})
		awaitSessionConsumerStored(t, row.publisher.client.Conn, sessionID)
	}
	heal()
	if hold := time.Since(cut); hold >= mpReceiveHoldBound {
		t.Fatalf("pod-b's receive connection was held %s, not within mpReceiveHoldBound (%s): pod-b may have read it lost, reconnected and lost receive for its lease",
			hold.Round(time.Millisecond), mpReceiveHoldBound)
	}
	mpReceiving(t, b, 30*time.Second)
	mpWaitActive(t, b, 10*time.Second)
	time.Sleep(5 * time.Second) // publish on past the hand-over
	row.stop()

	messages := row.publisher.all()
	row.serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	counts := row.serving.counts()
	for _, m := range messages {
		if counts[m.event] > 2 {
			t.Fatalf("%s to %s reached it %d times, want at most twice", m.event, m.session, counts[m.event])
		}
	}
	after := row.publisher.since(joined.Add(mpDecisionGrace))
	// The figures mpDecisionGrace rests on, in this run: when pod-a's relays stopped, and when the
	// last message pod-a forwarded - the last one that came twice - was published, each from its
	// decision.
	stopped := time.Duration(-1)
	for _, record := range mpRecords(row.a.Output.String(), "session relays follow the receive term") {
		if at := mpRecordTime(t, record); record["relaying"] == false && !at.Before(joined) {
			stopped = at.Sub(joined)
			break
		}
	}
	var forwarded time.Time
	for _, m := range messages {
		if counts[m.event] == 2 && m.published.After(forwarded) {
			forwarded = m.published
		}
	}
	t.Logf("pod-a's relays stopped %s after its decision, and the last message it forwarded, one that came twice, was published %s from it",
		stopped, forwarded.Sub(joined).Round(time.Millisecond))
	row.serving.requireOnce(t, after)
	t.Logf("%d messages published from mpDecisionGrace past pod-a's decision on, each taken once", len(after))
}

// mpRoleLaneConnection is the client name of a pod's role lane connection (bus.ConnectRoleLane),
// which an mpLaneRelay drops by.
const mpRoleLaneConnection = "envoy-role-lane"

// A role message the first pod forwarded while it delivered without a lease is recorded in its own
// ledger alone, which the machine's shared one does not see after the join (dedupe.Joining), so a
// sender that sends it again once the holder of the lease is the only role-lane member left has
// the holder forward it again with no pair in the shared ledger. The stream stores that forward
// once all the same: each forward to the role's holder carries one Nats-Msg-Id however often it is
// sent (roleForward.msgID, bus.ForwardMsgID), within the stream's duplicate window
// (streamDuplicateWindow, 72 h), so the holder's session takes the message once. Role messages are
// not stored (the stream's subjects capture no notifications.role.>), so a sender's repeat is the
// repeat a role message meets; the row puts each send on one pod's role lane by dropping the
// other's (mpLaneRelay.drop).
func TestTwoPodsLeaseHeldElsewhereOnALateGrantStoresARepeatedRoleForwardOnce(t *testing.T) {
	row := mpStartLateGrantRow(t, "late_role", mpStartPodBehindLanes)
	holder, role := row.sessions[0], "reviewer"
	row.a.setRole(t, holder, role)
	b := mpStartLateGrantHolderWith(t, row, mpStartPodBehindLanes)
	event := "evt-mp-role-repeat"
	item := contracts.Envelope{
		EventID:        event,
		Source:         "agent",
		SourceEventID:  event,
		Topic:          contracts.RoleTopicPrefix + role,
		DedupeKey:      "multiprocess." + event,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "multiprocess role message mp-event=" + event,
		TraceID:        "trace-" + event,
	}
	forwarded := func(pod *mpPod) bool {
		for _, record := range mpRecords(pod.Output.String(), "listener role forwarded") {
			if record["event_id"] == event {
				return true
			}
		}
		return false
	}

	healB := b.lanes.drop(mpRoleLaneConnection)
	time.Sleep(time.Second) // the server drops pod-b's role-lane member with its connection
	if err := row.publisher.client.Publish(item); err != nil {
		t.Fatalf("send the role message: %v", err)
	}
	mpEventually(t, 10*time.Second, "pod-a's role lane to forward the role message and its holder to take it", func() bool {
		return forwarded(row.a) && row.serving.counts()[event] == 1
	})
	healB()

	granted := row.grant.grant(t)
	mpJoined(t, row.a, leaseJoinedStandbyLine, granted)
	mpReceiving(t, b, 30*time.Second)
	mpWaitActive(t, b, 30*time.Second)
	healA := row.a.lanes.drop(mpRoleLaneConnection)
	defer healA()
	time.Sleep(time.Second) // the server drops pod-a's role-lane member with its connection
	if err := row.publisher.client.Publish(item); err != nil {
		t.Fatalf("send the role message again: %v", err)
	}
	mpEventually(t, 10*time.Second, "pod-b's role lane to forward the role message sent again", func() bool { return forwarded(b) })
	row.stop()
	time.Sleep(mpQuiet)
	if took := row.serving.counts()[event]; took != 1 {
		t.Fatalf("%s took the role message %s %d times, want once: pod-b's forward of it was stored again", holder, event, took)
	}
}

// A pod that delivered without a lease, whose join reads the record of a holder that has since
// died (killed while it waited out this pod's binding), counts that record held: it stands by,
// and its lease takes the record once the record has stood unchanged for a full ttl, as any
// standby's does, settling as a term over an expired record must. So it holds the lease within
// the wait it was in and one try of the grant (the join), leaseTTL, a standby poll and a second;
// it receives the durable as it takes the term, and its settle hold ends within settleCap (an ack
// wait and settleMargin, 7 s) and a second of that. Every message reaches its session exactly once.
func TestTwoPodsLeaseHeldByADeadPodOnALateGrantIsTakenWhenItExpires(t *testing.T) {
	row := mpStartLateGrantRow(t, "late_expired", mpStartPod)
	b := mpStartLateGrantHolder(t, row)
	b.kill(t)
	granted := row.grant.grant(t)
	joined := mpJoined(t, row.a, leaseJoinedStandbyLine, granted)
	settleCap := productionReceiveConfig().settleCap
	takeBound := leaseTTL + leaseStandbyPoll + time.Second
	var taken time.Time
	mpEventually(t, takeBound+5*time.Second, "pod-a to take the dead holder's expired record", func() bool {
		for _, record := range mpRecords(row.a.Output.String(), "listener lease acquired") {
			if record["from"] == leaseTakenExpired.String() {
				taken = mpRecordTime(t, record)
				return true
			}
		}
		return false
	})
	if took := taken.Sub(joined); took > takeBound {
		t.Fatalf("pod-a took the dead holder's expired record %s after its join (%s after the grant), past leaseTTL, a standby poll and a second (%s): a standby takes a record it has read unchanged for a full leaseTTL, counted from its first read at the join, at its next poll (lease.poll)",
			took.Round(time.Millisecond), taken.Sub(granted).Round(time.Millisecond), takeBound)
	}
	var settled time.Time
	mpEventually(t, settleCap+5*time.Second, "pod-a's settle hold to end", func() bool {
		records := mpRecords(row.a.Output.String(), "settle hold ended; delivering the held messages in stream order")
		if len(records) == 0 {
			return false
		}
		settled = mpRecordTime(t, records[0])
		return true
	})
	if held := settled.Sub(taken); held > settleCap+time.Second {
		t.Fatalf("pod-a's settle hold ended %s after it took the expired record, past settleCap and a second (%s): a term taken over an expired record holds what it receives for settleCap at most, an ack wait and settleMargin",
			held.Round(time.Millisecond), settleCap+time.Second)
	}
	t.Logf("pod-a took the expired record %s after the grant (%s after its join) and its settle hold ended %s after that",
		taken.Sub(granted).Round(time.Millisecond), taken.Sub(joined).Round(time.Millisecond), settled.Sub(taken).Round(time.Millisecond))
	mpWaitActive(t, row.a, 10*time.Second)
	time.Sleep(5 * time.Second) // publish on past the take
	row.stop()
	messages := row.publisher.all()
	row.serving.awaitAll(t, messages, 30*time.Second)
	time.Sleep(mpQuiet)
	row.serving.requireOnce(t, messages)
}
