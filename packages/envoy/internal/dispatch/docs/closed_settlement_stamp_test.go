package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A closed issue's settlement whose first pass commits a block-id stamp alone (commitStamp), and
// whose last pass then writes no version, stores nothing more and credits no one, has not saved
// nothing: its stamp is in the document's stored history, which the rule for a settlement that
// saved nothing counts (settlementPass.savedNothing, settlement.stored). So its commit forgets the
// editor the settlement before it kept (clearSettlementPending with no kept editor), as a
// settlement that stores its stamp in the transaction of its version does, and does not keep it as
// one that saved nothing does (clearSettlementPendingRow). The last pass reads the head after the
// stamp, so its own reads cannot tell. alice's edit inside the close margin is settled on the
// closed issue, which keeps alice (settleEditWhileClosed). A browser's edit, made while no peer is
// connected (editAsPeer), then takes the block id off the document's one paragraph, its text
// unchanged, and the document is owed naming no editor. The settlement after it stamps the
// paragraph and commits that stamp in its first pass; its next pass finds the text the latest
// version holds, credits SettlementActor and lists no one.
func TestAClosedIssuesSettlementWhoseFirstPassStampedForgetsTheKeptEditor(t *testing.T) {
	t.Parallel()
	service, artifactID, alice := settleEditWhileClosed(t)
	ctx := context.Background()
	version, _, _ := latestDocumentVersion(t, service.store, artifactID)
	editAsPeer(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		removeBlockIDs(tree)
		return tree
	})
	waitForLandedAppends(t, service, artifactID)
	// The row the append inserted names alice, the editor kept for the closed issue, which would
	// credit her and keep her whatever the settlement saved. The row is made to name no editor, as
	// TestAClosedIssuesSettlementThatSavesNothingKeepsTheKeptEditor owes one, so the settlement
	// credits SettlementActor and the rule for a settlement that saved nothing decides.
	if _, err := service.store.Pool.Exec(ctx, `update doc_settlements_pending set last_actor = null where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("owe the document's settlement naming no editor: %v", err)
	}
	if owed, named, err := readOwedSettlement(ctx, service.store.Pool, artifactID); err != nil || !owed || named != nil {
		t.Fatalf("after the edit that took the paragraph's block id off the document owes %t naming %+v (%v), want it owed naming nobody", owed, named, err)
	}
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, service.store, artifactID)); repairs == 0 {
		t.Fatal("the stored document holds every block id after the edit that took one off: this test is for a settlement that stamps one")
	}
	if kept := keptClosedEditSource(t, service, artifactID); kept == nil || !kept.SameAs(alice) {
		t.Fatalf("before the settlement the document keeps %+v as its closed edit source, want alice", kept)
	}
	unstamped := storedHead(t, service.store, artifactID)

	settleNow(service, artifactID)
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, service.store, artifactID)); repairs != 0 {
		t.Fatalf("the settlement left %d blocks without an id stored: this test is for a settlement whose pass commits a stamp", repairs)
	}
	if head := storedHead(t, service.store, artifactID); head <= unstamped {
		t.Fatalf("the settlement left the stored head at %d, want past %d: the stamp is stored", head, unstamped)
	}
	if now, _, _ := latestDocumentVersion(t, service.store, artifactID); now != version {
		t.Fatalf("the settlement left the latest version %d, want %d: this test is for a settlement that writes no version", now, version)
	}
	if owed, err := settlementPending(ctx, service.store.Pool, artifactID); err != nil || owed {
		t.Fatalf("after the settlement the document owes a settlement = %t (%v), want none: it committed", owed, err)
	}
	if kept := keptClosedEditSource(t, service, artifactID); kept != nil {
		t.Fatalf("after a closed issue's settlement whose first pass committed a stamp the document keeps %+v as its closed edit source, want none: the stamp is saved, so the settlement saved something and forgets the editor", *kept)
	}
}
