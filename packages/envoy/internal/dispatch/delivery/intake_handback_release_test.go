package delivery

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// durableEvents records, in the order the server sent them, every answer published to the delivery
// durable ($JS.ACK...) and every creation of that durable ($JS.EVENT.ADVISORY.CONSUMER.CREATED...),
// on a connection of the test's own to uri. Both subscriptions feed one channel, which nats.go fills
// in the order the server's messages arrive, so the record orders an answer against a creation.
// That order cannot show a hand-back that made the recreated consumer deliver a message again: the
// server sends a creation's advisory after the new consumer already takes answers, so the rows also
// read that consumer itself (requireDeliveredOnceAfterTakeOver). A third subscription takes the
// watch's own barrier messages (barrier), which say that every earlier event has been recorded.
type durableEvents struct {
	conn     *natsgo.Conn
	mu       sync.Mutex
	events   []string
	barriers map[string]bool
}

const durableCreated = "created"

// durableEventsBarrier is the subject of the watch's own barrier messages (durableEvents.barrier).
const durableEventsBarrier = "testdelivery.durable-events.barrier"

func watchDurableEvents(t *testing.T, uri string) *durableEvents {
	t.Helper()
	conn := testnats.Connect(t, uri)
	arrived := make(chan *natsgo.Msg, 4096)
	for _, subject := range []string{
		"$JS.ACK." + bus.Stream + "." + bus.DispatchDeliveryDurable + ".>",
		"$JS.EVENT.ADVISORY.CONSUMER.CREATED." + bus.Stream + "." + bus.DispatchDeliveryDurable,
		durableEventsBarrier,
	} {
		if _, err := conn.ChanSubscribe(subject, arrived); err != nil {
			t.Fatalf("subscribe to %s: %v", subject, err)
		}
	}
	if err := conn.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("flush the durable's watch: %v", err)
	}
	recorded := &durableEvents{conn: conn, barriers: map[string]bool{}}
	go func() {
		for msg := range arrived {
			recorded.mu.Lock()
			switch {
			case msg.Subject == durableEventsBarrier:
				recorded.barriers[string(msg.Data)] = true
			case strings.HasPrefix(msg.Subject, "$JS.EVENT."):
				recorded.events = append(recorded.events, durableCreated)
			default:
				recorded.events = append(recorded.events, string(msg.Data))
			}
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

// barrier waits until the watch has recorded every event the server had handled when barrier
// began: it publishes a message of its own on the watch's connection and waits until the watch
// records it. The server sends the watch each event as it handles the publish that makes it, and
// handles this one later, so the watch receives it after them, in order on its one channel.
func (r *durableEvents) barrier(t *testing.T) {
	t.Helper()
	id := natsgo.NewInbox()
	if err := r.conn.Publish(durableEventsBarrier, []byte(id)); err != nil {
		t.Fatalf("publish the durable watch's barrier: %v", err)
	}
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.barriers[id]
	}, func() string {
		return fmt.Sprintf("the durable's watch has not recorded its barrier %s; it holds %d event(s)", id, r.count())
	})
}

// holderAndStandby is a holder process whose clocks are fakes only the row moves -- its own
// deadlines' (stopClock, heldDurable.clock), its hold's (holdClock, heldDurable.holdClock) and its
// calls' waits for NATS (callClock, intakeTuning.callClock, which the standby beside it shares) --
// and the tries for the hold of a standby process beside it (standby, holdTries).
type holderAndStandby struct {
	holder    *intakeProcess
	stopClock *clocktest.Fake
	holdClock *clocktest.Fake
	callClock *clocktest.Fake
	standby   *holdTries
}

