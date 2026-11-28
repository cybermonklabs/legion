package docs

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// An ask block's answer state is the attribute node ask/answer_state in the store
// (schemanodes.StoreName), because `state` is a base attribute of every node: a block node
// whose `state` tuple read `answered` would be a node with no live state, which the census counts
// as a difference. While another build serves (serveTwoBuilds), settlement also keeps the answer
// state in the block's markdown as the attribute `state`, for that build's pages
// (rollWindowAskAttributes), so a version can hold a block whose markdown says
// `state="answered"`, as every version the release before wrote can.
//
// The document holds an open ask, which its first settlement indexes as an asks row. The row is
// then answered, and the next settlement writes the answered state into the block and a version
// holding it, whose block index pass writes the block's node. That node stays live, in its row
// and in its log, and the census over the store reads zero differences.
//
// This is a regression lock, not the proof that the store keeps an answer state off a node's
// base state. No writer stores a block's markdown attribute values yet, so this test passes with
// the seed naming the attribute ask/state as well. The proof is schemanodes'
// TestNoAttributeNodeStoresItsValuesUnderABaseAttribute, which fails naming the colliding
// attribute node.
func TestAnAnsweredAskBlockIndexesAsALiveNodeAndTheCensusAgrees(t *testing.T) {
	t.Parallel()
	service, document := newTestService(t)
	serveTwoBuilds(t, service.store)
	service.settle = time.Hour
	database := service.store
	ctx := context.Background()
	seedServiceText(t, service, document, ":::ask{#decision urgency=\"med\" multiple=\"false\" state=\"open\"}\n"+
		"Ship on Monday?\n\n- Yes: Monday.\n- No: Later.\n:::\n\nContext after.\n")
	service.settleRoom(document, 0)
	answerBlockAsk(t, service, document)
	settleCurrentGeneration(t, service, document)
	drainBlockIndex(t, service, "the answered ask")

	var nodeKind, address, blockType string
	if err := database.Pool.QueryRow(ctx, `
		select coalesce(node_kind, ''), coalesce(node_address, ''), type
		from document_blocks where artifact_id = $1 and block_id = 'decision'
	`, document).Scan(&nodeKind, &address, &blockType); err != nil {
		t.Fatalf("read the ask block's row: %v", err)
	}
	if blockType != "ask" || nodeKind != refs.BlockNodeKind || address == "" {
		t.Fatalf("the ask block's row is type %q naming node (%q, %q), want an ask naming a %s node", blockType, nodeKind, address, refs.BlockNodeKind)
	}
	// The text the pass indexed last is the answered block, so the node below is the node of a
	// block whose markdown says `state="answered"`.
	var indexed string
	if err := database.Pool.QueryRow(ctx, `
		select text from document_block_texts
		where artifact_id = $1 and block_id = 'decision' and text is not null
		order by version desc limit 1
	`, document).Scan(&indexed); err != nil {
		t.Fatalf("read the ask block's indexed text: %v", err)
	}
	if !strings.Contains(indexed, `state="answered"`) {
		t.Fatalf("the block index holds the ask block's text %q, want the answered block", indexed)
	}

	node, held, err := graphstore.ReadNode(ctx, database.Pool, nodeKind, address)
	if err != nil {
		t.Fatalf("read the ask block's node: %v", err)
	}
	if !held || node.State != graphstore.StateLive {
		t.Fatalf("the ask block's node is held %t with state %q, want a %s node", held, node.State, graphstore.StateLive)
	}
	value, stated, err := graphstore.CurrentValue(ctx, database.Pool, nodeKind, address, "state")
	if err != nil {
		t.Fatalf("read the ask block's base state: %v", err)
	}
	if !stated || value != graphstore.StateLive {
		t.Errorf("the ask block's base state reads %q (stated %t), want %s", value, stated, graphstore.StateLive)
	}
	history, err := graphstore.History(ctx, database.Pool, nodeKind, address, "state")
	if err != nil {
		t.Fatalf("read the ask block's state history: %v", err)
	}
	for _, tuple := range history {
		if tuple.ToKind != graphstore.KindValue || tuple.ToID != graphstore.StateLive {
			t.Errorf("the ask block's state history holds %s:%s, want %s alone", tuple.ToKind, tuple.ToID, graphstore.StateLive)
		}
	}
	if len(history) == 0 {
		t.Errorf("the ask block's node states no base state at all")
	}

	// The fixture's issue, document and first version are written by SQL (createIssueDocument), as
	// a build before their nodes wrote them (the issue-tree spec): the backfill gives each its
	// node and names no block, every one of which the block index named, so the census below
	// reads the blocks as the index left them.
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the fixture's document: %v", err)
	}
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	// The fixture is one document, so the census as a whole refuses on the kinds it checked no
	// row of (graphcensus.StoreReport.Refused); what this holds it to is the block kind, which
	// must have checked rows and read no difference in either direction.
	blocks := censusOfBlocks(t, census)
	if census.Differences() != 0 || blocks.RowDifferences != 0 || blocks.NodeDifferences != 0 || blocks.Rows == 0 || blocks.Nodes == 0 {
		t.Fatalf("the census read %d difference(s), %d and %d of them of blocks, over %d block row(s) and %d block node(s), want zero differences over the ask's blocks",
			census.Differences(), blocks.RowDifferences, blocks.NodeDifferences, blocks.Rows, blocks.Nodes)
	}
	t.Logf("the ask block's node %s is %s with %d state tuple(s), and the census reads zero differences over %d row(s), %d of them block rows against %d block node(s)",
		address, node.State, len(history), census.Rows(), blocks.Rows, blocks.Nodes)
}

