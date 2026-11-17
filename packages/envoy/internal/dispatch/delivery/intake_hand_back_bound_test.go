package delivery

// An envelope the intake hands back to the bus holds one of its intakeMaxAckPending in-flight
// slots, and four of them stop every envelope behind them. So the intake hands back with a delay
// only the envelope that is the one record of its change, and acknowledges every other it
// handled: one whose read is owed (pull_request_reads_kept_test.go), and one no delivery can get
// past (acceptance of cybermonklabs/legion#4209, round 3, defect 5). Any caller of Envoy's /v1
// publish route can send an envelope (dispatch://INFRA-3911).

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
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
// a pull request GitHub answers with a title holding a NUL byte, which no text column holds
// (Postgres refuses it with SQLSTATE 22021), so the head write would fail at every delivery for
// the values alone; before the fix that cost each envelope the cap's deliveries and an owed read
// that fails the same way at every pass, and since the review of cybermonklabs/legion#4209 at
// ef5112db43 (B5) the head row's own check refuses it before any statement, where until then a
// sort of Postgres's SQLSTATE took it for the values (valuesRefused). The next four are names
// GitHub cannot have (review of cybermonklabs/legion#4209 at ceeec40f46, B4): the reviewer's
// probe, a repository name of 3,500 random characters, which the btree of pull_request_reads_owed
// refused (SQLSTATE 54000; a name Postgres compresses below the btree's limit, 3,500 of one
// letter, is not refused there), so at ceeec40f46 each was handed back for ever and #7 had no head
// (TestReview7AnOwedRecordTheIndexRefusesHoldsASlot: `head_7_after=0s deliveries=25
// num_ack_pending=4`); an owner of 40 characters and a name of 101, which the table holds, so at
// ceeec40f46 each read GitHub and owed a refusal for a repository GitHub cannot have; and a
// character no repository name holds. The last two are such names reached through a rename: the
// envelope names a repository GitHub can have, and repo_projects maps it to a canonical name the
// settings route bounded only to `owner/name` (api's externalRef), of 3,500 random characters and
// of 101. With no failure of the owed record ending an envelope, the first of them was handed back
// for ever and the second owed a refusal for a repository GitHub cannot have, until the rows'
// values were checked where the rows are built.
func TestAnEnvelopeNoDeliveryCanGetPastIsFinalAtItsFirstDelivery(t *testing.T) {
	const (
		namesNoPullRequest = "names no pull request"
		noRowHolds         = "a value no row holds"
	)
	for _, poison := range []struct {
		name   string
		repo   string
		number func(i int) int
		title  string
		// line is what the line each of these envelopes logs says, and level its level.
		line, level string
		// renamedTo, where set, is the canonical name repo_projects maps repo to.
		renamedTo string
	}{
		{"number_3000000000_and_up", "acme/widgets", func(i int) int { return 3_000_000_000 + i }, "", namesNoPullRequest, "WARN", ""},
		{"number_zero_and_below", "acme/widgets", func(i int) int { return -i }, "", namesNoPullRequest, "WARN", ""},
		{"repository_not_owner_name", "acmewidgets", func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN", ""},
		{"an_answer_the_database_refuses", "acme/widgets", func(i int) int { return 900 + i }, "Change\x00", noRowHolds, "ERROR", ""},
		{"repository_name_of_3500_random_characters", "acme/" + randomName(3500), func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN", ""},
		{"owner_of_40_characters", strings.Repeat("a", 40) + "/widgets", func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN", ""},
		{"repository_name_of_101_characters", "acme/" + strings.Repeat("w", 101), func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN", ""},
		{"a_character_no_repository_name_holds", "acme/wid gets", func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN", ""},
		{"a_rename_to_a_name_of_3500_random_characters", renamedAwayRepo, func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN", "acme/" + randomName(3500)},
		{"a_rename_to_a_name_of_101_characters", renamedAwayRepo, func(i int) int { return 900 + i }, "", namesNoPullRequest, "WARN", "acme/" + strings.Repeat("r", 101)},
	} {
		t.Run(poison.name, func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 4209)
			if poison.renamedTo != "" {
				renameRepository(t, ctx, pool, poison.repo, poison.renamedTo)
			}
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

// No failure of the owed record ends an envelope once the envelope's name is checked
// (pullRequestNamed): R4 of dispatch://INFRA-3992, "While Dispatch's database cannot take that
// record, the bus keeps the event, for as long as the bus keeps events." The values of the record
// are the repository and the number alone, which that check holds to what the table takes
// (TestEachValueTheNameCheckTakesIsARowTheTablesTake), so a failure of its insert says nothing of
// them, whatever its SQLSTATE. A unique violation (23505) of the upsert, for one, is transient:
// PostgreSQL's page on INSERT says `ON CONFLICT` can raise one while a unique index of the table is
// built or rebuilt concurrently, and the review of cybermonklabs/legion#4209 at ef5112db43 counted
// 6 and 6 of them beside `REINDEX INDEX CONCURRENTLY` (B5; its red team's F1). At ef5112db43 a
// code of class 22, 23 or 54 was read as the values' own fault, and the envelope was acknowledged
// at its first delivery with nothing owed, so the pull request had no head until its next event.
//
// Each case makes every write of pull_request_reads_owed fail with one SQLSTATE over two
// deliveries, with GitHub answering the pull request's read with a 500, and then lets the table
// take writes again. The envelope must be handed back while the fault lasts, owed once the
// database takes the record, and acknowledged; once GitHub answers, one reconcile pass writes the
// head of that same pull request, with no new event.
func TestAnOwedRecordRefusedForAnyCauseLosesNoHead(t *testing.T) {
	for _, sqlstate := range []string{"23505", "54000", "22001"} {
		t.Run("sqlstate_"+sqlstate, func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 4213)
			const (
				number = 4213
				head   = "4213421342134213421342134213421342134213"
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
			restore := refuseOwedReadsWith(t, ctx, pool, sqlstate)

			natsClient := intakeTestClient(t)
			startIntake(t, newIntake(natsClient, pool, deliveryTestLeases(t, pool),
				client, events.NewBroker(), loadedSettingsCache(t, ctx, pool)))
			awaitBoundDurable(t, natsClient)
			before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
			publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
				prEnvelope("acme/widgets", number, "opened", head, branch, body, at))

			// The second delivery is the first hand-back's; it comes headWriteRedeliverAfter after
			// the first. An envelope ended at its first delivery is never delivered again.
			faulted := uint64(0)
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if faulted = deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before; faulted >= 2 {
					break
				}
			}
			restore()

			var info string
			waitFor(t, 60*time.Second, func() bool {
				stream, err := natsClient.JS().StreamInfo(bus.Stream)
				if err != nil {
					return false
				}
				consumer := deliveryConsumerInfo(t, natsClient)
				info = fmt.Sprintf("%+v", consumer)
				return consumer.AckFloor.Stream >= stream.State.LastSeq && consumer.NumAckPending == 0
			}, func() string {
				return "the intake did not acknowledge the envelope within 60 s of the database taking its owed read: " + info
			})
			delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before
			owed := owedCount(t, ctx, pool, "acme/widgets", number)
			github.mu.Lock()
			github.failing = false
			github.mu.Unlock()
			NewReconcile(pool, client, events.NewBroker()).runOnce(ctx)
			attributed, err := HeadAttributedTo(ctx, pool, issue, head)
			t.Logf("sqlstate=%s deliveries_while_refused=%d deliveries=%d owed_after_acknowledgement=%d head_is_basis_after_one_pass=%v",
				sqlstate, faulted, delivered, owed, attributed)
			if faulted < 2 {
				t.Errorf("the envelope was delivered %d time(s) in 30 s while the database refused its owed record with SQLSTATE %s, want it handed back: it was ended with nothing owed", faulted, sqlstate)
			}
			if owed != 1 {
				t.Errorf("#%d was owed %d read(s) when the envelope was acknowledged, want 1: the envelope may go only once its record is written", number, owed)
			}
			if err != nil || !attributed {
				t.Fatalf("HeadAttributedTo(%s, #%d's head) after the fault ends, GitHub answers again and one reconcile pass = %v, %v, want true: the database refused the owed record with SQLSTATE %s for a time, and that lost the head", issue, number, attributed, err, sqlstate)
			}
			// While the envelope is handed back, the operator sees the hand-back's WARN line at its
			// first delivery, naming the record's failure; the second delivery's is DEBUG.
			var lines []map[string]any
			for _, record := range logs() {
				if message, _ := record["msg"].(string); record["level"] == "WARN" && strings.Contains(message, "handing the envelope back") {
					lines = append(lines, record)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("%d WARN lines say the envelope is handed back over %d hand-backs, want 1: the line is written at the first delivery and then once every %d", len(lines), faulted, handBackLineEvery)
			}
			if cause, _ := lines[0]["record_error"].(string); lines[0]["deliveries"] != float64(1) || !strings.Contains(cause, "SQLSTATE "+sqlstate) {
				t.Errorf("the hand-back's WARN line = %v, want it at delivery 1 and naming the record's failure, SQLSTATE %s", lines[0], sqlstate)
			}
		})
	}
}

// The hand-back's WARN line is written at an envelope's first delivery and once every
// handBackLineEvery deliveries after it, and at DEBUG at every other (logHandBack): an envelope
// handed back every headWriteRedeliverAfter for the 72 hours of the bus is delivered up to 25,920
// times. A delivery whose count cannot be read logs at WARN.
func TestAHandedBackEnvelopeLogsItsCauseOnceEveryThirtyDeliveries(t *testing.T) {
	logs := captureLogs(t)
	cause := errors.New("ERROR: the test refuses writes of pull_request_reads_owed (SQLSTATE 55P03)")
	for delivered := 1; delivered <= 61; delivered++ {
		// Msg.Metadata reads the count from the reply subject of a message a subscription
		// delivered, as JetStream writes it: $JS.ACK.<stream>.<consumer>.<delivered>.<stream
		// sequence>.<consumer sequence>.<time>.<pending>.
		logHandBack(&natsgo.Msg{Subject: "notifications.github.acme.widgets.pr.7", Sub: &natsgo.Subscription{},
			Reply: fmt.Sprintf("$JS.ACK.ENVOY.dispatch-delivery.%d.%d.%d.1794892314678155619.0", delivered, delivered, delivered)},
			"acme/widgets", 7, ErrHeadNotStored, cause)
	}
	logHandBack(&natsgo.Msg{Subject: "notifications.github.acme.widgets.pr.8"}, "acme/widgets", 8, ErrHeadNotStored, cause)
	var warned []float64
	for _, record := range logs() {
		if record["level"] != "WARN" {
			continue
		}
		if cause, _ := record["record_error"].(string); !strings.Contains(cause, "SQLSTATE 55P03") {
			t.Errorf("a hand-back line %v does not name the record's failure", record)
		}
		delivered, _ := record["deliveries"].(float64)
		warned = append(warned, delivered)
	}
	if want := []float64{1, 31, 61, 0}; !slices.Equal(warned, want) {
		t.Errorf("61 deliveries of one envelope and one delivery with no count wrote WARN lines at deliveries %v, want %v", warned, want)
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
