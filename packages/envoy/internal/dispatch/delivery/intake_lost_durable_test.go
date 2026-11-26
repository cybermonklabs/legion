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
func TestADurableLostOutsideATakeOverIsCreatedAgainAfterTheLastAckFloorRead(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 61)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, deliveryRepo)
	natsClient := intakeTestClient(t)
	leases := deliveryTestLeases(t, pool)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, leases, fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	bound := awaitPushBound(t, natsClient, "", "the intake never bound its durable")

	const firstBefore, secondBefore, firstAfter, secondAfter = 6101, 6102, 6103, 6104
	publishAnswered(t, natsClient, github, firstBefore, openedForKey(firstBefore, key, "6101610161016101610161016101610161016101"))
	lastAcked := publishAnswered(t, natsClient, github, secondBefore, openedForKey(secondBefore, key, "6102610261026102610261026102610261026102"))
	awaitAcknowledged(t, natsClient)
	// Two of the intake's checks (intakeHoldRenew) and a second: at least one reads the ack floor
	// at the second pull request.
	time.Sleep(2*intakeHoldRenew + time.Second)

	taken, release := make(chan struct{}), make(chan struct{})
	ran := make(chan bool, 1)
	go func() {
		ran <- duty.Exclusive(ctx, leases, consumerLease(bus.DispatchDeliveryDurable), time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	releaseLease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseLease)
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatalf("the test never took the %s lease", consumerLease(bus.DispatchDeliveryDurable))
	}

	if err := natsClient.JS().DeleteConsumer(bus.Stream, bus.DispatchDeliveryDurable); err != nil {
		t.Fatalf("delete the delivery durable: %v", err)
	}
	publishAnswered(t, natsClient, github, firstAfter, openedForKey(firstAfter, key, "6103610361036103610361036103610361036103"))
	publishAnswered(t, natsClient, github, secondAfter, openedForKey(secondAfter, key, "6104610461046104610461046104610461046104"))

	lost := func() bool {
		for _, record := range logs() {
			if record["level"] == "ERROR" && record["msg"] == durableLost && record["name"] == bus.DispatchDeliveryDurable {
				return true
			}
		}
		return false
	}
	waitFor(t, 20*time.Second, lost, func() string {
		return fmt.Sprintf("the intake never said at ERROR that its durable was lost: %v", logs())
	})
	holdName := intakeHoldName(bus.DispatchDeliveryDurable)
	var token string
	waitFor(t, 20*time.Second, func() bool {
		read, _, err := duty.HoldToken(ctx, leases, holdName)
		token = read
		return err == nil && parseIntakeHoldToken(read).start == lastAcked+1
	}, func() string {
		return fmt.Sprintf("the hold's token after the loss is %q, want it to record the start %d, the sequence after the last ack floor the intake read", token, lastAcked+1)
	})

	releaseLease()
	if held := <-ran; !held {
		t.Fatalf("the test never held the %s lease, so the durable could be created again before the publishes", consumerLease(bus.DispatchDeliveryDurable))
	}
	created := awaitPushBound(t, natsClient, bound.Config.DeliverSubject, "the intake never created its lost durable again")
	for _, number := range []int{firstAfter, secondAfter} {
		waitFor(t, 20*time.Second, func() bool { return headRows(t, ctx, pool, number) == 1 }, func() string {
			return fmt.Sprintf("%s#%d, published after the durable was lost, has %d head rows 20s after the durable was created again at %d, want 1\n%s",
				deliveryRepo, number, headRows(t, ctx, pool, number), created.Config.OptStartSeq, intakeStateReport(t, natsClient))
		})
	}
	if created.Config.OptStartSeq != lastAcked+1 {
		t.Fatalf("the lost durable was created again at %d, want %d, the sequence after the last ack floor the intake read", created.Config.OptStartSeq, lastAcked+1)
	}
}
