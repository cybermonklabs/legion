package delivery

import (
	"context"
	"fmt"
	"sync"
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
	row := &stopRow{server: server, hold: hold, client: client, conn: client.Connection(), durable: &durable, sub: sub, subject: wanted.DeliverSubject, reconnected: reconnected}
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

// An intake's stop does not wait for nats.go's connection lock, which a reconnect's handshake and a
// write to a link that does not move hold. The stop ran its drain, its hand-backs and the
// unsubscribe that ends a slow drain inside its once with no bound, so it gave no answer until the
// lock was let go, and a second StopDelivering waited for it on the once. Each subtest has every
// StopDelivering it makes answer within testnats.HeldHandshakeAnswerWait, and checks that nats.go's
// lock was still held when they had (RequireLockHeld):
//
//   - a drain NATS has not taken: two calls at once during a held handshake, and once the hold is
//     released the subscription is drained all the same: the server counts no subscriber on its
//     deliver subject and the handle is no longer valid;
//   - the messages the stop held: a message waiting behind its key's handler is left with the
//     durable rather than handed back, since the drain was not taken;
//   - an unsubscribe while a write holds the lock: the stop drained on a moving link and its drain
//     did not end, and the unsubscribe that ends it, once the stop's context ends, meets the lock.
//
// The negative control is the stop as it was (DrainSubscription, the hand-backs and Unsubscribe
// inside the once with no bound): no StopDelivering answers within the wait. Handing the held
// messages back when the drain was not taken fails the second subtest the same way.
func TestAnIntakesStopDoesNotWaitOutAHeldHandshake(t *testing.T) {
	t.Run("a drain NATS has not taken", func(t *testing.T) {
		row := startStopRow(t, intakeKey, func(*intakeDelivery, intakeMessage) {}, false)
		row.hold.HoldReconnect(t)
		for len(row.reconnected) > 0 {
			<-row.reconnected
		}
		stop := row.stopWithin(testnats.HeldHandshakeAnswerWait)
		testnats.AnswersWhileHeld(t, "StopDelivering", stop, stop)
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

	t.Run("the messages the stop held", func(t *testing.T) {
		logs := captureLogs(t)
		handling := make(chan struct{}, 2)
		// The handler holds its message until the stop gives it up, and answers nothing: the
		// acknowledgement a real handler makes would wait for nats.go's lock as well.
		row := startStopRow(t, intakeKey, func(delivery *intakeDelivery, _ intakeMessage) {
			handling <- struct{}{}
			<-delivery.handling.Done()
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
		row.hold.HoldReconnect(t)
		// The handlers are given up as these contexts end, well inside the answer wait.
		stop := row.stopWithin(500 * time.Millisecond)
		testnats.AnswersWhileHeld(t, "StopDelivering", stop, stop)
		testnats.RequireLockHeld(t, row.conn)
		if !loggedMessage(logs(), stopLeftHeld) {
			t.Fatalf("the stop did not leave the message it held with the durable (%q); the messages logged were %q", stopLeftHeld, loggedMessages(logs()))
		}
	})

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
		testnats.AnswersWhileHeld(t, "StopDelivering", func() error {
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
