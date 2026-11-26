package bus

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A send of the core subscription begun while its connection reconnects gives no verdict. sendCore
// compares the connection's last error before the bind with the one after the flush, and nats.go
// sets that error anew within a reconnect: nil as the reconnect begins (doReconnect), nil after
// each failed attempt, and the handshake's result once an attempt connects (processConnectInit). A
// verdict made across that compares two sessions: a refusal before and nil after counted a
// subscription the new server accepted as refused, and ResendRefusedCore then tried the machine's
// group and dropped the subscription the client held.
//
// Here the server stops and the client starts to reconnect. The relay then freezes and the server
// starts again, granting the subscription: an attempt can connect to the relay but its handshake
// cannot finish. sendCore runs now. Without a check of the session as the send begins, nats.go kept
// the subscription for the reconnect and put the flush's PING in its reconnect buffer; the relay
// thaws once that PING is buffered, the reconnect finishes, the server accepts the subscription,
// and the PING's PONG ends the flush. sendCore must answer with an error, not a verdict made across
// the two sessions.
//
// The two tests after this one place a change of session inside a send that began on a connected
// connection, with the hooks around sendCore's flush.
func TestSendCoreGivesNoVerdictForASendAReconnectOvertook(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "send-core-reconnect"
	)
	seed, public := testnats.User(t)
	refusing := testnats.SubscribeGrantConfig(public, "_INBOX.>")
	granting := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, refusing, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if _, err := client.SubscribeCore(subject, func(*nats.Msg) {}, queue); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	conn := client.Conn
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	server.Stop(t)
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to read the stop", func() bool {
		return conn.Status() == nats.RECONNECTING
	})
	thaw := server.Freeze(t)
	t.Cleanup(thaw)
	server.Restart(t, granting)
	stale := conn.LastError()
	if conn.Status() != nats.RECONNECTING {
		t.Fatalf("the connection reads %v before the send; the test proves nothing", conn.Status())
	}
	reconnects := conn.Stats().Reconnects

	type answer struct {
		sub      *nats.Subscription
		accepted bool
		err      error
	}
	answered := make(chan answer, 1)
	go func() {
		sub, accepted, err := client.sendCore(recoverableSubscription{transport: coreSubscription, subject: subject, queue: queue, handler: func(*nats.Msg) {}}, conn, client.js, reconnects)
		answered <- answer{sub, accepted, err}
	}()
	// The PING is the one thing a reconnecting connection buffers here: the bind's SUB waits for
	// the reconnect, which sends every subscription again. A send that ended before it bound
	// anything buffers nothing, so the wait also ends when the answer arrives.
	var got answer
	pinged, done := false, false
	testwait.WithinPolling(t, 10*time.Second, time.Millisecond, "the flush's PING in the reconnect buffer, or the send's answer", func() bool {
		select {
		case got = <-answered:
			done = true
			return true
		default:
		}
		buffered, err := conn.Buffered()
		pinged = err == nil && buffered > 0
		return pinged
	})
	if pinged && conn.Status() != nats.RECONNECTING {
		t.Fatalf("the connection reads %v once the PING is buffered; the test proves nothing", conn.Status())
	}
	thaw()
	if !done {
		got = testwait.Receive(t, "sendCore to answer", answered)
	}
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to reconnect", func() bool {
		return conn.Status() == nats.CONNECTED && conn.Stats().Reconnects > reconnects
	})
	if errors.Is(got.err, nats.ErrTimeout) {
		t.Fatalf("the flush timed out, so no answer was read; the test proves nothing: %v", got.err)
	}

	// The restarted server accepted every subscription it was sent. nats.go reads CONNECTED before
	// it sends the subscriptions again, under the connection's lock, so this flush's PING follows
	// them and its PONG comes once the server has read, and traced, every one.
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the reconnected connection: %v", err)
	}
	trace := server.Log()
	trace = trace[strings.LastIndex(trace, "Starting nats-server"):]
	if sent := strings.Count(trace, "<<- [SUB "+subject+" "+queue+" "); sent == 0 {
		t.Fatalf("the restarted server received no subscription to %s in %s; the test proves nothing", subject, queue)
	}
	if refused := strings.Count(trace, "Permissions Violation for Subscription"); refused != 0 {
		t.Fatalf("the restarted server refused %d subscriptions; the test proves nothing", refused)
	}

	if got.err == nil {
		_ = got.sub.Unsubscribe()
		t.Fatalf("sendCore answered accepted=%v for a send its connection's reconnect overtook (last error %v as the send began, %v after it); want an error and no verdict", got.accepted, stale, conn.LastError())
	}
	if got.sub != nil {
		t.Fatalf("sendCore returned a subscription beside its error %v", got.err)
	}
}

