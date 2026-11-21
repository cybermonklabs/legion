package delivery

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// lossWatchers counts the goroutines inside a delivery's loss watcher (heldDurable.watchLoss).
func lossWatchers() int {
	stacks := make([]byte, 1<<20)
	for {
		n := runtime.Stack(stacks, true)
		if n < len(stacks) {
			return bytes.Count(stacks[:n], []byte("delivery.(*heldDurable).watchLoss("))
		}
		stacks = make([]byte, 2*len(stacks))
	}
}

// Taking the durable again under one live hold leaves no loss watcher running per attempt.
//
// The intake subscribes again under the hold it holds after a create that fails, and each subscribe
// starts a loss watcher for its delivery (heldDurable.watchLoss). A watcher that ended only with the
// hold kept one goroutine and one stopped delivery per take-over for as long as the hold lasted.
// Two subtests, each counting the goroutines inside a loss watcher from before the holder starts:
//   - every create refused by the grant (bus.ErrPublishDenied, which the take-over does not try
//     again): the holder takes the durable over every durableBindInterval and stops each delivery
//     with nothing handed back. After six creates under one hold, which still names the holder, at
//     most one watcher is left running: the latest attempt's, until its stop.
//   - a create that succeeds (the control): the bound delivery's watcher runs, one, which shows the
//     count sees a watcher.
//
// The negative control is the watcher that waits for the hold's end alone: the refused subtest fails
// with one watcher per take-over.
func TestTakingTheDurableAgainUnderOneHoldLeavesNoLossWatcherPerAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse bool
	}{
		{name: "every create refused by the grant", refuse: true},
		{name: "a create that succeeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			seedDeliverySettings(t, ctx, pool)
			var creates atomic.Int32
			create := createDurable
			createDurable = func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
				creates.Add(1)
				if tc.refuse {
					return nil, fmt.Errorf("the test's grant refuses the create: %w", bus.ErrPublishDenied)
				}
				return create(js, config, opts...)
			}
			// Registered before the holder starts, so it runs after the holder has stopped.
			t.Cleanup(func() { createDurable = create })
			before := lossWatchers()
			// The stream the durable is on; the holder creates no stream of its own.
			publisher := intakeTestClient(t)
			holder := startIntakeProcess(t, pool, testnats.URL(t), nil)
			if !tc.refuse {
				awaitBoundDurable(t, publisher)
				waitFor(t, 20*time.Second, func() bool { return lossWatchers()-before == 1 }, func() string {
					return fmt.Sprintf("control: %d loss watchers run for the one bound delivery, want 1", lossWatchers()-before)
				})
				return
			}
			waitFor(t, 30*time.Second, func() bool { return creates.Load() >= 1 }, func() string {
				return "control: the holder never tried to create the durable"
			})
			hold := subscribedDelivery(holder.intake).hold
			const attempts = 6
			waitFor(t, 60*time.Second, func() bool { return creates.Load() >= attempts }, func() string {
				return fmt.Sprintf("control: the holder tried %d creates, want %d", creates.Load(), attempts)
			})
			if subscribedDelivery(holder.intake).hold != hold || !holdLive(t, pool) {
				t.Fatal("control: the holder's take-overs did not all run under the one hold it took first, still live")
			}
			waitFor(t, 10*time.Second, func() bool { return lossWatchers()-before <= 1 }, func() string {
				return fmt.Sprintf("%d loss watchers are still running after %d take-overs under one live hold, want at most 1: each subscribe left one waiting for the hold's end",
					lossWatchers()-before, creates.Load())
			})
		})
	}
}

// unconfirmedHandBack is a holder's stop whose hand-back went into the holder's socket's own kernel
// send queue on a stalled link, unconfirmed (stopWithAnUnconfirmedHandBack): stopped closes once the
// holder's Run has returned; from is the first durable event of the stop, and sequences the stream
// sequences of the row's two messages.
type unconfirmedHandBack struct {
	holder    *intakeProcess
	pool      *store.Pool
	relay     *testnats.StallRelay
	recorded  *durableEvents
	from      int
	publisher *bus.Client
	sequences []uint64
	stopped   <-chan struct{}
	logs      func() []map[string]any
}

