package delivery

// An envelope the intake hands back to the bus holds one of its intakeMaxAckPending in-flight
// slots, and four of them stop every envelope behind them. So the intake hands back with a delay
// only the envelope that is the one record of its change, and acknowledges every other it
// handled: one whose read is owed (pull_request_reads_kept_test.go), and one no delivery can get
// past (acceptance of cybermonklabs/legion#4209, round 3, defect 5). Any caller of Envoy's /v1
// publish route can send an envelope.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
// name no pull request GitHub can have either. The next four are names GitHub cannot have (review
// of cybermonklabs/legion#4209 at ceeec40f46, B4): the reviewer's probe, a repository name of
// 3,500 random characters, which the btree of pull_request_reads_owed refused (SQLSTATE 54000; a
// name Postgres compresses below the btree's limit, 3,500 of one letter, is not refused there), so
// at ceeec40f46 each was handed back for ever and #7 had no head
// (TestReview7AnOwedRecordTheIndexRefusesHoldsASlot: `head_7_after=0s deliveries=25
// num_ack_pending=4`); an owner of 40 characters and a name of 101, which the table holds, so at
// ceeec40f46 each read GitHub and owed a refusal for a repository GitHub cannot have; and a
// character no repository name holds. The last two are such names as a rename pair's new name:
// the envelope names a repository GitHub can have, and repo_projects renames it to a name of
// 3,500 random characters and of 101, as only SQL by hand writes a pair. With no failure of the
// owed record ending an envelope, the first of them was handed back for ever and the second owed
// a refusal for a repository GitHub cannot have, until the rows' values were checked where the
// rows are built; at 3a21edb2c9 each was then acknowledged with nothing owed, so its head never
// came, also after the target was corrected (acceptance of cybermonklabs/legion#4209 at
// 3a21edb2c9, D1). The boot now drops such a pair (refs.NewRenames), so the envelope's own name is
// its canonical name: it is read from GitHub under that name, which GitHub answers 404 for here,
// and it is acknowledged at its first delivery with its read owed under that name and the line
// that says so, and the reconcile reads it once the target is corrected
// (TestAnEventWhileAStoredRenameTargetBreaksTheNameRuleIsReadOnceTheTargetIsCorrected). A GitHub
// answer no head row can hold is no such envelope either: its read is owed
// (TestAnAnswerNoRowHoldsIsOwedAtTheIntakeAndAtTheReconcile).
func TestAnEnvelopeNoDeliveryCanGetPastIsFinalAtItsFirstDelivery(t *testing.T) {
	const (
		namesNoPullRequest = "names no pull request"
		gitHubGaveNone     = "GitHub gave no pull request for this read"
	)
	for _, poison := range []struct {
		name   string
		repo   string
		number func(i int) int
		// renamedTo, where set, is the new name a repo_projects row renames repo to, which the
		// boot drops, and the read of each envelope is owed under repo.
		renamedTo string
	}{
		{"number_3000000000_and_up", "acme/widgets", func(i int) int { return 3_000_000_000 + i }, ""},
		{"number_zero_and_below", "acme/widgets", func(i int) int { return -i }, ""},
		{"repository_not_owner_name", "acmewidgets", func(i int) int { return 900 + i }, ""},
		{"repository_name_of_3500_random_characters", "acme/" + randomName(3500), func(i int) int { return 900 + i }, ""},
		{"owner_of_40_characters", strings.Repeat("a", 40) + "/widgets", func(i int) int { return 900 + i }, ""},
		{"repository_name_of_101_characters", "acme/" + strings.Repeat("w", 101), func(i int) int { return 900 + i }, ""},
		{"a_character_no_repository_name_holds", "acme/wid gets", func(i int) int { return 900 + i }, ""},
		{"a_rename_to_a_name_of_3500_random_characters", renamedAwayRepo, func(i int) int { return 900 + i }, "acme/" + randomName(3500)},
		{"a_rename_to_a_name_of_101_characters", renamedAwayRepo, func(i int) int { return 900 + i }, "acme/" + strings.Repeat("r", 101)},
	} {
		t.Run(poison.name, func(t *testing.T) {
			fastPageRetries(t)
			logs := captureLogs(t)
			pool, ctx := deliveryTestPool(t)
			issue := seedHeadIssue(t, ctx, pool, 4209)
			attributor := unrenamed()
			if poison.renamedTo != "" {
				renameRepository(t, ctx, pool, poison.repo, poison.renamedTo)
				attributor = NewAttributor(renamesOf(t, ctx, pool))
			}
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "acme/widgets")
			client := fake.newTestClient()
			natsClient := intakeTestClient(t)
			startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
				client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), attributor))
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
				t.Errorf("the durable made %d deliveries of %d envelopes, want one each: an envelope no delivery can get past was handed back", delivered, intakeMaxAckPending+1)
			}
			var owed, owedUnderGiven int
			if err := pool.QueryRow(ctx, `select count(*), count(*) filter (where repo = $1) from pull_request_reads_owed`, poison.repo).Scan(&owed, &owedUnderGiven); err != nil {
				t.Fatal(err)
			}
			wantOwed, wantLevel, wantLine := 0, "WARN", namesNoPullRequest
			if poison.renamedTo != "" {
				wantOwed, wantLevel, wantLine = intakeMaxAckPending, "INFO", gitHubGaveNone
			}
			if owed != wantOwed || owedUnderGiven != wantOwed {
				t.Errorf("%d reads owed, %d of them under %.50q, want %d under it: an envelope no delivery can get past owes no read, and one whose rename pair the boot dropped is owed under the name it gave", owed, owedUnderGiven, poison.repo, wantOwed)
			}
			lines := 0
			for _, record := range logs() {
				if message, _ := record["msg"].(string); record["level"] == wantLevel && strings.Contains(message, wantLine) {
					lines++
				}
				if poison.renamedTo != "" && strings.Contains(fmt.Sprint(record), poison.renamedTo) {
					t.Errorf("a line names the new name %.20q… of the pair the boot dropped: %.300v", poison.renamedTo, record)
				}
			}
			if lines != intakeMaxAckPending {
				t.Errorf("%d %s lines say %q, want one for each of the %d envelopes", lines, wantLevel, wantLine, intakeMaxAckPending)
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

// A GitHub answer that no head row can hold (errNoRowHolds, checkHeadRow) is a head not stored,
// at the intake as at the reconcile: the intake owes the pull request a read and acknowledges the
// envelope once that is written, and the reconcile keeps the read owed and reads it again every
// pass: no age of a failure and no count of tries ends a read that failed. GitHub answers the
// pull request with a head branch that holds a NUL byte, which no text column holds. A branch is
// a value of identity, so the write refuses it; a title with a NUL is a text of free form, which
// the write stores with U+FFFD (freeText). At def907052c the intake acknowledged such an envelope
// with one ERROR line and owed nothing, while the reconcile kept the same answer owed (review of
// cybermonklabs/legion#4209 at def907052c, N1).
func TestAnAnswerNoRowHoldsIsOwedAtTheIntakeAndAtTheReconcile(t *testing.T) {
	fastPageRetries(t)
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	const number = 4214
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	client := fake.newTestClient()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	github.set("acme/widgets", number, "open", poisonHead, "feature/x\x00", "", at)
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
		client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
		prEnvelope("acme/widgets", number, "opened", poisonHead, "feature/x", "", at))
	awaitAcknowledged(t, natsClient)

	owed := func() (reason string, reads int) {
		t.Helper()
		err := pool.QueryRow(ctx, `select reason, reads from pull_request_reads_owed where repo = 'acme/widgets' and number = $1`, number).Scan(&reason, &reads)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return reason, reads
	}
	delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before
	reason, reads := owed()
	lines := 0
	for _, record := range logs() {
		message, _ := record["msg"].(string)
		cause, _ := record["error"].(string)
		if record["level"] == "WARN" && strings.Contains(message, "the read it is owed is recorded") && strings.Contains(cause, "a value no row holds") {
			lines++
		}
	}
	t.Logf("intake: deliveries=%d owed=%q reads=%d recorded_lines=%d", delivered, reason, reads, lines)
	if delivered != 1 {
		t.Errorf("the envelope was delivered %d times, want once: it is acknowledged once its owed read is written", delivered)
	}
	if reason != string(owedFailure) || reads != 0 {
		t.Errorf("after the intake acknowledged the envelope, #%d is owed %q with %d reads, want %q with none: a GitHub answer no head row holds is a head not stored", number, reason, reads, owedFailure)
	}
	if lines != 1 {
		t.Errorf("%d WARN lines say the read it is owed is recorded, naming a value no row holds, want 1", lines)
	}

	NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)
	reason, reads = owed()
	heads := headRowCount(t, ctx, &Intake{pool: pool}, "acme/widgets", number)
	t.Logf("reconcile: owed=%q reads=%d heads=%d", reason, reads, heads)
	if reason != string(owedFailure) || reads != 1 || heads != 0 {
		t.Errorf("after one reconcile pass #%d is owed %q with %d reads and has %d head rows, want %q with 1 read and none: the pass reads it again, and the same answer keeps it owed", number, reason, reads, heads, owedFailure)
	}
}

