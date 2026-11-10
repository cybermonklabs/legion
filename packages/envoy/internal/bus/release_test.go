package bus_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// releaseDurable adds the push durable name for a test to bind, as a delivery intake binds its own,
// and returns name.
func releaseDurable(t *testing.T, client *bus.Client, name string) string {
	t.Helper()
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        name,
		DeliverSubject: natsgo.NewInbox(),
		AckPolicy:      natsgo.AckExplicitPolicy,
		FilterSubjects: bus.StreamSubjects(),
	}); err != nil {
		t.Fatalf("add the durable %s: %v", name, err)
	}
	return name
}

// bindCounting binds client's JetStream subscription to consumer with a handler that counts and
// acknowledges what it is handed.
func bindCounting(t *testing.T, client *bus.Client, consumer string, count *atomic.Int32) *natsgo.Subscription {
	t.Helper()
	sub, err := client.Subscribe("", func(msg *natsgo.Msg) {
		count.Add(1)
		_ = msg.Ack()
	}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	return sub
}

// drainSubscription drains sub and waits for nats.go to end the drain.
func drainSubscription(t *testing.T, sub *natsgo.Subscription) {
	t.Helper()
	if err := sub.Drain(); err != nil {
		t.Fatalf("drain the subscription: %v", err)
	}
	waitFor(t, 10*time.Second, "the drain to end", func() bool { return !sub.IsValid() })
}

// awaitHook waits for the reconnect hook to report a run, which comes after the client's restore of
// its subscriptions on that connection event.
func awaitHook(t *testing.T, hooked <-chan struct{}) {
	t.Helper()
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect hooks never ran")
	}
}

// awaitPushBound waits for the durable consumer to report want for its push binding. nats-server
// applies a push subscriber's interest, and its release, on the consumer's own goroutine, after
// the client's bind or unsubscribe is answered, so only the consumer's own report tells a test
// that the server has applied one.
func awaitPushBound(t *testing.T, client *bus.Client, consumer string, want bool, what string) {
	t.Helper()
	waitFor(t, 10*time.Second, what, func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.PushBound == want
	})
}

