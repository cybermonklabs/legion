package refs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// and the item read every bare key, `#id` and short id alike, for the author every item of these
// fixtures records (prefixAuthor). A route plans those two writes above its row locks and runs
// them as one batch of its own (api.planItemWrite), and counts what both moved for its event
// (CountedChanges); no test here reads those counts, so this runs the two writes alone.
func indexItemAsAWriteDoes(t *testing.T, tx pgx.Tx, kind ItemKind, itemID, body, serverURL string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	located := NewReader(ctx, tx, prefixAuthor, string(kind), itemID, serverURL).Mentions(body)
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

// NewItemBlock is the block an item's first index writes: each block of a comment's body, and each
// part of an ask, takes the id the index planned against an item with no state row
// (unindexedItem) gives it, with the excerpt that index writes, a long block's cut to
// BlockExcerptRunes; an id the text holds no block of is found by neither.
func TestNewItemBlockIsTheBlockAnItemsFirstIndexWrites(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("gnomon ", BlockExcerptRunes))
	body := "Status\nWe spin up the node.\n\nThe node answers on 8080.\n\n" + long
	ask := model.Ask{
		ID: "00000000-0000-0000-0000-000000000001", Question: "Which port should the node use?",
		Options: []model.AskOption{{Label: "Port 8080", Description: "The port it answers on now."}, {Label: "Port 8081"}},
	}
	parts := AskParts(ask)
	askSource, err := askPartsSource(ask, parts, askPartsSum(parts), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The parts hold no link, so no Reader is asked for one.
	askSource.pairs = func([]itemblocks.Block, []string) []blockPair { return nil }
	for _, item := range []struct {
		kind   ItemKind
		blocks []itemblocks.Block
		source itemBlockSource
	}{
		{ItemComment, itemblocks.Split(body), itemBodySource(ItemComment, body, nil, nil)},
		{ItemAsk, parts, askSource},
	} {
		index := ItemIndex{state: unindexedItem, stored: []itemblocks.Stored{}}
		first, err := planItemBlocks(index, item.kind, ask.ID, time.Now(), item.source)
		if err != nil {
			t.Fatalf("plan the first index of the %s: %v", item.kind, err)
		}
		for number := 1; number <= len(item.blocks)+1; number++ {
			id := fmt.Sprintf("b%d", number)
			_, wrote, written := first.Block(id)
			if excerpt, found := NewItemBlock(item.blocks, id); found != written || excerpt != wrote {
				t.Errorf("the %s's %s: NewItemBlock = %q, %v; its first index writes %q, %v", item.kind, id, excerpt, found, wrote, written)
			}
		}
	}
	if excerpt, found := NewItemBlock(itemblocks.Split(body), "b3"); !found || excerpt == long {
		t.Errorf("the long block's excerpt = %q, %v; want it cut to %d runes", excerpt, found, BlockExcerptRunes)
	}
}
