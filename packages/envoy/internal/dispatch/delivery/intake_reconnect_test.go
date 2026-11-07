package delivery

// What a process that held the delivery durable does once its NATS connection comes back: the
// durable belongs to whichever process took it over while it was away, and the one that came back
// is never a second bound subscriber beside it (INFRA-1551, R1). One row drives each of the two
// paths a connection comes back by -- a reconnect in place, where nats.go re-sends the
// subscription itself, and a replaced connection, where the bus client binds its own registration
// again (internal/bus/recovery.go).
//
// A third row holds the give-up to account for the work it had in flight (R2): the message in the
// handler it abandons reaches the process that keeps the durable, and inside a bound well under
// the ack wait that message would otherwise have waited out in that process's window.
//
// A fourth row holds the other half of that accounting: a handler that finishes while the link
// is down -- the ordinary outage, since the give-up abandons a handler only once its budget has
// ended -- has its message acknowledged once the link is back, so the process that keeps the
// durable is never handed work that was already done.
//
// A fifth row drives that acknowledgement on the other of the two paths a connection comes back
// by: a holder whose client closed the connection the message arrived on and dialled a
// replacement. The answer cannot go out on the message's own subscription there -- that
// connection is closed and refuses every publish -- so it is published over the connection the
// client holds then (intake_delivery.go's answerHeld).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A process whose NATS connection drops and reconnects in place comes back to a durable the
// process standing by took over while it was away, and leaves it there: the one pull request
// envelope published afterwards is handled once, by the process that took the durable over, the
// server reports one subscriber on the durable's deliver subject, the returned process says it
// gave the durable up, and it holds no delivery subscription of its own.
//
// The drop is a cut link, restored while the holder's client is still dialling the same URL
// through it, so nats.go reconnects that client in place and re-sends its SUB inside its own
// reconnect, under no lease and with nothing asking whether the durable is still this process's
// to deliver (the bus leaves a subscription a reconnect in place kept valid alone,
// restoreSubscriptions). That is the first of the defect's two paths.
//
// The assertions are what they are because ConsumerInfo.PushBound is one bool for one subscriber
// and for two: it says somebody is bound, never how many deliver. So the row reads the fan-out
// instead -- one envelope, counted per process on a fake GitHub of that process's own -- and the
// server's own count of the subscribers on the deliver subject, and reads PushBound only to keep
// a run where nobody is bound from passing.
func TestAHolderBackFromAReconnectInPlaceLeavesTheDurableToTheProcessThatTookIt(t *testing.T) {
	holderBackFromAConnectionEvent(t, func(_ *bus.Client, proxy *testnats.Proxy, _ func()) {
		proxy.Cut()
	}, 30*time.Second)
}

// A process whose connection was replaced comes back to a durable another process took over, and
// leaves it there, exactly as the row above holds for a reconnect in place.
//
// This row drives the defect's second path: the link is cut, the process standing by takes the
// durable over, and only then is the holder's connection closed, so its client dials a
// replacement and binds its own registration again (internal/bus/recovery.go's
// restoreSubscriptions) rather than having nats.go re-send it. That redial backs off from one
// second, so the holder's client is given 60 s to be connected again once the link is back.
//
// The assertions are the row above's, and for the row above's reason: PushBound is one bool
// however many subscribers deliver, so what says the two processes did not both deliver is the
// fan-out of one envelope counted per process, with the server's own subscriber count on the
// deliver subject beside it.
func TestAHolderWhoseConnectionWasReplacedLeavesTheDurableToTheProcessThatTookIt(t *testing.T) {
	holderBackFromAConnectionEvent(t, func(holderClient *bus.Client, proxy *testnats.Proxy, awaitKeeper func()) {
		proxy.Cut()
		// The hand-over first: a connection replaced before the standby has the durable would be
		// dialled while the durable is free, which is not the state this row is about.
		awaitKeeper()
		holderClient.Connection().Close()
	}, 60*time.Second)
}

