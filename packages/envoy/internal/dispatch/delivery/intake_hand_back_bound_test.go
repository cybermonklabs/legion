package delivery

// Every way the intake hands a `pull_request` envelope back to the bus has a limit (acceptance of
// cybermonklabs/legion#4209, round 3, defect 5). Any caller of Envoy's /v1 publish route can send
// an envelope (dispatch://INFRA-3911), and one that no delivery can get past must not hold one of
// the intake's intakeMaxAckPending in-flight slots for ever: four of them stop every envelope
// behind them.

import (
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// An envelope no delivery can get past is final at its first delivery: the intake acknowledges
// it and owes no read. Each case publishes intakeMaxAckPending such envelopes and then GitHub's
// own event of acme/widgets#7, and wants #7's head row within the 90 s of the tester's
// TestAcceptAnEnvelopeWhoseOwedReadCannotBeStoredDoesNotStopTheIntake, with the cap shrunk to 3
// as there; and each of the four delivered once.
//
// The first case is the tester's red test: numbers 3000000000 and up do not fit the integer
// columns, so at ac1460389fcc the first read of Postgres failed, the envelope was handed back to
// the cap, the owed record failed the same way, and it was handed back for ever. The second and
// third name no pull request a row can hold either. The last is the class past a check of the
// name: a pull request GitHub answers with a title holding a NUL byte, which Postgres refuses as a
// value (SQLSTATE 22021), so the head write fails at every delivery for the values alone; before
// the fix that cost each envelope the cap's 3 deliveries and an owed read that fails the same way
// at every pass.
func TestAnEnvelopeNoDeliveryCanGetPastIsFinalAtItsFirstDelivery(t *testing.T) {
	for _, poison := range []struct {
		name   string
		repo   string
		number func(i int) int
		title  string
	}{
		{"number_3000000000_and_up", "acme/widgets", func(i int) int { return 3_000_000_000 + i }, ""},
		{"number_zero_and_below", "acme/widgets", func(i int) int { return -i }, ""},
		{"repository_not_owner_name", "acmewidgets", func(i int) int { return 900 + i }, ""},
		{"an_answer_the_database_refuses", "acme/widgets", func(i int) int { return 900 + i }, "Change\x00"},
	} {
		t.Run(poison.name, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 4209)
			capHeadWriteDeliveriesAt(t, 3)
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			natsClient := intakeTestClient(t)
			startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool),
				client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
			awaitBoundDurable(t, natsClient)
			before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			for i := range intakeMaxAckPending {
				number := poison.number(i)
				if poison.title != "" {
					github.set("acme/widgets", number, "open", poisonHead, "feature/x", "", at)
					github.mu.Lock()
					github.pulls[number]["title"] = poison.title
					github.mu.Unlock()
				}
				publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
					prEnvelope(poison.repo, number, "opened", poisonHead, "feature/x", "", at))
			}
			const (
				pull = 7
				head = "7777777777777777777777777777777777777777"
			)
			branch, body := "legion/"+issue, "Dispatch: "+issue
			github.set("acme/widgets", pull, "open", head, branch, body, at)
			published := time.Now()
			publishPullRequestEnvelope(t, natsClient, "acme", "widgets", pull,
				prEnvelope("acme/widgets", pull, "opened", head, branch, body, at))

			waitFor(t, 90*time.Second, func() bool {
				return headRowCount(t, ctx, &Intake{pool: pool}, "acme/widgets", pull) == 1
			}, func() string {
				return fmt.Sprintf("90 s after GitHub's own event of #%d, behind %d envelopes of %s, #%d has no head row: those envelopes hold every in-flight slot; the durable is %+v",
					pull, intakeMaxAckPending, poison.name, pull, deliveryConsumerInfo(t, natsClient))
			})
			t.Logf("#%d's head row was written %s after its event", pull, time.Since(published).Round(time.Millisecond))
			awaitAcknowledged(t, natsClient)
			if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before; delivered != uint64(intakeMaxAckPending+1) {
				t.Errorf("the durable made %d deliveries of %d envelopes, want one each: an envelope no row can hold was handed back", delivered, intakeMaxAckPending+1)
			}
			var owed int
			if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed`).Scan(&owed); err != nil {
				t.Fatal(err)
			}
			if owed != 0 {
				t.Errorf("%d reads owed, want none: an envelope that names no pull request a row can hold owes no read", owed)
			}
		})
	}
}

// poisonHead is the head the envelopes no row can hold name.
const poisonHead = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// When the database takes no write of the owed record at any delivery the cap allows, the
// envelope still ends at the cap: GitHub answers 500 at each delivery, so the head row is never
// written, and another transaction holds pull_request_reads_owed locked for the whole run, so the
// owed record of each delivery waits out owedReadWriteTimeout and fails. At ac1460389fcc the
// envelope was then handed back for ever. Here it is acknowledged at the cap with one ERROR line
// naming the repository and the number: the one limit of R4, since Dispatch keeps no record of a
// change the database would not take.
func TestAnOwedReadTheDatabaseWillNotTakeEndsTheEnvelopeAtTheCap(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 76)
	capHeadWriteDeliveriesAt(t, 2)
	natsClient := intakeTestClient(t)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	const head = "7676767676767676767676767676767676767676"
	branch, body := "legion/"+issue, "Dispatch: "+issue
	opened := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", 76, "open", head, branch, body, opened)
	github.mu.Lock()
	github.failing = true
	github.mu.Unlock()

	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `lock table pull_request_reads_owed in access exclusive mode`); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = lock.Rollback(ctx)
		}
	}
	t.Cleanup(release)

	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool), client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 76, prEnvelope("acme/widgets", 76, "opened", head, branch, body, opened))

	var info *natsgo.ConsumerInfo
	waitFor(t, 60*time.Second, func() bool {
		info = deliveryConsumerInfo(t, natsClient)
		stream, err := natsClient.JS().StreamInfo(bus.Stream)
		return err == nil && info.AckFloor.Stream >= stream.State.LastSeq && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("the envelope is not acknowledged 60 s after the cap of 2 deliveries: it is handed back while the database will not take its owed read; the durable is %+v", info)
	})
	if delivered := info.Delivered.Consumer - before; delivered != 2 {
		t.Errorf("the envelope was delivered %d times, want the cap's 2", delivered)
	}
	release()
	var owed int
	if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed`).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	if owed != 0 {
		t.Errorf("%d reads owed after the database took none, want none", owed)
	}
}