// startHolderAndStandby starts a holder on holderURL, its NATS client given options, whose GitHub
// holds the fetch of pull request inFlight (startIntakeProcessWith) and whose clocks are fakes only
// the row moves; waits until it has bound the durable, which publisher reads, and its first try for
// the hold has ended (firstTryEnded: a standby's hook set before then would count that try as the
// standby's); keeps the hold's row live on the database's clock as the renewals a hold clock the row
// does not move never make would (keepHoldRowLive), past anything the row waits for; and starts a
// standby on natsURL, whose try as it starts must leave it standing by (holdTries.standBy). Every
// row on it asserts the holder's stop going on without a call NATS cannot answer under the link the
// row holds, so both processes wait for each call production's callWait, on a fake of the row's
// (fakeCallClock), which the row moves past each such call's wait (passStop, passCallWait); the
// standby's calls, on a link that moves, answer.
func startHolderAndStandby(t *testing.T, pool *store.Pool, publisher *bus.Client, holderURL, natsURL string, inFlight int, options ...bus.ConnectOption) holderAndStandby {
	t.Helper()
	started := holderAndStandby{stopClock: fakeIntakeClock(), holdClock: fakeIntakeClock(), callClock: fakeCallClock(t), standby: newHoldTries()}
	var holderTried <-chan struct{}
	started.holder = startIntakeProcessWith(t, pool, holderURL, func(number int) bool { return number == inFlight }, func(intake *Intake) {
		intake.clock, intake.holdClock = started.stopClock, started.holdClock
		holderTried = firstTryEnded(intake)
	}, options...)
	awaitBoundDurable(t, publisher)
	testwait.Receive(t, "the holder's first try for the hold to end", holderTried)
	keepHoldRowLive(t, pool, started.holder.intake.holder)
	startIntakeProcessWith(t, pool, natsURL, nil, started.standby.on)
	started.standby.standBy(t, "the standby, while the holder held the durable", 0)
	return started
}

// stopWithAHeldHandBack starts a holder and a standby (startHolderAndStandby), has the holder's
// handler hold one pull request with a second message of it waiting behind, stops the holder, and
// holds a write on the holder's link from inside the stop's hand-back of the waiting message, after
// the connected read has answered and before the publish (beforeHandBackPublish), so that publish
// waits for nats.go's lock. It returns once the write is held, with the stop under way.
//
// The holder's stop deadline and its hold run on its two clocks, fakes only the row moves: the
// stop's deadline, the intakes' stopBudget from its Run's end, passes only where the row moves the
// stop clock (passStop), and the hold renews, or is lost, only where the row moves the hold clock
// (renewHoldOnItsClock, loseHold), the hold's row kept live meanwhile. The standby tries for the hold
// only where the row moves its own fake (holdTries). Both intakes' ack wait is past the run
// (ackWaitPastTheRun): the hand-back is what must bring the message to the standby, so the server's
// own redelivery at the ack wait never comes inside the run, and the holder's handler holds the first
// message until the row releases it or the stop gives it up, never until a deadline of its own, so
// the second message queues behind it.
func stopWithAHeldHandBack(t *testing.T) heldHandBack {
	t.Helper()
	ackWaitPastTheRun(t)
	tuning := tuningFor(t)
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
	row.holderAndStandby = startHolderAndStandby(t, pool, publisher, link.URL(), natsURL, inFlight)
	row.take = row.holdClock.Now()
	holder := row.holder
	holderConn := holder.client.Connection()
	row.conn = holderConn

	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	waitFor(t, func() bool { return holder.fetches(inFlight) >= 1 }, func() string {
		return "the holder never started on the first message: " + consumerState(publisher)
	})
	row.delivery = subscribedDelivery(holder.intake)
	// The second message has to be in the holder's hands, queued behind the first, before the stop
	// begins, or the stop has nothing to hand back. The durable's count of messages outstanding does
	// not say that: nats-server counts a message delivered (consumer.go, deliverMsg's trackPending)
	// before its stream's send loop puts it on the wire (stream.go, internalLoop), and a stop whose
	// drain reaches the server first leaves the message with the durable, never sent to the holder.
	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	waitFor(t, func() bool { return queuedBehindKeys(row.delivery) == 1 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind the holder's handler, want the second message: %s", queuedBehindKeys(row.delivery), consumerState(publisher))
	})

	// The stop's hand-backs go out only once its drain has answered (mayHandBack), and each of the
	// stop's calls waits for NATS at most the intakes' callWait: a drain that answers after it has
	// the stop withhold its hand-backs, and the row has no publish to hold. So the stop's calls wait
	// for NATS with no bound of the row's, and the wait is put back as the hand-back's publish
	// begins, so the held write still outlives it.
	wait := tuning.callWait.Swap(math.MaxInt64)
	restoreWait := sync.OnceFunc(func() { tuning.callWait.Store(wait) })
	t.Cleanup(restoreWait)
	var holding sync.Once
	held := make(chan struct{})
	thawed := make(chan func(), 1)
	hook := func() {
		holding.Do(func() {
			restoreWait()
			thawed <- link.HoldWriteLock(t, holderConn)
			close(held)
		})
	}
	row.delivery.beforeHandBackPublish.Store(&hook)
	row.from = row.recorded.count()
	done := make(chan struct{})
	row.stopped = done
	go func() {
		holder.stop()
		close(done)
	}()
	waitFor(t, holder.keyedRunnerStopped, func() string {
		return "the stopping holder has not stopped its keyed runner: " + consumerState(publisher)
	})
	// The handler finishes as the stop waits for it; the stop then hands back the message that
	// was waiting behind it, and the hook holds the write before that publish. The hook runs on a
	// goroutine the holder's Run waits for -- the stop's own, or a handler's the stop gave up, whose
	// deferred answer hands its message back -- so a hand-back reaches the hook before Run returns,
	// and a Run that returned with held still open made none: the row fails then, at once. held is
	// read again once done is, as both can be closed by the time the select runs.
	holder.release()
	waitFor(t, func() bool {
		select {
		case <-held:
			return true
		case <-done:
			select {
			case <-held:
				return true
			default:
				t.Fatal("control: the holder's stop returned with no hand-back having reached its publish: it withheld them: " + consumerState(publisher))
			}
		default:
		}
		return false
	}, func() string {
		return "the holder's stop neither published a hand-back nor returned: " + consumerState(publisher)
	})
	row.thaw = testwait.Receive(t, "the hook's hold of the write, sent before held closed", thawed)
	return row
}

