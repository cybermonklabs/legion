package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// What these rows decide is how a call the bus makes directly on a running client's connection,
// outside Request, answers nats.go's close of that connection: ErrConnectionLost, which Reconnected
// matches once the client has recovered its connection, and never nats.go's raw close error, which
// its callers read as lasting. Each row makes the order certain rather than likely. A hold that is
// cut refuses every dial (testnats.ReplyHold.Cut), so a client whose connection the row closes
// stays on that closed connection, its recovery dialling in vain, until the row restores the hold:
// the call the row makes then is made on the client's closed connection, whatever the box's speed.
// Every wait is bounded by the test's deadline alone (testnats.Remaining, ownWait).

// closeHeld cuts the hold and closes c's connection, so c is a running client whose connection is
// closed and stays so until the hold is restored: every dial its recovery makes is refused.
func closeHeld(t *testing.T, c *Client, hold *testnats.ReplyHold) *nats.Conn {
	t.Helper()
	conn := c.Connection()
	hold.Cut()
	t.Cleanup(hold.Restore)
	conn.Close()
	if c.stopped() {
		t.Fatal("closing the client's connection stopped the client")
	}
	return conn
}

// requireLost requires err to be a running client's lost connection, the client c's, unsent when
// unsent says so, that names cause in its text.
func requireLost(t *testing.T, what string, err error, c *Client, unsent bool, cause error) {
	t.Helper()
	var lost *connectionLost
	if !errors.Is(err, ErrConnectionLost) || !errors.As(err, &lost) {
		t.Fatalf("%s, on a running client, answered %v, want ErrConnectionLost", what, err)
	}
	if lost.client != c || lost.unsent != unsent {
		t.Fatalf("%s answered %#v, want the client's connectionLost with unsent %v", what, lost, unsent)
	}
	if errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrNoServers) {
		t.Fatalf("%s answered %v, which still matches nats.go's close error its callers read as lasting", what, err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("%s answered %q, want nats.go's own answer %q kept in its text", what, err, cause)
	}
}

// requireReconnected requires Reconnected to match err once the hold is restored: the client
// recovers its connection within the row's own wait.
func requireReconnected(t *testing.T, what string, hold *testnats.ReplyHold, err error) {
	t.Helper()
	hold.Restore()
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if !Reconnected(ctx, err) {
		t.Fatalf("Reconnected did not match %s's %v once the hold was restored", what, err)
	}
}

// Each class of nats.go's answer for a connection's close (connectionCloses), given to a call the
// bus made directly on a running client's connection, is a lost connection (lostIfClosed), unsent
// as the call says, keeping nats.go's answer in its text, whether the connection is closed or
// connected again: a close answer for a call made on the connection itself comes from its close,
// its drain or a forced reconnect of it (TestADirectCallAForcedReconnectCutOffIsLostOnceTheConnectionIsBack),
// and Reconnected matches it once the client recovers its connection. The same answer stays
// nats.go's for a call on a connection the client has replaced (heldNow's nil), and on a stopped
// client.
//
// The control is this row with lostIfClosed answering err as it is: every class comes back as
// nats.go gave it.
func TestEveryCloseOfADirectCallOnARunningClientsConnectionIsLost(t *testing.T) {
	client, hold := heldClient(t)
	conn := client.Connection()
	for i, closed := range connectionCloses {
		requireLost(t, "a call on the connected connection nats.go answered "+closed.Error(), client.lostIfClosed(closed, conn, i%2 == 0), client, i%2 == 0, closed)
	}
	closeHeld(t, client, hold)
	for i, closed := range connectionCloses {
		requireLost(t, "a call on the closed connection nats.go answered "+closed.Error(), client.lostIfClosed(closed, conn, i%2 == 0), client, i%2 == 0, closed)
	}
	requireReconnected(t, "a call answered nats.ErrConnectionClosed", hold, client.lostIfClosed(nats.ErrConnectionClosed, conn, true))
	if err := client.lostIfClosed(nats.ErrConnectionClosed, client.heldNow(conn), true); !closedNotLost(err) {
		t.Fatalf("a call on the connection the client replaced answered %v, want nats.ErrConnectionClosed and not ErrConnectionLost", err)
	}
	client.Close()
	if err := client.lostIfClosed(nats.ErrConnectionClosed, conn, true); !closedNotLost(err) {
		t.Fatalf("a call on a stopped client answered %v, want nats.ErrConnectionClosed and not ErrConnectionLost", err)
	}
}

