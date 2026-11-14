package refs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// itemBlockNodeID is the node id of block blockID of the item of kind whose uuid is itemID, the
// one shape every block table's node_id column holds: `comment/<uuid>#b2`,
// `message/<uuid>#b1`, `ask/<uuid>#b3` for a part of an ask. Production writes that shape in
// text.TargetID and in the SQL that inserts a block row; a test writes it here rather than
// spelling it out at each assertion.
func itemBlockNodeID(kind ItemKind, itemID, blockID string) string {
	return string(kind) + "/" + itemID + "#" + blockID
}

// indexItemAsAWriteDoes indexes the body of comment or message itemID, written at `at`, as the
// route that writes one indexes it: its blocks and the mentions each holds (IndexItem), then
// the item's own references (replaceRefs), from one reading of body (Mentions), so the blocks
// and the item read every bare key, `#id` and short id alike. A route plans those two writes
// above its row locks and runs them as one batch of its own (api.planItemWrite), and counts
// what both moved for its event (CountedChanges); no test here reads those counts, so this runs
// the two writes alone.
func indexItemAsAWriteDoes(t *testing.T, tx pgx.Tx, kind ItemKind, itemID, body, serverURL string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	located := NewReader(ctx, tx, string(kind), itemID, serverURL).Mentions(body)
	if _, err := IndexItem(ctx, tx, kind, itemID, body, at, located, nil); err != nil {
		t.Fatalf("index the blocks of %s %s: %v", kind, itemID, err)
	}
	if _, err := replaceRefs(ctx, tx, string(kind), itemID, text.Refs(located), nil); err != nil {
		t.Fatalf("index the references of %s %s: %v", kind, itemID, err)
	}
}

// A mention in a comment or message starts at the block holding it: one pair per (block, target),
// so a target cited twice in one block is one pair and cited from two blocks is two, and an
// external link writes none.
func TestItemBlockPairsStartAtTheBlockHoldingEachMention(t *testing.T) {
	body := "Intro cites dispatch://CORE-1 and dispatch://CORE-1 again.\n\n" +
		"See https://example.com.\n\n" +
		"- dispatch://CORE-2\n- dispatch://CORE-1"
	next := itemblocks.Split(body)
	if len(next) != 3 {
		t.Fatalf("Split = %+v, want three blocks", next)
	}
	ids := []string{"b1", "b4", "b2"}
	pairs := itemBlockPairs(next, ids, text.ExtractAt(body, "https://dispatch.example"))
	got := make([]string, len(pairs))
	for i, pair := range pairs {
		got[i] = pair.blockID + "->" + pair.target.Kind + ":" + pair.target.ID
	}
	want := []string{"b1->issue:CORE-1", "b2->issue:CORE-2", "b2->issue:CORE-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("itemBlockPairs = %v, want %v", got, want)
	}
	if pairs := itemBlockPairs(next, ids, nil); len(pairs) != 0 {
		t.Fatalf("itemBlockPairs with no mentions = %v, want none", pairs)
	}
}

func TestBlockNumberReadsAMintedID(t *testing.T) {
	for id, want := range map[string]int{"b1": 1, "b12": 12} {
		if got, ok := blockNumber(id); !ok || got != want {
			t.Errorf("blockNumber(%q) = %d, %v, want %d", id, got, ok, want)
		}
	}
	for _, id := range []string{"", "b", "x1", "b1a"} {
		if _, ok := blockNumber(id); ok {
			t.Errorf("blockNumber(%q) read a number", id)
		}
	}
}
