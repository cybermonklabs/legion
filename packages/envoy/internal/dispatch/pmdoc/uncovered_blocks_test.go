package pmdoc

import (
	"fmt"
	"slices"
	"testing"
)

// blocksByName names doc's blocks for a test: a paragraph by its text, and every other block by
// its type and its place among the blocks of that type in document order ("bullet_list 0" is the
// first list, the outermost where lists nest).
func blocksByName(doc *Node) map[string]string {
	ids := map[string]string{}
	counts := map[string]int{}
	Walk(doc, func(node *Node) bool {
		id, _ := node.Attrs[BlockIDAttr].(string)
		switch {
		case id == "" || node.Type == "doc":
		case node.Type == "paragraph":
			ids[TextContent(node)] = id
		default:
			ids[fmt.Sprintf("%s %d", node.Type, counts[node.Type])] = id
			counts[node.Type]++
		}
		return true
	})
	return ids
}

// A removal is covered where the guard names a node from the document down to the outermost node
// it takes; otherwise what it leaves uncovered is that node as the listing names it, but that a
// list, a list item or a quote holding a named block is read block by block, since it holds
// nothing of its own but its attributes. A typed block is no such wrapper, and a removal of
// nothing leaves nothing uncovered.
func TestUncoveredBlocksIsWhatOfARemovalTheGuardDoesNotCover(t *testing.T) {
	t.Parallel()
	const (
		nested = "- Parent\n  - Child\n- Other\n"
		twice  = "- Parent\n  - First\n  - Second\n- Other\n"
		lone   = "Intro.\n\n- Only\n"
		quoted = "> Quoted.\n\nAfter.\n"
		typed  = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nAlpha.\n:::\n"
	)
	for _, test := range []struct {
		name, markdown string
		// block is the block BlockRemoval removes with nothing held, and find the text whose
		// delete TextblockRemoval reads where block is "".
		block, find string
		// named is the blocks the guard names, and uncovered the blocks it leaves uncovered, each
		// by its name (blocksByName).
		named, uncovered []string
	}{
		{
			name: "a list item under a guard on the item", markdown: twice, block: "list_item 0",
			named: []string{"list_item 0"},
		},
		{
			name: "a list item under a guard on the list holding it", markdown: twice, block: "list_item 0",
			named: []string{"bullet_list 0"},
		},
		{
			name: "a list item under no guard", markdown: twice, block: "list_item 0",
			uncovered: []string{"Parent", "bullet_list 1"},
		},
		{
			name: "a list item under its paragraph alone", markdown: twice, block: "list_item 0",
			named: []string{"Parent"}, uncovered: []string{"bullet_list 1"},
		},
		{
			name: "a list item under its paragraph and one of its nested items", markdown: twice, block: "list_item 0",
			named: []string{"Parent", "First"}, uncovered: []string{"Second"},
		},
		{
			name: "a list's only paragraph, which empties the list, under that paragraph", markdown: lone, block: "Only",
			named: []string{"Only"},
		},
		{
			name: "a list's only paragraph, which empties the list, under another block", markdown: lone, block: "Only",
			named: []string{"Intro."}, uncovered: []string{"bullet_list 0"},
		},
		{
			name: "the whole text of a list's only item, under its paragraph", markdown: lone, find: "Only",
			named: []string{"Only"},
		},
		{
			name: "the whole text of an item whose nested list takes its place, under the item", markdown: nested, find: "Parent",
			named: []string{"list_item 0"},
		},
		{
			name: "the whole text of an item whose nested list takes its place, under no guard", markdown: nested, find: "Parent",
			uncovered: []string{"Parent"},
		},
		{name: "part of a paragraph's text", markdown: lone, find: "Intro"},
		{
			name: "a quote's only paragraph, which empties the quote, under another block", markdown: quoted, block: "Quoted.",
			named: []string{"After."}, uncovered: []string{"blockquote 0"},
		},
		{
			name: "a typed block under the paragraph it holds", markdown: typed, block: "callout 0",
			named: []string{"Alpha."}, uncovered: []string{"callout 0"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := parseFixture(t, test.markdown)
			ids := blocksByName(doc)
			idOf := func(name string) string {
				t.Helper()
				if ids[name] == "" {
					t.Fatalf("no block %q in %v", name, ids)
				}
				return ids[name]
			}
			valid, err := ValidateDocument(doc, "BlockRemoval")
			if err != nil {
				t.Fatal(err)
			}
			var removed []int
			if test.block != "" {
				removed, err = valid.BlockRemoval(idOf(test.block), nil)
			} else {
				var r Range
				if r, err = FindQuote(doc, test.find, nil, nil); err != nil {
					t.Fatal(err)
				}
				removed, err = valid.TextblockRemoval(r)
			}
			if err != nil {
				t.Fatal(err)
			}
			named := make(map[string]struct{}, len(test.named))
			for _, name := range test.named {
				named[idOf(name)] = struct{}{}
			}
			want := make([]string, 0, len(test.uncovered))
			for _, name := range test.uncovered {
				want = append(want, idOf(name))
			}
			got := valid.UncoveredBlocks(removed, named)
			if !slices.Equal(got, want) {
				t.Fatalf("UncoveredBlocks under %v = %v, want %v (%v)", test.named, got, want, test.uncovered)
			}
		})
	}
}

// The zero ValidDocument holds no tree, so no path takes anything from it and it leaves nothing
// uncovered.
func TestTheZeroValidDocumentLeavesNothingUncovered(t *testing.T) {
	t.Parallel()
	if got := (ValidDocument{}).UncoveredBlocks([]int{0}, nil); got != nil {
		t.Fatalf("the zero ValidDocument's UncoveredBlocks = %v, want nil", got)
	}
}
