package delivery

// The three things the build before holds asked of its bind (#2913), asked of the hold and the
// take-over that replaced it: two processes polling one durable never both bind it, a holder that
// stops is replaced within a bound the code states, and a take-over that cannot finish gives its
// subscription up rather than leaving a binding no other process can see.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// takeOverAttempt is one intake's own process: its client, its lease pool and the intake itself,
// whose attempt the row drives by hand, as a bind poll does (heldDurable.run).
type takeOverAttempt struct {
	intake *Intake
	client *bus.Client
}

// newTakeOverProcess is one envoy-dispatch process's delivery intake over pool's database and
// natsURL's server, with a lease pool and a NATS client of its own, as a second process has. It
// is never run: the row calls attempt itself, so the two attempts' order is the row's and not
// two tickers'.
func newTakeOverProcess(t *testing.T, pool *store.Pool, natsURL string) *takeOverAttempt {
	t.Helper()
	second := storetest.SecondProcess(t, &store.Store{Pool: pool})
	leases := storetest.Leases(t, second.Pool)
	client, err := bus.Connect([]string{natsURL}, bus.WithName(intakeConnectionName))
	if err != nil {
		t.Fatalf("connect the process's intake: %v", err)
	}
	t.Cleanup(client.Close)
	intake := newIntake(client, second.Pool, leases, newFakeGitHub(t).newTestClient(), events.NewBroker(),
		loadedSettingsCache(t, t.Context(), second.Pool))
	t.Cleanup(func() {
		stopping, cancel := context.WithTimeout(context.Background(), intakeStopBudget)
		defer cancel()
		intake.stop(stopping)
	})
	return &takeOverAttempt{intake: intake, client: client}
}

// delivers is the deliver subject this intake delivers at, empty while it delivers nothing. Read
// after its attempt has returned, which is the only writer.
func (a *takeOverAttempt) delivers() string { return a.intake.subject }

// Two processes polling one durable never both bind it, whether one polls after the other or both
// poll at once: the durable's hold admits one of them, and the take-over that follows runs under
// the durable's consumer lease, so the second process neither subscribes nor recreates the
// durable. This is the regression lock the build before holds kept on its bind
// (TestTwoProcessesPollingOneDurableNeverBothBindIt in #2913), asked of the mechanism that
// replaced it.
func TestTwoProcessesPollingOneDurableNeverBothBindIt(t *testing.T) {
	t.Run("back to back", func(t *testing.T) {
		pool, ctx := deliveryTestPool(t)
		seedDeliverySettings(t, ctx, pool)
		natsURL := testnats.URL(t)
		publisher := intakeTestClient(t)
		first, second := newTakeOverProcess(t, pool, natsURL), newTakeOverProcess(t, pool, natsURL)

		first.intake.attempt(ctx)
		second.intake.attempt(ctx)

		requireOneBinder(t, publisher, first, second)
	})

	t.Run("at once", func(t *testing.T) {
		pool, ctx := deliveryTestPool(t)
		seedDeliverySettings(t, ctx, pool)
		natsURL := testnats.URL(t)
		publisher := intakeTestClient(t)
		first, second := newTakeOverProcess(t, pool, natsURL), newTakeOverProcess(t, pool, natsURL)

		// Both attempts are made from a goroutine of their own, released together, so neither
		// process's take-over is ordered ahead of the other's.
		start := make(chan struct{})
		var attempting sync.WaitGroup
		for _, process := range []*takeOverAttempt{first, second} {
			attempting.Go(func() {
				<-start
				process.intake.attempt(ctx)
			})
		}
		close(start)
		attempting.Wait()

		requireOneBinder(t, publisher, first, second)
	})
}

// requireOneBinder holds that exactly one of the two processes delivers from the durable, and
// that the server reports the durable bound to that one's deliver subject.
func requireOneBinder(t *testing.T, publisher *bus.Client, first, second *takeOverAttempt) {
	t.Helper()
	bound := []string{}
	for _, subject := range []string{first.delivers(), second.delivers()} {
		if subject != "" {
			bound = append(bound, subject)
		}
	}
	if len(bound) != 1 {
		t.Fatalf("%d of the two processes deliver from the durable (%v), want exactly one: %s",
			len(bound), bound, consumerState(publisher))
	}
	info := awaitPushBound(t, publisher, "the durable the two processes polled is bound to neither of them")
	if info.Config.DeliverSubject != bound[0] {
		t.Fatalf("the durable delivers to %s, want the one binder's subject %s", info.Config.DeliverSubject, bound[0])
	}
}

// standbyTakeOverBound is how long TestAHolderThatStopsIsReplacedWithinTheBoundTheCodeStates
// gives a standby to replace the holder that stopped. The holder releases its hold as it stops,
// so what the standby waits out is one durableBindInterval poll (2 s) and its own take-over: the
// durable's consumer lease, the delete, the subscribe and the create. Ten seconds is what the
// two-process row of the real binary allows a surviving envoy-dispatch after a holder *stops*
// while waiting for admission (cmd/dispatch/multiprocess_test.go,
// TestAProcessStoppedWhileWaitingForAdmissionReleasesItsDurables gives it four), with room for a
// loaded two-CPU host; nothing here is jittered, so this is a hang guard too. The hand-over after
// a holder *dies* is the slower one, bounded by the hold's lapse
// (TestAnAbandonedHoldersMessagesReachTheStandbyWithinTheHoldsLapse).
const standbyTakeOverBound = 10 * time.Second

