package refs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// roomAuthor wrote every version of the issue room these tests index (writeStoredVersion), and
// introduced each of its blocks (writerSources).
var roomAuthor = privateAuthor

// roomFragment is one document's fragment of an issue room as a test writes it: the document and
// the markdown of its fragment.
type roomFragment struct{ document, markdown string }

// renderRoomVersion renders fragments laid end to end as the one text of the issue room whose tree
// artifact is tree, as docs' fragmentsText lays them, each block's Document the document whose
// fragment holds it where that is not the tree itself (docs' blockDocuments): the first blocks of
// the text are the first fragment's, as many as its own render holds, and so on.
func renderRoomVersion(t *testing.T, tree string, fragments ...roomFragment) typedVersion {
	t.Helper()
	var documents, markdowns []string
	for _, fragment := range fragments {
		for range renderTypedVersion(t, fragment.markdown).blocks {
			documents = append(documents, fragment.document)
		}
		markdowns = append(markdowns, fragment.markdown)
	}
	version := renderTypedVersion(t, strings.Join(markdowns, "\n"))
	if len(version.blocks) != len(documents) {
		t.Fatalf("the room's text holds %d blocks and its fragments %d, want the fragments laid end to end", len(version.blocks), len(documents))
	}
	for index := range version.blocks {
		if documents[index] != tree {
			version.blocks[index].Document = documents[index]
		}
	}
	return version
}

// roomFacts is what DecisionFacts reads of version, the text of the issue room whose tree artifact
// is tree over versions, as a pass over the room reads it: against what the row of the newest of
// those versions says (ReadIndexedVersion over Versions.Newest), which it answers too, every block
// roomAuthor's.
func roomFacts(ctx context.Context, tx pgx.Tx, tree string, versions Versions, version typedVersion) (IndexedVersion, TypedFacts, error) {
	lead, newest := versions.Newest(tree)
	record, err := ReadIndexedVersion(ctx, tx, tree, lead, newest.Number)
	if err != nil {
		return IndexedVersion{}, TypedFacts{}, err
	}
	facts, err := DecisionFacts(ctx, tx, tree, record, versions, version.blocks, version.tree, version.pointers, writerSources(tx, tree, roomAuthor))
	return record, facts, err
}

// indexRoomWhole runs the block index over version, the room's text over versions, with its typed
// facts, as the room's own pass runs it in the fixture's transaction.
func indexRoomWhole(t *testing.T, tx pgx.Tx, tree string, versions Versions, version typedVersion) {
	t.Helper()
	ctx := context.Background()
	_, facts, err := roomFacts(ctx, tx, tree, versions, version)
	if err != nil {
		t.Fatalf("read the typed facts of the room: %v", err)
	}
	if _, won, _, _, err := IndexBlocks(ctx, tx, tree, versions, version.markdown, version.blocks, version.located, version.pointers, "", facts); err != nil || !won {
		t.Fatalf("index the room whole: won %t, %v", won, err)
	}
}

// planRoomTypedAlone runs the typed-node pass alone over version, the room's text over versions,
// as a write runs it where no block index runs: a write above the deferral bound over the whole
// room, fragment "", or a write of the document fragment of a room left owed over that fragment
// alone (docs' indexRoomLeftOwed). It writes the plan in the fixture's transaction and answers it.
func planRoomTypedAlone(t *testing.T, tx pgx.Tx, tree string, versions Versions, version typedVersion, fragment string) DecisionIndexing {
	t.Helper()
	ctx := context.Background()
	_, facts, err := roomFacts(ctx, tx, tree, versions, version)
	if err != nil {
		t.Fatalf("read the typed facts of the room: %v", err)
	}
	index, err := ReadBlockIndex(ctx, tx, tree)
	if err != nil {
		t.Fatalf("read the room's block index: %v", err)
	}
	plan, err := PlanDecisionNodes(ctx, tx, index, tree, facts, graphstore.Uncaused{}, fragment)
	if err != nil {
		t.Fatalf("plan the typed-node pass over the room: %v", err)
	}
	if err := store.RunWrite(ctx, tx, plan.Queue); err != nil {
		t.Fatalf("write the typed-node pass over the room: %v", err)
	}
	return plan
}

