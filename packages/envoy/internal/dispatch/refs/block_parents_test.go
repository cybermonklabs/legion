package refs

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A block that writes no byte of its own hangs under the block that holds it (c-tree-census: each
// node's parent is the node the rendering draws it inside). The renderer writes an empty list
// item as its marker alone, so the item's empty paragraph is a range of no bytes at the item's
// end, which is the list's end too: read by ranges alone it would end both and hang under the
// document. The ranges below are those of a list of the item `first` and an empty item, followed
// by the paragraph `After.`, and of a quote that holds one empty paragraph, `> `, as the renderer
// records them (pmdoc.RenderWithBlockOffsets): the marker of the empty item is `- `, two bytes,
// and its paragraph starts and ends after them.
func TestABlockOfNoBytesHangsUnderTheBlockThatEndsWhereItStarts(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		blocks []model.ArtifactBlock
		want   []int
	}{
		{
			name: "the empty paragraph of a list's empty last item hangs under that item",
			blocks: []model.ArtifactBlock{
				{ID: "list", Type: "bullet_list", From: 0, To: 10},
				{ID: "first", Type: "list_item", From: 0, To: 7},
				{ID: "first-text", Type: "paragraph", From: 2, To: 7},
				{ID: "empty", Type: "list_item", From: 8, To: 10},
				{ID: "empty-text", Type: "paragraph", From: 10, To: 10},
				{ID: "after", Type: "paragraph", From: 12, To: 18},
			},
			want: []int{-1, 0, 1, 0, 3, -1},
		},
		{
			name: "the empty paragraph of a quote hangs under the quote",
			blocks: []model.ArtifactBlock{
				{ID: "quote", Type: "blockquote", From: 0, To: 2},
				{ID: "quote-text", Type: "paragraph", From: 2, To: 2},
			},
			want: []int{-1, 0},
		},
		{
			name: "a block of no bytes holds nothing: a second one at its offset hangs under their holder",
			blocks: []model.ArtifactBlock{
				{ID: "quote", Type: "blockquote", From: 0, To: 2},
				{ID: "one", Type: "paragraph", From: 2, To: 2},
				{ID: "two", Type: "paragraph", From: 2, To: 2},
			},
			want: []int{-1, 0, 0},
		},
		{
			name: "a block that writes bytes where another ends stands beside it",
			blocks: []model.ArtifactBlock{
				{ID: "one", Type: "paragraph", From: 0, To: 4},
				{ID: "two", Type: "paragraph", From: 4, To: 8},
			},
			want: []int{-1, -1},
		},
		{
			name: "a block of no bytes after the end of another stands beside it",
			blocks: []model.ArtifactBlock{
				{ID: "one", Type: "paragraph", From: 0, To: 4},
				{ID: "empty", Type: "paragraph", From: 6, To: 6},
			},
			want: []int{-1, -1},
		},
	} {
		if got := blockParents(row.blocks); !slices.Equal(got, row.want) {
			t.Errorf("%s: blockParents answers %v, want %v", row.name, got, row.want)
		}
	}
}
