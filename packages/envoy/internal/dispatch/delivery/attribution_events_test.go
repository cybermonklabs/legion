package delivery

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// attributionEvent is one stored issue.pull_request_attribution_changed event, as the API and the
// issue's timeline read it.
type attributionEvent struct {
	Seq     int
	Actor   model.Actor
	Notify  bool
	Payload struct {
		Repo   string  `json:"repo"`
		Number int     `json:"number"`
		URL    string  `json:"url"`
		From   *string `json:"from"`
		To     *string `json:"to"`
		Source string  `json:"source"`
	}
}

// attributionEvents reads one issue's stored attribution events, oldest first.
func attributionEvents(t *testing.T, ctx context.Context, pool *store.Pool, issue string) []attributionEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select seq, actor, notify, payload from events where issue_key = $1 and type = $2 order by id
	`, issue, AttributionEventType)
	if err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	defer rows.Close()
	var found []attributionEvent
	for rows.Next() {
		var event attributionEvent
		var actor, payload []byte
		if err := rows.Scan(&event.Seq, &actor, &event.Notify, &payload); err != nil {
			t.Fatalf("scan an event of %s: %v", issue, err)
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			t.Fatalf("decode an event actor: %v", err)
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			t.Fatalf("decode an event payload: %v", err)
		}
		found = append(found, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	return found
}

// attributedHeadRow is the head of an open pull request attributed to issue, complete, so the
// write may replace what the row holds.
func attributedHeadRow(number int, issue *string, source attributionSource) PullRequestHead {
	return PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: "7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a",
		HeadRef: "legion/work", BaseRef: "main", State: "open", IssueKey: issue,
		AttributionSource: string(source), AttributionComplete: true, Title: "feat: the work",
		URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
	}
}

// seedAttributionIssue is seedHeadIssue with the issue's events cleared: this database is the
// server's own and outlives the run, and nothing truncates `events` between runs, so a count of
// one issue's events would otherwise carry every earlier run's with it (seedDeployedIssues
// clears them for the same reason).
func seedAttributionIssue(t *testing.T, ctx context.Context, pool *store.Pool, number int) string {
	t.Helper()
	key := seedHeadIssue(t, ctx, pool, number)
	if _, err := pool.Exec(ctx, `delete from events where issue_key = $1`, key); err != nil {
		t.Fatalf("clear the events of %s: %v", key, err)
	}
	return key
}

// TestAnAttributionThatMovesIsRecordedOnBothIssues: a write that moves a pull request from one
// issue to another appends one event on the issue it left and one on the issue it reached, each
// naming both sides and the source that decided the new one. The issue's results are measured
// against the heads attributed to it (HeadsAttributedTo), so a move changes what both issues are
// measured against, and R8 of INFRA-1522 asks that neither change without a record of it.
func TestAnAttributionThatMovesIsRecordedOnBothIssues(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	left := seedAttributionIssue(t, ctx, pool, 70)
	reached := seedAttributionIssue(t, ctx, pool, 71)
	broker := events.NewBroker()

	if err := UpsertPullRequestHead(ctx, pool, broker, attributedHeadRow(170, &left, sourceTitleBody)); err != nil {
		t.Fatalf("store the head attributed to %s: %v", left, err)
	}
	// Reaching an issue from nobody is a move too: one event, on the issue it reached.
	first := attributionEvents(t, ctx, pool, left)
	if len(first) != 1 || first[0].Payload.From != nil || first[0].Payload.To == nil || *first[0].Payload.To != left {
		t.Fatalf("%s carries %+v, want one event reaching it from nobody", left, first)
	}
	if first[0].Actor != AttributionEventActor || first[0].Notify || first[0].Seq <= 0 {
		t.Fatalf("the event's actor %+v, notify %v, seq %d; want the delivery-attribution system actor, quiet, a positive sequence",
			first[0].Actor, first[0].Notify, first[0].Seq)
	}

	if err := UpsertPullRequestHead(ctx, pool, broker, attributedHeadRow(170, &reached, sourceBranch)); err != nil {
		t.Fatalf("move the head to %s: %v", reached, err)
	}
	moved := attributionEvents(t, ctx, pool, left)
	if len(moved) != 2 {
		t.Fatalf("%s carries %d events, want 2: the one that credited it and the one that moved the pull request away", left, len(moved))
	}
	reachedEvents := attributionEvents(t, ctx, pool, reached)
	if len(reachedEvents) != 1 {
		t.Fatalf("%s carries %d events, want 1: the move that reached it", reached, len(reachedEvents))
	}
	for _, event := range []attributionEvent{moved[1], reachedEvents[0]} {
		if event.Payload.From == nil || *event.Payload.From != left {
			t.Errorf("the move's `from` = %v, want %s", event.Payload.From, left)
		}
		if event.Payload.To == nil || *event.Payload.To != reached {
			t.Errorf("the move's `to` = %v, want %s", event.Payload.To, reached)
		}
		if event.Payload.Source != string(sourceBranch) {
			t.Errorf("the move's source = %q, want %q", event.Payload.Source, sourceBranch)
		}
		if event.Payload.Repo != "acme/widgets" || event.Payload.Number != 170 {
			t.Errorf("the move names %s#%d, want acme/widgets#170", event.Payload.Repo, event.Payload.Number)
		}
	}

	// A pull request that reaches nobody records that too, on the issue it left alone.
	if err := UpsertPullRequestHead(ctx, pool, broker, attributedHeadRow(170, nil, sourceNone)); err != nil {
		t.Fatalf("uncredit the head: %v", err)
	}
	uncredited := attributionEvents(t, ctx, pool, reached)
	if len(uncredited) != 2 || uncredited[1].Payload.To != nil {
		t.Fatalf("%s carries %+v, want a second event leaving it credited to nobody", reached, uncredited)
	}
}

// TestAnAttributionThatStaysIsRecordedNowhere: a write that stores the issue the row already
// holds appends nothing, and neither does a write that carries no issue at all. The reconcile
// re-reads every open row every five minutes, so an event per write would bury each issue's
// timeline under the pass's own heartbeat.
func TestAnAttributionThatStaysIsRecordedNowhere(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedAttributionIssue(t, ctx, pool, 72)
	broker := events.NewBroker()

	if err := UpsertPullRequestHead(ctx, pool, broker, attributedHeadRow(172, &issue, sourceBranch)); err != nil {
		t.Fatalf("store the head attributed to %s: %v", issue, err)
	}
	for _, again := range []PullRequestHead{
		attributedHeadRow(172, &issue, sourceBranch),
		// The same issue from another source is no move either: the row credits the same issue.
		attributedHeadRow(172, &issue, sourceTitleBody),
	} {
		if err := UpsertPullRequestHead(ctx, pool, broker, again); err != nil {
			t.Fatalf("write the head again: %v", err)
		}
	}
	// An incomplete write cannot replace the row's issue, so it cannot move one either.
	incomplete := attributedHeadRow(172, nil, sourceNone)
	incomplete.AttributionComplete = false
	if err := UpsertPullRequestHead(ctx, pool, broker, incomplete); err != nil {
		t.Fatalf("write the head from an incomplete read: %v", err)
	}
	if found := attributionEvents(t, ctx, pool, issue); len(found) != 1 {
		t.Fatalf("%s carries %d events, want 1: only the write that first credited it moved anything", issue, len(found))
	}
	if stored := readHead(t, ctx, pool, "acme/widgets", 172); stored.IssueKey == nil || *stored.IssueKey != issue {
		t.Fatalf("the row = %+v, want it still credited to %s", stored, issue)
	}
}

// TestTheMergedArmTakesARowAnOlderRuleCredited: the one-off re-judging R8 asks for. A merged row
// an older attribution rule credited is offered to this build's rule once — under rule 1 the arm
// took the rows credited to nobody alone, which would have left a child's pull request on its
// parent for good — and a row this rule has already judged is not taken again.
func TestTheMergedArmTakesARowAnOlderRuleCredited(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	parent := seedHeadIssue(t, ctx, pool, 73)
	eventAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	read := time.Now().Add(-time.Hour)
	merged := func(number int) PullRequestHead {
		head := attributedHeadRow(number, &parent, sourceTitleBody)
		head.State, head.EventAt, head.CheckedAt = "merged", &eventAt, &read
		return head
	}
	// #173 was judged by the rule before this one, #174 by this one.
	for _, number := range []int{173, 174} {
		if err := UpsertPullRequestHead(ctx, pool, nil, merged(number)); err != nil {
			t.Fatalf("store the merged head #%d: %v", number, err)
		}
	}
	stampLegacyAttributionRule(t, ctx, pool, "acme/widgets", 173)

	batch, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	var offered, judged bool
	for _, head := range batch {
		switch {
		case head.Repo == "acme/widgets" && head.Number == 173:
			offered = true
		case head.Repo == "acme/widgets" && head.Number == 174:
			judged = true
		}
	}
	if !offered {
		t.Errorf("the batch = %+v, want #173: a merged row an older rule credited is re-judged once", batch)
	}
	if judged {
		t.Errorf("the batch = %+v, want no #174: this rule has judged it already", batch)
	}
}

// One pass offers at most RejudgedHeadsPerPass of the merged rows a raised rule brings back, and
// the rest of the batch goes to the rows a reader meets now. A raised rule offers every merged
// row of a deployment once, and those rows sort first — a merged row was read days ago and an
// open row five minutes ago — so without the bound the backfill fills every pass until it drains
// and no open head is re-read meanwhile.
func TestOnePassOffersBoundedlyManyRowsARaisedRuleBroughtBack(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 75)
	eventAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	read := time.Now().Add(-time.Hour)
	for number := 300; number < 300+RejudgedHeadsPerPass+10; number++ {
		head := attributedHeadRow(number, &issue, sourceTitleBody)
		head.State, head.EventAt, head.CheckedAt = "merged", &eventAt, &read
		if err := UpsertPullRequestHead(ctx, pool, nil, head); err != nil {
			t.Fatalf("store the merged head #%d: %v", number, err)
		}
		stampLegacyAttributionRule(t, ctx, pool, "acme/widgets", number)
	}
	open := attributedHeadRow(400, &issue, sourceTitleBody)
	open.State, open.EventAt, open.CheckedAt = "open", &eventAt, &read
	if err := UpsertPullRequestHead(ctx, pool, nil, open); err != nil {
		t.Fatalf("store the open head: %v", err)
	}

	batch, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	rejudged, openOffered := 0, false
	for _, head := range batch {
		if head.Number == 400 {
			openOffered = true
			continue
		}
		rejudged++
	}
	if rejudged != RejudgedHeadsPerPass {
		t.Errorf("the pass offered %d rows a raised rule brought back, want %d", rejudged, RejudgedHeadsPerPass)
	}
	if !openOffered {
		t.Errorf("the batch = %+v, want the open head among them: the backfill never takes the whole pass", batch)
	}
}

// A move's two events take both issues' owner locks in one statement, sorted by lock key, before
// the first append (appendAttributionChange, events.Broker.LockOwners), so it never holds one
// issue's lock while it waits for the other's against a writer that takes them the other way
// round. The test is that other writer: it holds the issue whose lock sorts first, and once the
// move waits, takes the other. A move that took the issue it left first, as its own appends reach
// it, would hold that lock while it waited for the one the test holds, and Postgres would break
// the cycle as a deadlock.
func TestAnAttributionMoveTakesBothIssuesLocksInLockKeyOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	first, second := seedAttributionIssue(t, ctx, pool, 72), seedAttributionIssue(t, ctx, pool, 73)
	var firstSortsFirst bool
	if err := pool.QueryRow(ctx, `select hashtext($1) < hashtext($2)`,
		store.IssueOwner(first).LockKey(), store.IssueOwner(second).LockKey()).Scan(&firstSortsFirst); err != nil {
		t.Fatalf("order the two issues' lock keys: %v", err)
	}
	low, high := first, second
	if !firstSortsFirst {
		low, high = second, first
	}
	// The move leaves the issue whose lock sorts last for the one whose lock sorts first, so its
	// own appends would reach the two in the opposite order to the lock key's.
	head := PullRequestHead{Repo: "acme/widgets", Number: 7300, URL: "https://github.com/acme/widgets/pull/7300",
		IssueKey: &low, AttributionSource: "branch"}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the other writer: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.IssueOwner(low)); err != nil {
		t.Fatalf("hold %s's owner lock: %v", low, err)
	}
	moved := make(chan error, 1)
	go func() {
		moved <- func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := appendAttributionChange(ctx, tx, events.NewBroker(), head, &high); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
	}()
	if early, answered, _ := storetest.AwaitOwnerLockWaiter(t, &store.Store{Pool: pool}, "the attribution move", moved); answered {
		t.Fatalf("the move answered (%v) while this test held %s's owner lock", early, low)
	}
	if err := store.LockOwner(ctx, holder, store.IssueOwner(high)); err != nil {
		t.Fatalf("the other writer could not take %s's owner lock behind %s's: %v; the move held %s's lock while "+
			"it waited for %s's, against the lock-key order", high, low, err, high, low)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the other writer: %v", err)
	}
	select {
	case err := <-moved:
		if err != nil {
			t.Fatalf("the move once the other writer committed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the move never finished once the other writer committed")
	}
}
