package delivery

// An envelope the intake hands back to the bus holds one of its intakeMaxAckPending in-flight
// slots, and four of them stop every envelope behind them. So the intake hands back with a delay
// only the envelope that is the one record of its change, and acknowledges every other it
// handled: one whose read is owed (pull_request_reads_kept_test.go), and one no delivery can get
// past (acceptance of cybermonklabs/legion#4209, round 3, defect 5). Any caller of Envoy's /v1
// publish route can send an envelope (dispatch://INFRA-3911).

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// An envelope no delivery can get past is final at its first delivery: the intake acknowledges
// it with one log line and owes no read. Each case publishes intakeMaxAckPending such envelopes
// and then GitHub's own event of acme/widgets#7, and wants #7's head row within the 90 s of the
// tester's TestAcceptAnEnvelopeWhoseOwedReadCannotBeStoredDoesNotStopTheIntake, each of the four
// delivered once, and each of them its line.
//
// The first case is the tester's red test: numbers 3000000000 and up do not fit the integer
// columns, so at ac1460389fcc the first read of Postgres failed, the envelope was handed back to
// the cap, the owed record failed the same way, and it was handed back for ever. The next two
// name no pull request GitHub can have either. The fourth is the class past a check of the name:
// a pull request GitHub answers with a title holding a NUL byte, which Postgres refuses as a value
// (SQLSTATE 22021), so the head write fails at every delivery for the values alone; before the
// fix that cost each envelope the cap's deliveries and an owed read that fails the same way at
// every pass. The last four are names GitHub cannot have (review of cybermonklabs/legion#4209 at
// ceeec40f46, B4): the reviewer's probe, a repository name of 3,500 random characters, which the
// btree of pull_request_reads_owed refused (SQLSTATE 54000; a name Postgres compresses below the
// btree's limit, 3,500 of one letter, is not refused there), so at ceeec40f46 each was handed back
// for ever and #7 had no head (TestReview7AnOwedRecordTheIndexRefusesHoldsASlot:
// `head_7_after=0s deliveries=25 num_ack_pending=4`); an owner of 40 characters and a name of
// 101, which the table holds, so at ceeec40f46 each read GitHub and owed a refusal for a
// repository GitHub cannot have; and a character no repository name holds.
func TestAnEnvelopeNoDeliveryCanGetPastIsFinalAtItsFirstDelivery(t *testing.T) {
	const namesNoPullRequest = "names no pull request"
	for _, poison := range []struct {
		name   string
		repo   string
		number func(i int) int
		title  string
		// line is what the line each of these envelopes logs says, and level its level.
		line, level string
	}{
		{"number_3000000000_and_up", "acme/widgets", func(i int) int { return 3_000_000_000 + i }, "", namesNoPullRequest, "WARN"},
		{"number_zero_and_below", "acme/widgets", func(i int) int { return -i }, "", namesNoPullRequest, "WARN"},
		{"repository_not_owner_name", "acmewidgets", func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN"},
		{"an_answer_the_database_refuses", "acme/widgets", func(i int) int { return 900 + i }, "Change\x00", "Postgres refused the values", "ERROR"},
		{"repository_name_of_3500_random_characters", "acme/" + randomName(3500), func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN"},
		{"owner_of_40_characters", strings.Repeat("a", 40) + "/widgets", func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN"},
		{"repository_name_of_101_characters", "acme/" + strings.Repeat("w", 101), func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN"},
		{"a_character_no_repository_name_holds", "acme/wid gets", func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN"},
	} {
		t.Run(poison.name, func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
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

			// Each envelope is published on the subject its payload's repository names, as Envoy's
			// publish route builds one, since the intake takes no envelope whose subject names
			// another repository (intakeKey); a repository that is not owner/name names none.
			owner, name, found := strings.Cut(poison.repo, "/")
			if !found {
				owner, name = "acme", "widgets"
			}
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			for i := range intakeMaxAckPending {
				number := poison.number(i)
				if poison.title != "" {
					github.set("acme/widgets", number, "open", poisonHead, "feature/x", "", at)
					github.mu.Lock()
					github.pulls[number]["title"] = poison.title
					github.mu.Unlock()
				}
				publishPullRequestEnvelope(t, natsClient, owner, name, number,
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
				t.Errorf("%d reads owed, want none: an envelope no delivery can get past owes no read", owed)
			}
			lines := 0
			for _, record := range logs() {
				if message, _ := record["msg"].(string); record["level"] == poison.level && strings.Contains(message, poison.line) {
					lines++
				}
			}
			if lines != intakeMaxAckPending {
				t.Errorf("%d %s lines say %q, want one for each of the %d envelopes", lines, poison.level, poison.line, intakeMaxAckPending)
			}
		})
	}
}

// randomName is n letters and digits in an order no compression shortens, the same at every run.
func randomName(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	rng := rand.New(rand.NewPCG(4209, uint64(n)))
	name := make([]byte, n)
	for i := range name {
		name[i] = letters[rng.IntN(len(letters))]
	}
	return string(name)
}

// The owed record's failure is sorted by its cause (recordRefused; review of
// cybermonklabs/legion#4209 at ceeec40f46, B4). A failure the envelope's values cause, SQLSTATE
// class 22, 23 or 54, can never be written, so the envelope ends at its first delivery with one
// ERROR line and holds no slot. Every other failure is the database not taking a record now, and
// the envelope is handed back until it does (TestAnOwedRecordTheDatabaseRefusesAtSomeDeliveriesLosesNoHead,
// with the table locked, and the hand-back rows of intake_head_write_test.go, with SQLSTATE
// 55P03): that is the side this does not change. Each case makes every write of
// pull_request_reads_owed fail with one SQLSTATE, GitHub answers the pull requests of acme/widgets
// with a 500, so each of intakeMaxAckPending envelopes of acme/widgets needs its record, and then
// GitHub's own event of other/tools#7, which needs none, must have its head. At ceeec40f46 the
// classes 23 and 54 were handed back for ever, so #7 had none.
func TestAnOwedRecordRefusedForItsValuesEndsItsEnvelope(t *testing.T) {
	for _, sqlstate := range []string{"54000", "23505", "22001"} {
		t.Run("sqlstate_"+sqlstate, func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 7)
			fake := newFakeGitHub(t)
			acme := serveGitHubPulls(t, fake, "acme/widgets")
			other := serveGitHubPulls(t, fake, "other/tools")
			client := fake.newTestClient()
			acme.mu.Lock()
			acme.failing = true
			acme.mu.Unlock()
			refuseOwedReadsWith(t, ctx, pool, sqlstate)

			natsClient := intakeTestClient(t)
			startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool),
				client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
			awaitBoundDurable(t, natsClient)
			before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			for n := 1; n <= intakeMaxAckPending; n++ {
				publishPullRequestEnvelope(t, natsClient, "acme", "widgets", n,
					prEnvelope("acme/widgets", n, "opened", fakeSHA("a", n), "feature/x", "", at))
			}
			const head = "7777777777777777777777777777777777777777"
			branch, body := "legion/"+issue, "Dispatch: "+issue
			other.set("other/tools", 7, "open", head, branch, body, at)
			published := time.Now()
			publishPullRequestEnvelope(t, natsClient, "other", "tools", 7,
				prEnvelope("other/tools", 7, "opened", head, branch, body, at))
			waitFor(t, 30*time.Second, func() bool {
				return headRowCount(t, ctx, &Intake{pool: pool}, "other/tools", 7) == 1
			}, func() string {
				return fmt.Sprintf("30 s after GitHub's own event of other/tools#7, behind %d envelopes whose owed record Postgres refuses with SQLSTATE %s, #7 has no head row: they hold every in-flight slot; the durable is %+v",
					intakeMaxAckPending, sqlstate, deliveryConsumerInfo(t, natsClient))
			})
			t.Logf("sqlstate=%s other_tools_7_head_after=%s", sqlstate, time.Since(published).Round(time.Millisecond))
			awaitAcknowledged(t, natsClient)
			if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before; delivered != uint64(intakeMaxAckPending+1) {
				t.Errorf("the durable made %d deliveries of %d envelopes, want one each: an envelope whose record its values make Postgres refuse was handed back", delivered, intakeMaxAckPending+1)
			}
			// The one ERROR line of an envelope deliver ends for its record names the record's
			// failure (record_error); the hand-back's line, which also names it, is a WARN.
			lines := 0
			for _, record := range logs() {
				if _, named := record["record_error"]; named && record["level"] == "ERROR" {
					lines++
				}
			}
			if lines != intakeMaxAckPending {
				t.Errorf("%d ERROR lines name the owed read's failure, want one for each of the %d envelopes", lines, intakeMaxAckPending)
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
