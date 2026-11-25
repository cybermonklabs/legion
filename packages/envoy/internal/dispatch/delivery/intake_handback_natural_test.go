package delivery

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A lost hold ends the connection of a pending hand-back even when a read of that connection holds
// the bus client's lock. The holder's stop holds two messages of a pull request behind the one in its
// handler. The first hand-back's publish waits behind a held write, and the second's connected read
// (mayHandBack) waits for nats.go's lock while it holds the client's, and is left running. The
// holder's renewals are then refused: the connection that carries the pending hand-back must be
// closed before the standby can create the durable, and no -NAK may reach the server after the
// creation. A Sever that took the client's lock waited behind that read, and left the connection
// open.
//
// The stop's deadline is on the holder's stop clock (heldDurable.clock), a fake the row moves once
// the write is held (driveStop). The hold runs on a fake of its own (heldDurable.holdClock), and its
// row is kept live on the database's clock until the row lapses it (keepHoldRowLive): the row moves
// the hold's clock to the loss deadline and past the wait the loss watcher arms for a flush's answer
// there, until the connection is closed (loseHoldUntilSevered). Only then does it lapse the hold's
// row (lapseHoldRow), so the standby takes the durable over; once the standby has created it, the
// row lets the link move, waits until the hand-back's publish has returned, flushes the holder's
// connection unless it has closed, and takes a barrier of its watch before it reads the events.
func TestALostHoldSeversAPendingHandBackBehindAConnectedRead(t *testing.T) {
	testnats.RequireSocketQueues(t)
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
	row.stopClock = clocktest.New(time.Now())
	row.holdClock = clocktest.New(time.Now())
	// The hand-back is what must bring the messages to the standby, so the server's own redelivery
	// at the ack wait never comes inside the run: both intakes take a message timeout past it
	// (ackWaitPastTheRun).
	holder := startIntakeProcessWith(t, pool, link.URL(), func(number int) bool { return number == inFlight }, func(p *intakeProcess) {
		ackWaitPastTheRun(p.intake)
		p.intake.clock = row.stopClock
		p.intake.holdClock = row.holdClock
	})
	row.holder = holder
	awaitBoundDurable(t, publisher)
	keepHoldRowLive(t, pool, holder)
	startIntakeProcessWith(t, pool, natsURL, nil, processAckWaitPastTheRun)
	holderConn := holder.client.Connection()
	row.conn = holderConn

	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	waitFor(t, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder has not started on the first message: " + consumerState(publisher)
	})
	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	// The two later messages have to be in the holder's hands, queued behind the first, before the
	// stop begins: the second's hand-back is the connected read that holds the client's lock. The
	// durable's count of messages outstanding does not say that (stopWithAHeldHandBack): the server
	// counts a message delivered before it is on the wire, and a stop whose drain reaches the server
	// first leaves it with the durable.
	row.delivery = subscribedDelivery(holder.intake)
	waitFor(t, func() bool { return queuedBehindKeys(row.delivery) == 2 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind the holder's handler, want the two later ones: %s", queuedBehindKeys(row.delivery), consumerState(publisher))
	})

	// The stop's hand-backs go out only once its drain has answered (mayHandBack), so the stop's
	// calls wait for NATS with no bound of the row's, and the hook puts the wait back as the first
	// hand-back's publish begins (stopWithAHeldHandBack).
	tuning := tuningFor(t)
	wait := tuning.callWait.Swap(math.MaxInt64)
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
	row.delivery.beforeHandBackPublish.Store(&hook)
	row.from = row.recorded.count()
	row.stopped = holder.stopping()
	waitFor(t, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	holder.release()
	testwait.Receive(t, "the stop's hand-back to reach its publish (control)", held)
	row.thaw = testwait.Receive(t, "the hook's stalled write, sent before held closed", thawed)
	driveStop(t, row.stopClock, row.stopped)
	if !row.handBackPending() {
		t.Fatal("control: the stop's hand-back reached NATS while the write was held")
	}
	if !holdLive(t, row.pool) {
		t.Fatal("control: the stop released its hold with its hand-back pending")
	}
	row.holder.leases.cut.Store(true)
	loseHoldUntilSevered(t, row.holdClock, row.holder,
		"the lost hold's sever to close the connection that carries the pending hand-back (a sever that waits behind the connected read's lock leaves it open)",
		connectionClosed(row.conn))
	pendingAtSever := row.handBackPending()
	lapseHoldRow(t, row.pool)
	waitFor(t, func() bool { return slices.Contains(row.recorded.since(row.from), durableCreated) }, func() string {
		return fmt.Sprintf("the standby has not taken over the lapsed hold and created the durable: %q; %s", row.recorded.since(row.from), consumerState(row.publisher))
	})
	row.thaw()
	testwait.Receive(t, "the hand-back's publish to return once the link moved", row.delivery.handBacksLanded())
	if err := row.conn.FlushWithContext(testnats.Context(t)); err != nil && !errors.Is(err, natsgo.ErrConnectionClosed) {
		t.Fatalf("flush the holder's connection once its link moved: %v", err)
	}
	row.recorded.barrier(t)
	t.Logf("at the sever: hand-back pending=%v; events after: %q", pendingAtSever, row.recorded.since(row.from))
	requireNoHandBackAfterCreation(t, row.recorded, row.from, row.publisher, row.sequences)
}

