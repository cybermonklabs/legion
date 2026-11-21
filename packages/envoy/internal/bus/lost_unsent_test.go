package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A stored publish that Request did not send, because the life of its connection had ended before
// the send (requestUnsent), answers ErrConnectionLost without ErrUnacknowledged: the stream cannot
// hold it, so the listener's sender limit charges nothing for the 503 its route then answers, and
// the caller's retry, which stores the message, is the one charge. The hold cuts the connection and
// refuses every reconnect (testnats.ReplyHold.Cut), so the client is down when the publish is made,
// and the publish goes straight to sendStored, past the connect publishJetStreamOn makes first: the
// moment of a loss that lands between that connect and the send.
//
// The control is this row with requestUnsent left out of sendUnanswered: the error wraps
// ErrUnacknowledged, and a route would charge the 503 and then the retry that stores the message.
func TestAStoredPublishItsLostConnectionKeptFromBeingSentIsNoErrUnacknowledged(t *testing.T) {
	client, hold := heldClient(t)
	hold.Cut()
	deadline := time.Now().Add(testnats.Remaining(t))
	for client.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("the client was still connected through the cut hold at the test's deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	subject := contracts.AgentSubject(contracts.NoProject, "ses_lost_unsent")
	_, err := client.sendStored(ctx, subject, []byte(`{"unsent":true}`), "lost-unsent", true, nats.Context(ctx))
	if !errors.Is(err, ErrConnectionLost) || errors.Is(err, ErrUnacknowledged) {
		t.Fatalf("a publish made after its connection was lost answered %v, want ErrConnectionLost and no ErrUnacknowledged: it was never sent", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("the publish answered only once its own deadline ended (%v), want the loss at once", ctx.Err())
	}
}