// An event of a repository whose stored rename target is no name GitHub can hold (SQL by hand,
// which nothing checks, since no route writes a pair) is owed a read under the repository's own
// name, which a read can reach, and the reconcile of a process that boots once an operator has
// corrected the target reads it: no path of the intake ends an envelope with nothing owed while
// its head is not stored. The boot drops such a pair (refs.NewRenames), so while the target stands
// the old name is its own canonical name. #4243 has no row; #4241 has one, stored under the old
// name before the target went bad, and its event moves its head. While the target stands, GitHub
// answers neither under the old name, the repository having moved, so a pass reads both and both
// stay owed; after the correction, one pass of a process that read the corrected pair writes both
// heads under the new name. At 3a21edb2c9 the intake acknowledged each event with one WARN line
// and owed nothing, so #4243's head never came and #4241 kept its older head, also after the
// correction (acceptance of cybermonklabs/legion#4209 at 3a21edb2c9, D1).
func TestAnEventWhileAStoredRenameTargetBreaksTheNameRuleIsReadOnceTheTargetIsCorrected(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const (
		old, renamed = "acme/legacy", "acme/renamed"
		opened       = 4243
		pushed       = 4241
		before       = "4241000000000000000000000000000000004241"
		after        = "4241424142414241424142414241424142414241"
		openedHead   = "4243424342434243424342434243424342434243"
	)
	refused := "acme/" + strings.Repeat("x", 101)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, renamed)
	client := fake.newTestClient()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	earlier := at.Add(-time.Hour)
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: old, Number: pushed, HeadSHA: before, HeadRef: "feature/x", BaseRef: "main", State: "open",
		EventAt: &earlier, CheckedAt: &earlier, GitHubReadAt: &earlier,
	}); err != nil {
		t.Fatalf("store #%d under %s before the target went bad: %v", pushed, old, err)
	}
	renameRepository(t, ctx, pool, old, refused)
	standingRenames := renamesOf(t, ctx, pool)
	if dropped := standingRenames.Dropped(); len(dropped) != 1 || dropped[0].From != old || standingRenames.Canonical(old) != old {
		t.Fatalf("the boot's Renames dropped %v and calls %s %q, want the one pair %s to %.20q… dropped and the old name its own", dropped, old, standingRenames.Canonical(old), old, refused)
	}
	standing := NewAttributor(standingRenames)
	github.set(renamed, pushed, "open", after, "feature/x", "", at)
	github.set(renamed, opened, "open", openedHead, "feature/y", "", at)

	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
		client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), standing))
	awaitBoundDurable(t, natsClient)
	publishPullRequestEnvelope(t, natsClient, "acme", "legacy", pushed,
		prEnvelope(old, pushed, "synchronize", after, "feature/x", "", at))
	publishPullRequestEnvelope(t, natsClient, "acme", "legacy", opened,
		prEnvelope(old, opened, "opened", openedHead, "feature/y", "", at))
	awaitAcknowledged(t, natsClient)
	for _, number := range []int{pushed, opened} {
		if owed := owedCount(t, ctx, pool, old, number); owed != 1 {
			t.Errorf("#%d is owed %d read(s) under %s once its event is acknowledged while the stored target %.20q… stands, want 1: the envelope was ended with nothing owed", number, owed, old, refused)
		}
	}

	NewReconcile(pool, client, events.NewBroker(), standing).runOnce(ctx)
	for _, number := range []int{pushed, opened} {
		if owed := owedCount(t, ctx, pool, old, number); owed != 1 {
			t.Errorf("#%d is owed %d read(s) after a pass while the target stands, want 1: no read succeeded", number, owed)
		}
		if heads := headRowCount(t, ctx, &Intake{pool: pool}, renamed, number); heads != 0 {
			t.Errorf("#%d has %d head rows under %s while the target stands, want none", number, heads, renamed)
		}
	}

	renameRepository(t, ctx, pool, old, renamed)
	corrected := NewAttributor(renamesOf(t, ctx, pool))
	NewReconcile(pool, client, events.NewBroker(), corrected).runOnce(ctx)
	for _, want := range []struct {
		number int
		head   string
	}{{pushed, after}, {opened, openedHead}} {
		heads := headRowCount(t, ctx, &Intake{pool: pool}, renamed, want.number)
		owed := owedCount(t, ctx, pool, old, want.number)
		var head string
		if heads == 1 {
			head = readHead(t, corrected, ctx, pool, renamed, want.number).HeadSHA
		}
		t.Logf("after the correction and one pass: #%d heads=%d head=%.12s owed=%d", want.number, heads, head, owed)
		if heads != 1 || head != want.head {
			t.Errorf("#%d after the correction and one pass has %d head rows under %s at %q, want one at %s: the event that came while the target stood is lost", want.number, heads, renamed, head, want.head[:12])
		}
		if owed != 0 {
			t.Errorf("#%d is still owed %d read(s) under %s after its head was written, want none", want.number, owed, old)
		}
		if stale := headRowCount(t, ctx, &Intake{pool: pool}, old, want.number); stale != 0 {
			t.Errorf("#%d keeps %d row(s) under the old name %s, want none: one pull request keeps one row", want.number, stale, old)
		}
	}
}

