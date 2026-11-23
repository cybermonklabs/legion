package testnats

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// A hold reports a reply withheld once the server's answer to a held request reaches it, and a
// flush only once the PONG of a PING the client sent after that request has reached the client:
// a request that sent no PING is reported withheld and never flushed. That is what lets a test
// drop the connection while a flushing request waits for its reply, rather than while it is still
// in its flush, which nats.go ends at the drop by itself (processOpErr clears the pending flushes).
//
// The control is the hold at 2b8a5d7cbb, whose one channel closed 50 ms after the withheld reply
// whether a PONG followed it or not: the same request is reported ready with no PING sent.
func TestAReplyHoldReportsAFlushOnlyOnceItsPongReachesTheClient(t *testing.T) {
	url := URL(t)
	responder := Connect(t, url)
	if _, err := responder.Subscribe("hold.test.request", func(m *natsgo.Msg) { _ = m.Respond([]byte("answer")) }); err != nil {
		t.Fatalf("subscribe the responder: %v", err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatalf("flush the responder's subscription: %v", err)
	}
	hold := StartReplyHold(t, url)
	client := Connect(t, hold.URL())
	inbox := natsgo.NewInbox()
	replies, err := client.SubscribeSync(inbox)
	if err != nil {
		t.Fatalf("subscribe the reply inbox: %v", err)
	}
	held := hold.Hold("hold.test.")
	// A publish with no flush behind it: nats.go's flusher writes it, and sends no PING.
	if err := client.PublishRequest("hold.test.request", inbox, []byte("question")); err != nil {
		t.Fatalf("send the request: %v", err)
	}
	select {
	case <-held.Withheld():
	case <-time.After(Remaining(t)):
		t.Fatal("the hold had not withheld the reply by the test's deadline")
	}
	// An absence, which a slow box can only miss, never invent: the flush is not reported while
	// the client has sent none.
	select {
	case <-held.Flushed():
		t.Fatal("the hold reported a flush the client never sent")
	case <-time.After(200 * time.Millisecond):
	}
	if err := client.Flush(); err != nil {
		t.Fatalf("flush the client: %v", err)
	}
	select {
	case <-held.Flushed():
	case <-time.After(Remaining(t)):
		t.Fatal("the hold had not reported the flush by the test's deadline, its PONG forwarded")
	}
	if msg, err := replies.NextMsg(100 * time.Millisecond); err == nil {
		t.Fatalf("the held reply reached the client: %q", msg.Data)
	}
}

// A flush hold keeps from the client the PONG of the flush that follows a publish on its prefix, so
// that flush waits until the connection drops, which nats.go answers it with as a close
// (nats.ErrConnectionClosed: processOpErr clears the pending flushes). A flush after a publish on
// another subject, made first, is answered as ever. That is what lets a test drop the connection
// while a publish's flush is still waiting for its PONG, the order a drop gives a flush in flight.
//
// The control is this row with keepPong reporting false for every PONG: the held flush returns nil
// before the drop.
func TestAFlushHoldKeepsThePongOfAPublishsFlushUntilTheDrop(t *testing.T) {
	hold := StartReplyHold(t, URL(t))
	client := Connect(t, hold.URL())
	held := hold.HoldFlush("hold.flush.")
	if err := client.Publish("hold.other", []byte("other")); err != nil {
		t.Fatalf("publish on another subject: %v", err)
	}
	if err := client.Flush(); err != nil {
		t.Fatalf("the flush after a publish the hold does not name answered %v, want it answered", err)
	}
	if err := client.Publish("hold.flush.one", []byte("held")); err != nil {
		t.Fatalf("publish on the held prefix: %v", err)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- client.FlushTimeout(Remaining(t)) }()
	select {
	case <-held.Withheld():
	case err := <-flushed:
		t.Fatalf("the held publish's flush answered %v before the hold withheld its PONG", err)
	case <-time.After(Remaining(t)):
		t.Fatal("the hold had not withheld the flush's PONG by the test's deadline")
	}
	// An absence, which a slow box can only miss, never invent: the flush is still waiting.
	select {
	case err := <-flushed:
		t.Fatalf("the flush whose PONG the hold keeps answered %v before the drop", err)
	case <-time.After(200 * time.Millisecond):
	}
	hold.Drop()
	select {
	case err := <-flushed:
		if !errors.Is(err, natsgo.ErrConnectionClosed) {
			t.Fatalf("the flush the drop cut off answered %v, want nats.ErrConnectionClosed", err)
		}
	case <-time.After(Remaining(t)):
		t.Fatal("the flush the drop cut off was still waiting at the test's deadline")
	}
}

// A cut hold keeps its clients disconnected: a client through it loses its connection at the Cut,
// makes no reconnect in five of its reconnect waits, and reconnects after Restore. That is what
// lets a test see what a client does while it is still disconnected, however long the box takes,
// where Drop lets it reconnect at its next attempt. The rows await the client's own disconnect and
// reconnect callbacks, which nats.go reports on buffered channels however late the test goroutine
// runs, so neither row misses an event between two looks at the client's state; every wait is
// bounded by the test's deadline alone. The cut row's absence check is a cut's to keep however slow
// the box is.
//
// The control is Drop in the same rig: the client reconnects without a Restore.
func TestACutHoldRefusesEveryReconnectUntilRestore(t *testing.T) {
	url := URL(t)
	const wait = 100 * time.Millisecond
	connectThrough := func(t *testing.T, hold *ReplyHold) (*natsgo.Conn, <-chan struct{}, <-chan struct{}) {
		t.Helper()
		down, up := make(chan struct{}, 8), make(chan struct{}, 8)
		client, err := natsgo.Connect(hold.URL(),
			natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(wait),
			natsgo.DisconnectErrHandler(func(*natsgo.Conn, error) { down <- struct{}{} }),
			natsgo.ReconnectHandler(func(*natsgo.Conn) { up <- struct{}{} }),
		)
		if err != nil {
			t.Fatalf("connect through the hold: %v", err)
		}
		t.Cleanup(client.Close)
		return client, down, up
	}
	t.Run("cut", func(t *testing.T) {
		hold := StartReplyHold(t, url)
		client, down, up := connectThrough(t, hold)
		hold.Cut()
		waitSignal(t, down, Remaining(t), "the client's disconnect after the Cut")
		time.Sleep(5 * wait)
		select {
		case <-up:
			t.Fatal("the client reconnected through a cut hold")
		default:
		}
		if client.IsConnected() {
			t.Fatal("the client is connected through a cut hold")
		}
		hold.Restore()
		waitSignal(t, up, Remaining(t), "the client's reconnect after Restore")
	})
	t.Run("drop, the control", func(t *testing.T) {
		hold := StartReplyHold(t, url)
		_, down, up := connectThrough(t, hold)
		hold.Drop()
		waitSignal(t, down, Remaining(t), "the client's disconnect after the Drop")
		waitSignal(t, up, Remaining(t), "the client's reconnect after the Drop, with no Restore")
	})
}