// stopWithAnUnconfirmedHandBack starts a holder whose NATS client, with options, goes through a
// relay that can stall it (testnats.StallRelay), and a standby on a link of its own. The holder's
// handler holds one pull request with a second message of it waiting behind. The row stops the holder
// and, from inside the stop's hand-back of the waiting message (beforeHandBackPublish), stalls the
// relay and has the holder's connection write a 64 KiB filler, until the relay's reader has stopped:
// the relay's window is then full, so the hand-back's -NAK is written into the holder's socket's own
// kernel send queue, where nothing in user space reads it, and no flush can confirm it. It returns
// once the hand-back's publish has returned, with the stop under way.
func stopWithAnUnconfirmedHandBack(t *testing.T, options ...bus.ConnectOption) unconfirmedHandBack {
	t.Helper()
	var row unconfirmedHandBack
	row.logs = captureLogs(t)
	shrinkIntakeFlowControl(t)
	budget := intakeStopBudget
	intakeStopBudget = stopBudget
	t.Cleanup(func() { intakeStopBudget = budget })
	pool, ctx := deliveryTestPool(t)
	row.pool = pool
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	row.relay = testnats.StartStallRelay(t, natsURL)
	row.publisher = intakeTestClient(t)
	row.recorded = watchDurableEvents(t, natsURL)

	const inFlight = 100
	holder := startIntakeProcess(t, pool, row.relay.URL(), func(number int) bool { return number == inFlight }, options...)
	row.holder = holder
	awaitBoundDurable(t, row.publisher)
	startIntakeProcess(t, pool, natsURL, nil)
	holderConn := holder.client.Connection()
	row.sequences = append(row.sequences, publishMergedPullRequest(t, row.publisher, inFlight))
	waitFor(t, 20*time.Second, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(row.publisher)
	})
	row.sequences = append(row.sequences, publishMergedPullRequest(t, row.publisher, inFlight))
	awaitAckPending(t, row.publisher, 2)

	// The stop's hand-backs go out only once its drain has answered; the row gives the stop's calls
	// a 30 s guard, and puts the wait back as the hand-back's publish begins (stopWithAHeldHandBack).
	wait := callNATSWait.Swap(int64(30 * time.Second))
	restoreWait := sync.OnceFunc(func() { callNATSWait.Store(wait) })
	t.Cleanup(restoreWait)
	var stalling sync.Once
	filled := make(chan error, 1)
	letGo := make(chan struct{})
	release := sync.OnceFunc(func() { close(letGo) })
	t.Cleanup(release)
	hook := func() {
		stalling.Do(func() {
			restoreWait()
			row.relay.Stall()
			filled <- writeFiller(holderConn)
			<-letGo
		})
	}
	delivery := subscribedDelivery(holder.intake)
	delivery.beforeHandBackPublish.Store(&hook)
	row.from = row.recorded.count()
	stopped := make(chan struct{})
	row.stopped = stopped
	go func() {
		holder.stop()
		close(stopped)
	}()
	waitFor(t, 20*time.Second, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(row.publisher)
	})
	holder.release()
	select {
	case err := <-filled:
		if err != nil {
			t.Fatalf("control: the holder's connection did not write the filler into its socket: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("control: the stop's hand-back never reached its publish: " + consumerState(row.publisher))
	}
	row.relay.AwaitParked(t)
	release()
	select {
	case <-delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("control: the hand-back's publish did not return within 30 s on the stalled link")
	}
	return row
}

// writeFiller publishes a 64 KiB filler on conn and waits until nats.go has written it into the
// socket, so a stalled relay's window fills with it.
func writeFiller(conn *natsgo.Conn) error {
	if err := conn.Publish("testdelivery.stall.filler", make([]byte, 64<<10)); err != nil {
		return err
	}
	return writtenIntoTheSocket(conn)
}

// writtenIntoTheSocket waits until nats.go has written everything conn had buffered into its socket.
func writtenIntoTheSocket(conn *natsgo.Conn) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		buffered, err := conn.Buffered()
		if err != nil {
			return err
		}
		if buffered == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("nats.go still held what it had buffered 30 s on")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireHandBackStillOnTheHolder fails the test when the relay read the hand-back's -NAK or holds
// it in its socket's receive queue: the bytes would have left the holder's socket, which no reset can
// withdraw, and the row would not measure the holder.
func requireHandBackStillOnTheHolder(t *testing.T, relay *testnats.StallRelay) {
	t.Helper()
	if bytes.Contains(relay.ReadByRelay(t), []byte("-NAK")) {
		t.Fatal("control: the hand-back's -NAK reached the relay before its window filled, so it left the holder's socket")
	}
}

// moveAndRequireNoLateHandBack waits for the standby to create the durable, lets the stalled link
// move, waits until the server has handled everything the stalled socket still sent, and fails the
// test when a -NAK reached the server after the creation, or when the standby's consumer delivered
// one of the row's messages more than once (requireNoHandBackAfterCreation).
func moveAndRequireNoLateHandBack(t *testing.T, row unconfirmedHandBack) {
	t.Helper()
	waitFor(t, 60*time.Second, func() bool { return slices.Contains(row.recorded.since(row.from), durableCreated) }, func() string {
		return fmt.Sprintf("the standby did not take over the released hold and create the durable: %q; %s", row.recorded.since(row.from), consumerState(row.publisher))
	})
	row.relay.Move()
	row.relay.AwaitEnded(t)
	row.recorded.barrier(t)
	t.Logf("events after the stop: %q", row.recorded.since(row.from))
	requireNoHandBackAfterCreation(t, row.recorded, row.from, row.publisher, row.sequences)
}

// A hand-back left unconfirmed in the holder's socket at shutdown never reaches the durable a
// standby recreates.
//
// The holder's stop hands back the message behind its handler onto a stalled link, and the -NAK's
// bytes go into the holder's socket's own kernel send queue (stopWithAnUnconfirmedHandBack): no relay
// or proxy has read them. The stop's flush gets no answer, so the stop returns with the hold renewing
// (finalizeHandBacks). StartIntakes' stop then closes the client, and the finalizer, reading the
// closed connection as settled, releases the hold. A normal close leaves the -NAK in the kernel,
// which sends it once the link moves: after the standby has recreated the durable. A socket that
// carries an unconfirmed hand-back is reset as it closes (bus.Client.MarkUnconfirmed), which discards
// it. The row requires that StartIntakes' stop returned with the hold released, and once the standby
// has created the durable and the link has moved, that the server has handled everything the stalled
// socket still sent with no -NAK after the creation. The negative control is the close before the
// mark: the -NAK arrives after the creation.
func TestAHandBackUnconfirmedInTheSocketAtShutdownNeverReachesTheStandbysDurable(t *testing.T) {
	row := stopWithAnUnconfirmedHandBack(t)
	select {
	case <-row.stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's Run did not return within 30 s of its stop's deadline")
	}
	if !loggedMessage(row.logs(), intakeHoldFinalizing) || !holdLive(t, row.pool) {
		t.Fatalf("control: the stop released its hold although no flush could confirm its hand-back; logged %q", loggedMessages(row.logs()))
	}
	requireHandBackStillOnTheHolder(t, row.relay)
	var runs sync.WaitGroup
	closed := make(chan struct{})
	go func() {
		stopIntakes(&runs, &row.holder.intake.heldDurable)
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("StartIntakes' stop did not return within 30 s")
	}
	if holdLive(t, row.pool) {
		t.Fatal("control: StartIntakes' stop returned with the hold still live")
	}
	moveAndRequireNoLateHandBack(t, row)
}

// A hand-back left in a socket nats.go gives up as stale never reaches the durable a standby
// recreates.
//
// The holder's client declares a connection whose pings go unanswered stale within seconds
// (bus.WithSilentCutDetection, as envoy-dispatch's clients do). Its stop hands back the message behind
// its handler onto a stalled link, with the -NAK's bytes in the socket's own kernel send queue
// (stopWithAnUnconfirmedHandBack). nats.go then declares that connection stale, closes its socket with
// no failed write, and reconnects through the relay on a new socket, where the finalizer's flush
// answers and the hold is released. A normal close of the old socket leaves the -NAK in its kernel,
// which sends it once the link moves, after the standby has recreated the durable; a flush on the new
// socket says nothing of it. A socket that carries an unconfirmed hand-back is reset as it closes
// (bus.Client.MarkUnconfirmed). The row requires a second connection through the relay and the hold
// released, and, once the standby has created the durable and the old link has moved, no -NAK after
// the creation. The negative control is the close before the mark: the -NAK arrives after the
// creation.
func TestAHandBackOnAConnectionGivenUpAsStaleNeverReachesTheStandbysDurable(t *testing.T) {
	row := stopWithAnUnconfirmedHandBack(t, bus.WithSilentCutDetection())
	select {
	case <-row.stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's Run did not return within 30 s of its stop's deadline")
	}
	requireHandBackStillOnTheHolder(t, row.relay)
	waitFor(t, 30*time.Second, func() bool { return row.relay.Accepted() >= 2 && !holdLive(t, row.pool) }, func() string {
		return fmt.Sprintf("control: the holder did not reconnect on a new socket and release its hold: %d connection(s) through the relay, hold live=%t; logged %q",
			row.relay.Accepted(), holdLive(t, row.pool), loggedMessages(row.logs()))
	})
	moveAndRequireNoLateHandBack(t, row)
}

// A hand-back an earlier delivery under the same hold left unconfirmed keeps the hold at the stop,
// and reaches the server before the standby recreates the durable.
//
// The intake takes the durable again under the hold it holds after a create or a token write that
// fails, and after a check that finds the durable elsewhere, so a process can stop while a delivery
// before the latest still has a hand-back in the socket's kernel send queue that no flush has
// confirmed. The row's holder takes the hold and the durable by hand, through a relay it can freeze
// (testnats.StallRelay.Freeze), and its first delivery is handed one message, which its handler
// leaves unanswered. The row freezes the link, fills the relay's window, and has that delivery hand
// the message back, so the -NAK goes into the socket's own kernel send queue, where the relay has not
// read it. It stops that delivery, subscribes a second one under the same hold, which hands nothing
// back, and stops the holder under a context that has already ended. The row requires the hold still
// live once that stop has returned, then lets the link move, and requires that the -NAK reached the
// server before the standby created the durable and that the standby's consumer delivered the
// message once (requireHandBackBeforeCreation). A stop that reads only the latest delivery released
// the hold at once, leaving the -NAK to go out whenever the link moved, after a standby could have
// recreated the durable; that is the negative control, which fails the hold's check.
func TestAHandBackAnEarlierDeliveryLeftUnconfirmedKeepsTheHoldAtTheStop(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	relay := testnats.StartStallRelay(t, natsURL)
	publisher := intakeTestClient(t)
	recorded := watchDurableEvents(t, natsURL)
	client, err := bus.Connect([]string{relay.URL()}, bus.WithName(intakeConnectionName))
	if err != nil {
		t.Fatalf("connect the holder through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	handed := make(chan *natsgo.Msg, 1)
	durable := newHeldDurable(client, deliveryTestLeases(t, pool), deliveryConsumerConfig, intakeKey,
		func(_ *intakeDelivery, message intakeMessage) {
			select {
			case handed <- message.msg:
			default:
			}
		})
	if !durable.takeHold(ctx) {
		t.Fatal("control: the holder did not take the delivery durable's hold")
	}
	first := durable.config()
	if _, err := durable.subscribe(first.DeliverSubject); err != nil {
		t.Fatalf("subscribe the holder's first delivery at %s: %v", first.DeliverSubject, err)
	}
	if _, err := publisher.JS().AddConsumer(bus.Stream, &first); err != nil {
		t.Fatalf("create the delivery durable: %v", err)
	}
	token := intakeHoldToken{subject: first.DeliverSubject}
	if err := durable.hold.SetToken(ctx, token.String()); err != nil {
		t.Fatalf("record the holder's deliver subject: %v", err)
	}
	durable.recorded, durable.subject = token, first.DeliverSubject
	earlier := delivering(&durable)
	// Run in reverse: the link moves and the holder stops before its finalizer is waited for, which
	// then confirms its hand-back and releases the hold.
	t.Cleanup(durable.waitFinalizers)
	t.Cleanup(func() {
		relay.Move()
		durable.stop(cancelled())
	})

	startIntakeProcess(t, pool, natsURL, nil)
	from := recorded.count()
	sequences := []uint64{publishMergedPullRequest(t, publisher, 100)}
	var msg *natsgo.Msg
	select {
	case msg = <-handed:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the holder's first delivery was not handed the message: " + consumerState(publisher))
	}

	relay.Freeze()
	holderConn := client.Connection()
	if err := writeFiller(holderConn); err != nil {
		t.Fatalf("control: the holder's connection did not write the filler into its socket: %v", err)
	}
	relay.AwaitParked(t)
	earlier.handBack(msg)
	select {
	case <-earlier.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("control: the first delivery's hand-back did not return within 30 s on the frozen link")
	}
	if !earlier.handedBack() {
		t.Fatalf("control: the first delivery withheld its hand-back; logged %q", loggedMessages(logs()))
	}
	if err := writtenIntoTheSocket(holderConn); err != nil {
		t.Fatalf("control: nats.go did not write the hand-back into the socket: %v", err)
	}
	requireHandBackStillOnTheHolder(t, relay)

	durable.StopDelivering(cancelled())
	second := durable.config()
	if _, err := durable.subscribe(second.DeliverSubject); err != nil {
		t.Fatalf("subscribe the holder's second delivery at %s: %v", second.DeliverSubject, err)
	}
	if latest := delivering(&durable); latest == earlier || latest.hold != earlier.hold || latest.handedBack() {
		t.Fatal("control: the holder's second delivery is not a new one under the same hold that handed nothing back")
	}
	stopped := make(chan struct{})
	go func() {
		durable.stop(cancelled())
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's stop did not return within 30 s")
	}
	if !holdLive(t, pool) {
		t.Fatalf("the stop released its hold while the earlier delivery's hand-back was unconfirmed in the socket's send queue, so a standby can recreate the durable before the link moves and sends it; events %q", recorded.since(from))
	}
	relay.Move()
	requireHandBackBeforeCreation(t, recorded, from, publisher, sequences)
}

// delivering is the latest delivery durable subscribed (heldDurable.delivering), read under its lock.
func delivering(durable *heldDurable) *intakeDelivery {
	durable.mu.Lock()
	defer durable.mu.Unlock()
	return durable.delivering
}
