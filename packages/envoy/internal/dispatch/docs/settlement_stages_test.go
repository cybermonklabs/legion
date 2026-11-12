package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A settlement's stages are tested one at a time, each over a live service and its PostgreSQL:
// a stage reads the room's live instance and writes through a transaction holding the document's
// owner row, so none of them can be called without the stages before it having run (settlePass).
// Each test here runs the stages before the one it tests exactly as settlePass runs them, so the
// sequence itself is under test too: TestSettlementStagesSettleTheDocumentInOrder fails when two
// of them are swapped.
//
// A settlement runs in passes, and a document whose ask blocks need repairs takes three: one
// plans the repairs outside the document's locks (planRepairs), the next writes them into the
// room and commits them (commitRepairs), and the last versions what they left (commitVersion).
// The tests below run those passes as settleRoomWithin runs them, one after another over the one
// settlement.

// stageSettlement is the settlement settleRoomWithin builds before any pass has run: the room's
// state, the live instance it holds for every pass, the generation the room stands at, the pool
// every read and write of the settlement goes through, and how the settlement reports a stop
// short.
func stageSettlement(t *testing.T, service *Service, artifactID string) *settlement {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	settlements, err := service.store.Pool.Settlements()
	if err != nil {
		t.Fatalf("open the settlements pool: %v", err)
	}
	doc := service.srv.GetDoc(artifactID)
	if doc == nil {
		t.Fatal("the room holds no live instance for the settlement's stages to run over")
	}
	settle := &settlement{
		service: service, room: artifactID, state: state, doc: doc,
		settlements: settlements, generation: generation,
	}
	settle.stoppedShort = func(err error) settleOutcome {
		return settleOutcome{next: settleRetry, err: err, generation: settle.generation}
	}
	return settle
}

// openStageTransaction opens what settlePass opens between take and readLocked: the pass's ledger
// and a transaction of the settlements pool holding the document's owner row, and the context the
// stages after take read and write through. The transaction is rolled back when the test ends, and
// what the pass wrote into the room is discarded with it, as abandon discards it, so a pass left
// half-run holds neither the document's advisory lock nor the room's persistence worker. A pass
// whose own stage committed or rolled back the transaction finished its writes itself.
func openStageTransaction(t *testing.T, pass *settlementPass, parent context.Context) context.Context {
	t.Helper()
	service, artifactID := pass.service, pass.room
	ledger := &Ledger{service: service, ownTx: true}
	ctx := withRoomCursor(store.WithTransactionTracking(withLedger(parent, ledger)), pass.cursor)
	tx, err := pass.settlements.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settlement's transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); errors.Is(err, pgx.ErrTxClosed) {
			return
		}
		service.discardSuppressedPersistence(artifactID, pass.slots...)
	})
	pass.ledger, pass.tx = ledger, tx
	owner, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		t.Fatalf("lock the document's owner row: %v", err)
	}
	if !open {
		t.Fatal("the document's owner is closed")
	}
	pass.owner = owner
	return withOwnerVerified(ctx)
}

// requireStageGoesOn requires the stage named to have left the pass going on, rather than ending
// it with an outcome of its own.
func requireStageGoesOn(t *testing.T, stage string, step settleStep, outcome settleOutcome) {
	t.Helper()
	if step != settleOn {
		t.Fatalf("%s ended the settlement at step %s with outcome %+v", stage, step, outcome)
	}
}

// reconciledStagePass runs one pass up to its reconciliation, as settlePass runs it: the take, the
// transaction holding the owner row, the locked read, the stamp, the stamped blocks' authors and
// the ask blocks' reconciliation. It answers the pass and the context its later stages run under.
func reconciledStagePass(t *testing.T, settle *settlement, parent context.Context) (*settlementPass, context.Context) {
	t.Helper()
	pass := &settlementPass{settlement: settle}
	step, outcome := pass.take()
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, pass, parent)
	step, outcome = pass.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	step, outcome = pass.applyStamp(ctx)
	requireStageGoesOn(t, "applyStamp", step, outcome)
	if len(pass.slots) > 0 {
		t.Fatal("the pass wrote a stamp into the room, which commits alone rather than being reconciled")
	}
	if err := recordStampedAskAuthors(ctx, pass.tx, pass.room, pass.renamed, pass.carried); err != nil {
		t.Fatalf("record the stamped ask blocks' authors: %v", err)
	}
	reconciliation, err := settle.service.reconcileAskBlocks(ctx, pass.tx, pass.room, pass.owner, pass.read, pass.credit)
	if err != nil {
		t.Fatalf("reconcile the document's ask blocks: %v", err)
	}
	pass.reconciliation = reconciliation
	return pass, ctx
}