// A message the process holding the delivery durable had in flight, inside its handler, when a
// connection event took the durable away from it is handled by the process that keeps the
// durable, and soon (INFRA-1551, R2). The handler the give-up abandons hands its message to the
// delivery rather than acknowledging it itself (Intake.deliver), the delivery holds it until the
// stop has a live connection to hand it back over (intake_delivery.go's stop,
// awaitLiveConnection), and the keeper is handed it then.
//
// What A2 is read as here: JetStream is at-least-once, and every write this package makes is an
// upsert the reconcile makes too, so a redelivery is a no-op rather than a duplicate (intake.go's
// package comment). What this row decides is therefore one handling by the keeper within the
// bound it states, and it says nothing about how many times NATS delivered the message.
//
// The bound is what makes the row a proof rather than a restatement of what NATS does anyway. A
// message nobody hands back is redelivered when intakeAckWait() expires -- intakeMaxAckPending
// (4) times intakeMessageTimeout (1 min) plus a minute, five minutes here -- and it holds one of
// the keeper's four in-flight slots for every second of it, which is longer than the five-minute
// reconcile this intake exists to beat. A handling well inside that wait is the hand-back's.
//
// R2 is this durable's alone: the job intake acknowledges each message as it receives it (jobs.go's
// deliver), so it has nothing in flight to hand back.
func TestAMessageInFlightWhenTheDurableIsGivenUpReachesTheProcessThatKeepsIt(t *testing.T) {
	// The give-up's own budget, and nothing else: the stop a connection event runs waits under it
	// for the handler it abandons and for a live connection to hand that handler's message back
	// over, so 10 s rather than 15 s takes 5 s off this row and changes nothing it measures.
	//
	// What the row does not shrink, and cannot, is the ack wait it is measured against:
	// Intake.bind stamps deliveryConsumerConfig() on the durable, whose AckWait is
	// intakeAckWait(), and that has a one-minute floor whatever a row does to its inputs -- five
	// minutes here, since the row leaves intakeMessageTimeout and intakeMaxAckPending as they
	// are. The durable this row runs against keeps its five-minute ack wait, which is what makes
	// a handling inside the bound below the hand-back rather than a redelivery the server would
	// have made of its own accord.
	//
	// Restored in a cleanup registered before the intakes are started, so the restore runs after
	// both Runs have returned: cleanups run last registered first, and runIntake's own cleanup
	// waits for Run, whose stop reads the budget.
	budget := intakeStopBudget
	intakeStopBudget = 10 * time.Second
	t.Cleanup(func() { intakeStopBudget = budget })

	processes := startConnectionEventProcesses(t, holderHoldsItsFirstFetch)
	// Registered after the intakes are started, so it runs before their stops do, and here
	// rather than at the end of the row, so a row that fails leaves no request held.
	t.Cleanup(processes.holderHeld.release)

	// One envelope while the holder is the only bound process, and nothing done until its
	// handler is inside its GitHub fetch: the message is in flight, in a handler, when the
	// durable is given up below.
	publishPullRequestEnvelope(t, processes.keeperClient, "acme", "widgets", 1, mergedPullRequest(1))
	processes.holderHeld.awaitEntered(t)

	// The cut is the connection event: the holder gives its subscription up at the disconnect,
	// under the 10 s budget, with its handler still in that fetch.
	processes.proxy.Cut()
	processes.awaitKeeper(t)
	processes.proxy.Restore()
	awaitHolderConnectedAgain(t, processes.holderClient, processes.hooked, 60*time.Second)

	// 60 s: the give-up's 10 s budget, which is how long the stop waits for the handler it
	// abandons, plus the hand-back and the server's redelivery of what was handed back -- and a
	// fifth of the five-minute ack wait the row cannot change. A keeper that fetches the pull
	// request inside it was handed the message; one that has not by then is waiting those five
	// minutes out with one of its four in-flight slots held.
	waitFor(t, 60*time.Second, func() bool {
		return processes.keeperFetches.Load() >= 1
	}, func() string {
		return fmt.Sprintf("the process that keeps the durable never handled the message the other had in flight: it fetched the pull request %d time(s), the process that gave the durable up %d time(s), and the server reports the durable bound = %t",
			processes.keeperFetches.Load(), processes.holderFetches.Load(), deliveryDurableBound(processes.keeperClient))
	})
	// A second of quiet before the count is read, so a second handling of that one message --
	// which the server hands a bound subscriber within moments of deciding to -- is counted
	// rather than missed.
	time.Sleep(time.Second)
	if keeperHandled := processes.keeperFetches.Load(); keeperHandled != 1 {
		t.Errorf("the process that keeps the durable fetched the pull request %d time(s), want 1: the message the other had in flight is handled once there",
			keeperHandled)
	}
	// The holder's one fetch is the attempt the give-up abandoned, so the row says plainly that
	// the work was left there and redone by the keeper rather than finished by both.
	if holderHandled := processes.holderFetches.Load(); holderHandled != 1 {
		t.Errorf("the process that gave the durable up fetched the pull request %d time(s), want the one attempt the give-up abandoned",
			holderHandled)
	}
}

