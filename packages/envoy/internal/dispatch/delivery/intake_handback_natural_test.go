package delivery

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A lost hold ends the connection of a pending hand-back even when a read of that connection holds
// the bus client's lock. The holder's stop holds two messages of a pull request behind the one in its
// handler. The first hand-back's publish waits behind a held write, and the second's connected read
// (mayHandBack) waits for nats.go's lock while it holds the client's, and is left running. The
// holder's renewals are then refused: by the time the standby creates the durable, the connection
// that carries the pending hand-back must be closed, and no -NAK may reach the server after the
// creation. A Sever that took the client's lock waited behind that read, and the standby created
// the durable with the connection still open.
func TestALostHoldSeversAPendingHandBackBehindAConnectedRead(t *testing.T) {
	testnats.RequireSocketQueues(t)
	shrinkIntakeFlowControl(t)
	tuning := tuningFor(t)
	tuning.stopBudget = stopBudget
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

	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	waitFor(t, 20*time.Second, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	waitFor(t, 20*time.Second, func() bool { return queuedBehindKeys(subscribedDelivery(holder.intake)) == 2 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind the holder's handler, want the second and third messages: %s", queuedBehindKeys(subscribedDelivery(holder.intake)), consumerState(publisher))
	})

	wait := tuning.callWait.Swap(int64(30 * time.Second))
	restoreWait := sync.OnceFunc(func() { tuning.callWait.Store(wait) })
	t.Cleanup(restoreWait)
	var holding sync.Once
	held := make(chan struct{})
	thawed := make(chan func(), 1)
	hook := func() {
		holding.Do(func() {
			restoreWait()
			thawed <- link.HoldStalledWrite(t, holderConn)
			close(held)
		})
	}
	row.delivery = subscribedDelivery(holder.intake)
	row.delivery.beforeHandBackPublish.Store(&hook)
	row.from = row.recorded.count()
	done := make(chan struct{})
	go func() {
		holder.stop()
		close(done)
	}()
	waitFor(t, 20*time.Second, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	holder.release()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the stop's hand-back never reached its publish")
	}
	row.thaw = <-thawed
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's Run did not return within 30 s while its hand-back was held")
	}
	if !row.handBackPending() {
		t.Fatal("control: the stop's hand-back reached NATS while the write was held")
	}
	if !holdLive(t, row.pool, row.holder.intake.holder) {
		t.Fatal("control: the stop released its hold with its hand-back pending")
	}
	row.holder.leases.cut.Store(true)
	waitFor(t, 60*time.Second, func() bool { return slices.Contains(row.recorded.since(row.from), durableCreated) }, func() string {
		return fmt.Sprintf("the standby did not take over the lapsed hold and create the durable: %q; %s", row.recorded.since(row.from), consumerState(row.publisher))
	})
	closed := make(chan bool, 1)
	go func() { closed <- row.conn.IsClosed() }()
	var ended bool
	select {
	case ended = <-closed:
	case <-time.After(2 * time.Second):
	}
	pendingAtCreation := row.handBackPending()
	row.thaw()
	select {
	case <-row.delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("the hand-back's publish had not returned 30 s after the link moved")
	}
	if err := row.conn.FlushTimeout(10 * time.Second); err != nil && !errors.Is(err, natsgo.ErrConnectionClosed) {
		t.Fatalf("flush the holder's connection once its link moved: %v", err)
	}
	row.recorded.barrier(t)
	t.Logf("at the standby's creation: holder connection closed=%v, hand-back pending=%v; events after: %q", ended, pendingAtCreation, row.recorded.since(row.from))
	if !ended {
		t.Errorf("the standby created the durable while the connection carrying the pending hand-back was still open (its held write still held nats.go's lock 2 s later): the lost hold's sever had not ended it")
	}
	requireNoHandBackAfterCreation(t, row.recorded, row.from, row.publisher, row.sequences)
}

// A lost hold ends the connection of an unconfirmed hand-back even while the finalizer's flush waits
// for nats.go's lock. The holder's stop hands back the message behind its handler onto a frozen
// link: the publish lands in the socket, the stop's flush gets no answer, and the stop returns with
// the hold renewing (finalizeHandBacks). Then a stalled write holds nats.go's lock, so the
// finalizer's next flush waits for that lock with no bound of its own, and the holder's renewals are
// refused. By the time the standby creates the durable, the holder's connection must be closed. A
// finalizer that read the lost hold only between its flushes, and severed from there, waited inside
// that flush, and the standby created the durable with the connection still open. The row checks the
// connection and not the order of events: what the frozen relay had already read of the socket
// before the sever is beyond the client, and reaches the server once the relay thaws.
func TestALostHoldSeversAnUnconfirmedHandBackWhileTheFinalizersFlushWaits(t *testing.T) {
	testnats.RequireSocketQueues(t)
	logs := captureIntakeLogs(t)
	shrinkIntakeFlowControl(t)
	tuning := tuningFor(t)
	tuning.stopBudget = stopBudget
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
	startIntakeProcess(t, pool, natsURL, nil)
	holderConn := holder.client.Connection()
	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, 20*time.Second, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, 20*time.Second, func() bool { return queuedBehindKeys(subscribedDelivery(holder.intake)) == 1 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind the holder's handler, want the second message: %s", queuedBehindKeys(subscribedDelivery(holder.intake)), consumerState(publisher))
	})

	// The stop's hand-backs go out only once its drain has answered; the row gives the stop's calls
	// a 30 s guard, and puts the wait back as the hand-back's publish begins (stopWithAHeldHandBack).
	wait := tuning.callWait.Swap(int64(30 * time.Second))
	restoreWait := sync.OnceFunc(func() { tuning.callWait.Store(wait) })
	t.Cleanup(restoreWait)
	var freezing sync.Once
	frozen := make(chan struct{})
	hook := func() {
		freezing.Do(func() {
			restoreWait()
			link.Freeze()
			close(frozen)
		})
	}
	delivery := subscribedDelivery(holder.intake)
	delivery.beforeHandBackPublish.Store(&hook)
	from := recorded.count()
	stopped := make(chan struct{})
	go func() {
		holder.stop()
		close(stopped)
	}()
	waitFor(t, 20*time.Second, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	holder.release()
	select {
	case <-frozen:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the stop's hand-back never reached its publish")
	}
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's Run did not return within 30 s of its hand-back")
	}
	select {
	case <-delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("control: the hand-back's publish did not land in the frozen link's socket")
	}
	if !loggedMessage(logs(), intakeHoldFinalizing) || !holdLive(t, pool, holder.intake.holder) {
		t.Fatalf("control: the stop released its hold although no flush could confirm its hand-back; logged %q", loggedMessages(logs()))
	}
	thaw := link.HoldStalledWrite(t, holderConn)
	holder.leases.cut.Store(true)
	waitFor(t, 60*time.Second, func() bool { return slices.Contains(recorded.since(from), durableCreated) }, func() string {
		return fmt.Sprintf("the standby did not take over the lapsed hold and create the durable: %q; %s", recorded.since(from), consumerState(publisher))
	})
	closed := make(chan bool, 1)
	go func() { closed <- holderConn.IsClosed() }()
	ended := false
	select {
	case ended = <-closed:
	case <-time.After(2 * time.Second):
	}
	thaw()
	if !ended {
		t.Fatalf("the standby created the durable while the connection carrying the unconfirmed hand-back was still open: the finalizer's flush waited for nats.go's lock, and nothing severed the connection; events %q", recorded.since(from))
	}
}