// seedAnsweredAskBlock seeds a document holding one ask block, settles it so the block is
// indexed, and answers that ask in its row alone, so the block disagrees with its row until a
// settlement repairs it. It returns the markdown the repaired document reads as.
func seedAnsweredAskBlock(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	return ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext.\n"
}

// take copies the room's tree at an exact commit position and renders it, under no lock of the
// document's, and stands at the generation the room holds then, which every later stage compares
// the room against.
func TestSettlementTakeCopiesTheRoomsTreeAndRendersIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Context.\n")
	settleCurrentGeneration(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	pass := &settlementPass{settlement: settle}
	step, outcome := pass.take()
	requireStageGoesOn(t, "take", step, outcome)

	if pass.read == nil {
		t.Fatal("take copied no tree for the pass to reconcile and version")
	}
	if pass.markdown != "Context.\n" || pass.renderErr != nil {
		t.Fatalf("take rendered %q (%v), want the seeded text", pass.markdown, pass.renderErr)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	if pass.generation != generation {
		t.Fatalf("take stands at generation %d, want the room's %d", pass.generation, generation)
	}
	if pass.cursor == nil || pass.taken.heldThrough != pass.cursor.heldThrough() {
		t.Fatalf("take stands at update %d, want the instance's %v", pass.taken.heldThrough, pass.cursor)
	}
}

// readLocked reads what a version of the pass is compared against and credited to, under the
// document's owner row and cursor lock.
func TestSettlementReadLockedReadsWhatTheVersionIsComparedAgainst(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Context.\n")
	settleCurrentGeneration(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	pass := &settlementPass{settlement: settle}
	step, outcome := pass.take()
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, pass, parent)
	step, outcome = pass.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)

	if pass.latest.markdown != "Context.\n" {
		t.Fatalf("readLocked read the latest version as %q, want the settled text", pass.latest.markdown)
	}
	if pass.credit.state != settle.state {
		t.Fatalf("readLocked credited room state %p, want the settlement's %p", pass.credit.state, settle.state)
	}
}

// applyStamp stamps into the room the block ids the document lacks, holding the stamp's update
// from the room's persistence until the pass stores it.
func TestSettlementApplyStampStampsTheBlockIdsTheDocumentLacks(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	owedStamp(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	pass := &settlementPass{settlement: settle}
	step, outcome := pass.take()
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, pass, parent)
	step, outcome = pass.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	if repairs := pmdoc.BlockIDRepairCount(pass.read); repairs != 1 {
		t.Fatalf("the document the pass read needs %d block ids, want the one the test left it", repairs)
	}
	if !pass.stamping {
		t.Fatal("take left the pass stamping nothing, with a block id owed")
	}
	step, outcome = pass.applyStamp(ctx)
	// settleOn and not settleDone: the stamp is in the room, and the pass goes on to commit it
	// alone (commitStamp); the document it leaves is reconciled and versioned by a later pass.
	requireStageGoesOn(t, "applyStamp", step, outcome)

	if pass.stamp == nil || len(pass.slots) != 1 {
		t.Fatalf("applyStamp wrote %d updates into the room with stamp %v, want the one stamp", len(pass.slots), pass.stamp != nil)
	}
	if pass.stampSuperseded {
		t.Fatal("applyStamp ended the settlement with its stamp, with nothing having moved the room")
	}
}

// commitStamp commits the stamp alone: the stamp is in the room and its browsers, so it is stored
// and committed with no version, and the settlement goes on to a pass that reads the stamped
// document.
func TestSettlementCommitStampCommitsTheStampAlone(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	owedStamp(t, service, artifactID)
	versions := latestVersionNumber(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	pass := &settlementPass{settlement: settle}
	step, outcome := pass.take()
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, pass, parent)
	step, outcome = pass.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	step, outcome = pass.applyStamp(ctx)
	requireStageGoesOn(t, "applyStamp", step, outcome)

	step, outcome = pass.commitStamp(ctx)
	if step != settleAgain || outcome != (settleOutcome{}) {
		t.Fatalf("commitStamp ended at step %s with outcome %+v, want the stamp committed and a pass left", step, outcome)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the stamp committed alone")
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, service.store, artifactID)); repairs != 0 {
		t.Fatalf("the stored document still needs %d block ids after its stamp committed", repairs)
	}
	if number := latestVersionNumber(t, service, artifactID); number != versions {
		t.Fatalf("the stamp that committed alone wrote version %d, want the document left at %d", number, versions)
	}
}

