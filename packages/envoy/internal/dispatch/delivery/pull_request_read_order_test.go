package delivery

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The order of two answers of one GitHub second comes from one source that only rises, and not
// from the clocks of the processes that read. A row holds the older head A of second S, written by
// a reader whose clock was an hour ahead of the others' (two replicas whose clocks differ, or a
// clock that stepped back after the write). GitHub now holds B, in the same second S. The
// reconcile's next pass reads B, and the row must hold B after that one pass: the stored head can
// be an older one until the next pass, and no longer.
func TestAStoredReadStartAheadOfTheReadersClocksKeepsNoOlderHeadPastOnePass(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const number = 7701
	second := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	older, newer := fakeSHA("a", number), fakeSHA("b", number)
	ahead := time.Now().Add(time.Hour)
	stale := time.Now().Add(-time.Hour)
	stored := PullRequestHead{Repo: "acme/widgets", Number: number,
		HeadSHA: older, HeadRef: "feature/x", BaseRef: "main", State: "open",
		EventAt: &second, CheckedAt: &stale, GitHubReadAt: &ahead}
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, readAheadOfTheOthers(t, ctx, pool, stored)); err != nil {
		t.Fatal(err)
	}
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	github.set("acme/widgets", number, "open", newer, "feature/x", "", second)
	NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed()).runOnce(ctx)
	got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", number)
	t.Logf("after one pass: head=%.12s owed=%d github_reads=%d", got.HeadSHA, owedCount(t, ctx, pool, "acme/widgets", number), github.reads.Load())
	if got.HeadSHA != newer {
		t.Errorf("after one reconcile pass the row holds %.12s, want GitHub's %.12s: a stored read start an hour ahead of the readers' clocks keeps the older head of second %s",
			got.HeadSHA, newer, second.Format(time.RFC3339))
	}
}

// A read's place comes from the sequence, so a read that begins later takes a later place,
// whatever the clock of the process that read. A row holds the older head A of second S, and its
// read had a clock an hour ahead of the others. GitHub now holds B, in the same second S. One
// reconcile pass reads B, and the row holds B after that pass. The test has no control case: its
// control is a mutation of startRead in which a later read takes a lower place, as a clock ahead
// did under the clock rule. That mutation is applied with go test -overlay, and the test then
// fails (the deep audit's case place_from_the_sequence).
func TestAPlaceFromTheSequenceLetsTheNextReadReplaceAnOlderHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const number = 7801
	second := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	older, newer := fakeSHA("a", number), fakeSHA("b", number)
	ahead := time.Now().Add(time.Hour)
	stale := time.Now().Add(-time.Hour)
	read, err := startRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	place := read.order
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
		HeadSHA: older, HeadRef: "feature/x", BaseRef: "main", State: "open",
		EventAt: &second, CheckedAt: &stale, GitHubReadAt: &ahead, GitHubReadOrder: &place}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	github.set("acme/widgets", number, "open", newer, "feature/x", "", second)
	NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed()).runOnce(ctx)
	got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", number)
	t.Logf("stored_place=%d head=%.1s github_reads=%d", place, got.HeadSHA, github.reads.Load())
	if got.HeadSHA != newer {
		t.Errorf("OLDER_HEAD_KEPT after one pass: %.1s", got.HeadSHA)
	}
}

// readAheadOfTheOthers is head as a reader of this build writes it whose clock is ahead of the
// other readers' clocks: the read takes its place in the order of reads (startRead), which is
// earlier than every later read's, and its clock reads head.GitHubReadAt.
func readAheadOfTheOthers(t *testing.T, ctx context.Context, pool *store.Pool, head PullRequestHead) PullRequestHead {
	t.Helper()
	read, err := startRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	head.GitHubReadOrder = &read.order
	return head
}

// Two replicas whose clocks differ by ten seconds order their reads as the reads took their
// places, not as their clocks read. Replica X, whose clock is ten seconds ahead, begins its read
// first and is answered A; GitHub moves the pull request to B in the same second; replica Y begins
// its read next and is answered B, and Y's answer lands first. X's answer A then lands, carrying
// the later clock. The row keeps B, and a read is owed, since the two answers differ.
func TestTwoReplicasWhoseClocksDifferOrderTheirReadsAsTheReadsBegan(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const number = 7702
	second := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	a, b := fakeSHA("a", number), fakeSHA("b", number)
	x, err := startRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	x.at = x.at.Add(10 * time.Second)
	y, err := startRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	writeAnswer(t, ctx, pool, number, b, second, y)
	writeAnswer(t, ctx, pool, number, a, second, x)
	got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", number)
	owed := owedCount(t, ctx, pool, "acme/widgets", number)
	t.Logf("head=%.12s owed=%d x_clock_ahead_by=10s", got.HeadSHA, owed)
	if got.HeadSHA != b {
		t.Errorf("the row holds %.12s, want B, %.12s: the answer of the read that began first replaced the later one because its replica's clock is ahead", got.HeadSHA, b)
	}
	if owed != 1 {
		t.Errorf("%d reads are owed after two answers of one second with two heads, want 1", owed)
	}
}

