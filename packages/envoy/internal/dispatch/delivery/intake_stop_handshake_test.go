package delivery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// stopRow is one intake delivery subscribed through a handshake hold (testnats.HandshakeHold) on a
// NATS server of the row's own, whose monitoring endpoint counts the subscribers on its deliver
// subject.
type stopRow struct {
	server *testnats.Server
	// uri is the server's own client URL, which reaches it outside the hold.
	uri    string
	hold   *testnats.HandshakeHold
	client *bus.Client
	// conn is the client's connection, read before any hold: Client.Connection takes the client's
	// lock, which a caller of Client.Connected holds while it waits for nats.go's lock, as the
	// stop's drain does during a held handshake.
	conn    *natsgo.Conn
	durable *heldDurable
	sub     *natsgo.Subscription
	subject string
	// reconnected takes one value per reconnect of the client.
	reconnected chan struct{}
}

// startStopRow subscribes a delivery keyed by key and handled by handle at the durable's deliver
// subject, creating the durable first when createDurable says so, and waits until the server counts
// its subscriber.
func startStopRow(t *testing.T, key func(*natsgo.Msg) (string, *contracts.Envelope, bool), handle func(*intakeDelivery, intakeMessage), createDurable bool) *stopRow {
	t.Helper()
	server, uri := testnats.Start(t)
	hold := testnats.StartHandshakeHold(t, uri)
	client := reconnectTestClient(t, hold.URL())
	// Registered after the client's close, so they run before it: nats.go's Close waits for its
	// connection lock, which a held handshake or a held write holds.
	t.Cleanup(hold.Restore)
	t.Cleanup(hold.Release)
	reconnected := make(chan struct{}, 8)
	client.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		select {
		case reconnected <- struct{}{}:
		default:
		}
		return nil
	})
	wanted := deliveryConsumerConfig()
	if createDurable {
		if _, err := client.JS().AddConsumer(bus.Stream, &wanted); err != nil {
			t.Fatalf("create the delivery durable: %v", err)
		}
	}
	durable := newHeldDurable(client, nil, func() natsgo.ConsumerConfig { return wanted }, key, handle)
	sub, err := durable.subscribe(wanted.DeliverSubject)
	if err != nil {
		t.Fatalf("subscribe at %s: %v", wanted.DeliverSubject, err)
	}
	row := &stopRow{server: server, uri: uri, hold: hold, client: client, conn: client.Connection(), durable: &durable, sub: sub, subject: wanted.DeliverSubject, reconnected: reconnected}
	waitFor(t, 10*time.Second, func() bool { return row.subscribers(t) == 1 }, func() string {
		return fmt.Sprintf("control: the server reports %d subscriber(s) on the deliver subject before the hold, want 1", row.subscribers(t))
	})
	return row
}

// subscribers is how many subscribers the server counts on the row's deliver subject.
func (r *stopRow) subscribers(t *testing.T) int {
	return deliverSubjectSubscribers(t, r.server, r.subject)
}

// delivery is the subscription the row's durable delivers on.
func (r *stopRow) delivery() *intakeDelivery {
	r.durable.mu.Lock()
	defer r.durable.mu.Unlock()
	return r.durable.delivering
}

// publishPullRequestSeven publishes two envelopes of one pull request, which the intake keys alike,
// so the second waits behind the first.
func (r *stopRow) publishPullRequestSeven(t *testing.T) {
	t.Helper()
	for range 2 {
		publishPullRequestEnvelope(t, r.client, "acme", "widgets", 7, map[string]string{
			"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
		})
	}
}

// stopWithin is a StopDelivering of the row's durable under a context that ends after budget.
func (r *stopRow) stopWithin(budget time.Duration) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		r.durable.StopDelivering(ctx)
		return nil
	}
}

