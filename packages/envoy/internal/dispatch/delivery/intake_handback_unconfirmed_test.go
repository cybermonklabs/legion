package delivery

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// lossWatchers is how many of durable's loss watchers still run (heldDurable.lossWatchers, raised as
// subscribe starts a watcher and lowered as the watcher returns): that intake's alone, so a row that
// counts them runs beside the others.
func lossWatchers(durable *heldDurable) int {
	return int(durable.lossWatchers.Load())
}

// Taking the durable again under one live hold leaves no loss watcher running per attempt.
//
// The intake subscribes again under the hold it holds after a create that fails, and each subscribe
// starts a loss watcher for its delivery (heldDurable.watchLoss). A watcher that ended only with the
// hold kept one goroutine and one stopped delivery per take-over for as long as the hold lasted.
// Two subtests, each counting the holder's own loss watchers still running (lossWatchers):
//   - every create refused by the grant (bus.ErrPublishDenied, which the take-over does not try
//     again): the holder takes the durable over at each of its tries and stops each delivery with
//     nothing handed back. Its tries run on a fake hold clock the row moves one durableBindInterval
//     at a time (holdTries), each try ending before the next move. After six creates under one hold,
//     which still names the holder, at most one watcher is left running: the latest attempt's, until
//     its stop.
//   - a create that succeeds (the control): the bound delivery's watcher runs, one, which shows the
//     count sees a watcher.
//
// The negative control is the watcher that waits for the hold's end alone: the refused subtest fails
// with one watcher per take-over.
func TestTakingTheDurableAgainUnderOneHoldLeavesNoLossWatcherPerAttempt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		refuse bool
	}{
		{name: "every create refused by the grant", refuse: true},
		{name: "a create that succeeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, ctx := deliveryTestPool(t)
			seedDeliverySettings(t, ctx, pool)
			var creates atomic.Int32
			tuning := tuningFor(t)
			create := tuning.createDurable
			tuning.createDurable = func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
				creates.Add(1)
				if tc.refuse {
					return nil, fmt.Errorf("the test's grant refuses the create: %w", bus.ErrPublishDenied)
				}
				return create(js, config, opts...)
			}
			// The stream the durable is on; the holder creates no stream of its own.
			publisher := intakeTestClient(t)
			tries := newHoldTries()
			holder := startIntakeProcessWith(t, pool, testnats.URL(t), nil, tries.before)
			watchers := func() int { return lossWatchers(&holder.intake.heldDurable) }
			if !tc.refuse {
				awaitBoundDurable(t, publisher)
				waitFor(t, func() bool { return watchers() == 1 }, func() string {
					return fmt.Sprintf("control: %d loss watchers run for the one bound delivery, want 1", watchers())
				})
				return
			}
			if tries.first(t, "the holder's try for the durable as it starts") {
				t.Fatal("control: the holder delivers from the durable although the grant refused its create")
			}
			if creates.Load() < 1 {
				t.Fatal("control: the holder's try at its start did not try to create the durable")
			}
			hold := subscribedDelivery(holder.intake).hold
			const attempts = 6
			testwait.Until(t, fmt.Sprintf("the holder to try %d creates, one try at a time", attempts), func() (string, bool) {
				if creates.Load() >= attempts {
					return "", true
				}
				if tries.next(t, "the holder's next try for the durable") {
					t.Fatal("control: the holder delivers from the durable although the grant refused its create")
				}
				return fmt.Sprintf("the holder tried %d creates, want %d", creates.Load(), attempts), false
			})
			if subscribedDelivery(holder.intake).hold != hold || !holdLive(t, pool, holder.intake.holder) {
				t.Fatal("control: the holder's take-overs did not all run under the one hold it took first, still live")
			}
			waitFor(t, func() bool { return watchers() <= 1 }, func() string {
				return fmt.Sprintf("%d loss watchers are still running after %d take-overs under one live hold, want at most 1: each subscribe left one waiting for the hold's end",
					watchers(), creates.Load())
			})
		})
	}
}