// A handler that finishes while its process is cut off from NATS has its message acknowledged
// once the link is back, and the process that took the durable over is never handed it
// (INFRA-1551, R2). This is the ordinary case of an outage, and the half the row above does not
// drive: the give-up abandons a handler only when its budget ends, so a handling of about a
// second finishes inside the outage, with the stop already under way. Its acknowledgement is a
// publish exactly as a hand-back is, and the bus buffers none while the client reconnects
// (opts.ReconnectBufSize = -1, internal/bus/connect.go), so the handler's own acknowledgement is
// refused, the delivery holds it with what it hands back, and the stop makes it once the link is
// back (acknowledgeOrHold, intake_delivery.go's stop).
//
// An acknowledgement made then reaches the server although the subscription it arrived on has
// been unsubscribed: it is published at the message's own reply subject, which belongs to the
// consumer, and nothing about the subscription is read (intake_delivery.go's answerHeld). The
// holder here reconnects in place, so the connection it is published over is the one the
// message arrived on; the row below drives the holder whose connection was replaced, where it
// is not.
//
// What the row reads is the durable's own outstanding count (NumAckPending, deliveryAckPending).
// 0 says the server took the acknowledgement and the keeper's four in-flight slots are free
// again; a handler that acknowledged for itself while the link was down leaves it at 1 until the
// ack wait expires, which is what this row fails on.
//
// The 60 s bound is a fifth of that ack wait, which the row cannot change: Intake.bind stamps
// deliveryConsumerConfig() on the durable, whose AckWait is intakeAckWait(), and that has a
// one-minute floor whatever a row does to its inputs -- five minutes here, since the row leaves
// intakeMessageTimeout and intakeMaxAckPending as they are. It is far longer than the stop needs
// (the acknowledgement goes out as the client reports connected, inside the give-up's budget)
// and far shorter than the wait a refused acknowledgement leaves, so neither a slow pod nor the
// server's own redelivery decides the row.
func TestAHandlerThatFinishesDuringTheOutageIsAcknowledgedWhenTheLinkIsBack(t *testing.T) {
	// The give-up's own budget, and restored in a cleanup registered before the intakes are
	// started, so the restore runs after both Runs have returned. Here it bounds the stop's wait
	// for a live connection rather than its wait for a handler, which the row releases itself:
	// the budget has to cover the cut, the keeper's bind (a durableBindInterval poll, 2 s, plus
	// the consumer lease and the bind confirmation), the restore and nats.go's one-second
	// reconnect, and a budget that runs out instead fails the row as "never acknowledged", which
	// names the wrong cause. 30 s rather than the 15 s of the binary because this pod runs the
	// row in 4 to 6 s and a loaded CI runner has to fit in too; a larger budget costs the row
	// nothing, since the handler is released early and the budget only bounds waits that end
	// when the link is back, inside the row's own 60 s bound.
	budget := intakeStopBudget
	intakeStopBudget = 30 * time.Second
	t.Cleanup(func() { intakeStopBudget = budget })

	processes := startConnectionEventProcesses(t, holderHoldsItsFirstFetch)
	// The row releases the hold itself; this is for a row that fails before it gets there, and
	// is registered after the intakes are started so it runs before their stops do.
	t.Cleanup(processes.holderHeld.release)

	// One envelope while the holder is the only bound process, and nothing done until its
	// handler is inside its GitHub fetch.
	publishPullRequestEnvelope(t, processes.keeperClient, "acme", "widgets", 1, mergedPullRequest(1))
	processes.holderHeld.awaitEntered(t)

	// The cut is the connection event: the holder's stop begins at it, with the handler still in
	// that fetch.
	processes.proxy.Cut()
	// The hold is released only once that stop has ended the subscription, which it does before
	// it waits for the handlers. From there the handler's acknowledgement is one the stop holds
	// rather than one the handler makes for itself over the dead link, which is the case this
	// row is about and the one the row above cannot reach.
	waitFor(t, 30*time.Second, func() bool {
		return !intakeHoldsSubscription(processes.holder)
	}, func() string {
		return fmt.Sprintf("the process cut off from NATS never gave its delivery subscription up: it holds a live one = %t, the standby holds one = %t",
			intakeHoldsSubscription(processes.holder), intakeHoldsSubscription(processes.keeper))
	})
	processes.holderHeld.release()

	processes.awaitKeeper(t)
	// Read before the restore, so the row knows the acknowledgement it waits for below is the
	// stop's own: while the link is down nothing has reached the server for the message whose
	// handler has just finished.
	if pending := deliveryAckPending(processes.keeperClient); pending != 1 {
		t.Fatalf("the durable has %d message(s) outstanding while the holder is cut off, want the 1 its finished handler is holding (-1 reads as a consumer this test could not read)",
			pending)
	}
	processes.proxy.Restore()
	awaitHolderConnectedAgain(t, processes.holderClient, processes.hooked, 60*time.Second)

	waitFor(t, 60*time.Second, func() bool {
		return deliveryAckPending(processes.keeperClient) == 0
	}, func() string {
		return fmt.Sprintf("the durable still has %d message(s) outstanding %s after the link came back: the handling that finished during the outage was never acknowledged, so it holds one of the keeper's %d in-flight slots until the %s ack wait expires; the keeper fetched the pull request %d time(s) and the process that gave the durable up %d time(s)",
			deliveryAckPending(processes.keeperClient), 60*time.Second, intakeMaxAckPending, intakeAckWait(),
			processes.keeperFetches.Load(), processes.holderFetches.Load())
	})
	// A second of quiet before the keeper's count is read, so a delivery to it -- which the
	// server hands a bound subscriber within moments of deciding to -- is counted rather than
	// missed.
	time.Sleep(time.Second)
	if keeperHandled := processes.keeperFetches.Load(); keeperHandled != 0 {
		t.Errorf("the process that keeps the durable fetched the pull request %d time(s), want 0: the other process finished that handling, and its acknowledgement is what keeps this one from redoing it",
			keeperHandled)
	}
	// The holder's one fetch is the handling that finished inside the outage: the work was done
	// there, once, and neither given back nor redone.
	if holderHandled := processes.holderFetches.Load(); holderHandled != 1 {
		t.Errorf("the process that gave the durable up fetched the pull request %d time(s), want the one handling it finished during the outage",
			holderHandled)
	}
}