// A send whose connection reconnects between the bind and the server's answer gives no verdict,
// and the subscription it made is drained, not unsubscribed. nats.go sends that subscription again
// with every other one as it reconnects, so the new server may accept it and deliver to it; the
// answer the flush then reads is the new session's, and the last error was cleared on the way.
//
// The hook before the flush restarts the server, which grants the subscription, waits for the
// client to reconnect and for the server to hold the subscription nats.go sent again, and has two
// messages published to it. The handler holds the first, and the second waits behind it. sendCore
// must end with errSendSuperseded and leave both messages to the handler: an unsubscribe would drop
// the second.
func TestSendCoreGivesNoVerdictAndDrainsASendItsConnectionReconnectedUnder(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "send-core-reconnected"
		topic   = "notifications.team.send-core-reconnected"
	)
	seed, public := testnats.User(t)
	granting := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granting, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	var conn *nats.Conn
	hooked := false
	client, err := Connect([]string{uri}, WithBeforeCoreSendFlush(func() {
		hooked = true
		reconnects := conn.Stats().Reconnects
		server.Restart(t, granting)
		testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to reconnect inside the send", func() bool {
			return conn.Status() == nats.CONNECTED && conn.Stats().Reconnects > reconnects
		})
		// nats.go reads CONNECTED before it sends the subscriptions again, under the connection's
		// lock, so this flush's PONG comes once the server holds the subscription it was sent.
		if err := conn.Flush(); err != nil {
			t.Fatalf("flush the reconnected connection: %v", err)
		}
		publisher, err := Dial("send-core-publisher", []string{uri}, os.LookupEnv)
		if err != nil {
			t.Fatalf("connect the publisher: %v", err)
		}
		defer publisher.Close()
		for range 2 {
			if err := publisher.Publish(topic, []byte("{}")); err != nil {
				t.Fatalf("publish %s: %v", topic, err)
			}
		}
		// The server delivers each message as it reads it, so both are on their way to the client
		// before this flush's PONG, and so before the PONG of the send's own flush.
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush the publishes: %v", err)
		}
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn = client.Conn
	release := make(chan struct{})
	var free sync.Once
	let := func() { free.Do(func() { close(release) }) }
	t.Cleanup(let)
	handled := make(chan string, 2)
	handler := func(msg *nats.Msg) {
		<-release
		handled <- msg.Subject
	}

	start := conn.Stats().Reconnects
	sub, accepted, err := client.sendCore(recoverableSubscription{transport: coreSubscription, subject: subject, queue: queue, handler: handler}, conn, client.js, start)
	if !hooked {
		t.Fatal("the hook before the flush never ran; the test proves nothing")
	}
	if errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("the flush timed out, so no answer was read; the test proves nothing: %v", err)
	}
	trace := server.Log()
	trace = trace[strings.LastIndex(trace, "Starting nats-server"):]
	if delivered := strings.Count(trace, "->> [MSG "+topic+" "); delivered != 2 {
		t.Fatalf("the restarted server delivered %d messages on %s, want 2; the test proves nothing", delivered, topic)
	}
	if !errors.Is(err, errSendSuperseded) || sub != nil {
		if sub != nil {
			_ = sub.Unsubscribe()
		}
		t.Fatalf("sendCore answered accepted=%v, error %v, for a send its connection reconnected under; want errSendSuperseded and no subscription", accepted, err)
	}
	let()
	for _, which := range []string{"first", "second"} {
		if got := testwait.Receive(t, "the "+which+" of the 2 messages the new server delivered to the subscription the send made to reach the handler", handled); got != topic {
			t.Fatalf("the handler was handed %s, want %s", got, topic)
		}
	}
}

