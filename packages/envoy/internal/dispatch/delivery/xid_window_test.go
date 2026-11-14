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
// (recordDeployedEvent). That transaction writes its row inline, so its window holds three
// statements: the row's marking update, the batch appending issue.deployed (the issue owner's
// lock, which this transaction already holds, the issue's row and the insert), and the COMMIT
// ending them. Everything that comes before the first of them holds no transaction id - the
// shipped rows with the run that shipped each of them, read by the pass itself outside any
// transaction (listUnrecordedShipped), and the issue's owner lock, which the append would
// otherwise be the first to take and which is advisory (store.LockOwner).
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
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(3,
		"the pull request's marking update, the batch appending issue.deployed and the COMMIT ending them"))
}

// A head write moves a pull request from one issue to another, so it records the move on both
// issues (UpsertPullRequestHead, appendAttributionChange): the webhook intake's write, the
// reconcile's refresh and the results read's read-through all make it. Its window opens at the
// row's own lock, which headIssueBefore takes to read the issue the row held, and holds database
// statements only: that locked read, the row's upsert, the lock of both issues' owners in lock-key
// order (events.Broker.LockOwners), one batch per issue appending its
// issue.pull_request_attribution_changed, and the COMMIT ending them.
func TestXidWindowAttributionMoveRecorded(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	tracer := storetest.NewXidTracer()
	traced := storetest.Traced(t, pool, tracer, nil)
	left := seedAttributionIssue(t, ctx, traced, 76)
	reached := seedAttributionIssue(t, ctx, traced, 77)
	broker := events.NewBroker()
	// The row is credited to the issue it leaves first, whatever an earlier run of this database
	// left it holding, so the measured write is a move; that first write may record a move off
	// the issue the measured one reaches, whose events are cleared again behind it.
	if err := UpsertPullRequestHead(ctx, traced, broker, attributedHeadRow(176, &left, sourceTitleBody)); err != nil {
		t.Fatalf("store the head attributed to %s: %v", left, err)
	}
	seedAttributionIssue(t, ctx, traced, 77)

	report := tracer.Measure(t, traced, func() {
		if err := UpsertPullRequestHead(storetest.MarkXid(ctx), traced, broker, attributedHeadRow(176, &reached, sourceBranch)); err != nil {
			t.Errorf("move the head to %s: %v", reached, err)
		}
	})
	if moved := attributionEvents(t, ctx, traced, reached); len(moved) != 1 || moved[0].Payload.From == nil || *moved[0].Payload.From != left {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): %s carries %+v, want the one move the measured write recorded", reached, moved)
	}
	storetest.AssertXidWindows(t, report, storetest.XidWindowOf(6,
		"the row's locked read of the issue it held, the row's upsert, the lock of both issues' owners, "+
			"the batch appending issue.pull_request_attribution_changed on each of the two issues and the COMMIT ending them"))
}