// A handler that finishes once its client has closed the connection its message arrived on and
// dialled a replacement has that message acknowledged all the same, and the process that took
// the durable over is never handed it (INFRA-1551, R2). This is the row above's measurement on
// the second of the two paths a connection comes back by, and the one the row above cannot
// reach: there the holder reconnects in place, so the message's own subscription carries a live
// connection again and Msg.Ack would answer it.
//
// Here it would not. Msg.Ack and Msg.Nak publish on the connection of the message's own
// subscription (sub.conn, nats.go's js.go:3608-3610), which the client closed; a publish on it
// is refused with ErrConnectionClosed however long the stop waits, and the message would wait
// out the five-minute ack wait in the keeper's four-slot window although its handling was done.
// What answers it is the stop publishing at the message's reply subject -- the consumer's, not
// this subscription's -- over the connection the client holds then (intake_delivery.go's
// answerHeld). Sent through Msg.Ack instead, this row fails with the message still outstanding.
//
// The order of the three steps is what puts the row on that path. The cut is the connection
// event the stop begins at; the keeper takes the durable over before the connection is closed,
// as the replaced-connection row above does it, since a connection replaced while the durable
// is free is not the state this row is about; and the hold is released last, so the handler
// finishes with its connection closed and its own acknowledgement refused there.
//
// What the row reads is the row above's: the durable's outstanding count (NumAckPending,
// deliveryAckPending), 0 within 60 s, which is a fifth of the ack wait the row cannot change,
// and the keeper's fetch count, which stays 0 because the work was done here.
func TestAHandlerThatFinishesWhenTheHoldersConnectionWasReplacedIsAcknowledgedAllTheSame(t *testing.T) {
	// The give-up's own budget, raised and restored as the row above does it and for its
	// reasons, with one more wait to cover here: the client's redial of the connection this row
	// closes, which backs off from a second (internal/bus/recovery.go's recoverUntilHealthy). A
	// larger budget still costs the row nothing -- the handler is released early, so the budget
	// only bounds waits that end when the client is connected again -- and a budget that ran out
	// would fail the row as "never acknowledged", which names the wrong cause.
	budget := intakeStopBudget
	intakeStopBudget = 30 * time.Second
	t.Cleanup(func() { intakeStopBudget = budget })

	processes := startConnectionEventProcesses(t, holderHoldsItsFirstFetch)
	// The row releases the hold itself; this is for a row that fails before it gets there, and
	// is registered after the intakes are started so it runs before their stops do.
	t.Cleanup(processes.holderHeld.release)

	// One envelope while the holder is the only bound process, and nothing done until its
	// handler is inside its GitHub fetch.
	publishPullRequestEnvelope(t, processes.keeperClient, "acme", "widgets", 1, mergedPullRequest(1))
	processes.holderHeld.awaitEntered(t)

	// The cut is the connection event: the holder's stop begins at it, with the handler still in
	// that fetch.
	processes.proxy.Cut()
	waitFor(t, 30*time.Second, func() bool {
		return !intakeHoldsSubscription(processes.holder)
	}, func() string {
		return fmt.Sprintf("the process cut off from NATS never gave its delivery subscription up: it holds a live one = %t, the standby holds one = %t",
			intakeHoldsSubscription(processes.holder), intakeHoldsSubscription(processes.keeper))
	})
	processes.awaitKeeper(t)
	// The close is this row's path: the client cannot reconnect this connection in place any
	// more, so it dials a replacement, and the connection the stop answers over is not the one
	// the message arrived on.
	processes.holderClient.Connection().Close()
	processes.holderHeld.release()

	// Read before the restore, so the row knows the acknowledgement it waits for below is the
	// stop's own: while the link is down nothing has reached the server for this message.
	if pending := deliveryAckPending(processes.keeperClient); pending != 1 {
		t.Fatalf("the durable has %d message(s) outstanding while the holder is cut off, want the 1 its finished handler is holding (-1 reads as a consumer this test could not read)",
			pending)
	}
	processes.proxy.Restore()
	// 60 s, as the replaced-connection row above gives it: the client dials its replacement
	// rather than having nats.go reconnect in place, and that dial backs off from a second.
	awaitHolderConnectedAgain(t, processes.holderClient, processes.hooked, 60*time.Second)

	waitFor(t, 60*time.Second, func() bool {
		return deliveryAckPending(processes.keeperClient) == 0
	}, func() string {
		return fmt.Sprintf("the durable still has %d message(s) outstanding %s after the link came back: the handling that finished once the holder's connection had been replaced was never acknowledged -- an answer made on the closed connection the message arrived on is refused -- so it holds one of the keeper's %d in-flight slots until the %s ack wait expires; the keeper fetched the pull request %d time(s) and the process that gave the durable up %d time(s)",
			deliveryAckPending(processes.keeperClient), 60*time.Second, intakeMaxAckPending, intakeAckWait(),
			processes.keeperFetches.Load(), processes.holderFetches.Load())
	})
	// A second of quiet before the keeper's count is read, so a delivery to it -- which the
	// server hands a bound subscriber within moments of deciding to -- is counted rather than
	// missed.
	time.Sleep(time.Second)
	if keeperHandled := processes.keeperFetches.Load(); keeperHandled != 0 {
		t.Errorf("the process that keeps the durable fetched the pull request %d time(s), want 0: the other process finished that handling, and its acknowledgement is what keeps this one from redoing it",
			keeperHandled)
	}
	// The holder's one fetch is the handling that finished while its connection was being
	// replaced: the work was done there, once, and neither given back nor redone.
	if holderHandled := processes.holderFetches.Load(); holderHandled != 1 {
		t.Errorf("the process that gave the durable up fetched the pull request %d time(s), want the one handling it finished while its connection was replaced",
			holderHandled)
	}
}

