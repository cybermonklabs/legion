package delivery

import (
	"context"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// An intake's stop does not wait out a reconnect's handshake. While the hub holds its answer to
// the client's CONNECT, nats.go holds its connection lock, which the stop's drain waits for: the
// stop gave no answer until the handshake attempt ended, and a second StopDelivering waited for it
// on the stop's once. Two StopDelivering calls made at once while the handshake is held both
// return within testnats.HeldHandshakeAnswerWait, and once the hold is released and the client has
// reconnected, the subscription is drained all the same: the server reports no subscriber on its
// deliver subject and the handle is no longer valid.
//
// The controls: the server reports the subscription's subscriber before the hold, and the client's
// reconnect hook runs before the row reads the server's count again, so that count is read of a
// connection that resent its subscriptions. The negative control is the stop as it was
// (DrainSubscription inside the once with no check of the connection): neither call returns within
// the wait.
func TestAnIntakesStopDoesNotWaitOutAHeldHandshake(t *testing.T) {
	server, uri := testnats.Start(t)
	hold := testnats.StartHandshakeHold(t, uri)
	natsClient := reconnectTestClient(t, hold.URL())
	t.Cleanup(hold.Release)
	reconnected := make(chan struct{}, 8)
	natsClient.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		select {
		case reconnected <- struct{}{}:
		default:
		}
		return nil
	})
	durable := newHeldDurable(natsClient, nil, deliveryConsumerConfig, intakeKey, func(*intakeDelivery, intakeMessage) {})
	subject := deliveryConsumerConfig().DeliverSubject
	sub, err := durable.subscribe(subject)
	if err != nil {
		t.Fatalf("subscribe at %s: %v", subject, err)
	}
	waitFor(t, 10*time.Second, func() bool { return deliverSubjectSubscribers(t, server, subject) == 1 }, func() string {
		return fmt.Sprintf("control: the server reports %d subscriber(s) on the deliver subject before the hold, want 1", deliverSubjectSubscribers(t, server, subject))
	})

	hold.HoldReconnect(t)
	for len(reconnected) > 0 {
		<-reconnected
	}
	stop := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), testnats.HeldHandshakeAnswerWait)
		defer cancel()
		durable.StopDelivering(ctx)
		return nil
	}
	testnats.AnswersWhileHeld(t, "StopDelivering", stop, stop)

	hold.Release()
	select {
	case <-reconnected:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the client did not reconnect within 30 s of the hold's release")
	}
	waitFor(t, 30*time.Second, func() bool { return !sub.IsValid() && deliverSubjectSubscribers(t, server, subject) == 0 }, func() string {
		return fmt.Sprintf("after the reconnect the stopped subscription is still valid = %t and the server reports %d subscriber(s) on its deliver subject, want neither: the stop left it subscribed",
			sub.IsValid(), deliverSubjectSubscribers(t, server, subject))
	})
}
