package bus

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A client made with WithWriteDeadline closes within its deadline and a second while a write to a
// socket that takes no byte holds nats.go's connection lock. The link freezes and a write of
// nats.go's own stalls in its socket (testnats.HandshakeHold.HoldStalledWrite), and the link is
// never thawed: only the deadline ends the write, and Close, which waits for the lock, then takes
// it. Without the option the write would hold the lock, and Close with it, for nats.go's default
// minute.
func TestAClientWithAWriteDeadlineClosesWithinItWhileAWriteIsFrozen(t *testing.T) {
	testnats.RequireSocketQueues(t)
	const deadline = 2 * time.Second
	client, hold := heldHandshakeClient(t, WithName(t.Name()), WithWriteDeadline(deadline))
	conn := client.Connection()
	writeHeld := time.Now()
	hold.HoldStalledWrite(t, conn)
	testnats.RequireLockHeld(t, conn)

	closing := time.Now()
	testnats.AnswersWithin(t, deadline+time.Second, "Close", func() error {
		client.Close()
		return nil
	})
	if !conn.IsClosed() {
		t.Fatal("Close returned, but the connection has not closed")
	}
	t.Logf("Close returned %s after its call and %s after the frozen write began", time.Since(closing).Round(time.Millisecond), time.Since(writeHeld).Round(time.Millisecond))
}
