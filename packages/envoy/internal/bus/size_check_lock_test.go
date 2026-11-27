package bus

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// smallMaxPayload is the max payload of the server this file starts: far under the 1 MiB default,
// so the message its row measures is about 64 KiB, the bound plus one byte and the envelope's
// fields. It is kvchunktest.MaxPayload's value, written out because that package imports this one.
const smallMaxPayload = 64 << 10

// The size checks read the max payload the client keeps (Client.maxPayload) and no state of
// nats.go's, so they answer while a reconnect attempt holds nats.go's connection lock across its
// handshake, which nats.Conn.MaxPayload waits for. The client reaches its own server through a
// handshake hold (testnats.HandshakeHold), the hold drops the connection, and the row waits until
// the reconnect's CONNECT has reached the hold and the lock is taken (HoldReconnect). Then
// TooLargeToPublish and a core publish of a message past the bound must each answer ErrTooLarge
// within that attempt (AnswersWithinAttempt): once both have answered the lock must still be taken,
// nats.go must not have let it go since they began, and no later attempt may have reached the hold.
// A check that waited for the lock answers only once the attempt ends at the client's connect
// timeout, so the subtest fails on the lock's state, and the row then fails on the checks by name:
// they waited for nats.go's connection lock. The verdict reads the lock's own state and the hold's
// counts, never how long a call took; AnswersWithinAttempt's one wall-clock bound is its 30 s guard
// against a call that never answers.
func TestTheSizeChecksAnswerWhileAReconnectHoldsTheConnectionLock(t *testing.T) {
	t.Parallel()
	_, server := testnats.StartConfig(t, "max_payload: "+strconv.Itoa(smallMaxPayload)+"\njetstream {}\n", []string{"-js"}, true)
	hold := testnats.StartHandshakeHold(t, server)
	client, err := Connect([]string{hold.URL()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect through the handshake hold: %v", err)
	}
	// nats.go's Close waits for its connection lock, which a held handshake holds, so the hold is
	// released and the proxy restored before the client closes (heldHandshakeClient).
	t.Cleanup(client.Close)
	t.Cleanup(hold.Restore)
	t.Cleanup(hold.Release)
	if maxPayload := client.maxPayload.Load(); maxPayload != smallMaxPayload {
		t.Fatalf("UNKNOWN: the connected client keeps a max payload of %d, want the server's %d", maxPayload, smallMaxPayload)
	}
	const topic = contracts.RoleTopicPrefix + "held-handshake"
	big := sizeEnvelope(topic, strings.Repeat("p", smallMaxPayload+1))
	conn := client.Connection()

	hold.HoldReconnect(t, conn)
	var answers []error
	if !t.Run("within the held attempt", func(t *testing.T) {
		answers = hold.AnswersWithinAttempt(t, conn, "a size check of a message past the max payload",
			func() error { return client.TooLargeToPublish(big) },
			func() error { return client.PublishCoreContext(context.Background(), topic, big) })
	}) {
		t.Fatal("the size checks waited for nats.go's connection lock: they did not both answer within the reconnect attempt that held it, as the subtest above reads")
	}
	for i, err := range answers {
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("size check %d answered %v while the reconnect held the lock, want ErrTooLarge", i+1, err)
		}
	}
}