// holderBackFromAConnectionEvent runs the sequence the first two rows of this file share, on the
// startConnectionEventProcesses leaves bound and standing by. drop is the row's own way of taking
// the holder's connection away, and is handed awaitKeeper, the wait for the keeper to have taken
// the durable over, for a drop whose second step needs the hand-over to have happened. backWithin
// is how long the holder's client may take to be connected again once the link is restored.
func holderBackFromAConnectionEvent(t *testing.T, drop func(holderClient *bus.Client, proxy *testnats.Proxy, awaitKeeper func()), backWithin time.Duration) {
	processes := startConnectionEventProcesses(t, holderCountsItsFetches)
	server, logs := processes.server, processes.logs
	holder, keeper := processes.holder, processes.keeper
	holderClient, keeperClient := processes.holderClient, processes.keeperClient
	holderFetches, keeperFetches := processes.holderFetches, processes.keeperFetches

	awaitKeeper := func() { processes.awaitKeeper(t) }
	drop(holderClient, processes.proxy, awaitKeeper)
	awaitKeeper()

	processes.proxy.Restore()
	awaitHolderConnectedAgain(t, holderClient, processes.hooked, backWithin)
	// A round trip on the returned process's own connection, so the server has processed
	// everything that connection sent on its way back -- the SUB nats.go re-sends inside its
	// reconnect among it -- before the envelope is published on the other process's connection.
	// Without it the row could publish into a server that had not yet read that SUB, and read a
	// fan-out that had not happened yet.
	if err := holderClient.Connection().FlushTimeout(5 * time.Second); err != nil {
		t.Logf("the returned process's connection did not answer a flush: %v", err)
	}

	publishPullRequestEnvelope(t, keeperClient, "acme", "widgets", 11, mergedPullRequest(11))
	waitFor(t, 30*time.Second, func() bool {
		return holderFetches.Load()+keeperFetches.Load() >= 1
	}, func() string {
		return fmt.Sprintf("neither process handled the pull request envelope: the returned process fetched it %d time(s), the process that took the durable over %d time(s), and the server reports the durable bound = %t",
			holderFetches.Load(), keeperFetches.Load(), deliveryDurableBound(keeperClient))
	})
	// The server hands every subscriber of a deliver subject the same message at the same moment,
	// so a second after the first handling is long enough for a second process's handling of that
	// one envelope to have landed and been counted.
	time.Sleep(time.Second)

	// The fan-out first, so it is measured and reported before anything else can stop the row:
	// it is the negative control of this whole change.
	holderHandled, keeperHandled := holderFetches.Load(), keeperFetches.Load()
	switch {
	case holderHandled == 1 && keeperHandled == 1:
		t.Errorf("both processes handled the one pull request envelope: the process whose connection came back fetched it %d time(s) and the process that took the durable over %d time(s), want 0 and 1",
			holderHandled, keeperHandled)
	case holderHandled != 0 || keeperHandled != 1:
		t.Errorf("the one pull request envelope was fetched %d time(s) by the process whose connection came back and %d time(s) by the process that took the durable over, want 0 and 1",
			holderHandled, keeperHandled)
	}

	deliverSubject := deliveryConsumerInfo(t, keeperClient).Config.DeliverSubject
	if subscribers := deliverSubjectSubscribers(t, server, deliverSubject); subscribers != 1 {
		t.Errorf("the server reports %d subscriber(s) on the durable's deliver subject %q, want 1", subscribers, deliverSubject)
	}

	if info := deliveryConsumerInfo(t, keeperClient); !info.PushBound {
		t.Errorf("the server reports the durable %s bound to no subscriber: delivered %d, ack floor %d, waiting %d",
			bus.DispatchDeliveryDurable, info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending)
	}

	// Only the process whose connection came back can log this: the other had no connection
	// event.
	if !loggedMessage(logs(), intakeConnectionChangedMessage) {
		t.Errorf("no process logged %q, so none gave the delivery durable up on its connection event; the messages logged were %q",
			intakeConnectionChangedMessage, loggedMessages(logs()))
	}

	holderDelivering, keeperDelivering := intakeHoldsSubscription(holder), intakeHoldsSubscription(keeper)
	if holderDelivering || !keeperDelivering {
		t.Errorf("after the connection came back the process that had been away holds a live delivery subscription = %t and the process that took the durable over holds one = %t, want false and true",
			holderDelivering, keeperDelivering)
	}
}