// unconfirmedHandBack is a holder's stop whose hand-back went into the holder's socket's own kernel
// send queue on a stalled link, unconfirmed (stopWithAnUnconfirmedHandBack): stopped closes once the
// holder's Run has returned; from is the first durable event of the stop, and sequences the stream
// sequences of the row's two messages. stopClock and holdClock are the holder's two clocks
// (heldDurable.clock, heldDurable.holdClock), fakes only the row moves (passStop, finalize), and
// standby the standby's tries for the hold, on a fake of its own.
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
	stopClock *clocktest.Fake
	holdClock *clocktest.Fake
	standby   *holdTries
}

// stopWithAnUnconfirmedHandBack starts a holder whose NATS client, with options, goes through a
// relay that can stall it (testnats.StallRelay), and a standby on a link of its own. The holder's
// handler holds one pull request with a second message of it waiting behind. The row stops the holder
// and, from inside the stop's hand-back of the waiting message (beforeHandBackPublish), stalls the
// relay and has the holder's connection write a 64 KiB filler, until the relay's reader has stopped:
// the relay's window is then full, so the hand-back's -NAK is written into the holder's socket's own
// kernel send queue, where nothing in user space reads it, and no flush can confirm it. It returns
// once the hand-back's publish has returned, with the stop under way.
//
// The holder's stop deadline and its flushes' bounds and waits are on its two fakes, which stand
// where the holder started until the row moves them (passStop, finalize), and the standby tries for
// the hold only where the row moves its own (holdTries). Both intakes' ack wait is past the run
// (ackWaitPastTheRun): the hand-back is what must bring the message to the standby, so the server's
// own redelivery at the ack wait never comes inside the run.
func stopWithAnUnconfirmedHandBack(t *testing.T, options ...bus.ConnectOption) unconfirmedHandBack {
	t.Helper()
	var row unconfirmedHandBack
	row.logs = captureIntakeLogs(t)
	ackWaitPastTheRun(t)
	tuning := tuningFor(t)
	pool, ctx := deliveryTestPool(t)
	row.pool = pool
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	row.relay = testnats.StartStallRelay(t, natsURL)
	row.publisher = intakeTestClient(t)
	row.recorded = watchDurableEvents(t, natsURL)

	const inFlight = 100
	row.stopClock, row.holdClock = fakeIntakeClock(), fakeIntakeClock()
	holder := startIntakeProcessWith(t, pool, row.relay.URL(), func(number int) bool { return number == inFlight }, func(p *intakeProcess) {
		p.intake.clock, p.intake.holdClock = row.stopClock, row.holdClock
	}, options...)
	row.holder = holder
	awaitBoundDurable(t, row.publisher)
	row.standby = newHoldTries()
	startIntakeProcessWith(t, pool, natsURL, nil, row.standby.before)
	if row.standby.first(t, "the standby's try for the hold as it starts") {
		t.Fatal("control: the standby took the durable over at its start, while the holder held it")
	}
	holderConn := holder.client.Connection()
	row.sequences = append(row.sequences, publishMergedPullRequest(t, row.publisher, inFlight))
	waitFor(t, func() bool { return holder.fetches(inFlight) == 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(row.publisher)
	})
	row.sequences = append(row.sequences, publishMergedPullRequest(t, row.publisher, inFlight))
	waitFor(t, func() bool { return queuedBehindKeys(subscribedDelivery(holder.intake)) == 1 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind the holder's handler, want the second message: %s", queuedBehindKeys(subscribedDelivery(holder.intake)), consumerState(row.publisher))
	})

	// The stop's hand-backs go out only once its drain has answered; the stop's calls wait for NATS
	// with no bound of the row's, and the hook puts the wait back as the hand-back's publish begins
	// (stopWithAHeldHandBack).
	wait := tuning.callWait.Swap(math.MaxInt64)
	restoreWait := sync.OnceFunc(func() { tuning.callWait.Store(wait) })
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
	waitFor(t, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(row.publisher)
	})
	holder.release()
	if err := testwait.Receive(t, "the stop's hand-back to reach its publish and the filler to be written (control)", filled); err != nil {
		t.Fatalf("control: the holder's connection did not write the filler into its socket: %v", err)
	}
	row.relay.AwaitParked(t)
	release()
	testwait.Receive(t, "the hand-back's publish to return on the stalled link (control)", delivery.handBacksLanded())
	return row
}

