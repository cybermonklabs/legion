package bus_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// listenerDrain is the drain the listener runs at shutdown (cmd/listener), which the tests below
// drain for: on a clocktest.Fake its length costs nothing.
const listenerDrain = 13 * time.Second

// deliveriesShare is when, from Drain's start on its clock, the delivery subscriptions stop being
// waited for: all of timeout but the part its hooks keep (bus.DrainHookReserve, or 30% of a
// shorter drain).
func deliveriesShare(timeout time.Duration) time.Duration {
	return timeout - min(bus.DrainHookReserve, timeout*3/10)
}

// connectWithHandler connects a client under options and subscribes a core handler that signals
// when a delivery reaches it and then holds it until release is closed.
func connectWithHandler(t *testing.T, release <-chan struct{}, options ...bus.ConnectOption) (*bus.Client, *atomic.Bool) {
	t.Helper()
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri}, options...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{})
	finished := &atomic.Bool{}
	if _, err := client.Conn.Subscribe("drain.in-flight", func(*natsgo.Msg) {
		close(started)
		<-release
		finished.Store(true)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Publish("drain.in-flight", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	testwait.Receive(t, "the delivery to reach its handler", started)
	return client, finished
}

// released is a gate already open, for a handler that returns at once.
func released() <-chan struct{} {
	gate := make(chan struct{})
	close(gate)
	return gate
}

// heldUntilCleanup is a gate the test opens only as it ends, for a handler that never returns
// within the test.
func heldUntilCleanup(t *testing.T) <-chan struct{} {
	gate := make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(gate) }))
	return gate
}

// drainOn starts client's Drain(timeout) on a goroutine and returns where its error arrives.
func drainOn(client *bus.Client, timeout time.Duration) <-chan error {
	drained := make(chan error, 1)
	go func() { drained <- client.Drain(timeout) }()
	return drained
}

// A drain lets a delivery already in its handler finish before the connection closes. nats.go's
// Drain only starts the drain, so returning when it returns would close under the handler. The
// handler's subscription is the connection's own, which the connection's drain waits for, and the
// drain's clock stands still: once it has armed its deadline for the connection to close it is
// waiting for the handler, which the test then lets finish.
func TestDrainLetsAnInFlightDeliveryFinish(t *testing.T) {
	clock := clocktest.New(fakeStart)
	release := make(chan struct{})
	client, finished := connectWithHandler(t, release, bus.WithClock(clock))
	drained := drainOn(client, listenerDrain)
	awaitArmedAt(t, clock, "Drain's wait for the connection to close behind the delivery in its handler", fakeStart.Add(listenerDrain).Equal)
	if len(drained) != 0 {
		t.Fatalf("Drain returned %v while a delivery was still in its handler", <-drained)
	}
	close(release)
	if err := testwait.Receive(t, "the drain to end once the delivery finished", drained); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Drain returned while a delivery was still in its handler")
	}
	if !client.Conn.IsClosed() {
		t.Fatal("Drain returned with the connection still open")
	}
}

// A drain that cannot finish closes the connection at its deadline, so a handler that never
// returns cannot keep a shutting-down process alive. The deadline is on the drain's clock, which
// the test moves to it once the drain has armed it.
func TestDrainClosesTheConnectionAtItsDeadline(t *testing.T) {
	clock := clocktest.New(fakeStart)
	client, _ := connectWithHandler(t, heldUntilCleanup(t), bus.WithClock(clock))
	drained := drainOn(client, listenerDrain)
	deadline := fakeStart.Add(listenerDrain)
	awaitArmedAt(t, clock, "Drain's deadline for the connection the handler holds open", deadline.Equal)
	clock.Set(deadline)
	if err := testwait.Receive(t, "the drain to end at its deadline", drained); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v, want deadline exceeded", err)
	}
	if !client.Conn.IsClosed() {
		t.Fatal("Drain returned at its deadline with the connection still open")
	}
}

// The drain's close is a shutdown, not a lost connection: the client does not reconnect and
// re-subscribe afterwards, which would hand deliveries to a process that is about to exit. The
// close starts a recovery, which finds the client stopped and says so; once it has, nothing is
// left to reconnect the client.
func TestDrainDoesNotReconnect(t *testing.T) {
	logs := captureBusLogs(t)
	name := t.Name()
	client, _ := connectWithHandler(t, released(), bus.WithName(name))
	if _, err := client.SubscribeCore("notifications.role.>", func(*natsgo.Msg) {}, "drain-test"); err != nil {
		t.Fatalf("subscribe role lane: %v", err)
	}
	if err := client.Drain(listenerDrain); err != nil {
		t.Fatalf("drain: %v", err)
	}
	testwait.Eventually(t, "the recovery the drain's close started to find the client stopped", func() bool {
		return logs.count(name, "envoy nats recovery cancelled") > 0
	})
	if client.Connected() {
		t.Fatal("the client reconnected after Drain closed its connection")
	}
}

