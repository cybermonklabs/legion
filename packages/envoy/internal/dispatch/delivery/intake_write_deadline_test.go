package delivery

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// frozenWriteDeadline is the write deadline the frozen-write row's holder writes with
// (bus.WithWriteDeadline): a fifth of production's IntakeWriteDeadline, which
// TestAnIntakeClientWritesWithProductionsWriteDeadline holds. It is a socket deadline nats.go keeps
// on the wall clock, which no clock of this module reaches (INFRA-3149 allowed a row's own value,
// dispatch://INFRA-3149/comment/cc01d314-cbbc-4990-b232-cb40ac523f86, item 9). It stays far above
// what a write the socket takes whole needs, and above what HoldStalledWrite needs to read the
// write it stalled as stalled for good, which the deadline would otherwise end first.
const frozenWriteDeadline = time.Second

// The frozen-write row's ack wait (intakeTuning.ackWait): its holder's maxAckPending (4) times
// frozenWriteMessageTimeout, plus frozenWriteAckLanding, 5 s. The third message comes back to the
// holder only at the server's redelivery once that ack wait has passed, which is the row's claim,
// and the row counts each message handled once, so the ack wait stays far above one handling and
// its acknowledgement's trip: about 717 ms together at the largest docs/testing.md records ("A test
// asserts no count of work a broker may repeat"). Both are the server's span, which no clock of
// this module reaches (INFRA-3149's item 8). The handlers' own deadline is on the holder's clock, a
// fake the row never moves, so frozenWriteMessageTimeout cuts no handling short.
const (
	frozenWriteMessageTimeout = time.Second
	frozenWriteAckLanding     = time.Second
)

// A write to a frozen link outside any stop holds the intake's connection lock only until the
// client's write deadline, and no message is lost: the deadline ends the write, nats.go closes that
// socket and reconnects the same connection once the link takes it, and each message published
// around the freeze is handled once.
//
// The row runs a holder whose client writes with a deadline of the row's own
// (bus.WithWriteDeadline at frozenWriteDeadline; production's IntakeWriteDeadline, and the bus
// option that applies it, are TestAnIntakeClientWritesWithProductionsWriteDeadline's) through a link
// (testnats.HandshakeHold). Two messages are handled before the freeze. The link then freezes with a
// write of nats.go's own stalled in the holder's socket (HoldStalledWrite), and a third message is
// published while it is frozen: the server pushes it to the frozen socket, which never hands it to
// the holder. The row waits for the write to end, so the connection is no longer connected, and
// requires that it ended within the write deadline and as long again of the freeze, the ratio the
// row kept when it wrote with production's deadline (IntakeWriteDeadline and 5 s). Then it thaws the
// link, requires the same connection to reconnect (its reconnect count grown by one), and publishes
// a fourth message. It requires that each of the four messages is handled once and acknowledged: the
// third comes back to the reconnected holder after the durable's ack wait (frozenWriteMessageTimeout
// and frozenWriteAckLanding), since the server counted it delivered to the socket that went. No
// message is handed back outside a stop here: the GitHub intake hands one back only when a head
// write or an acknowledgement fails. What the row waits out is the write deadline, a socket's, and
// the server's redelivery after the ack wait, which is its claim.
//
// Control: with nats.go's default write deadline (startIntakeProcess with no option), the write
// holds the lock for a minute, the connection is still connected twice frozenWriteDeadline after
// the freeze, and the row fails at that check.
func TestAWriteFrozenPastTheIntakesWriteDeadlineReconnectsAndLosesNoMessage(t *testing.T) {
	t.Parallel()
	testnats.RequireSocketQueues(t)
	tuning := tuningFor(t)
	tuning.messageTimeout, tuning.ackLanding = frozenWriteMessageTimeout, frozenWriteAckLanding
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	link := testnats.StartHandshakeHold(t, natsURL)
	t.Cleanup(link.Restore)
	publisher := intakeTestClient(t)
	holder := startIntakeProcessWith(t, pool, link.URL(), nil, func(p *intakeProcess) { p.intake.clock = fakeIntakeClock() },
		bus.WithWriteDeadline(frozenWriteDeadline))
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
	publishMergedPullRequest(t, publisher, numbers[2])
	// The read waits for nats.go's lock, which the frozen write holds, so it runs on a goroutine of
	// its own, through the shared wait's form for a helper, and the row waits for its answer.
	writeEnded := make(chan error, 1)
	go func() {
		writeEnded <- testwait.InHelper("the frozen-write row", "the holder's connection to stop reading connected once its frozen write ended",
			func() bool { return !conn.IsConnected() })
	}()
	if err := testwait.Receive(t, "the frozen write to end and the holder's connection to stop reading connected", writeEnded); err != nil {
		t.Fatal(err)
	}
	ended := time.Since(frozen)
	if bound := 2 * frozenWriteDeadline; ended > bound {
		t.Fatalf("the holder's connection was still connected %s after its write froze, past twice its write deadline (%s): the write held nats.go's connection lock past the deadline",
			ended.Round(time.Millisecond), frozenWriteDeadline)
	}
	thaw()
	waitFor(t, func() bool { return conn.Stats().Reconnects == before+1 && conn.IsConnected() }, func() string {
		return fmt.Sprintf("the holder's connection did not reconnect once the link thawed: reconnects %d, from %d before the freeze",
			conn.Stats().Reconnects, before)
	})
	publishMergedPullRequest(t, publisher, numbers[3])

	waitFor(t, func() bool {
		for _, number := range numbers {
			if holder.fetches(number) == 0 {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("the holder did not handle every message: fetches %v; %s", fetchesOf(holder, numbers), consumerState(publisher))
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

// An intake's client writes with production's write deadline, IntakeWriteDeadline, 5 s: the value
// cmd/dispatch connects every intake's client with (bus.WithWriteDeadline), which the bus hands
// nats.go as the deadline of each write to the socket (FlusherTimeout). The frozen-write row above
// writes with a deadline of its own; this row holds production's value, and that a client connected
// with it, as cmd/dispatch connects an intake's, writes with it.
func TestAnIntakeClientWritesWithProductionsWriteDeadline(t *testing.T) {
	t.Parallel()
	if IntakeWriteDeadline != 5*time.Second {
		t.Errorf("IntakeWriteDeadline = %s, want production's 5s", IntakeWriteDeadline)
	}
	client := intakeTestClient(t, bus.WithName(intakeConnectionName), bus.WithWriteDeadline(IntakeWriteDeadline))
	if got := client.Connection().Opts.FlusherTimeout; got != IntakeWriteDeadline {
		t.Errorf("a client connected with bus.WithWriteDeadline(IntakeWriteDeadline) writes with a deadline of %s, want %s", got, IntakeWriteDeadline)
	}
}
