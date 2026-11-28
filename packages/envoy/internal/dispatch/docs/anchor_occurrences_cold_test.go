package docs

import (
	"context"
	"maps"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The tree read finds where each comment's anchor stands in its block (AnchorOccurrences) beside
// the positions of its blocks (Blocks), after its transaction, on a document no room may hold: a
// room idles out a minute after its last write. Both read the document a read of its text reads,
// so for a document no room holds the cold read's one fold of the stored history answers both,
// the tree it was rendered from kept beside the rendering for a document holding a comment's mark
// (buildDocumentRead). A read of an unchanged stored state then decodes nothing: before, each read
// of the occurrences loaded and decoded the whole history (documentTree), which beside the 9,252
// comments of c-tree-read-bound's largest spec cost the spec view about 250 ms a read. Each read
// still answers the occurrence of the anchor, which the test checks: the second of the block's two
// matches of the quote, and none for an anchor whose mark the document does not hold.
func TestTheAnchorsOfAColdDocumentReadOnItsCachedRead(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	persistence := &historyDecodeCountingStore{VersionedStore: NewPgVersioned(database)}
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, persistence)
	seedServiceText(t, service, artifactID, "# Heading\n\nWe keep the quote here, and we keep the quote there.\n")
	settleCurrentGeneration(t, service, artifactID)
	blocks, err := service.Blocks(context.Background(), artifactID)
	if err != nil || len(blocks) != 2 {
		t.Fatalf("read the blocks: %d, %v; want the heading and the paragraph", len(blocks), err)
	}
	paragraph := blocks[1].ID
	const quote = "keep the quote"
	alice := model.Actor{Kind: "user", ID: "alice"}
	second := 1
	if err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.MarkQuote(WithCommentMarks(ctx), artifactID, MarkSpec{Kind: MarkComment, ID: "c-second", By: alice}, quote,
			&QuoteScope{Occurrence: &second, BlockID: paragraph})
		return err
	}); err != nil {
		t.Fatalf("mark the quote: %v", err)
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	persistence.folds.Store(0)
	persistence.loads.Store(0)

	anchors := []CommentAnchorInBlock{{MarkID: "c-second", BlockID: paragraph, Quote: quote}, {MarkID: "c-none", BlockID: paragraph, Quote: quote}}
	want := map[int]int{0: 1}
	read := func(what string) {
		t.Helper()
		if _, err := service.Blocks(context.Background(), artifactID); err != nil {
			t.Fatalf("%s: read the blocks: %v", what, err)
		}
		occurrences, err := service.AnchorOccurrences(context.Background(), artifactID, anchors)
		if err != nil {
			t.Fatalf("%s: read the anchors' occurrences: %v", what, err)
		}
		if !maps.Equal(occurrences, want) {
			t.Fatalf("%s: the anchors' occurrences are %v, want %v: the mark stands on the block's second match, and c-none on none", what, occurrences, want)
		}
		if service.srv.GetDoc(artifactID) != nil {
			t.Fatalf("%s: the read loaded the document's room", what)
		}
	}
	read("the first read")
	if folds, loads := persistence.folds.Load(), persistence.loads.Load(); folds != 1 || loads != 0 {
		t.Fatalf("the first read folded the history %d times and loaded it %d times, want one fold and no other load", folds, loads)
	}
	read("a second read")
	read("a third read")
	if folds, loads := persistence.folds.Load(), persistence.loads.Load(); folds != 1 || loads != 0 {
		t.Fatalf("after the reads of the cached state the history was folded %d times and loaded %d times, want the first read's one fold alone", folds, loads)
	}
}

// TestOneReadAnswersTheBlocksAndTheAnchorsAsTheirOwnReads holds BlocksAndAnchorOccurrences, the one
// read of a document the tree read takes its block positions and its comments' occurrences from,
// to Blocks and AnchorOccurrences, right after a comment's mark is written and again once the room
// is evicted, when both read the cold read's cached tree: the same blocks, the anchor's occurrence
// on the second of its block's two matches of the quote, and none for an anchor whose mark the
// document does not hold. With no anchors it answers the same blocks and no occurrence.
func TestOneReadAnswersTheBlocksAndTheAnchorsAsTheirOwnReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, NewPgVersioned(database))
	seedServiceText(t, service, artifactID, "# Heading\n\nWe keep the quote here, and we keep the quote there.\n")
	settleCurrentGeneration(t, service, artifactID)
	blocks, err := service.Blocks(ctx, artifactID)
	if err != nil || len(blocks) != 2 {
		t.Fatalf("read the blocks: %d, %v; want the heading and the paragraph", len(blocks), err)
	}
	paragraph := blocks[1].ID
	const quote = "keep the quote"
	second := 1
	if err := joinedWrite(service, func(ctx context.Context) error {
		_, err := service.MarkQuote(WithCommentMarks(ctx), artifactID, MarkSpec{Kind: MarkComment, ID: "c-second", By: model.Actor{Kind: "user", ID: "alice"}}, quote,
			&QuoteScope{Occurrence: &second, BlockID: paragraph})
		return err
	}); err != nil {
		t.Fatalf("mark the quote: %v", err)
	}
	anchors := []CommentAnchorInBlock{{MarkID: "c-second", BlockID: paragraph, Quote: quote}, {MarkID: "c-none", BlockID: paragraph, Quote: quote}}
	compare := func(what string) {
		t.Helper()
		wantBlocks, err := service.Blocks(ctx, artifactID)
		if err != nil {
			t.Fatalf("%s: read the blocks: %v", what, err)
		}
		wantOccurrences, err := service.AnchorOccurrences(ctx, artifactID, anchors)
		if err != nil {
			t.Fatalf("%s: read the anchors' occurrences: %v", what, err)
		}
		gotBlocks, gotOccurrences, err := service.BlocksAndAnchorOccurrences(ctx, artifactID, anchors)
		if err != nil {
			t.Fatalf("%s: read the blocks and the anchors' occurrences in one read: %v", what, err)
		}
		if !reflect.DeepEqual(gotBlocks, wantBlocks) {
			t.Errorf("%s: the one read answers the blocks %+v, and Blocks %+v", what, gotBlocks, wantBlocks)
		}
		if want := map[int]int{0: 1}; !maps.Equal(gotOccurrences, wantOccurrences) || !maps.Equal(gotOccurrences, want) {
			t.Errorf("%s: the one read answers the occurrences %v and AnchorOccurrences %v, want %v: the mark stands on the block's second match, and c-none on none",
				what, gotOccurrences, wantOccurrences, want)
		}
		onlyBlocks, none, err := service.BlocksAndAnchorOccurrences(ctx, artifactID, nil)
		if err != nil || !reflect.DeepEqual(onlyBlocks, wantBlocks) || none != nil {
			t.Errorf("%s: the one read with no anchors answers the blocks %+v, the occurrences %v and %v, want Blocks' %+v and none", what, onlyBlocks, none, err, wantBlocks)
		}
	}
	compare("after the mark's write")
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	compare("with no room resident")
}
