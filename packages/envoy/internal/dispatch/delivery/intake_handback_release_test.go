package delivery

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// durableEvents records, in the order the server sent them, every answer published to the delivery
// durable ($JS.ACK...) and every creation of that durable ($JS.EVENT.ADVISORY.CONSUMER.CREATED...),
// on a connection of the test's own to uri. Both subscriptions feed one channel, which nats.go fills
// in the order the server's messages arrive, so the record orders an answer against a creation.
// That order cannot show a hand-back that made the recreated consumer deliver a message again: the
// server sends a creation's advisory after the new consumer already takes answers, so the rows also
// read that consumer itself (requireDeliveredOnceAfterTakeOver).
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

// heldPublish names whose hand-back of the row's second message a row holds before its publish
// (stopWithAHeldHandBack).
type heldPublish int

const (
	// stopsHandBack is the stop's own hand-back: the second message waits behind its key's
	// handler, the stop takes it from the keyed runner and hands it back (intakeDelivery.stop).
	stopsHandBack heldPublish = iota
	// callbacksHandBack is the callback's hand-back: the callback of the second message runs once
	// the stop has handed back what it held, as nats.go runs it during the drain, and hands the
	// message back itself (intakeDelivery.receive, giveBack).
	callbacksHandBack
)

// stopWithAHeldHandBack starts a holder and a standby, has the holder's handler hold one pull
// request with a second message of it behind, stops the holder, and holds a write on the holder's
// link while a hand-back of the second message waits, after the connected read has answered and
// before the publish (beforeHandBackPublish), so that publish waits for nats.go's lock. by names
// whose hand-back that is:
//   - stopsHandBack: the second message waits behind its key's handler, and the stop hands it
//     back. Its publish goes on once the write is held. The row stops the holder only once the
//     second message waits behind its key in the holder. Two messages outstanding at the server
//     do not show that: the server counts a push message as delivered as soon as it sends it,
//     before the message reaches the holder's client.
//   - callbacksHandBack: the callback of the second message waits before it routes the message
//     (beforeReceive) until the stop has handed back what it held (released), and then hands the
//     message back itself. Its publish goes on only when the row thaws the link, so the stop
//     decides whether to wait for that hand-back while its publish is still held.
//
// It returns once the write is held, with the stop under way. The stop's deadline is
// intakeStopBudget, set to budget.
func stopWithAHeldHandBack(t *testing.T, budget time.Duration, by heldPublish) heldHandBack {
	t.Helper()
	shrinkIntakeFlowControl(t)
	before := intakeStopBudget
	intakeStopBudget = budget
	t.Cleanup(func() { intakeStopBudget = before })
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
	// In a callback row, the callback of the second message waits before it routes the message,
	// so the message never reaches its key and the stop holds nothing of it. The hook is stored
	// once the first message's handler has started, so the first message it sees is the second.
	// It waits until the test's goroutine lets it go on (receive), once the stop has set released;
	// the cleanup, which runs before the holder's own, lets it go on when the test ends early.
	receiving := make(chan struct{})
	receive := func() {}
	if by == callbacksHandBack {
		goOn := make(chan struct{})
		receive = sync.OnceFunc(func() { close(goOn) })
		t.Cleanup(receive)
		var first sync.Once
		hook := func(*natsgo.Msg) {
			first.Do(func() {
				close(receiving)
				<-goOn
			})
		}
		subscribedDelivery(holder.intake).beforeReceive.Store(&hook)
	}
	row.sequences = append(row.sequences, publishMergedPullRequest(t, publisher, inFlight))
	awaitAckPending(t, publisher, 2)
	if by == stopsHandBack {
		waitFor(t, 20*time.Second, func() bool { return holder.keyedRunnerQueued() == 1 }, func() string {
			return fmt.Sprintf("control: %d message(s) wait behind their key in the holder, want the second message: it has not reached the holder's handler queue, so the stop would have nothing to hand back: %s",
				holder.keyedRunnerQueued(), consumerState(publisher))
		})
	}
	if by == callbacksHandBack {
		select {
		case <-receiving:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the holder's callback was never handed the second message: " + consumerState(publisher))
		}
	}

	// The stop's hand-backs go out only once its drain has answered (mayHandBack), and on a loaded
	// runner the drain can take longer than callNATS's wait: the stop then withholds its hand-backs,
	// and the row has no publish to hold. The row gives the stop's calls a 30 s guard, and puts the
	// wait back as the hand-back's publish begins, so the held write still outlives it.
	//
	// The hand-back waits in its hook, before its publish, until the test's goroutine holds the
	// write lock, and in a callback row until the row thaws the link. Every bounded wait and every
	// failure is on the test's goroutine, not the delivery's, so the hand-back reaching its publish
	// and the write taking the lock each have a bound and a message of their own. ended, closed by
	// a cleanup that runs before the holder's own, lets a hand-back still held there go on when the
	// test ends early.
	wait := callNATSWait.Swap(int64(30 * time.Second))
	restoreWait := sync.OnceFunc(func() { callNATSWait.Store(wait) })
	t.Cleanup(restoreWait)
	var holding sync.Once
	reached := make(chan struct{})
	held := make(chan struct{})
	ended := make(chan struct{})
	t.Cleanup(func() { close(ended) })
	hook := func() {
		holding.Do(func() {
			restoreWait()
			close(reached)
			select {
			case <-held:
			case <-ended:
			}
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
	// The handler finishes as the stop waits for it. In a stop row, the stop then hands back the
	// message that was waiting behind it. In a callback row, the stop then sets released, and the
	// test lets the callback go on, which hands the second message back at once (giveBack). Either
	// hand-back waits in the hook, before its publish.
	holder.release()
	unreached := "control: the stop's hand-back never reached its publish: the stop withheld it, as it does when its drain has not answered within the row's 30 s guard: "
	if by == callbacksHandBack {
		waitFor(t, 30*time.Second, holder.heldListReleased, func() string {
			return "control: the stop never handed back what it held, so the callback would hold its message rather than hand it back: " + consumerState(publisher)
		})
		receive()
		unreached = "control: the callback's hand-back never reached its publish: the callback withheld it, as it does when the drain has not answered within the row's 30 s guard: "
	}
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal(unreached + consumerState(publisher))
	}
	row.thaw = link.HoldWriteLock(t, holderConn)
	if by == callbacksHandBack {
		thaw := row.thaw
		row.thaw = sync.OnceFunc(func() {
			close(held)
			thaw()
		})
		return row
	}
	close(held)
	return row
}

// heldHandBack is a holder's stop with a hand-back's publish held (stopWithAHeldHandBack): thaw
// lets the holder's link move and ends the held write, and in a callback row first lets the held
// publish go on; stopped closes when the holder's Run has returned; conn is the holder's
// connection, delivery the subscription its stop stopped, from the first durable event of the
// stop, and sequences the stream sequences of the row's two messages.
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
	sequences []uint64
}

// handBackPending reports whether a hand-back of the stopped delivery is still taken and not
// settled: in these rows, a publish still waiting for NATS.
func (r heldHandBack) handBackPending() bool {
	select {
	case <-r.delivery.handBacksLanded():
		return false
	default:
		return true
	}
}

// stopBudget is the stop's deadline in the rows whose stop must reach its deadline with the
// hand-back still pending (intakeStopBudget): short, since each waits it out.
const stopBudget = 3 * time.Second

// landingBudget is the stop's deadline in the row whose hand-back must land before it. Every step
// of that row between the stop's start and the link moving (the write's hold, the stop's own calls
// under it, the subtest's wait for the stop to start waiting for its hand-back) is spent from it, so
// it is long: with stopBudget, CI job 136549646454 of envoy-go-race saw the stop's deadline pass
// before the hand-back landed, and the subtest then read the hold the finalizer keeps.
const landingBudget = 30 * time.Second

// holderHolds reports whether the delivery durable's hold is live and names holder's own process.
// The standby is woken by the release (duty.HoldReleasedChannel) and takes the hold at once, so a
// read of whether any process holds it, made once the stop has released it, can find the
// standby's: under six busy loops 1 of 400 runs of the landing row read it so and failed as a stop
// that kept its hold.
func holderHolds(t *testing.T, pool *store.Pool, holder *intakeProcess) bool {
	t.Helper()
	var named string
	if err := pool.QueryRow(t.Context(),
		"select coalesce((select holder::text from duty_holds where name = $1 and expires_at > now()), '')",
		intakeHoldName(bus.DispatchDeliveryDurable)).Scan(&named); err != nil {
		t.Fatalf("read the delivery durable's hold: %v", err)
	}
	return named == holder.intake.holder.String()
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
// standby's creation of the durable, and fails the test when the hand-back came after, or when the
// standby's consumer delivered one of the row's messages (sequences) more than once.
func requireHandBackBeforeCreation(t *testing.T, recorded *durableEvents, from int, publisher *bus.Client, sequences []uint64) {
	t.Helper()
	waitFor(t, 60*time.Second, func() bool {
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
	waitFor(t, 60*time.Second, func() bool {
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
	waitFor(t, 60*time.Second, func() bool { return slices.Contains(recorded.since(from), durableCreated) }, func() string {
		return fmt.Sprintf("the standby did not create the durable: %q; %s", recorded.since(from), consumerState(publisher))
	})
	if events := recorded.since(from); handBackAfterCreation(events) {
		t.Fatalf("a hand-back landed after the standby created the durable, so it reached the standby's consumer: %q", events)
	}
	requireDeliveredOnceAfterTakeOver(t, publisher, sequences)
}

// A hand-back the holder made during its stop never reaches the durable a standby recreates.
//
// A hand-back is a publish at the message's reply subject, which names the durable rather than the
// consumer: one that reaches the server after the standby's take-over has deleted the durable and
// created it again under the same name moves that message's pending entry on the standby's consumer
// and has the standby delivered it a second time. The stop waits for each publish at most
// bus.ConnLockWait (intakeDelivery.publishHandBack) and goes on without one NATS has not taken; so
// the stop releases the hold only once every hand-back taken has settled and every one published
// has reached the server; one still pending at the stop's deadline keeps the hold renewing until it
// has (heldDurable.finalizeHandBacks).
//
// Every subtest holds a write on the holder's link while a hand-back waits before its publish: the
// stop's own in the first three, the callback's in the fourth (stopWithAHeldHandBack). Each reads,
// in the order the server sent them, the answers published to the durable and the durable's
// creations, on a connection of its own, and every step waits for an event. Once the standby has
// acknowledged the messages the holder left, each also reads the consumer the standby created and
// requires that it delivered each of them once: a hand-back can make that consumer deliver a
// message again before the server sends its creation's advisory, where the order alone passes it
// (requireDeliveredOnceAfterTakeOver).
//   - a hand-back that lands before the stop's deadline (landingBudget): the stop goes on without
//     the publish (its log line). Then, before the link moves, the stop is seen waiting for its
//     hand-back with its hold live (handBacksWaiting). Once the link moves, the hold is released by
//     the time the stop returns, and the hand-back reached the server before the standby created the
//     durable. The subtest reads its own premise first: a stop whose deadline passed before its
//     hand-back landed logs intakeHoldFinalizing, and the subtest then fails as a premise not met,
//     naming the budget, rather than as a stop that kept its hold;
//   - a hand-back still pending at the stop's deadline: the link stays held, and the stop returns
//     while it is, with the hand-back still pending (handBacksLanded). The hold still names the
//     holder then, and the standby has not created the durable. With the link still held, the hold is
//     renewed before the database's clock passes the expiry it had as the stop returned, so it does
//     not lapse, and the standby still has not created the durable. Once the link moves, the
//     hand-back reaches the server before the standby creates the durable;
//   - a pending hand-back at shutdown: the Run returns at its deadline, then StartIntakes' stop
//     closes the client that can still send it and waits for its finalizer. The hold is gone before
//     that stop returns, so its caller can close the pool without leaving the hold to lapse. The
//     close sends the hand-back in its flush or drops it with the connection, whichever takes
//     nats.go's lock first; either way it does not reach the server after the standby creates the
//     durable;
//   - a hand-back the callback takes during the drain (landingBudget): the callback of the second
//     message runs once the stop has handed back what it held, and hands the message back itself.
//     Its publish is held until the link moves. The stop's drain loop gives up while the write is
//     held, and the stop is then seen waiting for that hand-back with its hold live. Once the link
//     moves, the subtest reads what the first one reads.
//
// No step waits on a time but the stop's own deadline (intakeStopBudget) and the hold's own expiry;
// each 30 s wait only guards against a stop that never gets to the step the wait is for. The
// negative controls, each with the subtests that it fails:
//   - a release that does not wait for the hand-backs fails all four. The first sees the hold
//     released before the link moves. The second sees the hold released when the stop returns with
//     the hand-back pending. The third sees no finalizer settle the hand-back after the client
//     closes. The fourth sees the hold released while the callback's publish is held;
//   - a stop that never releases its hold when a publish was left running fails the first: the
//     hold is still live after the stop returns;
//   - a release made at the deadline with a hand-back pending fails the second: the hold is gone
//     when the stop returns;
//   - a stop that leaves its hold to lapse with a hand-back pending fails the second: the hold
//     reaches its expiry unrenewed;
//   - an untracked finalizer fails the third: its shutdown stop returns with the hold live;
//   - the holder's -NAK published again while the standby holds the message, with the server
//     holding the creation's advisory until the new consumer has delivered it a second time, fails
//     the first three by that second delivery. The count does not fail a stop that gives up the
//     holder's handler at its deadline, whose deferred answer hands the first message back too: the
//     new consumer then starts at that message and delivers both once;
//   - a hand-back counted only once its publish begins, in publishHandBack, fails the fourth: the
//     stop finds no hand-back counted when its drain loop gives up, and releases its hold while the
//     callback's publish is held.
func TestAHandBackLeftRunningLandsBeforeTheStandbyTakesOver(t *testing.T) {
	t.Run("a hand-back that lands before the stop's deadline", func(t *testing.T) {
		logs := captureLogs(t)
		// The stop runs the hook on its own goroutine, so the hook only closes waiting. The hook is
		// stored before stopWithAHeldHandBack starts the stop, so the stop finds it in handBacksReached.
		waiting := make(chan struct{})
		var signal sync.Once
		hook := func() { signal.Do(func() { close(waiting) }) }
		handBacksWaiting.Store(&hook)
		t.Cleanup(func() { handBacksWaiting.Store(nil) })
		row := stopWithAHeldHandBack(t, landingBudget, stopsHandBack)
		waitFor(t, 30*time.Second, func() bool { return loggedMessage(logs(), callLeftRunning) }, func() string {
			return "control: the stop never went on without the held hand-back's publish: " + consumerState(row.publisher)
		})
		waitFor(t, 30*time.Second, func() bool {
			select {
			case <-waiting:
				return true
			default:
				return !holderHolds(t, row.pool, row.holder)
			}
		}, func() string {
			return "the stop did not start to wait for its hand-back and did not release its hold within 30 s of going on without the publish: " + consumerState(row.publisher)
		})
		if !holderHolds(t, row.pool, row.holder) {
			t.Fatalf("the stop released its hold before its hand-back reached the server, so a standby could recreate the durable first; the holder logged %q", loggedMessages(logs()))
		}
		row.thaw()
		select {
		case <-row.stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the holder's stop did not return within 30 s of the link moving")
		}
		// The premise: the hand-back landed before the stop's deadline. A stop whose deadline passed
		// first has said so (intakeHoldFinalizing) and left its hold to the finalizer, and such a run
		// reads nothing of the stop's release, whatever the hold reads next.
		if loggedMessage(logs(), intakeHoldFinalizing) {
			t.Fatalf("control: the premise did not hold: the stop's deadline (landingBudget, %s) passed before its hand-back landed, so this run reads nothing of a hand-back that lands before it; the holder logged %q", landingBudget, loggedMessages(logs()))
		}
		if holderHolds(t, row.pool, row.holder) {
			t.Fatalf("the stop kept its hold although its hand-back reached the server before its deadline, which costs the standby the lapse; the holder logged %q", loggedMessages(logs()))
		}
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher, row.sequences)
	})

	t.Run("a hand-back still pending at the stop's deadline", func(t *testing.T) {
		row := stopWithAHeldHandBack(t, stopBudget, stopsHandBack)
		select {
		case <-row.stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the holder's stop did not return within 30 s while its hand-back was held: it waits for the hand-back")
		}
		testnats.RequireLockHeld(t, row.conn)
		if !row.handBackPending() {
			t.Fatal("control: the stop's hand-back reached NATS while the write was held")
		}
		if !holderHolds(t, row.pool, row.holder) {
			t.Fatal("the stop released its hold with a hand-back still pending, so a standby may recreate the durable before the hand-back reaches the server")
		}
		if slices.Contains(row.recorded.since(row.from), durableCreated) {
			t.Fatalf("the standby created the durable while the holder's hand-back was pending: %q", row.recorded.since(row.from))
		}
		if !holdRenewedBeforeItsExpiry(t, row.pool) {
			t.Fatalf("the hold reached its expiry unrenewed with the holder's hand-back still pending, so the standby can recreate the durable before a link that heals sends the hand-back to its consumer: %q", row.recorded.since(row.from))
		}
		testnats.RequireLockHeld(t, row.conn)
		if !row.handBackPending() {
			t.Fatal("control: the stop's hand-back reached NATS while the write was held")
		}
		if slices.Contains(row.recorded.since(row.from), durableCreated) {
			t.Fatalf("the standby created the durable while the holder's hand-back was pending and its hold renewing: %q", row.recorded.since(row.from))
		}
		row.thaw()
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher, row.sequences)
	})

	t.Run("a pending hand-back at shutdown", func(t *testing.T) {
		row := stopWithAHeldHandBack(t, stopBudget, stopsHandBack)
		select {
		case <-row.stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the holder's Run did not return within 30 s while its hand-back was held")
		}
		if !row.handBackPending() {
			t.Fatal("control: the hand-back reached NATS before its client closed")
		}
		finalizing := make(chan struct{})
		releaseFinalizer := make(chan struct{})
		hook := func() {
			close(finalizing)
			<-releaseFinalizer
		}
		finalizerBeforeRelease.Store(&hook)
		t.Cleanup(func() { finalizerBeforeRelease.Store(nil) })
		var runs sync.WaitGroup
		closed := make(chan struct{})
		go func() {
			stopIntakes(&runs, &row.holder.intake.heldDurable)
			close(closed)
		}()
		testnats.RequireLockHeld(t, row.conn)
		row.thaw()
		select {
		case <-finalizing:
		case <-time.After(30 * time.Second):
			t.Fatal("the client close did not settle the pending hand-back within 30 s")
		}
		select {
		case <-closed:
			t.Fatal("StartIntakes' stop returned while its finalizer was held before release")
		default:
		}
		if !holderHolds(t, row.pool, row.holder) {
			t.Fatal("the hold was released before the finalizer's release step")
		}
		close(releaseFinalizer)
		select {
		case <-closed:
		case <-time.After(30 * time.Second):
			t.Fatal("StartIntakes' stop did not return within 30 s of finalizer release")
		}
		if holderHolds(t, row.pool, row.holder) {
			t.Fatal("the pending hand-back's finalizer released but left its hold live: shutdown could close the pool before releasing it")
		}
		requireNoHandBackAfterCreation(t, row.recorded, row.from, row.publisher, row.sequences)
	})

	// The callback of the second message runs once the stop has handed back what it held, as
	// nats.go runs a message the client had buffered during the drain. It hands the message back
	// itself (receive, giveBack, handBack), and its publish waits in beforeHandBackPublish until the
	// row thaws the link. With the write held, the stop's drain loop gives up, since its connected
	// read does not answer, and the stop then decides whether to wait for that hand-back. The
	// subtest requires that the stop waits for it (handBacksWaiting) with its hold live, and then
	// reads what the landing subtest reads.
	//
	// Control: with the hand-back counted only once its publish begins, in publishHandBack (the
	// code before the take), the subtest fails at that check. The stop finds no hand-back counted
	// when its drain loop gives up, and releases its hold while the callback's publish is held.
	t.Run("a hand-back the callback takes during the drain", func(t *testing.T) {
		logs := captureLogs(t)
		// The stop runs the hook on its own goroutine, so the hook only closes waiting. The hook is
		// stored before stopWithAHeldHandBack starts the stop, so the stop finds it in handBacksReached.
		waiting := make(chan struct{})
		var signal sync.Once
		hook := func() { signal.Do(func() { close(waiting) }) }
		handBacksWaiting.Store(&hook)
		t.Cleanup(func() { handBacksWaiting.Store(nil) })
		row := stopWithAHeldHandBack(t, landingBudget, callbacksHandBack)
		waitFor(t, 30*time.Second, func() bool {
			select {
			case <-waiting:
				return true
			default:
				return !holderHolds(t, row.pool, row.holder)
			}
		}, func() string {
			return "the stop did not start to wait for the callback's hand-back and did not release its hold within 30 s of the write being held: " + consumerState(row.publisher)
		})
		if !holderHolds(t, row.pool, row.holder) {
			t.Fatalf("the stop released its hold while a hand-back the subscription's callback took during the drain had not reached the server; the holder logged %q", loggedMessages(logs()))
		}
		row.thaw()
		select {
		case <-row.stopped:
		case <-time.After(30 * time.Second):
			t.Fatal("the holder's stop did not return within 30 s of the link moving")
		}
		// The premise, as in the landing subtest: the hand-back landed before the stop's deadline.
		if loggedMessage(logs(), intakeHoldFinalizing) {
			t.Fatalf("control: the premise did not hold: the stop's deadline (landingBudget, %s) passed before the callback's hand-back landed, so this run reads nothing of a hand-back that lands before it; the holder logged %q", landingBudget, loggedMessages(logs()))
		}
		if holderHolds(t, row.pool, row.holder) {
			t.Fatalf("the stop kept its hold although the callback's hand-back reached the server before its deadline, which costs the standby the lapse; the holder logged %q", loggedMessages(logs()))
		}
		requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher, row.sequences)
	})
}