// A drained attempt is not sent again by a reconnect during its drain. nats.go keeps a draining
// subscription in its set until the handler has taken what it holds, so a reconnect in that time
// sends it again, and checkDrained then removes it with no UNSUB ever written: the server would
// hold a member of the attempt's queue group that the client no longer reads. The drain is watched
// (watchDrain), so the reconnect finds it unsubscribed.
//
// As in the test above, a restart inside the send leaves the attempt with two messages, the
// handler holding the first, and sendCore drains it. While the handler still holds it, the server
// restarts again. Once the client has reconnected, the server must hold no subscription in the
// attempt's queue group.
func TestAReleasedAttemptIsNotSentAgainByAReconnectDuringItsDrain(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "send-core-drain-reconnect"
		topic   = "notifications.team.send-core-drain-reconnect"
	)
	seed, public := testnats.User(t)
	granting := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granting, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	var conn *nats.Conn
	restartAndReconnect := func(what string) {
		reconnects := conn.Stats().Reconnects
		server.Restart(t, granting)
		testwait.Until(t, what, func() (nats.Status, bool) {
			status := conn.Status()
			return status, status == nats.CONNECTED && conn.Stats().Reconnects > reconnects
		})
		// nats.go reads CONNECTED before it sends the subscriptions again, under the connection's
		// lock, so this flush's PONG comes once the server holds every subscription it was sent.
		if err := conn.Flush(); err != nil {
			t.Fatalf("flush the reconnected connection: %v", err)
		}
	}
	client, err := Connect([]string{uri}, WithBeforeCoreSendFlush(func() {
		restartAndReconnect("the connection to reconnect inside the send")
		publisher, err := Dial("send-core-publisher", []string{uri}, os.LookupEnv)
		if err != nil {
			t.Fatalf("connect the publisher: %v", err)
		}
		defer publisher.Close()
		for range 2 {
			if err := publisher.Publish(topic, []byte("{}")); err != nil {
				t.Fatalf("publish %s: %v", topic, err)
			}
		}
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush the publishes: %v", err)
		}
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn = client.Conn
	release := make(chan struct{})
	var free sync.Once
	let := func() { free.Do(func() { close(release) }) }
	t.Cleanup(let)
	entered := make(chan struct{}, 2)
	handler := func(*nats.Msg) {
		entered <- struct{}{}
		<-release
	}

	start := conn.Stats().Reconnects
	sub, _, err := client.sendCore(recoverableSubscription{transport: coreSubscription, subject: subject, queue: queue, handler: handler}, conn, client.js, start)
	if !errors.Is(err, errSendSuperseded) || sub != nil {
		t.Fatalf("sendCore answered error %v, subscription %v; want errSendSuperseded and no subscription, the drain this test needs", err, sub)
	}
	testwait.Receive(t, "the handler to be handed a message, without which the drain has nothing pending and the test proves nothing", entered)

	restartAndReconnect("the connection to reconnect during the drain")
	trace := server.Log()
	trace = trace[strings.LastIndex(trace, "Starting nats-server"):]
	if resent := strings.Count(trace, "<<- [SUB "+subject+" "+queue+" "); resent != 0 {
		t.Fatalf("the server restarted during the drain received the drained attempt's subscription %d times; it holds a queue-group member the client no longer reads", resent)
	}
}

// A released attempt's drain is not cut by the late callback of the reconnect it began after.
// nats.go queues a reconnect's callback (onReconnect, which reports the connection event) under the
// connection's lock, and the PONG of a flush needs that lock too, so the flush can return, sendCore
// release its attempt and the drain begin on the reconnected session before that callback has run.
// The event the callback then reports wakes the drain's watch (watchDrain), whose channel was taken
// before it, but the session it reports is the one the drain began in: an unsubscribe there would
// drop what that session delivered to the attempt.
//
// As in TestSendCoreGivesNoVerdictAndDrainsASendItsConnectionReconnectedUnder, a restart inside the
// send leaves the attempt with two messages, the handler holding the first, and sendCore drains it.
// Here the client's callback goroutine is also held, in a disconnect hook, until sendCore has
// returned, so the reconnect's callback runs after the drain began. Once it has run, the handler
// is let go and must be handed both messages.
func TestALateReconnectCallbackDoesNotCutAReleasedAttemptsDrain(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "send-core-late-callback"
		topic   = "notifications.team.send-core-late-callback"
	)
	seed, public := testnats.User(t)
	granting := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granting, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	var conn *nats.Conn
	client, err := Connect([]string{uri}, WithBeforeCoreSendFlush(func() {
		reconnects := conn.Stats().Reconnects
		server.Restart(t, granting)
		testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to reconnect inside the send", func() bool {
			return conn.Status() == nats.CONNECTED && conn.Stats().Reconnects > reconnects
		})
		if err := conn.Flush(); err != nil {
			t.Fatalf("flush the reconnected connection: %v", err)
		}
		publisher, err := Dial("send-core-publisher", []string{uri}, os.LookupEnv)
		if err != nil {
			t.Fatalf("connect the publisher: %v", err)
		}
		defer publisher.Close()
		for range 2 {
			if err := publisher.Publish(topic, []byte("{}")); err != nil {
				t.Fatalf("publish %s: %v", topic, err)
			}
		}
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush the publishes: %v", err)
		}
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn = client.Conn
	// nats.go runs its callbacks on one goroutine, in order, so the reconnect's callback waits
	// behind the restart's disconnect callback, which this hook holds until unhold.
	held := make(chan struct{})
	unhold := sync.OnceFunc(func() { close(held) })
	t.Cleanup(unhold)
	client.AddDisconnectHook(func() {
		select {
		case <-held:
		case <-time.After(30 * time.Second):
		}
	})
	var callbackBegan atomic.Bool
	callbackRan := make(chan struct{})
	ran := sync.OnceFunc(func() { close(callbackRan) })
	onReconnect := conn.ReconnectHandler()
	conn.SetReconnectHandler(func(nc *nats.Conn) {
		callbackBegan.Store(true)
		onReconnect(nc)
		ran()
	})
	release := make(chan struct{})
	let := sync.OnceFunc(func() { close(release) })
	t.Cleanup(let)
	handled := make(chan string, 2)
	handler := func(msg *nats.Msg) {
		<-release
		handled <- msg.Subject
	}

	start := conn.Stats().Reconnects
	sub, _, err := client.sendCore(recoverableSubscription{transport: coreSubscription, subject: subject, queue: queue, handler: handler}, conn, client.js, start)
	if callbackBegan.Load() {
		t.Fatal("the reconnect's callback began before sendCore returned; the drain did not begin before it and the test proves nothing")
	}
	trace := server.Log()
	trace = trace[strings.LastIndex(trace, "Starting nats-server"):]
	if delivered := strings.Count(trace, "->> [MSG "+topic+" "); delivered != 2 {
		t.Fatalf("the restarted server delivered %d messages on %s, want 2; the test proves nothing", delivered, topic)
	}
	if !errors.Is(err, errSendSuperseded) || sub != nil {
		t.Fatalf("sendCore answered error %v, subscription %v; want errSendSuperseded and no subscription, the drain this test needs", err, sub)
	}
	unhold()
	testwait.Receive(t, "the reconnect's callback to run, without which the test proves nothing", callbackRan)
	// The watch wakes at the event the callback reported; this gives it the time to act on it
	// before the handler is let go. A watch slower than this could let a cut drain pass here,
	// never fail one that is kept.
	time.Sleep(200 * time.Millisecond)
	let()
	for _, which := range []string{"first", "second"} {
		if got := testwait.Receive(t, "the "+which+" of the 2 messages the new server delivered to the attempt to reach the handler (a drain the late callback of the reconnect it began after ended hands it none)", handled); got != topic {
			t.Fatalf("the handler was handed %s, want %s", got, topic)
		}
	}
}