// A drain lets a role-lane delivery finish its forward. The listener's role handler forwards each
// message to the holder with StartRequestStoredTo, which opens a reply subscription per forward;
// a connection that is already draining refuses new subscriptions, so the delivery subscriptions
// drain first, while the connection still accepts them. Each handler waits for the drain to begin
// before it forwards, so both forwards are published after the drain has begun; both are stored on
// the holder's agent subject, which the stream captures and acknowledges as a forward needs.
func TestDrainLetsARoleForwardInItsHandlerFinish(t *testing.T) {
	_, uri := startNATS(t)
	publisher := testnats.Connect(t, uri)
	t.Cleanup(publisher.Close)
	holderSubject := contracts.LegacyAgentSubject("drain-holder")

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithClock(clocktest.New(fakeStart)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	forwards := make(chan error, 2)
	started := make(chan struct{}, 2)
	drainBegun := make(chan struct{})
	// beginDrain opens the handlers' gate once the drain has begun, and the cleanup opens it for a
	// row that failed before then, so no handler is left blocked on it.
	beginDrain := sync.OnceFunc(func() { close(drainBegun) })
	t.Cleanup(beginDrain)
	lane, err := client.SubscribeCore("notifications.role.drain-test", func(message *natsgo.Msg) {
		started <- struct{}{}
		<-drainBegun
		forward, err := client.StartRequestStoredTo(holderSubject, contracts.Envelope{
			EventID:       "evt-" + string(message.Data),
			Source:        "agent",
			SourceEventID: "source-" + string(message.Data),
			Topic:         "notifications.role.drain-test",
			DedupeKey:     "drain-" + string(message.Data),
			IssuedAt:      contracts.NowMillis(),
		}, bus.ForwardMsgID("drain-"+string(message.Data), holderSubject), 2*time.Second)
		if err != nil {
			forwards <- err
			return
		}
		forwards <- forward.Wait()
	}, "drain-test")
	if err != nil {
		t.Fatalf("role subscribe: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush role subscription: %v", err)
	}
	for _, body := range []string{"first", "second"} {
		if err := publisher.Publish("notifications.role.drain-test", []byte(body)); err != nil {
			t.Fatalf("publish %s: %v", body, err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush publishes: %v", err)
	}
	testwait.Receive(t, "the first role message to reach its handler", started)

	drained := drainOn(client, listenerDrain)
	testwait.Eventually(t, "Drain to drain the role lane", lane.IsDraining)
	beginDrain()
	if err := testwait.Receive(t, "the drain to end once the forwards finished", drained); err != nil {
		t.Fatalf("drain: %v", err)
	}
	for index := range 2 {
		select {
		case err := <-forwards:
			if err != nil {
				t.Fatalf("forward %d failed during the drain: %v", index+1, err)
			}
		default:
			t.Fatalf("Drain returned with forward %d not finished", index+1)
		}
	}
}

// After a drain the client stays down: a publish from a request still in flight at shutdown gets
// an error rather than dialling a new connection nothing will drain.
func TestAPublishAfterDrainDoesNotReconnect(t *testing.T) {
	client, _ := connectWithHandler(t, released())
	if err := client.Drain(listenerDrain); err != nil {
		t.Fatalf("drain: %v", err)
	}
	err := client.Publish(contracts.Envelope{
		EventID:       "evt-after-drain",
		Source:        "agent",
		SourceEventID: "source-after-drain",
		Topic:         "notifications.role.after-drain",
		DedupeKey:     "after-drain",
		IssuedAt:      contracts.NowMillis(),
	})
	if err == nil {
		t.Fatal("a publish after Drain succeeded")
	}
	if client.Connected() {
		t.Fatal("a publish after Drain reconnected the client")
	}
}

// A delivery subscription whose handler outlasts the deadline does not hold the drain: Drain waits
// for the subscriptions it drains only until the deliveries' share of its deadline, then closes the
// connection. The share is on the drain's clock, which the test moves to it once armed.
func TestDrainClosesAtItsDeadlineWhileADeliverySubscriptionDrains(t *testing.T) {
	_, uri := startNATS(t)
	clock := clocktest.New(fakeStart)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithClock(clock))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{}, 1)
	held := heldUntilCleanup(t)
	if _, err := client.SubscribeCore("notifications.role.drain-deadline", func(*natsgo.Msg) {
		started <- struct{}{}
		<-held
	}, "drain-deadline"); err != nil {
		t.Fatalf("role subscribe: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-deadline", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	testwait.Receive(t, "the delivery to reach its handler", started)
	drained := drainOn(client, listenerDrain)
	share := fakeStart.Add(deliveriesShare(listenerDrain))
	awaitArmedAt(t, clock, "Drain's deadline for the delivery subscription", share.Equal)
	clock.Set(share)
	if err := testwait.Receive(t, "the drain to end at its deadline", drained); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v, want deadline exceeded", err)
	}
	if !client.Conn.IsClosed() {
		t.Fatal("Drain returned at its deadline with the connection still open")
	}
}

// A drain that starts while the connection is reconnecting closes it at once: nothing sent while
// reconnecting reaches the server, so a drained subscription would keep its server-side SUB (a
// reconnect re-sends it) and the drain would only run out its deadline, which is on a clock that
// stands still here, so such a drain would never return.
func TestDrainWhileReconnectingClosesAtOnce(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithClock(clocktest.New(fakeStart)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if _, err := client.SubscribeCore("notifications.role.drain-reconnecting", func(*natsgo.Msg) {}, "drain-reconnecting"); err != nil {
		t.Fatalf("role subscribe: %v", err)
	}
	srv.Stop(t)
	testwait.Eventually(t, "the client to start reconnecting", func() bool { return client.Conn.IsReconnecting() })

	testwait.Receive(t, "the drain while reconnecting to return with its clock standing still", drainOn(client, listenerDrain))
	if !client.Conn.IsClosed() {
		t.Fatal("Drain returned with the reconnecting connection still open")
	}
}

// Drain does not wait for a bind in flight. A reconnect can lose a bind's request, which JetStream
// then waits out for 10 s, so a shutdown that lands meanwhile, with the listener's drain budget,
// would spend it on a subscription that delivers nothing yet. Drain returns once the connection has
// drained, and the close ends the bind, which binds nothing: the bind's end is the close's, not its
// own wait running out, which is what a drain that waited for it would leave.
func TestDrainDoesNotWaitForABindInFlight(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.url()}, bus.WithClock(clocktest.New(fakeStart)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	const consumer = "drain-bind-in-flight"
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        consumer,
		DeliverSubject: natsgo.NewInbox(),
		AckPolicy:      natsgo.AckExplicitPolicy,
		FilterSubjects: bus.StreamSubjects(),
	}); err != nil {
		t.Fatalf("add the durable: %v", err)
	}
	bound := make(chan error, 1)
	proxy.arm("$JS.API.CONSUMER.INFO.")
	go func() {
		_, err := client.Subscribe("", func(msg *natsgo.Msg) { _ = msg.Ack() }, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
		bound <- err
	}()
	testwait.Receive(t, "the bind's consumer lookup to reach the link", proxy.swallowed)
	proxy.drop()
	testwait.Eventually(t, "the connection to reconnect in place", func() bool {
		return client.Connected() && client.Conn.Stats().Reconnects > 0
	})

	select {
	case err := <-bound:
		t.Fatalf("the bind returned before the drain (%v), so nothing was in flight", err)
	default:
	}

	if err := testwait.Receive(t, "the drain with a bind in flight to return with its clock standing still", drainOn(client, listenerDrain)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	err = testwait.Receive(t, "the bind in flight to end with the drain's close", bound)
	if err == nil {
		t.Fatal("the bind in flight at the drain bound the durable")
	}
	if errors.Is(err, natsgo.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the bind in flight ended by running out its own wait (%v), not by the drain's close", err)
	}
	if client.SubOK() {
		t.Fatal("SubOK reports the durable bound after the drain")
	}
}

// A reconnect that came before Drain leaves the connection to the drain, even when nats.go runs its
// callback only once the drain is under way, as a loaded process can: the drain still lets a
// delivery in its handler finish, and returns once the connection has drained. Closing the
// connection there would end Drain at once on the CLOSED event and cut the delivery off. The
// drain's deadline is on a clock that stands still, so its return is the connection's drain.
func TestDrainKeepsAConnectionThatReconnectedBeforeIt(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithClock(clocktest.New(fakeStart)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	conn := client.Conn
	// nats.go runs a connection's callbacks one at a time, in order, so a disconnect callback that
	// waits holds back the reconnect callback queued behind it.
	release := make(chan struct{})
	releaseCallbacks := sync.OnceFunc(func() { close(release) })
	defer releaseCallbacks()
	conn.SetDisconnectErrHandler(func(*natsgo.Conn, error) { <-release })
	onReconnect := conn.ReconnectHandler()
	reconnectRan := make(chan struct{})
	conn.SetReconnectHandler(func(nc *natsgo.Conn) {
		onReconnect(nc)
		close(reconnectRan)
	})

	inHandler := make(chan struct{})
	finish := make(chan struct{})
	finishHandler := sync.OnceFunc(func() { close(finish) })
	defer finishHandler()
	finished := &atomic.Bool{}
	if _, err := conn.Subscribe("drain.reconnected-before", func(*natsgo.Msg) {
		close(inHandler)
		<-finish
		finished.Store(true)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := conn.Publish("drain.reconnected-before", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	testwait.Receive(t, "the delivery to reach its handler", inHandler)

	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	testwait.Eventually(t, "the connection to reconnect in place", func() bool {
		return conn.Status() == natsgo.CONNECTED && conn.Stats().Reconnects == 1
	})

	drained := drainOn(client, listenerDrain)
	testwait.Eventually(t, "Drain to drain the connection", conn.IsDraining)

	// nats.go's close() sends the CLOSED event synchronously, so a callback that closed the
	// connection has sent it by the time the callback returns.
	closed := conn.StatusChanged(natsgo.CLOSED)
	defer conn.RemoveStatusListener(closed)
	releaseCallbacks()
	testwait.Receive(t, "the reconnect callback to run", reconnectRan)
	select {
	case <-closed:
		t.Fatal("the late reconnect callback closed the connection with the delivery still in its handler")
	default:
	}
	finishHandler()
	if err := testwait.Receive(t, "Drain to return once the delivery finished, its deadline not reached", drained); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Drain returned before the delivery in its handler finished")
	}
	if !conn.IsClosed() {
		t.Fatal("Drain returned with the connection still open")
	}
}

// A connection closed while nats.go drains it, as nats.go closes a connection whose server goes
// away during the drain, ends the drain at once. nats.go v1.50.0 marks such a connection draining
// again and waits 5 s for a reply on the socket it closed, then closes it a second time, so a drain
// that read the connection's state could miss the close and wait those 5 s out. bus.Connect makes
// no JetStream request (it ensures no stream, unlike ConnectOwningStream), so nats.go has no
// request subscription to drain and marks the connection draining again as soon as the close
// empties its subscriptions; whether a reader of the state misses it is a race, so the test runs it
// several times. The order is read off the connection's CLOSED events, which a listener the test
// empties at once counts: when Drain has returned, nats.go's second close, at the end of its 5 s,
// must not have come yet. The drain's own deadline is on a clock that stands still.
func TestDrainEndsWhenTheConnectionClosesWhileNATSDrainsIt(t *testing.T) {
	uri := testnats.URL(t)
	for attempt := 1; attempt <= 10; attempt++ {
		client, err := bus.Connect([]string{uri}, bus.WithClock(clocktest.New(fakeStart)))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		conn := client.Conn
		inHandler := make(chan struct{})
		release := make(chan struct{})
		releaseHandler := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseHandler)
		if _, err := conn.Subscribe("drain.closed-while-draining", func(*natsgo.Msg) {
			close(inHandler)
			<-release
		}); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := conn.Publish("drain.closed-while-draining", nil); err != nil {
			t.Fatalf("publish: %v", err)
		}
		testwait.Receive(t, "the delivery to reach its handler", inHandler)
		var closes atomic.Int32
		closedEvents := conn.StatusChanged(natsgo.CLOSED)
		go func() {
			for range closedEvents {
				closes.Add(1)
			}
		}()
		drained := drainOn(client, listenerDrain)
		// The delivery in its handler holds nats.go's drain of the connection's subscriptions.
		testwait.Eventually(t, "Drain to drain the connection", conn.IsDraining)
		conn.Close()
		releaseHandler()
		if err := testwait.Receive(t, "Drain to return after the connection closed while nats.go drained it", drained); err != nil {
			t.Fatalf("attempt %d: drain: %v", attempt, err)
		}
		if n := int(closes.Load()) + len(closedEvents); n > 1 {
			t.Fatalf("attempt %d: Drain returned only after nats.go closed the connection a second time, at the end of its own wait, want at once", attempt)
		}
		conn.RemoveStatusListener(closedEvents)
	}
}