// A call whose wait for a connection dials one to replace the running client's closed connection,
// and finds no server to connect to, answers ErrConnectionLost, unsent, rather than nats.go's
// ErrNoServers: the client's connection is lost, and its recovery goes on dialling. Reconnected
// matches it once a server answers again, and the call made then succeeds. The gate's address has
// nothing listening while it is closed, so every dial is refused (testnats.Gate), which nats.go
// answers only once its ten attempts have run out (connectWithContext), about ten seconds.
//
// The control is this row with lostIfClosed taken out of ensureConnWithContext's dial: the
// subscribe answers nats.ErrNoServers.
func TestADialThatFindsNoServerToReplaceAClosedConnectionIsLost(t *testing.T) {
	srv, _ := testnats.Start(t)
	gate := testnats.StartGate(t, srv)
	client, err := Connect([]string{gate.URL()})
	if err != nil {
		t.Fatalf("connect through the gate: %v", err)
	}
	t.Cleanup(client.Close)
	gate.Close()
	client.Connection().Close()
	const subject = "envoy.closelost.dial"
	_, err = client.SubscribeCore(subject, func(*nats.Msg) {})
	requireLost(t, "a subscribe whose dial found no server", err, client, true, nats.ErrNoServers)
	gate.Open()
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if !Reconnected(ctx, err) {
		t.Fatalf("Reconnected did not match %v once the gate was open", err)
	}
	if _, err := client.SubscribeCore(subject, func(*nats.Msg) {}); err != nil {
		t.Fatalf("the subscribe made on the recovered connection answered %v", err)
	}
}

// A core publish whose flush the connection's drop cut off, its PONG still due, answers
// ErrConnectionLost at the drop, while the client is still disconnected, rather than the
// nats.ErrConnectionClosed nats.go answers that flush with (processOpErr clears the pending
// flushes): the publish may have reached the server, so it is not unsent. Reconnected matches it
// once the hold is restored, and a publish made then is confirmed. The hold keeps the flush's PONG
// from the client (testnats.ReplyHold.HoldFlush), so the flush is waiting for it when the hold is
// cut, and the cut refuses every reconnect until the row has its answer. The held publish is made
// through publishConfirmed, the call PublishCoreContext makes and the one that names the close, under
// the row's own deadline (ownWait) rather than PublishCoreContext's publishBound, so the order
// decides the row and not how soon the box gets to the cut.
//
// The control is this row with lostIfClosed taken out of publishConfirmed's flush: the publish
// answers "bus: flush publish to ...: nats: connection closed".
func TestACorePublishWhoseFlushTheDropCutOffIsLost(t *testing.T) {
	client, hold := heldClient(t)
	const subject = "envoy.closelost.core"
	item := contracts.Envelope{Topic: subject, Source: "test", DedupeKey: "close-lost-core"}
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	held := hold.HoldFlush(subject)
	answered := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
		defer cancel()
		conn := client.Connection()
		answered <- client.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Data: data}, conn.PublishMsg)
	}()
	select {
	case <-held.Withheld():
	case err := <-answered:
		t.Fatalf("the core publish answered %v before the hold withheld its flush's PONG", err)
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the hold had not withheld the core publish's flush by the test's deadline")
	}
	hold.Cut()
	t.Cleanup(hold.Restore)
	err = nil
	select {
	case err = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the core publish whose flush the drop cut off was still waiting at the test's deadline")
	}
	if client.Connected() {
		t.Fatalf("the core publish whose flush the drop cut off answered %v only once the client was connected again", err)
	}
	requireLost(t, "a core publish whose flush the drop cut off", err, client, false, nats.ErrConnectionClosed)
	requireReconnected(t, "the core publish", hold, err)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if err := client.PublishCoreContext(ctx, subject, item); err != nil {
		t.Fatalf("the core publish made on the recovered connection answered %v", err)
	}
}