// A settlement that opens the ask of a new ask block holding a shaped pointer, and writes a
// version in the same transaction, states the block's shaped tie. The version's block index reads
// the asks table, which holds no row the settlement has only queued, so it states none of the
// block's ties; the settlement writes them after it (writeOpenedAskTies): the block_refs row,
// naming the ask's node, and the ask's shaped tuple, caused by the ask.opened that records the ask.
func TestASettlementThatOpensAnAskAndWritesAVersionStatesItsShapedTie(t *testing.T) {
	t.Parallel()
	database, service := blockIndexService(t)
	ctx := context.Background()
	const slug = "opened-ties"
	document := writeDocumentThroughTheRequestPath(t, service, database, slug, "# Ties\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	settleCurrentGeneration(t, service, document)
	rig := &docsWriterRig{database: database, service: service}
	before := rig.versionCount(t, document)
	service.addConnection(document, 1, model.Actor{Kind: "user", ID: "alice"})
	shaped := refs.BlockRef(refs.ArtifactRef(&coreIssue, "CORE", nil, slug, "doc", false), "r1")
	editLiveTree(t, service, document, appendBlocks(t, ":::ask{#q-tie urgency=\"med\" multiple=\"false\" shaped=\""+shaped+"\"}\n"+
		"Which store holds the corpus?\n:::\n"))
	settleCurrentGeneration(t, service, document)
	if after := rig.versionCount(t, document); after != before+1 {
		t.Fatalf("the settlement left %d versions of the document, want %d: it is the settlement that writes a version", after, before+1)
	}

	ask := blockAskOn(t, service, document, "q-tie")
	target := graphstore.ArtifactRefKey("CORE-1", "CORE", "", slug) + "#r1"
	var nodeKind, nodeAddress *string
	if err := database.Pool.QueryRow(ctx, `
		select node_kind, node_address from block_refs
		where artifact_id = $1 and block_id = 'q-tie' and kind = $2 and to_kind = $3 and to_id = $4
	`, document, refs.TupleShaped, refs.BlockNodeKind, target).Scan(&nodeKind, &nodeAddress); err != nil {
		t.Fatalf("read the shaped row of q-tie to %s: %v", target, err)
	}
	if nodeKind == nil || nodeAddress == nil || *nodeKind != graphstore.KindAsk || *nodeAddress != ask.ID {
		t.Errorf("the shaped row of q-tie names the node (%v, %v), want the ask's node (%s, %s)", nodeKind, nodeAddress, graphstore.KindAsk, ask.ID)
	}
	ties, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindAsk, ask.ID, []string{refs.TupleShaped})
	if err != nil {
		t.Fatalf("read the shaped ties of ask %s: %v", ask.ID, err)
	}
	if len(ties) != 1 || ties[0].ToKind != refs.BlockNodeKind || ties[0].ToID != target {
		t.Errorf("ask %s states the shaped ties %+v, want one to %s", ask.ID, ties, target)
	}
	if err := rig.tiesCaused(ctx, document, "q-tie"); err != nil {
		t.Error(err)
	}
}