// passStop moves the holder's stop through what it waits for on its clocks until its Run returns:
// the stop's deadline, the holder's stopBudget on its stopClock from the moment its Run's context
// ended, which the clock has not moved from (stopContext), once the stop has armed it; then its
// holdClock to each moment the stop and its loss watcher arm a flush's bound or wait at, one
// intakeFlushWait on (flushHandBacks, watchLoss), until Run has returned.
func (row unconfirmedHandBack) passStop(t *testing.T) {
	t.Helper()
	budget := tuningFor(t).stopBudget
	row.stopClock.AwaitArmedAt(t, "the holder's stop deadline, armed as its Run's context ended", row.stopClock.Now().Add(budget))
	row.stopClock.Advance(budget)
	moveTicks(t, row.holdClock, intakeFlushWait, func() bool { return closedChannel(row.stopped) }, func() string {
		return fmt.Sprintf("the holder's Run has not returned past its stop's deadline; its hold clock stands at %s, with %v armed", row.holdClock.Now(), row.holdClock.Armed())
	})
}

// finalize moves the holder's holdClock to each tick its hand-back's finalizer waits for, one
// intakeFlushWait apart (finalizeHandBacks), and to each its loss watcher's flushes wait for, until
// done reports true, through the shared wait, whose give-up prints what.
func (row unconfirmedHandBack) finalize(t *testing.T, done func() bool, what func() string) {
	t.Helper()
	moveTicks(t, row.holdClock, intakeFlushWait, done, what)
}

