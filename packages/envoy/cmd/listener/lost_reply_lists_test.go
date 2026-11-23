package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The two callers of the session consumer list hold nothing while they list, the sweep's pass and
// the relays' list at gaining the durable binding. The session consumers' client answers a request
// whose connection is down with bus.ErrConnectionLost at once, so each makes its list again once
// the connection is back, inside the list's own budget (bus.Reconnected). The rows below drop the
// connection while a list waits for its reply (testnats.ReplyHold); the list it makes again on the
// new connection is the one the caller then uses. Every wait of the rows is bounded by the test's
// deadline alone (testnats.Remaining); the list's own budget, sessionConsumerCallTimeout (5 s),
// bounds the code's wait for the reconnect, which nats.go makes at its next attempt, one second on.
//
// The control of both rows is bd213550c0, whose callers did not list again: the sweep's pass
// logged the list as one it could not make and deleted nothing, and the relays' list came back
// empty, so each relay read its own consumer and restarted it (RestartAtFloor).

// consumerListPrefix is the subject a session consumer list is sent on.
const consumerListPrefix = "$JS.API.CONSUMER.LIST."

// awaitHeldList waits, up to the test's deadline, until the hold has withheld the reply to a
// consumer list and forwarded the PONG of that list's flush (requestConfirmed flushes behind its
// publish), so the list is waiting for its reply.
func awaitHeldList(t *testing.T, held *testnats.HeldReplies) {
	t.Helper()
	for _, event := range []struct {
		ch   <-chan struct{}
		what string
	}{{held.Withheld(), "withheld the list's reply"}, {held.Flushed(), "forwarded the PONG of the list's flush"}} {
		select {
		case <-event.ch:
		case <-time.After(testnats.Remaining(t)):
			t.Fatalf("the hold had not %s by the test's deadline", event.what)
		}
	}
}

// A sweep pass whose consumer list lost its connection lists again once the connection is back,
// and deletes the consumer of a session that ended without deregistering, as a pass whose list
// answered does (TestTheSweepDeletesTheConsumerOfASessionThatEndedWithoutDeregistering).
func TestASweepWhoseListTheDropCutOffListsAgainOnTheNewConnection(t *testing.T) {
	url := testnats.URL(t)
	hold := testnats.StartReplyHold(t, url)
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	now := &stateClock{}
	rig := sweepOver(newSessionConsumerRigOn(t, client, hold.URL(), sweepSessionOptions(sweepRecordTTL, now)...), now)
	const sessionID = "ses_sweep_lost_list"
	rig.register(t, sessionID)
	rig.now.advance(sweepLapse)
	rig.requireState(t, sessionID, session.Gone)

	held := hold.Hold(consumerListPrefix)
	passed := make(chan struct{})
	go func() {
		rig.sweep.pass(context.Background())
		close(passed)
	}()
	awaitHeldList(t, held)
	hold.Drop()
	select {
	case <-passed:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the sweep's pass had not ended by the test's deadline")
	}
	if logs := rig.harness.logs.String(); strings.Contains(logs, "session consumer sweep could not list the session consumers") {
		t.Fatalf("the sweep's pass gave its list up at the drop rather than list again on the new connection:\n%s", logs)
	}
	name, _ := bus.SessionConsumerName(sessionID)
	js, err := testnats.Connect(t, url).JetStream()
	if err != nil {
		t.Fatalf("open JetStream on the server: %v", err)
	}
	if info, err := js.ConsumerInfo(bus.Stream, name); err == nil {
		t.Fatalf("the consumer %s of a session that ended is still on the stream after the pass (created %s)", name, info.Created)
	}
}

// The relays' list at gaining the durable binding (sessionRelays.listConsumers), whose connection
// was lost, lists again once the connection is back: it holds the session's consumer, so the relay
// that listing starts keeps that consumer with no call of its own (bus.SessionConsumers.KeepListed)
// and pulls it, rather than restarting it at the ack floor (RestartAtFloor), which deletes the
// consumer and creates it again. The row sees that through the consumer's creation time.
func TestARelayListTheDropCutOffListsAgainAndTheRelayKeepsItsConsumer(t *testing.T) {
	rig, hold, url, client := heldListener(t)
	relays, err := startSessionRelays(rig.deps.consumers, rig.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
		defer cancel()
		relays.stop(ctx)
	})
	const sessionID = "ses_relay_lost_list"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	name, _ := bus.SessionConsumerName(sessionID)
	js, err := testnats.Connect(t, url).JetStream()
	if err != nil {
		t.Fatalf("open JetStream on the server: %v", err)
	}
	before, err := js.ConsumerInfo(bus.Stream, name)
	if err != nil {
		t.Fatalf("read the session's consumer: %v", err)
	}

	held := hold.Hold(consumerListPrefix)
	listedAt := rig.deps.consumers.mark()
	answered := make(chan int, 1)
	var listedRun *relayRun
	go func() {
		listed := relays.listConsumers()
		if info := listed[sessionID]; info != nil {
			listedRun = &relayRun{sessionID: sessionID, done: make(chan struct{}), resume: make(chan struct{}, 1), listed: info, listedAt: listedAt}
		}
		answered <- len(listed)
	}()
	awaitHeldList(t, held)
	hold.Drop()
	select {
	case n := <-answered:
		if listedRun == nil {
			t.Fatalf("the relays' list whose connection the drop cut off came back with %d consumers and not the session's, want it listed again on the new connection", n)
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the relays' list had not answered by the test's deadline")
	}

	// The restart reads the session's interest row through the listener's own client, which
	// reconnects on its own one-second wait, apart from the session consumers' client the list
	// went through. The relay loop would wait for it (relayLost); this row makes one restart, so
	// it waits for that client first, up to the test's deadline.
	for !client.Connected() && testnats.Remaining(t) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	_, pull, outcome := relays.restart(ctx, listedRun)
	if pull == nil {
		t.Fatalf("the relay started from the list did not pull the session's consumer (outcome %d)", outcome)
	}
	pull.Stop()
	after, err := js.ConsumerInfo(bus.Stream, name)
	if err != nil {
		t.Fatalf("read the session's consumer after the relay started: %v", err)
	}
	if !after.Created.Equal(before.Created) {
		t.Fatalf("the relay created the session's consumer again (created %s, was %s), want it kept from the list with no call", after.Created, before.Created)
	}
}