// planRoomDeferred runs, over version, the room's text over versions, the two passes a write that
// leaves the room's blocks to the block-index reconciliation runs in its own transaction (docs'
// planTypedPasses): the typed-node pass over the whole room, then, against the index it leaves,
// the requirements, checks and plan tasks the write states at once (DecisionIndexing's
// PlanAuthored). It writes both in the fixture's transaction and answers their plan.
func planRoomDeferred(t *testing.T, tx pgx.Tx, tree string, versions Versions, version typedVersion) DecisionIndexing {
	t.Helper()
	ctx := context.Background()
	record, facts, err := roomFacts(ctx, tx, tree, versions, version)
	if err != nil {
		t.Fatalf("read the typed facts of the room: %v", err)
	}
	index, err := ReadBlockIndex(ctx, tx, tree)
	if err != nil {
		t.Fatalf("read the room's block index: %v", err)
	}
	decisions, err := PlanDecisionNodes(ctx, tx, index, tree, facts, graphstore.Uncaused{}, "")
	if err != nil {
		t.Fatalf("plan the typed-node pass over the room: %v", err)
	}
	plan, err := decisions.PlanAuthored(ctx, tx, record, versions, version.markdown, version.blocks, version.located, version.pointers, windowServer, record.At)
	if err != nil {
		t.Fatalf("plan the requirements, checks and plan tasks of the room: %v", err)
	}
	if err := store.RunWrite(ctx, tx, plan.Queue); err != nil {
		t.Fatalf("write the passes over the room: %v", err)
	}
	return plan
}

// blockTies is the typed rows of block id of the room's index (typedRowsOf).
func blockTies(t *testing.T, tx pgx.Tx, tree, id string) []string {
	t.Helper()
	var ties []string
	for _, row := range typedRowsOf(t, tx, tree) {
		if strings.HasPrefix(row, id+" ") {
			ties = append(ties, row)
		}
	}
	return ties
}

// decisionMarkdown is the decision block id, written alone.
func decisionMarkdown(id string) string {
	return ":::decision{#" + id + "}\nWe chose REST for " + id + ".\n:::\n"
}

// requirementMarkdown is the requirement block id, written alone.
func requirementMarkdown(id string) string {
	return ":::requirement{#" + id + "}\nThe requirement " + id + ".\n:::\n"
}

// A write of a room left owed runs the typed-node pass over its own document's fragment alone
// (docs' indexRoomLeftOwed). The spec's fragment holds no decision, so its facts are bare, and the
// pass's reconcile of the ties of the blocks the version no longer holds as decisions reaches the
// blocks of that fragment alone (pairScope.holding): the Verification document's decision d1,
// whose row the room's own pass wrote, keeps its ties, which a pass reaching every block of the
// room's index would drop.
func TestALeftOwedPassOverOneFragmentLeavesTheTiesOfAnotherFragmentsDecision(t *testing.T) {
	tx := windowFixture(t)
	spec, _, verification, tree, _ := roomFixture(t, tx)
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 1, requirementMarkdown("r1"))
	verificationAt := writeStoredVersion(t, tx, verification, 1, decisionMarkdown("d1"))
	indexRoomWhole(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, renderRoomVersion(t, tree, roomFragment{spec, requirementMarkdown("r1")}, roomFragment{verification, decisionMarkdown("d1")}))
	ties := blockTies(t, tx, tree, "d1")
	if len(ties) == 0 {
		t.Fatalf("the room's own pass wrote no tie of the decision d1, so this test proves nothing")
	}

	written := requirementMarkdown("r1") + "\n" + requirementMarkdown("r2")
	specNext := writeStoredVersion(t, tx, spec, 2, written)
	planRoomTypedAlone(t, tx, tree, Versions{spec: {Number: 2, At: specNext}}, renderRoomVersion(t, tree, roomFragment{spec, written}), spec)

	if after := blockTies(t, tx, tree, "d1"); !slices.Equal(after, ties) {
		t.Errorf("the pass over the spec's fragment left the decision d1 of the Verification document's fragment the ties %v, want %v", after, ties)
	}
}