// A holder that stops is replaced inside the bound the code states, which is the half of the
// durable's single binding that makes it worth having. This is #2913's regression lock on the
// hand-over, measured against the mechanism that replaced its bind: the exclusion the hold buys
// must not be paid for with a slower hand-over.
func TestAHolderThatStopsIsReplacedWithinTheBoundTheCodeStates(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	holder := startIntakeProcess(t, pool, natsURL, nil)
	held := awaitPushBound(t, publisher, "the holder never bound the durable")
	standby := startIntakeProcess(t, pool, natsURL, nil)
	// Two of the standby's polls: by then it has tried for the hold, found it held and stood by,
	// so the hand-over measured below is one of its ordinary polls and not its first.
	time.Sleep(2 * durableBindInterval)

	holder.stop()
	stopped := time.Now()
	var taken *natsgo.ConsumerInfo
	waitFor(t, standbyTakeOverBound, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err != nil || !info.PushBound || info.Config.DeliverSubject == held.Config.DeliverSubject {
			return false
		}
		taken = info
		return true
	}, func() string {
		return "the standby did not take the durable the holder released over within the bound: " + consumerState(publisher)
	})
	t.Logf("the standby took the durable over %s after the holder stopped", time.Since(stopped))

	publishMergedPullRequest(t, publisher, 11)
	waitFor(t, 20*time.Second, func() bool { return standby.fetches(11) == 1 }, func() string {
		return "the standby that took the durable over did not handle a message: " + consumerState(publisher)
	})
	if taken.Config.DeliverSubject == held.Config.DeliverSubject {
		t.Fatalf("the standby took the durable over on the holder's own deliver subject %s", taken.Config.DeliverSubject)
	}
}

// A take-over that cannot finish gives its subscription up rather than leaving a binding no other
// process can see: the subscribe runs before the create (so the durable is bound from the moment
// it exists, TestATakeOverCreatesTheDurableAlreadyBound), and a create that never lands therefore
// leaves a subscription on a durable that does not exist. The intake drops it whole, says so at
// ERROR, and binds the durable afresh on a later attempt. This is what #2913's
// TestABindTheServerNeverConfirmsIsUnsubscribedAndFails held of its confirmation step, asked of
// the step that replaced it: there is no confirmation read any more, because the create's own
// answer is the confirmation.
func TestATakeOverWhoseCreateNeverLandsGivesItsSubscriptionUp(t *testing.T) {
	logs := captureLogs(t)
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	var failing atomic.Bool
	failing.Store(true)
	attempts := failCreatesWhile(t, failing.Load)
	process := startIntakeProcess(t, pool, natsURL, nil)

	// The take-over subscribes before its create, so a subscription stands while the creates
	// fail. That subscription is read here, and the row then holds that this very one is given
	// up whole: its keyed runner is stopped (heldDurable.stopDeliveringWithin), which no later
	// attempt undoes, since each one subscribes afresh.
	var subscribed *intakeDelivery
	waitFor(t, 30*time.Second, func() bool {
		process.intake.mu.Lock()
		subscribed = process.intake.delivering
		process.intake.mu.Unlock()
		return attempts.Load() > 0 && subscribed != nil
	}, func() string {
		return fmt.Sprintf("the take-over never subscribed before its create: creates=%d; %s", attempts.Load(), consumerState(publisher))
	})
	waitFor(t, 30*time.Second, func() bool {
		subscribed.keys.mu.Lock()
		defer subscribed.keys.mu.Unlock()
		return subscribed.keys.stopped
	}, func() string {
		return fmt.Sprintf("the take-over whose create never landed kept its subscription: creates=%d; %s; logs %v",
			attempts.Load(), consumerState(publisher), logs())
	})
	if _, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable); err == nil {
		t.Fatalf("the durable exists, so the create the test failed did land: %s", consumerState(publisher))
	}
	var said bool
	for _, record := range logs() {
		if record["level"] == "ERROR" && record["msg"] == "dispatch delivery: create NATS consumer" &&
			record["name"] == bus.DispatchDeliveryDurable {
			said = true
		}
	}
	if !said {
		t.Fatalf("the intake said nothing about the create it gave up on: %v", logs())
	}

	// The create lands on a later attempt, and the intake binds the durable afresh and delivers.
	failing.Store(false)
	publishMergedPullRequest(t, publisher, 12)
	waitFor(t, 30*time.Second, func() bool { return process.fetches(12) == 1 }, func() string {
		return "the intake never bound the durable afresh after the take-over it gave up: " + consumerState(publisher)
	})
}
