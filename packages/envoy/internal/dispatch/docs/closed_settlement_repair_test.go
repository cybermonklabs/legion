package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A closed issue's settlement whose first pass commits a repair of the document's typed blocks
// alone (the pass that writes reconciled blocks into the room and stores them), and whose last
// pass then writes no version, stores nothing more and credits no one, has not saved nothing: the
// repair is in the document's stored history, which the rule for a settlement that saved nothing
// counts (settlementPass.savedNothing, settlement.stored). So its commit forgets the editor the
// settlement before it kept, as TestAClosedIssuesSettlementWhoseFirstPassStampedForgetsTheKeptEditor
// has a stamp do, and does not keep it as a settlement that saved nothing does. The last pass reads
// the head after the repair, so its own reads cannot tell. The document holds an answered ask,
// saved in a version; alice's edit inside the close margin is settled on the closed issue, which
// keeps alice (as settleEditWhileClosed does). A browser's edit, made while no peer is connected,
// then rewords the decided ask's question, and the document is owed naming no editor. The
// settlement after it writes the decided wording back into the block and commits that repair in
// its first pass; its next pass finds the text the latest version holds, credits SettlementActor
// and lists no one.
func TestAClosedIssuesSettlementWhoseFirstPassCommittedARepairForgetsTheKeptEditor(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Context.\n\n"+shipBlock)
	settleCurrentGeneration(t, service, artifactID)
	source := blockAskOn(t, service, artifactID, "ship")
	answer, _ := decidedShip("answered")
	if err := closeSource(ctx, service, artifactID, "ship", source, answer, nil); err != nil {
		t.Fatalf("answer the ship ask: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	if decided := blockAskOn(t, service, artifactID, "ship"); decided.State != "answered" {
		t.Fatalf("the ship ask reads %s after its answer settled, want answered", decided.State)
	}

	// The closed period of settleEditWhileClosed, on this document.
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the document's room while DOC-1 is open: %v", err)
	}
	if _, err := service.store.Pool.Exec(ctx, `update issues set closed_at = now() + interval '1 minute' where key = 'DOC-1'`); err != nil {
		t.Fatalf("close DOC-1 in the database alone: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	service.addConnection(artifactID, 1, alice)
	editAsPeer(t, service, artifactID, replaceRun("Context.", "Context, typed by alice inside the close margin."))
	service.removeConnection(artifactID, 1)
	waitForLandedAppends(t, service, artifactID)
	service.settle = 20 * time.Millisecond
	if err := service.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the closed issue's owed settlement: %v", err)
	}
	waitForNoPendingSettlement(t, service, artifactID, "the closed issue's document to settle alice's edit")
	service.settle = time.Hour
	if kept := keptClosedEditSource(t, service, artifactID); kept == nil || !kept.SameAs(alice) {
		t.Fatalf("the closed issue's settlement kept %+v as the document's closed edit source, want alice", kept)
	}
	version, saved, _ := latestDocumentVersion(t, service.store, artifactID)
	if !strings.Contains(saved, "Ship on Monday?") {
		t.Fatalf("the latest version does not hold the decided wording:\n%s", saved)
	}

	editAsPeer(t, service, artifactID, replaceRun("Ship on Monday?", "Ship on Tuesday?"))
	waitForLandedAppends(t, service, artifactID)
	// As in the stamp's row: the row names no editor, so the settlement credits SettlementActor and
	// the rule for a settlement that saved nothing decides.
	if _, err := service.store.Pool.Exec(ctx, `update doc_settlements_pending set last_actor = null where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("owe the document's settlement naming no editor: %v", err)
	}
	if owed, named, err := readOwedSettlement(ctx, service.store.Pool, artifactID); err != nil || !owed || named != nil {
		t.Fatalf("after the edit that reworded the decided ask the document owes %t naming %+v (%v), want it owed naming nobody", owed, named, err)
	}
	if kept := keptClosedEditSource(t, service, artifactID); kept == nil || !kept.SameAs(alice) {
		t.Fatalf("before the settlement the document keeps %+v as its closed edit source, want alice", kept)
	}
	reworded := storedHead(t, service.store, artifactID)

	settleNow(service, artifactID)
	stored, err := pmdoc.Render(persistedProofTree(t, service.store, artifactID))
	if err != nil {
		t.Fatalf("render the stored document: %v", err)
	}
	if !strings.Contains(stored, "Ship on Monday?") || strings.Contains(stored, "Ship on Tuesday?") {
		t.Fatalf("the stored document after the settlement reads:\n%s\nwant the decided wording written back: this test is for a settlement whose pass commits a repair", stored)
	}
	if head := storedHead(t, service.store, artifactID); head <= reworded {
		t.Fatalf("the settlement left the stored head at %d, want past %d: the repair is stored", head, reworded)
	}
	if now, _, _ := latestDocumentVersion(t, service.store, artifactID); now != version {
		t.Fatalf("the settlement left the latest version %d, want %d: this test is for a settlement that writes no version", now, version)
	}
	if owed, err := settlementPending(ctx, service.store.Pool, artifactID); err != nil || owed {
		t.Fatalf("after the settlement the document owes a settlement = %t (%v), want none: it committed", owed, err)
	}
	if kept := keptClosedEditSource(t, service, artifactID); kept != nil {
		t.Fatalf("after a closed issue's settlement whose first pass committed a repair the document keeps %+v as its closed edit source, want none: the repair is saved, so the settlement saved something and forgets the editor", *kept)
	}
}