// One refused write of the owed record, at the cap's delivery alone, loses no head (acceptance of
// cybermonklabs/legion#4209, round 4, defect 7). Taken from the tester's
// TestAcceptAnOwedRecordRefusedOnlyAtTheCapLosesNoHead unchanged but for its name and layout: the
// cap is 3, GitHub answers 500 for the whole run, and another session holds pull_request_reads_owed
// locked for 15 s from the second delivery's hand-back, over the third delivery, whose owed record
// waits out owedReadWriteTimeout and fails. Every other write succeeds. Then GitHub answers again
// and one reconcile pass runs, which must write the head. At 78731c6606 the owed read was recorded
// at the cap's delivery alone, so that one refusal acknowledged the envelope with nothing owed;
// each failed delivery now records it, so the first two deliveries' records stand.
func TestAnOwedRecordRefusedAtTheCapsOneDeliveryLosesNoHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 4210)
	capHeadWriteDeliveriesAt(t, 3)
	const (
		number = 4210
		head   = "4210421042104210421042104210421042104210"
	)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	branch, body := "legion/"+issue, "Dispatch: "+issue
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", number, "open", head, branch, body, at)
	github.mu.Lock()
	github.failing = true
	github.mu.Unlock()
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool),
		client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
		prEnvelope("acme/widgets", number, "opened", head, branch, body, at))

	// The second delivery has been made and handed back; the third, the cap, comes 10 s later.
	waitFor(t, 60*time.Second, func() bool { return deliveryConsumerInfo(t, natsClient).Delivered.Consumer >= 2 }, func() string {
		return fmt.Sprintf("the envelope was not delivered a second time: %+v", deliveryConsumerInfo(t, natsClient))
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, "lock table pull_request_reads_owed in access exclusive mode"); err != nil {
		t.Fatalf("lock pull_request_reads_owed: %v", err)
	}
	time.Sleep(15 * time.Second)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	t.Logf("pull_request_reads_owed was locked for 15 s around the cap's delivery; the durable is %+v", deliveryConsumerInfo(t, natsClient))

	waitFor(t, 60*time.Second, func() bool {
		stream, err := natsClient.JS().StreamInfo(bus.Stream)
		if err != nil {
			return false
		}
		info := deliveryConsumerInfo(t, natsClient)
		return info.AckFloor.Stream >= stream.State.LastSeq && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("the intake did not acknowledge the envelope: %+v", deliveryConsumerInfo(t, natsClient))
	})
	github.mu.Lock()
	github.failing = false
	github.mu.Unlock()
	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)

	if attributed, err := HeadAttributedTo(ctx, pool, issue, head); err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, #%d's head) after GitHub answers again and the reconcile's next pass = %v, %v, want true; the pull request is owed %d read(s): the database refused the owed record only at the cap's one delivery and took every write before and after it, so this is not the README's \"takes no write for the whole cap\"",
			issue, number, attributed, err, owedCount(t, ctx, pool, "acme/widgets", number))
	}
}