func publishReleaseProbe(t *testing.T, client *bus.Client, subject string) {
	t.Helper()
	if _, err := client.JS().Publish(subject, []byte(`{}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// A subscription its caller has ended with DrainSubscription stays unbound across a reconnect, in
// place or onto a connection the client dials to replace a closed one: DrainSubscription gives the
// registration up, without which the client would bind the drained registration again and its
// stopped handler take the durable's deliveries. A later Subscribe on the same client binds the
// durable afresh and is handed what was published meanwhile.
func TestADrainedSubscriptionIsNotBoundAgainByAReconnect(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reconnect func(*bus.Client) error
	}{
		{name: "in place", reconnect: func(client *bus.Client) error { return client.Conn.ForceReconnect() }},
		{name: "replaced connection", reconnect: func(client *bus.Client) error {
			client.Conn.Close()
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, uri := startNATS(t)
			client, err := bus.ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer client.Close()
			consumer := releaseDurable(t, client, "released")
			hooked := make(chan struct{}, 8)
			client.AddReconnectHook(func(*natsgo.Conn) error {
				hooked <- struct{}{}
				return nil
			})

			var stopped atomic.Int32
			sub := bindCounting(t, client, consumer, &stopped)
			drained, err := client.DrainSubscription(sub)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if drained != sub {
				t.Fatalf("DrainSubscription drained %p, want the handle Subscribe returned (%p)", drained, sub)
			}
			waitFor(t, 10*time.Second, "the drain to end", func() bool { return !drained.IsValid() })

			if err := tc.reconnect(client); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			awaitHook(t, hooked)
			publishReleaseProbe(t, client, "notifications.github.test.released")
			// nats-server applies a new subscriber's interest on the consumer's own goroutine, so a
			// rebind would show as push-bound, and be handed the message, within moments.
			time.Sleep(time.Second)
			if client.SubOK() {
				t.Fatal("SubOK reports a subscription bound after it was drained")
			}
			info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
			if err != nil {
				t.Fatalf("read the durable: %v", err)
			}
			if info.PushBound {
				t.Fatal("the reconnect bound the drained subscription to the durable again")
			}
			if got := stopped.Load(); got != 0 {
				t.Fatalf("the drained subscription's handler was handed %d message(s) after the reconnect", got)
			}

			var fresh atomic.Int32
			bindCounting(t, client, consumer, &fresh)
			waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
			if !client.SubOK() {
				t.Fatal("SubOK reports the fresh subscription unbound")
			}
			if got := stopped.Load(); got != 0 {
				t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
			}
		})
	}
}

// A subscription that ended otherwise than by DrainSubscription -- its handle drained directly --
// is bound again by the next reconnect; DrainSubscription then drains that rebound handle and gives
// the registration up, so the durable is left free and the stopped handler is handed nothing more.
func TestDrainSubscriptionEndsWhatARestoreBoundAfterTheSubscriptionEnded(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	consumer := releaseDurable(t, client, "released")
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	var stopped atomic.Int32
	sub := bindCounting(t, client, consumer, &stopped)
	drainSubscription(t, sub)
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	awaitHook(t, hooked)
	if !client.SubOK() {
		t.Fatal("the reconnect did not bind the drained registration again; the test proves nothing")
	}
	// Wait for the server to apply the restore's bind before draining it: the drain and the checks
	// below would otherwise pass on a binding the server applies afterwards.
	awaitPushBound(t, client, consumer, true, "the durable to take the restore's binding")

	drained, err := client.DrainSubscription(sub)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained == nil || drained == sub {
		t.Fatalf("DrainSubscription drained %p, want the handle the restore bound, not the one Subscribe returned (%p)", drained, sub)
	}
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was drained")
	}
	awaitPushBound(t, client, consumer, false, "the durable to lose its subscriber")
	publishReleaseProbe(t, client, "notifications.github.test.released")
	time.Sleep(time.Second)
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
	}
}

// A caller that gives its subscription up while the client's connection is down leaves the durable
// free, so another process can take it over: DrainSubscription unsubscribes the handle rather than
// draining it, because a drain begun while the link is down writes no UNSUB and the reconnect
// sends the draining subscription anew, leaving the server reporting the durable push-bound to a
// client that delivers nothing. The server is read from a second client connected to it directly:
// the giving-up client's own reads run over the link under test, and can say nothing while it is
// down.
func TestDrainSubscriptionWhileTheConnectionIsDownLeavesNoPushBindingBehind(t *testing.T) {
	_, uri := startNATS(t)
	relay := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{relay.url()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	defer client.Close()
	direct, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the second client: %v", err)
	}
	defer direct.Close()
	consumer := releaseDurable(t, direct, "released")
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	var stopped atomic.Int32
	sub := bindCounting(t, client, consumer, &stopped)
	awaitPushBound(t, direct, consumer, true, "the durable to take the subscription's binding")

	relay.refuse.Store(true)
	relay.drop()
	waitFor(t, 10*time.Second, "the client to start reconnecting", client.Conn.IsReconnecting)
	publishReleaseProbe(t, direct, "notifications.github.test.released")

	drained, err := client.DrainSubscription(sub)
	if err != nil {
		t.Fatalf("give the subscription up while the connection is down: %v", err)
	}
	// Reported rather than fatal, so a run where the give-up drained instead reaches the server's
	// own answer below and says both what the call returned and what the durable was left as.
	if drained != nil {
		t.Errorf("DrainSubscription returned %p to wait on; an unsubscribe leaves no handle to wait on", drained)
	}

	relay.refuse.Store(false)
	// The reconnect is where a drain would show: nats.go sends every subscription the connection
	// still holds anew, so the checks below read the durable only once that resend has landed.
	awaitHook(t, hooked)
	awaitPushBound(t, direct, consumer, false, "the durable to be left free by the give-up")
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was given up")
	}
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the given-up subscription's handler was handed %d message(s) after the reconnect", got)
	}

	var fresh atomic.Int32
	bindCounting(t, direct, consumer, &fresh)
	waitFor(t, 10*time.Second, "the second client to be handed what was published meanwhile", func() bool { return fresh.Load() == 1 })
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the given-up subscription's handler was handed %d message(s)", got)
	}
}

// A link that drops while the drain is still in flight leaves no push binding behind either: the
// drained handle is unsubscribed at the client's disconnect, so the reconnect's resend does not
// find it in the connection's subscription list, and the server reports the durable free for
// another process to take over. This is the second window in which a drain meets a reconnect:
// nats.go keeps a draining subscription in that list until its handler has taken what was
// buffered (checkDrained), and a SUB re-sent for it binds the durable to a client whose handler
// has stopped.
//
// Two levers hold the drain open across the drop, and are what make the row deterministic rather
// than a race with checkDrained. The relay is armed before the give-up, so the drain's UNSUB and
// the flush checkDrained opens with are swallowed: the server never learns of the drain, and the
// binding it reports afterwards is the one the resend leaves. And the handler is held inside its
// delivery, which keeps the subscription's pending count above zero, so checkDrained goes on
// through the reconnect instead of removing the subscription in its first pass after the drop --
// the drop releases its flush at once (clearPendingFlushCalls), and an idle handler would let it
// remove the subscription before the resend and hide the window. The handler is let go only once
// the server has been read.
//
// The server is read from a second client connected to it directly, as the row above does: the
// giving-up client's own reads run over the link under test.
func TestDrainSubscriptionWhileTheDrainIsInFlightLeavesNoPushBindingBehind(t *testing.T) {
	_, uri := startNATS(t)
	relay := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{relay.url()})
	if err != nil {
		t.Fatalf("connect through the relay: %v", err)
	}
	defer client.Close()
	direct, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the second client: %v", err)
	}
	defer direct.Close()
	consumer := releaseDurable(t, direct, "released")
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	handling := make(chan struct{}, 1)
	held := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(held) })
	defer letGo()
	var stopped atomic.Int32
	sub, err := client.Subscribe("", func(msg *natsgo.Msg) {
		stopped.Add(1)
		_ = msg.Ack()
		handling <- struct{}{}
		<-held
	}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	awaitPushBound(t, direct, consumer, true, "the durable to take the subscription's binding")

	// The handler stays inside this delivery until the checks below have run: nats.go counts a
	// message pending until its handler returns, and a drain ends at the pass that finds nothing
	// pending.
	publishReleaseProbe(t, direct, "notifications.github.test.held")
	select {
	case <-handling:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was never handed the message that holds the drain open")
	}

	relay.arm("UNSUB")
	drained, err := client.DrainSubscription(sub)
	if err != nil {
		t.Fatalf("give the subscription up while the connection is live: %v", err)
	}
	if drained != sub {
		t.Fatalf("DrainSubscription returned %p to wait on, want the drained handle (%p): the give-up took no drain, so no drain was in flight at the drop", drained, sub)
	}
	select {
	case <-relay.swallowed:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain's UNSUB never reached the link, so the drop did not land on a drain in flight")
	}

	relay.refuse.Store(true)
	relay.drop()
	waitFor(t, 10*time.Second, "the client to start reconnecting", client.Conn.IsReconnecting)
	publishReleaseProbe(t, direct, "notifications.github.test.released")

	relay.refuse.Store(false)
	// The reconnect is where the drain shows: nats.go sends every subscription the connection
	// still holds anew, so the checks below read the durable only once that resend has landed.
	awaitHook(t, hooked)
	awaitPushBound(t, direct, consumer, false, "the durable to be left free by the give-up")
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was given up")
	}

	letGo()
	waitFor(t, 10*time.Second, "the drain to end", func() bool { return !drained.IsValid() })
	if got := stopped.Load(); got != 1 {
		t.Fatalf("the given-up subscription's handler was handed %d message(s), want only the one it was held inside", got)
	}

	var fresh atomic.Int32
	bindCounting(t, direct, consumer, &fresh)
	waitFor(t, 10*time.Second, "the second client to be handed what was published meanwhile", func() bool { return fresh.Load() == 1 })
	if got := stopped.Load(); got != 1 {
		t.Fatalf("the given-up subscription's handler was handed %d message(s) after the drain ended", got)
	}
}

// A caller that gives its subscription up while the client's connection is live still drains it:
// the handle comes back for the caller to wait on, the handler is handed what the client had
// buffered, and the client holds no subscription once the drain ends.
func TestDrainSubscriptionWhileTheConnectionIsLiveStillDrains(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	consumer := releaseDurable(t, client, "released")

	var handled atomic.Int32
	sub := bindCounting(t, client, consumer, &handled)
	awaitPushBound(t, client, consumer, true, "the durable to take the subscription's binding")
	publishReleaseProbe(t, client, "notifications.github.test.released")
	// The drain hands the handler what the client has buffered and nothing new, so the message must
	// have left the server before the drain's UNSUB reaches it. The consumer's own count of what it
	// delivered is the last thing a test can read before the message is the client's.
	waitFor(t, 10*time.Second, "the durable to deliver the message", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.Delivered.Consumer >= 1
	})

	drained, err := client.DrainSubscription(sub)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained != sub {
		t.Fatalf("DrainSubscription returned %p to wait on, want the handle Subscribe returned (%p)", drained, sub)
	}
	waitFor(t, 10*time.Second, "the drain to end", func() bool { return !drained.IsValid() })
	if got := handled.Load(); got != 1 {
		t.Fatalf("the drained subscription's handler was handed %d message(s), want the one published before the drain", got)
	}
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was drained")
	}
}

// DrainSubscription of a handle a later Subscribe replaced drains and gives up nothing: the
// replacement stays the client's subscription, and a recovery onto a replacement connection binds
// it again.
func TestDrainSubscriptionOfAReplacedSubscriptionKeepsTheReplacement(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	// Two durables, so the replacement's bind does not race the server's release of the binding the
	// replaced handle held.
	replacedDurable := releaseDurable(t, client, "replaced")
	replacementDurable := releaseDurable(t, client, "replacement")

	var first, second atomic.Int32
	replaced := bindCounting(t, client, replacedDurable, &first)
	bindCounting(t, client, replacementDurable, &second)
	if drained, err := client.DrainSubscription(replaced); drained != nil || err != nil {
		t.Fatalf("DrainSubscription of a replaced handle = %p, %v; want nothing drained", drained, err)
	}
	if !client.SubOK() {
		t.Fatal("draining a replaced handle gave up its replacement")
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "the recovery to bind the replacement again", client.SubOK)
	publishReleaseProbe(t, client, "notifications.github.test.replaced")
	waitFor(t, 10*time.Second, "the replacement to be handed the message", func() bool { return second.Load() == 1 })
	if got := first.Load(); got != 0 {
		t.Fatalf("the replaced subscription's handler was handed %d message(s)", got)
	}
}

// subscribeAbsent subscribes client's JetStream subscription to the durable name before it exists,
// with a handler that counts what it is handed, and requires the bind to fail: the client keeps
// the registration, which a reconnect binds once the durable exists.
func subscribeAbsent(t *testing.T, client *bus.Client, name string, count *atomic.Int32) {
	t.Helper()
	sub, err := client.Subscribe("", func(msg *natsgo.Msg) {
		count.Add(1)
		_ = msg.Ack()
	}, natsgo.Bind(bus.Stream, name), natsgo.ManualAck())
	if err == nil {
		t.Fatalf("bound the durable %s before it existed (%v); the test proves nothing", name, sub)
	}
}

// A subscription whose bind failed, and whose caller gave up its registration, stays unbound
// across a reconnect, in place or onto a connection the client dials to replace a closed one, even
// once the durable it named exists: without the release the client binds the failed registration,
// and the handler its caller gave up takes the durable's deliveries. A later Subscribe on the same
// client binds the durable and is handed what was published meanwhile.
func TestAFailedSubscriptionReleasedIsNotBoundByAReconnect(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reconnect func(*bus.Client) error
	}{
		{name: "in place", reconnect: func(client *bus.Client) error { return client.Conn.ForceReconnect() }},
		{name: "replaced connection", reconnect: func(client *bus.Client) error {
			client.Conn.Close()
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, uri := startNATS(t)
			client, err := bus.ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer client.Close()
			hooked := make(chan struct{}, 8)
			client.AddReconnectHook(func(*natsgo.Conn) error {
				hooked <- struct{}{}
				return nil
			})

			var failed atomic.Int32
			subscribeAbsent(t, client, "failed", &failed)
			client.ReleaseFailed("")
			consumer := releaseDurable(t, client, "failed")

			if err := tc.reconnect(client); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			awaitHook(t, hooked)
			publishReleaseProbe(t, client, "notifications.github.test.failed")
			// nats-server applies a new subscriber's interest on the consumer's own goroutine, so a
			// bind would show as push-bound, and be handed the message, within moments.
			time.Sleep(time.Second)
			if client.SubOK() {
				t.Fatal("SubOK reports a subscription bound after its failed registration was released")
			}
			info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
			if err != nil {
				t.Fatalf("read the durable: %v", err)
			}
			if info.PushBound {
				t.Fatal("the reconnect bound the released failed registration to the durable")
			}
			if got := failed.Load(); got != 0 {
				t.Fatalf("the failed subscription's handler was handed %d message(s) after the reconnect", got)
			}

			var fresh atomic.Int32
			bindCounting(t, client, consumer, &fresh)
			waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
			if !client.SubOK() {
				t.Fatal("SubOK reports the fresh subscription unbound")
			}
			if got := failed.Load(); got != 0 {
				t.Fatalf("the failed subscription's handler was handed %d message(s)", got)
			}
		})
	}
}

// A reconnect that lands after a subscription's bind failed and before its caller gave up the
// registration binds it; ReleaseFailed then gives up the registration and unsubscribes that bound
// handle, so the durable is left free and the handler is handed nothing.
func TestReleaseFailedUnsubscribesWhatARestoreBound(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	var failed atomic.Int32
	subscribeAbsent(t, client, "failed", &failed)
	consumer := releaseDurable(t, client, "failed")
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	awaitHook(t, hooked)
	if !client.SubOK() {
		t.Fatal("the reconnect did not bind the failed registration; the test proves nothing")
	}
	// Wait for the server to apply the restore's bind before releasing it: only once the durable
	// has reported the binding, and then its loss, is the server known to have applied both, so the
	// probe published next stays pending for the fresh bind rather than going to the released
	// handle's deliver inbox and waiting out the consumer's AckWait there.
	awaitPushBound(t, client, consumer, true, "the durable to take the restore's binding")

	client.ReleaseFailed("")
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after its failed registration was released")
	}
	awaitPushBound(t, client, consumer, false, "the durable to lose its subscriber")
	publishReleaseProbe(t, client, "notifications.github.test.failed")
	time.Sleep(time.Second)
	if got := failed.Load(); got != 0 {
		t.Fatalf("the failed subscription's handler was handed %d message(s)", got)
	}

	var fresh atomic.Int32
	bindCounting(t, client, consumer, &fresh)
	waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
}

// ReleaseFailed gives up only a registration whose Subscribe failed: not the one a later Subscribe
// on the subject made and bound, nor the one a Subscribe on another subject was refused beside
// (ErrSubscriptionHeld). The kept subscription is bound again by a recovery onto a replacement
// connection.
func TestReleaseFailedKeepsASubscriptionThatBound(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var failed, bound atomic.Int32
	subscribeAbsent(t, client, "later", &failed)
	consumer := releaseDurable(t, client, "later")
	bindCounting(t, client, consumer, &bound)
	client.ReleaseFailed("")
	if !client.SubOK() {
		t.Fatal("ReleaseFailed gave up a later Subscribe's bound subscription")
	}
	const other = "notifications.github.test.other"
	if _, err := client.Subscribe(other, func(*natsgo.Msg) {}); !errors.Is(err, bus.ErrSubscriptionHeld) {
		t.Fatalf("subscribe on another subject: %v, want %v", err, bus.ErrSubscriptionHeld)
	}
	client.ReleaseFailed(other)
	if !client.SubOK() {
		t.Fatal("ReleaseFailed of a refused subject gave up the held subscription")
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "the recovery to bind the kept subscription again", client.SubOK)
	publishReleaseProbe(t, client, "notifications.github.test.later")
	waitFor(t, 10*time.Second, "the kept subscription to be handed the message", func() bool { return bound.Load() == 1 })
	if got := failed.Load(); got != 0 {
		t.Fatalf("the failed subscription's handler was handed %d message(s)", got)
	}
}

// DrainSubscription after the client replaced its connection drains the handle the client bound
// on the new connection, the one delivering, rather than the handle Subscribe returned, which went
// with the closed connection: the drain answers no error and the durable loses its subscriber.
// No recovery binds the drained registration again, and a later Subscribe binds the durable afresh
// and is handed what was published meanwhile.
func TestDrainSubscriptionDrainsTheHandleARestoreBound(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	consumer := releaseDurable(t, client, "drained")
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	var stopped atomic.Int32
	sub := bindCounting(t, client, consumer, &stopped)

	client.Conn.Close()
	awaitHook(t, hooked)
	waitFor(t, 30*time.Second, "the recovery to bind the registration again", client.SubOK)
	if sub.IsValid() {
		t.Fatal("the handle Subscribe returned is still valid after its connection closed; the test proves nothing")
	}
	// Wait for the server to apply the recovery's bind before draining it, so the drain's release
	// of the binding below is one the server has something to apply it to.
	awaitPushBound(t, client, consumer, true, "the durable to take the recovery's binding")

	drained, err := client.DrainSubscription(sub)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained == nil || drained == sub {
		t.Fatalf("DrainSubscription drained %p, want the handle the recovery bound, not the one Subscribe returned (%p)", drained, sub)
	}
	waitFor(t, 10*time.Second, "the drain to end", func() bool { return !drained.IsValid() })
	awaitPushBound(t, client, consumer, false, "the durable to lose its subscriber")

	client.Conn.Close()
	awaitHook(t, hooked)
	publishReleaseProbe(t, client, "notifications.github.test.drained")
	// nats-server applies a new subscriber's interest on the consumer's own goroutine, so a rebind
	// would show as push-bound, and be handed the message, within moments.
	time.Sleep(time.Second)
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was drained")
	}
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if info.PushBound {
		t.Fatal("the recovery bound the drained registration to the durable again")
	}
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
	}

	var fresh atomic.Int32
	bindCounting(t, client, consumer, &fresh)
	waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
	}
}
