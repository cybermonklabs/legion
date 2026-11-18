package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAReindexLeavesAProposedRequirementProposed holds the block index to restating every block
// node it writes (blockNode, graphstore.Node's Restates; dispatch://INFRA-1950/spec#b-r-proposed).
// A requirement a write marked proposed is written again, live, by the pass over a version that
// changes its words, and stays proposed; the pass over a version that drops it writes it gone;
// and the pass over a version that restores it writes it live. After each pass the row and the
// log of its state agree, the log holding one value for each pass that moved the state and none
// for the pass that kept it, and its visibility is the project's throughout.
func TestAReindexLeavesAProposedRequirementProposed(t *testing.T) {
	ctx := context.Background()
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "PROP", "proposed-md")
	const requirement = "r-ship"

	withRequirement := func(words string) blockDoc {
		text := words + "\n"
		return blockDoc{markdown: text + "bb\n", blocks: []model.ArtifactBlock{
			{ID: requirement, Type: "requirement", From: 0, To: len(text)},
			{ID: "b", Type: "paragraph", From: len(text), To: len(text) + len("bb\n")},
		}}
	}
	without := blockDoc{markdown: "bb\n", blocks: []model.ArtifactBlock{
		{ID: "b", Type: "paragraph", From: 0, To: len("bb\n")},
	}}
	index := func(number int, doc blockDoc) versionRecord {
		t.Helper()
		record := document.writeVersion(t, number, doc.markdown, writer)
		if won, _ := document.indexVersion(t, number, doc, record); !won {
			t.Fatalf("version %d did not become the version the document's blocks stand at", number)
		}
		return record
	}

	first := index(1, withRequirement("We ship it."))
	requireRestatedBlock(t, document, requirement, graphstore.StateLive, []string{graphstore.StateLive})

	// A write that owns the state - a plain write, as the state route's is - marks it proposed.
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		return graphstore.Write(ctx, tx, []graphstore.Node{{
			Kind: BlockNodeKind, ID: document.address(requirement), Supertag: BlockSupertag,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateProposed,
			MadeAt: first.at.Add(time.Second), MadeBy: writer, SourceEvent: graphstore.NoEvent,
		}}, nil)
	}); err != nil {
		t.Fatalf("mark the requirement proposed: %v", err)
	}
	requireRestatedBlock(t, document, requirement, graphstore.StateProposed,
		[]string{graphstore.StateLive, graphstore.StateProposed})

	index(2, withRequirement("We ship it today."))
	if changed := document.changes(t)[requirement]; changed.version != 2 {
		t.Fatalf("the requirement's text last changed in version %d, want 2: the pass did not write its node again", changed.version)
	}
	requireRestatedBlock(t, document, requirement, graphstore.StateProposed,
		[]string{graphstore.StateLive, graphstore.StateProposed})

	index(3, without)
	requireRestatedBlock(t, document, requirement, graphstore.StateGone,
		[]string{graphstore.StateLive, graphstore.StateProposed, graphstore.StateGone})

	index(4, withRequirement("We ship it today."))
	requireRestatedBlock(t, document, requirement, graphstore.StateLive,
		[]string{graphstore.StateLive, graphstore.StateProposed, graphstore.StateGone, graphstore.StateLive})
}

// requireRestatedBlock fails unless the block's node row holds state and the project's
// visibility, the log's newest state and visibility are the row's, and the log states exactly
// states, in order, and the project's visibility once.
func requireRestatedBlock(t *testing.T, document parentDocument, blockID, state string, states []string) {
	t.Helper()
	ctx := context.Background()
	address := document.address(blockID)
	node, held, err := graphstore.ReadNode(ctx, document.database.Pool, BlockNodeKind, address)
	if err != nil || !held {
		t.Fatalf("read block %s's node: held %t, %v", blockID, held, err)
	}
	if node.State != state || node.Visibility != graphstore.VisibilityProject {
		t.Errorf("block %s's node reads state %q and visibility %q, want %q and %q",
			blockID, node.State, node.Visibility, state, graphstore.VisibilityProject)
	}
	for _, attribute := range []struct {
		name, row string
		want      []string
	}{
		{graphstore.AttributeState, node.State, states},
		{graphstore.AttributeVisibility, node.Visibility, []string{graphstore.VisibilityProject}},
	} {
		current, held, err := graphstore.CurrentValue(ctx, document.database.Pool, BlockNodeKind, address, attribute.name)
		if err != nil || !held || current != attribute.row {
			t.Errorf("block %s's row reads %s %q and the log's newest is %q (held %t, %v)",
				blockID, attribute.name, attribute.row, current, held, err)
		}
		history, err := graphstore.History(ctx, document.database.Pool, BlockNodeKind, address, attribute.name)
		if err != nil {
			t.Fatalf("read block %s's %s history: %v", blockID, attribute.name, err)
		}
		stated := make([]string, len(history))
		for position, tuple := range history {
			stated[position] = tuple.ToID
		}
		if !slices.Equal(stated, attribute.want) {
			t.Errorf("block %s's log states %s %v, want %v: one value for each write that moved it, none for one that kept it",
				blockID, attribute.name, stated, attribute.want)
		}
	}
}