// heldHandBack is a holder's stop with a hand-back's publish held (stopWithAHeldHandBack): thaw
// lets the holder's link move and ends the held write; stopped closes when the holder's Run has
// returned; conn is the holder's connection, delivery the subscription its stop stopped, from
// the first durable event of the stop, and sequences the stream sequences of the row's two
// messages. The holder, its two clocks and the standby's tries are the embedded holderAndStandby's,
// all moved only by the row, and take is the hold clock's moment as the holder took its hold.
type heldHandBack struct {
	holderAndStandby
	pool      *store.Pool
	thaw      func()
	recorded  *durableEvents
	from      int
	stopped   <-chan struct{}
	publisher *bus.Client
	conn      *natsgo.Conn
	delivery  *intakeDelivery
	sequences []uint64
	take      time.Time
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

// passStop moves the holder's stop through what it waits for on its clocks until its Run returns
// (passStopOn).
func (r heldHandBack) passStop(t *testing.T) {
	t.Helper()
	passStopOn(t, r.stopClock, r.holdClock, r.callClock, r.stopped)
}

// passStopOn moves a holder's stop through what it waits for on its three clocks, fakes only the
// row moves, until its Run returns (stopped): the stop's deadline, the intakes' stopBudget on
// stopClock from the moment its Run's context ended, which that clock has not moved from
// (stopContext), once the stop has armed it; then, until Run has returned, callClock, the clock of
// the intake's calls (fakeCallClock), to each wait for NATS a call of the stop arms there that NATS
// cannot answer under the link the row holds (callNATS, passNextWait), and holdClock to each moment
// the stop and its loss watcher arm a flush's bound or wait at, one intakeFlushWait on
// (flushHandBacks, watchLoss). The stop's wait for its drain ends at the stop's deadline, which
// gives its handlers up (awaitDrain), so nothing more of stopClock's is moved.
func passStopOn(t *testing.T, stopClock, holdClock, callClock *clocktest.Fake, stopped <-chan struct{}) {
	t.Helper()
	budget := tuningFor(t).stopBudget
	stopClock.AwaitArmedAt(t, "the holder's stop deadline, armed as its Run's context ended", stopClock.Now().Add(budget))
	stopClock.Advance(budget)
	moveTicks(t, holdClock, intakeFlushWait, func() bool {
		if closedChannel(stopped) {
			return true
		}
		passNextWait(callClock, time.Duration(tuningFor(t).callWait.Load()))
		return false
	}, func() string {
		return fmt.Sprintf("the holder's Run has not returned past its stop's deadline; its hold clock stands at %s, with %v armed, and the clock of its calls at %s, with %v armed",
			holdClock.Now(), holdClock.Armed(), callClock.Now(), callClock.Armed())
	})
}

// renewHoldOnItsClock moves the holder's hold clock to the hold's first renewal, intakeHoldRenew
// past the take, once the renewal has armed it (duty.Hold), and waits until that renewal has landed:
// the hold's row expires intakeHoldTTL past a statement's now() between a moment of the database's
// clock read before the move and the read, which the expiry keepHoldRowLive set a day ahead is not.
// The row has moved the hold clock no further than a flush's bound before (passStop), so no renewal
// has run. A hold whose renewals stopped arms none, and the wait for it gives up naming it.
//
// duty's renewal reads no expiry: `update duty_holds set expires_at = now() + make_interval(secs =>
// $3) where name = $1 and holder = $2` (duty's renewHold) lands on a row of this holder's that has
// lapsed as well as on a live one, and makes it live again. So a renewal that landed is a renewal
// before the row's expiry only when its now() came before the expiry the row had as the move began,
// read with the database's clock before the move: the row fails when it came at or after it, a
// lapsed row revived, which a standby could have taken in between. It then keeps the row live again
// (keepHoldRowLive), as the renewals after it would, since nothing moves the clock to them.
func (r heldHandBack) renewHoldOnItsClock(t *testing.T) {
	t.Helper()
	var since, expiry time.Time
	if err := r.pool.QueryRow(t.Context(), "select clock_timestamp(), expires_at from duty_holds where name = $1 and holder = $2",
		intakeHoldName(bus.DispatchDeliveryDurable), r.holder.intake.holder).Scan(&since, &expiry); err != nil {
		t.Fatalf("read the database's clock and the expiry of the delivery durable's hold: %v", err)
	}
	renewal := r.take.Add(intakeHoldRenew)
	r.holdClock.AwaitArmedAt(t, "the hold's first renewal, intakeHoldRenew past the take", renewal)
	r.holdClock.Set(renewal)
	var expires time.Time
	var beforeExpiry bool
	testwait.Until(t, "the hold's renewal the move brought to land while the holder's hand-back is pending", func() (string, bool) {
		var renewed bool
		if err := r.pool.QueryRow(t.Context(), `
			select expires_at, expires_at - make_interval(secs => $2) between $3 and clock_timestamp(),
				expires_at - make_interval(secs => $2) < $5
			from duty_holds where name = $1 and holder = $4
		`, intakeHoldName(bus.DispatchDeliveryDurable), intakeHoldTTL.Seconds(), since, r.holder.intake.holder, expiry).Scan(&expires, &renewed, &beforeExpiry); err != nil {
			t.Fatalf("read the delivery durable's hold: %v", err)
		}
		return fmt.Sprintf("the hold expires at %s; the move began after %s", expires.Format(time.RFC3339Nano), since.Format(time.RFC3339Nano)), renewed
	})
	if !beforeExpiry {
		t.Fatalf("the hold's renewal landed at %s on the database's clock, not before the expiry %s the row had as the move began: it revived a lapsed row rather than renewing a live one",
			expires.Add(-intakeHoldTTL).Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano))
	}
	keepHoldRowLive(t, r.pool, r.holder.intake.holder)
}