// An older answer of GitHub that lands after a newer one does not replace the newer head (an
// older event does not replace a newer head). Two reads of this build take their places; the
// later read is answered first and its answer lands first: updated an hour later, reopened, open
// at head B. The earlier read's answer lands next: updated an hour before, closed at head A. Over
// a row that a read of this build wrote, the trigger owes no read for that write, so the head
// write's own guard on the event's time is what keeps B. The row must keep B or owe a read, and
// after the reconcile's next pass, whose GitHub answers B, it must hold B, open. A head write that
// takes every answer whatever its time leaves the row closed at A, with no read owed, after the
// pass.
func TestAnOlderAnswerThatLandsAfterANewerOneDoesNotReplaceItsHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	const repo, number = "acme/widgets", 7704
	headA, headB := fakeSHA("a", number), fakeSHA("b", number)
	older := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	newer := older.Add(time.Hour)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, repo)
	github.set(repo, number, "open", headB, "feature/x", "", newer)
	early, err := startRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	late, err := startRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	write := func(head, state string, updated time.Time, read readStart) {
		t.Helper()
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: repo, Number: number,
			HeadSHA: head, HeadRef: "feature/x", BaseRef: "main", State: state, Title: "A change",
			URL: "https://github.com/acme/widgets/pull/7704", EventAt: &updated,
			GitHubReadAt: &read.at, GitHubReadOrder: &read.order}); err != nil {
			t.Fatalf("write %s at %.1s: %v", state, head, err)
		}
	}
	write(headB, "open", newer, late)
	write(headA, "closed", older, early)
	afterWrites := readHead(t, unrenamed(), ctx, pool, repo, number)
	owed := owedCount(t, ctx, pool, repo, number)
	NewReconcile(pool, fake.newTestClient(), events.NewBroker(), unrenamed()).runOnce(ctx)
	afterPass := readHead(t, unrenamed(), ctx, pool, repo, number)
	t.Logf("after_writes=%s@%.1s owed=%d after_pass=%s@%.1s", afterWrites.State, afterWrites.HeadSHA, owed, afterPass.State, afterPass.HeadSHA)
	if afterWrites.HeadSHA != headB && owed == 0 {
		t.Errorf("the older answer replaced the newer head: the row is %s at %.1s with no read owed, want open at B or a read owed", afterWrites.State, afterWrites.HeadSHA)
	}
	if afterPass.HeadSHA != headB || afterPass.State != "open" {
		t.Errorf("after one reconcile pass the row is %s at %.1s, want GitHub's head, open at B", afterPass.State, afterPass.HeadSHA)
	}
}

// Every landing order of two and three answers of one second, read by reads that took their
// places in the order of their index, whatever the order the answers land in: the row holds the
// answer of the read that took the last place, and a read is owed exactly where the answers
// differ (the deep audit of cybermonklabs/legion#4209 at ac562e9837 ran these 48 cases; its
// probe, TestProbeEveryLandingOrderOfAnswersOfOneSecond, ordered the reads by clocks).
func TestEveryLandingOrderOfAnswersOfOneSecondKeepsTheAnswerOfTheLastRead(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	second := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	heads := map[byte]string{'A': fakeSHA("a", 1), 'B': fakeSHA("b", 1)}
	number, cases := 7800, 0
	for _, pattern := range []string{"AB", "BA", "AA", "AAB", "ABA", "ABB", "BAA", "BAB", "BBA", "AAA"} {
		for _, order := range landingOrders(len(pattern)) {
			number++
			cases++
			reads := make([]readStart, len(pattern))
			for i := range reads {
				read, err := startRead(ctx, pool)
				if err != nil {
					t.Fatal(err)
				}
				reads[i] = read
			}
			for _, writer := range order {
				writeAnswer(t, ctx, pool, number, heads[pattern[writer]], second, reads[writer])
			}
			got := readHead(t, unrenamed(), ctx, pool, "acme/widgets", number)
			owed := owedCount(t, ctx, pool, "acme/widgets", number)
			differ := strings.Count(pattern, pattern[:1]) != len(pattern)
			if want := heads[pattern[len(pattern)-1]]; got.HeadSHA != want || (owed > 0) != differ {
				t.Errorf("answers %s landing in the order %v: the row holds %.1s with %d reads owed, want %.1s, owed %v",
					pattern, order, got.HeadSHA, owed, want, differ)
			}
		}
	}
	t.Logf("landing_orders=%d", cases)
	if cases != 48 {
		t.Fatalf("%d landing orders, want the audit's 48", cases)
	}
}