// A pull request GitHub gives no answer for, because it has no such pull request or because the
// App cannot read the repository, is owed a read of kind refusal, and the INFO line that says so
// is written once, after that owe is written: the line says the reconcile reads it again, which is
// true only then. While the owe cannot be written the envelope is handed back, and the line of
// each such delivery is the hand-back's, under its cadence. At def907052c the INFO line came before
// the owe, so it was written at every hand-back, every headWriteRedeliverAfter, and said the
// reconcile reads the pull request again when nothing was owed (review of
// cybermonklabs/legion#4209 at def907052c, N2).
//
// The database refuses every owe of the first two deliveries and none after: the fault ends at the
// end of the second delivery's last owe, inside that delivery (oweWaitTracer's lift). A delivery
// tries two owes, the refusal's (oweRefusedRead) and, once that is refused, the failure's
// (deliver's oweEnvelopesRead); a fault that ended between the two would take the failure's and
// acknowledge the envelope with no refusal owed and no INFO line, which is right for that order
// and is not the case read here. When the test ended the fault by time - as soon as NATS had made
// the second delivery, and later a second after a hand-back's line - a run under -race, and a
// lift held up past that second, ended it there.
func TestAPullRequestGitHubGivesNoAnswerForLogsOneLineOnceItsReadIsOwed(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		refuse func(fake *fakeGitHub)
	}{
		{"github_has_no_such_pull_request", func(*fakeGitHub) {}},
		{"the_app_cannot_read_the_repository", func(fake *fakeGitHub) { fake.notInstalled.Store("acme/widgets", true) }},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			fastPageRetries(t)
			// DEBUG too, since the second hand-back's line is DEBUG (handBackLineDue).
			logs := captureLogsAt(t, slog.LevelDebug)
			pool, ctx := deliveryTestPool(t)
			const number = 4215
			fake := newFakeGitHub(t)
			serveGitHubPulls(t, fake, "acme/widgets")
			refusal.refuse(fake)
			client := fake.newTestClient()
			fault := refuseOwedReads(t, ctx, pool)
			owes := &oweWaitTracer{lift: fault, liftOn: owedFailure, liftAt: faultedDeliveries}
			natsClient := intakeTestClient(t)
			tuningFor(t).redeliverAfter = faultedRedelivery
			intake := newIntake(tuningFor(t), natsClient, storetest.Traced(t, pool, owes, nil), deliveryTestLeases(t, pool),
				client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
			startIntake(t, intake)
			awaitBoundDurable(t, natsClient)
			before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
				prEnvelope("acme/widgets", number, "opened", poisonHead, "feature/x", "", at))

			// The fault ends at the end of the second delivery's last owe, inside that delivery
			// (oweWaitTracer's lift), so both owes of deliveries 1 and 2 are refused and none of
			// delivery 3's: the third delivery owes GitHub's refusal, whenever it comes.
			waitFor(t, 30*time.Second, func() bool { return len(handBackSegments(logs())) >= faultedDeliveries }, func() string {
				return fmt.Sprintf("the envelope was handed back %d time(s) in 30 s while its owed read could not be written, want %d: %+v",
					len(handBackSegments(logs())), faultedDeliveries, deliveryConsumerInfo(t, natsClient))
			})
			awaitAcknowledged(t, natsClient)
			faulted := len(handBackSegments(logs()))
			delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before
			var reason string
			if err := pool.QueryRow(ctx, `select reason from pull_request_reads_owed where repo = 'acme/widgets' and number = $1`, number).Scan(&reason); err != nil {
				t.Fatalf("read the read #%d is owed after the envelope was acknowledged: %v", number, err)
			}
			var said []map[string]any
			for _, record := range logs() {
				if message, _ := record["msg"].(string); record["level"] == "INFO" && strings.Contains(message, "the reconcile reads it again") {
					said = append(said, record)
				}
			}
			t.Logf("hand_backs_while_refused=%d deliveries=%d owed=%q info_lines=%d", faulted, delivered, reason, len(said))
			if faulted != faultedDeliveries {
				t.Errorf("the envelope was handed back %d times, want %d: the fault ends inside delivery %d", faulted, faultedDeliveries, faultedDeliveries)
			}
			if reason != string(owedRefusal) {
				t.Errorf("#%d is owed %q, want %q", number, reason, owedRefusal)
			}
			if len(said) != 1 {
				t.Fatalf("%d INFO lines over %d deliveries say the reconcile reads the pull request again, want 1, written once the read is owed: %v", len(said), delivered, said)
			}
			if said[0]["repo"] != "acme/widgets" || said[0]["number"] != float64(number) {
				t.Errorf("the line %v does not name acme/widgets#%d", said[0], number)
			}
		})
	}
}