// planRepairs plans the repairs the reconciliation found, outside the document's locks: its pass
// rolls back having written nothing into the room, and the plan is what the next pass commits.
func TestSettlementPlanRepairsPlansTheAnsweredAskBlocksRepair(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedAnsweredAskBlock(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	pass, ctx := reconciledStagePass(t, settle, parent)
	if len(pass.reconciliation.repairs) != 1 {
		t.Fatalf("the reconciliation found %d repairs, want the answered ask block's one", len(pass.reconciliation.repairs))
	}
	if blockID := pass.reconciliation.repairs[0].blockID; blockID != "ask-block" {
		t.Fatalf("the reconciliation found a repair of block %q, want the document's ask block", blockID)
	}

	step, outcome := pass.planRepairs(ctx)
	if step != settleAgain || outcome != (settleOutcome{}) {
		t.Fatalf("planRepairs ended at step %s with outcome %+v, want the repairs planned and a pass left", step, outcome)
	}
	if settle.plan == nil {
		t.Fatal("planRepairs planned nothing for the next pass to commit")
	}
	if settle.plan.markdown != pass.markdown || settle.plan.heldThrough != pass.taken.heldThrough {
		t.Fatalf("planRepairs planned for the read at %d rendering %q, want this pass's %d and %q",
			settle.plan.heldThrough, settle.plan.markdown, pass.taken.heldThrough, pass.markdown)
	}
	if len(pass.slots) != 0 {
		t.Fatalf("planRepairs wrote %d updates into the room, want none: commitRepairs writes them", len(pass.slots))
	}
}

// commitRepairs writes the planned repairs into the room and commits them with the rows and
// events their reconciliation wrote, and no version: the pass after it versions what they left.
func TestSettlementCommitRepairsWritesTheRepairIntoTheRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedAnsweredAskBlock(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	planning, planningCtx := reconciledStagePass(t, settle, parent)
	step, outcome := planning.planRepairs(planningCtx)
	if step != settleAgain {
		t.Fatalf("planRepairs ended at step %s with outcome %+v, want the repairs planned", step, outcome)
	}

	// The second pass takes the plan and commits its repairs: commitRepairs renders nothing
	// itself and writes what the plan left (p.planned), so planRepairs is how it is reached.
	committing, ctx := reconciledStagePass(t, settle, parent)
	step, outcome = committing.planRepairs(ctx)
	if step != settleAgain || outcome != (settleOutcome{}) {
		t.Fatalf("the second pass ended at step %s with outcome %+v, want its repairs committed", step, outcome)
	}
	if len(committing.slots) != 1 {
		t.Fatalf("commitRepairs wrote %d updates into the room, want the one repair", len(committing.slots))
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the repairs committed")
}

// The stages settle a document in the order settlePass runs them, pass after pass: the repair the
// reconciliation plans reaches the room, the store and the document's version. Swap two of them
// below and the test fails - with the repair planned after the pass that writes it, the
// settlement writes nothing into the room and versions the document unrepaired.
func TestSettlementStagesSettleTheDocumentInOrder(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	want := seedAnsweredAskBlock(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()

	// The first pass plans the repairs and writes nothing: what each of the two repair passes
	// leaves is asserted between them rather than after both, because that is what holds their
	// order.
	planning, planningCtx := reconciledStagePass(t, settle, parent)
	if len(planning.reconciliation.repairs) != 1 {
		t.Fatalf("the first pass reconciled %d repairs, want the one the answered ask block needs", len(planning.reconciliation.repairs))
	}
	step, outcome := planning.planRepairs(planningCtx)
	if step != settleAgain {
		t.Fatalf("planRepairs ended at step %s with outcome %+v, want the repairs planned", step, outcome)
	}
	if len(planning.slots) != 0 {
		t.Fatalf("the planning pass wrote %d updates into the room, want none", len(planning.slots))
	}

	// The second pass takes that plan and commits the repairs it leaves.
	committing, committingCtx := reconciledStagePass(t, settle, parent)
	step, outcome = committing.planRepairs(committingCtx)
	if step != settleAgain || outcome != (settleOutcome{}) {
		t.Fatalf("the second pass ended at step %s with outcome %+v, want its repairs committed", step, outcome)
	}
	if len(committing.slots) != 1 {
		t.Fatalf("the second pass wrote %d updates into the room, want the one repair its plan left", len(committing.slots))
	}

	// The third pass versions the document the repairs left, needing no repair of its own.
	versioning, versioningCtx := reconciledStagePass(t, settle, parent)
	if len(versioning.reconciliation.repairs) != 0 {
		t.Fatalf("the third pass found %d repairs owed, want the document repaired", len(versioning.reconciliation.repairs))
	}
	step, outcome = versioning.commitVersion(versioningCtx)
	if step != settleDone || outcome != (settleOutcome{}) {
		t.Fatalf("commitVersion ended at step %s with outcome %+v, want the settlement ended", step, outcome)
	}

	requireNoSuppressedSlots(t, service, artifactID, "after the settlement's stages ran")
	waitForDocumentText(t, service, artifactID, want)
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}