// writeAnswer writes GitHub's answer at head of second through the head write every reader goes
// through, as the read read made it (headRow sets the same three values).
func writeAnswer(t *testing.T, ctx context.Context, pool *store.Pool, number int, head string, second time.Time, read readStart) {
	t.Helper()
	now := time.Now()
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, PullRequestHead{Repo: "acme/widgets", Number: number,
		HeadSHA: head, HeadRef: "feature/x", BaseRef: "main", State: "open",
		EventAt: &second, CheckedAt: &now, GitHubReadAt: &read.at, GitHubReadOrder: &read.order}); err != nil {
		t.Fatalf("write #%d at %.1s: %v", number, head, err)
	}
}

// landingOrders is every order of n answers.
func landingOrders(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var orders [][]int
	for _, shorter := range landingOrders(n - 1) {
		for i := 0; i <= len(shorter); i++ {
			orders = append(orders, append(append(append([]int{}, shorter[:i]...), n-1), shorter[i:]...))
		}
	}
	return orders
}

// A read whose place in the order cannot be taken does not start: the intake reads nothing of
// GitHub, its head is not stored, and deliver takes the arm of every other failure of the
// database before a head write: it owes the pull request a read and acknowledges the envelope
// (and hands it back where the owe cannot be written either, TestAnOwedRecordRefusedForAnyCause
// LosesNoHead). The sequence is renamed away for the delivery, so the place's statement fails while
// the owe, whose default names the sequence by its object and not by its name, is written. Once
// the sequence is back, the reconcile's next pass writes the head.
func TestAReadWhosePlaceCannotBeTakenDoesNotStartAndIsOwed(t *testing.T) {
	fastPageRetries(t)
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	const number = 7703
	issue := seedHeadIssue(t, ctx, pool, number)
	head, branch, body := fakeSHA("c", number), "legion/"+issue, "Dispatch: "+issue
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	fake := newFakeGitHub(t)
	github := serveGitHubPulls(t, fake, "acme/widgets")
	github.set("acme/widgets", number, "open", head, branch, body, at)
	client := fake.newTestClient()
	rename := func(from, to string) {
		t.Helper()
		if _, err := pool.Exec(ctx, "alter sequence "+from+" rename to "+to); err != nil {
			t.Fatalf("rename the sequence %s to %s: %v", from, to, err)
		}
	}
	rename("pull_request_reads_owed_revision_seq", "pull_request_reads_owed_revision_away")
	restored := false
	restore := func() {
		if !restored {
			restored = true
			rename("pull_request_reads_owed_revision_away", "pull_request_reads_owed_revision_seq")
		}
	}
	t.Cleanup(restore)
	payload, err := json.Marshal(prEnvelope("acme/widgets", number, "opened", head, branch, body, at))
	if err != nil {
		t.Fatal(err)
	}
	newIntake(tuningFor(t), nil, pool, nil, client, nil, newSettingsCache(pool, nil), unrenamed()).deliver(deliveryWithNoNATS(ctx), intakeMessage{
		msg:      &natsgo.Msg{Subject: "notifications.github.acme.widgets.pr." + strconv.Itoa(number)},
		envelope: &contracts.Envelope{Payload: string(payload)},
	})
	readsAtDelivery, owedAtDelivery := github.reads.Load(), owedCount(t, ctx, pool, "acme/widgets", number)
	var arm string
	for _, record := range logs() {
		if message, _ := record["msg"].(string); strings.Contains(message, "the read it is owed is recorded; acknowledging") {
			arm = message
		}
	}
	restore()
	NewReconcile(pool, client, events.NewBroker(), unrenamed()).runOnce(ctx)
	attributed, err := unrenamed().HeadAttributedTo(ctx, pool, issue, head)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("github_reads_at_delivery=%d owed_at_delivery=%d arm=%q head_is_basis_after_pass=%v", readsAtDelivery, owedAtDelivery, arm, attributed)
	if readsAtDelivery != 0 {
		t.Errorf("the delivery read GitHub %d times while the read's place could not be taken, want none: the read does not start", readsAtDelivery)
	}
	if owedAtDelivery != 1 || arm == "" {
		t.Errorf("the delivery left %d reads owed and took the arm %q, want 1 owed and the arm that owes the read and acknowledges", owedAtDelivery, arm)
	}
	if !attributed {
		t.Errorf("#%d's head is no basis after the sequence is back and one reconcile pass, want it written", number)
	}
}