// The read a failed delivery records is deleted by the later delivery that writes the head, so a
// failure the intake got past costs the reconcile no read. GitHub answers the first delivery's read
// with a 500, which records the owed read, and the second delivery's read with the pull request,
// which writes the head. After it nothing is owed, and the reconcile's next pass reads nothing of
// GitHub for the pull request. The control is the delete taken out, with which one read stays owed.
func TestAHeadWrittenAfterAFailedDeliveryLeavesNothingOwed(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 4211)
	const (
		number = 4211
		head   = "4211421142114211421142114211421142114211"
	)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	branch, body := "legion/"+issue, "Dispatch: "+issue
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", number, "open", head, branch, body, at)
	github.mu.Lock()
	github.failing = true
	github.mu.Unlock()
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool),
		client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
		prEnvelope("acme/widgets", number, "opened", head, branch, body, at))

	// The first delivery has read GitHub and been refused; the redelivery comes 10 s later.
	waitFor(t, 30*time.Second, func() bool { return github.reads.Load() >= 1 }, func() string {
		return "the first delivery did not read GitHub"
	})
	time.Sleep(2 * time.Second)
	github.mu.Lock()
	github.failing = false
	github.mu.Unlock()
	awaitAcknowledged(t, natsClient)
	if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before; delivered != 2 {
		t.Fatalf("the envelope was delivered %d times, want 2: one refused by GitHub, one that writes the head", delivered)
	}
	if attributed, err := HeadAttributedTo(ctx, pool, issue, head); err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, #%d's head) after the second delivery = %v, %v; want true", issue, number, attributed, err)
	}
	if owed := owedCount(t, ctx, pool, "acme/widgets", number); owed != 0 {
		t.Errorf("#%d is owed %d read(s) after a delivery wrote its head, want none: the first delivery's record was not deleted", number, owed)
	}
	readsBefore := github.reads.Load()
	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
	if reads := github.reads.Load() - readsBefore; reads != 0 {
		t.Errorf("the reconcile's next pass read #%d from GitHub %d time(s), want none: its head was written", number, reads)
	}
}

// A message whose delivery count cannot be read (no JetStream metadata) has spent its deliveries:
// no count can cap it, so handing it back would have no limit. deliver then records the read its
// pull request is owed and acknowledges it, as at the cap.
func TestAMessageWithNoDeliveryCountHasSpentItsDeliveries(t *testing.T) {
	if _, spent := headWriteDeliveriesSpent(&natsgo.Msg{Subject: "notifications.github.acme.widgets.pr.7"}); !spent {
		t.Fatal("a message with no JetStream metadata is not spent, want spent: its hand-backs would have no limit")
	}
}
