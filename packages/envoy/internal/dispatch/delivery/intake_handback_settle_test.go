package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A hand-back that a subscription callback begins as the stop ends never reaches the durable a
// standby recreates.
//
// When the stop's context ends while its drain waits, the stop unsubscribes, and an unsubscribe does
// not wait for a callback that is still running. Such a callback can be handing back a message the
// client had buffered (giveBack): past the hand-back rule, and not yet published. The stop seals the
// delivery before it decides to release its hold, and a hand-back is admitted and counted in one step
// with reading the seal (intakeDelivery.seal, admitHandBack), so that hand-back is either counted,
// and the release waits for it, or never published. Counted only once its publish began, it could be
// missed: the stop released its hold, the standby recreated the durable and started the message, and
// the callback's -NAK made the standby's consumer deliver the message a second time.
//
// The row's holder takes the hold and the durable by hand, with a callback that holds the one
// message in its keying. Its stop begins, hands back what it held and waits for its drain, which the
// held callback keeps from ending. The callback is then let go: its message's key has stopped, so it
// hands the message back, and the row holds that hand-back at beforeHandBackPublish, which runs once
// the rule has let it through and before it is admitted. The standby's handler holds every message
// it fetches, so a -NAK that reached its consumer would have it delivered again. Two subtests:
//   - a callback's hand-back that lands before the stop's context ends: the row lets it go before it
//     ends the context, and the hand-back reaches the server before the standby creates the durable.
//   - a callback's hand-back held as the stop's context ends: the stop returns. If its hold is live,
//     the stop counted the hand-back: the row lets it go at once, and it reaches the server before
//     the standby creates the durable. If the hold is gone, the row lets the hand-back go only once
//     the standby has started the message, and waits until the hand-back reaches the server or the
//     holder logs it withheld (handBackSealed). A -NAK after the standby's creation fails the row,
//     naming how many times the standby's consumer delivered the message; a withheld hand-back
//     leaves the message to the standby alone.
//
// Each subtest ends once the standby has acknowledged the message, by requiring that no -NAK reached
// the server after the standby created the durable and that its consumer delivered the message once
// (requireNoHandBackAfterCreation). No step waits on a time: each waits for an event, with a 30 s or
// 60 s guard against one that never comes. The negative controls: the code before the seal, where the
// hand-back was counted after beforeHandBackPublish, and a stop that does not seal, each fail the
// second subtest with the standby's consumer delivering the message twice.
func TestACallbacksHandBackAsTheStopEndsLandsBeforeTheStandbyTakesOver(t *testing.T) {
	// Serial until the hang a reviewer saw once in requireHandBackBeforeCreation is explained
	// (build-cluster Job legion-ci-handback-hang-4zwgk reproduces it at main).
	t.Run("a callback's hand-back that lands before the stop's context ends", func(t *testing.T) {
		callbackHandBackAsTheStopEnds(t, false)
	})
	t.Run("a callback's hand-back held as the stop's context ends", func(t *testing.T) {
		callbackHandBackAsTheStopEnds(t, true)
	})
}