// standbyTakesOverOnceReleased moves the holder's hold clock to each tick the finalizer of its
// hand-back waits for, one intakeFlushWait apart (finalizeHandBacks), until the holder has released
// its hold, whose row is then gone (holdReleased), and then has the standby try for the hold until
// it takes the durable over (holdTries.untilDelivering).
func (r heldHandBack) standbyTakesOverOnceReleased(t *testing.T) {
	t.Helper()
	moveTicks(t, r.holdClock, intakeFlushWait, func() bool { return holdReleased(t, r.pool) }, func() string {
		return fmt.Sprintf("the holder has not released its hold; its hold clock stands at %s, with %v armed", r.holdClock.Now(), r.holdClock.Armed())
	})
	r.standby.untilDelivering(t, "the standby to take over the hold the holder released")
}

// loseHoldUntilSevered loses the hold of intake, whose lease pool the row has cut and whose hold
// runs on holdClock, a fake only the row moves (loseHold), and waits until severed reports the
// effect of the sever the loss must bring, under what. A delivery whose hand-back may still reach
// NATS as its hold is lost waits intakeFlushWait on the hold's clock for a flush's answer
// (heldDurable.lostWith), and severs the intake's connection when none has come by then
// (severHandBacks): each reading of severed that finds no sever yet moves holdClock past that wait
// once the loss watcher has armed it, and never further (clocktest.Fake.SetWhen). A watcher whose
// flush answers false first severs at that answer and disarms its wait, which is then never there
// to pass. The wait decides whether the sever comes, so it runs on the hold's clock and not on the
// wall clock. It is half the hold's margin, the time the hold's row stays live past the loss
// deadline, so in production the sever comes before a standby can take the durable over; the row's
// own order (lapseHoldRow once the sever came) does not read that, so this reads the two constants.
func loseHoldUntilSevered(t *testing.T, holdClock *clocktest.Fake, intake *Intake, what string, severed func() (string, bool)) {
	t.Helper()
	if intakeFlushWait >= intakeHoldMargin {
		t.Fatalf("intakeFlushWait (%s) is not under intakeHoldMargin (%s): a lost hold's sever could come after the hold's row has lapsed and a standby has recreated the durable", intakeFlushWait, intakeHoldMargin)
	}
	deadline := loseHold(t, holdClock, intake)
	waited := deadline.Add(intakeFlushWait)
	testwait.Until(t, what, func() (string, bool) {
		reading, done := severed()
		if !done {
			holdClock.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
				return waited, now.Before(waited) && slices.ContainsFunc(armed, waited.Equal)
			})
		}
		return fmt.Sprintf("%s; the hold's clock %s past its loss deadline", reading, holdClock.Now().Sub(deadline)), done
	})
}