// A lost hold ends the connection of an unconfirmed hand-back even while the finalizer's flush waits
// for nats.go's lock. The holder's stop hands back the message behind its handler onto a frozen
// link: the publish lands in the socket, the stop's flush gets no answer, and the stop returns with
// the hold renewing (finalizeHandBacks). Then a stalled write holds nats.go's lock, so the
// finalizer's next flush waits for that lock with no bound of its own, and the holder's renewals are
// refused. The holder's connection must be closed before the standby can create the durable. A
// finalizer that read the lost hold only between its flushes, and severed from there, waited inside
// that flush, and left the connection open. The row checks the connection and not the order of
// events: what the frozen relay had already read of the socket before the sever is beyond the
// client, and reaches the server once the relay thaws.
//
// The stop's deadline and its flush's bound are on the holder's stop clock (heldDurable.clock), a
// fake the row moves once the hand-back has landed (driveStop). The hold runs on a fake of its own
// (heldDurable.holdClock), and its row is kept live on the database's clock until the row lapses it
// (keepHoldRowLive): the row moves the hold's clock to the loss deadline and past the wait the loss
// watcher arms for a flush's answer there, until the connection is closed (loseHoldUntilSevered),
// and only then lapses the hold's row (lapseHoldRow) and waits for the standby's creation of the
// durable.
func TestALostHoldSeversAnUnconfirmedHandBackWhileTheFinalizersFlushWaits(t *testing.T) {
	testnats.RequireSocketQueues(t)
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	link := testnats.StartHandshakeHold(t, natsURL)
	t.Cleanup(link.Restore)
	publisher := intakeTestClient(t)
	recorded := watchDurableEvents(t, natsURL)

	const inFlight = 100
	stopClock := clocktest.New(time.Now())
	holdClock := clocktest.New(time.Now())
	holder := startIntakeProcessWith(t, pool, link.URL(), func(number int) bool { return number == inFlight }, func(p *intakeProcess) {
		ackWaitPastTheRun(p.intake)
		p.intake.clock = stopClock
		p.intake.holdClock = holdClock
	})
	awaitBoundDurable(t, publisher)
	keepHoldRowLive(t, pool, holder)
	startIntakeProcessWith(t, pool, natsURL, nil, processAckWaitPastTheRun)
	holderConn := holder.client.Connection()
	publishMergedPullRequest(t, publisher, inFlight)
	waitFor(t, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder has not started on the first message: " + consumerState(publisher)
	})
	publishMergedPullRequest(t, publisher, inFlight)
	// The second message has to be in the holder's hands, queued behind the first, before the stop
	// begins, or the stop has nothing to hand back onto the frozen link; the durable's count of
	// messages outstanding does not say that (stopWithAHeldHandBack).
	delivery := subscribedDelivery(holder.intake)
	waitFor(t, func() bool { return queuedBehindKeys(delivery) == 1 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind the holder's handler, want the second message: %s", queuedBehindKeys(delivery), consumerState(publisher))
	})

	// The stop's hand-backs go out only once its drain has answered; the stop's calls wait for NATS
	// with no bound of the row's, and the hook puts the wait back as the hand-back's publish begins
	// (stopWithAHeldHandBack).
	tuning := tuningFor(t)
	wait := tuning.callWait.Swap(math.MaxInt64)
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
	delivery.beforeHandBackPublish.Store(&hook)
	from := recorded.count()
	stopped := holder.stopping()
	waitFor(t, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	holder.release()
	testwait.Receive(t, "the stop's hand-back to reach its publish (control)", frozen)
	testwait.Receive(t, "the hand-back's publish to land in the frozen link's socket (control)", delivery.handBacksLanded())
	driveStop(t, stopClock, stopped)
	if !loggedMessage(logs(), intakeHoldFinalizing) || !holdLive(t, pool) {
		t.Fatalf("control: the stop released its hold although no flush could confirm its hand-back; logged %q", loggedMessages(logs()))
	}
	thaw := link.HoldStalledWrite(t, holderConn)
	holder.leases.cut.Store(true)
	loseHoldUntilSevered(t, holdClock, holder,
		"the lost hold's sever to close the connection that carries the unconfirmed hand-back while the finalizer's flush waits for nats.go's lock (a finalizer that reads the loss only between its flushes leaves it open)",
		connectionClosed(holderConn))
	lapseHoldRow(t, pool)
	waitFor(t, func() bool { return slices.Contains(recorded.since(from), durableCreated) }, func() string {
		return fmt.Sprintf("the standby has not taken over the lapsed hold and created the durable: %q; %s", recorded.since(from), consumerState(publisher))
	})
	thaw()
}
