package delivery

// An envelope the intake hands back to the bus holds one of its intakeMaxAckPending in-flight
// slots, and four of them stop every envelope behind them. So the intake hands back with a delay
// only the envelope that is the one record of its change, and acknowledges every other it
// handled: one whose read is owed (pull_request_reads_kept_test.go), and one no delivery can get
// past (acceptance of cybermonklabs/legion#4209, round 3, defect 5). Any caller of Envoy's /v1
// publish route can send an envelope (dispatch://INFRA-3911).

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// An envelope no delivery can get past is final at its first delivery: the intake acknowledges
// it and owes no read. Each case publishes intakeMaxAckPending such envelopes and then GitHub's
// own event of acme/widgets#7, and wants #7's head row within the 90 s of the tester's
// TestAcceptAnEnvelopeWhoseOwedReadCannotBeStoredDoesNotStopTheIntake, and each of the four
// delivered once.
//
// The first case is the tester's red test: numbers 3000000000 and up do not fit the integer
// columns, so at ac1460389fcc the first read of Postgres failed, the envelope was handed back to
// the cap, the owed record failed the same way, and it was handed back for ever. The second and
// third name no pull request a row can hold either. The last is the class past a check of the
// name: a pull request GitHub answers with a title holding a NUL byte, which Postgres refuses as a
// value (SQLSTATE 22021), so the head write fails at every delivery for the values alone; before
// the fix that cost each envelope the cap's deliveries and an owed read that fails the same way
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

// The read a failed delivery records is deleted by the later read of the intake that writes the
// head, so a failure the intake got past costs the reconcile no read. GitHub answers the opening
// envelope's read with a 500, which records the owed read and acknowledges the envelope; then
// GitHub answers, and the pull request's next event, a push, writes the head. After it nothing is
// owed, and the reconcile's next pass reads nothing of GitHub for the pull request. The control
// is the delete taken out, with which one read stays owed.
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
	awaitAcknowledged(t, natsClient)
	if owed := owedCount(t, ctx, pool, "acme/widgets", number); owed != 1 {
		t.Fatalf("#%d is owed %d read(s) after GitHub's 500 was acknowledged, want 1", number, owed)
	}

	pushed := at.Add(time.Second)
	github.set("acme/widgets", number, "open", head, branch, body, pushed)
	github.mu.Lock()
	github.failing = false
	github.mu.Unlock()
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
		prEnvelope("acme/widgets", number, "synchronize", head, branch, body, pushed))
	awaitAcknowledged(t, natsClient)
	if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before; delivered != 2 {
		t.Fatalf("the two envelopes were delivered %d times, want once each", delivered)
	}
	if attributed, err := HeadAttributedTo(ctx, pool, issue, head); err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, #%d's head) after the push = %v, %v; want true", issue, number, attributed, err)
	}
	if owed := owedCount(t, ctx, pool, "acme/widgets", number); owed != 0 {
		t.Errorf("#%d is owed %d read(s) after a read wrote its head, want none: the failed delivery's record was not deleted", number, owed)
	}
	readsBefore := github.reads.Load()
	NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
	if reads := github.reads.Load() - readsBefore; reads != 0 {
		t.Errorf("the reconcile's next pass read #%d from GitHub %d time(s), want none: its head was written", number, reads)
	}
}
