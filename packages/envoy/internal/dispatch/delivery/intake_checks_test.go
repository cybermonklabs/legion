package delivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settlementHandedBack is the line deliver logs as it hands back a settlement whose issue it
// could not mark owed.
const settlementHandedBack = "dispatch delivery: a settled CI run's issue was not marked owed; handing the envelope back"

// takeOverMarkFailed is the line a take-over logs when its mark of every issue with an open head
// failed, and it took the durable over no further.
const takeOverMarkFailed = "dispatch delivery: the step before the take-over failed; the next attempt takes the durable over"

// checksSettlementPayload is the settlement the listener's CI summary loop publishes for #number
// of repo at sha, encoded from its own type (cistore.Summary): one check run, finished green,
// with the groups and runs a handler here reads none of.
func checksSettlementPayload(t *testing.T, repo string, number int, sha string) string {
	t.Helper()
	payload, err := json.Marshal(cistore.Summary{
		Kind: "checks", Repo: repo, Number: fmt.Sprint(number), SHA: sha,
		CheckRuns:  []cistore.CheckRunRef{{Name: "go-checks", ID: 901, Conclusion: "success"}},
		Generation: 1, Snapshot: "snapshot",
		Passed: cistore.StatusGroup{Count: 1, Checks: []string{"go-checks"}},
		Failed: cistore.StatusGroup{Checks: []string{}}, Running: cistore.StatusGroup{Checks: []string{}},
		Queued: cistore.StatusGroup{Checks: []string{}}, Cancelled: cistore.StatusGroup{Checks: []string{}},
		Skipped: cistore.StatusGroup{Checks: []string{}}, FailingChecks: []cistore.FailingCheck{},
	})
	if err != nil {
		t.Fatalf("encode the settlement of %s#%d: %v", repo, number, err)
	}
	return string(payload)
}

// storeHead stores #number of the delivery repository at sha in state, attributed to issue (""
// for none), as a head write leaves it.
func storeHead(t *testing.T, pool *store.Pool, number int, sha, state, issue string) {
	t.Helper()
	var attributed *string
	if issue != "" {
		attributed = &issue
	}
	if _, err := pool.Exec(t.Context(), `
		insert into pull_request_heads (repo, number, head_sha, state, issue_key) values ($1, $2, $3, $4, $5)
	`, deliveryRepo, number, sha, state, attributed); err != nil {
		t.Fatalf("store the head of %s#%d: %v", deliveryRepo, number, err)
	}
}