// holdLive reports whether the delivery durable's hold is live and names holder, the row's
// heldDurable.holder. A row's standby takes a released or lapsed hold at its next poll
// (durableBindInterval), so a read of whether any process holds the hold, made once the holder has
// let it go, can find the standby's.
func holdLive(t *testing.T, pool *store.Pool, holder uuid.UUID) bool {
	t.Helper()
	var named string
	if err := pool.QueryRow(t.Context(),
		"select coalesce((select holder::text from duty_holds where name = $1 and expires_at > now()), '')",
		intakeHoldName(bus.DispatchDeliveryDurable)).Scan(&named); err != nil {
		t.Fatalf("read the delivery durable's hold: %v", err)
	}
	return named == holder.String()
}

// requireHandBackBeforeCreation waits until the server has had both the holder's hand-back and the
// standby's creation of the durable, and fails the test when the hand-back came after, or when the
// standby's consumer delivered one of the row's messages (sequences) more than once.
func requireHandBackBeforeCreation(t *testing.T, recorded *durableEvents, from int, publisher *bus.Client, sequences []uint64) {
	t.Helper()
	waitFor(t, func() bool {
		events := recorded.since(from)
		return slices.Contains(events, "-NAK") && slices.Contains(events, durableCreated)
	}, func() string {
		return fmt.Sprintf("the hand-back and the standby's creation of the durable did not both reach the server: %q; %s", recorded.since(from), consumerState(publisher))
	})
	events := recorded.since(from)
	if !slices.Contains(events[:slices.Index(events, durableCreated)], "-NAK") {
		t.Fatalf("the hand-back landed after the standby created the durable, so it reached the standby's consumer: %q", events)
	}
	if handBackAfterCreation(events) {
		t.Fatalf("a hand-back landed after the standby created the durable, so it reached the standby's consumer: %q", events)
	}
	requireDeliveredOnceAfterTakeOver(t, publisher, sequences)
}

// handBackAfterCreation reports whether a -NAK follows the first creation of the durable in events.
// It reads every -NAK, not only the first: a -NAK after the creation behind an earlier one counts.
func handBackAfterCreation(events []string) bool {
	created := slices.Index(events, durableCreated)
	return created >= 0 && slices.Contains(events[created+1:], "-NAK")
}