// watchHandBacks counts the negative acknowledgements the server is sent on its own client, a
// connection of the test's own to uri, outside the hold: what the server receives, whatever the
// intake logged.
func watchHandBacks(t *testing.T, uri string) func() int64 {
	t.Helper()
	var naks atomic.Int64
	observer := testnats.Connect(t, uri)
	if _, err := observer.Subscribe("$JS.ACK.>", func(msg *natsgo.Msg) {
		if strings.HasPrefix(string(msg.Data), "-NAK") {
			naks.Add(1)
		}
	}); err != nil {
		t.Fatalf("subscribe to the stream's acknowledgements: %v", err)
	}
	if err := observer.Flush(); err != nil {
		t.Fatalf("flush the acknowledgement watch: %v", err)
	}
	return naks.Load
}

// startGivenUpRow starts a row whose handler does what Intake.deliver does with a message whose
// handling the stop gave up -- it waits for the give-up and hands the message back (handBack) --
// and publishes two messages of one pull request, so the first is in its handler and the second
// waits behind its key, held by the stop.
func startGivenUpRow(t *testing.T) *stopRow {
	t.Helper()
	handling := make(chan struct{}, 2)
	row := startStopRow(t, intakeKey, func(delivery *intakeDelivery, message intakeMessage) {
		handling <- struct{}{}
		<-delivery.handling.Done()
		delivery.handBack(message.msg)
	}, true)
	row.publishPullRequestSeven(t)
	select {
	case <-handling:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the first message never reached its handler")
	}
	waitFor(t, 30*time.Second, func() bool { return queuedBehindKeys(row.delivery()) == 1 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind their key's handler, want the second message", queuedBehindKeys(row.delivery()))
	})
	return row
}

// requireNoHandBack releases the row's hold, waits for its client to reconnect and then a further
// two seconds, and fails the test when the server was sent a hand-back of either message.
func (r *stopRow) requireNoHandBack(t *testing.T, naks func() int64) {
	t.Helper()
	for len(r.reconnected) > 0 {
		<-r.reconnected
	}
	r.hold.Release()
	select {
	case <-r.reconnected:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the client did not reconnect within 30 s of the hold's release")
	}
	time.Sleep(2 * time.Second)
	if n := naks(); n != 0 {
		t.Fatalf("the server was sent %d hand-back(s) of the messages the stop withheld, want none", n)
	}
}

