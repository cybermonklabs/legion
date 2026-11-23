//go:build !race

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The ask repair (RepairIssueAsks, repairIssueAsksOf) is a background writer rather than a
// route, so it is measured beside the route window table (route_xid_window_test.go) rather than
// as a row of it: on a repair fixture whose pool carries the xid tracer, with the harness's large
// spec, one pass is run under a marked context. Each issue's repair takes the issue's owner lock
// and reads under it (store.LockOwnerAndRead), reads its asks, its closer or reopener and does the
// document work, all holding no transaction id, and queues the asks' rows, their events and the
// document writes on the ledger, which sends them as one batch at its commit.

// TestXidWindowIssueAskRepairWithdrawal measures the repair of an issue a release older than the
// withdrawal closed while it held a decision block's ask and a plain ask: the repair withdraws
// both in its latest closer's name and gives the spec the close's final sweep.
func TestXidWindowIssueAskRepairWithdrawal(t *testing.T) {
	t.Parallel()
	tracer := storetest.NewXidTracer()
	f := newRepairFixtureOn(t, 20*time.Millisecond, tracer)
	spec, _ := xidSpec(1)
	issue := createInteractionIssue(t, f.handler, xidProject, "Closed by an older release", spec+"\n"+closeDecision)
	awaitIndexedAskBlock(t, f.handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	asks := []string{
		requireBlockAsk(t, f.handler, issue.Key, issue.PrimaryArtifactID, "close-decision"),
		createSessionAsk(t, f.handler, issue.Key, "Which region?"),
	}
	awaitSettled(t, f.database, issue.PrimaryArtifactID)
	closer := repairActor("session-closer")
	closeWithoutWithdrawing(t, f, issue.Key, &closer, time.Now().Add(-time.Hour))
	awaitQuietDatabase(t, f.database, false)

	report := measureIssueAskRepair(t, f, tracer, func(repair IssueAskRepair) bool { return repair.Withdrawn == len(asks) })
	storetest.AssertXidWindows(t, report, storetest.XidBatchedWindow)
}

// TestXidWindowIssueAskRepairRestoration measures the repair of an issue whose close withdrew a
// decision block's ask and a plain ask and which a release older than the restoration reopened:
// the repair restores both in its reopener's name and writes the block open into the spec.
func TestXidWindowIssueAskRepairRestoration(t *testing.T) {
	t.Parallel()
	tracer := storetest.NewXidTracer()
	f := newRepairFixtureOn(t, 20*time.Millisecond, tracer)
	spec, _ := xidSpec(1)
	issue := createInteractionIssue(t, f.handler, xidProject, "Reopened by an older release", spec+"\n"+closeDecision)
	awaitIndexedAskBlock(t, f.handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	asks := []string{
		requireBlockAsk(t, f.handler, issue.Key, issue.PrimaryArtifactID, "close-decision"),
		createSessionAsk(t, f.handler, issue.Key, "Which region?"),
	}
	awaitSettled(t, f.database, issue.PrimaryArtifactID)
	if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	// The close's writes leave the spec's settlement armed, and a settlement armed before a reopen
	// outlives the reopen's close of the room and loads the room again (docs' settlement.take), so
	// it runs here, before the reopen whose close awaitIssueRoomsClosed waits for.
	// That re-warm is the product's design (settlement.take's own comment), not a defect.
	if err := f.documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the closed issue's documents: %v", err)
	}
	reopenWithoutRestoring(t, f, issue.Key, "in_progress", repairActor("session-reopener"))
	// The close's settlement has run, and the older release's reopen is SQL alone, which arms no
	// settlement (awaitQuietDatabase), so the measurement waits for the quiet alone.
	awaitQuietDatabase(t, f.database, false)

	report := measureIssueAskRepair(t, f, tracer, func(repair IssueAskRepair) bool { return repair.Restored == len(asks) })
	storetest.AssertXidWindows(t, report, storetest.XidBatchedWindow)
}

// measureIssueAskRepair runs one repair pass under a marked context and measures it, failing as a
// rig problem unless the pass made the repair repaired says it means.
func measureIssueAskRepair(t *testing.T, f repairFixture, tracer *storetest.XidTracer, repaired func(IssueAskRepair) bool) *storetest.XidReport {
	t.Helper()
	var repair IssueAskRepair
	var err error
	report := tracer.Measure(t, f.database.Pool, func() {
		repair, err = RepairIssueAsks(storetest.MarkXid(context.Background()), f.deps, nil)
	})
	if err != nil || !repaired(repair) {
		t.Fatalf("XID RIG PROBLEM (not a finding about the code): the measured repair = %+v, %v", repair, err)
	}
	return report
}
