package refs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// headViewArmBlock is one block of the issue ARM-1 that
// TestAViewReadsThroughItsArmsEveryBlockItsRangeSelects writes into the head projection under the
// issue's node: its address, its rank, its supertag, its document_blocks type ("" for none), its
// text, whether its state is gone, and whether the view's rows hold it.
type headViewArmBlock struct {
	address, rank, supertag, blockType, text string
	gone, read                               bool
}

// headViewArmBlocks are the blocks of each kind an arm of headRowsView reads, and of each kind it
// must leave unread, for a view of check and requirement: an arm reads a block through the index
// that holds its kind (migration 20261128054905_issue_tree_head_view_indexes). The last two are no
// blocks of the issue's own documents - one of another issue's document placed under the issue,
// whose address sorts below `ARM-1/`, and a finding of a lens, whose address sorts from `ARM-10` on
// - which a view reads whole, and which rank before every block of the issue's own documents.
var headViewArmBlocks = []headViewArmBlock{
	{address: "ARM-1/spec#first", rank: "a", supertag: "paragraph", blockType: "paragraph", text: "The opening paragraph.", read: true},
	{address: "ARM-1/spec#req", rank: "b", supertag: "requirement", blockType: "requirement", text: ":::requirement{#req}\nThe list orders the asks.\n:::", read: true},
	{address: "ARM-1/spec#checklist", rank: "c", supertag: "checklist", blockType: "checklist", text: ":::checklist{#checklist}\nA longer name.\n:::", read: true},
	{address: "ARM-1/spec#typed-by-row", rank: "d", supertag: "paragraph", blockType: "requirement", text: "A paragraph its row types.", read: true},
	{address: "ARM-1/spec#list", rank: "e", supertag: "list", blockType: "list", text: "- :::requirement{#in-list}\n  Listed.\n  :::", read: true},
	{address: "ARM-1/spec#nested", rank: "f", supertag: "plan_task", blockType: "plan_task", text: ":::plan_task{#nested}\n:::check{#inner}\nInner.\n:::\n:::", read: true},
	{address: "ARM-1/spec#task", rank: "g", supertag: "plan_task", blockType: "plan_task", text: ":::plan_task{#task}\nBuild the index.\n:::"},
	{address: "ARM-1/spec#prose", rank: "h", supertag: "paragraph", blockType: "paragraph", text: "Prose no view takes."},
	{address: "ARM-1/spec#gone", rank: "i", supertag: "requirement", text: ":::requirement{#gone}\nRetired.\n:::", gone: true},
	{address: "ARM-0/plan#placed", rank: "0", supertag: "paragraph", blockType: "paragraph", text: "A paragraph of another issue.", read: true},
	{address: "lens/l1#finding", rank: "1", supertag: "finding", text: "A finding of a lens.", read: true},
}