// An intake's stop does not wait for nats.go's connection lock, which a reconnect's handshake and a
// write to a link that does not move hold. The stop ran its drain, its hand-backs and the
// unsubscribe that ends a slow drain inside its once with no bound, and waited for its handlers,
// whose answers waited for that lock too, so it gave no answer until the lock was let go, and a
// second StopDelivering waited for it on the once. Each subtest has every StopDelivering it makes
// answer within slack past its own context, a few times bus.ConnLockWait, and, but for the one
// whose link comes back, checks that nats.go's lock was still held when they had (RequireLockHeld):
//
//   - a drain NATS has not taken: two calls at once during a held handshake, and once the hold is
//     released the subscription is drained all the same: the server counts no subscriber on its
//     deliver subject and the handle is no longer valid;
//   - a given-up handler during a held handshake: the handler hands its message back as
//     Intake.deliver does, and the stop holds a second message behind it; the server is sent no
//     hand-back of either, even once the hold is released;
//   - a stop between reconnect attempts: the same, begun while nats.go waits between two held
//     attempts with its lock free, so the drain answers at once on a link that is down;
//   - a drain on a link that is down: the stop's drain answers at once between two attempts, the
//     link then comes back, and only then is the handler given up: the server is sent no hand-back
//     of either message, since nothing says the server had the subscription's interest gone;
//   - an acknowledgement, a head-write hand-back and a job acknowledgement begun before the stop:
//     a handler makes its answer during a held handshake, and the stop begins while it waits;
//   - an unsubscribe while a write holds the lock: the stop drained on a moving link and its drain
//     did not end, and the unsubscribe that ends it, once the stop's context ends, meets the lock.
//
// The negative control is the stop as it was (DrainSubscription, the hand-backs and Unsubscribe
// inside the once with no bound): no StopDelivering answers in time. So is a hand-back that skips
// the stop's rule (handBack), the given-up handler's in the second and third subtests; a stop that
// withholds only when its drain did not answer, in the fourth; and an answer begun before the stop
// with no bound, in the fifth to seventh.
func TestAnIntakesStopDoesNotWaitOutAHeldHandshake(t *testing.T) {
	// A stop whose calls to NATS each answer at bus.ConnLockWait answers within this past its own
	// context, on a loaded runner.
	const slack = 600 * time.Millisecond

	t.Run("a drain NATS has not taken", func(t *testing.T) {
		row := startStopRow(t, intakeKey, func(*intakeDelivery, intakeMessage) {}, false)
		row.hold.HoldReconnect(t)
		for len(row.reconnected) > 0 {
			<-row.reconnected
		}
		stop := row.stopWithin(testnats.HeldHandshakeAnswerWait)
		testnats.AnswersWithin(t, slack, "StopDelivering", stop, stop)
		testnats.RequireLockHeld(t, row.conn)

		row.hold.Release()
		select {
		case <-row.reconnected:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the client did not reconnect within 30 s of the hold's release")
		}
		waitFor(t, 30*time.Second, func() bool { return !row.sub.IsValid() && row.subscribers(t) == 0 }, func() string {
			return fmt.Sprintf("after the reconnect the stopped subscription is still valid = %t and the server reports %d subscriber(s) on its deliver subject, want neither: the stop left it subscribed",
				row.sub.IsValid(), row.subscribers(t))
		})
	})

	t.Run("a given-up handler during a held handshake", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		row.hold.HoldReconnect(t)
		const budget = 300 * time.Millisecond
		stop := row.stopWithin(budget)
		testnats.AnswersWithin(t, budget+slack, "StopDelivering", stop, stop)
		testnats.RequireLockHeld(t, row.conn)
		row.requireNoHandBack(t, naks)
	})

	t.Run("a stop between reconnect attempts", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		row.hold.HoldReconnect(t)
		// The held attempt ends at the client's connect timeout; nats.go then waits a second
		// before the next, which the hold holds too.
		testnats.AwaitLockFree(t, row.conn)
		// Past the wait between attempts, so the handler is given up, and hands its message back,
		// while the next attempt's handshake is held.
		const budget = 1300 * time.Millisecond
		stop := row.stopWithin(budget)
		testnats.AnswersWithin(t, budget+slack, "StopDelivering", stop, stop)
		testnats.RequireLockHeld(t, row.conn)
		row.requireNoHandBack(t, naks)
	})

	t.Run("a drain on a link that is down", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		row.hold.HoldReconnect(t)
		for len(row.reconnected) > 0 {
			<-row.reconnected
		}
		// Between two held attempts the link is down and nats.go's lock is free, so the drain
		// answers at once: DrainSubscription reads the link down and unsubscribes on the client alone.
		testnats.AwaitLockFree(t, row.conn)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stopped := make(chan struct{})
		go func() {
			row.durable.StopDelivering(ctx)
			close(stopped)
		}()
		waitFor(t, 5*time.Second, func() bool { return !row.sub.IsValid() }, func() string {
			return "control: the stop's drain did not end the subscription on the down link between attempts"
		})
		row.hold.Release()
		select {
		case <-row.reconnected:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the client did not reconnect within 30 s of the hold's release")
		}
		// The link is back: the handler is given up only now, and the stop hands back what it held.
		cancel()
		testnats.AnswersWithin(t, slack, "StopDelivering", func() error {
			<-stopped
			return nil
		})
		time.Sleep(2 * time.Second)
		if n := naks(); n != 0 {
			t.Fatalf("the server was sent %d hand-back(s) of the messages a stop whose drain found the link down held, want none", n)
		}
	})

	for _, answer := range []struct {
		name string
		make func(*intakeDelivery, intakeMessage)
	}{
		{"an acknowledgement begun before the stop", func(d *intakeDelivery, m intakeMessage) { d.acknowledge(m.msg) }},
		{"a head-write hand-back begun before the stop", func(d *intakeDelivery, m intakeMessage) { d.handBackOwn(m.msg, headWriteRedeliverAfter) }},
		{"a job acknowledgement begun before the stop", func(d *intakeDelivery, m intakeMessage) {
			(&JobIntake{ctx: context.Background()}).deliver(d, m)
		}},
	} {
		t.Run(answer.name, func(t *testing.T) {
			entered := make(chan struct{}, 2)
			proceed := make(chan struct{})
			var proceeding sync.Once
			goOn := func() { proceeding.Do(func() { close(proceed) }) }
			t.Cleanup(goOn)
			// The handler makes its answer to NATS once the row lets it, as Intake.deliver and
			// JobIntake.deliver do when their handling is done.
			row := startStopRow(t, intakeKey, func(d *intakeDelivery, m intakeMessage) {
				entered <- struct{}{}
				<-proceed
				answer.make(d, m)
			}, true)
			row.publishPullRequestSeven(t)
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				t.Fatal("control: the first message never reached its handler")
			}
			row.hold.HoldReconnect(t)
			goOn()
			// The answer waits on nats.go's lock from here; the stop begins while it does.
			time.Sleep(300 * time.Millisecond)
			if row.delivery().stopping.Load() {
				t.Fatal("control: the stop began before the handler's answer")
			}
			const budget = 300 * time.Millisecond
			stop := row.stopWithin(budget)
			testnats.AnswersWithin(t, budget+slack, "StopDelivering", stop, stop)
			testnats.RequireLockHeld(t, row.conn)
		})
	}

	t.Run("an unsubscribe while a write holds the lock", func(t *testing.T) {
		keyed := make(chan struct{}, 2)
		release := make(chan struct{})
		var released sync.Once
		t.Cleanup(func() { released.Do(func() { close(release) }) })
		// Keying the first message holds the subscription's callback, so the second stays in the
		// subscription's buffer and its drain cannot end.
		row := startStopRow(t, func(msg *natsgo.Msg) (string, *contracts.Envelope, bool) {
			select {
			case keyed <- struct{}{}:
			default:
			}
			<-release
			return intakeKey(msg)
		}, func(*intakeDelivery, intakeMessage) {}, true)
		row.publishPullRequestSeven(t)
		select {
		case <-keyed:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the first message never reached the subscription's callback")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stopped := make(chan struct{})
		go func() {
			row.durable.StopDelivering(ctx)
			close(stopped)
		}()
		waitFor(t, 30*time.Second, func() bool { return stopReleased(row.delivery()) }, func() string {
			return "control: the stop never reached its wait for the drain to end"
		})
		select {
		case <-stopped:
			t.Fatal("control: the stop returned while its drain had a message buffered and its context had not ended, so no unsubscribe was due")
		default:
		}
		row.hold.HoldWriteLock(t, row.conn)
		cancel()
		testnats.AnswersWithin(t, slack, "StopDelivering", func() error {
			<-stopped
			return nil
		})
		testnats.RequireLockHeld(t, row.conn)
	})
}

// queuedBehindKeys is how many messages of delivery wait behind their key's handler.
func queuedBehindKeys(delivery *intakeDelivery) int {
	delivery.keys.mu.Lock()
	defer delivery.keys.mu.Unlock()
	queued := 0
	for _, items := range delivery.keys.queued {
		queued += len(items)
	}
	return queued
}

// stopReleased reports whether delivery's stop has dealt with the messages it held, so it is past
// its hand-backs, waiting for its drain to end.
func stopReleased(delivery *intakeDelivery) bool {
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	return delivery.released
}
