package delivery

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// durableEvents records, in the order the server sent them, every answer published to the delivery
// durable ($JS.ACK...) and every creation of that durable ($JS.EVENT.ADVISORY.CONSUMER.CREATED...),
// on a connection of the test's own to uri. Both subscriptions feed one channel, which nats.go fills
// in the order the server's messages arrive, so the record orders an answer against a creation.
type durableEvents struct {
	mu     sync.Mutex
	events []string
}

const durableCreated = "created"

func watchDurableEvents(t *testing.T, uri string) *durableEvents {
	t.Helper()
	conn := testnats.Connect(t, uri)
	arrived := make(chan *natsgo.Msg, 4096)
	for _, subject := range []string{
		"$JS.ACK." + bus.Stream + "." + bus.DispatchDeliveryDurable + ".>",
		"$JS.EVENT.ADVISORY.CONSUMER.CREATED." + bus.Stream + "." + bus.DispatchDeliveryDurable,
	} {
		if _, err := conn.ChanSubscribe(subject, arrived); err != nil {
			t.Fatalf("subscribe to %s: %v", subject, err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the durable's watch: %v", err)
	}
	recorded := &durableEvents{}
	go func() {
		for msg := range arrived {
			event := string(msg.Data)
			if strings.HasPrefix(msg.Subject, "$JS.EVENT.") {
				event = durableCreated
			}
			recorded.mu.Lock()
			recorded.events = append(recorded.events, event)
			recorded.mu.Unlock()
		}
	}()
	return recorded
}

// since is every event recorded from the from'th on.
func (r *durableEvents) since(from int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events[from:]...)
}

func (r *durableEvents) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// A hand-back NATS had not taken when the holder's stop went on without it lands before the
// standby takes the durable over, never on the durable the standby recreates.
//
// A hand-back is a publish at the message's reply subject, which names the durable rather than the
// consumer: one that lands after the standby's take-over has deleted the durable and created it again
// under the same name moves that message's pending entry on the standby's consumer and has the
// standby delivered it a second time. The stop waits for each publish at most bus.ConnLockWait
// (intakeDelivery.publishHandBack) and goes on without one NATS has not taken, so the hold's release,
// which lets the standby take over, waits for every such publish first (heldDurable.awaitHandBacks).
//
// The holder's NATS link runs through a proxy of its own. The holder's handler holds one pull request
// and a second message of it waits behind; the handler is let finish once the stop has stopped the
// keyed runner, and a write is held on the holder's link from inside the stop's hand-back of the
// waiting message, after the connected read has answered and before the publish
// (beforeHandBackPublish). That publish is left running. The link moves again when either event
// comes: the holder's release is waiting for the hand-back (awaitingHandBacks), or the standby has
// created the durable. Every step waits for an event; none for a time.
//
// The row reads, in the order the server sent them, the answers published to the durable and the
// durable's creations after the stop began: a hand-back (-NAK) must be among them, and must come
// before the standby's creation. The negative control is a release that does not wait for the
// hand-backs: the standby creates the durable first, the link then moves, and the hand-back lands
// after the creation.
func TestAHandBackLeftRunningLandsBeforeTheStandbyTakesOver(t *testing.T) {
	shrinkIntakeFlowControl(t)
	// The stop gives the drain, which the held write keeps from ending, this long to end.
	budget := intakeStopBudget
	intakeStopBudget = 3 * time.Second
	t.Cleanup(func() { intakeStopBudget = budget })
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	link := testnats.StartHandshakeHold(t, natsURL)
	t.Cleanup(link.Restore)
	publisher := intakeTestClient(t)
	recorded := watchDurableEvents(t, natsURL)

	const inFlight = 100
	holder := startIntakeProcess(t, pool, link.URL(), func(number int) bool { return number == inFlight })
	awaitBoundDurable(t, publisher)
	standby := startIntakeProcess(t, pool, natsURL, nil)
	holderConn := holder.client.Connection()

	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, 20*time.Second, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	publishMergedPullRequest(t, publisher, inFlight)
	awaitAckPending(t, publisher, 2)

	var holding sync.Once
	held := make(chan struct{})
	hook := func() {
		holding.Do(func() {
			link.HoldWriteLock(t, holderConn)
			close(held)
		})
	}
	subscribedDelivery(holder.intake).beforeHandBackPublish.Store(&hook)
	from := recorded.count()
	go holder.stop()
	waitFor(t, 20*time.Second, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	// The handler finishes as the stop waits for it; the stop then hands back the message that
	// was waiting behind it, and the hook holds the write before that publish.
	holder.release()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the stop's hand-back never reached its publish: " + consumerState(publisher))
	}

	created := func() bool { return slicesContains(recorded.since(from), durableCreated) }
	waitFor(t, 60*time.Second, func() bool { return holder.intake.awaitingHandBacks.Load() || created() }, func() string {
		return fmt.Sprintf("neither did the holder's release wait for the hand-back nor did the standby create the durable: %q; %s", recorded.since(from), consumerState(publisher))
	})
	link.Restore()

	waitFor(t, 60*time.Second, func() bool {
		events := recorded.since(from)
		return slicesContains(events, "-NAK") && slicesContains(events, durableCreated)
	}, func() string {
		return fmt.Sprintf("the hand-back and the standby's creation of the durable did not both reach the server once the link moved: %q; standby fetched %d; %s",
			recorded.since(from), standby.fetches(inFlight), consumerState(publisher))
	})
	events := recorded.since(from)
	nak, creation := indexOf(events, "-NAK"), indexOf(events, durableCreated)
	if nak > creation {
		t.Fatalf("the hand-back the stop left running landed after the standby created the durable, so it reached the standby's consumer: %q", events)
	}
}

func slicesContains(events []string, want string) bool { return indexOf(events, want) >= 0 }

func indexOf(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}