// requireDeliveredOnceAfterTakeOver waits until the consumer the standby created has delivered
// every message it had to and the standby has acknowledged them, and fails the test when it
// delivered one of them more than once. It had to deliver each of the row's messages (sequences)
// from its start sequence on (OptStartSeq, the durable's ack floor plus one; takeOverConfig): the
// message the holder's stop handed back, and the one before it too when the stop gave up the
// holder's handler at its deadline and the handler's deferred answer handed that one back as well
// (Intake.deliver). Delivered.Consumer counts every delivery, so a hand-back that made the new
// consumer deliver a message again makes it one more than that count. The recorded order cannot
// show that hand-back: nats-server v2.15.1 makes a new consumer's ack subscription live (setLeader,
// consumer.go:1491) before it queues the consumer's create advisory (consumer.go:1504), and sends
// the advisory from the stream's own send queue, so the -NAK can be recorded before "created".
// NumRedelivered does not keep it either: the standby's acknowledgement clears it
// (consumer.go:3802). A -NAK that reaches the new consumer before its first delivery of the
// message, or after the standby acknowledged it, changes nothing, because the server ignores it
// (processNak, consumer.go:3255 and :3259), and the row does not fail for it.
func requireDeliveredOnceAfterTakeOver(t *testing.T, publisher *bus.Client, sequences []uint64) {
	t.Helper()
	var info *natsgo.ConsumerInfo
	waitFor(t, func() bool {
		read, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err != nil {
			return false
		}
		info = read
		return info.Delivered.Consumer > 0 && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return "the standby's consumer did not deliver the messages the holder left and have them acknowledged: " + consumerState(publisher)
	})
	start := info.Config.OptStartSeq
	var want uint64
	for _, sequence := range sequences {
		if sequence >= start {
			want++
		}
	}
	if info.Delivered.Consumer != want {
		t.Fatalf("the standby's consumer delivered %d time(s) from stream sequence %d, want %d, once for each of the row's messages from there (stream sequences %v): a hand-back made it deliver one again (last delivered stream sequence %d, redelivered %d)",
			info.Delivered.Consumer, start, want, sequences, info.Delivered.Stream, info.NumRedelivered)
	}
}

// requireNoHandBackAfterCreation waits for the standby's creation of the durable and fails the test
// when a hand-back reached the server after it, or when the standby's consumer delivered one of the
// row's messages (sequences) more than once. A hand-back the holder's client dropped with its
// connection never reaches the server, which is allowed: the message stays with the durable.
func requireNoHandBackAfterCreation(t *testing.T, recorded *durableEvents, from int, publisher *bus.Client, sequences []uint64) {
	t.Helper()
	waitFor(t, func() bool { return slices.Contains(recorded.since(from), durableCreated) }, func() string {
		return fmt.Sprintf("the standby did not create the durable: %q; %s", recorded.since(from), consumerState(publisher))
	})
	if events := recorded.since(from); handBackAfterCreation(events) {
		t.Fatalf("a hand-back landed after the standby created the durable, so it reached the standby's consumer: %q", events)
	}
	requireDeliveredOnceAfterTakeOver(t, publisher, sequences)
}