// callbackHandBackAsTheStopEnds is one subtest of the row above: heldPastTheStop holds the callback's
// hand-back until the stop has returned.
func callbackHandBackAsTheStopEnds(t *testing.T, heldPastTheStop bool) {
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
	wanted := deliveryConsumerConfig(tuningFor(t))
	durable := newHeldDurable(tuningFor(t), client, deliveryTestLeases(t, pool), func(*intakeTuning) natsgo.ConsumerConfig { return wanted },
		func(msg *natsgo.Msg) (string, *contracts.Envelope, bool) {
			entered.Do(func() { close(keying) })
			<-letKeyingGo
			return intakeKey(msg)
		}, func(*intakeDelivery, intakeMessage) {})
	if !durable.takeHold(ctx) {
		t.Fatal("control: the holder did not take the delivery durable's hold")
	}
	if _, err := durable.subscribe(wanted.DeliverSubject); err != nil {
		t.Fatalf("subscribe the holder at %s: %v", wanted.DeliverSubject, err)
	}
	if _, err := client.JS().AddConsumer(bus.Stream, &wanted); err != nil {
		t.Fatalf("create the delivery durable: %v", err)
	}
	token := intakeHoldToken{subject: wanted.DeliverSubject}
	if err := durable.hold.SetToken(ctx, token.String()); err != nil {
		t.Fatalf("record the holder's deliver subject: %v", err)
	}
	durable.recorded, durable.subject = token, wanted.DeliverSubject
	delivery := durable.delivering
	// Registered before the hand-back's release, so that release runs first: a finalizer waits for
	// the hand-back to land.
	t.Cleanup(durable.waitFinalizers)
	t.Cleanup(func() { durable.stop(cancelled()) })

	standby := startIntakeProcess(t, pool, natsURL, func(int) bool { return true })
	from := recorded.count()
	sequences := []uint64{publishMergedPullRequest(t, client, number)}
	select {
	case <-keying:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the holder's callback never began keying the message: " + consumerState(client))
	}

	atPublish := make(chan struct{})
	var reached sync.Once
	letPublishGo := make(chan struct{})
	releasePublish := sync.OnceFunc(func() { close(letPublishGo) })
	t.Cleanup(releasePublish)
	hook := func() {
		reached.Do(func() { close(atPublish) })
		<-letPublishGo
	}
	delivery.beforeHandBackPublish.Store(&hook)

	stopping, endStop := context.WithCancel(context.Background())
	defer endStop()
	stopped := make(chan struct{})
	go func() {
		durable.stop(stopping)
		close(stopped)
	}()
	waitFor(t, 30*time.Second, func() bool { return stopReleased(delivery) }, func() string {
		return "control: the holder's stop never reached its wait for the drain to end"
	})
	releaseKeying()
	select {
	case <-atPublish:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the callback's hand-back never reached its publish: the rule withheld it")
	}
	if !heldPastTheStop {
		releasePublish()
		select {
		case <-delivery.handBacksLanded():
		case <-time.After(30 * time.Second):
			t.Fatal("control: the callback's hand-back did not land within 30 s of its release")
		}
	}
	endStop()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the holder's stop did not return within 30 s of its context's end")
	}

	if heldPastTheStop && !holdLive(t, pool) {
		// The stop released its hold with the callback's hand-back still to be decided: the standby
		// takes the durable over and starts the message, and only then does the hand-back go on.
		waitFor(t, 30*time.Second, func() bool { return standby.fetches(number) == 1 }, func() string {
			return "control: the standby did not start the message the holder left: " + consumerState(client)
		})
		releasePublish()
		waitFor(t, 30*time.Second, func() bool {
			return handBackAfterCreation(recorded.since(from)) || loggedMessage(logs(), handBackSealed)
		}, func() string {
			return fmt.Sprintf("the callback's hand-back neither reached the server nor was withheld within 30 s of its release: %q; logged %q", recorded.since(from), loggedMessages(logs()))
		})
		if handBackAfterCreation(recorded.since(from)) {
			delivered := uint64(0)
			testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool {
				info, err := client.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
				if err == nil {
					delivered = info.Delivered.Consumer
				}
				return delivered >= 2
			})
			t.Fatalf("the stop released its hold before the callback's hand-back was decided, and the hand-back went on: the -NAK reached the server after the standby created the durable, and the standby's consumer delivered the one message %d time(s); events %q",
				delivered, recorded.since(from))
		}
		standby.release()
		requireNoHandBackAfterCreation(t, recorded, from, client, sequences)
		return
	}
	releasePublish()
	waitFor(t, 60*time.Second, func() bool { return standby.fetches(number) >= 1 }, func() string {
		return fmt.Sprintf("the standby did not start the message the holder left: %s; events %q", consumerState(client), recorded.since(from))
	})
	standby.release()
	requireHandBackBeforeCreation(t, recorded, from, client, sequences)
}

// A hand-back still pending when the holder's hold is lost never reaches the durable a standby
// recreates.
//
// A stop whose hand-back is pending at its deadline returns with the hold renewing on a goroutine of
// its own until that hand-back lands (heldDurable.finalizeHandBacks). A hold whose renewals then
// fail, as when the database is out of reach, is lost intakeHoldMargin before its row lapses, and
// the standby takes the durable over once it has. The hand-back, waiting behind a write that holds
// nats.go's lock, would go out once that write ends, onto the standby's consumer; so the lost hold
// severs the connection that carries it, which ends the write and drops the hand-back
// (heldDurable.severHandBacks, bus.Client.Sever).
//
// Both subtests hold a write on the holder's link from inside its stop's hand-back
// (stopWithAHeldHandBack) and wait for the stop to return with the hand-back pending and the hold
// live. Two subtests:
//   - a hand-back pending while the hold renews (the control): the hold renews past the expiry it had
//     then, the link moves, and the hand-back reaches the server before the standby creates the
//     durable.
//   - a hand-back pending when the hold's renewals fail: the row refuses the holder's every database
//     statement (cuttableLeases), waits until the standby has created the durable, and only then lets
//     the link move. It then waits until the hand-back's publish has returned, flushes the holder's
//     connection unless it has closed, so the server has handled whatever that connection sent, and
//     takes a barrier of its watch (durableEvents.barrier), so the watch has recorded it. No -NAK
//     reached the server after the creation; a hand-back dropped with the severed connection never
//     reaches it, which is allowed (requireNoHandBackAfterCreation). The standby acknowledges the
//     message as soon as it is delivered, so a late -NAK usually finds it acknowledged and changes
//     no count; the order is what shows it.
//
// Each ends by requiring that the standby's consumer delivered each message the holder left once.
// No step waits on a time but the hold's own expiry; a 60 s guard covers a standby that never
// takes over. The negative control is the finalizer before the sever, which returned at the lost
// hold and left the hand-back pending: the second subtest fails there with the -NAK after the
// creation.
func TestAHandBackPendingWhenTheHoldIsLostNeverReachesTheStandbysDurable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		lose bool
	}{
		{name: "a hand-back pending while the hold renews"},
		{name: "a hand-back pending when the hold's renewals fail", lose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := stopWithAHeldHandBack(t)
			select {
			case <-row.stopped:
			case <-time.After(30 * time.Second):
				t.Fatal("the holder's Run did not return within 30 s while its hand-back was held")
			}
			if !row.handBackPending() {
				t.Fatal("control: the stop's hand-back reached NATS while the write was held")
			}
			if !holdLive(t, row.pool) {
				t.Fatal("control: the stop released its hold with its hand-back pending")
			}
			if !tc.lose {
				if !holdRenewedBeforeItsExpiry(t, row.pool) {
					t.Fatal("control: the hold reached its expiry unrenewed while its hand-back was pending and its database reachable")
				}
				row.thaw()
				requireHandBackBeforeCreation(t, row.recorded, row.from, row.publisher, row.sequences)
				return
			}
			row.holder.leases.cut.Store(true)
			waitFor(t, 60*time.Second, func() bool { return slices.Contains(row.recorded.since(row.from), durableCreated) }, func() string {
				return fmt.Sprintf("the standby did not take over the lapsed hold and create the durable: %q; %s", row.recorded.since(row.from), consumerState(row.publisher))
			})
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
			requireNoHandBackAfterCreation(t, row.recorded, row.from, row.publisher, row.sequences)
		})
	}
}

