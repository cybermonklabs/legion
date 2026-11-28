package delivery

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A durable lost outside a take-over -- deleted while this intake delivers from it -- is created
// again right after the last ack floor the intake read of it (check), and the loss is logged at
// ERROR. The intake handles and acknowledges two pull requests, and a check reads the floor after
// them. The test then holds the durable's consumer lease, so no take-over can run, deletes the
// durable and publishes two more pull requests: they reach the stream while no durable exists.
// While the lease is held, the hold's token records the sequence after the second pull request as
// the start, where another holder would read it (recordLoss). Once the lease is free, the intake
// creates the durable again there, so the two published after the loss get their head rows.
// Created at the stream's next message instead, as a durable with no start recorded is, it would
// skip both.
//
// The intake's checks and its tries for the durable tick on its hold clock (heldDurable.holdClock),
// a fake the row moves: one intakeHoldRenew to the check that reads the floor, once the two pull
// requests are acknowledged, and one more to the check that finds the durable gone, each waited for
// through the tuning's checked hook; then a durableBindInterval at a time to the try that creates the
// durable again. The moves stay short of the hold's loss deadline (intakeHoldTTL - intakeHoldMargin
// past the take), so the hold covers every step whether or not a renewal the moves fire lands.
func TestADurableLostOutsideATakeOverIsCreatedAgainAfterTheLastAckFloorRead(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 61)
	natsClient := intakeTestClient(t)
	leases := deliveryTestLeases(t, pool)
	checks := make(chan struct{}, 1)
	checked := func() {
		select {
		case checks <- struct{}{}:
		default:
		}
	}
	tuningFor(t).checked.Store(&checked)
	holdClock := fakeIntakeClock()
	start := holdClock.Now()
	intake := newIntake(tuningFor(t), natsClient, pool, leases, newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	intake.holdClock = holdClock
	startIntake(t, intake)
	bound := awaitPushBound(t, natsClient, "", "the intake never bound its durable")
	// check moves the hold clock to the intake's next check of its durable, once the run loop has
	// armed it (clock.Ticker arms a tick once the work for the one before has returned), and waits
	// for that check to end (the tuning's checked hook).
	check := func(n int, what string) {
		t.Helper()
		holdClock.AwaitArmedAt(t, fmt.Sprintf("the intake's check %d of its durable", n), start.Add(time.Duration(n)*intakeHoldRenew))
		holdClock.Set(start.Add(time.Duration(n) * intakeHoldRenew))
		testwait.Receive(t, what, checks)
	}

	const firstBefore, secondBefore, firstAfter, secondAfter = 6101, 6102, 6103, 6104
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", firstBefore, openedForKey(firstBefore, key, "6101610161016101610161016101610161016101"))
	lastAcked := publishPullRequestEnvelope(t, natsClient, "acme", "widgets", secondBefore, openedForKey(secondBefore, key, "6102610261026102610261026102610261026102"))
	awaitAcknowledged(t, natsClient)
	check(1, "the intake's check that reads the ack floor at the second pull request")

	taken, release := make(chan struct{}), make(chan struct{})
	ran := make(chan bool, 1)
	go func() {
		ran <- duty.Exclusive(ctx, nil, leases, consumerLease(bus.DispatchDeliveryDurable), time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	releaseLease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseLease)
	testwait.Receive(t, fmt.Sprintf("the test to take the %s lease", consumerLease(bus.DispatchDeliveryDurable)), taken)

	if err := natsClient.JS().DeleteConsumer(bus.Stream, bus.DispatchDeliveryDurable); err != nil {
		t.Fatalf("delete the delivery durable: %v", err)
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", firstAfter, openedForKey(firstAfter, key, "6103610361036103610361036103610361036103"))
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", secondAfter, openedForKey(secondAfter, key, "6104610461046104610461046104610461046104"))

	lost := func() bool {
		for _, record := range logs() {
			if record["level"] == "ERROR" && record["msg"] == durableLost && record["name"] == bus.DispatchDeliveryDurable {
				return true
			}
		}
		return false
	}
	check(2, "the intake's check that finds its durable gone")
	if !lost() {
		t.Fatalf("the intake's check of its deleted durable did not say at ERROR that it was lost: %v", logs())
	}
	holdName := intakeHoldName(bus.DispatchDeliveryDurable)
	var token string
	waitFor(t, func() bool {
		read, _, err := duty.HoldToken(ctx, leases, holdName)
		token = read
		return err == nil && parseIntakeHoldToken(read).start == lastAcked+1
	}, func() string {
		return fmt.Sprintf("the hold's token after the loss is %q, want it to record the start %d, the sequence after the last ack floor the intake read", token, lastAcked+1)
	})

	releaseLease()
	if held := testwait.Receive(t, "the test's duty.Exclusive to return once the lease was released", ran); !held {
		t.Fatalf("the test never held the %s lease, so the durable could be created again before the publishes", consumerLease(bus.DispatchDeliveryDurable))
	}
	// The intake's next try for the durable after the loss is the one that creates it again: the
	// tries tick a durableBindInterval apart on the hold clock, and one that comes while the test
	// holds the lease is skipped.
	moveTicks(t, holdClock, durableBindInterval, func() bool {
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		return err == nil && info.PushBound && info.Config.DeliverSubject != bound.Config.DeliverSubject
	}, func() string {
		return fmt.Sprintf("the intake never created its lost durable again; its hold clock stands %s past its start: %s",
			holdClock.Now().Sub(start), consumerState(natsClient))
	})
	created := awaitPushBound(t, natsClient, bound.Config.DeliverSubject, "the intake never created its lost durable again")
	for _, number := range []int{firstAfter, secondAfter} {
		waitFor(t, func() bool { return headRows(t, ctx, pool, number) == 1 }, func() string {
			return fmt.Sprintf("%s#%d, published after the durable was lost, has %d head rows after the durable was created again at %d, want 1\n%s",
				deliveryRepo, number, headRows(t, ctx, pool, number), created.Config.OptStartSeq, intakeStateReport(t, natsClient))
		})
	}
	if created.Config.OptStartSeq != lastAcked+1 {
		t.Fatalf("the lost durable was created again at %d, want %d, the sequence after the last ack floor the intake read", created.Config.OptStartSeq, lastAcked+1)
	}
}
