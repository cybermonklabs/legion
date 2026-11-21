package delivery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// stopRow is one intake delivery subscribed through a handshake hold (testnats.HandshakeHold) on a
// NATS server of the row's own, whose monitoring endpoint counts the subscribers on its deliver
// subject.
type stopRow struct {
	server *testnats.Server
	// uri is the server's own client URL, which reaches it outside the hold.
	uri    string
	hold   *testnats.HandshakeHold
	client *bus.Client
	// conn is the client's connection, read before any hold: Client.Connection takes the client's
	// lock, which a caller of Client.Connected holds while it waits for nats.go's lock, as the
	// stop's drain does during a held handshake.
	conn    *natsgo.Conn
	durable *heldDurable
	sub     *natsgo.Subscription
	subject string
	// reconnected takes one value per reconnect of the client.
	reconnected chan struct{}
	// decided takes one value each time a given-up handler's hand-back has returned
	// (startGivenUpRow), whether it published or withheld.
	decided chan struct{}
	// entries counts the given-up handler's entries (startGivenUpRow): one for each message whose
	// handler started.
	entries *atomic.Int32
}

// startStopRow subscribes a delivery keyed by key and handled by handle at the durable's deliver
// subject, creating the durable first when createDurable says so, and waits until the server counts
// its subscriber.
func startStopRow(t *testing.T, key func(*natsgo.Msg) (string, *contracts.Envelope, bool), handle func(*intakeDelivery, intakeMessage), createDurable bool) *stopRow {
	t.Helper()
	server, uri := testnats.Start(t)
	hold := testnats.StartHandshakeHold(t, uri)
	client := reconnectTestClient(t, hold.URL())
	// Registered after the client's close, so they run before it: nats.go's Close waits for its
	// connection lock, which a held handshake or a held write holds.
	t.Cleanup(hold.Restore)
	t.Cleanup(hold.Release)
	reconnected := make(chan struct{}, 8)
	client.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		select {
		case reconnected <- struct{}{}:
		default:
		}
		return nil
	})
	wanted := deliveryConsumerConfig()
	if createDurable {
		if _, err := client.JS().AddConsumer(bus.Stream, &wanted); err != nil {
			t.Fatalf("create the delivery durable: %v", err)
		}
	}
	durable := newHeldDurable(client, nil, func() natsgo.ConsumerConfig { return wanted }, key, handle)
	sub, err := durable.subscribe(wanted.DeliverSubject)
	if err != nil {
		t.Fatalf("subscribe at %s: %v", wanted.DeliverSubject, err)
	}
	row := &stopRow{server: server, uri: uri, hold: hold, client: client, conn: client.Connection(), durable: &durable, sub: sub, subject: wanted.DeliverSubject, reconnected: reconnected}
	waitFor(t, 10*time.Second, func() bool { return row.subscribers(t) == 1 }, func() string {
		return fmt.Sprintf("control: the server reports %d subscriber(s) on the deliver subject before the hold, want 1", row.subscribers(t))
	})
	return row
}

// subscribers is how many subscribers the server counts on the row's deliver subject.
func (r *stopRow) subscribers(t *testing.T) int {
	return deliverSubjectSubscribers(t, r.server, r.subject)
}

// delivery is the subscription the row's durable delivers on.
func (r *stopRow) delivery() *intakeDelivery {
	r.durable.mu.Lock()
	defer r.durable.mu.Unlock()
	return r.durable.delivering
}

// publishPullRequestSeven publishes two envelopes of one pull request, which the intake keys alike,
// so the second waits behind the first.
func (r *stopRow) publishPullRequestSeven(t *testing.T) {
	t.Helper()
	for range 2 {
		publishPullRequestEnvelope(t, r.client, "acme", "widgets", 7, map[string]string{
			"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
		})
	}
}

// stopUnder starts a StopDelivering of the row's durable under ctx, and returns a call that waits
// for it to return, for AnswersWithinAttempt or answersWhileWriteHeld.
func (r *stopRow) stopUnder(ctx context.Context) func() error {
	stopped := make(chan struct{})
	go func() {
		r.durable.StopDelivering(ctx)
		close(stopped)
	}()
	return func() error {
		<-stopped
		return nil
	}
}