// A stop under a context that has already ended holds the message waiting behind its key, and
// hands it back once its drain has answered, rather than letting it start its handler under the
// cancelled context.
//
// The stop's give-up cancels the handlers' context, and under an ended context it runs at once. The
// stop took the messages waiting behind their keys only after it had armed the give-up: a stop
// descheduled between the two let the given-up handler return and the keyed runner start the
// message behind it under the cancelled context. That handler's hand-back, like the first's, came
// before the drain had answered, so the stop's rule withheld both, and the stop held nothing and
// handed nothing back: the message waited for the take-over or the ack wait (intakeDelivery.stop).
//
// The row's handler does what Intake.deliver does with a given-up message: it waits for its context
// to end and hands the message back. Two messages of one pull request are published, so the first is
// in its handler and the second waits behind its key, on a link that moves. One stop runs under an
// ended context, and once its give-up is armed (intakeTuning.stopArmed) it waits until the key has no
// handler goroutine left: the first handler has returned, and the runner has either started the
// second or found its queue taken. Then the row requires that one handler started, and, once every
// hand-back has landed and the client has flushed, that the server was sent one hand-back: the
// second message's, since the first's came before the drain answered. The negative control is the
// stop that takes the queue after arming the give-up: two handlers start and the server is sent no
// hand-back.
func TestAStopUnderAnEndedContextHoldsTheMessageWaitingBehindItsKey(t *testing.T) {
	var started atomic.Int32
	row := startStopRow(t, intakeKey, func(delivery *intakeDelivery, message intakeMessage) {
		started.Add(1)
		<-delivery.handling.Done()
		delivery.handBack(slog.Default(), message.msg)
	}, true)
	naks := watchHandBacks(t, row.uri)
	delivery := row.delivery()
	row.publishPullRequestSeven(t)
	waitFor(t, 30*time.Second, func() bool { return started.Load() == 1 && queuedBehindKeys(delivery) == 1 }, func() string {
		return fmt.Sprintf("control: %d handler(s) started and %d message(s) wait behind their key, want the first in its handler and the second behind it", started.Load(), queuedBehindKeys(delivery))
	})

	keyIdle := func() bool {
		delivery.keys.mu.Lock()
		defer delivery.keys.mu.Unlock()
		return len(delivery.keys.queued) == 0
	}
	// Every wait and failure is on the row's goroutine; ended lets a stop still held in the hook go
	// on when the row ends early.
	idle := make(chan struct{})
	ended := make(chan struct{})
	t.Cleanup(func() { close(ended) })
	var waiting sync.Once
	armed := func() {
		waiting.Do(func() {
			for !keyIdle() {
				select {
				case <-ended:
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			close(idle)
		})
	}
	delivery.tuning.stopArmed.Store(&armed)
	stopped := row.stopUnder(cancelled())
	select {
	case <-idle:
	case <-time.After(30 * time.Second):
		t.Fatal("control: the given-up handler's key still had a handler goroutine 30 s after the give-up was armed")
	}
	testnats.AnswersWithin(t, 30*time.Second, "StopDelivering", stopped)
	delivery.tuning.stopArmed.Store(nil)
	if n := started.Load(); n != 1 {
		t.Fatalf("%d handlers started: the message waiting behind its key started its handler under the stop's ended context, rather than being held by the stop", n)
	}
	select {
	case <-delivery.handBacksLanded():
	case <-time.After(30 * time.Second):
		t.Fatal("a hand-back publish was still waiting for NATS 30 s after the stop returned")
	}
	if err := row.conn.Flush(); err != nil {
		t.Fatalf("flush the intake's client: %v", err)
	}
	if n := naks(); n != 1 {
		t.Fatalf("the server was sent %d hand-back(s), want 1: the second message's, which the stop held and handed back once its drain answered", n)
	}
}
