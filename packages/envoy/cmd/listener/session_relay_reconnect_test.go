package main

import (
	"context"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A reconnect of the session-consumer connection costs the sessions this listener relays nothing
// but the restart itself. The pull's request is dropped with the connection it was sent on
// (bus.ErrSessionPullReconnected), which says nothing about the session or its consumer, so the
// relay restarts at the session's ack floor at once rather than waiting out the retry backoff its
// run's earlier failures had raised (sessionRelays.relay, relayReconnected).
//
// A run's wait only ever doubles, to relayRetryMax, and a registration under a new epoch
// restarts the running relay rather than replacing its run (sessionRelays.registered), so the
// backoff that run had reached is the one the reconnect meets: without this, a run that had met
// a few failed restarts would cost its session 30 s at every later reconnect, which is what the
// fragment pr2404-session-pull-reconnect.yaml and AGENTS.md say it does not.
func TestASessionRelayRestartsAtOnceWhenItsPullsConnectionReconnects(t *testing.T) {
	rig, sessionID, standIn, held, floor := sessionWithAHeldFrame(t, "ses_relay_reconnect_")
	agent := contracts.AgentSubject(sessionID)

	// Three failed restarts raise this run's retry wait: it waits relayRetryFirst, then 2 s, then
	// 4 s, so the wait standing when the reconnect comes is 8 s.
	failed := failCreates(t, rig, sessionID, 3)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	for range 3 {
		awaitCreateFailure(t, failed)
	}
	// The fourth restart creates the consumer at the floor it read, and the relay pulls it: the
	// message the session was holding comes again, and the session writes it this time, so the
	// consumer's floor covers it and the restart after the reconnect sends it no third time.
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), held)
	awaitAckState(t, rig.consumers, sessionID, 0, floor+1)

	if err := rig.deps.consumers.consumers.ForceReconnect(); err != nil {
		t.Fatalf("reconnect the session-consumer connection in place: %v", err)
	}
	reconnected := time.Now()
	item, _ := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 30*time.Second), item)
	if took := time.Since(reconnected); took > 2*time.Second {
		t.Fatalf("the relay sent the session's next message %s after the reconnect, want it restarted at once rather than after the 8 s its earlier failures had raised", took.Round(time.Millisecond))
	}
}

// interestsUnread is the line a relay restart writes when it could not read the session's stored
// topics (sessionRelays.storedFilter).
const interestsUnread = "session relay could not read the session's interests"

// The restart a reconnect of the session-consumer connection starts waits for the main
// connection, the one the session's stored topics are read through, when that one is still
// reconnecting: the two connections reconnect on their own, so the session-consumer one can be
// back first, and a read sent on a connection that is reconnecting fails at once, the reconnect
// buffer being off. Failed, the restart cost the session a retry backoff, 1 s or more, for a main
// connection that was back a moment later (sessionRelays.storedFilter).
//
// The test owns the order: the main connection goes through a proxy it cuts, which refuses every
// reconnect until it is restored, and the session-consumer connection, straight to the server,
// reconnects while the cut holds. Only once the restart is under way and the session-consumer
// connection answers is the main one let back.
func TestASessionRelayRestartWaitsForTheMainConnectionRatherThanFailingOnIt(t *testing.T) {
	url := testnats.URL(t)
	hub := testnats.StartProxy(t, url)
	client, err := bus.ConnectOwningStream([]string{hub.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's main connection through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newRelayRigUnownedOn(t, client, url, relayPauseFirst, relayPauseMax)
	rig.relays.setOwned(true)
	sessionID := "ses_relay_main_reconnect_" + id.New()
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	first, _ := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)
	waitFor(t, 10*time.Second, "the relay's restart to have let go of its turn", func() bool { return len(rig.relays.restarts) == 0 })

	hub.Cut()
	waitFor(t, 10*time.Second, "the main connection to be down", func() bool { return !client.Connected() })
	logged := len(rig.harness.logs.String())
	since := func() string { return rig.harness.logs.String()[logged:] }
	if err := rig.deps.consumers.consumers.ForceReconnect(); err != nil {
		t.Fatalf("reconnect the session-consumer connection in place: %v", err)
	}
	// The reconnect ends the pull and the relay restarts: the restart holds its turn
	// (relayRestartLimit) until it returns, so a turn held is a restart under way, and one that
	// failed on the main connection has written its line.
	waitFor(t, 10*time.Second, "the relay to restart after the session-consumer reconnect", func() bool {
		return len(rig.relays.restarts) == 1 || strings.Contains(since(), interestsUnread)
	})
	infoCtx, cancelInfo := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelInfo()
	if _, err := rig.deps.consumers.consumers.Info(infoCtx, sessionID); err != nil {
		t.Fatalf("the session-consumer connection did not answer after its reconnect: %v", err)
	}
	// The cut holds a while longer, so a restart that fails on the main connection has long
	// failed before it is let back.
	time.Sleep(500 * time.Millisecond)
	if client.Connected() {
		t.Fatal("the main connection is back while the proxy is cut")
	}
	if strings.Contains(since(), interestsUnread) {
		t.Fatalf("the restart failed reading the session's interests while the main connection was reconnecting, want it waiting for that connection:\n%s", since())
	}
	if len(rig.relays.restarts) != 1 {
		t.Fatalf("the restart is no longer under way while the main connection is down:\n%s", since())
	}

	// Stored while the main connection is down, by a connection of the test's own: only the
	// restart's pull can deliver it.
	direct := testnats.Connect(t, url)
	t.Cleanup(direct.Close)
	js, err := direct.JetStream()
	if err != nil {
		t.Fatalf("open JetStream on the test's connection: %v", err)
	}
	second := listenerTestEnvelope(agent, "relay-test-"+id.New())
	if _, err := js.Publish(agent, marshalListenerEnvelope(t, second)); err != nil {
		t.Fatalf("store a message on %s: %v", agent, err)
	}
	back := client.Connection().StatusChanged(natsgo.CONNECTED)
	hub.Restore()
	select {
	case <-back:
	case <-time.After(30 * time.Second):
		t.Fatal("the main connection did not reconnect within 30 s of the proxy's restore")
	}
	reconnectedAt := time.Now()
	frame := standIn.collect(t, 1, 10*time.Second)[0]
	requireFrames(t, []relayedFrame{frame}, second)
	took := frame.arrived.Sub(reconnectedAt)
	if took > 500*time.Millisecond {
		t.Fatalf("the session had the message stored during the cut %s after the main connection reconnected, want within 500ms:\n%s", took.Round(time.Millisecond), since())
	}
	t.Logf("the session had the message stored during the cut %s after the main connection reconnected", took.Round(time.Millisecond))
	if strings.Contains(since(), interestsUnread) {
		t.Fatalf("the restart failed reading the session's interests:\n%s", since())
	}
}