// connectionEventProcesses is the two processes every row of this file holds to account, set up
// and running: the holder, whose bus client reaches NATS through a proxy the row can take down,
// and the keeper, whose client reaches the server directly and which stands by for the durable
// until the holder is cut off.
//
// Each process gets a fake GitHub of its own, so one envelope's handling can be counted per
// process, and that count is the whole measurement of every row here: a durable two processes
// deliver answers ConsumerInfo exactly as one does.
type connectionEventProcesses struct {
	// server is the row's own NATS server, whose monitoring endpoint answers for this row's
	// subscribers alone (deliverSubjectSubscribers).
	server *testnats.Server
	// proxy carries the holder's connection and no other, so a row takes that one process off
	// NATS by cutting it.
	proxy        *testnats.Proxy
	holderClient *bus.Client
	keeperClient *bus.Client
	holder       *Intake
	keeper       *Intake
	// holderFetches and keeperFetches are the pull request fetches each process has made.
	holderFetches fetchCount
	keeperFetches fetchCount
	// holderHeld is the hold on the holder's first fetch of pull request 1, and is nil unless the
	// row asked for a holding fake (holderHoldsItsFirstFetch).
	holderHeld *heldPullRequests
	// hooked takes one value per reconnect of the holder's client (awaitHolderConnectedAgain).
	hooked <-chan struct{}
	// logs is every record logged since before either process started.
	logs func() []map[string]any
}

// holderGitHubFake is what the holder's fake GitHub does with a pull request fetch: count it,
// which is all a row measuring one envelope's fan-out needs, or count it and hold the first fetch
// of pull request 1, for a row that needs a handler of the holder's still inside its fetch when
// the connection goes.
type holderGitHubFake int

const (
	holderCountsItsFetches holderGitHubFake = iota
	holderHoldsItsFirstFetch
)