// requireNoCensusDifference fails t on every difference the census reads in each of kinds, which
// must each have checked a row, and logs the census.
func requireNoCensusDifference(t *testing.T, census *graphcensus.StoreReport, kinds ...string) {
	t.Helper()
	var printed strings.Builder
	census.Write(&printed)
	t.Logf("the census:\n%s", printed.String())
	for _, name := range kinds {
		held := false
		for _, kind := range census.Kinds {
			if kind.Kind != name {
				continue
			}
			held = true
			for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
				t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
			}
			if kind.Rows == 0 {
				t.Errorf("the census checked no row of kind %s, want the fixture's", kind.Kind)
			}
		}
		if !held {
			t.Errorf("the census reports no kind %s", name)
		}
	}
}

// An ask block's shaped tie hangs on its ask's node (refs.TypedFacts.pairs), and its writer judges
// the store's bound on the ask's id, not on the block's own address (refs' planBlockRefs), so it
// states the tie wherever the ask is marked. The census holds the tie's block_refs row against that
// tuple on the same node (graphcensus' tieInBound) even where the block's own address is past the
// bound: a document whose ref_key is 1,020 bytes long - a name of up to 2,692 bytes is accepted -
// holds every block past the bound, and its asks within it.
//
// Such a document gains an ask block shaped by a requirement of a document within the bound, and
// its settlement opens the block's ask. The ask's shaped row names the ask's node, which states the
// tie, and the census reads no difference of the shaped kind, whose row and tuple it holds to each
// other, nor of graph_edges' answer of it.
func TestTheCensusHoldsTheShapedTieOfAnAskWhoseBlockIsPastTheBound(t *testing.T) {
	t.Parallel()
	database, service := blockIndexService(t)
	ctx := context.Background()
	const target = "shaped-target"
	targetDocument := writeDocumentThroughTheRequestPath(t, service, database, target, "# Ties\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	settleCurrentGeneration(t, service, targetDocument)
	slug := strings.Repeat("l", graphstore.MaxAddressBytes-4-len("CORE-1/"))
	if refKey := graphstore.ArtifactRefKey("CORE-1", "CORE", "", slug); len(refKey) != graphstore.MaxAddressBytes-4 || graphstore.AddressFits(refKey+"#q-far") {
		t.Fatalf("the document's ref_key is %d bytes and its block q-far fits %t, want %d bytes and a block past the bound",
			len(refKey), graphstore.AddressFits(refKey+"#q-far"), graphstore.MaxAddressBytes-4)
	}
	document := writeDocumentThroughTheRequestPath(t, service, database, slug, "# Far\n\nThe corpus is far away.\n")
	settleCurrentGeneration(t, service, document)
	service.addConnection(document, 1, model.Actor{Kind: "user", ID: "alice"})
	shaped := refs.BlockRef(refs.ArtifactRef(&coreIssue, "CORE", nil, target, "doc", false), "r1")
	editLiveTree(t, service, document, appendBlocks(t, ":::ask{#q-far urgency=\"med\" multiple=\"false\" shaped=\""+shaped+"\"}\n"+
		"Which store holds the corpus?\n:::\n"))
	settleCurrentGeneration(t, service, document)

	ask := blockAskOn(t, service, document, "q-far")
	if !askMarked(t, database, ask.ID) {
		t.Fatalf("ask %s names no node, want it marked: its documents are within the bound", ask.ID)
	}
	far := graphstore.ArtifactRefKey("CORE-1", "CORE", "", target) + "#r1"
	var nodeKind, nodeAddress *string
	if err := database.Pool.QueryRow(ctx, `
		select node_kind, node_address from block_refs
		where artifact_id = $1 and block_id = 'q-far' and kind = $2 and to_kind = $3 and to_id = $4
	`, document, refs.TupleShaped, refs.BlockNodeKind, far).Scan(&nodeKind, &nodeAddress); err != nil {
		t.Fatalf("read the shaped row of q-far to %s: %v", far, err)
	}
	if nodeKind == nil || nodeAddress == nil || *nodeKind != graphstore.KindAsk || *nodeAddress != ask.ID {
		t.Fatalf("the shaped row of q-far names the node (%v, %v), want the ask's node (%s, %s)", nodeKind, nodeAddress, graphstore.KindAsk, ask.ID)
	}
	ties, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindAsk, ask.ID, []string{refs.TupleShaped})
	if err != nil || len(ties) != 1 || ties[0].ToID != far {
		t.Fatalf("ask %s states the shaped ties %+v (%v), want one to %s", ask.ID, ties, err, far)
	}

	requireNoCensusDifference(t, takeStoreCensus(t, database), refs.TupleShaped, refs.TupleShaped+"_edge")
}