// faultedRedelivery is the hand-back delay of an intake whose test faults pull_request_reads_owed
// for its first deliveries: the fault ends inside the last faulted delivery (oweWaitTracer's
// lift), so the delay decides no outcome and only keeps the test short.
const faultedRedelivery = 100 * time.Millisecond

// faultedDeliveries is how many deliveries TestAPullRequestGitHubGivesNoAnswerForLogsOneLineOnceItsReadIsOwed
// faults: two, so the envelope is handed back twice, once at the cadence's WARN and once at DEBUG.
const faultedDeliveries = 2

// The owe of a read GitHub refused waits on a lock no longer than the statements beside it do
// (owedTableBound), so a lock of pull_request_reads_owed holds a delivery, and one of the
// intakeMaxAckPending slots its repository shares, for seconds and not for the message's whole
// deadline; the delivery then hands the envelope back, and the hand-back line names GitHub's
// refusal and the record's failure. Another session holds the table in ACCESS EXCLUSIVE mode, the
// intake runs on its own pool with no lock_timeout, and the message's deadline is ten minutes, so
// a delivery whose owe waits on that deadline writes no hand-back line in the minute the test
// waits. The test times each owe's statement on the intake's pool: the refused read's owe
// (reason refusal) must give up no later than the owe beside it, the failure deliver owes next,
// and a margin for a loaded machine. At 3a21edb2c9 the owe of the refused read ran on the
// message's context and waited out a 60 s lock, where the other statements fail after 5 s
// (acceptance of cybermonklabs/legion#4209 at 3a21edb2c9, its last note); an owe of the refused
// read bounded at 25 s beside one bounded at 5 s passed the test's earlier form, which measured
// only the minute (the deep audit of ac562e9837, finding 1). The lock ends at the end of the first
// delivery's last owe, the failure's, inside that delivery (oweWaitTracer's lift), so it never
// ends between two owes of one delivery, where the owe beside would take the read as a failure.
func TestTheOweOfARefusedReadWaitsOnALockNoLongerThanTheStatementsBesideIt(t *testing.T) {
	fastPageRetries(t)
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	tuningFor(t).messageTimeout = 10 * time.Minute
	const number = 4217
	fake := newFakeGitHub(t)
	serveGitHubPulls(t, fake, "acme/widgets")
	shrinkOwedReadWriteTimeout(t)
	release := lockOwedReads(t, ctx, pool)
	owes := &oweWaitTracer{lift: release, liftOn: owedFailure, liftAt: 1}
	natsClient := intakeTestClient(t)
	tuningFor(t).redeliverAfter = faultedRedelivery
	intake := newIntake(tuningFor(t), natsClient, storetest.Traced(t, pool, owes, nil), deliveryTestLeases(t, pool),
		fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
	startIntake(t, intake)
	awaitBoundDurable(t, natsClient)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	published := time.Now()
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
		prEnvelope("acme/widgets", number, "opened", poisonHead, "feature/x", "", at))
	var line map[string]any
	waitFor(t, time.Minute, func() bool {
		for _, record := range logs() {
			if message, _ := record["msg"].(string); record["level"] == "WARN" && strings.Contains(message, "handing the envelope back") {
				line = record
				return true
			}
		}
		return false
	}, func() string {
		return fmt.Sprintf("no hand-back line in the minute after the event of #%d, while pull_request_reads_owed is locked: the owe of GitHub's refusal waits on the message's deadline (%s)", number, intakeMessageTimeout)
	})
	took := time.Since(published)
	refused, beside := owes.first(owedRefusal), owes.first(owedFailure)
	awaitAcknowledged(t, natsClient)
	var reason string
	if err := pool.QueryRow(ctx, `select reason from pull_request_reads_owed where repo = 'acme/widgets' and number = $1`, number).Scan(&reason); err != nil {
		t.Fatalf("read the read #%d is owed once the lock ended: %v", number, err)
	}
	cause, _ := line["error"].(string)
	record, _ := line["record_error"].(string)
	t.Logf("hand_back_after=%s refused_owe_waited=%s owe_beside_waited=%s owed=%q error=%q record_error=%q",
		took.Round(time.Millisecond), refused.Round(time.Millisecond), beside.Round(time.Millisecond), reason, cause, record)
	const margin = 3 * time.Second
	if refused == 0 || beside == 0 {
		t.Fatalf("the refused read's owe waited %s and the owe beside it %s, want both timed: the case did not happen", refused, beside)
	}
	if refused > beside+margin {
		t.Errorf("the owe of GitHub's refusal waited %s on the lock, want no longer than the owe beside it (%s) and %s",
			refused.Round(time.Millisecond), beside.Round(time.Millisecond), margin)
	}
	if !strings.Contains(cause, "status 404") || !strings.Contains(cause, "is owed") || !strings.Contains(record, "is owed") {
		t.Errorf("the hand-back line's error %q and record_error %q, want GitHub's refusal (status 404) and the record's failure", cause, record)
	}
	if reason != string(owedRefusal) {
		t.Errorf("#%d is owed %q once the lock ended, want %q", number, reason, owedRefusal)
	}
}