// A retired document's fragment leaves its room, and every pass over the room keeps its blocks as
// the retirement left them (BlockIndexing.retained). A write of the room above the deferral bound
// runs the typed-node pass over the whole room alone, whose text holds no fragment of the retired
// Verification document, and its reconcile of the ties of the blocks the version no longer holds as
// decisions leaves the retired fragment's decision d1 its ties.
func TestADeferredRoomPassKeepsTheTiesOfARetiredFragmentsDecision(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	spec, _, verification, tree, treeRef := roomFixture(t, tx)
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 1, requirementMarkdown("r1"))
	verificationAt := writeStoredVersion(t, tx, verification, 1, decisionMarkdown("d1"))
	indexRoomWhole(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, renderRoomVersion(t, tree, roomFragment{spec, requirementMarkdown("r1")}, roomFragment{verification, decisionMarkdown("d1")}))
	ties := blockTies(t, tx, tree, "d1")
	if len(ties) == 0 {
		t.Fatalf("the room's own pass wrote no tie of the decision d1, so this test proves nothing")
	}
	if _, err := tx.Exec(ctx, `update artifacts set retired_at = now(), role = null where id = $1::uuid`, verification); err != nil {
		t.Fatalf("retire the Verification document: %v", err)
	}

	written := requirementMarkdown("r1") + "\n" + requirementMarkdown("r2")
	specNext := writeStoredVersion(t, tx, spec, 2, written)
	planRoomTypedAlone(t, tx, tree, Versions{tree: {Number: 1, At: treeAt}, spec: {Number: 2, At: specNext}},
		renderRoomVersion(t, tree, roomFragment{spec, written}), "")

	if after := blockTies(t, tx, tree, "d1"); !slices.Equal(after, ties) {
		t.Errorf("the deferred pass over the room left the retired fragment's decision d1 the ties %v, want %v", after, ties)
	}
	if node, found, err := graphstore.ReadNode(ctx, tx, BlockNodeKind, treeRef+"#d1"); err != nil || !found || node.State != graphstore.StateLive {
		t.Errorf("the retired fragment's decision d1 reads node state %q (found %t, %v), want live, as the retirement left it", node.State, found, err)
	}
}

// A write of the room above the deferral bound runs the typed-node pass over the whole room alone,
// over the latest version of each document: the Verification document's version 2, which removed
// its decision d1, and the spec's version 2, written after it. The reconcile of the ties of the
// blocks the version no longer holds as decisions drops d1's and retracts each one's tuple at the
// time of the version of d1's own document that removed it, the Verification document's version 2
// (BlockIndexing.versionOf over the document d1's row names), as a pass that drops a block dates
// its rows (planGone): neither the spec's version, the newest, nor the tree's own.
func TestADeferredRoomPassDatesARemovedDecisionsTiesAtItsFragmentsVersion(t *testing.T) {
	tx := windowFixture(t)
	spec, _, verification, tree, treeRef := roomFixture(t, tx)
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 1, requirementMarkdown("r1"))
	decided := decisionMarkdown("d1") + "\n" + decisionMarkdown("d2")
	verificationAt := writeStoredVersion(t, tx, verification, 1, decided)
	indexRoomWhole(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}, verification: {Number: 1, At: verificationAt},
	}, renderRoomVersion(t, tree, roomFragment{spec, requirementMarkdown("r1")}, roomFragment{verification, decided}))
	if len(blockTies(t, tx, tree, "d1")) == 0 {
		t.Fatalf("the room's own pass wrote no tie of the decision d1, so this test proves nothing")
	}

	removedAt := writeStoredVersion(t, tx, verification, 2, decisionMarkdown("d2"))
	written := requirementMarkdown("r1") + "\n" + requirementMarkdown("r2")
	specNext := writeStoredVersion(t, tx, spec, 2, written)
	if !removedAt.Before(specNext) || !treeAt.Before(removedAt) {
		t.Fatalf("the tree was written at %s, the version that removed d1 at %s and the spec's at %s, want three times of the database's clock in order", treeAt, removedAt, specNext)
	}
	plan := planRoomTypedAlone(t, tx, tree, Versions{
		tree: {Number: 1, At: treeAt}, spec: {Number: 2, At: specNext}, verification: {Number: 2, At: removedAt},
	}, renderRoomVersion(t, tree, roomFragment{spec, written}, roomFragment{verification, decisionMarkdown("d2")}), "")

	if after := blockTies(t, tx, tree, "d1"); len(after) != 0 {
		t.Errorf("the deferred pass that removed d1 left it the ties %v, want none", after)
	}
	var retracted []time.Time
	for _, held := range plan.Stated().Tuples {
		if tuple := held.Tuple; tuple.FromKind == BlockNodeKind && tuple.FromID == treeRef+"#d1" && slices.Contains(typedKinds, tuple.Kind) && !tuple.Asserted {
			retracted = append(retracted, tuple.At)
		}
	}
	if len(retracted) == 0 {
		t.Fatalf("the deferred pass that removed d1 retracted no tie of it")
	}
	for _, at := range retracted {
		if !at.Equal(removedAt) {
			t.Errorf("the deferred pass retracted a tie of d1 at %s, want the Verification document's version 2, %s (the spec's version is %s and the tree's %s)", at, removedAt, specNext, treeAt)
		}
	}
}