// cancelled is a context that has already ended: a stop under it gives its handlers up at once.
func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// answersWhileWriteHeld waits for each call to answer and fails the test unless all have while a
// write still holds nats.go's lock on conn (RequireLockHeld). The held write never ends on its own,
// so a call that waits for the lock never answers, and the 30 s wait is only a guard.
func answersWhileWriteHeld(t *testing.T, conn *natsgo.Conn, what string, calls ...func() error) {
	t.Helper()
	testnats.AnswersWithin(t, 30*time.Second, what, calls...)
	testnats.RequireLockHeld(t, conn)
}

// watchHandBacks counts the negative acknowledgements the server is sent, on a connection of the
// test's own to uri, outside the hold. The count it returns is read after a Barrier of that
// connection, so every callback the server routed to it before the barrier has run.
func watchHandBacks(t *testing.T, uri string) func() int64 {
	t.Helper()
	var naks atomic.Int64
	observer := testnats.Connect(t, uri)
	if _, err := observer.Subscribe("$JS.ACK.>", func(msg *natsgo.Msg) {
		if strings.HasPrefix(string(msg.Data), "-NAK") {
			naks.Add(1)
		}
	}); err != nil {
		t.Fatalf("subscribe to the stream's acknowledgements: %v", err)
	}
	if err := observer.Flush(); err != nil {
		t.Fatalf("flush the acknowledgement watch: %v", err)
	}
	return func() int64 {
		if err := observer.Flush(); err != nil {
			t.Fatalf("flush the acknowledgement watch: %v", err)
		}
		done := make(chan struct{})
		if err := observer.Barrier(func() { close(done) }); err != nil {
			t.Fatalf("barrier the acknowledgement watch: %v", err)
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the acknowledgement watch's barrier did not return within 30 s")
		}
		return naks.Load()
	}
}

// startGivenUpRow starts a row whose handler does what Intake.deliver does with a message whose
// handling the stop gave up -- it waits for the give-up and hands the message back (handBack) --
// and publishes two messages of one pull request, so the first is in its handler and the second
// waits behind its key, held by the stop. The row counts each entry into the handler (entries).
func startGivenUpRow(t *testing.T) *stopRow {
	t.Helper()
	handling := make(chan struct{}, 2)
	decided := make(chan struct{}, 2)
	entries := new(atomic.Int32)
	row := startStopRow(t, intakeKey, func(delivery *intakeDelivery, message intakeMessage) {
		entries.Add(1)
		handling <- struct{}{}
		<-delivery.handling.Done()
		delivery.handBack(message.msg)
		select {
		case decided <- struct{}{}:
		default:
		}
	}, true)
	row.decided = decided
	row.entries = entries
	row.publishPullRequestSeven(t)
	select {
	case <-handling:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the first message never reached its handler")
	}
	waitFor(t, 30*time.Second, func() bool { return queuedBehindKeys(row.delivery()) == 1 }, func() string {
		return fmt.Sprintf("control: %d message(s) wait behind their key's handler, want the second message", queuedBehindKeys(row.delivery()))
	})
	return row
}

// requireNoHandBack fails the test when the server was sent a hand-back of a message of delivery.
// It reads that once the client's link is back and moving: once every hand-back publish delivery
// left running has returned (handBacksLanded), the client's flush has put whatever it had queued on
// the server, and the watch's Barrier has run every callback the server routed to it.
func (r *stopRow) requireNoHandBack(t *testing.T, delivery *intakeDelivery, naks func() int64) {
	t.Helper()
	select {
	case <-delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("a hand-back publish was still waiting for NATS 30 s after the link came back")
	}
	if err := r.conn.Flush(); err != nil {
		t.Fatalf("flush the intake's client once its link was back: %v", err)
	}
	if n := naks(); n != 0 {
		t.Fatalf("the server was sent %d hand-back(s) of the messages the stop withheld, want none", n)
	}
}

// reconnect releases the row's hold and waits until the client has reconnected through it.
func (r *stopRow) reconnect(t *testing.T) {
	t.Helper()
	for len(r.reconnected) > 0 {
		<-r.reconnected
	}
	r.hold.Release()
	select {
	case <-r.reconnected:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the client did not reconnect within 30 s of the hold's release")
	}
}