// A JetStream subscribe made on a running client's closed connection answers ErrConnectionLost,
// rather than the nats.ErrConnectionClosed nats.go refuses it with, and Reconnected matches it once
// the hold is restored. The registration the failed bind left stays, so the client's recovery
// binds it on the connection that replaces the closed one (SubOK).
//
// The control is this row with lostIfClosed taken out of registerSubscription: the subscribe
// answers nats.ErrConnectionClosed.
func TestASubscribeOnARunningClientsClosedConnectionIsLost(t *testing.T) {
	client, hold := heldClient(t)
	closeHeld(t, client, hold)
	_, err := client.Subscribe(contracts.AgentSubject(contracts.NoProject, "ses_close_lost_subscribe"), func(*nats.Msg) {})
	requireLost(t, "a JetStream subscribe on the closed connection", err, client, false, nats.ErrConnectionClosed)
	requireReconnected(t, "the JetStream subscribe", hold, err)
	for !client.SubOK() {
		if testnats.Remaining(t) <= 0 {
			t.Fatal("the recovery had not bound the subscription's registration by the test's deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A KV watch created on a handle of a running client's connection once that connection is closed
// answers ErrConnectionLost, rather than the nats.ErrConnectionClosed nats.go refuses it with, and
// Reconnected matches it once the hold is restored. The handle was opened on the closed connection,
// which the client has then replaced, so a watch made on it again answers nats.ErrConnectionClosed,
// which no retry answers, and not ErrConnectionLost, which its caller would retry for ever; a
// handle opened on the client's connection watches the bucket, and once the client is stopped the
// same handle's watch answers nats.go's own error.
//
// The control is this row with lostIfClosed taken out of WatchKeys: the first watch answers
// nats.ErrConnectionClosed.
func TestAWatchOnARunningClientsClosedConnectionIsLost(t *testing.T) {
	client, hold := heldClient(t)
	kv := lostReplyBucket(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	closeHeld(t, client, hold)
	_, err := kv.WatchKeys(ctx, jetstream.AllKeys)
	requireLost(t, "a KV watch on the closed connection", err, client, false, nats.ErrConnectionClosed)
	requireReconnected(t, "the KV watch", hold, err)
	if _, err := kv.WatchKeys(ctx, jetstream.AllKeys); !closedNotLost(err) {
		t.Fatalf("a watch on the handle of the replaced connection answered %v, want nats.ErrConnectionClosed and not ErrConnectionLost", err)
	}
	fresh := lostReplyBucket(t, client)
	watcher, err := fresh.WatchKeys(ctx, jetstream.AllKeys)
	if err != nil {
		t.Fatalf("a watch on a handle of the client's connection answered %v", err)
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("stop the watch: %v", err)
	}
	DrainWatcher(watcher)
	client.Close()
	if _, err := fresh.WatchKeys(ctx, jetstream.AllKeys); !closedNotLost(err) {
		t.Fatalf("a watch on a stopped client answered %v, want nats.ErrConnectionClosed and not ErrConnectionLost", err)
	}
}

// An acknowledgement of a pulled message, and the pull's detach, made once the running client's
// connection the message arrived on is closed, answer ErrConnectionLost, unsent, rather than the
// nats.ErrConnectionClosed nats.go refuses each of them with before writing anything, and
// Reconnected matches the acknowledgement's once the hold is restored. The message's connection is
// then one the client has replaced, so its acknowledgement made again answers
// nats.ErrConnectionClosed, which no retry answers.
//
// The control is this row with lostIfClosed taken out of the pull's calls (SessionPull.lostIfClosed
// answering err as it is): the acknowledgement answers nats.ErrConnectionClosed.
func TestAnAckOnARunningClientsClosedConnectionIsLost(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	owner, err := ConnectOwningStream([]string{hold.URL()}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	const sessionID = "ses_close_lost_ack"
	subject := contracts.AgentSubject(contracts.NoProject, sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := consumers.Ensure(ctx, sessionID, []string{subject}, 0); err != nil {
		t.Fatalf("create the session's consumer: %v", err)
	}
	publishStored(t, owner, subject, "held")
	pull := pullSession(t, consumers, sessionID)
	msg := next(t, pull)
	closeHeld(t, consumers.client, hold)
	ackErr := msg.Ack()
	requireLost(t, "an acknowledgement on the closed connection", ackErr, consumers.client, true, nats.ErrConnectionClosed)
	if err := pull.Detach(); !errors.Is(err, ErrConnectionLost) || errors.Is(err, nats.ErrConnectionClosed) {
		t.Fatalf("the pull's detach on the running client's closed connection answered %v, want ErrConnectionLost", err)
	}
	requireReconnected(t, "the acknowledgement", hold, ackErr)
	if err := msg.Ack(); !closedNotLost(err) {
		t.Fatalf("an acknowledgement on the connection the client replaced answered %v, want nats.ErrConnectionClosed and not ErrConnectionLost", err)
	}
}

// A pull's own request made once the running client's connection the pull is on has closed, before
// the client has ended that connection's life, answers ErrConnectionLost, unsent, rather than the
// nats.ErrConnectionClosed nats.go refuses it with before writing anything (nats.go v1.55.0
// nats.go:4740): the order a close gives a pull whose next round starts at the close, since the
// pull sends its request while it reads the life as running (SessionPull.pull). The gate holds the
// life's end (withLifeEndGate), so the request is made in that order whatever the box's speed, and
// Reconnected matches its answer once the gate is released and the hold restored.
//
// The control is this row with lostIfClosed taken out of pull: the request answers
// nats.ErrConnectionClosed.
func TestAPullRequestOnARunningClientsClosedConnectionIsLost(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	owner, err := ConnectOwningStream([]string{hold.URL()}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	gate := newLifeEndHold()
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil, withLifeEndGate(gate.gate))
	if err != nil {
		t.Fatalf("connect the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	t.Cleanup(gate.release)
	const sessionID = "ses_close_lost_pull"
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	if _, err := consumers.Ensure(ctx, sessionID, []string{contracts.AgentSubject(contracts.NoProject, sessionID)}, 0); err != nil {
		t.Fatalf("create the session's consumer: %v", err)
	}
	pull := pullSession(t, consumers, sessionID)
	closeHeld(t, consumers.client, hold)
	select {
	case <-gate.held:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("no end of the closed connection's life reached the gate by the test's deadline")
	}
	err = pull.pull()
	gate.release()
	requireLost(t, "a pull's request on the closed connection", err, consumers.client, true, nats.ErrConnectionClosed)
	requireReconnected(t, "the pull's request", hold, err)
}