// startConnectionEventProcesses sets the two processes up and leaves them where every row of this
// file begins: the holder bound to the delivery durable, the keeper standing by for it, a
// reconnect hook on the holder's client, and the logs captured from before either process
// started. holderFake is what the holder's fake GitHub does with a fetch.
func startConnectionEventProcesses(t *testing.T, holderFake holderGitHubFake) *connectionEventProcesses {
	t.Helper()
	logs := captureLogs(t)
	// A NATS server of this row's own rather than the package's shared one (testnats.URL): the
	// row reads the server's monitoring endpoint for the subscribers on one deliver subject, and
	// on the shared server that endpoint answers for every test running beside this one.
	server, uri := testnats.Start(t)
	proxy := testnats.StartProxy(t, uri)
	holderClient := reconnectTestClient(t, proxy.URL())
	keeperClient := reconnectTestClient(t, uri)

	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	holderFetches, holderHeld, holderGitHub := holderFakeGitHub(t, holderFake)
	keeperFetches, keeperGitHub := countingGitHub(t)
	holder := newIntake(holderClient, pool, deliveryTestLeases(t, pool), holderGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))
	keeper := newIntake(keeperClient, pool, deliveryTestLeases(t, pool), keeperGitHub, events.NewBroker(), loadedSettingsCache(t, ctx, pool))

	// The holder goes first and has the durable before the keeper starts, so the row knows whose
	// connection to drop. Its own subscription is waited for as well as the server's PushBound:
	// the bind is this process's only once both say so.
	runIntake(t, holder)
	waitFor(t, 30*time.Second, func() bool {
		return intakeHoldsSubscription(holder) && deliveryDurableBound(keeperClient)
	}, func() string {
		return fmt.Sprintf("the first process never took the delivery durable: its own subscription is live = %t, and the server reports the durable bound = %t",
			intakeHoldsSubscription(holder), deliveryDurableBound(keeperClient))
	})
	// The keeper stands by: the durable admits one subscriber, and its standby line is the proof
	// its bind poll is running before the holder is cut off.
	runIntake(t, keeper)
	waitFor(t, 30*time.Second, func() bool {
		return loggedMessage(logs(), standbyConsumerHeldMessage)
	}, func() string {
		return fmt.Sprintf("the second process never stood by for the durable the first holds (%q); the messages logged were %q",
			standbyConsumerHeldMessage, loggedMessages(logs()))
	})

	// A reconnect hook on the holder's client, so a row can wait for the connection to be back
	// and for the client's own restore of its subscriptions to have run: the bus restores them
	// before it runs these hooks (internal/bus/recovery.go). Waiting on SubOK instead would never
	// return once the intake gives its registration up on the connection event, which is exactly
	// what this change makes it do.
	hooked := make(chan struct{}, 8)
	holderClient.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	return &connectionEventProcesses{
		server: server, proxy: proxy,
		holderClient: holderClient, keeperClient: keeperClient,
		holder: holder, keeper: keeper,
		holderFetches: holderFetches, keeperFetches: keeperFetches.Load,
		holderHeld: holderHeld, hooked: hooked, logs: logs,
	}
}

// awaitKeeper waits for the keeper to have taken the durable over from the holder
// (awaitDurableTakenOver).
func (p *connectionEventProcesses) awaitKeeper(t *testing.T) {
	t.Helper()
	awaitDurableTakenOver(t, p.holder, p.keeper, p.keeperClient)
}

// fetchCount is how many pull request fetches one process has made, read the same way whichever
// fake GitHub that process was given: a counting fake's atomic counter, or a holding fake's own
// record of the fetches it was asked for. Load, so that a row reads either of them as it reads an
// atomic.Int64.
type fetchCount func() int64

func (count fetchCount) Load() int64 { return count() }

// holderFakeGitHub is the holder's fake GitHub and the count of its fetches: a counting one
// (countingGitHub), or one that counts and holds its first fetch of pull request 1 until the hold
// is released or the handler making it is abandoned (holdPullRequestOneUntilAbandoned, whose
// record of the fetches it was asked for is this fake's count).
//
// That hold's wait on the request's own context, and not on the release alone, is what a row
// holding a handler through a give-up needs: the give-up abandons a handler by cancelling it, the
// stop waits for every handler to return before it hands the messages back, and a handler returns
// only once the fetch it is inside does.
func holderFakeGitHub(t *testing.T, which holderGitHubFake) (fetchCount, *heldPullRequests, *githubapp.Client) {
	t.Helper()
	if which == holderCountsItsFetches {
		fetches, github := countingGitHub(t)
		return fetches.Load, nil, github
	}
	fake := newFakeGitHub(t)
	held := holdPullRequestOneUntilAbandoned(fake)
	return func() int64 { return heldFetches(held, "1") }, held, fake.newTestClient()
}

// heldFetches is how many fetches of pull request number a holding fake GitHub was asked for: one
// "start" event each, recorded as the fetch begins, which is where a counting fake adds to its
// counter (heldPullRequests.of).
func heldFetches(held *heldPullRequests, number string) int64 {
	fetches := int64(0)
	for _, event := range held.of(number) {
		if event == "start "+number {
			fetches++
		}
	}
	return fetches
}

// awaitDurableTakenOver waits for the process standing by to hold the delivery durable while the
// other process is cut off from NATS, and names what each of the two is when it never does. It is
// called twice in a row that closes the holder's connection after the hand-over, which costs one
// read: the wait returns at once once the keeper is delivering.
func awaitDurableTakenOver(t *testing.T, holder, keeper *Intake, keeperClient *bus.Client) {
	t.Helper()
	waitFor(t, 30*time.Second, func() bool {
		return intakeHoldsSubscription(keeper)
	}, func() string {
		return fmt.Sprintf("the standby process never took the delivery durable over while the holder was cut off: the holder's subscription is live = %t, the standby's = %t, and the server reports the durable bound = %t",
			intakeHoldsSubscription(holder), intakeHoldsSubscription(keeper), deliveryDurableBound(keeperClient))
	})
}