// oweWaitTracer times each owe of a read on the pool it traces: the statement of
// owePullRequestRead that writes pull_request_reads_owed, from the moment pgx sends it to its
// answer, by the reason it owes.
//
// Where lift is set, it also ends a test's fault of pull_request_reads_owed, and decides the
// fault per delivery rather than by time: it calls lift at the end of the liftAt-th owe of reason
// liftOn. A delivery whose owes the fault refuses tries the refusal's owe and then the failure's,
// or the failure's alone, and hands the envelope back only after its last owe has answered, so
// with liftOn the failure every owe of the first liftAt deliveries meets the fault and no owe of
// a later delivery does: the lift runs inside the delivery that makes that owe, before it hands
// the envelope back, so no goroutine of the test, however late it runs, can end the fault between
// two owes of one delivery (the review of cybermonklabs/legion#4209 at e5412e6626, N1: two owes
// 500 ms apart and a lift 1.2 s late failed the test that lifted after a one-second gap).
type oweWaitTracer struct {
	lift   func()
	liftOn owedReason
	liftAt int

	mu    sync.Mutex
	waits []oweWait
	ended int
}

type oweWait struct {
	reason string
	took   time.Duration
}

// oweStart is one owe's statement as pgx sent it: its reason and the moment it was sent.
type oweStart struct {
	reason string
	at     time.Time
}

