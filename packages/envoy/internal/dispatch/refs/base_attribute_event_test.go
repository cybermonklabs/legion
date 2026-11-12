package refs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Two values of one base attribute of one node, stated under one event, are both held in the
// log.
//
// This is the hazard the repeat key switched on once base attributes started carrying real
// events. `graph_tuples_value_once` is the key that makes an append repeat safely: one event
// states one fact once, so a writer that is retried appends the same row again and
// `on conflict do nothing` drops it. Its columns decide what "the same fact" means, and until
// this round they were (from_kind, from_id, kind, source_event, asserted) - the value itself
// left out, because a value has no length bound and a btree key is bounded at about 2,704
// bytes. With the value out of the key, two different values of one attribute of one node under
// one event are one key: the first is kept and the second is dropped, silently, leaving the row
// holding the last value and the log the first. The key now carries md5(to_id), which is the
// same fact test for a retry and a different one for a different value
// (store/migrations/20261109013400_graph_store.up.sql).
//
// No writer of this head reaches that, and this test is what holds that true for the next one.
// Read path by path at this head:
//
//   - the comment path writes its node once per request (commentStoreWrite.writeNode) and never
//     states a comment gone: refs/blocks.go is the only writer in the tree that puts
//     graphstore.StateGone on a node;
//   - the block index states each node once per pass - the ids a version holds are stated live
//     and the ids it dropped are stated gone, and the two sets are disjoint - and every caller
//     causes one pass with one event (docs/block_index.go, docs/service.go, api/artifacts.go
//     twice, api/issue_create.go, cmd/dispatch-seed/write_docs.go);
//   - the backfill writes each legacy row once, under that row's own event, and a block's node
//     address comes from one row;
//   - the seed writes at the sentinel, which both keys leave out.
//
// So the test drives the closest a writer can come: two index passes of one document in one
// transaction, the first dropping a block and the second restoring it, both appended under one
// event. Under the key this branch ships the log holds both values; under the key it replaces
// the restore is dropped and the node row reads live while the log's newest value reads gone.
func TestTwoValuesOfOneBaseAttributeUnderOneEventAreBothHeld(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "BASE", "base-attribute-md")

	whole := blockDoc{markdown: "aa\nbb\n", blocks: []model.ArtifactBlock{
		{ID: "a", Type: "paragraph", From: 0, To: 3},
		{ID: "b", Type: "paragraph", From: 3, To: 6},
	}}
	without := blockDoc{markdown: "aa\n", blocks: []model.ArtifactBlock{
		{ID: "a", Type: "paragraph", From: 0, To: 3},
	}}

	first := document.writeVersion(t, 1, whole.markdown, writer)
	if won, _ := document.indexVersion(t, 1, whole, first); !won {
		t.Fatalf("the document's first index did not become the version its blocks stand at")
	}
	dropping := document.writeVersion(t, 2, without.markdown, writer)
	restoring := document.writeVersion(t, 3, whole.markdown, writer)

	// One transaction, one event, two passes: the block is stated gone and then live again.
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the two passes: %v", err)
	}
	defer tx.Rollback(ctx)
	_, _, dropped, err := IndexBlocks(ctx, tx, document.artifact, 2, dropping.at, without.markdown, without.blocks, nil, nil)
	if err != nil {
		t.Fatalf("index the version that drops the block: %v", err)
	}
	if err := dropped.Caused(ctx, tx, dropping.event); err != nil {
		t.Fatalf("append what the version that drops the block stated: %v", err)
	}
	_, _, restored, err := IndexBlocks(ctx, tx, document.artifact, 3, restoring.at, whole.markdown, whole.blocks, nil, nil)
	if err != nil {
		t.Fatalf("index the version that restores the block: %v", err)
	}
	if err := restored.Caused(ctx, tx, dropping.event); err != nil {
		t.Fatalf("append what the version that restores the block stated: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the two passes: %v", err)
	}

	address := document.address("b")
	history, err := graphstore.History(ctx, database.Pool, BlockNodeKind, address, "state")
	if err != nil {
		t.Fatalf("read the block's state history: %v", err)
	}
	stated := make([]string, 0, len(history))
	for _, tuple := range history {
		stated = append(stated, tuple.ToID)
	}
	want := []string{graphstore.StateLive, graphstore.StateGone, graphstore.StateLive}
	if len(stated) != len(want) {
		t.Fatalf("the log holds %v for the block's state, want %v: a value a write stated is missing from it",
			stated, want)
	}
	for index, value := range want {
		if stated[index] != value {
			t.Fatalf("the log holds %v for the block's state, want %v", stated, want)
		}
	}
	if history[1].SourceEvent != dropping.event || history[2].SourceEvent != dropping.event {
		t.Errorf("the two values stated under one event carry causes %d and %d, want %d for both",
			history[1].SourceEvent, history[2].SourceEvent, dropping.event)
	}

	// The row is the projection of those tuples: what the last pass stated is what it reads.
	node, held, err := graphstore.ReadNode(ctx, database.Pool, BlockNodeKind, address)
	if err != nil || !held {
		t.Fatalf("read the block's node: held=%t err=%v", held, err)
	}
	if node.State != stated[len(stated)-1] {
		t.Errorf("the node row reads state %q while the log's newest value is %q", node.State, stated[len(stated)-1])
	}
}