// A write of a room left owed runs the typed-node pass over its own document's fragment alone
// (docs' indexRoomLeftOwed), which writes no document_blocks row of a decision it places: only the
// room's own pass writes that, and the pass records the decision's document in the room
// (planRoomTypedBlocks). A later write of the same room still left owed retypes the decision d1
// into a requirement: its facts are bare, and the reconcile of the ties of the blocks the version
// no longer holds as decisions reaches d1, whose fragment the room's record names
// (unrowedTieBlocks), and drops its ties, which would stand until the room's own pass otherwise.
func TestALeftOwedRetypeOfADecisionAnEarlierPassOfItsFragmentPlacedDropsItsTies(t *testing.T) {
	ctx := context.Background()
	tx := windowFixture(t)
	spec, _, verification, tree, _ := roomFixture(t, tx)
	treeAt := writeStoredVersion(t, tx, tree, 1, "")
	specAt := writeStoredVersion(t, tx, spec, 1, requirementMarkdown("r1"))
	indexRoomWhole(t, tx, tree, Versions{tree: {Number: 1, At: treeAt}, spec: {Number: 1, At: specAt}},
		renderRoomVersion(t, tree, roomFragment{spec, requirementMarkdown("r1")}))

	placedAt := writeStoredVersion(t, tx, verification, 1, decisionMarkdown("d1"))
	planRoomTypedAlone(t, tx, tree, Versions{verification: {Number: 1, At: placedAt}},
		renderRoomVersion(t, tree, roomFragment{verification, decisionMarkdown("d1")}), verification)
	if len(blockTies(t, tx, tree, "d1")) == 0 {
		t.Fatalf("the pass over the Verification document's fragment wrote no tie of the decision d1, so this test proves nothing")
	}
	var rows, recorded int
	if err := tx.QueryRow(ctx, `
		select (select count(*) from document_blocks where artifact_id = $1::uuid and block_id = 'd1'),
		       (select count(*) from issue_room_blocks where tree_artifact_id = $1::uuid and block_id = 'd1' and document_id = $2::uuid)
	`, tree, verification).Scan(&rows, &recorded); err != nil {
		t.Fatalf("read the room's row and record of d1: %v", err)
	}
	if rows != 0 || recorded != 1 {
		t.Fatalf("the room holds %d document_blocks rows of d1 and %d records of it in the Verification document, want none and one, or this test proves nothing", rows, recorded)
	}

	retyped := requirementMarkdown("d1")
	retypedAt := writeStoredVersion(t, tx, verification, 2, retyped)
	planRoomTypedAlone(t, tx, tree, Versions{verification: {Number: 2, At: retypedAt}},
		renderRoomVersion(t, tree, roomFragment{verification, retyped}), verification)

	if after := blockTies(t, tx, tree, "d1"); len(after) != 0 {
		t.Errorf("the pass over the version that retyped d1 into a requirement left it the ties %v, want none", after)
	}
}