// A hand-back the holder's stop published never reaches the durable a standby recreates.
//
// A hand-back is a publish at the message's reply subject, which names the durable rather than the
// consumer: one that reaches the server after the standby's take-over has deleted the durable and
// created it again under the same name moves that message's pending entry on the standby's consumer
// and has the standby delivered it a second time. The stop waits for each publish at most
// bus.ConnLockWait (intakeDelivery.publishHandBack) and goes on without one NATS has not taken; so
// the stop releases the hold only once every hand-back has reached the server; one still pending at
// the stop's deadline keeps the hold renewing until it has (heldDurable.finalizeHandBacks).
//
// All three subtests hold a write on the holder's link from inside its stop's hand-back
// (stopWithAHeldHandBack). Each reads, in the order the server sent them, the answers published to
// the durable and the durable's creations, on a connection of its own, and every step waits for an
// event. Once the standby has acknowledged the messages the holder left, each also reads the
// consumer the standby created and requires that it delivered each of them once: a hand-back can
// make that consumer deliver a message again before the server sends its creation's advisory, where
// the order alone passes it (requireDeliveredOnceAfterTakeOver).
//   - a hand-back that lands before the stop's deadline: the link moves once the stop has gone on
//     without the publish (its log line). The hold is released by the time the stop returns, and
//     the hand-back reached the server before the standby created the durable.
//   - a hand-back still pending at the stop's deadline: the link stays held, and the stop returns
//     while it is, with the hand-back still pending (handBacksLanded). The hold still names the
//     holder then, and the standby's try stands by and has not created the durable. With the link
//     still held, the hold's next renewal, which the row brings on the hold's clock, lands, so the
//     hold does not lapse, and the standby's next try still stands by. Once the link moves, the
//     hand-back reaches the server before the standby creates the durable;
//   - a pending hand-back at shutdown: the Run returns at its deadline, then StartIntakes' stop
//     closes the client that can still send it and waits for its finalizer. The hold is gone before
//     that stop returns, so its caller can close the pool without leaving the hold to lapse. The
//     close sends the hand-back in its flush or drops it with the connection, whichever takes
//     nats.go's lock first; either way it does not reach the server after the standby creates the
//     durable.
//
// No step waits on a time. The stop's own deadline (the intakes' stopBudget) is on the holder's stop
// clock, which the first subtest never moves and the second and third move to that deadline once
// the write is held (passStop); each of the stop's calls' waits for NATS (callNATS, callWait) is on
// the clock of the intake's calls (fakeCallClock), which the first subtest moves only past the held
// publish's wait, once the publish has armed it (passCallWait), and the second and third past each
// call's wait (passStop); the hold's renewals and the bounds of the flushes that confirm its
// hand-backs are on its hold clock, which the row moves where it needs one (renewHoldOnItsClock,
// standbyTakesOverOnceReleased); and the standby tries for the hold only where the row moves its
// own clock. Every wait is testwait's, which only guards against an event that never comes. The first
// subtest's stop deadline never passes, so a run whose stop still said its hand-back had not reached
// NATS by that deadline (intakeHoldFinalizing) fails as a control. stopWithAHeldHandBack begins the
// stop only once the second message is queued in the holder, and decides whether the stop's
// hand-back reached its publish by which comes first, that publish or the holder's Run returning.
// The negative controls: a stop that withholds its hand-backs fails all three there, as the holder's
// Run returns; a release that does not wait for the hand-backs fails the second's hold, as its stop
// returns; a stop that never releases its hold when a publish was left running fails the first's
// release; a release made at the deadline with a hand-back pending fails the second's hold and its
// standby's try; a stop that leaves its hold to lapse with a hand-back pending writes no renewal, and
// the second's wait for it gives up naming it; an untracked finalizer fails the third by returning
// from its shutdown stop with the hold live; and the holder's -NAK published again while the standby
// holds the message, with the server holding the creation's advisory until the new consumer has
// delivered it a second time, fails all three by that second delivery. The count does not fail a
// stop that gives up the holder's handler at its deadline, whose deferred answer hands the first
// message back too: the new consumer then starts at that message and delivers both once.
func TestAHandBackLeftRunningLandsBeforeTheStandbyTakesOver(t *testing.T) {
	t.Parallel()
	t.Run("a hand-back that lands before the stop's deadline", func(t *testing.T) {
		t.Parallel()
		logs := captureIntakeLogs(t)
		row := stopWithAHeldHandBack(t)
		// The publish waits for NATS at most callWait on the clock of the holder's calls (callNATS),
		// which the row moves there once the publish has armed it; the stop clock, and so the stop's
		// deadline, stays where it is.
		passCallWait(t, row.callClock, time.Duration(tuningFor(t).callWait.Load()), "the held hand-back's publish")
		waitFor(t, func() bool { return loggedMessage(logs(), callLeftRunning) }, func() string {
			return "control: the stop never went on without the held hand-back's publish: " + consumerState(row.publisher)
		})
		row.thaw()
		testwait.Receive(t, "the holder's stop to return once the link moved", row.stopped)
		// The premise: the hand-back landed before the stop's deadline, which the row never moves the
		// stop clock to. A stop that still said its hand-back had not reached NATS by the deadline
		// (intakeHoldFinalizing) left its hold to its finalizer, and such a run reads nothing of a
		// hand-back that lands before the deadline, whatever the hold reads next.
		if loggedMessage(logs(), intakeHoldFinalizing) {
			t.Fatalf("control: the premise did not hold: the stop said its hand-back had not reached NATS by its deadline, which its clock never reached, so this run reads nothing of a hand-back that lands before it; the holder logged %q", loggedMessages(logs()))
		}
		if holdLive(t, row.pool, row.holder.intake.holder) {
			t.Fatalf("the stop kept its hold although its hand-back reached the server before its deadline, which costs the standby the lapse; the holder logged %q", loggedMessages(logs()))
		}
		row.standbyTakesOverOnceReleased(t)
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher, row.sequences)
	})

	t.Run("a hand-back still pending at the stop's deadline", func(t *testing.T) {
		t.Parallel()
		row := stopWithAHeldHandBack(t)
		row.passStop(t)
		testnats.RequireLockHeld(t, row.conn)
		if !row.handBackPending() {
			t.Fatal("control: the stop's hand-back reached NATS while the write was held")
		}
		if !holdLive(t, row.pool, row.holder.intake.holder) {
			t.Fatal("the stop released its hold with a hand-back still pending, so a standby may recreate the durable before the hand-back reaches the server")
		}
		if row.standby.next(t, "the standby's try while the holder's hand-back is pending") {
			t.Fatalf("the standby took the durable over while the holder's hand-back was pending: %q", row.recorded.since(row.from))
		}
		if slices.Contains(row.recorded.since(row.from), durableCreated) {
			t.Fatalf("the standby created the durable while the holder's hand-back was pending: %q", row.recorded.since(row.from))
		}
		row.renewHoldOnItsClock(t)
		testnats.RequireLockHeld(t, row.conn)
		if !row.handBackPending() {
			t.Fatal("control: the stop's hand-back reached NATS while the write was held")
		}
		if row.standby.next(t, "the standby's try while the holder's hand-back is pending and its hold renewing") {
			t.Fatalf("the standby took the durable over while the holder's hand-back was pending and its hold renewing: %q", row.recorded.since(row.from))
		}
		if slices.Contains(row.recorded.since(row.from), durableCreated) {
			t.Fatalf("the standby created the durable while the holder's hand-back was pending and its hold renewing: %q", row.recorded.since(row.from))
		}
		row.thaw()
		row.standbyTakesOverOnceReleased(t)
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher, row.sequences)
	})

	t.Run("a pending hand-back at shutdown", func(t *testing.T) {
		t.Parallel()
		row := stopWithAHeldHandBack(t)
		row.passStop(t)
		if !row.handBackPending() {
			t.Fatal("control: the hand-back reached NATS before its client closed")
		}
		finalizing := make(chan struct{})
		releaseFinalizer := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseFinalizer) })
		hook := func() {
			close(finalizing)
			<-releaseFinalizer
		}
		row.holder.intake.tuning.beforeRelease.Store(&hook)
		var runs sync.WaitGroup
		closed := make(chan struct{})
		go func() {
			stopIntakes(&runs, &row.holder.intake.heldDurable)
			close(closed)
		}()
		// A row that fails from here on still ends the stop before it ends: its cleanups let the
		// held finalizer go and thaw the link, then wait for the stop through the shared wait.
		t.Cleanup(func() {
			testwait.Receive(t, "StartIntakes' stop to return once the row's cleanup released its finalizer", closed)
		})
		t.Cleanup(row.thaw)
		t.Cleanup(release)
		testnats.RequireLockHeld(t, row.conn)
		row.thaw()
		// The finalizer's first flush may find the close not yet done; it then waits for its next
		// tick, intakeFlushWait on the hold clock (finalizeHandBacks), which the row moves it to.
		moveTicks(t, row.holdClock, intakeFlushWait, func() bool { return closedChannel(finalizing) }, func() string {
			return fmt.Sprintf("the client close did not settle the pending hand-back, so its finalizer never reached its release; its hold clock stands at %s, with %v armed", row.holdClock.Now(), row.holdClock.Armed())
		})
		select {
		case <-closed:
			t.Fatal("StartIntakes' stop returned while its finalizer was held before release")
		default:
		}
		if !holdLive(t, row.pool, row.holder.intake.holder) {
			t.Fatal("the hold was released before the finalizer's release step")
		}
		release()
		testwait.Receive(t, "StartIntakes' stop to return once its finalizer was released", closed)
		if holdLive(t, row.pool, row.holder.intake.holder) {
			t.Fatal("the pending hand-back's finalizer released but left its hold live: shutdown could close the pool before releasing it")
		}
		row.standby.untilDelivering(t, "the standby to take over the hold the holder's finalizer released")
		requireNoHandBackAfterCreation(t, row.recorded, row.from, row.publisher, row.sequences)
	})
}
