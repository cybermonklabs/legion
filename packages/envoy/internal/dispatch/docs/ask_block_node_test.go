package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// An ask block's answer state is the attribute `state` in its markdown and the attribute node
// ask/answer_state in the store (schemanodes.StoreName), because `state` is a base attribute of
// every node: a block node whose `state` tuple read `answered` would be a node with no live state,
// which the census counts as a difference.
//
// The document holds an open ask, which its first settlement indexes as an asks row. The row is
// then answered, and the next settlement writes the answered state into the block and a version
// holding it, whose block index pass writes the block's node. That node stays live, in its row
// and in its log, and the census over the store reads zero differences.
func TestAnAnsweredAskBlockIndexesAsALiveNodeAndTheCensusAgrees(t *testing.T) {
	service, document := newTestService(t)
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

	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range append(append([]graphcensus.Difference{}, kind.ReportedRows...), kind.ReportedNodes...) {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	blocks := censusOfBlocks(t, census)
	if census.Differences() != 0 || census.Refused() || blocks.Rows == 0 || blocks.Nodes == 0 {
		t.Fatalf("the census read %d difference(s), refused=%t, over %d block row(s) and %d block node(s), want zero differences over the ask's blocks",
			census.Differences(), census.Refused(), blocks.Rows, blocks.Nodes)
	}
	t.Logf("the ask block's node %s is %s with %d state tuple(s), and the census reads zero differences over %d row(s), %d of them block rows against %d block node(s)",
		address, node.State, len(history), census.Rows(), blocks.Rows, blocks.Nodes)
}