// An intake's stop does not wait for nats.go's connection lock, which a reconnect's handshake and a
// write to a link that does not move hold. The stop ran its drain, its hand-backs and the
// unsubscribe that ends a slow drain inside its once with no bound, and waited for its handlers,
// whose answers waited for that lock too, so it gave no answer until the lock was let go, and a
// second StopDelivering waited for it on the once.
//
// Every subtest decides by events, never by how long a call took. One that holds a handshake has
// every StopDelivering it makes answer within the attempt held when it waits for them
// (HandshakeHold.AnswersWithinAttempt): no later attempt has reached the hold, and nats.go's lock is
// still held. One that holds a write has them answer while the write is still held; the write never
// ends on its own. Each stop's context has already ended, or never ends, so no stop answers by its
// own deadline:
//
//   - a drain NATS has not taken: two calls at once during a held handshake, under contexts that
//     never end; once the hold is released the subscription is drained all the same: the server
//     counts no subscriber on its deliver subject and the handle is no longer valid;
//   - a given-up handler during a held handshake: the handler hands its message back as
//     Intake.deliver does, and the stop holds a second message behind it; the server is sent no
//     hand-back of either, even once the hold is released and the link moves;
//   - a stop between reconnect attempts: the same, begun while nats.go waits between two held
//     attempts with its lock free, so the drain answers at once on a link that is down; the stops'
//     context ends once the next attempt is held;
//   - a drain on a link that is down: the stop's drain answers at once between two attempts, the
//     link then comes back, and only then is the handler given up: the server is sent no hand-back
//     of either message, since nothing says the server had the subscription's interest gone;
//   - an acknowledgement, a head-write hand-back and a job acknowledgement begun before the stop:
//     a handler begins its answer during a held handshake, and the stop begins once it has;
//   - an unsubscribe while a write holds the lock: the stop drained on a moving link and its drain
//     did not end, and the unsubscribe that ends it, once the stop's context ends, meets the lock;
//   - a hand-back while a write holds the lock: the connected read before the first hand-back the
//     rule lets through answers, and that hand-back then waits before its publish
//     (beforeHandBackPublish) until a write holds nats.go's lock. The subtest's own goroutine takes
//     the lock, so the hand-back reaching its publish and the write taking the lock each have a
//     bound of their own. Once the stop has returned, the durable still holds both messages for
//     redelivery: neither is acknowledged, and both are still outstanding with it;
//   - a drain the lock holds: the stop drained on a moving link and waits for its drain to end, with
//     a message still buffered, under a context that never ends; a write then holds nats.go's lock,
//     under which nats.go ends no drain, and the stop and a second StopDelivering return while it is
//     held. The drain's own wait (intakeDrainWait) is kept from ending it;
//   - a given-up handler woken as the give-up is armed: as the second, with one stop, whose
//     give-up runs at once under its ended context; the stop waits, once its give-up is armed
//     (stopGiveUpArmed), for the handler it woke to decide its hand-back, and the server is sent
//     no hand-back of either message;
//   - a queued message's handler does not start once the stop has begun: as the eleventh, on a
//     moving link with no hold; once the woken handler has decided and returned, the keyed runner
//     runs no handler and has started none other, so the second message's handler never started.
//     The stop then goes on and hands that message back: the server is sent its hand-back, and the
//     durable still holds both messages for redelivery, neither acknowledged.
//
// The negative control is the stop as it was (DrainSubscription, the hand-backs and Unsubscribe
// inside the once with no bound): no StopDelivering answers within the attempt or while the write is
// held. So is a hand-back that skips the stop's rule (handBack), the given-up handler's in the second
// and third subtests; a stop that withholds only when its drain did not answer, in the fourth; an
// answer begun before the stop with no bound, in the fifth to seventh; a hand-back published inline,
// in the ninth; a wait for the drain that reads nothing of the connection, in the tenth; a stop
// that sets stopping only after its give-up is armed, in the eleventh, whose woken handler publishes
// the hand-back the stop withholds; and a stop that takes the messages waiting behind their keys
// only after its give-up is armed, in the twelfth, whose keyed runner starts the queued message's
// handler under the ended context, so that message's hand-back is withheld rather than held.
func TestAnIntakesStopDoesNotWaitOutAHeldHandshake(t *testing.T) {
	t.Run("a drain NATS has not taken", func(t *testing.T) {
		row := startStopRow(t, intakeKey, func(*intakeDelivery, intakeMessage) {}, false)
		row.hold.HoldReconnect(t, row.conn)
		row.hold.AnswersWithinAttempt(t, row.conn, "StopDelivering", row.stopUnder(context.Background()), row.stopUnder(context.Background()))
		row.reconnect(t)
		waitFor(t, 30*time.Second, func() bool { return !row.sub.IsValid() && row.subscribers(t) == 0 }, func() string {
			return fmt.Sprintf("after the reconnect the stopped subscription is still valid = %t and the server reports %d subscriber(s) on its deliver subject, want neither: the stop left it subscribed",
				row.sub.IsValid(), row.subscribers(t))
		})
	})

	t.Run("a given-up handler during a held handshake", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		delivery := row.delivery()
		row.hold.HoldReconnect(t, row.conn)
		row.hold.AnswersWithinAttempt(t, row.conn, "StopDelivering", row.stopUnder(cancelled()), row.stopUnder(cancelled()))
		row.reconnect(t)
		row.requireNoHandBack(t, delivery, naks)
	})

	t.Run("a stop between reconnect attempts", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		delivery := row.delivery()
		row.hold.HoldReconnect(t, row.conn)
		// The held attempt ends at the client's connect timeout; nats.go then waits a second
		// before the next, which the hold holds too.
		testnats.AwaitLockFree(t, row.conn)
		between := row.hold.Attempts()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first, second := row.stopUnder(ctx), row.stopUnder(ctx)
		// The handler is given up, and hands its message back, once the next attempt is held.
		waitFor(t, 30*time.Second, func() bool { return row.hold.Attempts() > between }, func() string {
			return "control: no next attempt reached the hold"
		})
		testnats.AwaitLockHeld(t, row.conn)
		cancel()
		row.hold.AnswersWithinAttempt(t, row.conn, "StopDelivering", first, second)
		row.reconnect(t)
		row.requireNoHandBack(t, delivery, naks)
	})

	t.Run("a drain on a link that is down", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		delivery := row.delivery()
		row.hold.HoldReconnect(t, row.conn)
		// Between two held attempts the link is down and nats.go's lock is free, so the drain
		// answers at once: DrainSubscription reads the link down and unsubscribes on the client alone.
		testnats.AwaitLockFree(t, row.conn)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stopped := row.stopUnder(ctx)
		waitFor(t, 30*time.Second, func() bool { return !row.sub.IsValid() }, func() string {
			return "control: the stop's drain did not end the subscription on the down link between attempts"
		})
		row.reconnect(t)
		// The link is back: the handler is given up only now, and the stop hands back what it held.
		cancel()
		testnats.AnswersWithin(t, 30*time.Second, "StopDelivering", stopped)
		row.requireNoHandBack(t, delivery, naks)
	})

	for _, answer := range []struct {
		name string
		make func(*intakeDelivery, intakeMessage)
	}{
		{"an acknowledgement begun before the stop", func(d *intakeDelivery, m intakeMessage) { d.acknowledge(m.msg) }},
		{"a head-write hand-back begun before the stop", func(d *intakeDelivery, m intakeMessage) { d.handBackOwn(m.msg, headWriteRedeliverAfter) }},
		{"a job acknowledgement begun before the stop", func(d *intakeDelivery, m intakeMessage) {
			(&JobIntake{ctx: context.Background()}).deliver(d, m)
		}},
	} {
		t.Run(answer.name, func(t *testing.T) {
			entered := make(chan struct{}, 2)
			answering := make(chan struct{}, 2)
			proceed := make(chan struct{})
			var proceeding sync.Once
			goOn := func() { proceeding.Do(func() { close(proceed) }) }
			t.Cleanup(goOn)
			// The handler makes its answer to NATS once the row lets it, as Intake.deliver and
			// JobIntake.deliver do when their handling is done.
			row := startStopRow(t, intakeKey, func(d *intakeDelivery, m intakeMessage) {
				entered <- struct{}{}
				<-proceed
				answering <- struct{}{}
				answer.make(d, m)
			}, true)
			row.publishPullRequestSeven(t)
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				t.Fatal("control: the first message never reached its handler")
			}
			row.hold.HoldReconnect(t, row.conn)
			goOn()
			select {
			case <-answering:
			case <-time.After(30 * time.Second):
				t.Fatal("control: the handler never began its answer")
			}
			row.hold.AnswersWithinAttempt(t, row.conn, "StopDelivering", row.stopUnder(cancelled()), row.stopUnder(cancelled()))
		})
	}

	t.Run("an unsubscribe while a write holds the lock", func(t *testing.T) {
		row, release := startBufferedDrainRow(t)
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stopped := row.stopUnder(ctx)
		awaitDrainWait(t, row)
		row.hold.HoldWriteLock(t, row.conn)
		cancel()
		answersWhileWriteHeld(t, row.conn, "StopDelivering", stopped)
	})

	t.Run("a hand-back while a write holds the lock", func(t *testing.T) {
		row := startGivenUpRow(t)
		publisher := row.outsideHold(t)
		// The link moves until the subtest holds the write, so the stop's drain and its read of the
		// connection before the hand-back answer; the 30 s wait is only a guard, so that a call
		// slower than callNATS's wait on a loaded runner does not withhold the hand-back the row
		// reads. The subtest puts callNATS's own wait back before it holds the write, and the
		// hand-back's publish, which starts after the hook, waits only that long for the held lock.
		// Registered after startGivenUpRow, so the wait is restored before the row's teardown runs.
		wait := callNATSWait.Swap(int64(30 * time.Second))
		t.Cleanup(func() { callNATSWait.Store(wait) })
		// The hand-back waits in its hook, before its publish, until the subtest holds the write
		// lock. Every wait and every failure is on the subtest's goroutine, not the handler's, so
		// the hand-back reaching its publish and the write taking the lock each have a bound and a
		// message of their own. ended, closed by a cleanup that runs before the row's own, lets a
		// hand-back still held there go on when the subtest ends early.
		reached := make(chan struct{})
		held := make(chan struct{})
		ended := make(chan struct{})
		t.Cleanup(func() { close(ended) })
		var reaching sync.Once
		hook := func() {
			reaching.Do(func() { close(reached) })
			select {
			case <-held:
			case <-ended:
			}
		}
		row.delivery().beforeHandBackPublish.Store(&hook)
		// The stop gives the handler up at its start. The handler's hand-back comes before the
		// drain has answered and is withheld; once the drain has answered, the stop hands back the
		// message it took from behind the key, and that hand-back waits in the hook.
		stopped := row.stopUnder(cancelled())
		select {
		case <-reached:
		case <-time.After(30 * time.Second):
			t.Fatal("control: no hand-back reached its publish within 30 s")
		}
		callNATSWait.Store(wait)
		row.hold.HoldWriteLock(t, row.conn)
		close(held)
		answersWhileWriteHeld(t, row.conn, "StopDelivering", stopped)
		requireBothOutstanding(t, publisher, "once the stop returned while a write held nats.go's lock")
	})

	t.Run("a drain the lock holds", func(t *testing.T) {
		wait := intakeDrainWait.Swap(int64(time.Hour))
		t.Cleanup(func() { intakeDrainWait.Store(wait) })
		row, release := startBufferedDrainRow(t)
		defer release()
		stopped := row.stopUnder(context.Background())
		awaitDrainWait(t, row)
		delivery := row.delivery()
		delivery.mu.Lock()
		taken := delivery.handBackAllowed
		delivery.mu.Unlock()
		if !taken {
			t.Fatal("control: the stop's drain was not taken on a moving link")
		}
		row.hold.HoldWriteLock(t, row.conn)
		answersWhileWriteHeld(t, row.conn, "StopDelivering", stopped, row.stopUnder(context.Background()))
	})

	t.Run("a given-up handler woken as the give-up is armed", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		delivery := row.delivery()
		// The stop waits in its hook until the subtest has seen the woken handler decide. Every
		// wait and every failure is on the subtest's goroutine, not the stop's: a failure the hook
		// reported could land after the subtest had ended. ended, closed by a cleanup that runs
		// before the row's own, lets a stop still held there go on when the subtest ends early.
		reached := make(chan struct{})
		proceed := make(chan struct{})
		ended := make(chan struct{})
		t.Cleanup(func() { close(ended) })
		var arming sync.Once
		armed := func() {
			arming.Do(func() { close(reached) })
			select {
			case <-proceed:
			case <-ended:
			}
		}
		stopGiveUpArmed.Store(&armed)
		t.Cleanup(func() { stopGiveUpArmed.Store(nil) })
		row.hold.HoldReconnect(t, row.conn)
		stopped := row.stopUnder(cancelled())
		select {
		case <-reached:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the stop did not arm its give-up within 30 s")
		}
		select {
		case <-row.decided:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the given-up handler did not decide its hand-back within 30 s of the give-up")
		}
		close(proceed)
		row.hold.AnswersWithinAttempt(t, row.conn, "StopDelivering", stopped)
		stopGiveUpArmed.Store(nil)
		row.reconnect(t)
		row.requireNoHandBack(t, delivery, naks)
	})

	t.Run("a queued message's handler does not start once the stop has begun", func(t *testing.T) {
		row := startGivenUpRow(t)
		naks := watchHandBacks(t, row.uri)
		publisher := row.outsideHold(t)
		delivery := row.delivery()
		// The link moves, so every call of the stop's answers; the 30 s wait is only a guard, so
		// that a drain or a connected read slower than callNATS's wait on a loaded runner does not
		// withhold the hand-back the row reads. Registered after startGivenUpRow, so the wait is
		// restored before the row's teardown runs.
		wait := callNATSWait.Swap(int64(30 * time.Second))
		t.Cleanup(func() { callNATSWait.Store(wait) })
		// As in the subtest before: the stop waits in its hook, every wait and every failure is on
		// the subtest's goroutine, and ended, closed by a cleanup that runs before the row's own,
		// lets a stop still held there go on when the subtest ends early.
		reached := make(chan struct{})
		proceed := make(chan struct{})
		ended := make(chan struct{})
		t.Cleanup(func() { close(ended) })
		var arming sync.Once
		armed := func() {
			arming.Do(func() { close(reached) })
			select {
			case <-proceed:
			case <-ended:
			}
		}
		stopGiveUpArmed.Store(&armed)
		t.Cleanup(func() { stopGiveUpArmed.Store(nil) })
		stopped := row.stopUnder(cancelled())
		select {
		case <-reached:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the stop did not arm its give-up within 30 s")
		}
		select {
		case <-row.decided:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the given-up handler did not decide its hand-back within 30 s of the give-up")
		}
		// The keyed runner keeps the key while a run of it is in its handler. Once the first
		// handler returns, the runner starts the message still queued on the key, if the stop left
		// one there, and lets the key go only once that handler has returned too.
		waitFor(t, 30*time.Second, func() bool { return runningKeys(delivery) == 0 }, func() string {
			return fmt.Sprintf("control: %d key(s) still had a handler running 30 s after the given-up handler decided", runningKeys(delivery))
		})
		if entries := row.entries.Load(); entries != 1 {
			t.Fatalf("%d handlers started, want 1: the queued message's handler started during the stop, under a context already ended, so its hand-back was withheld rather than held", entries)
		}
		close(proceed)
		testnats.AnswersWithin(t, 30*time.Second, "StopDelivering", stopped)
		stopGiveUpArmed.Store(nil)
		select {
		case <-delivery.handBacksLanded():
		case <-time.After(30 * time.Second):
			t.Fatal("the stop's hand-back was still waiting for NATS 30 s after the stop returned")
		}
		if err := row.conn.Flush(); err != nil {
			t.Fatalf("flush the intake's client once the stop returned: %v", err)
		}
		if n := naks(); n != 1 {
			t.Fatalf("the server was sent %d hand-back(s), want 1: the queued message's, which the stop held and handed back", n)
		}
		requireBothOutstanding(t, publisher, "once the stop handed the queued message back")
	})
}

