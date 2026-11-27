package refs

import (
	"context"
	crand "crypto/rand"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// TestAMentionWhoseFarEndIsPastTheBoundLeavesOutItsTuple is this change's R2 at the bound
// for a version's block index. A block's address is one end of a mention's tuple and the target is
// the other: graph_tuples' read and repeat indexes key both ends whole, so a block address of
// 1,024 bytes beside a reference target of text.MaxTargetBytes (2,048) is a key of about 3 KiB,
// past the roughly 2,704 bytes a btree entry holds. The block index now leaves out the tuple
// (planBlockRefs) and stamps nothing on the block_refs row it projects (queueBlockRefs), so the
// version write goes on: the block's node and its in_document tuple are written, the row keeps
// its null node columns for the old `block_refs` arm, and no mention tuple is appended.
func TestAMentionWhoseFarEndIsPastTheBoundLeavesOutItsTuple(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	const server = "https://dispatch.example"
	// A reference to a block of another document whose target id is exactly the bound, the widest
	// far end a mention can hold.
	target := randText(text.MaxTargetBytes - len("CORE-1/#x"))
	markdown := "See dispatch://CORE-1/artifact/" + target + "#b-x.\n"
	located := text.ExtractAt(markdown, text.NewOrigins(server))
	if len(located) != 1 || located[0].Kind != "block" {
		t.Fatalf("the reference at the bound parsed as %#v, want one block", located)
	}
	if id := text.TargetID(located[0].Ref); len(id) != text.MaxTargetBytes {
		t.Fatalf("the target is %d bytes, want the bound %d", len(id), text.MaxTargetBytes)
	}

	// The citing document's ref_key and the citing block's id make a block address of exactly the
	// store's bound: the ref_key is `ACME/<slug>`, and the block id fills the rest to 1,024 bytes.
	createReferenceProject(t, tx, "ACME")
	citer, refKey := createReferenceArtifact(t, tx, "", "ACME", randText(800))
	blockID := randText(graphstore.MaxAddressBytes - len(refKey) - len("#"))
	if len(blockID) > text.MaxBlockIDBytes {
		t.Fatalf("the citing block id is %d bytes, past the block-id bound %d: shorten the ref_key", len(blockID), text.MaxBlockIDBytes)
	}
	address := refKey + "#" + blockID
	if len(address) != graphstore.MaxAddressBytes {
		t.Fatalf("the citing block's address is %d bytes, want the bound %d", len(address), graphstore.MaxAddressBytes)
	}

	blocks := []model.ArtifactBlock{{ID: blockID, Type: "paragraph", From: 0, To: len(markdown)}}
	_, won, write, err := IndexBlocks(ctx, tx, citer, 1, time.Now(), markdown, blocks, located, nil, "")
	if err != nil || !won {
		t.Fatalf("index the version (won %v): %v", won, err)
	}
	// The event that caused the write appends its tuples. On the old code the mention tuple went in
	// here and failed with "index row size ... exceeds btree ... 2704"; now it is left out.
	if err := graphstore.Caused(ctx, tx, write, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the version stated: %v", err)
	}

	// The block's node is written: its address fits on its own.
	if _, held, err := graphstore.ReadNode(ctx, tx, BlockNodeKind, address); err != nil || !held {
		t.Fatalf("the store holds the citing block's node %t (err %v), want it", held, err)
	}
	// The in_document tuple is written; the mention tuple to the over-bound target is not.
	inDocument, err := graphstore.History(ctx, tx, BlockNodeKind, address, TupleInDocument)
	if err != nil {
		t.Fatalf("read the block's in_document tuples: %v", err)
	}
	if len(inDocument) == 0 {
		t.Fatalf("the block states no in_document tuple, want one")
	}
	var mentions int
	if err := tx.QueryRow(ctx, `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and to_kind = 'block'
	`, BlockNodeKind, address).Scan(&mentions); err != nil {
		t.Fatalf("count the block's mention tuples: %v", err)
	}
	if mentions != 0 {
		t.Fatalf("the block states %d mention tuple(s), want none: the far end is past the bound", mentions)
	}
	// The block_refs row keeps its null node columns, which the old arm answers.
	var nodeKind, nodeAddress *string
	if err := tx.QueryRow(ctx, `
		select node_kind, node_address from block_refs where artifact_id = $1::uuid and to_kind = 'block'
	`, citer).Scan(&nodeKind, &nodeAddress); err != nil {
		t.Fatalf("read the block_refs row: %v", err)
	}
	if nodeKind != nil || nodeAddress != nil {
		t.Fatalf("the block_refs row names node (%v, %v), want null columns", nodeKind, nodeAddress)
	}
}

// randText is an incompressible string of n lower-case alphanumeric bytes: a btree compresses a
// run of one byte, so a control needs an address the index cannot shrink under its bound.
func randText(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
