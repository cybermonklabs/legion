package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// offsetBlocks maps a render's offsets to the blocks the index takes, and the attributes have to
// come with them: the block index is where a filter over a typed block's attribute reads, so a
// block that arrives there with none is a block that declares none, which is a different fact.
// Dropping them silently is the shape of this defect - the render carries the attributes, the API
// response path keeps them, and only the path into document_blocks lost them, so nothing fails
// and a later attribute filter answers nothing.
func TestOffsetBlocksCarriesTheAttributesTheRenderFound(t *testing.T) {
	offsets := []pmdoc.BlockOffset{
		{
			ID: "c1", Type: "check", From: 0, To: 10,
			Attributes: []model.BlockAttributeValue{
				{Name: "serves", Value: "r1", Kind: "block[]"},
				{Name: "surface", Value: "cli", Kind: "enum"},
			},
		},
		{ID: "p1", Type: "paragraph", From: 10, To: 20},
	}

	blocks := offsetBlocks(offsets)

	if len(blocks) != len(offsets) {
		t.Fatalf("offsetBlocks returned %d blocks, want %d", len(blocks), len(offsets))
	}
	if len(blocks[0].Attributes) != 2 {
		t.Fatalf("the check block reached the index with %d attributes, want 2: %+v",
			len(blocks[0].Attributes), blocks[0].Attributes)
	}
	for index, want := range offsets[0].Attributes {
		if got := blocks[0].Attributes[index]; got != want {
			t.Fatalf("attribute %d = %+v, want %+v", index, got, want)
		}
	}
	if blocks[0].ID != "c1" || blocks[0].Type != "check" || blocks[0].From != 0 || blocks[0].To != 10 {
		t.Fatalf("the check block lost its identity or offsets: %+v", blocks[0])
	}
	if len(blocks[1].Attributes) != 0 {
		t.Fatalf("a paragraph reached the index with attributes: %+v", blocks[1].Attributes)
	}
}
