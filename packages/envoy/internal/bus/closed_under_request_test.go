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

// A stored write the bus sends once (a publish whose caller reads the duplicate verdict, a cursor
// write under an expected last sequence) whose connection closes under it answers
// ErrConnectionLost, as one whose connection dropped does, and Reconnected sends it again once the
// client has replaced the connection. Such a close is nats.go's own close of a running client's
// connection, which it makes after the same authorization error twice in a row or an
// unclassified -ERR (connect.go's options); the client then dials a replacement (onClosed). The
// hold withholds the write's acknowledgement and the row closes the client's connection while it
// is withheld, so the close comes between the send and its answer. nats.go's close answers the
// pending request before it reports CLOSED to the listener that ends the request's life
// (bindRequest), so the answer can reach the request first.
//
// The control is these rows at 190f7cb177, which named a closed answer by the request's life alone
// and passed nats.ErrConnectionClosed on when the life had not ended yet: the keep's Reconnected
// and the bus's own retry read that error as no reason to send again.
func TestAWriteSentOnceWhoseConnectionClosesUnderItAnswersConnectionLost(t *testing.T) {
	for _, row := range []struct {
		name, subject string
		send          func(context.Context, *Client, string) error
	}{
		{"a publish under a message id whose caller reads the verdict", contracts.AgentSubject(contracts.NoProject, "ses_closed_stored"), func(ctx context.Context, c *Client, subject string) error {
			item := contracts.Envelope{Topic: subject, Source: "test", DedupeKey: "closed-stored"}
			_, err := c.PublishStoredAck(ctx, subject, item, "closed-stored")
			return err
		}},
		{"a cursor write under an expected last sequence", KeptRoleCursorSubject("closed-cursor"), func(ctx context.Context, c *Client, subject string) error {
			_, err := c.PublishCursorAt(ctx, subject, []byte(`{"kept":7}`), 0)
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			client, hold := heldClient(t)
			held := hold.Hold(row.subject)
			answered := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
				defer cancel()
				answered <- row.send(ctx, client, row.subject)
			}()
			select {
			case <-held.Withheld():
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("the hold withheld no acknowledgement by the test's deadline")
			}
			closed := time.Now()
			client.Connection().Close()
			var err error
			select {
			case err = <-answered:
			case <-time.After(testnats.Remaining(t)):
				t.Fatal("the write whose connection closed under it was still waiting at the test's deadline")
			}
			if !errors.Is(err, ErrConnectionLost) {
				t.Fatalf("the write answered %v %s after the close (nats.ErrConnectionClosed: %t), want ErrConnectionLost",
					err, time.Since(closed), errors.Is(err, nats.ErrConnectionClosed))
			}
			answeredAfter := time.Since(closed)
			ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
			defer cancel()
			if !Reconnected(ctx, err) {
				t.Fatal("Reconnected found no replacement for the closed connection by the row's deadline")
			}
			t.Logf("the write answered ErrConnectionLost %s after the close, and the client replaced the connection %s after it",
				answeredAfter, time.Since(closed))
		})
	}
}