// TestAViewReadsThroughItsArmsEveryBlockItsRangeSelects holds the blocks of an issue that a view's
// read of its rows (HeadViewRows, headRowsView) answers, each read through the index of its kind,
// to the blocks a read of the whole range of the issue's own blocks selects, tested one by one: a
// block whose supertag is a type; whose supertag is a longer name that begins with a type, whose
// text the type's pattern finds; whose document_blocks type is a type and is not its supertag; whose
// text opens a typed block of a type in a list, or inside a typed block of another type; the first
// of the issue's own blocks under the issue's node, which no type takes; and every block of the
// issue that is no block of its own documents. It leaves unread a typed block of another type whose
// text holds its own fences alone, a paragraph, and a gone block that no document_blocks row stands
// for. The read with no view (HeadRows over a skeleton) reads every block that is no block of the
// issue's own documents, and of the issue's own blocks those of the documents its skeleton names alone.
// It refuses a type that is no typed block's name.
func TestAViewReadsThroughItsArmsEveryBlockItsRangeSelects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	const key = "ARM-1"
	types := []string{"check", "requirement"}
	for _, block := range headViewArmBlocks {
		kinds, texts := []string{}, []string{}
		if block.gone {
			kinds, texts = []string{graphstore.AttributeState}, []string{graphstore.StateGone}
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into issue_head_nodes (issue_key, node_kind, node_address, parent_kind, parent_address, rank, text, supertag, block_type,
				value_kinds, value_texts, relation_kinds, relation_to_kinds, relation_to_ids)
			values ($1, $2, $3, $4, $1, $5, $6, $7, nullif($8, ''), $9, $10, '{}', '{}', '{}')`,
			key, BlockNodeKind, block.address, graphstore.KindIssue, block.rank, block.text, block.supertag, block.blockType,
			kinds, texts); err != nil {
			t.Fatalf("write the head row of %s: %v", block.address, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `insert into issue_head_marks (issue_key, filled_at) values ($1, now())`, key); err != nil {
		t.Fatalf("mark %s whole: %v", key, err)
	}

	// The whole range of the issue's own blocks, tested block by block: what a view read of them
	// before its arms.
	rows, err := database.Pool.Query(ctx, `
		select blocks.node_address from issue_head_nodes blocks
		where blocks.issue_key = $1 and blocks.node_kind = $2 and blocks.node_address >= $1 || '/' and blocks.node_address < $1 || '0'
		  and (blocks.supertag = any($3::text[]) or blocks.block_type = any($3::text[])
		       or (position(':::' in coalesce(blocks.text, '')) > 0 and blocks.text ~ $4))
		  and (blocks.document_id is not null
		       or blocks.value_texts[array_position(blocks.value_kinds, $5)] is distinct from $6)`,
		key, BlockNodeKind, types, typedOpening(types), graphstore.AttributeState, graphstore.StateGone)
	if err != nil {
		t.Fatalf("read the blocks the whole range of %s selects: %v", key, err)
	}
	selected := map[string]bool{}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			t.Fatalf("read a block the whole range of %s selects: %v", key, err)
		}
		selected[address] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks the whole range of %s selects: %v", key, err)
	}

	heads, marked, err := HeadViewRows(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, key, HeadView{Types: types})
	if err != nil || !marked {
		t.Fatalf("read the view's rows of %s: marked %t, %v", key, marked, err)
	}
	read := map[string]bool{}
	for node := range heads.Rows {
		if node.Kind == BlockNodeKind {
			read[node.ID] = true
		}
	}
	for _, block := range headViewArmBlocks {
		if read[block.address] != block.read {
			t.Errorf("the view's rows of %s hold %s (supertag %q, type %q, text %q): %t, want %t",
				key, block.address, block.supertag, block.blockType, block.text, read[block.address], block.read)
		}
		own := strings.HasPrefix(block.address, key+"/")
		if want := block.read && own && block.address != key+"/spec#first"; selected[block.address] != want {
			t.Errorf("the whole range of %s selects %s: %t, want %t", key, block.address, selected[block.address], want)
		}
	}
	for address := range selected {
		if !read[address] {
			t.Errorf("the view's rows of %s leave out %s, which the whole range selects", key, address)
		}
	}
	t.Logf("the view's rows of %s hold the blocks %s", key, strings.Join(slices.Sorted(maps.Keys(read)), ", "))

	// The read with no view reads the blocks of the documents its skeleton names and every block
	// that is no block of the issue's own documents (headRowsSkeleton).
	for _, documents := range [][]string{{}, {key + "/spec"}} {
		skeleton, marked, err := HeadRows(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, key, &HeadSkeleton{Documents: documents})
		if err != nil || !marked {
			t.Fatalf("read the skeleton of %s over the documents %v: marked %t, %v", key, documents, marked, err)
		}
		for _, block := range headViewArmBlocks {
			want := !strings.HasPrefix(block.address, key+"/") || len(documents) > 0
			if _, held := skeleton.Rows[Target{Kind: BlockNodeKind, ID: block.address}]; held != want {
				t.Errorf("the skeleton of %s over the documents %v holds %s: %t, want %t", key, documents, block.address, held, want)
			}
		}
	}

	if _, _, err := HeadViewRows(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, key, HeadView{Types: []string{"check|requirement"}}); err == nil {
		t.Error("a view of the type check|requirement read its rows, want a refusal: a type is a typed block's name")
	}
}
