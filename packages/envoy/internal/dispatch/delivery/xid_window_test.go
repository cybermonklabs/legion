//go:build !race

package delivery

// The delivery timeline's own write transactions under storetest's xid rig (INFRA-636, plan test
// T1). A timeline pass runs no HTTP route, so the test below runs the pass itself under a marked
// context (storetest.MarkXid) on a traced pool and judges the transaction it ran exactly as the
// API's xid window tests judge a route's: from the statement that gave the transaction its xid to
// its COMMIT, holding no more statements than the measurement declares (storetest.XidShape), and
// in a build that judges a window's time, keeping D6's bound on its gaps.

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A production run has shipped a merged pull request, so the pass records its issue's
// issue.deployed and marks the row, each pull request in a transaction of its own
// (recordDeployedEvent). That transaction writes its row inline, so its window holds four
// statements: the row's marking update, the statement's read of the pull request's issue under
// the row's lock (PullRequestIssues), the batch appending issue.deployed (the issue owner's lock,
// which this transaction already holds, the issue's row and the insert), and the COMMIT ending
// them. Everything that comes before the first of them holds no transaction id - the shipped rows
// with the run that shipped each of them and the statement's issue of each, read by the pass
// itself outside any transaction (listUnrecordedShipped), and the issue's owner lock, which the
// append would otherwise be the first to take and which is advisory (store.LockOwner).
func TestXidWindowDeployedEventRecorded(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	tracer := storetest.NewXidTracer()
	traced := storetest.Traced(t, pool, tracer, nil)
	settings := seedDeliverySettings(t, ctx, traced)
	seedDeployedIssues(t, ctx, traced)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	seedMergedPullRequest(t, ctx, traced, "acme/widgets", 601, merged, deployedIssue)
	seedDeployRun(t, ctx, traced, 910, merged.Add(time.Hour), merged.Add(70*time.Minute), DeliveryJobConclusionSuccess)
	broker := events.NewBroker()

	var written int
	report := tracer.Measure(t, traced, func() {
		var err error
		written, err = RecordDeployedEvents(storetest.MarkXid(ctx), traced, settings, broker)
		if err != nil {
			t.Errorf("RecordDeployedEvents: %v", err)
		}
	})
	if written != 1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the pass wrote %d issue.deployed events, want 1", written)
	}
	requireDeployedEvent(t, ctx, traced, deployedIssue, "acme/widgets", 601, 910, "2024-01-01T02:10:00Z")
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(4,
		"the pull request's marking update, the statement's read of its issue, the batch appending issue.deployed and the COMMIT ending them"))
}

// A pass appends the events of a queued move of a pull request from one issue to another
// (AppendAttributionEvents): the head writes and the sync of the statement's answers queue it
// (SyncAttributions). Its window opens at the take, which locks and deletes the queued move, and
// holds database statements only: that take, the read of the pull request's URL and of whether
// each issue still exists, the lock of both issues' owners in lock-key order
// (events.Broker.LockOwners), one batch per issue appending its
// issue.pull_request_attribution_changed, and the COMMIT ending them.
func TestXidWindowAttributionMoveRecorded(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	tracer := storetest.NewXidTracer()
	traced := storetest.Traced(t, pool, tracer, nil)
	left := seedHeadIssue(t, ctx, traced, 76)
	reached := seedHeadIssue(t, ctx, traced, 77)
	broker := events.NewBroker()
	// The row is credited to the issue it leaves first, and a pass records that, so the measured
	// append is a move. The database is this test's own clone (deliveryTestPool), so no other
	// move waits beside it.
	if err := UpsertPullRequestHead(ctx, traced, attributedHeadRow(176, &left, sourceTitleBody)); err != nil {
		t.Fatalf("store the head attributed to %s: %v", left, err)
	}
	if _, appended := attributionPass(t, ctx, traced, broker); appended != 1 {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the first pass appended %d events, want 1", appended)
	}
	if err := UpsertPullRequestHead(ctx, traced, attributedHeadRow(176, &reached, sourceBranch)); err != nil {
		t.Fatalf("move the head to %s: %v", reached, err)
	}
	syncAttributions(t, ctx, traced)

	var appended int
	report := tracer.Measure(t, traced, func() {
		var err error
		if appended, err = AppendAttributionEvents(storetest.MarkXid(ctx), traced, broker); err != nil {
			t.Errorf("append the events of the move to %s: %v", reached, err)
		}
	})
	if moved := attributionEvents(t, ctx, traced, reached); appended != 2 || len(moved) != 1 || moved[0].Payload.From == nil || *moved[0].Payload.From != left {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): %d events appended, %s carries %+v, want the one move the measured append recorded", appended, reached, moved)
	}
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(6,
		"the take of the queued move, the read of its pull request and issues, the lock of both issues' owners, "+
			"the batch appending issue.pull_request_attribution_changed on each of the two issues and the COMMIT ending them"))
}