// A hand-back outside any stop is sent and reaches the server, even while a write holds nats.go's
// lock past ConnLockWait. The handler waits until the write is held, then calls handBack. The
// connected read that follows the hand-back rule runs only during a stop (mayHandBack), so it
// never blocks the hand-back here, and the publish is left running (callNATS's bound expires).
// Once the lock is released the hand-back reaches the server. The negative control is
// a mayHandBack that makes the connected read outside any stop.
func TestAHandBackOutsideAStopReachesTheServerWhileAWriteHoldsTheLock(t *testing.T) {
	handlerReady := make(chan struct{}, 1)
	writeHeld := make(chan struct{})
	row := startStopRow(t, intakeKey, func(delivery *intakeDelivery, message intakeMessage) {
		select {
		case handlerReady <- struct{}{}:
		default:
		}
		<-writeHeld
		delivery.handBack(message.msg)
	}, true)
	nakCount := watchHandBacks(t, row.uri)
	publishPullRequestEnvelope(t, row.client, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	select {
	case <-handlerReady:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the handler did not reach its wait within 30 s")
	}
	releaseWrite := row.hold.HoldWriteLock(t, row.conn)
	close(writeHeld)
	delivery := row.delivery()
	waitFor(t, 30*time.Second, func() bool { return delivery.handedBack() }, func() string {
		return "the hand-back was withheld while a write held nats.go's lock: a connected read outside any stop blocked it"
	})
	testnats.RequireLockHeld(t, row.conn)
	releaseWrite()
	select {
	case <-delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("the hand-back did not land within 30 s of the lock's release")
	}
	if err := row.conn.Flush(); err != nil {
		t.Fatalf("flush the intake's client: %v", err)
	}
	if n := nakCount(); n == 0 {
		t.Fatal("the hand-back outside any stop did not reach the server after the lock was released: a connected read outside any stop withheld it")
	}
}

// A hand-back outside any stop on a reconnecting link logs at INFO, not ERROR. The bus sets
// ReconnectBufSize -1 (connect.go:267), which disables the reconnect buffer: nats.go returns
// ErrReconnectBufExceeded at once for every publish during a reconnect. The message stays
// outstanding for its ack wait. The negative control is a callNATS without the INFO case for
// ErrReconnectBufExceeded.
func TestAHandBackOnAReconnectingLinkLogsAtInfo(t *testing.T) {
	logs := captureLogs(t)
	handlerReady := make(chan struct{}, 1)
	linkCut := make(chan struct{})
	row := startStopRow(t, intakeKey, func(delivery *intakeDelivery, message intakeMessage) {
		select {
		case handlerReady <- struct{}{}:
		default:
		}
		<-linkCut
		delivery.handBack(message.msg)
	}, true)
	publishPullRequestEnvelope(t, row.client, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	select {
	case <-handlerReady:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the handler did not reach its wait within 30 s")
	}
	row.hold.Cut()
	deadline := time.Now().Add(30 * time.Second)
	for row.conn.Status() != natsgo.RECONNECTING {
		if time.Now().After(deadline) {
			t.Fatalf("control: the connection did not reach RECONNECTING within 30 s: status %v", row.conn.Status())
		}
		time.Sleep(time.Millisecond)
	}
	close(linkCut)
	delivery := row.delivery()
	select {
	case <-delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("the hand-back did not land within 30 s on the reconnecting link")
	}
	// The error is logged on callNATS's call goroutine, after the deferred that closes published
	// runs. Wait for the INFO record itself, with a 30 s guard.
	waitFor(t, 30*time.Second, func() bool {
		for _, record := range logs() {
			msg, _ := record["msg"].(string)
			if strings.Contains(msg, "hand a message back for redelivery") {
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("no log record for the hand-back on a reconnecting link; logged: %v", loggedMessages(logs()))
	})
	for _, record := range logs() {
		msg, _ := record["msg"].(string)
		if !strings.Contains(msg, "hand a message back for redelivery") {
			continue
		}
		level, _ := record["level"].(string)
		if level == "ERROR" {
			t.Fatalf("the hand-back on a reconnecting link logged at ERROR: %v; want INFO for ErrReconnectBufExceeded", record)
		}
		if level != "INFO" {
			t.Fatalf("the hand-back on a reconnecting link logged at %s: %v; want INFO", level, record)
		}
	}
}

// A hand-back's publish count returns to zero even when every callNATS outlives its wait. A
// callNATS that runs its call inside bus.Bounded skips the call when its context has already
// ended; the decrement inside the call never runs, handBacksLanded never closes, and the
// finalizer waits forever. With callNATSWait set to 0, callNATS's timer is ready at once, so a call
// may outlive its wait (select picks between the timer and a fast answer), and the row checks that
// handBacksLanded closes either way. The negative control is such a callNATS (bus.Bounded
// with a context.WithTimeout of callNATSWait).
func TestACallNATSThatOutlivesItsWaitStillCompletesItsBookkeeping(t *testing.T) {
	var first sync.Once
	row := startStopRow(t, intakeKey, func(delivery *intakeDelivery, message intakeMessage) {
		// Only the first delivery is handed back: a hand-back with no delay is redelivered at once,
		// and handing each redelivery back would loop until teardown.
		first.Do(func() { delivery.handBack(message.msg) })
	}, true)
	// Registered after startStopRow, so the wait is restored before the row's teardown runs.
	saved := callNATSWait.Swap(0)
	t.Cleanup(func() { callNATSWait.Store(saved) })
	publishPullRequestEnvelope(t, row.client, "acme", "widgets", 7, map[string]string{
		"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	delivery := row.delivery()
	waitFor(t, 30*time.Second, func() bool { return delivery.handedBack() }, func() string {
		return "the hand-back was never published"
	})
	select {
	case <-delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("handBacksLanded never closed: the publish count was lost because callNATS skipped the call")
	}
}

// startBufferedDrainRow starts a row whose subscription's callback holds the first message of a
// pull request while keying it, so the second stays in the subscription's buffer and a drain cannot
// end. release lets the callback go.
func startBufferedDrainRow(t *testing.T) (*stopRow, func()) {
	t.Helper()
	keyed := make(chan struct{}, 2)
	hold := make(chan struct{})
	var released sync.Once
	release := func() { released.Do(func() { close(hold) }) }
	t.Cleanup(release)
	row := startStopRow(t, func(msg *natsgo.Msg) (string, *contracts.Envelope, bool) {
		select {
		case keyed <- struct{}{}:
		default:
		}
		<-hold
		return intakeKey(msg)
	}, func(*intakeDelivery, intakeMessage) {}, true)
	row.publishPullRequestSeven(t)
	select {
	case <-keyed:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the first message never reached the subscription's callback")
	}
	return row, release
}

// awaitDrainWait waits until the row's stop has dealt with the messages it held, so it is past its
// hand-backs, waiting for its drain to end.
func awaitDrainWait(t *testing.T, row *stopRow) {
	t.Helper()
	waitFor(t, 30*time.Second, func() bool { return stopReleased(row.delivery()) }, func() string {
		return "control: the stop never reached its wait for the drain to end"
	})
}

// queuedBehindKeys is how many messages of delivery wait behind their key's handler.
func queuedBehindKeys(delivery *intakeDelivery) int {
	delivery.keys.mu.Lock()
	defer delivery.keys.mu.Unlock()
	queued := 0
	for _, items := range delivery.keys.queued {
		queued += len(items)
	}
	return queued
}

// runningKeys is how many keys of delivery have a run in its handler: the keyed runner keeps a key
// in queued from the start of its first run until its queue is empty as a run of it returns.
func runningKeys(delivery *intakeDelivery) int {
	delivery.keys.mu.Lock()
	defer delivery.keys.mu.Unlock()
	return len(delivery.keys.queued)
}

// outsideHold connects a client of the test's own to the row's server, outside the hold, so a read
// on it, such as the delivery durable's consumer info, is answered while the row's link is frozen.
func (r *stopRow) outsideHold(t *testing.T) *bus.Client {
	t.Helper()
	client, err := bus.Connect([]string{r.uri})
	if err != nil {
		t.Fatalf("connect NATS at %s outside the hold: %v", r.uri, err)
	}
	t.Cleanup(client.Close)
	return client
}

// requireBothOutstanding fails the test, naming when, unless the delivery durable still holds both
// of the row's messages for redelivery: neither is acknowledged, so the durable's ack floor is
// below the first of them, the first message of the row's stream, and both are still outstanding
// with it (NumAckPending). It reads the durable on publisher, a client outside the hold
// (outsideHold).
func requireBothOutstanding(t *testing.T, publisher *bus.Client, when string) {
	t.Helper()
	stream, err := publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream %s: %v", when, err)
	}
	info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the delivery durable %s: %v", when, err)
	}
	if info.AckFloor.Stream >= stream.State.FirstSeq || info.NumAckPending != 2 {
		t.Fatalf("%s, the durable does not hold both messages for redelivery: want its ack floor below stream sequence %d and 2 messages outstanding, a message was acknowledged or lost: %s",
			when, stream.State.FirstSeq, consumerState(publisher))
	}
}

// stopReleased reports whether delivery's stop has dealt with the messages it held, so it is past
// its hand-backs, waiting for its drain to end.
func stopReleased(delivery *intakeDelivery) bool {
	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	return delivery.released
}