// A send whose connection drops after the server's answer arrived, and before it is read, gives no
// verdict. nats.go turns the connection RECONNECTING and then clears its last error, and it counts
// the reconnect only once a dial succeeds, so with the server gone the count has not moved: the
// status is what shows the answer's session has ended. The subscription the send made is
// unsubscribed, not drained: a draining subscription stays in nats.go's set, and the reconnect would
// send it again with no UNSUB, a member of the queue group that the client no longer reads.
//
// The hook after the flush stops the server and waits for the connection to read RECONNECTING.
// sendCore must end with errSendSuperseded. The server then starts again, granting the
// subscription, and once the client has reconnected the server must have received no subscription
// in the send's queue group.
func TestSendCoreGivesNoVerdictForAnAnswerReadAfterItsConnectionDropped(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "send-core-dropped"
	)
	seed, public := testnats.User(t)
	granting := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granting, []string{"-DV", "-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	var conn *nats.Conn
	hooked := false
	client, err := Connect([]string{uri}, WithAfterCoreSendFlush(func() {
		hooked = true
		server.Stop(t)
		testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to read the stop", func() bool {
			return conn.Status() == nats.RECONNECTING
		})
	}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn = client.Conn

	start := conn.Stats().Reconnects
	sub, accepted, err := client.sendCore(recoverableSubscription{transport: coreSubscription, subject: subject, queue: queue, handler: func(*nats.Msg) {}}, conn, client.js, start)
	if !hooked {
		t.Fatal("the hook after the flush never ran; the test proves nothing")
	}
	if reconnects := conn.Stats().Reconnects; reconnects != start {
		t.Fatalf("the connection reconnected %d times inside the send; the test proves nothing about a drop the count does not show", reconnects-start)
	}
	if !errors.Is(err, errSendSuperseded) || sub != nil {
		if sub != nil {
			_ = sub.Unsubscribe()
		}
		t.Fatalf("sendCore answered accepted=%v, error %v, for an answer read after its connection dropped; want errSendSuperseded and no subscription", accepted, err)
	}

	server.Restart(t, granting)
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "the connection to reconnect", func() bool {
		return conn.Status() == nats.CONNECTED && conn.Stats().Reconnects > start
	})
	// nats.go reads CONNECTED before it sends the subscriptions again, so this flush's PONG comes
	// once the server has read every one.
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the reconnected connection: %v", err)
	}
	trace := server.Log()
	trace = trace[strings.LastIndex(trace, "Starting nats-server"):]
	if resent := strings.Count(trace, "<<- [SUB "+subject+" "+queue+" "); resent != 0 {
		t.Fatalf("the restarted server received the send's subscription %d times; the client sent again a subscription it gave no verdict on", resent)
	}
}