// closedChannel reports whether ch has closed.
func closedChannel(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// holdReleased reports whether the delivery durable's hold has no row at all: its holder released
// it, which deletes the row (duty.Hold.Release), where a lapse keeps it with its token.
func holdReleased(t *testing.T, pool *store.Pool) bool {
	t.Helper()
	var gone bool
	if err := pool.QueryRow(t.Context(), "select not exists (select 1 from duty_holds where name = $1)",
		intakeHoldName(bus.DispatchDeliveryDurable)).Scan(&gone); err != nil {
		t.Fatalf("read the delivery durable's hold: %v", err)
	}
	return gone
}

// writeFiller publishes a 64 KiB filler on conn and waits until nats.go has written it into the
// socket, so a stalled relay's window fills with it.
func writeFiller(conn *natsgo.Conn) error {
	if err := conn.Publish("testdelivery.stall.filler", make([]byte, 64<<10)); err != nil {
		return err
	}
	return writtenIntoTheSocket(conn)
}

// writtenIntoTheSocket waits until nats.go has written everything conn had buffered into its
// socket. It runs in the stop's hand-back hook, where no t is at hand, so it waits through the
// shared wait's form for a helper (testwait.InHelper), under the bound every shared wait has.
func writtenIntoTheSocket(conn *natsgo.Conn) error {
	var readErr error
	waited := testwait.InHelper("writtenIntoTheSocket", "nats.go to write what it had buffered into its socket", func() bool {
		buffered, err := conn.Buffered()
		if err != nil {
			readErr = err
			return true
		}
		return buffered == 0
	})
	if readErr != nil {
		return readErr
	}
	return waited
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

// moveAndRequireNoLateHandBack has the standby try for the released hold until it takes the durable
// over (holdTries.untilDelivering) and waits for it to create the durable, lets the stalled link
// move, waits until the server has handled everything the stalled socket still sent, and fails the
// test when a -NAK reached the server after the creation, or when the standby's consumer delivered
// one of the row's messages more than once (requireNoHandBackAfterCreation).
func moveAndRequireNoLateHandBack(t *testing.T, row unconfirmedHandBack) {
	t.Helper()
	row.standby.untilDelivering(t, "the standby to take over the released hold")
	waitFor(t, func() bool { return slices.Contains(row.recorded.since(row.from), durableCreated) }, func() string {
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
//
// The stop's deadline and the flushes' bounds and ticks pass where the row moves the holder's fakes
// (passStop, finalize). The holder's hold is not renewed meanwhile, since the row moves its hold
// clock less than intakeHoldRenew, so its row stays live on the database's clock for intakeHoldTTL
// from the take, far past the row's reads of it.
func TestAHandBackUnconfirmedInTheSocketAtShutdownNeverReachesTheStandbysDurable(t *testing.T) {
	t.Parallel()
	row := stopWithAnUnconfirmedHandBack(t)
	row.passStop(t)
	if !loggedMessage(row.logs(), intakeHoldFinalizing) || !holdLive(t, row.pool, row.holder.intake.holder) {
		t.Fatalf("control: the stop released its hold although no flush could confirm its hand-back; logged %q", loggedMessages(row.logs()))
	}
	requireHandBackStillOnTheHolder(t, row.relay)
	var runs sync.WaitGroup
	closed := make(chan struct{})
	go func() {
		stopIntakes(&runs, &row.holder.intake.heldDurable)
		close(closed)
	}()
	row.finalize(t, func() bool { return closedChannel(closed) }, func() string {
		return fmt.Sprintf("StartIntakes' stop has not returned: the finalizer did not release the hold once the client closed; logged %q", loggedMessages(row.logs()))
	})
	if !holdReleased(t, row.pool) {
		t.Fatal("control: StartIntakes' stop returned with the hold not released")
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
//
// The stop's deadline and the finalizer's ticks pass where the row moves the holder's fakes
// (passStop, finalize). What the row waits out is internal/bus's silent-cut detection: nats.go's
// pings, a second apart with two outstanding, which no option of the bus sets.
func TestAHandBackOnAConnectionGivenUpAsStaleNeverReachesTheStandbysDurable(t *testing.T) {
	t.Parallel()
	row := stopWithAnUnconfirmedHandBack(t, bus.WithSilentCutDetection())
	row.passStop(t)
	requireHandBackStillOnTheHolder(t, row.relay)
	row.finalize(t, func() bool { return row.relay.Accepted() >= 2 && holdReleased(t, row.pool) }, func() string {
		return fmt.Sprintf("control: the holder has not reconnected on a new socket and released its hold: %d connection(s) through the relay, hold released=%t; logged %q",
			row.relay.Accepted(), holdReleased(t, row.pool), loggedMessages(row.logs()))
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
	t.Parallel()
	logs := captureIntakeLogs(t)
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
	durable := newHeldDurable(tuningFor(t), client, deliveryTestLeases(t, pool), deliveryConsumerConfig, intakeKey,
		func(_ *intakeDelivery, message intakeMessage) {
			select {
			case handed <- message.msg:
			default:
			}
		})
	if !durable.takeHold(ctx) {
		t.Fatal("control: the holder did not take the delivery durable's hold")
	}
	first := durable.config(durable.tuning)
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
	second := durable.config(durable.tuning)
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
	if !holdLive(t, pool, durable.holder) {
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

// A hand-back an earlier delivery has admitted keeps the hold at the stop before its publish begins.
//
// The stop's release waits for the latest delivery's own hand-backs, and for an earlier delivery
// under the same hold it reads only the connection's marks (bus.Client.CarriesUnconfirmed,
// ConfirmMarked). A subscription callback the earlier delivery's unsubscribe went on without can be
// admitted for a hand-back just before that delivery's seal (admitHandBack). When the connection was
// marked only after the admission, outside the delivery's lock, a callback descheduled between the
// two let a later stop read no mark and release the hold, and its -NAK was written after the release.
//
// The row's holder takes the hold and the durable by hand, with a callback that holds the one message
// in its keying. The first delivery's stop begins and waits for its drain, which the held callback
// keeps from ending. The callback is then let go: its message's key has stopped, so it hands the
// message back, and the row holds that hand-back once it is admitted (afterHandBackAdmitted), before
// its publish. The row ends the stop's context, so the first delivery unsubscribes and seals with the
// hand-back counted, subscribes a second delivery under the same hold, which hands nothing back, and
// stops the holder under a context that has already ended. It requires the hold still live once that
// stop has returned. It then lets the hand-back go on, and requires that the -NAK reached the server
// before the standby created the durable and that the standby's consumer delivered the message once
// (requireHandBackBeforeCreation). The negative control is the mark made after the admission, with
// this hook between the two: the stop releases the hold, and the row fails at the hold's check.
//
// The stop's drain and its connected read wait for NATS with no bound of the row's (callWait), so a
// loaded machine cannot have the stop withhold the hand-back. The holder's finalizer flushes once
// each intakeFlushWait tick of its hold clock, a fake the row moves (finalizeHandBacks), and the
// standby tries for the released hold only where the row moves its own (holdTries).
func TestAnEarlierDeliverysHandBackAdmittedBeforeItsPublishKeepsTheHoldAtTheStop(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	client := intakeTestClient(t)
	recorded := watchDurableEvents(t, natsURL)

	const number = 100
	keying := make(chan struct{})
	var entered sync.Once
	letKeyingGo := make(chan struct{})
	releaseKeying := sync.OnceFunc(func() { close(letKeyingGo) })
	t.Cleanup(releaseKeying)
	tuning := tuningFor(t)
	durable := newHeldDurable(tuning, client, deliveryTestLeases(t, pool), deliveryConsumerConfig,
		func(msg *natsgo.Msg) (string, *contracts.Envelope, bool) {
			entered.Do(func() { close(keying) })
			<-letKeyingGo
			return intakeKey(msg)
		}, func(*intakeDelivery, intakeMessage) {})
	holdClock := fakeIntakeClock()
	durable.holdClock = holdClock
	if !durable.takeHold(ctx) {
		t.Fatal("control: the holder did not take the delivery durable's hold")
	}
	first := durable.config(durable.tuning)
	if _, err := durable.subscribe(first.DeliverSubject); err != nil {
		t.Fatalf("subscribe the holder's first delivery at %s: %v", first.DeliverSubject, err)
	}
	if _, err := client.JS().AddConsumer(bus.Stream, &first); err != nil {
		t.Fatalf("create the delivery durable: %v", err)
	}
	token := intakeHoldToken{subject: first.DeliverSubject}
	if err := durable.hold.SetToken(ctx, token.String()); err != nil {
		t.Fatalf("record the holder's deliver subject: %v", err)
	}
	durable.recorded, durable.subject = token, first.DeliverSubject
	earlier := delivering(&durable)
	// Registered before the hand-back's release, so that release runs first: a finalizer waits for
	// the hand-back to land, and then flushes at its hold clock's ticks, which the cleanup moves.
	t.Cleanup(func() {
		finalized := make(chan struct{})
		go func() {
			durable.waitFinalizers()
			close(finalized)
		}()
		moveTicks(t, holdClock, intakeFlushWait, func() bool { return closedChannel(finalized) }, func() string {
			return "the holder's finalizer has not released its hold at the row's end"
		})
	})
	t.Cleanup(func() { durable.stop(cancelled()) })

	tries := newHoldTries()
	standby := startIntakeProcessWith(t, pool, natsURL, func(int) bool { return true }, tries.before)
	if tries.first(t, "the standby's try for the hold as it starts") {
		t.Fatal("control: the standby took the durable over at its start, while the holder held it")
	}
	from := recorded.count()
	sequences := []uint64{publishMergedPullRequest(t, client, number)}
	testwait.Receive(t, "the first delivery's callback to begin keying the message (control)", keying)

	admitted := make(chan struct{})
	var reached sync.Once
	letPublishGo := make(chan struct{})
	releasePublish := sync.OnceFunc(func() { close(letPublishGo) })
	t.Cleanup(releasePublish)
	hook := func() {
		reached.Do(func() { close(admitted) })
		<-letPublishGo
	}
	earlier.afterHandBackAdmitted.Store(&hook)

	// The stop's hand-back goes out only once its drain has answered, and only while its connected
	// read answers: each of the stop's calls waits for NATS with no bound of the row's.
	tuning.callWait.Store(math.MaxInt64)
	stopping, endStop := context.WithCancel(context.Background())
	defer endStop()
	firstStopped := make(chan struct{})
	go func() {
		durable.StopDelivering(stopping)
		close(firstStopped)
	}()
	waitFor(t, func() bool { return stopReleased(earlier) }, func() string {
		return "control: the first delivery's stop has not reached its wait for the drain to end"
	})
	releaseKeying()
	waitFor(t, func() bool { return closedChannel(admitted) }, func() string {
		return fmt.Sprintf("control: the callback's hand-back has not been admitted: the rule withheld it; logged %q", loggedMessages(logs()))
	})
	endStop()
	testwait.Receive(t, "the first delivery's stop to return once its context ended", firstStopped)
	if closedChannel(earlier.handBacksLanded()) {
		t.Fatal("control: the first delivery's stop returned with no hand-back counted")
	}

	second := durable.config(durable.tuning)
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
	moveTicks(t, holdClock, intakeFlushWait, func() bool { return closedChannel(stopped) }, func() string {
		return fmt.Sprintf("the holder's stop has not returned; its hold clock stands at %s, with %v armed", holdClock.Now(), holdClock.Armed())
	})
	if !holdLive(t, pool, durable.holder) {
		t.Fatalf("the stop released its hold while the earlier delivery's admitted hand-back had not been published, so its -NAK goes out after a standby can recreate the durable; events %q", recorded.since(from))
	}
	releasePublish()
	moveTicks(t, holdClock, intakeFlushWait, func() bool { return holdReleased(t, pool) }, func() string {
		return fmt.Sprintf("the holder's finalizer has not released its hold once the hand-back went on; logged %q", loggedMessages(logs()))
	})
	tries.untilDelivering(t, "the standby to take over the hold the holder released")
	waitFor(t, func() bool { return standby.fetches(number) >= 1 }, func() string {
		return fmt.Sprintf("the standby has not started the message the holder left: %s; events %q", consumerState(client), recorded.since(from))
	})
	standby.release()
	requireHandBackBeforeCreation(t, recorded, from, client, sequences)
}

// A release at the stop is no loss to an earlier delivery whose loss watcher is still confirming its
// hand-back.
//
// The stop releases the hold only once every hand-back the connection carries is confirmed, and the
// release closes the hold's Lost as a loss does. An earlier delivery under the same hold, sealed with a
// hand-back, keeps its loss watcher flushing until a flush confirms it (heldDurable.watchLoss). A
// release marked settled only for the latest delivery left that watcher reading the release as a loss:
// it flushed again (lostWith), and severed the connection when that flush took over intakeFlushWait.
//
// The row's holder takes the hold and the durable by hand, through a relay it can freeze
// (testnats.StallRelay.Freeze). Its first delivery hands its one message back on the moving link, with
// a delay that keeps the server from delivering it again during the row (handBackOwn), and a flush
// confirms that hand-back, so the connection carries no mark. The row then freezes the link
// and fills the relay's window, so no flush answers. It stops the first delivery, whose watcher then
// flushes on the frozen link, subscribes a second delivery under the same hold, which hands nothing
// back, and stops the holder under a context that has already ended, which releases the hold: the
// connection carries nothing unconfirmed. Once both deliveries' watchers have ended, the row requires
// that none severed the connection. The negative control is the settlement of the latest delivery
// alone: the first delivery's watcher severs, and the row fails naming the severed connection.
func TestAReleaseAtTheStopSeversNothingForAnEarlierDeliveryStillConfirmingItsHandBack(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	relay := testnats.StartStallRelay(t, natsURL)
	publisher := intakeTestClient(t)
	client, err := bus.Connect([]string{relay.URL()}, bus.WithName(intakeConnectionName))
	if err != nil {
		t.Fatalf("connect the holder through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	handed := make(chan *natsgo.Msg, 1)
	durable := newHeldDurable(tuningFor(t), client, deliveryTestLeases(t, pool), deliveryConsumerConfig, intakeKey,
		func(_ *intakeDelivery, message intakeMessage) {
			select {
			case handed <- message.msg:
			default:
			}
		})
	watchers := func() int { return lossWatchers(&durable) }
	if !durable.takeHold(ctx) {
		t.Fatal("control: the holder did not take the delivery durable's hold")
	}
	first := durable.config(durable.tuning)
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
	// Run in reverse: the link moves and the holder stops before its finalizer is waited for.
	t.Cleanup(durable.waitFinalizers)
	t.Cleanup(func() {
		relay.Move()
		durable.stop(cancelled())
	})

	publishMergedPullRequest(t, publisher, 100)
	msg := testwait.Receive(t, "the holder's first delivery to be handed the message (control)", handed)
	earlier.handBackOwn(msg, time.Hour)
	testwait.Receive(t, "the first delivery's hand-back to return on the moving link (control)", earlier.handBacksLanded())
	if !earlier.handedBack() {
		t.Fatalf("control: the first delivery withheld its hand-back; logged %q", loggedMessages(logs()))
	}
	if !durable.handBacksSettled() || client.CarriesUnconfirmed() {
		t.Fatal("control: a flush on the moving link did not confirm the first delivery's hand-back")
	}

	relay.Freeze()
	holderConn := client.Connection()
	if err := writeFiller(holderConn); err != nil {
		t.Fatalf("control: the holder's connection did not write the filler into its socket: %v", err)
	}
	relay.AwaitParked(t)
	durable.StopDelivering(cancelled())
	second := durable.config(durable.tuning)
	if _, err := durable.subscribe(second.DeliverSubject); err != nil {
		t.Fatalf("subscribe the holder's second delivery at %s: %v", second.DeliverSubject, err)
	}
	if latest := delivering(&durable); latest == earlier || latest.hold != earlier.hold || latest.handedBack() {
		t.Fatal("control: the holder's second delivery is not a new one under the same hold that handed nothing back")
	}
	waitFor(t, func() bool { return watchers() == 2 }, func() string {
		return fmt.Sprintf("control: %d loss watchers run, want 2: the first delivery's, flushing on the frozen link, and the second's", watchers())
	})
	stopped := make(chan struct{})
	go func() {
		durable.stop(cancelled())
		close(stopped)
	}()
	testwait.Receive(t, "the holder's stop to return", stopped)
	if holdLive(t, pool, durable.holder) {
		t.Fatalf("control: the stop did not release its hold, with nothing unconfirmed on the connection; logged %q", loggedMessages(logs()))
	}
	waitFor(t, func() bool { return watchers() == 0 }, func() string {
		return fmt.Sprintf("%d loss watchers still run since the release", watchers())
	})
	if loggedMessage(logs(), intakeHandBacksSevered) || holderConn.IsClosed() {
		t.Fatalf("the stop released its hold with every hand-back confirmed, and the earlier delivery's loss watcher read the release as a loss and severed the connection (status %v); logged %q",
			holderConn.Status(), loggedMessages(logs()))
	}
}