// awaitHolderConnectedAgain waits for the client that was cut off to have come back: its
// reconnect hooks have run, which the bus runs after it has restored whatever subscriptions it
// still holds registrations for, and its connection reports live. Measuring the envelope before
// that restore has ended would read a fan-out that had not happened yet. The hook is what the
// wait reads, rather than the client holding a subscription, because a client whose registration
// the intake gave up comes back holding none.
func awaitHolderConnectedAgain(t *testing.T, holderClient *bus.Client, hooked <-chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-hooked:
	case <-time.After(within):
		t.Fatalf("the client cut off from NATS never ran its reconnect hooks in %s once the link was restored: connected = %t, holds a subscription = %t",
			within, holderClient.Connected(), holderClient.SubOK())
	}
	waitFor(t, 30*time.Second, holderClient.Connected, func() string {
		return "the client cut off from NATS ran its reconnect hooks and still reports no live connection"
	})
}

// intakeHoldsSubscription reports whether intake has a delivery subscription of its own that is
// still live: one its last subscribe installed, on a connection that still carries it. It is how
// a row tells which of two processes is delivering the durable, which the server's own
// ConsumerInfo cannot say.
func intakeHoldsSubscription(intake *Intake) bool {
	intake.mu.Lock()
	defer intake.mu.Unlock()
	return intake.delivering != nil && intake.delivering.sub.IsValid()
}

// deliveryDurableBound reports whether the server has a subscriber on the delivery durable, and
// false while the durable does not exist yet: a row reads it before the first intake has created
// it, where deliveryConsumerInfo would fail the test.
func deliveryDurableBound(natsClient *bus.Client) bool {
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	return err == nil && info.PushBound
}

// deliveryAckPending is how many messages the delivery durable still has outstanding, and -1
// while the consumer cannot be read, so a row polling it reads a number rather than failing on
// one read a server took its time over.
func deliveryAckPending(natsClient *bus.Client) int {
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		return -1
	}
	return info.NumAckPending
}

// deliverSubjectSubscribers is how many subscribers the server itself reports on subject, read
// from its monitoring endpoint. ConsumerInfo.PushBound is one bool for one subscriber and for
// two, so the server's own subscription list is the only place a second subscriber on a durable's
// deliver subject can be counted.
func deliverSubjectSubscribers(t *testing.T, server *testnats.Server, subject string) int {
	t.Helper()
	endpoint := server.MonitorURL() + "/subsz?subs=1"
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s, want 200 OK", endpoint, response.Status)
	}
	var reported struct {
		Subscriptions []struct {
			Subject string `json:"subject"`
		} `json:"subscriptions_list"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reported); err != nil {
		t.Fatalf("decode %s: %v", endpoint, err)
	}
	subscribers := 0
	for _, subscription := range reported.Subscriptions {
		if subscription.Subject == subject {
			subscribers++
		}
	}
	return subscribers
}

// countingGitHub is a fake GitHub of one process's own, counting the pull request fetches that
// process makes and answering every one of them 500, which the intake logs and acknowledges
// (holdPullRequestOne). One per process is what lets a row say which process handled an envelope.
func countingGitHub(t *testing.T) (*atomic.Int64, *githubapp.Client) {
	t.Helper()
	fetches := &atomic.Int64{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	return fetches, fake.newTestClient()
}

// reconnectTestClient connects one NATS client at uri for one process of a row of this file, and
// closes it with the test. intakeTestClient connects to the package's shared server, which no row
// here can cut a link to.
func reconnectTestClient(t *testing.T, uri string) *bus.Client {
	t.Helper()
	natsClient, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect NATS at %s: %v", uri, err)
	}
	t.Cleanup(natsClient.Close)
	return natsClient
}

// loggedMessage reports whether any captured record's msg is message.
func loggedMessage(records []map[string]any, message string) bool {
	for _, record := range records {
		if logged, _ := record["msg"].(string); logged == message {
			return true
		}
	}
	return false
}

// loggedMessages is each distinct msg the capture holds, in the order they were first logged:
// what a failure prints in place of the message it looked for and did not find.
func loggedMessages(records []map[string]any) []string {
	var messages []string
	for _, record := range records {
		logged, _ := record["msg"].(string)
		if logged != "" && !slices.Contains(messages, logged) {
			messages = append(messages, logged)
		}
	}
	return messages
}
