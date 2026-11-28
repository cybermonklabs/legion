package delivery

// R3 of the results lookup (v-first-start-tip): the first start after the deploy begins after the
// last event the bus holds, so it reads no older event; an open pull request then gets its head at
// its next event; and a later start reads no event again. The first start's position is #5525's
// (takeOverStart, takeover_start.go); this file holds the three claims together on the intake that
// writes a head from GitHub's answer, so a start that read an event would both ask GitHub and write
// a head.

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// TestAFirstStartReadsNoEventTheBusHeldAndASecondStartReadsNoneAgain: the bus holds the opened
// events of two pull requests, GitHub answers both, and no intake has ever bound. The first start
// binds, and neither pull request has a head or was read from GitHub. A push of the second after
// the start writes its head at the pushed commit within 5 s. A second start, after the first
// stopped, makes no read of GitHub and writes no head of the first pull request.
//
// The control is the same history with the durable created before the first start to deliver from
// the stream's first message (by start sequence 1), as the verification's negative control
// creates it: that start reads the older events, so the first pull request gets a head, which the
// claim's own row refuses.
func TestAFirstStartReadsNoEventTheBusHeldAndASecondStartReadsNoneAgain(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name         string
		fromTheFirst bool
	}{
		{"the first start of a durable that never existed", false},
		{"control: a durable created to deliver from the stream's first message", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			pool, ctx := deliveryTestPool(t)
			key := seedHeadIssue(t, ctx, pool, 64)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, deliveryRepo)
			publisher := intakeTestClient(t)

			const held, pushed = 6401, 6402
			publishAnswered(t, publisher, github, held, openedForKey(held, key, "6401640164016401640164016401640164016401"))
			publishAnswered(t, publisher, github, pushed, openedForKey(pushed, key, "6402640264026402640264026402640264026402"))
			if row.fromTheFirst {
				if _, err := publisher.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
					Durable:        bus.DispatchDeliveryDurable,
					DeliverSubject: natsgo.NewInbox(),
					FilterSubject:  githubIntakeSubject,
					AckPolicy:      natsgo.AckExplicitPolicy,
					DeliverPolicy:  natsgo.DeliverByStartSequencePolicy,
					OptStartSeq:    1,
				}); err != nil {
					t.Fatalf("create the durable at the stream's first message: %v", err)
				}
			}

			start := func(what string, previous string) (stop func()) {
				t.Helper()
				intake := newIntake(tuningFor(t), intakeTestClient(t), pool, deliveryTestLeases(t, pool), fake.newTestClient(),
					events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
				runCtx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					intake.Run(runCtx)
				}()
				stop = func() {
					cancel()
					<-done
				}
				t.Cleanup(stop)
				awaitPushBound(t, publisher, previous, what)
				awaitAcknowledged(t, publisher)
				return stop
			}
			stopFirst := start("the first start did not bind the durable", "")
			bound, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
			if err != nil {
				t.Fatalf("read the durable after the first start: %v", err)
			}
			readsAtStart := github.reads.Load()
			if row.fromTheFirst {
				if rows := headRows(t, ctx, pool, held); rows != 1 || readsAtStart == 0 {
					t.Fatalf("CONTROL: the start of a durable created at the stream's first message left %s#%d %d head rows after %d reads of GitHub, want 1 after a read: the check below could not see a start that read an older event",
						deliveryRepo, held, rows, readsAtStart)
				}
				return
			}
			for _, number := range []int{held, pushed} {
				if rows := headRows(t, ctx, pool, number); rows != 0 {
					t.Errorf("%s#%d, opened before the first start, has %d head rows after it, want 0: the first start read an event the bus held", deliveryRepo, number, rows)
				}
			}
			if readsAtStart != 0 {
				t.Errorf("the first start read %d pull requests from GitHub, want none: it read an event the bus held", readsAtStart)
			}

			const head = "6402640264026402640264026402640264026403"
			push := openedForKey(pushed, key, head)
			push["action"], push["updated_at"] = "synchronize", time.Now().UTC().Format(time.RFC3339)
			publishedAt := time.Now()
			publishAnswered(t, publisher, github, pushed, push)
			waitFor(t, 5*time.Second, func() bool { return headRows(t, ctx, pool, pushed) == 1 }, func() string {
				return "the push after the first start wrote no head within 5 s\n" + intakeStateReport(t, publisher)
			})
			if stored := readHead(t, unrenamed(), ctx, pool, deliveryRepo, pushed); stored.HeadSHA != head || stored.State != "open" {
				t.Fatalf("%s#%d's head after the push is %s %s, want open at %s", deliveryRepo, pushed, stored.State, stored.HeadSHA, head)
			}
			t.Logf("the push's head was written %s after its publish", time.Since(publishedAt).Round(time.Millisecond))

			stopFirst()
			readsBeforeSecond := github.reads.Load()
			start("the second start did not bind the durable", bound.Config.DeliverSubject)
			if reads := github.reads.Load() - readsBeforeSecond; reads != 0 {
				t.Errorf("the second start read %d pull requests from GitHub, want none: it read an event again", reads)
			}
			if rows := headRows(t, ctx, pool, held); rows != 0 {
				t.Errorf("%s#%d has %d head rows after the second start, want 0: the second start read an event the first left", deliveryRepo, held, rows)
			}
		})
	}
}