type oweStartKey struct{}

// TraceQueryStart notes the start of each owe that owePullRequestRead sends: its four arguments
// are the repository, the number, the reason and the word of a refusal.
func (o *oweWaitTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "insert into pull_request_reads_owed") && len(data.Args) == 4 {
		return context.WithValue(ctx, oweStartKey{}, oweStart{reason: fmt.Sprint(data.Args[2]), at: time.Now()})
	}
	return ctx
}

func (o *oweWaitTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	started, ok := ctx.Value(oweStartKey{}).(oweStart)
	if !ok {
		return
	}
	o.mu.Lock()
	o.waits = append(o.waits, oweWait{reason: started.reason, took: time.Since(started.at)})
	due := false
	if o.lift != nil && started.reason == string(o.liftOn) {
		o.ended++
		due = o.ended == o.liftAt
	}
	o.mu.Unlock()
	if due {
		o.lift()
	}
}

// first is how long the first owe of reason waited, 0 where none was made.
func (o *oweWaitTracer) first(reason owedReason) time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, wait := range o.waits {
		if wait.reason == string(reason) {
			return wait.took
		}
	}
	return 0
}

// No failure of the owed record ends an envelope once the envelope's name is checked
// (pullRequestNamed): while Dispatch's database cannot take that record, the bus keeps the event,
// for as long as the bus keeps events. The values of the record
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
			tuningFor(t).redeliverAfter = testRedeliverAfter
			intake := newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
				client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed())
			startIntake(t, intake)
			awaitBoundDurable(t, natsClient)
			before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer
			publishPullRequestEnvelope(t, natsClient, "acme", "widgets", number,
				prEnvelope("acme/widgets", number, "opened", head, branch, body, at))

			// The second delivery is the first hand-back's; it comes the intake's redeliverAfter
			// after the first. An envelope ended at its first delivery is never delivered again.
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
			NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)
			attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head)
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
		logHandBack(slog.Default(), &natsgo.Msg{Subject: "notifications.github.acme.widgets.pr.7", Sub: &natsgo.Subscription{},
			Reply: fmt.Sprintf("$JS.ACK.ENVOY.dispatch-delivery.%d.%d.%d.1794892314678155619.0", delivered, delivered, delivered)},
			headWriteRedeliverAfter, "acme/widgets", 7, ErrHeadNotStored, cause)
	}
	logHandBack(slog.Default(), &natsgo.Msg{Subject: "notifications.github.acme.widgets.pr.8"}, headWriteRedeliverAfter, "acme/widgets", 8, ErrHeadNotStored, cause)
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
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
		client, events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
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
	if attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head); err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s, #%d's head) after the push = %v, %v; want true", issue, number, attributed, err)
	}
	if owed := owedCount(t, ctx, pool, "acme/widgets", number); owed != 0 {
		t.Errorf("#%d is owed %d read(s) after a read wrote its head, want none: the failed delivery's record was not deleted", number, owed)
	}
	readsBefore := github.reads.Load()
	NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)
	if reads := github.reads.Load() - readsBefore; reads != 0 {
		t.Errorf("the reconcile's next pass read #%d from GitHub %d time(s), want none: its head was written", number, reads)
	}
}