// clearCheckStateMarks deletes every mark, so a test counts only the marks of what it does next.
func clearCheckStateMarks(t *testing.T, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from check_states_owed`); err != nil {
		t.Fatalf("clear the marks: %v", err)
	}
}

// checkStateMarksOf is how many marks of issue's check states the database holds.
func checkStateMarksOf(t *testing.T, pool *store.Pool, issue string) int {
	t.Helper()
	var marks int
	if err := pool.QueryRow(t.Context(), `select count(*) from check_states_owed where issue_key = $1`, issue).Scan(&marks); err != nil {
		t.Fatalf("count the marks of %s: %v", issue, err)
	}
	return marks
}

// allCheckStateMarks is how many marks of any issue the database holds.
func allCheckStateMarks(t *testing.T, pool *store.Pool) int {
	t.Helper()
	var marks int
	if err := pool.QueryRow(t.Context(), `select count(*) from check_states_owed`).Scan(&marks); err != nil {
		t.Fatalf("count the marks: %v", err)
	}
	return marks
}

// renameCheckStatesOwe renames the function check_states_owe from to to, so a statement that
// calls it fails as a statement the database refuses does, and back.
func renameCheckStatesOwe(t *testing.T, pool *store.Pool, from, to string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), fmt.Sprintf(`alter function %s(text) rename to %s`, from, to)); err != nil {
		t.Fatalf("rename %s to %s: %v", from, to, err)
	}
}

// logged is how many records of logs have msg as their message.
func logged(logs func() []map[string]any, msg string) int {
	count := 0
	for _, record := range logs() {
		if record["msg"] == msg {
			count++
		}
	}
	return count
}

// A settlement of a pull request's checks whose head is open marks the issue that holds it once,
// reading the settlement's string fields and none of its groups or runs: no ERROR line, no GitHub
// call and no delivery row, and the event's time recorded, as for every kind the intake handles.
// A settlement of a pull request no issue holds marks nothing.
func TestASettlementMarksTheIssueOfItsOpenHeadAndNothingElse(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	issue := seedHeadIssue(t, ctx, pool, 9202)
	storeHead(t, pool, 9202, "6666666666666666666666666666666666666666", "open", issue)
	storeHead(t, pool, 9203, "7777777777777777777777777777777777777777", "open", "")
	logs := captureIntakeLogs(t)
	fake := newFakeGitHub(t)
	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)
	clearCheckStateMarks(t, pool)

	publishGitHubEnvelope(t, natsClient, "notifications.github.acme.widgets.pr.9202.checks",
		checksSettlementPayload(t, deliveryRepo, 9202, "6666666666666666666666666666666666666666"))
	publishGitHubEnvelope(t, natsClient, "notifications.github.acme.widgets.pr.9203.checks",
		checksSettlementPayload(t, deliveryRepo, 9203, "7777777777777777777777777777777777777777"))
	awaitAcknowledged(t, natsClient)

	if marks := checkStateMarksOf(t, pool, issue); marks != 1 {
		t.Fatalf("the settlement of #9202 left %d marks of %s, want 1", marks, issue)
	}
	if marks := allCheckStateMarks(t, pool); marks != 1 {
		t.Fatalf("the two settlements left %d marks, want the one of %s: #9203 is no issue's", marks, issue)
	}
	for _, record := range logs() {
		if record["level"] == "ERROR" && strings.HasPrefix(fmt.Sprint(record["msg"]), "dispatch delivery:") {
			t.Errorf("the intake logged an ERROR for a settlement: %v", record)
		}
	}
	if mints := fake.tokenMints.Load(); mints != 0 {
		t.Errorf("minted %d installation tokens, want none: a settlement calls no GitHub", mints)
	}
	var prs int
	if err := pool.QueryRow(ctx, "select count(*) from delivery_pull_requests").Scan(&prs); err != nil {
		t.Fatalf("count pull requests: %v", err)
	}
	if runs := countRuns(t, ctx, pool); prs != 0 || runs != 0 {
		t.Errorf("stored %d pull requests and %d runs, want none", prs, runs)
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastEventAt == nil {
		t.Errorf("last_event_at after a settlement the intake handled = %+v, %v; want it recorded", settings.LastEventAt, err)
	}
}

// A settlement whose issue could not be marked owed is handed back, and its redelivery marks the
// issue. No table stores a CI run, so the mark is the one write that has the check states
// settler read a run that concluded, and an acknowledged settlement with no mark would leave the
// state of a check CI proves as it was while the run went on.
//
// The mark cannot be written while check_states_owe is renamed away, as a database that refuses
// the statement refuses it; it is renamed once the intake has taken the durable over, since the
// take-over marks too. The test asks NATS, not the handler: once the hand-back is logged, the
// function is put back, and the envelope's second delivery, ownHandBackAfter later, writes the one
// mark and is acknowledged.
func TestASettlementWhoseMarkCouldNotBeWrittenIsHandedBack(t *testing.T) {
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	issue := seedHeadIssue(t, ctx, pool, 9201)
	const sha = "5555555555555555555555555555555555555555"
	storeHead(t, pool, 9201, sha, "open", issue)

	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
		newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	awaitBoundDurable(t, natsClient)
	// The head's insert and the take-over marked the issue owed; this test counts the
	// settlement's mark alone.
	clearCheckStateMarks(t, pool)
	renameCheckStatesOwe(t, pool, "check_states_owe", "check_states_owe_away")
	before := deliveryConsumerInfo(t, natsClient).Delivered.Consumer

	publishGitHubEnvelope(t, natsClient, "notifications.github.acme.widgets.pr.9201.checks", checksSettlementPayload(t, deliveryRepo, 9201, sha))
	waitFor(t, 20*time.Second, func() bool { return logged(logs, settlementHandedBack) > 0 }, func() string {
		return fmt.Sprintf("the settlement whose mark failed was not handed back: %+v", deliveryConsumerInfo(t, natsClient))
	})
	if marks := checkStateMarksOf(t, pool, issue); marks != 0 {
		t.Fatalf("%s holds %d marks while check_states_owe is away, want none", issue, marks)
	}

	renameCheckStatesOwe(t, pool, "check_states_owe_away", "check_states_owe")
	waitFor(t, 3*ownHandBackAfter, func() bool { return checkStateMarksOf(t, pool, issue) == 1 }, func() string {
		return fmt.Sprintf("the redelivered settlement left %d marks of %s, want 1: %+v",
			checkStateMarksOf(t, pool, issue), issue, deliveryConsumerInfo(t, natsClient))
	})
	awaitAcknowledged(t, natsClient)
	if delivered := deliveryConsumerInfo(t, natsClient).Delivered.Consumer - before; delivered != 2 {
		t.Fatalf("the settlement was delivered %d times, want 2: the one whose mark failed and the one that marked", delivered)
	}
}

// A take-over marks the check states of every issue with an open attributed head owed, once
// each, before it changes the durable: the holder before may have acknowledged a settlement with
// no mark, as a build before the settlements' marks does. An issue whose head is closed, and an
// open head no issue holds, get no mark. A take-over whose mark fails takes nothing over: the
// durable is not created, the failure is logged at each attempt, and once the mark can be written
// the next attempt marks and binds.
func TestATakeOverMarksEveryIssueWithAnOpenHeadBeforeItBinds(t *testing.T) {
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	open, closed := seedHeadIssue(t, ctx, pool, 9204), seedHeadIssue(t, ctx, pool, 9205)
	storeHead(t, pool, 9204, "8888888888888888888888888888888888888888", "open", open)
	storeHead(t, pool, 9206, "9999999999999999999999999999999999999999", "open", open)
	storeHead(t, pool, 9205, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "closed", closed)
	storeHead(t, pool, 9207, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "open", "")
	clearCheckStateMarks(t, pool)
	renameCheckStatesOwe(t, pool, "check_states_owe", "check_states_owe_away")

	natsClient := intakeTestClient(t)
	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool),
		newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	waitFor(t, 20*time.Second, func() bool { return logged(logs, takeOverMarkFailed) >= 2 }, func() string {
		return fmt.Sprintf("the take-over logged its failed mark %d times, want two attempts", logged(logs, takeOverMarkFailed))
	})
	if _, err := natsClient.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable); !errors.Is(err, natsgo.ErrConsumerNotFound) {
		t.Fatalf("the delivery durable after take-overs whose mark failed: %v, want it never created", err)
	}

	renameCheckStatesOwe(t, pool, "check_states_owe_away", "check_states_owe")
	awaitBoundDurable(t, natsClient)
	if marks := checkStateMarksOf(t, pool, open); marks != 1 {
		t.Fatalf("the take-over left %d marks of %s, whose two heads are open, want 1", marks, open)
	}
	if marks := allCheckStateMarks(t, pool); marks != 1 {
		t.Fatalf("the take-over left %d marks, want the one of %s: %s's head is closed and #9207 is no issue's", marks, open, closed)
	}
}
