package refs

import (
	"context"
	"encoding/json"
	"fmt"
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

// laterBlockState is the state this build marks a requirement with (graphstore.StateProposed),
// which dispatch://INFRA-1149's release meets on the blocks this build marked, since it serves
// beside this build during a roll and alone after a rollback. Its test of that rule,
// TestAWriteOfANodeAgainKeepsTheValuesItDoesNotOwn, holds this build's block index too.
const laterBlockState = graphstore.StateProposed

// TestAWriteOfANodeAgainKeepsTheValuesItDoesNotOwn holds the block index to R8 of
// this change: a write that writes a node again sets only the values it owns, so the
// index pass, writing a block again, keeps the block's stored visibility, and its stored state
// unless the pass itself makes the block gone or brings a gone block back (blockNode,
// graphstore.Node's Restates).
//
// A document is indexed through the real pass, and its requirement block is then marked
// proposed by SQL, as a later release's build writes that mark: the node row's state moved
// and one state tuple appended, at that build's sentinel cause (markBlockState). The pass over a
// version that changes another block's words and the pass over one that changes the
// requirement's own words each leave it proposed and its visibility as it was; the pass over a
// version that drops it writes it gone; and the pass over one that restores it writes it live.
// After each pass the row and the log of its state agree, the log holding one value for each
// write that moved the state and none for a pass that kept it.
func TestAWriteOfANodeAgainKeepsTheValuesItDoesNotOwn(t *testing.T) {
	database := parentDatabase(t)
	writer := model.Actor{Kind: "user", ID: "alice"}
	document := newParentDocument(t, database, "KEEP", "kept-md")
	const requirement, other = "r-ship", "b"

	withRequirement := func(words, otherWords string) blockDoc {
		text, rest := words+"\n", otherWords+"\n"
		return blockDoc{markdown: text + rest, blocks: []model.ArtifactBlock{
			{ID: requirement, Type: "requirement", From: 0, To: len(text)},
			{ID: other, Type: "paragraph", From: len(text), To: len(text) + len(rest)},
		}}
	}
	without := func(otherWords string) blockDoc {
		rest := otherWords + "\n"
		return blockDoc{markdown: rest, blocks: []model.ArtifactBlock{
			{ID: other, Type: "paragraph", From: 0, To: len(rest)},
		}}
	}
	index := func(number int, doc blockDoc) versionRecord {
		t.Helper()
		record := document.writeVersion(t, number, doc.markdown, writer)
		if won, _ := document.indexVersion(t, number, doc, record); !won {
			t.Fatalf("version %d did not become the version the document's blocks stand at", number)
		}
		return record
	}
	changedIn := func(blockID string, number int) {
		t.Helper()
		if changed := document.changes(t)[blockID]; changed.version != number {
			t.Fatalf("block %s's text last changed in version %d, want %d: the pass did not write it again", blockID, changed.version, number)
		}
	}

	first := index(1, withRequirement("We ship it.", "bb"))
	visibility := requireBlockState(t, document, requirement, graphstore.StateLive, "", []string{graphstore.StateLive})

	markBlockState(t, document, requirement, laterBlockState, model.Actor{Kind: "user", ID: "bob"}, first.at.Add(time.Second))
	states := []string{graphstore.StateLive, laterBlockState}
	requireBlockState(t, document, requirement, laterBlockState, visibility, states)

	// A version that changes the words of another block.
	index(2, withRequirement("We ship it.", "bb changed"))
	changedIn(other, 2)
	requireBlockState(t, document, requirement, laterBlockState, visibility, states)

	// A version that changes the words of the requirement itself, whose node the pass writes
	// again, live.
	index(3, withRequirement("We ship it today.", "bb changed"))
	changedIn(requirement, 3)
	requireBlockState(t, document, requirement, laterBlockState, visibility, states)

	// A version that drops the requirement makes it gone, and one that restores it brings it back.
	index(4, without("bb changed"))
	states = append(states, graphstore.StateGone)
	requireBlockState(t, document, requirement, graphstore.StateGone, visibility, states)

	index(5, withRequirement("We ship it today.", "bb changed"))
	states = append(states, graphstore.StateLive)
	requireBlockState(t, document, requirement, graphstore.StateLive, visibility, states)
}

// markBlockState writes, by SQL, a state of a block's node as a later release's build
// writes it: one state tuple, an attribute of the node with the value itself as its far end
// (graphstore.KindValue), carrying the write's time and writer at the sentinel cause, as the
// store's writer states a base attribute it moved (write.go's baseAttributeTuple), and the node
// row's state moved to match, its cause kept.
func markBlockState(t *testing.T, document parentDocument, blockID, state string, who model.Actor, at time.Time) {
	t.Helper()
	ctx := context.Background()
	recorded, err := json.Marshal(who.Recorded())
	if err != nil {
		t.Fatalf("encode the writer of block %s's state: %v", blockID, err)
	}
	if err := pgx.BeginFunc(ctx, document.database.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update graph_nodes set state = $3 where kind = $1 and id = $2`,
			BlockNodeKind, document.address(blockID), state)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("the store holds no node of block %s to mark", blockID)
		}
		_, err = tx.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
			values ($1, $2, $3, $4, $5, $6, $7, $8, null, true)
		`, BlockNodeKind, document.address(blockID), graphstore.AttributeState, graphstore.KindValue, state,
			at, recorded, graphstore.NoEvent)
		return err
	}); err != nil {
		t.Fatalf("mark block %s %s: %v", blockID, state, err)
	}
}

// requireBlockState fails unless the block's node row holds state, and visibility where one is
// given, the log's newest state and visibility are the row's, and the log states exactly states,
// in order, and one visibility. It answers the visibility the row holds. Each failure names the
// block, the value it reads and the value it wants.
func requireBlockState(t *testing.T, document parentDocument, blockID, state, visibility string, states []string) string {
	t.Helper()
	ctx := context.Background()
	address := document.address(blockID)
	node, held, err := graphstore.ReadNode(ctx, document.database.Pool, BlockNodeKind, address)
	if err != nil || !held {
		t.Fatalf("read block %s's node: held %t, %v", blockID, held, err)
	}
	if node.State != state {
		t.Errorf("block %s's node reads state %q, want %q", blockID, node.State, state)
	}
	if visibility != "" && node.Visibility != visibility {
		t.Errorf("block %s's node reads visibility %q, want %q, the visibility it held before", blockID, node.Visibility, visibility)
	}
	for _, attribute := range []struct {
		name, row string
		want      []string
	}{
		{graphstore.AttributeState, node.State, states},
		{graphstore.AttributeVisibility, node.Visibility, []string{node.Visibility}},
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
	return node.Visibility
}
