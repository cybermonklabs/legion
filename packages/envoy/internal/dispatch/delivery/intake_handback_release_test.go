package delivery

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// stopWithAHeldHandBack starts a holder and a standby, has the holder's handler hold one pull
// request with a second message of it waiting behind, stops the holder, and holds a write on the
// holder's link from inside the stop's hand-back of the waiting message, after the connected read
// has answered and before the publish (beforeHandBackPublish), so that publish waits for nats.go's
// lock. It returns once the write is held, with the stop under way. The stop's deadline is
// intakeStopBudget, shrunk to stopBudget.
func stopWithAHeldHandBack(t *testing.T) heldHandBack {
	t.Helper()
	shrinkIntakeFlowControl(t)
	budget := intakeStopBudget
	intakeStopBudget = stopBudget
	t.Cleanup(func() { intakeStopBudget = budget })
	var row heldHandBack
	pool, ctx := deliveryTestPool(t)
	row.pool = pool
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	link := testnats.StartHandshakeHold(t, natsURL)
	t.Cleanup(link.Restore)
	publisher := intakeTestClient(t)
	row.publisher = publisher
	row.recorded = watchDurableEvents(t, natsURL)

	const inFlight = 100
	holder := startIntakeProcess(t, pool, link.URL(), func(number int) bool { return number == inFlight })
	row.holder = holder
	awaitBoundDurable(t, publisher)
	startIntakeProcess(t, pool, natsURL, nil)
	holderConn := holder.client.Connection()
	row.conn = holderConn

	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, 20*time.Second, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	publishMergedPullRequest(t, publisher, inFlight)
	awaitAckPending(t, publisher, 2)

	var holding sync.Once
	held := make(chan struct{})
	thawed := make(chan func(), 1)
	hook := func() {
		holding.Do(func() {
			thawed <- link.HoldWriteLock(t, holderConn)
			close(held)
		})
	}
	row.delivery = subscribedDelivery(holder.intake)
	row.delivery.beforeHandBackPublish.Store(&hook)
	row.from = row.recorded.count()
	done := make(chan struct{})
	row.stopped = done
	go func() {
		holder.stop()
		close(done)
	}()
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
	row.thaw = <-thawed
	return row
}

// heldHandBack is a holder's stop with a hand-back's publish held (stopWithAHeldHandBack): thaw
// lets the holder's link move and ends the held write; stopped closes when the holder's Run has
// returned; conn is the holder's connection, delivery the subscription its stop stopped, and from
// the first durable event of the stop.
type heldHandBack struct {
	holder    *intakeProcess
	pool      *store.Pool
	thaw      func()
	recorded  *durableEvents
	from      int
	stopped   <-chan struct{}
	publisher *bus.Client
	conn      *natsgo.Conn
	delivery  *intakeDelivery
}

// handBackPending reports whether a hand-back publish of the stopped delivery is still waiting for
// NATS.
func (r heldHandBack) handBackPending() bool {
	select {
	case <-r.delivery.handBacksLanded():
		return false
	default:
		return true
	}
}

// stopBudget is the stop's deadline in the rows below (intakeStopBudget).
const stopBudget = 3 * time.Second

// holdLive reports whether the delivery durable's hold still names a holder, live.
func holdLive(t *testing.T, pool *store.Pool) bool {
	t.Helper()
	_, live, err := duty.HoldToken(t.Context(), pool, intakeHoldName(bus.DispatchDeliveryDurable))
	if err != nil {
		t.Fatalf("read the delivery durable's hold: %v", err)
	}
	return live
}

