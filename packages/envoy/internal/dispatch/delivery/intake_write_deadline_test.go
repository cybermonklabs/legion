package delivery

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A write to a frozen link outside any stop holds the intake's connection lock only until the
// production write deadline (IntakeWriteDeadline), and no message is lost: the deadline ends the
// write, nats.go closes that socket and reconnects the same connection once the link takes it,
// and each message published around the freeze is handled once.
//
// The row runs a holder whose client writes with production's intake write deadline
// (bus.WithWriteDeadline at IntakeWriteDeadline) through a link (testnats.HandshakeHold). Two
// messages are handled before the freeze. The link then freezes with a write of nats.go's own
// stalled in the holder's socket (HoldStalledWrite), and a third message is published while it is
// frozen: the server pushes it to the frozen socket, which never hands it to the holder. The row
// requires that the write ends, so the connection is no longer connected, within
// IntakeWriteDeadline and 5 s of the freeze; then it thaws the link, requires the same connection
// to reconnect (its reconnect count grown by one), and publishes a fourth message. It requires
// that each of the four messages is handled once and acknowledged: the third comes back to the
// reconnected holder after the durable's ack wait (intakeTuning.ackWait, 80 s here, with
// shrinkIntakeFlowControl), since the server counted it delivered to the socket that went. No
// message is handed back outside a stop here: the GitHub intake hands one back only when a head
// write or an acknowledgement fails.
//
// Control: with nats.go's default write deadline (startIntakeProcess with no option), the write
// holds the lock for a minute, the connection is still connected 10 s after the freeze, and the
// row fails at that check.
func TestAWriteFrozenPastTheIntakesWriteDeadlineReconnectsAndLosesNoMessage(t *testing.T) {
	testnats.RequireSocketQueues(t)
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	link := testnats.StartHandshakeHold(t, natsURL)
	t.Cleanup(link.Restore)
	publisher := intakeTestClient(t)
	holder := startIntakeProcess(t, pool, link.URL(), nil, bus.WithWriteDeadline(IntakeWriteDeadline))
	awaitBoundDurable(t, publisher)
	conn := holder.client.Connection()
	before := conn.Stats().Reconnects

	numbers := []int{1, 2, 3, 4}
	for _, number := range numbers[:2] {
		publishMergedPullRequest(t, publisher, number)
	}
	awaitEverythingAcknowledged(t, publisher)

	thaw := link.HoldStalledWrite(t, conn)
	frozen := time.Now()
	writeBound := time.After(IntakeWriteDeadline + 5*time.Second)
	publishMergedPullRequest(t, publisher, numbers[2])
	// The read waits for nats.go's lock, which the frozen write holds, so it runs on a goroutine of
	// its own and the row's wait stays bounded.
	writeEnded := make(chan struct{})
	go func() {
		defer close(writeEnded)
		for conn.IsConnected() && t.Context().Err() == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}()
	select {
	case <-writeEnded:
	case <-writeBound:
		t.Fatalf("the holder's connection was still connected %s after its write froze: the write held nats.go's connection lock past the write deadline (%s)",
			time.Since(frozen).Round(time.Millisecond), IntakeWriteDeadline)
	}
	ended := time.Since(frozen)
	thaw()
	waitFor(t, 30*time.Second, func() bool { return conn.Stats().Reconnects == before+1 && conn.IsConnected() }, func() string {
		return fmt.Sprintf("the holder's connection did not reconnect within 30 s of the link thawing: reconnects %d, from %d before the freeze",
			conn.Stats().Reconnects, before)
	})
	publishMergedPullRequest(t, publisher, numbers[3])

	waitFor(t, tuningFor(t).ackWait()+30*time.Second, func() bool {
		for _, number := range numbers {
			if holder.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the holder did not handle every message within the ack wait and 30 s: fetches %v; %s", fetchesOf(holder, numbers), consumerState(publisher))
	})
	awaitEverythingAcknowledged(t, publisher)
	for _, number := range numbers {
		if got := holder.fetches(number); got != 1 {
			t.Errorf("the holder handled pull request %d %d times, want once: fetches %v", number, got, fetchesOf(holder, numbers))
		}
	}
	if got := conn.Stats().Reconnects; got != before+1 {
		t.Errorf("the holder's connection reconnected %d times, want once", got-before)
	}
	t.Logf("the write deadline ended the frozen write %s after the freeze", ended.Round(time.Millisecond))
}