// holdRenewedBeforeItsExpiry reads the expiry the delivery durable's hold has now, then waits until
// a renewal moves it later or the database's clock passes it, and reports whether the renewal came
// first. A hold left to lapse is past its expiry within the hold's TTL; a 30 s wait past that only
// guards against a row that neither moves nor lapses.
func holdRenewedBeforeItsExpiry(t *testing.T, pool *store.Pool) bool {
	t.Helper()
	read := func() (expires, now time.Time) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), "select expires_at, now() from duty_holds where name = $1",
			intakeHoldName(bus.DispatchDeliveryDurable)).Scan(&expires, &now); err != nil {
			t.Fatalf("read the delivery durable's hold: %v", err)
		}
		return expires, now
	}
	first, _ := read()
	guard := time.Now().Add(intakeHoldTTL + 30*time.Second)
	for time.Now().Before(guard) {
		expires, now := read()
		switch {
		case expires.After(first):
			return true
		case !now.Before(first):
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the delivery durable's hold was neither renewed nor past its expiry %s after it was read", intakeHoldTTL+30*time.Second)
	return false
}

// requireHandBackBeforeCreation waits until the server has had both the holder's hand-back and the
// standby's creation of the durable, and fails the test when the hand-back came after.
func requireHandBackBeforeCreation(t *testing.T, recorded *durableEvents, from int, publisher *bus.Client) {
	t.Helper()
	waitFor(t, 60*time.Second, func() bool {
		events := recorded.since(from)
		return slicesContains(events, "-NAK") && slicesContains(events, durableCreated)
	}, func() string {
		return fmt.Sprintf("the hand-back and the standby's creation of the durable did not both reach the server: %q; %s", recorded.since(from), consumerState(publisher))
	})
	events := recorded.since(from)
	if indexOf(events, "-NAK") > indexOf(events, durableCreated) {
		t.Fatalf("the hand-back landed after the standby created the durable, so it reached the standby's consumer: %q", events)
	}
}

// A hand-back the holder's stop published never reaches the durable a standby recreates.
//
// A hand-back is a publish at the message's reply subject, which names the durable rather than the
// consumer: one that reaches the server after the standby's take-over has deleted the durable and
// created it again under the same name moves that message's pending entry on the standby's consumer
// and has the standby delivered it a second time. The stop waits for each publish at most
// bus.ConnLockWait (intakeDelivery.publishHandBack) and goes on without one NATS has not taken, so
// the stop releases the hold only once every hand-back has reached the server; one still pending at
// the stop's deadline keeps the hold renewing until it has (heldDurable.finalizeHandBacks).
//
// Both subtests hold a write on the holder's link from inside its stop's hand-back
// (stopWithAHeldHandBack). Each reads, in the order the server sent them, the answers published to
// the durable and the durable's creations, on a connection of its own, and every step waits for an
// event:
//   - a hand-back that lands before the stop's deadline: the link moves once the stop has gone on
//     without the publish (its log line). The hold is released by the time the stop returns, and the
//     hand-back reached the server before the standby created the durable.
//   - a hand-back still pending at the stop's deadline: the link stays held, and the stop returns
//     while it is, with the hand-back still pending (handBacksLanded). The hold still names the
//     holder then, and the standby has not created the durable. With the link still held, the hold is
//     renewed before the database's clock passes the expiry it had as the stop returned, so it does
//     not lapse, and the standby still has not created the durable. Once the link moves, the
//     hand-back reaches the server before the standby creates the durable;
//   - a pending hand-back at shutdown: the Run returns at its deadline, then StartIntakes' stop
//     closes the client that can still send it and waits for its finalizer. The hold is gone before
//     that stop returns, so its caller can close the pool without leaving the hold to lapse.
//
// No step waits on a time but the stop's own deadline (intakeStopBudget) and the hold's own expiry;
// a 30 s wait only guards against a stop that never returns. The negative controls: a release that
// does not wait for the hand-backs fails the first's order; a stop that never releases its hold when
// a publish was left running fails the first's release; a release made at the deadline with a
// hand-back pending fails the second's hold; a stop that leaves its hold to lapse with a hand-back
// pending (this change's earlier 884d560ceb) fails the second's renewal; and f83e859506's untracked
// finalizer fails the third by returning from its shutdown stop with the hold live.
func TestAHandBackLeftRunningLandsBeforeTheStandbyTakesOver(t *testing.T) {
	t.Run("a hand-back that lands before the stop's deadline", func(t *testing.T) {
		logs := captureLogs(t)
		row := stopWithAHeldHandBack(t)
		waitFor(t, 30*time.Second, func() bool { return loggedMessage(logs(), callLeftRunning) }, func() string {
			return "control: the stop never went on without the held hand-back's publish: " + consumerState(row.publisher)
		})
		row.thaw()
		select {
		case <-row.stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the holder's stop did not return within 30 s of the link moving")
		}
		if holdLive(t, row.pool) {
			t.Fatalf("the stop kept its hold although its hand-back reached the server before its deadline, which costs the standby the lapse; the holder logged %q", loggedMessages(logs()))
		}
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher)
	})

	t.Run("a hand-back still pending at the stop's deadline", func(t *testing.T) {
		row := stopWithAHeldHandBack(t)
		select {
		case <-row.stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the holder's stop did not return within 30 s while its hand-back was held: it waits for the hand-back")
		}
		testnats.RequireLockHeld(t, row.conn)
		if !row.handBackPending() {
			t.Fatal("control: the stop's hand-back reached NATS while the write was held")
		}
		if !holdLive(t, row.pool) {
			t.Fatal("the stop released its hold with a hand-back still pending, so a standby may recreate the durable before the hand-back reaches the server")
		}
		if slicesContains(row.recorded.since(row.from), durableCreated) {
			t.Fatalf("the standby created the durable while the holder's hand-back was pending: %q", row.recorded.since(row.from))
		}
		if !holdRenewedBeforeItsExpiry(t, row.pool) {
			t.Fatalf("the hold reached its expiry unrenewed with the holder's hand-back still pending, so the standby can recreate the durable before a link that heals sends the hand-back to its consumer: %q", row.recorded.since(row.from))
		}
		testnats.RequireLockHeld(t, row.conn)
		if !row.handBackPending() {
			t.Fatal("control: the stop's hand-back reached NATS while the write was held")
		}
		if slicesContains(row.recorded.since(row.from), durableCreated) {
			t.Fatalf("the standby created the durable while the holder's hand-back was pending and its hold renewing: %q", row.recorded.since(row.from))
		}
		row.thaw()
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher)
	})

	t.Run("a pending hand-back at shutdown", func(t *testing.T) {
		row := stopWithAHeldHandBack(t)
		select {
		case <-row.stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the holder's Run did not return within 30 s while its hand-back was held")
		}
		if !row.handBackPending() {
			t.Fatal("control: the hand-back reached NATS before its client closed")
		}
		var runs sync.WaitGroup
		closed := make(chan struct{})
		go func() {
			stopIntakes(&runs, []*bus.Client{row.holder.client}, &row.holder.intake.heldDurable)
			close(closed)
		}()
		testnats.RequireLockHeld(t, row.conn)
		row.thaw()
		select {
		case <-closed:
		case <-time.After(30 * time.Second):
			t.Fatal("StartIntakes' stop did not return within 30 s of the link moving")
		}
		if holdLive(t, row.pool) {
			t.Fatal("the pending hand-back's finalizer returned after the client closed but left its hold live: shutdown could close the pool before releasing it")
		}
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher)
	})
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
